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

func TestAgentRoutesAllowlist(t *testing.T) {
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
		{http.MethodPost, "/api/v1/agent/threads", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/agent/catalog", http.StatusMethodNotAllowed},
		{http.MethodPut, "/api/v1/agent/mcp", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/agent/threads/t1/briefing", http.StatusNotFound},
		{http.MethodGet, "/api/v1/agent/threads/00000000-0000-0000-0000-000000000000/items", http.StatusNotFound},
		{http.MethodGet, "/api/v1/agent/catalog/", http.StatusNotFound},
		{http.MethodGet, "/api/v1/agent/unknown", http.StatusNotFound},
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
		t.Fatalf("upstream called %d times for rejected routes", calls.Load())
	}
}

func TestAgentReadRoutesForwardAllowlistedQuery(t *testing.T) {
	type seen struct{ path, query, auth string }
	var got []seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, seen{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	server := newAgentIngestTestServer(upstream.URL)

	requests := []struct{ path, wantPath, wantQuery string }{
		{"/api/v1/agent/catalog?scope=github.com%2Facme%2Fw&limit=10&offset=5&token=leak", "/api/v1/agent/catalog", "limit=10&offset=5&scope=github.com%2Facme%2Fw"},
		{"/api/v1/agent/threads?state=all&limit=2&debug=1", "/api/v1/agent/threads", "limit=2&state=all"},
		{"/api/v1/agent/memory?q=xid8&kind=decision,pitfall&scope=s", "/api/v1/agent/memory", "kind=decision%2Cpitfall&q=xid8&scope=s"},
		{"/api/v1/agent/sync?cursor=MDow&limit=50&offset=9", "/api/v1/agent/sync", "cursor=MDow&limit=50"},
		{"/api/v1/agent/threads/0a1b2c3d-0000-4000-8000-00000000abcd/briefing?events=5&x=1", "/api/v1/agent/threads/0a1b2c3d-0000-4000-8000-00000000abcd/briefing", "events=5"},
	}
	for _, req := range requests {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, agentIngestRequest(http.MethodGet, req.path, ""))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %s = %d %s", req.path, response.Code, response.Body.String())
		}
		last := got[len(got)-1]
		if last.path != req.wantPath || last.query != req.wantQuery {
			t.Errorf("GET %s forwarded as %s?%s, want %s?%s", req.path, last.path, last.query, req.wantPath, req.wantQuery)
		}
		if last.auth != "Bearer qmd-ingest-token" {
			t.Errorf("GET %s Authorization = %q", req.path, last.auth)
		}
	}
}

func TestAgentReadOversizedResponseIsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Chunked (no Content-Length): the limit must still apply.
		flusher := w.(http.Flusher)
		chunk := strings.Repeat("a", 64*1024)
		for written := 0; written <= agentReadResponseMaxBytes; written += len(chunk) {
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	response := httptest.NewRecorder()
	newAgentIngestTestServer(upstream.URL).Handler().ServeHTTP(response, agentIngestRequest(http.MethodGet, agentCatalogPath, ""))
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "qmd_response_too_large") {
		t.Fatalf("response = %d %.120s", response.Code, response.Body.String())
	}
}

func newAgentMCPTestServer(upstreamURL string) *Server {
	server := newAgentIngestTestServer(upstreamURL)
	server.qmdMCPToken = "qmd-mcp-token"
	server.qmdMCPClient = newQMDMCPClient()
	return server
}

func TestAgentMCPPassesSessionHeadersAndSwapsCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			t.Fatalf("upstream path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer qmd-mcp-token" {
			t.Fatalf("Authorization = %q, want the QMD MCP credential", got)
		}
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"initialize"`) {
				w.Header().Set("Mcp-Session-Id", r.Header.Get("Mcp-Session-Id"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
				return
			}
			if r.Header.Get("Accept") != "application/json, text/event-stream" {
				t.Fatalf("Accept = %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Mcp-Session-Id", "session-123")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		case http.MethodDelete:
			if r.Header.Get("Mcp-Session-Id") != "session-123" {
				t.Fatalf("DELETE session = %q", r.Header.Get("Mcp-Session-Id"))
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer upstream.Close()
	server := newAgentMCPTestServer(upstream.URL)

	initialize := agentIngestRequest(http.MethodPost, agentMCPPath, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	initialize.Header.Set("Accept", "application/json, text/event-stream")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, initialize)
	if response.Code != http.StatusOK || response.Header().Get("Mcp-Session-Id") != "session-123" {
		t.Fatalf("initialize = %d session %q", response.Code, response.Header().Get("Mcp-Session-Id"))
	}

	list := agentIngestRequest(http.MethodPost, agentMCPPath, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list.Header.Set("Mcp-Session-Id", "session-123")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, list)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"tools"`) || response.Header().Get("Mcp-Session-Id") != "session-123" {
		t.Fatalf("tools/list = %d %s", response.Code, response.Body.String())
	}

	remove := agentIngestRequest(http.MethodDelete, agentMCPPath, "")
	remove.Header.Set("Mcp-Session-Id", "session-123")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, remove)
	if response.Code != http.StatusOK {
		t.Fatalf("DELETE = %d", response.Code)
	}
}

func TestAgentMCPStreamsEventStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, "event: message\ndata: {\"n\":1}\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	bridge := httptest.NewServer(newAgentMCPTestServer(upstream.URL).Handler())
	defer bridge.Close()
	request, _ := http.NewRequest(http.MethodGet, bridge.URL+agentMCPPath, nil)
	request.Header.Set("Authorization", "Bearer bridge-user-token")
	request.Header.Set("Mcp-Session-Id", "session-123")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || strings.Count(string(body), "event: message") != 3 {
		t.Fatalf("stream = %d %q", response.StatusCode, body)
	}
}

func TestAgentMCPRejectsBeforeForwarding(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	noToken := newAgentIngestTestServer(upstream.URL) // MCP credential not configured
	unauthorized := agentIngestRequest(http.MethodPost, agentMCPPath, `{}`)
	unauthorized.Header.Set("Authorization", "Bearer qmd-mcp-token")
	tooLarge := agentIngestRequest(http.MethodPost, agentMCPPath, strings.Repeat("a", agentMCPRequestMaxBytes+1))

	cases := []struct {
		name    string
		server  *Server
		request *http.Request
		want    int
	}{
		{"mcp token missing", noToken, agentIngestRequest(http.MethodPost, agentMCPPath, `{}`), http.StatusServiceUnavailable},
		{"caller uses the upstream token", newAgentMCPTestServer(upstream.URL), unauthorized, http.StatusUnauthorized},
		{"too large", newAgentMCPTestServer(upstream.URL), tooLarge, http.StatusRequestEntityTooLarge},
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

func TestQMDEndpointTarget(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8181":                     "http://127.0.0.1:8181/mcp",
		"http://127.0.0.1:8181/":                    "http://127.0.0.1:8181/mcp",
		"http://127.0.0.1:8181/api/v1/agent/ingest": "http://127.0.0.1:8181/mcp",
		"https://qmd.internal/base":                 "https://qmd.internal/base/mcp",
	}
	for base, want := range cases {
		target, err := qmdEndpointTarget(base, qmdMCPPath)
		if err != nil || target.String() != want {
			t.Errorf("qmdEndpointTarget(%q) = %v, %v; want %s", base, target, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "http://user:pw@x", "http://x?y=1", "not a url"} {
		if _, err := qmdEndpointTarget(bad, qmdMCPPath); err == nil {
			t.Errorf("qmdEndpointTarget(%q) accepted an invalid base", bad)
		}
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

func TestAgentCatalogForwardsWithQMDCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/qmd/api/v1/agent/catalog" {
			t.Fatalf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer qmd-ingest-token" {
			t.Fatalf("Authorization = %q, want the QMD ingest credential", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"catalog":{"pinnedTasks":[]}}`))
	}))
	defer upstream.Close()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/catalog", nil)
	request.Header.Set("Authorization", "Bearer bridge-user-token")
	response := httptest.NewRecorder()
	newAgentIngestTestServer(upstream.URL+"/qmd").Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"pinnedTasks"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}
