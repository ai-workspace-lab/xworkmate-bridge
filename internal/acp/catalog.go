package acp

import (
	"sync"
)

type CapabilityCatalog struct {
	mu sync.RWMutex
	
	ProviderCatalog          []any `json:"providerCatalog"`
	GatewayProviders         []any `json:"gatewayProviders"`
	AvailableExecutionTargets []any `json:"availableExecutionTargets"`
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
	
	return map[string]any{
		"singleAgent":               true,
		"providerCatalog":           c.ProviderCatalog,
		"gatewayProviders":          c.GatewayProviders,
		"availableExecutionTargets": c.AvailableExecutionTargets,
	}
}
