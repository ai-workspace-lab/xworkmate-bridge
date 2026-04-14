package acp

import (
	"strings"

	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

const (
	productionGatewayEndpointURL  = "wss://openclaw.svc.plus"
	productionCodexEndpointURL    = "https://acp-server.svc.plus/codex/acp/rpc"
	productionOpenCodeEndpointURL = "https://acp-server.svc.plus/opencode/acp/rpc"
	productionGeminiEndpointURL   = "https://acp-server.svc.plus/gemini/acp/rpc"
)

type syncedProvider struct {
	ProviderID          string
	Label               string
	Endpoint            string
	AuthorizationHeader string
	Enabled             bool
}

func bridgeUpstreamAuthorizationHeader() string {
	return normalizeAuthorizationHeader(
		firstNonEmptyString(
			strings.TrimSpace(shared.EnvOrDefault("INTERNAL_SERVICE_TOKEN", "")),
			strings.TrimSpace(shared.EnvOrDefault("BRIDGE_AUTH_TOKEN", "")),
		),
	)
}

func newProductionProviderCatalog() (map[string]syncedProvider, []string) {
	authorizationHeader := bridgeUpstreamAuthorizationHeader()
	providers := []syncedProvider{
		{
			ProviderID:          "codex",
			Label:               "Codex",
			Endpoint:            productionCodexEndpointURL,
			AuthorizationHeader: authorizationHeader,
			Enabled:             true,
		},
		{
			ProviderID:          "opencode",
			Label:               "OpenCode",
			Endpoint:            productionOpenCodeEndpointURL,
			AuthorizationHeader: authorizationHeader,
			Enabled:             true,
		},
		{
			ProviderID:          "gemini",
			Label:               "Gemini",
			Endpoint:            productionGeminiEndpointURL,
			AuthorizationHeader: authorizationHeader,
			Enabled:             true,
		},
	}
	catalog := make(map[string]syncedProvider, len(providers))
	order := make([]string, 0, len(providers))
	for _, provider := range providers {
		catalog[provider.ProviderID] = provider
		order = append(order, provider.ProviderID)
	}
	return catalog, order
}

func (s *Server) syncedProviderByID(providerID string) (syncedProvider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	provider, ok := s.providerCatalog[strings.TrimSpace(providerID)]
	if !ok || !provider.Enabled || strings.TrimSpace(provider.Endpoint) == "" {
		return syncedProvider{}, false
	}
	return provider, true
}

func (s *Server) availableProviders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ordered := make([]string, 0, len(s.providerOrder))
	for _, providerID := range s.providerOrder {
		provider, ok := s.providerCatalog[providerID]
		if !ok {
			continue
		}
		if !provider.Enabled || strings.TrimSpace(provider.Endpoint) == "" {
			continue
		}
		ordered = append(ordered, provider.ProviderID)
	}
	return ordered
}

func (s *Server) availableProviderCatalog() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]map[string]any, 0, len(s.providerOrder))
	for _, providerID := range s.providerOrder {
		provider, ok := s.providerCatalog[providerID]
		if !ok {
			continue
		}
		if !provider.Enabled || strings.TrimSpace(provider.Endpoint) == "" {
			continue
		}
		result = append(result, map[string]any{
			"providerId": provider.ProviderID,
			"label":      providerLabel(provider),
			"targets":    []string{"agent"},
		})
	}
	return result
}

func providerLabel(provider syncedProvider) string {
	if label := strings.TrimSpace(provider.Label); label != "" {
		return label
	}
	return provider.ProviderID
}

func availableGatewayProviderCatalog() []map[string]any {
	return []map[string]any{
		{
			"providerId": router.GatewayProviderOpenClaw,
			"label":      "OpenClaw",
			"targets":    []string{"gateway"},
			"providerDisplay": map[string]any{
				"logoEmoji": "🦞",
			},
		},
	}
}

func availableExecutionTargets(
	providerCatalog []map[string]any,
	gatewayProviders []map[string]any,
) []string {
	result := make([]string, 0, 2)
	if len(providerCatalog) > 0 {
		result = append(result, "agent")
	}
	if len(gatewayProviders) > 0 {
		result = append(result, "gateway")
	}
	return result
}
