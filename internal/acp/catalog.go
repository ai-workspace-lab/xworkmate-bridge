package acp

import (
	"sync"
)

type CapabilityCatalog struct {
	mu sync.RWMutex

	ProviderCatalog           []any `json:"providerCatalog"`
	GatewayProviders          []any `json:"gatewayProviders"`
	AvailableExecutionTargets []any `json:"availableExecutionTargets"`
	ProviderProbeSummary      []any `json:"providerProbeSummary"`
}

func (c *CapabilityCatalog) Update(providers []any, targets []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ProviderCatalog = providers
	c.AvailableExecutionTargets = targets
}

func (c *CapabilityCatalog) Get() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := map[string]any{
		"singleAgent":               true,
		"multiAgent":                true,
		"providerCatalog":           append([]any(nil), c.ProviderCatalog...),
		"gatewayProviders":          append([]any(nil), c.GatewayProviders...),
		"availableExecutionTargets": append([]any(nil), c.AvailableExecutionTargets...),
		"providerProbeSummary":      append([]any(nil), c.ProviderProbeSummary...),
	}
	result["capabilities"] = map[string]any{
		"single_agent":              true,
		"multi_agent":               true,
		"providerCatalog":           append([]any(nil), c.ProviderCatalog...),
		"gatewayProviders":          append([]any(nil), c.GatewayProviders...),
		"availableExecutionTargets": append([]any(nil), c.AvailableExecutionTargets...),
	}
	return result
}
