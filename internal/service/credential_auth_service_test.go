package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCredentialTokenAuthServiceUsesInternalServiceHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Service-Token"); got != "accounts-service-token" {
			t.Fatalf("X-Service-Token = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("unexpected forwarded Authorization header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true,"accountId":"account-1"}`))
	}))
	defer server.Close()

	service := NewCredentialTokenAuthService(server.URL, "accounts-service-token")
	if !service.ValidateAuthorizationHeader("Bearer user-tenant-token") {
		t.Fatal("expected Accounts-backed credential to be accepted")
	}
}

func TestCredentialTokenAuthServiceRejectsActiveResponseWithoutAccountID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true}`))
	}))
	defer server.Close()

	service := NewCredentialTokenAuthService(server.URL, "accounts-service-token")
	if _, err := service.ResolveAuthorizationHeader(t.Context(), "Bearer user-tenant-token"); err == nil {
		t.Fatal("active credential without an account principal must fail closed")
	}
}

func TestCredentialTokenAuthServiceFailsClosed(t *testing.T) {
	service := NewCredentialTokenAuthService("", "")
	if service.ValidateAuthorizationHeader("Bearer user-tenant-token") {
		t.Fatal("missing introspection configuration must not authorize")
	}
}
