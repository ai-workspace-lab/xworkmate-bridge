package acp

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
	"xworkmate-bridge/internal/shared"
)

// Default production endpoints for XWorkmate managed bridge environment.
const (
	productionGatewayEndpointURL = "https://xworkmate-bridge.svc.plus/gateway/openclaw/"
)

type syncedProvider struct {
	ProviderID          string
	Label               string
	Endpoint            string
	AuthorizationHeader string
	Enabled             bool
}

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
	configPath := shared.EnvOrDefault("BRIDGE_CONFIG_PATH", "/app/config.yaml")

	if _, err := os.Stat(configPath); err == nil {
		data, err := os.ReadFile(configPath)
		if err == nil {
			_ = yaml.Unmarshal(data, config)
		}
	}
	return config
}

func resolveURL(yamlVal string, defaultVal string, envKeys ...string) string {
	val := strings.TrimSpace(yamlVal)
	if val != "" {
		return val
	}
	for _, key := range envKeys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return defaultVal
}

func bridgeUpstreamAuthorizationHeader() string {
	token := strings.TrimSpace(shared.EnvOrDefault("BRIDGE_AUTH_TOKEN", ""))
	if token == "" {
		token = strings.TrimSpace(shared.EnvOrDefault("INTERNAL_SERVICE_TOKEN", ""))
	}
	if token != "" && !strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return "Bearer " + token
	}
	return token
}

func newProductionProviderCatalog() (map[string]syncedProvider, []string) {
	config := loadBridgeConfig()
	authorizationHeader := bridgeUpstreamAuthorizationHeader()

	providers := []struct {
		id         string
		label      string
		yaml       string
		envKeys    []string
		defaultURL string
	}{
		{
			id:         "codex",
			label:      "Codex",
			yaml:       config.Upstream.CodexURL,
			envKeys:    []string{"CODEX_RPC_URL"},
			defaultURL: "https://xworkmate-bridge.svc.plus/acp-server/codex/acp/rpc",
		},
		{
			id:         "opencode",
			label:      "OpenCode",
			yaml:       config.Upstream.OpenCodeURL,
			envKeys:    []string{"OPENCODE_RPC_URL"},
			defaultURL: "https://xworkmate-bridge.svc.plus/acp-server/opencode/acp/rpc",
		},
		{
			id:         "gemini",
			label:      "Gemini",
			yaml:       config.Upstream.GeminiURL,
			envKeys:    []string{"GEMINI_RPC_URL"},
			defaultURL: "https://xworkmate-bridge.svc.plus/acp-server/gemini/acp/rpc",
		},
	}

	catalog := make(map[string]syncedProvider)
	var order []string

	for _, p := range providers {
		endpoint := resolveURL(p.yaml, p.defaultURL, p.envKeys...)
		catalog[p.id] = syncedProvider{
			ProviderID:          p.id,
			Label:               p.label,
			Endpoint:            endpoint,
			AuthorizationHeader: authorizationHeader,
			Enabled:             endpoint != "",
		}
		order = append(order, p.id)
	}

	return catalog, order
}

func (s *Server) syncedProviderByID(providerID string) (syncedProvider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providerCatalog[providerID]
	return p, ok
}

func (s *Server) availableProviderCatalog() []Provider {
	s.mu.Lock()
	defer s.mu.Unlock()

	var catalog []Provider
	for _, id := range s.providerOrder {
		if p, ok := s.providerCatalog[id]; ok && p.Enabled {
			catalog = append(catalog, Provider{
				ProviderID: p.ProviderID,
				Label:      p.Label,
				Targets:    []string{"agent"},
			})
		}
	}
	return catalog
}

func (s *Server) availableProviders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var providers []string
	for _, id := range s.providerOrder {
		if p, ok := s.providerCatalog[id]; ok && p.Enabled {
			providers = append(providers, p.ProviderID)
		}
	}
	return providers
}

type ProviderDisplay struct {
	LogoEmoji string `json:"logoEmoji,omitempty"`
}

type Provider struct {
	ProviderID      string           `json:"providerId"`
	Label           string           `json:"label"`
	Targets         []string         `json:"targets"`
	ProviderDisplay *ProviderDisplay `json:"providerDisplay,omitempty"`
}

func availableGatewayProviderCatalog() []Provider {
	return []Provider{
		{
			ProviderID: "openclaw",
			Label:      "OpenClaw",
			Targets:    []string{"gateway"},
			ProviderDisplay: &ProviderDisplay{
				LogoEmoji: "🦞",
			},
		},
	}
}

func availableExecutionTargets(
	providerCatalog []Provider,
	gatewayProviders []Provider,
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
