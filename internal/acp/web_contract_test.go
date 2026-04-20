package acp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestHTTPHandlerRootAndPingExposeRuntimeVersionInfo(t *testing.T) {
	t.Setenv("IMAGE", "ghcr.io/x-evor/xworkmate-bridge:0123456789abcdef0123456789abcdef01234567")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	handler := server.Handler()

	rootRecorder := httptest.NewRecorder()
	rootRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	handler.ServeHTTP(rootRecorder, rootRequest)

	if rootRecorder.Code != http.StatusOK {
		t.Fatalf("expected root 200, got %d", rootRecorder.Code)
	}
	if !strings.Contains(rootRecorder.Body.String(), "xworkmate-bridge is running") {
		t.Fatalf("expected root body to contain service banner, got %q", rootRecorder.Body.String())
	}

	pingRecorder := httptest.NewRecorder()
	pingRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/ping", nil)
	handler.ServeHTTP(pingRecorder, pingRequest)

	if pingRecorder.Code != http.StatusOK {
		t.Fatalf("expected ping 200, got %d", pingRecorder.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(pingRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode ping payload: %v", err)
	}

	if got := payload["status"]; got != "ok" {
		t.Fatalf("expected status ok, got %#v", got)
	}
	if got := payload["image"]; got != "ghcr.io/x-evor/xworkmate-bridge:0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("expected full image ref, got %#v", got)
	}
	if got := payload["tag"]; got != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("expected full image tag, got %#v", got)
	}
	if got := payload["commit"]; got != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("expected full image commit, got %#v", got)
	}
	if got := payload["version"]; got != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("expected full image version, got %#v", got)
	}
}

func TestHTTPHandlerBareAliasPathsExposeCapabilities(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	handler := server.Handler()

	cases := []struct {
		name     string
		path     string
		wantMode string
		wantID   string
	}{
		{name: "gateway", path: "/gateway/openclaw", wantMode: "gateway", wantID: "openclaw"},
		{name: "codex", path: "/acp-server/codex", wantMode: "agent", wantID: "codex"},
		{name: "opencode", path: "/acp-server/opencode", wantMode: "agent", wantID: "opencode"},
		{name: "gemini", path: "/acp-server/gemini", wantMode: "agent", wantID: "gemini"},
		{name: "hermes", path: "/acp-server/hermes", wantMode: "agent", wantID: "hermes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+tc.path, nil)
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", recorder.Code)
			}
			if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Fatalf("expected application/json content type, got %q", got)
			}

			var envelope map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode capability alias response: %v", err)
			}
			result := asMap(envelope["result"])
			if got := result["singleAgent"]; got != true {
				t.Fatalf("expected singleAgent true, got %#v", got)
			}
			if got := result["resolvedExecutionTarget"]; got != nil {
				t.Fatalf("did not expect resolvedExecutionTarget in alias response, got %#v", got)
			}
			if tc.wantMode == "gateway" {
				gatewayProviders := mustObjectList(t, result["gatewayProviders"])
				if len(gatewayProviders) != 1 {
					t.Fatalf("expected one gateway provider, got %#v", gatewayProviders)
				}
				if got := gatewayProviders[0]["providerId"]; got != tc.wantID {
					t.Fatalf("expected gateway provider %q, got %#v", tc.wantID, got)
				}
				return
			}
			providerCatalog := mustObjectList(t, result["providerCatalog"])
			found := false
			for _, provider := range providerCatalog {
				if provider["providerId"] == tc.wantID {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected provider %q in providerCatalog, got %#v", tc.wantID, providerCatalog)
			}
		})
	}
}

func TestHTTPHandlerBareAliasPathsServeRPC(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	handler := server.Handler()

	cases := []struct {
		name     string
		path     string
		wantMode string
		wantID   string
	}{
		{name: "gateway", path: "/gateway/openclaw", wantMode: "gateway", wantID: "openclaw"},
		{name: "codex", path: "/acp-server/codex", wantMode: "agent", wantID: "codex"},
		{name: "opencode", path: "/acp-server/opencode", wantMode: "agent", wantID: "opencode"},
		{name: "gemini", path: "/acp-server/gemini", wantMode: "agent", wantID: "gemini"},
		{name: "hermes", path: "/acp-server/hermes", wantMode: "agent", wantID: "hermes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+tc.path, strings.NewReader(`{"jsonrpc":"2.0","id":"rpc-1","method":"acp.capabilities"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer test-token")
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", recorder.Code)
			}
			if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Fatalf("expected application/json content type, got %q", got)
			}

			var envelope map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode rpc alias response: %v", err)
			}
			if got := envelope["id"]; got != "rpc-1" {
				t.Fatalf("expected rpc id rpc-1, got %#v", got)
			}
			result := asMap(envelope["result"])
			if tc.wantMode == "gateway" {
				gatewayProviders := mustObjectList(t, result["gatewayProviders"])
				if len(gatewayProviders) != 1 {
					t.Fatalf("expected one gateway provider, got %#v", gatewayProviders)
				}
				if got := gatewayProviders[0]["providerId"]; got != tc.wantID {
					t.Fatalf("expected gateway provider %q, got %#v", tc.wantID, got)
				}
				return
			}
			providerCatalog := mustObjectList(t, result["providerCatalog"])
			found := false
			for _, provider := range providerCatalog {
				if provider["providerId"] == tc.wantID {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected provider %q in providerCatalog, got %#v", tc.wantID, providerCatalog)
			}
		})
	}
}

func TestParseImageVersionInfoHandlesTaggedImageRef(t *testing.T) {
	info := parseImageVersionInfo("ghcr.io/x-evor/xworkmate-bridge:main-2026-04-12")

	if info.ImageRef != "ghcr.io/x-evor/xworkmate-bridge:main-2026-04-12" {
		t.Fatalf("expected full image ref, got %q", info.ImageRef)
	}
	if info.Tag != "main-2026-04-12" {
		t.Fatalf("expected tag main-2026-04-12, got %q", info.Tag)
	}
	if info.Commit != "" {
		t.Fatalf("expected empty commit for non-hex tag, got %q", info.Commit)
	}
	if info.Version != "main-2026-04-12" {
		t.Fatalf("expected version main-2026-04-12, got %q", info.Version)
	}
}

func TestHandleWebSocketRejectsUnknownOrigin(t *testing.T) {
	t.Setenv("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/acp", nil)
	request.Header.Set("Origin", "https://evil.example.com")

	server.HandleWebSocket(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("expected application/json content type, got %q", got)
	}
}

func TestHandleRPCAllowsPreflightForConfiguredOrigin(t *testing.T) {
	t.Setenv("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus,http://localhost:*")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1/acp/rpc", nil)
	request.Header.Set("Origin", "https://xworkmate.svc.plus")
	request.Header.Set("Access-Control-Request-Method", "POST")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://xworkmate.svc.plus" {
		t.Fatalf("expected allow origin header, got %q", got)
	}
}

func TestHandleRPCRequiresAuthorizationEvenWhenBridgeAuthTokenUnset(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"acp.capabilities"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when BRIDGE_AUTH_TOKEN is unset but no header provided, got %d", recorder.Code)
	}
}

func TestHandleRPCRequiresBearerAuthorizationWhenBridgeAuthTokenConfigured(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"acp.capabilities"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestHandleRPCRejectsUnknownOrigin(t *testing.T) {
	t.Setenv("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"acp.capabilities"}`),
	)
	request.Header.Set("Origin", "https://evil.example.com")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", recorder.Code)
	}
	var envelope map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if _, ok := envelope["error"]; !ok {
		t.Fatalf("expected JSON-RPC error envelope, got %v", envelope)
	}
}

func TestHandleRPCMethodErrorUsesJSONEnvelope(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/acp/rpc", nil)
	request.Header.Set("Authorization", "Bearer test")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("expected application/json content type, got %q", got)
	}
}

func TestHandleRPCCapabilitiesStillReturnsJSONResult(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"acp.capabilities"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("expected application/json content type, got %q", got)
	}
	if !strings.Contains(recorder.Body.String(), `"providerCatalog"`) {
		t.Fatalf("expected capabilities response, got %q", recorder.Body.String())
	}
}

func TestAuthorizedRejectsUnauthenticatedRequestsWhenBridgeAuthTokenUnset(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/acp", nil)
	if server.authorized(request) {
		t.Fatal("expected unauthenticated request to be rejected even if BRIDGE_AUTH_TOKEN is unset")
	}
}

func TestHandleRPCCapabilitiesReturnsCanonicalProviderContract(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"cap-1","method":"acp.capabilities"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}

	var envelope map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode capabilities response: %v", err)
	}

	result := asMap(envelope["result"])
	if got := result["singleAgent"]; got != true {
		t.Fatalf("expected singleAgent true, got %v", got)
	}
	availableTargets := mustStringList(t, result["availableExecutionTargets"])
	if !reflect.DeepEqual(availableTargets, []string{"agent", "gateway"}) {
		t.Fatalf("expected canonical execution targets, got %#v", availableTargets)
	}

	providerCatalog := mustObjectList(t, result["providerCatalog"])
	if len(providerCatalog) != 4 {
		t.Fatalf("expected 4 providers, got %#v", providerCatalog)
	}
	wantAgentIDs := []string{"codex", "opencode", "gemini", "hermes"}
	wantAgentLabels := []string{"Codex", "OpenCode", "Gemini", "Hermes"}
	for index, wantID := range wantAgentIDs {
		if got := providerCatalog[index]["providerId"]; got != wantID {
			t.Fatalf("expected provider %q at index %d, got %#v", wantID, index, providerCatalog)
		}
		if got := providerCatalog[index]["label"]; got != wantAgentLabels[index] {
			t.Fatalf("expected label %q at index %d, got %#v", wantAgentLabels[index], index, providerCatalog)
		}
		if targets := mustStringList(t, providerCatalog[index]["targets"]); !reflect.DeepEqual(targets, []string{"agent"}) {
			t.Fatalf("expected agent targets for %q, got %#v", wantID, targets)
		}
	}

	gatewayProviders := mustObjectList(t, result["gatewayProviders"])
	if len(gatewayProviders) != 1 {
		t.Fatalf("expected exactly one gateway provider, got %#v", gatewayProviders)
	}
	if got := gatewayProviders[0]["providerId"]; got != "openclaw" {
		t.Fatalf("expected gateway providerId openclaw, got %#v", gatewayProviders[0])
	}
	if got := gatewayProviders[0]["label"]; got != "OpenClaw" {
		t.Fatalf("expected gateway label OpenClaw, got %#v", gatewayProviders[0])
	}
	if targets := mustStringList(t, gatewayProviders[0]["targets"]); !reflect.DeepEqual(targets, []string{"gateway"}) {
		t.Fatalf("expected gateway targets, got %#v", targets)
	}
}

func TestHandleRPCSessionStartSucceedsWithExplicitProvider(t *testing.T) {
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer internal-test-token" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      "task-1",
			"result": map[string]any{
				"success":  true,
				"provider": "opencode",
				"output":   "pong",
			},
		})
	}))
	defer externalServer.Close()

	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-test-token")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID:          "opencode",
		Label:               "OpenCode",
		Endpoint:            externalServer.URL,
		AuthorizationHeader: "Bearer internal-test-token",
		Enabled:             true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"task-1","method":"session.start","params":{"sessionId":"s1","threadId":"t1","taskPrompt":"Reply with exactly pong","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"singleAgent","explicitProviderId":"opencode"}}}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer bridge-token")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"output":"pong"`) {
		t.Fatalf("expected pong output, got %q", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"provider":"opencode"`) {
		t.Fatalf("expected opencode provider, got %q", recorder.Body.String())
	}
}

func mustObjectList(t *testing.T, value any) []map[string]any {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("expected object list, got %#v", value)
	}
	items := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		items = append(items, asMap(item))
	}
	return items
}

func mustStringList(t *testing.T, value any) []string {
	t.Helper()
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			items = append(items, strings.TrimSpace(item.(string)))
		}
		return items
	default:
		t.Fatalf("expected string list, got %#v", value)
		return nil
	}
}

func TestHandleWebSocketRequiresBearerAuthorization(t *testing.T) {
	t.Setenv("ACP_ALLOWED_ORIGINS", "https://xworkmate.svc.plus")
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")

	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/acp", nil)
	request.Header.Set("Origin", "https://xworkmate.svc.plus")

	server.HandleWebSocket(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}
