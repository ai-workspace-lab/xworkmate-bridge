package acp

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
	"xworkmate-bridge/internal/shared"
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
	configPath := shared.EnvOrDefault("BRIDGE_CONFIG_PATH", "config.yaml")

	if _, err := os.Stat(configPath); err == nil {
		data, err := os.ReadFile(configPath)
		if err == nil {
			_ = yaml.Unmarshal(data, config)
		}
	}
	return config
}

func resolveURL(yamlVal, envKey string) string {
	val := strings.TrimSpace(yamlVal)
	if val != "" {
		return val
	}
	return strings.TrimSpace(shared.EnvOrDefault(envKey, ""))
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
		id     string
		label  string
		yaml   string
		envKey string
	}{
		{"codex", "Codex", config.Upstream.CodexURL, "OPENCLAW_CODEX_URL"},
		{"opencode", "OpenCode", config.Upstream.OpenCodeURL, "OPENCLAW_OPENCODE_URL"},
		{"gemini", "Gemini", config.Upstream.GeminiURL, "OPENCLAW_GEMINI_URL"},
	}

	catalog := make(map[string]syncedProvider)
	var order []string

	for _, p := range providers {
		endpoint := resolveURL(p.yaml, p.envKey)
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
