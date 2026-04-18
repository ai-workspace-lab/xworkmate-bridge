package acp

import (
	"strings"

	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

const (
	productionGatewayEndpointURL  = "https://xworkmate-bridge.svc.plus/gateway/openclaw/"
	productionCodexEndpointURL    = "https://xworkmate-bridge.svc.plus/acp-server/codex/acp/rpc"
	productionOpenCodeEndpointURL = "https://xworkmate-bridge.svc.plus/acp-server/opencode/acp/rpc"
	productionGeminiEndpointURL   = "https://xworkmate-bridge.svc.plus/acp-server/gemini/acp/rpc"
)

func bridgeUpstreamAuthorizationHeader() string {
	return strings.TrimSpace(shared.EnvOrDefault("BRIDGE_AUTH_TOKEN", ""))
}

func newProductionProviderCatalog() (map[string]syncedProvider, []string) {
	authorizationHeader := bridgeUpstreamAuthorizationHeader()
	catalog := map[string]syncedProvider{
		"codex": {
			Provider: router.Provider{
				ProviderID: "codex",
				Label:      "Codex",
				Targets:    []string{router.ExecutionTargetAgent},
			},
			Endpoint:            strings.TrimSpace(shared.EnvOrDefault("OPENCLAW_CODEX_URL", productionCodexEndpointURL)),
			AuthorizationHeader: authorizationHeader,
		},
		"opencode": {
			Provider: router.Provider{
				ProviderID: "opencode",
				Label:      "OpenCode",
				Targets:    []string{router.ExecutionTargetAgent},
			},
			Endpoint:            strings.TrimSpace(shared.EnvOrDefault("OPENCLAW_OPENCODE_URL", productionOpenCodeEndpointURL)),
			AuthorizationHeader: authorizationHeader,
		},
		"gemini": {
			Provider: router.Provider{
				ProviderID: "gemini",
				Label:      "Gemini",
				Targets:    []string{router.ExecutionTargetAgent},
			},
			Endpoint:            strings.TrimSpace(shared.EnvOrDefault("OPENCLAW_GEMINI_URL", productionGeminiEndpointURL)),
			AuthorizationHeader: authorizationHeader,
		},
	}
	order := []string{"codex", "opencode", "gemini"}
	return catalog, order
}

func availableGatewayProviderCatalog() []router.Provider {
	return []router.Provider{
		{
			ProviderId: "openclaw",
			Label:      "OpenClaw",
			Targets:    []string{router.ExecutionTargetGateway},
			ProviderDisplay: &router.ProviderDisplay{
				LogoEmoji: "🦞",
			},
		},
	}
}

func availableExecutionTargets(
	providerCatalog map[string]syncedProvider,
	gatewayProviders []router.Provider,
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
