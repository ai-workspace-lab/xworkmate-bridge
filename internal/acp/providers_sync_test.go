package acp

import (
	"encoding/json"
	"testing"

	"xworkmate-bridge/internal/shared"
)

func setTestBridgeProvider(server *Server, provider syncedProvider) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.adapters == nil {
		server.adapters = make(map[string]ProviderAdapter)
	}
	server.adapters[provider.ProviderID] = &ProxyAdapter{
		providerID: provider.ProviderID,
		endpoint:   resolveSingleAgentForwardEndpoint(provider),
		authHeader: provider.AuthorizationHeader,
	}
	
	if server.catalog != nil {
		server.catalog.ProviderCatalog = append(server.catalog.ProviderCatalog, map[string]any{
			"providerId": provider.ProviderID,
			"label":      provider.Label,
			"targets":    []string{"agent"},
		})
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
	targets, ok := capabilities["availableExecutionTargets"].([]any)
	if !ok {
		// Try fallback decoding if it was serialized
		data, _ := json.Marshal(capabilities["availableExecutionTargets"])
		if err := json.Unmarshal(data, &targets); err != nil {
			t.Fatalf("expected availableExecutionTargets array, got %T", capabilities["availableExecutionTargets"])
		}
	}
	
	foundAgent := false
	for _, target := range targets {
		if target == "agent" {
			foundAgent = true
			break
		}
	}
	if !foundAgent {
		t.Fatalf("expected agent target in availableExecutionTargets, got %v", targets)
	}

	var catalog []map[string]any
	// Try decoding if it was serialized
	data, _ := json.Marshal(capabilities["providerCatalog"])
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("expected providerCatalog array, got %T", capabilities["providerCatalog"])
	}

	if len(catalog) < 4 {
		t.Fatalf("expected at least 4 production providers, got %d", len(catalog))
	}

	providers := make(map[string]map[string]any)
	for _, p := range catalog {
		providers[p["providerId"].(string)] = p
	}

	if _, ok := providers["codex"]; !ok {
		t.Error("missing codex provider")
	}
	if _, ok := providers["opencode"]; !ok {
		t.Error("missing opencode provider")
	}
	if _, ok := providers["gemini"]; !ok {
		t.Error("missing gemini provider")
	}
	if _, ok := providers["hermes"]; !ok {
		t.Error("missing hermes provider")
	}
}

func TestProductionProviderCatalogFallsBackToBridgeAuthToken(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-token")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")

	_, catalog, _ := newProductionProviderCatalog()
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

	_, catalog, _ := newProductionProviderCatalog()
	p, ok := catalog["codex"]
	if !ok {
		t.Fatal("missing codex")
	}

	if got := p.AuthorizationHeader; got != "Bearer dedicated-token" {
		t.Fatalf("expected dedicated bearer header, got %q", got)
	}
}

func TestProductionProviderCatalogIgnoresInternalServiceToken(t *testing.T) {
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "legacy-token")

	_, catalog, _ := newProductionProviderCatalog()
	p, ok := catalog["codex"]
	if !ok {
		t.Fatal("missing codex")
	}

	if got := p.AuthorizationHeader; got != "" {
		t.Fatalf("expected empty auth header when BRIDGE_AUTH_TOKEN is unset, got %q", got)
	}
}
