package acp

import (
	"os"
	"testing"
)

func TestResolveSingleAgentForwardEndpointFromExampleConfig(t *testing.T) {
	// Set the config path to example/config.yaml relative to this test file
	os.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	defer os.Unsetenv("BRIDGE_CONFIG_PATH")

	catalog, order := newProductionProviderCatalog()
	if len(order) == 0 {
		t.Fatal("Expected non-empty provider order from example/config.yaml")
	}

	expectedEndpoints := map[string]string{
		"codex":    "ws://127.0.0.1:9001/acp",
		"opencode": "http://127.0.0.1:38992",
		"gemini":   "http://127.0.0.1:8791",
		"hermes":   "ws://127.0.0.1:3920",
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
			name: "preserves upstream endpoint",
			provider: syncedProvider{
				ProviderID: "custom",
				Endpoint:   "https://upstream-provider.example.com/acp/rpc",
			},
			want: "https://upstream-provider.example.com/acp/rpc",
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
