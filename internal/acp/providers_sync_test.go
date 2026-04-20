package acp

import (
	"encoding/json"
	"testing"

	"xworkmate-bridge/internal/shared"
)

func setTestBridgeProvider(server *Server, provider syncedProvider) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.providerCatalog == nil {
		server.providerCatalog = map[string]syncedProvider{}
	}
	server.providerCatalog[provider.ProviderID] = provider

	found := false
	for _, id := range server.providerOrder {
		if id == provider.ProviderID {
			found = true
			break
		}
	}
	if !found {
		server.providerOrder = append(server.providerOrder, provider.ProviderID)
	}
}

func TestCapabilitiesExposeBuiltInProductionProviderCatalog(t *testing.T) {
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")

	server := NewServer()
	response, err := server.handleRequest(shared.RPCRequest{
		Method: "acp.capabilities",
		Params: map[string]any{},
	}, nil)
	if err != nil {
		t.Fatalf("handleRequest error: %v", err)
	}

	capabilities := response
	if got := capabilities["singleAgent"]; got != true {
		t.Fatalf("expected singleAgent true, got %v", got)
	}

	catalog, ok := capabilities["providerCatalog"].([]Provider)
	if !ok {
		// Try fallback decoding if it was serialized
		data, _ := json.Marshal(capabilities["providerCatalog"])
		var providers []Provider
		if err := json.Unmarshal(data, &providers); err == nil {
			catalog = providers
		} else {
			t.Fatalf("expected providerCatalog array, got %T", capabilities["providerCatalog"])
		}
	}

	if len(catalog) < 3 {
		t.Fatalf("expected at least 3 production providers, got %d", len(catalog))
	}

	providers := make(map[string]Provider)
	for _, p := range catalog {
		providers[p.ProviderID] = p
	}

	if _, ok := providers["codex"]; !ok {
		t.Error("missing codex provider")
	}
	if _, ok := providers["opencode"]; !ok {
		t.Error("missing opencode provider")
	}
}

func TestProductionProviderCatalogFallsBackToBridgeAuthToken(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")

	catalog, _ := newProductionProviderCatalog()
	p, ok := catalog["codex"]
	if !ok {
		t.Fatal("missing codex")
	}

	if got := p.AuthorizationHeader; got != "Bearer bridge-token" {
		t.Fatalf("expected bearer header, got %q", got)
	}
}

func TestProductionProviderCatalogPrefersDedicatedBridgeAuthToken(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "dedicated-token")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "legacy-token")

	catalog, _ := newProductionProviderCatalog()
	p, ok := catalog["codex"]
	if !ok {
		t.Fatal("missing codex")
	}

	if got := p.AuthorizationHeader; got != "Bearer dedicated-token" {
		t.Fatalf("expected dedicated bearer header, got %q", got)
	}
}
