package acp

import (
	"context"
	"os"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/memory"
)

// Bootstrap initializes the control plane components
func (s *Server) Bootstrap() {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, providerCatalog, providerOrder := newProductionProviderCatalogFromConfig(s.config)
	s.config = config
	s.providerOrder = providerOrder

	if s.gateway == nil {
		s.gateway = gatewayruntime.NewManager()
	}

	homeDir, _ := os.UserHomeDir()
	s.memoryService = memory.NewService(homeDir)

	s.routingEngine = &DefaultRoutingEngine{server: s}
	s.orchestrator = NewSessionOrchestrator(s)
	s.jobs = newJobManager(s)
	s.providers = make(map[string]ProviderCompat)
	s.sessions = make(map[string]*session)

	for id, p := range providerCatalog {
		if p.Enabled {
			s.providers[id] = newProviderCompat(p)
		}
	}

	// Build Initial Catalog
	s.catalog = &CapabilityCatalog{
		ProviderCatalog: make([]any, 0),
		GatewayProviders: []any{
			map[string]any{
				"providerId":      "openclaw",
				"label":           "OpenClaw",
				"targets":         []string{"gateway"},
				"providerDisplay": map[string]any{"logoEmoji": "🦞"},
			},
		},
		AvailableExecutionTargets: []any{"agent", "gateway"},
		ProviderProbeSummary:      make([]any, 0),
	}

	for _, id := range providerOrder {
		p, ok := providerCatalog[id]
		if !ok || !p.Enabled {
			continue
		}
		category := "native"
		if id == "gemini" || id == "hermes" {
			category = "protocol-adapter"
		}
		s.catalog.ProviderCatalog = append(s.catalog.ProviderCatalog, map[string]any{
			"providerId": id,
			"label":      p.Label,
			"targets":    []string{"agent"},
			"category":   category,
		})
		if compat, ok := s.providers[id]; ok {
			probe := compat.Probe(context.Background())
			s.catalog.ProviderProbeSummary = append(s.catalog.ProviderProbeSummary, map[string]any{
				"providerId": id,
				"available":  probe.Available,
				"status":     probe.Status,
			})
		}
	}
}

func (s *Server) getAvailableProviderIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for _, id := range s.providerOrder {
		if _, ok := s.providers[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids
}
