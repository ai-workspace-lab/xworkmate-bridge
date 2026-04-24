package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveSingleAgentForwardEndpointFromExampleConfig(t *testing.T) {
	// Set the config path to example/config.yaml relative to this test file
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")

	_, catalog, order := newProductionProviderCatalog()
	if len(order) == 0 {
		t.Fatal("Expected non-empty provider order from example/config.yaml")
	}

	expectedEndpoints := map[string]string{
		"codex":    "ws://127.0.0.1:9001/acp",
		"opencode": "http://127.0.0.1:38992/acp/rpc",
		"gemini":   "http://127.0.0.1:8791/acp/rpc",
		"hermes":   "ws://127.0.0.1:3920/acp",
	}

	for _, id := range order {
		id := id
		t.Run(id, func(t *testing.T) {
			provider, ok := catalog[id]
			if !ok {
				t.Errorf("Provider %s missing from catalog", id)
				return
			}
			if !provider.Enabled {
				t.Errorf("Provider %s should be enabled in example config", id)
			}

			want := expectedEndpoints[id]
			got := resolveSingleAgentForwardEndpoint(provider)
			if got != want {
				t.Errorf("resolveSingleAgentForwardEndpoint(%s) = %q, want %q (from example config)", id, got, want)
			}
		})
	}
}

func TestResolveSingleAgentForwardEndpointManual(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		provider syncedProvider
		want     string
	}{
		{
			name: "preserves http rpc endpoint",
			provider: syncedProvider{
				ProviderID: "custom",
				Endpoint:   "https://upstream-provider.example.com/acp/rpc",
			},
			want: "https://upstream-provider.example.com/acp/rpc",
		},
		{
			name: "normalizes http acp endpoint to rpc endpoint",
			provider: syncedProvider{
				ProviderID: "opencode",
				Endpoint:   "http://127.0.0.1:39992/acp",
			},
			want: "http://127.0.0.1:39992/acp/rpc",
		},
		{
			name: "normalizes websocket opencode endpoint to http rpc endpoint",
			provider: syncedProvider{
				ProviderID: "opencode",
				Endpoint:   "ws://127.0.0.1:39992/acp",
			},
			want: "http://127.0.0.1:39992/acp/rpc",
		},
		{
			name: "does not duplicate nested acp path",
			provider: syncedProvider{
				ProviderID: "opencode",
				Endpoint:   "http://127.0.0.1:39992/acp",
			},
			want: "http://127.0.0.1:39992/acp/rpc",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveSingleAgentForwardEndpoint(tc.provider); got != tc.want {
				t.Fatalf("resolveSingleAgentForwardEndpoint() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeAuthorizationHeader(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"":                 "",
		"Bearer bridge":    "Bearer bridge",
		"bridge-token":     "Bearer bridge-token",
		"   bridge-token ": "Bearer bridge-token",
	}
	for raw, want := range cases {
		raw, want := raw, want
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if got := normalizeAuthorizationHeader(raw); got != want {
				t.Fatalf("normalizeAuthorizationHeader(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}

func TestCodexCompatTranslatesSessionLifecycleToThreadAndTurnRPC(t *testing.T) {
	t.Parallel()

	var methods []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		method := stringValue(request["method"])
		methods = append(methods, method)
		result := map[string]any{}
		switch method {
		case "thread/start":
			result["id"] = "codex-thread-1"
		case "turn/start":
			result["output"] = "pong"
		default:
			t.Fatalf("unexpected codex upstream method %q", method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result":  result,
		})
	}))
	defer upstream.Close()

	compat := newProviderCompat(syncedProvider{
		ProviderID: "codex",
		Label:      "Codex",
		Endpoint:   upstream.URL,
		Enabled:    true,
	})
	result, err := compat.StartSession(
		context.Background(),
		"session-1",
		"thread-1",
		map[string]any{
			"taskPrompt":       "Reply with exactly pong",
			"workingDirectory": t.TempDir(),
		},
		nil,
	)
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if got := result["output"]; got != "pong" {
		t.Fatalf("expected pong output, got %#v", result)
	}
	if len(methods) != 2 || methods[0] != "thread/start" || methods[1] != "turn/start" {
		t.Fatalf("expected thread/start then turn/start, got %#v", methods)
	}
}

func TestExternalACPNotificationCollectorExtractsNestedSessionUpdateText(t *testing.T) {
	t.Parallel()

	collector := &externalACPNotificationCollector{}
	collector.observe(map[string]any{
		"method": "session.update",
		"params": map[string]any{
			"turnId": "turn-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{
					"text": "pong",
				},
			},
		},
	})

	result := collector.apply(map[string]any{})
	if got := result["output"]; got != "pong" {
		t.Fatalf("expected output pong, got %#v", result)
	}
	if got := result["turnId"]; got != "turn-1" {
		t.Fatalf("expected turnId turn-1, got %#v", result)
	}
}

func TestExternalACPNotificationCollectorPrefersStreamTextOverAckResult(t *testing.T) {
	t.Parallel()

	collector := &externalACPNotificationCollector{}
	collector.observe(map[string]any{
		"method": "session.update",
		"params": map[string]any{
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{
					"text": "pong",
				},
			},
		},
	})

	result := collector.apply(map[string]any{
		"output":  "ok",
		"summary": "ok",
		"message": "ok",
	})
	if got := result["output"]; got != "pong" {
		t.Fatalf("expected stream text to win over ack result, got %#v", result)
	}
}
