package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
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

func TestCodexCompatSendMessageWithoutProviderThreadStateDoesNotStartSession(t *testing.T) {
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
		methods = append(methods, stringValue(request["method"]))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result":  map[string]any{"id": "unexpected-thread"},
		})
	}))
	defer upstream.Close()

	compat := newProviderCompat(syncedProvider{
		ProviderID: "codex",
		Label:      "Codex",
		Endpoint:   upstream.URL,
		Enabled:    true,
	})
	_, err := compat.SendMessage(
		context.Background(),
		"session-missing",
		"thread-missing",
		map[string]any{
			"taskPrompt":       "continue",
			"workingDirectory": t.TempDir(),
		},
		nil,
	)
	if err == nil {
		t.Fatal("expected continuation unavailable error")
	}
	continuationErr, ok := asSessionContinuationUnavailableError(err)
	if !ok {
		t.Fatalf("expected continuation unavailable error, got %T %v", err, err)
	}
	if continuationErr.sessionID != "session-missing" ||
		continuationErr.threadID != "thread-missing" ||
		continuationErr.providerID != "codex" {
		t.Fatalf("unexpected continuation error context: %#v", continuationErr)
	}
	if len(methods) != 0 {
		t.Fatalf("session.message without provider state must not call upstream, got %#v", methods)
	}
}

func TestCodexCompatConvertsEmptyTurnResultToDisplayableFailure(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		result := map[string]any{}
		if stringValue(request["method"]) == "thread/start" {
			result["id"] = "codex-thread-1"
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
	if got := result["success"]; got != false {
		t.Fatalf("expected failure success flag, got %#v", result)
	}
	if got := result["error"]; got != "codex returned no displayable output" {
		t.Fatalf("expected displayable error, got %#v", result)
	}
}

func TestCodexCompatWaitsForTurnCompletedNotification(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer func() {
			_ = conn.Close()
		}()
		for {
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			method := stringValue(request["method"])
			switch method {
			case "initialize":
				if err := conn.WriteJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      request["id"],
					"result":  map[string]any{"protocolVersion": 1},
				}); err != nil {
					t.Fatalf("write initialize response: %v", err)
				}
			case "thread/start":
				if err := conn.WriteJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      request["id"],
					"result":  map[string]any{"id": "codex-thread-1"},
				}); err != nil {
					t.Fatalf("write thread response: %v", err)
				}
			case "turn/start":
				turn := map[string]any{
					"id":     "turn-1",
					"status": "inProgress",
					"items":  []any{},
				}
				if err := conn.WriteJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      request["id"],
					"result":  map[string]any{"turn": turn},
				}); err != nil {
					t.Fatalf("write turn response: %v", err)
				}
				if err := conn.WriteJSON(map[string]any{
					"method": "item/completed",
					"params": map[string]any{
						"item": map[string]any{
							"type":    "assistant_message",
							"content": []any{map[string]any{"text": "pong"}},
						},
					},
				}); err != nil {
					t.Fatalf("write item completed: %v", err)
				}
				turn["status"] = "completed"
				if err := conn.WriteJSON(map[string]any{
					"method": "turn/completed",
					"params": map[string]any{
						"threadId": "codex-thread-1",
						"turn":     turn,
					},
				}); err != nil {
					t.Fatalf("write turn completed: %v", err)
				}
			default:
				t.Fatalf("unexpected method %q", method)
			}
		}
	}))
	defer upstream.Close()

	compat := newProviderCompat(syncedProvider{
		ProviderID: "codex",
		Label:      "Codex",
		Endpoint:   "ws" + strings.TrimPrefix(upstream.URL, "http"),
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
		t.Fatalf("expected output pong after turn/completed, got %#v", result)
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

func TestExternalACPNotificationCollectorConvertsToolErrorToFailure(t *testing.T) {
	t.Parallel()

	collector := &externalACPNotificationCollector{}
	collector.observe(map[string]any{
		"method": "session.update",
		"params": map[string]any{
			"update": map[string]any{
				"sessionUpdate": "tool_error",
				"error":         true,
				"message":       "exec_command failed: Failed to create unified exec process",
			},
		},
	})

	result := collector.apply(map[string]any{})
	if got := result["success"]; got != false {
		t.Fatalf("expected failure result, got %#v", result)
	}
	if got := result["error"]; got != "exec_command failed: Failed to create unified exec process" {
		t.Fatalf("expected tool error text, got %#v", result)
	}
	if _, ok := result["output"]; ok {
		t.Fatalf("did not expect tool error to become output, got %#v", result)
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

func TestExternalACPNotificationCollectorFiltersCodexProtocolNoise(t *testing.T) {
	t.Parallel()

	collector := &externalACPNotificationCollector{}
	collector.observe(map[string]any{
		"method": "turn/started",
		"params": map[string]any{
			"turnId": "019dd328-fcf8-7b71-845c-6ad9a81f9e0a",
			"turn": map[string]any{
				"id":     "019dd328-fcf8-7b71-845c-6ad9a81f9e0a",
				"status": "inProgress",
			},
		},
	})
	collector.observe(map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"item": map[string]any{
				"type": "userMessage",
				"content": []any{
					map[string]any{
						"type": "input_text",
						"text": "hi Execution context:\n\n• target: agent\n\n• permission: default",
					},
				},
			},
		},
	})
	collector.observe(map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"item": map[string]any{
				"type": "agentMessage",
				"id":   "msg_0babee661b91dbe10169f06cd278308191bfb59772d03718dc",
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": "agentMessage msg_0babee661b91dbe10169f06cd278308191bfb59772d03718dc final_answer hi hi",
					},
				},
			},
		},
	})

	result := collector.apply(map[string]any{
		"output":  "ok",
		"summary": "ok",
		"message": "ok",
	})
	if got := result["output"]; got != "hi hi" {
		t.Fatalf("expected only final assistant text, got %#v", result)
	}
	if got := result["summary"]; got != "hi hi" {
		t.Fatalf("expected only final assistant summary, got %#v", result)
	}
}

func TestExternalACPNotificationCollectorIgnoresCodexCommentaryMessages(t *testing.T) {
	t.Parallel()

	collector := &externalACPNotificationCollector{}
	collector.observe(map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"item": map[string]any{
				"type": "agentMessage",
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": "commentary agentMessage msg_088265db975167a00169f0707edd688191a2174abeb1592aa3\nYou\nasked\nfor\na\nsimple\nterminal\n-style\nresponse\n,\nso\nI\n’m\nhandling\nit\ndirectly\n.\nYou asked for a simple terminal-style response, so I’m handling it directly.",
					},
				},
			},
		},
	})
	collector.observe(map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"item": map[string]any{
				"type": "agentMessage",
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": "hello\nhello",
					},
				},
			},
		},
	})

	result := collector.apply(map[string]any{})
	if got := result["output"]; got != "hello" {
		t.Fatalf("expected commentary to be hidden and duplicate final line collapsed, got %#v", result)
	}
}
