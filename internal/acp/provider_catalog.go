package acp

import (
	"strings"

	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

// 默认生产端点（仅作为最后的回退）
const (
	defaultGatewayURL  = "https://xworkmate-bridge.svc.plus/gateway/openclaw/"
	defaultCodexURL    = "https://xworkmate-bridge.svc.plus/acp-server/codex/acp/rpc"
	defaultOpenCodeURL = "https://xworkmate-bridge.svc.plus/acp-server/opencode/acp/rpc"
	defaultGeminiURL   = "https://xworkmate-bridge.svc.plus/acp-server/gemini/acp/rpc"
)

func bridgeUpstreamAuthorizationHeader() string {
	return strings.TrimSpace(shared.EnvOrDefault("BRIDGE_AUTH_TOKEN", ""))
}

func newProductionProviderCatalog() (map[string]syncedProvider, []string) {
	authorizationHeader := bridgeUpstreamAuthorizationHeader()

	// 全面支持通过环境变量配置端点
	catalog := map[string]syncedProvider{
		"codex": {
			Provider: router.Provider{
				ProviderID: "codex",
				Label:      "Codex",
				Targets:    []string{router.ExecutionTargetAgent},
			},
			Endpoint:            strings.TrimSpace(shared.EnvOrDefault("OPENCLAW_CODEX_URL", defaultCodexURL)),
			AuthorizationHeader: authorizationHeader,
		},
		"opencode": {
			Provider: router.Provider{
				ProviderID: "opencode",
				Label:      "OpenCode",
				Targets:    []string{router.ExecutionTargetAgent},
			},
			Endpoint:            strings.TrimSpace(shared.EnvOrDefault("OPENCLAW_OPENCODE_URL", defaultOpenCodeURL)),
			AuthorizationHeader: authorizationHeader,
		},
		"gemini": {
			Provider: router.Provider{
				ProviderID: "gemini",
				Label:      "Gemini",
				Targets:    []string{router.ExecutionTargetAgent},
			},
			Endpoint:            strings.TrimSpace(shared.EnvOrDefault("OPENCLAW_GEMINI_URL", defaultGeminiURL)),
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

// 获取上游 Gateway 报告地址
func resolveGatewayReportedRemoteAddress(server *Server, request any) string {
	// 优先使用环境变量配置
	rawURL := strings.TrimSpace(shared.EnvOrDefault("OPENCLAW_GATEWAY_URL", defaultGatewayURL))
	if strings.Contains(rawURL, "://") {
		parts := strings.Split(rawURL, "://")
		if len(parts) > 1 {
			hostPath := strings.Split(parts[1], "/")[0]
			if !strings.Contains(hostPath, ":") {
				return hostPath + ":443"
			}
			return hostPath
		}
	}
	return "xworkmate-bridge.svc.plus:443"
}
