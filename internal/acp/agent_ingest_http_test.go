package acp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"xworkmate-bridge/internal/service"
)

func newAgentIngestTestServer(upstreamURL string) *Server {
	return &Server{
		authService:     service.NewStaticTokenAuthService("bridge-user-token"),
		qmdIngestAPIURL: upstreamURL,
		qmdIngestToken:  "qmd-ingest-token",
		qmdIngestClient: newQMDIngestClient(),
	}
}

func agentIngestRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer bridge-user-token")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestAgentIngestForwardsWithQMDCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/qmd/api/v1/agent/ingest" || r.URL.RawQuery != "" {
			t.Fatalf("upstream request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer qmd-ingest-token" {
			t.Fatalf("Authorization = %q, want the QMD ingest credential", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Fatalf("Cookie leaked upstream: %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"source":"chatgpt-web"}` {
			t.Fatalf("body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "qmd=secret")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"threadId":"t1"}`))
	}))
	defer upstream.Close()

	request := agentIngestRequest(http.MethodPost, "/api/v1/agent/ingest?leak=1", `{"source":"chatgpt-web"}`)
	request.Header.Set("Cookie", "portal-session=secret")
	response := httptest.NewRecorder()
	newAgentIngestTestServer(upstream.URL+"/qmd").Handler().ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || response.Body.String() != `{"threadId":"t1"}` {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie leaked downstream: %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestAgentIngestIsOneWay(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	server := newAgentIngestTestServer(upstream.URL)

	tests := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/v1/agent/ingest", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/v1/agent/ingest", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/agent/threads", http.StatusNotFound},
		{http.MethodPost, "/api/v1/agent/threads/t1/briefing", http.StatusNotFound},
		{http.MethodPost, "/api/v1/agent/ingest/../threads", http.StatusNotFound},
	}
	for _, tt := range tests {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, agentIngestRequest(tt.method, tt.path, `{}`))
		if response.Code != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, response.Code, tt.want)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream called %d times for non-ingest requests", calls.Load())
	}
}

func TestAgentIngestRejectsBeforeForwarding(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	unauthorized := agentIngestRequest(http.MethodPost, agentIngestPath, `{}`)
	unauthorized.Header.Set("Authorization", "Bearer wrong")
	missingBearer := agentIngestRequest(http.MethodPost, agentIngestPath, `{}`)
	missingBearer.Header.Del("Authorization")
	notJSON := agentIngestRequest(http.MethodPost, agentIngestPath, `x=1`)
	notJSON.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tooLarge := agentIngestRequest(http.MethodPost, agentIngestPath, `{"x":"`+strings.Repeat("a", agentIngestRequestMaxBytes)+`"}`)

	cases := []struct {
		name    string
		server  *Server
		request *http.Request
		want    int
	}{
		{"wrong token", newAgentIngestTestServer(upstream.URL), unauthorized, http.StatusUnauthorized},
		{"missing bearer", newAgentIngestTestServer(upstream.URL), missingBearer, http.StatusUnauthorized},
		{"not json", newAgentIngestTestServer(upstream.URL), notJSON, http.StatusUnsupportedMediaType},
		{"too large", newAgentIngestTestServer(upstream.URL), tooLarge, http.StatusRequestEntityTooLarge},
		{"no url", &Server{authService: service.NewStaticTokenAuthService("bridge-user-token"), qmdIngestToken: "t", qmdIngestClient: newQMDIngestClient()}, agentIngestRequest(http.MethodPost, agentIngestPath, `{}`), http.StatusServiceUnavailable},
		{"no qmd token", &Server{authService: service.NewStaticTokenAuthService("bridge-user-token"), qmdIngestAPIURL: upstream.URL, qmdIngestClient: newQMDIngestClient()}, agentIngestRequest(http.MethodPost, agentIngestPath, `{}`), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		response := httptest.NewRecorder()
		tc.server.Handler().ServeHTTP(response, tc.request)
		if response.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, response.Code, response.Body.String(), tc.want)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream called %d times for rejected requests", calls.Load())
	}
}

func TestAgentIngestUpstreamDownIsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstreamURL := upstream.URL
	upstream.Close()

	response := httptest.NewRecorder()
	newAgentIngestTestServer(upstreamURL).Handler().ServeHTTP(response, agentIngestRequest(http.MethodPost, agentIngestPath, `{}`))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestAgentIngestDoesNotShadowTaskSessionRoutes(t *testing.T) {
	var path string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := &Server{accountsSessionAPIURL: upstream.URL, accountsSessionClient: newAccountsSessionProxyClient()}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	request.Header.Set("Authorization", "Bearer account-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || path != "/api/v1/namespaces" {
		t.Fatalf("task session route = %d path %q", response.Code, path)
	}
}
