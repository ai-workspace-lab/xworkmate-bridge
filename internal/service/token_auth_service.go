package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type StaticTokenAuthService struct {
	expectedTokens map[string]struct{}
}

func NewStaticTokenAuthService(expectedToken string, extraTokens ...string) *StaticTokenAuthService {
	tokens := map[string]struct{}{}
	for _, token := range append([]string{expectedToken}, extraTokens...) {
		trimmed := strings.TrimSpace(token)
		if trimmed != "" {
			tokens[trimmed] = struct{}{}
		}
	}
	return &StaticTokenAuthService{expectedTokens: tokens}
}

func (s *StaticTokenAuthService) ValidateToken(token string) bool {
	token = strings.TrimSpace(token)
	if len(s.expectedTokens) == 0 {
		return true
	}
	_, ok := s.expectedTokens[token]
	return ok
}

func (s *StaticTokenAuthService) ValidateAuthorizationHeader(header string) bool {
	header = strings.TrimSpace(header)
	if len(s.expectedTokens) == 0 {
		return true
	}
	if header == "" {
		return false
	}
	if s.ValidateToken(header) {
		return true
	}
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return false
	}
	return s.ValidateToken(strings.TrimSpace(header[len("Bearer "):]))
}

// ResolveAuthorizationHeader provides a stable development-only account scope
// without retaining the static token. Production identity comes from Accounts.
func (s *StaticTokenAuthService) ResolveAuthorizationHeader(_ context.Context, header string) (AuthorizationPrincipal, error) {
	if !s.ValidateAuthorizationHeader(header) {
		return AuthorizationPrincipal{}, ErrAuthorizationPrincipalUnavailable
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(header)))
	return AuthorizationPrincipal{AccountID: "static-" + hex.EncodeToString(digest[:])}, nil
}
