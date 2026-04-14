package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xworkmate-bridge/internal/shared"
)

func setTestBridgeProvider(server *Server, provider syncedProvider) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.providerCatalog == nil {
		server.providerCatalog = map[string]syncedProvider{}
	}
	providerID := strings.TrimSpace(provider.ProviderID)
	provider.ProviderID = providerID
	server.providerCatalog[providerID] = provider
}

func TestCapabilitiesExposeBuiltInProductionProviderCatalog(t *testing.T) {
	server := NewServer()

	result, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "acp.capabilities",
		Params: map[string]any{},
	}, func(map[string]any) {})
	if rpcErr != nil {
		t.Fatalf("expected capabilities success, got %v", rpcErr)
	}
	providerCatalog, ok := result["providerCatalog"].([]map[string]any)
	if !ok {
		t.Fatalf("expected providerCatalog array, got %#v", result)
	}
	gatewayProviders, ok := result["gatewayProviders"].([]map[string]any)
	if !ok {
		t.Fatalf("expected gatewayProviders array, got %#v", result)
	}
	availableExecutionTargets, ok := result["availableExecutionTargets"].([]string)
	if !ok {
		t.Fatalf("expected availableExecutionTargets array, got %#v", result)
	}
	if len(providerCatalog) != 3 {
		t.Fatalf("expected 3 built-in providers, got %#v", providerCatalog)
	}
	if len(gatewayProviders) != 2 {
		t.Fatalf("expected 2 built-in gateway providers, got %#v", gatewayProviders)
	}
	if len(availableExecutionTargets) != 2 ||
		availableExecutionTargets[0] != "agent" ||
		availableExecutionTargets[1] != "gateway" {
		t.Fatalf("expected agent/gateway execution targets, got %#v", availableExecutionTargets)
	}
	wantOrder := []string{"codex", "opencode", "gemini"}
	wantLabels := []string{"Codex", "OpenCode", "Gemini"}
	for index, want := range wantOrder {
		if got := providerCatalog[index]["providerId"]; got != want {
			t.Fatalf("expected provider %q at index %d, got %#v", want, index, providerCatalog)
		}
		if got := providerCatalog[index]["label"]; got != wantLabels[index] {
			t.Fatalf("expected label %q at index %d, got %#v", wantLabels[index], index, providerCatalog)
		}
		targets, ok := providerCatalog[index]["targets"].([]string)
		if !ok || len(targets) != 1 || targets[0] != "agent" {
			t.Fatalf("expected agent target metadata at index %d, got %#v", index, providerCatalog[index]["targets"])
		}
	}
	wantGatewayOrder := []string{"local", "openclaw"}
	wantGatewayLabels := []string{"Local", "OpenClaw"}
	for index, want := range wantGatewayOrder {
		if got := gatewayProviders[index]["providerId"]; got != want {
			t.Fatalf("expected gateway provider %q at index %d, got %#v", want, index, gatewayProviders)
		}
		if got := gatewayProviders[index]["label"]; got != wantGatewayLabels[index] {
			t.Fatalf("expected gateway label %q at index %d, got %#v", wantGatewayLabels[index], index, gatewayProviders)
		}
		targets, ok := gatewayProviders[index]["targets"].([]string)
		if !ok || len(targets) != 1 || targets[0] != "gateway" {
			t.Fatalf("expected gateway target metadata at index %d, got %#v", index, gatewayProviders[index]["targets"])
		}
	}
	openClawDisplay, ok := gatewayProviders[1]["providerDisplay"].(map[string]any)
	if !ok {
		t.Fatalf("expected providerDisplay metadata for openclaw, got %#v", gatewayProviders[1])
	}
	if got := openClawDisplay["logoEmoji"]; got != "🦞" {
		t.Fatalf("expected openclaw logo emoji, got %#v", got)
	}
}

func TestProductionProviderCatalogFallsBackToBridgeAuthToken(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-auth-token")

	catalog, order := newProductionProviderCatalog()
	if len(order) != 3 {
		t.Fatalf("expected 3 providers in order, got %#v", order)
	}
	for _, providerID := range order {
		provider, ok := catalog[providerID]
		if !ok {
			t.Fatalf("expected provider %q in catalog", providerID)
		}
		if got := provider.AuthorizationHeader; got != "Bearer bridge-auth-token" {
			t.Fatalf("expected fallback bearer header for %q, got %q", providerID, got)
		}
	}
}

func TestProvidersSyncMethodIsRemovedFromProductionFlow(t *testing.T) {
	server := NewServer()
	_, rpcErr := server.handleRequest(shared.RPCRequest{
		Method: "xworkmate.providers.sync",
	}, func(map[string]any) {})
	if rpcErr == nil {
		t.Fatalf("expected xworkmate.providers.sync to be unavailable")
	}
	if rpcErr.Code != -32601 {
		t.Fatalf("expected unknown method error, got %#v", rpcErr)
	}
	if !strings.Contains(rpcErr.Message, "xworkmate.providers.sync") {
		t.Fatalf("expected method name in error, got %#v", rpcErr)
	}
}

func TestExecuteSessionTaskUsesBuiltInProductionProvider(t *testing.T) {
	var lastForwardedParams map[string]any
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acp/rpc" {
			http.NotFound(w, r)
			return
		}
		defer func() {
			_ = r.Body.Close()
		}()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		lastForwardedParams = asMap(request["params"])
		method, _ := request["method"].(string)
		switch method {
		case "session.start":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request["id"],
				"result": map[string]any{
					"success":  true,
					"output":   "external-provider-ok",
					"turnId":   "turn-external",
					"provider": "codex",
					"mode":     "single-agent",
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request["id"],
				"result":  map[string]any{"ok": true},
			})
		}
	}))
	defer externalServer.Close()

	server := NewServer()
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-test-token")
	setTestBridgeProvider(server, syncedProvider{
		ProviderID:          "codex",
		Label:               "Codex",
		Endpoint:            externalServer.URL,
		AuthorizationHeader: "Bearer internal-test-token",
		Enabled:             true,
	})

	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-external",
				"threadId":         "thread-external",
				"taskPrompt":       "hello from external provider",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":             "explicit",
					"explicitExecutionTarget": "singleAgent",
					"explicitProviderId":      "codex",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected success, got rpc error: %v", rpcErr)
	}
	if got := response["output"]; got != "external-provider-ok" {
		t.Fatalf("expected external provider output, got %#v", response)
	}
	if got := response["resolvedProviderId"]; got != "codex" {
		t.Fatalf("expected resolved provider codex, got %#v", response)
	}
	if _, exists := lastForwardedParams["metadata"]; exists {
		t.Fatalf("expected metadata to be stripped for external provider request, got %#v", lastForwardedParams)
	}
}

func TestExecuteSessionTaskUsesBridgeAuthTokenFallbackForBuiltInProvider(t *testing.T) {
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer bridge-auth-token" {
			t.Fatalf("expected fallback bearer auth header, got %q", got)
		}
		defer func() {
			_ = r.Body.Close()
		}()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result": map[string]any{
				"success": true,
				"output":  "bridge-auth-token-ok",
			},
		})
	}))
	defer externalServer.Close()

	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-auth-token")

	server := NewServer()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID:          "codex",
		Label:               "Codex",
		Endpoint:            externalServer.URL,
		AuthorizationHeader: bridgeUpstreamAuthorizationHeader(),
		Enabled:             true,
	})

	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-bridge-auth-fallback",
				"threadId":         "thread-bridge-auth-fallback",
				"taskPrompt":       "hello from bridge auth fallback",
				"workingDirectory": t.TempDir(),
				"routing": map[string]any{
					"routingMode":             "explicit",
					"explicitExecutionTarget": "singleAgent",
					"explicitProviderId":      "codex",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected success, got rpc error: %v", rpcErr)
	}
	if got := response["output"]; got != "bridge-auth-token-ok" {
		t.Fatalf("expected fallback provider output, got %#v", response)
	}
}

func TestExecuteSessionTaskEnrichesExternalProviderResultWithArtifactsAndRemoteMetadata(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workingDir, "outputs"), 0o755); err != nil {
		t.Fatalf("mkdir outputs: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(workingDir, "outputs", "report.txt"),
		[]byte("artifact-body"),
		0o644,
	); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acp/rpc" {
			http.NotFound(w, r)
			return
		}
		defer func() {
			_ = r.Body.Close()
		}()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result": map[string]any{
				"success":                  true,
				"output":                   "external-provider-ok",
				"turnId":                   "turn-external-artifacts",
				"provider":                 "claude",
				"mode":                     "single-agent",
				"resolvedWorkingDirectory": "/remote/threads/task-42",
				"resolvedWorkspaceRefKind": "remotePath",
			},
		})
	}))
	defer externalServer.Close()

	server := NewServer()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID:          "codex",
		Label:               "Codex",
		Endpoint:            externalServer.URL,
		AuthorizationHeader: "Bearer internal-test-token",
		Enabled:             true,
	})

	response, rpcErr := server.executeSessionTask(task{
		req: shared.RPCRequest{
			Method: "session.start",
			Params: map[string]any{
				"sessionId":        "session-external-artifacts",
				"threadId":         "thread-external-artifacts",
				"taskPrompt":       "hello from external provider",
				"workingDirectory": workingDir,
				"routing": map[string]any{
					"routingMode":             "explicit",
					"explicitExecutionTarget": "singleAgent",
					"explicitProviderId":      "codex",
				},
			},
		},
	})
	if rpcErr != nil {
		t.Fatalf("expected success, got rpc error: %v", rpcErr)
	}
	if got := response["remoteWorkingDirectory"]; got != "/remote/threads/task-42" {
		t.Fatalf("expected remoteWorkingDirectory to be preserved, got %#v", got)
	}
	if got := response["remoteWorkspaceRefKind"]; got != "remotePath" {
		t.Fatalf("expected remoteWorkspaceRefKind remotePath, got %#v", got)
	}
	artifacts, ok := response["artifacts"].([]map[string]any)
	if !ok || len(artifacts) == 0 {
		t.Fatalf("expected enriched artifacts, got %#v", response["artifacts"])
	}
	artifact := artifacts[0]
	if got := artifact["relativePath"]; got != "outputs/report.txt" {
		t.Fatalf("expected relativePath outputs/report.txt, got %#v", got)
	}
	if got := artifact["content"]; got != "artifact-body" {
		t.Fatalf("expected inline artifact content, got %#v", got)
	}
	if got := artifact["encoding"]; got != "utf8" {
		t.Fatalf("expected utf8 artifact encoding, got %#v", got)
	}
	remoteExecution, ok := response["remoteExecution"].(map[string]any)
	if !ok {
		t.Fatalf("expected remoteExecution metadata, got %#v", response["remoteExecution"])
	}
	if got := remoteExecution["remoteWorkingDirectory"]; got != "/remote/threads/task-42" {
		t.Fatalf("expected remoteExecution remoteWorkingDirectory, got %#v", got)
	}
}

func TestRunSingleAgentRequiresAdvertisedProvider(t *testing.T) {
	server := NewServer()
	session := server.getOrCreateSession("session-local", "thread-local")
	result := server.runSingleAgent(
		context.Background(),
		"session.start",
		session,
		map[string]any{
			"provider":         "claude",
			"taskPrompt":       "hello",
			"workingDirectory": filepath.Join(t.TempDir(), "missing"),
		},
		"turn-local",
		func(map[string]any) {},
	)
	if result.err != nil {
		t.Fatalf("expected structured response, got rpc error: %v", result.err)
	}
	if success, _ := result.response["success"].(bool); success {
		t.Fatalf("expected unavailable response, got %#v", result.response)
	}
	if got := result.response["error"]; got != "provider is not advertised by the bridge" {
		t.Fatalf("expected provider unavailable error, got %#v", result.response)
	}
}

func TestHandleRPCRequiresExplicitBearerForExternalProvider(t *testing.T) {
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer synced-provider-token" {
			t.Fatalf("expected explicit synced provider bearer header, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      "run-auth",
			"result": map[string]any{
				"success": true,
				"output":  "forwarded-auth-ok",
			},
		})
	}))
	defer externalServer.Close()

	t.Setenv("INTERNAL_SERVICE_TOKEN", "synced-provider-token")
	server := NewServer()
	setTestBridgeProvider(server, syncedProvider{
		ProviderID:          "codex",
		Label:               "Codex",
		Endpoint:            externalServer.URL,
		AuthorizationHeader: "Bearer synced-provider-token",
		Enabled:             true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/acp/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":"run-auth","method":"session.start","params":{"sessionId":"s1","threadId":"t1","taskPrompt":"hello","workingDirectory":"`+t.TempDir()+`","routing":{"routingMode":"explicit","explicitExecutionTarget":"singleAgent","explicitProviderId":"codex"}}}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer bridge-token")

	server.HandleRPC(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "forwarded-auth-ok") {
		t.Fatalf("expected forwarded provider response, got %q", recorder.Body.String())
	}
}

func TestExternalACPNotificationCollectorSynthesizesOutputAndWorkspace(t *testing.T) {
	collector := &externalACPNotificationCollector{}
	collector.observe(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session.update",
		"params": map[string]any{
			"sessionId":                "session-streamed",
			"threadId":                 "thread-streamed",
			"turnId":                   "turn-streamed",
			"type":                     "delta",
			"delta":                    "streamed external output",
			"resolvedWorkingDirectory": "/tmp/thread-streamed",
			"pending":                  false,
			"error":                    false,
		},
	})

	result := collector.apply(map[string]any{
		"success": true,
	})

	if got := result["output"]; got != "streamed external output" {
		t.Fatalf("expected synthesized output from notifications, got %#v", result)
	}
	if got := result["summary"]; got != "streamed external output" {
		t.Fatalf("expected synthesized summary from notifications, got %#v", result)
	}
	if got := result["turnId"]; got != "turn-streamed" {
		t.Fatalf("expected synthesized turnId, got %#v", result)
	}
	if got := result["resolvedWorkingDirectory"]; got != "/tmp/thread-streamed" {
		t.Fatalf("expected synthesized working directory, got %#v", result)
	}
}
