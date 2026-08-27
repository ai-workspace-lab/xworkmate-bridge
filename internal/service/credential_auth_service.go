package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var ErrAuthorizationPrincipalUnavailable = errors.New("authorization principal unavailable")

type AuthorizationPrincipal struct {
	AccountID string
}

// CredentialTokenAuthService validates a user credential with Accounts. It is
// deliberately fail-closed: a missing endpoint, malformed header, timeout or
// non-2xx reply is never accepted.
type CredentialTokenAuthService struct {
	endpoint     string
	serviceToken string
	client       *http.Client
}

func NewCredentialTokenAuthService(endpoint, serviceToken string) *CredentialTokenAuthService {
	return &CredentialTokenAuthService{
		endpoint:     strings.TrimSpace(endpoint),
		serviceToken: strings.TrimSpace(serviceToken),
		client:       &http.Client{Timeout: 5 * time.Second},
	}
}

func (s *CredentialTokenAuthService) ValidateAuthorizationHeader(header string) bool {
	payload, err := s.introspect(context.Background(), header)
	return err == nil && payload.Active
}

func (s *CredentialTokenAuthService) ResolveAuthorizationHeader(ctx context.Context, header string) (AuthorizationPrincipal, error) {
	payload, err := s.introspect(ctx, header)
	if err != nil || !payload.Active {
		return AuthorizationPrincipal{}, ErrAuthorizationPrincipalUnavailable
	}
	accountID := strings.TrimSpace(payload.AccountID)
	if accountID == "" {
		accountID = strings.TrimSpace(payload.UserID)
	}
	if accountID == "" {
		accountID = strings.TrimSpace(payload.Subject)
	}
	if accountID == "" {
		return AuthorizationPrincipal{}, ErrAuthorizationPrincipalUnavailable
	}
	return AuthorizationPrincipal{AccountID: accountID}, nil
}

type credentialIntrospectionPayload struct {
	Active    bool   `json:"active"`
	AccountID string `json:"accountId"`
	UserID    string `json:"userId"`
	Subject   string `json:"sub"`
}

func (s *CredentialTokenAuthService) introspect(ctx context.Context, header string) (credentialIntrospectionPayload, error) {
	if s == nil || s.endpoint == "" || s.serviceToken == "" {
		return credentialIntrospectionPayload{}, ErrAuthorizationPrincipalUnavailable
	}
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return credentialIntrospectionPayload{}, ErrAuthorizationPrincipalUnavailable
	}
	token := strings.TrimSpace(header[len("Bearer "):])
	if token == "" {
		return credentialIntrospectionPayload{}, ErrAuthorizationPrincipalUnavailable
	}
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return credentialIntrospectionPayload{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return credentialIntrospectionPayload{}, err
	}
	// Accounts internal routes authenticate service callers with this dedicated
	// header. The user credential never leaves the bridge boundary.
	req.Header.Set("X-Service-Token", s.serviceToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return credentialIntrospectionPayload{}, err
	}
	if resp.StatusCode != http.StatusOK {
		if closeErr := resp.Body.Close(); closeErr != nil {
			return credentialIntrospectionPayload{}, closeErr
		}
		return credentialIntrospectionPayload{}, ErrAuthorizationPrincipalUnavailable
	}
	var payload credentialIntrospectionPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		if closeErr := resp.Body.Close(); closeErr != nil {
			return credentialIntrospectionPayload{}, errors.Join(err, closeErr)
		}
		return credentialIntrospectionPayload{}, err
	}
	if err := resp.Body.Close(); err != nil {
		return credentialIntrospectionPayload{}, err
	}
	return payload, nil
}
