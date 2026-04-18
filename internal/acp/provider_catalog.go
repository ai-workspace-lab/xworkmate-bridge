package acp

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

// 默认生产端点
const (
	defaultGatewayURL  = "https://xworkmate-bridge.svc.plus/gateway/openclaw/"
	defaultCodexURL    = "https://xworkmate-bridge.svc.plus/acp-server/codex/acp/rpc"
	defaultOpenCodeURL = "https://xworkmate-bridge.svc.plus/acp-server/opencode/acp/rpc"
	defaultGeminiURL   = "https://xworkmate-bridge.svc.plus/acp-server/gemini/acp/rpc"
)

type BridgeConfig struct {
	Upstream struct {
		GatewayURL  string `yaml:"gateway_url"`
		CodexURL    string `yaml:"codex_url"`
		OpenCodeURL string `yaml:"opencode_url"`
		GeminiURL   string `yaml:"gemini_url"`
	} `yaml:"upstream"`
}

func loadBridgeConfig() *BridgeConfig {
	config := &BridgeConfig{}
	configPath := shared.EnvOrDefault("BRIDGE_CONFIG_PATH", "config.yaml")

	if _, err := os.Stat(configPath); err == nil {
		data, err := os.ReadFile(configPath)
		if err == nil {
			_ = yaml.Unmarshal(data, config)
		}
	}
	return config
}

func resolveURL(yamlVal, envKey, defaultVal string) string {
	val := strings.TrimSpace(yamlVal)
	if val != "" {
		return val
	}
	return strings.TrimSpace(shared.EnvOrDefault(envKey, defaultVal))
}

func bridgeUpstreamAuthorizationHeader() string {
	return strings.TrimSpace(shared.EnvOrDefault("BRIDGE_AUTH_TOKEN", ""))
}

func newProductionProviderCatalog() (map[string]syncedProvider, []string) {
	config := loadBridgeConfig()
	authorizationHeader := bridgeUpstreamAuthorizationHeader()

	catalog := map[string]syncedProvider{
		"codex": {
			Provider: router.Provider{
				ProviderID: "codex",
				Label:      "Codex",
				Targets:    []string{router.ExecutionTargetAgent},
			},
			Endpoint:            resolveURL(config.Upstream.CodexURL, "OPENCLAW_CODEX_URL", defaultCodexURL),
			AuthorizationHeader: authorizationHeader,
		},
		"opencode": {
			Provider: router.Provider{
				ProviderID: "opencode",
				Label:      "OpenCode",
				Targets:    []string{router.ExecutionTargetAgent},
			},
			Endpoint:            resolveURL(config.Upstream.OpenCodeURL, "OPENCLAW_OPENCODE_URL", defaultOpenCodeURL),
			AuthorizationHeader: authorizationHeader,
		},
		"gemini": {
			Provider: router.Provider{
				ProviderID: "gemini",
				Label:      "Gemini",
				Targets:    []string{router.ExecutionTargetAgent},
			},
			Endpoint:            resolveURL(config.Upstream.GeminiURL, "OPENCLAW_GEMINI_URL", defaultGeminiURL),
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

func resolveGatewayReportedRemoteAddress(server *Server, request any) string {
	config := loadBridgeConfig()
	rawURL := resolveURL(config.Upstream.GatewayURL, "OPENCLAW_GATEWAY_URL", defaultGatewayURL)

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
