package acp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newTaskSessionProxyTestServer(upstreamURL string) *Server {
	return &Server{
		accountsSessionAPIURL: upstreamURL,
		accountsSessionClient: newAccountsSessionProxyClient(),
	}
}

func TestTaskSessionProxyForwardsCompatibleRequestAndResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/accounts/api/v1/sessions/session-1/messages" || r.URL.RawQuery != "source=portal" {
			t.Fatalf("upstream request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer account-session-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Fatalf("Cookie leaked upstream: %q", got)
		}
		if got := r.Header.Get("X-Service-Token"); got != "" {
			t.Fatalf("X-Service-Token leaked upstream: %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"clientRequestId":"request-1","text":"hello"}` {
			t.Fatalf("body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "accounts-secret=value")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sessionId":"session-1","snapshotVersion":2}`))
	}))
	defer upstream.Close()

	server := newTaskSessionProxyTestServer(upstream.URL + "/accounts")
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/session-1/messages?source=portal", strings.NewReader(`{"clientRequestId":"request-1","text":"hello"}`))
	request.Header.Set("Authorization", "Bearer account-session-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", "portal-session=secret")
	request.Header.Set("X-Service-Token", "must-not-forward")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusCreated || response.Body.String() != `{"sessionId":"session-1","snapshotVersion":2}` {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie leaked downstream: %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestTaskSessionProxyRouteAllowlist(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/api/v1/namespaces", true},
		{http.MethodGet, "/api/v1/namespaces/personal/sessions", true},
		{http.MethodPost, "/api/v1/namespaces/personal/sessions", true},
		{http.MethodGet, "/api/v1/sessions/session-1", true},
		{http.MethodGet, "/api/v1/sessions/session-1/events", true},
		{http.MethodPost, "/api/v1/sessions/session-1/messages", true},
		{http.MethodDelete, "/api/v1/sessions/session-1", false},
		{http.MethodPost, "/api/v1/sessions/session-1/events", false},
		{http.MethodGet, "/api/v1/admin/sessions", false},
		{http.MethodGet, "/api/v1/sessions/../admin", false},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			if got := taskSessionProxyRouteAllowed(test.method, test.path); got != test.want {
				t.Fatalf("allowed = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTaskSessionProxyRejectsUnconfiguredUpstream(t *testing.T) {
	server := newTaskSessionProxyTestServer("")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"accounts_session_api_unavailable"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestTaskSessionProxyRequiresBearerAuthorization(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	server := newTaskSessionProxyTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("status/calls = %d/%d", response.Code, calls.Load())
	}
}

func TestTaskSessionProxyDoesNotIntrospectAccountsBearer(t *testing.T) {
	var authorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"namespaces":[]}`))
	}))
	defer upstream.Close()
	server := newTaskSessionProxyTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	request.Header.Set("Authorization", "Bearer accounts-session-token")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK || authorization != "Bearer accounts-session-token" {
		t.Fatalf("status/authorization = %d/%q", response.Code, authorization)
	}
}

func TestTaskSessionProxyRejectsOversizedRequestBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	server := newTaskSessionProxyTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/session-1/messages", strings.NewReader(strings.Repeat("x", taskSessionProxyRequestMaxBytes+1)))
	request.Header.Set("Authorization", "Bearer bridge-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
		t.Fatalf("status/calls = %d/%d", response.Code, calls.Load())
	}
}

func TestTaskSessionProxyRejectsDeclaredOversizedResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4194305")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTaskSessionProxyTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"code":"accounts_response_too_large"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestTaskSessionProxyDoesNotFollowRedirects(t *testing.T) {
	var redirectedCalls atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedCalls.Add(1)
	}))
	defer redirectTarget.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirectTarget.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	server := newTaskSessionProxyTestServer(upstream.URL)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	request.Header.Set("Authorization", "Bearer bridge-token")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusTemporaryRedirect || redirectedCalls.Load() != 0 {
		t.Fatalf("status/redirect calls = %d/%d", response.Code, redirectedCalls.Load())
	}
}
