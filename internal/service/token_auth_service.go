package service

import "strings"

type StaticTokenAuthService struct {
	expectedToken string
}

func NewStaticTokenAuthService(expectedToken string) *StaticTokenAuthService {
	return &StaticTokenAuthService{
		expectedToken: strings.TrimSpace(expectedToken),
	}
}

func (s *StaticTokenAuthService) ValidateToken(token string) bool {
	token = strings.TrimSpace(token)
	if s.expectedToken == "" {
		return true
	}
	return token == s.expectedToken
}

func (s *StaticTokenAuthService) ValidateAuthorizationHeader(header string) bool {
	header = strings.TrimSpace(header)
	if s.expectedToken == "" {
		return true
	}
	if header == "" {
		return false
	}
	if header == s.expectedToken {
		return true
	}
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return false
	}
	return strings.TrimSpace(header[len("Bearer "):]) == s.expectedToken
}
