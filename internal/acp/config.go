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
		HermesURL   string `yaml:"hermes_url"`
	} `yaml:"upstream"`
	Distributed     DistributedConfig     `yaml:"distributed"`
	OpenClawGateway OpenClawGatewayConfig `yaml:"openclaw_gateway"`
}

type DistributedConfig struct {
	TaskForwardEndpoint string `yaml:"task_forward_endpoint"`
	TaskForwardToken    string `yaml:"task_forward_token"`
}

type OpenClawGatewayConfig struct {
	MaxActive    *int   `yaml:"max_active"`
	MaxQueued    *int   `yaml:"max_queued"`
	QueueTimeout string `yaml:"queue_timeout"`
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

func resolveURL(yamlVal string, envKeys ...string) string {
	val := strings.TrimSpace(yamlVal)
	if val != "" {
		return val
	}
	for _, key := range envKeys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func bridgeUpstreamAuthorizationHeader() string {
	token := bridgeSharedAuthToken()
	if token != "" && !strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return "Bearer " + token
	}
	return token
}

func bridgeSharedAuthToken() string {
	return strings.TrimSpace(shared.EnvOrDefault("BRIDGE_AUTH_TOKEN", ""))
}

func resolveDistributedTaskForwardEndpoint(config *BridgeConfig) string {
	yamlVal := ""
	if config != nil {
		yamlVal = config.Distributed.TaskForwardEndpoint
	}
	return resolveURL(yamlVal, "XWORKMATE_BRIDGE_TASK_FORWARD_ENDPOINT", "BRIDGE_TASK_FORWARD_ENDPOINT")
}

func resolveDistributedTaskForwardToken(config *BridgeConfig) string {
	if token := strings.TrimSpace(os.Getenv("XWORKMATE_BRIDGE_TASK_FORWARD_TOKEN")); token != "" {
		return token
	}
	if token := strings.TrimSpace(os.Getenv("BRIDGE_TASK_FORWARD_TOKEN")); token != "" {
		return token
	}
	if config != nil {
		if token := strings.TrimSpace(config.Distributed.TaskForwardToken); token != "" {
			return token
		}
	}
	return bridgeSharedAuthToken()
}

func newProductionProviderCatalog() (*BridgeConfig, map[string]syncedProvider, []string) {
	return newProductionProviderCatalogFromConfig(loadBridgeConfig())
}

func newProductionProviderCatalogFromConfig(config *BridgeConfig) (*BridgeConfig, map[string]syncedProvider, []string) {
	if config == nil {
		config = &BridgeConfig{}
	}
	authorizationHeader := bridgeUpstreamAuthorizationHeader()

	providers := []struct {
		id      string
		label   string
		yaml    string
		envKeys []string
	}{
		{
			id:      "codex",
			label:   "Codex",
			yaml:    config.Upstream.CodexURL,
			envKeys: []string{"CODEX_RPC_URL"},
		},
		{
			id:      "opencode",
			label:   "OpenCode",
			yaml:    config.Upstream.OpenCodeURL,
			envKeys: []string{"OPENCODE_RPC_URL"},
		},
		{
			id:      "gemini",
			label:   "Gemini",
			yaml:    config.Upstream.GeminiURL,
			envKeys: []string{"GEMINI_RPC_URL"},
		},
		{
			id:      "hermes",
			label:   "Hermes",
			yaml:    config.Upstream.HermesURL,
			envKeys: []string{"HERMES_RPC_URL"},
		},
	}

	catalog := make(map[string]syncedProvider)
	var order []string

	for _, p := range providers {
		endpoint := resolveURL(p.yaml, p.envKeys...)
		catalog[p.id] = syncedProvider{
			ProviderID:          p.id,
			Label:               p.label,
			Endpoint:            endpoint,
			AuthorizationHeader: authorizationHeader,
			Enabled:             endpoint != "",
		}
		order = append(order, p.id)
	}

	return config, catalog, order
}
