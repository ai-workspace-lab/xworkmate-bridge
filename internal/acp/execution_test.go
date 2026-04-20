package acp

import (
	"os"
	"strings"
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
		"codex":    "https://xworkmate-bridge.svc.plus/acp-server/codex/acp/rpc",
		"opencode": "https://xworkmate-bridge.svc.plus/acp-server/opencode/acp/rpc",
		"gemini":   "https://xworkmate-bridge.svc.plus/acp-server/gemini/acp/rpc",
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
