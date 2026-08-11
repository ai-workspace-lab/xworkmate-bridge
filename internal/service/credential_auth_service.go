package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

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
	if s == nil || s.endpoint == "" || s.serviceToken == "" {
		return false
	}
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return false
	}
	token := strings.TrimSpace(header[len("Bearer "):])
	if token == "" {
		return false
	}
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return false
	}
	req, err := http.NewRequest(http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return false
	}
	// Accounts internal routes authenticate service callers with this dedicated
	// header. The user credential never leaves the bridge boundary.
	req.Header.Set("X-Service-Token", s.serviceToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var payload struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false
	}
	return payload.Active
}
