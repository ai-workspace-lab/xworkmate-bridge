package acp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPServerWriteTimeoutCoversOpenClawAgentWait(t *testing.T) {
	server := newHTTPServer("127.0.0.1:0", http.NewServeMux())

	if server.ReadTimeout != 30*time.Second {
		t.Fatalf("expected fixed read timeout, got %s", server.ReadTimeout)
	}
	if server.IdleTimeout != 2*time.Minute {
		t.Fatalf("expected fixed idle timeout, got %s", server.IdleTimeout)
	}
	if server.WriteTimeout <= openClawAgentWaitMaxTimeout {
		t.Fatalf(
			"expected write timeout %s to exceed OpenClaw max agent.wait timeout %s",
			server.WriteTimeout,
			openClawAgentWaitMaxTimeout,
		)
	}
	if got, want := server.WriteTimeout-openClawAgentWaitMaxTimeout, openClawAgentWaitHTTPMargin; got != want {
		t.Fatalf("expected one-minute write timeout margin, got %s", got)
	}
}

func TestNewServerUsesAccountsCredentialIntrospection(t *testing.T) {
	introspection := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Service-Token"); got != "accounts-service-token" {
			t.Fatalf("service token = %q, want accounts-service-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":true}`))
	}))
	defer introspection.Close()

	t.Setenv("BRIDGE_ACCOUNTS_INTROSPECTION_URL", introspection.URL)
	t.Setenv("BRIDGE_ACCOUNTS_SERVICE_TOKEN", "accounts-service-token")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-service-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")

	server := NewServer()
	authValidator, ok := server.authService.(authorizationHeaderValidator)
	if !ok || !authValidator.ValidateAuthorizationHeader("Bearer user-tenant-token") {
		t.Fatal("expected Accounts-backed credential to be accepted")
	}
	if server.taskRunAuthService == nil || !server.taskRunAuthService.ValidateAuthorizationHeader("Bearer internal-service-token") {
		t.Fatal("expected dedicated internal service token to be configured")
	}
}
