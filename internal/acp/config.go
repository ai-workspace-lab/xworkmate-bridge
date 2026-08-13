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
	TaskForwardToken  string                      `yaml:"task_forward_token"`
	Topology          string                      `yaml:"topology"`
	LocalNodeID       string                      `yaml:"local_node_id"`
	TaskForwardPeerID string                      `yaml:"task_forward_peer_id"`
	Nodes             []DistributedNodeConfig     `yaml:"nodes"`
	Forwarding        DistributedForwardingConfig `yaml:"forwarding"`
}

type DistributedNodeConfig struct {
	ID             string   `yaml:"id"`
	Role           string   `yaml:"role"`
	Zone           string   `yaml:"zone"`
	PublicBaseURL  string   `yaml:"public_base_url"`
	BridgeEndpoint string   `yaml:"bridge_endpoint"`
	Capabilities   []string `yaml:"capabilities"`
}

type DistributedForwardingConfig struct {
	HopLimit      int                            `yaml:"hop_limit"`
	DefaultAction string                         `yaml:"default_action"`
	Rules         []DistributedForwardRuleConfig `yaml:"rules"`
	Routes        []DistributedRouteConfig       `yaml:"routes"`
}

type DistributedForwardRuleConfig struct {
	Methods []string                       `yaml:"methods"`
	Target  DistributedForwardTargetConfig `yaml:"target"`
}

type DistributedForwardTargetConfig struct {
	NodeID   string                           `yaml:"node_id"`
	Selector DistributedForwardSelectorConfig `yaml:"selector"`
	Strategy string                           `yaml:"strategy"`
}

type DistributedForwardSelectorConfig struct {
	Role       string `yaml:"role"`
	Zone       string `yaml:"zone"`
	Capability string `yaml:"capability"`
}

type DistributedRouteConfig struct {
	TargetNodeID  string `yaml:"target_node_id"`
	NextHopNodeID string `yaml:"next_hop_node_id"`
}

type OpenClawGatewayConfig struct {
	MaxActive                *int   `yaml:"max_active"`
	MaxQueued                *int   `yaml:"max_queued"`
	QueueTimeout             string `yaml:"queue_timeout"`
	MaxAllowedSilentDuration string `yaml:"max_allowed_silent_duration"`
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
	if explicit := strings.TrimSpace(os.Getenv("UPSTREAM_AUTHORIZATION_HEADER")); explicit != "" {
		return explicit
	}
	token := bridgePublicAuthToken()
	if token != "" && !strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return "Bearer " + token
	}
	return token
}

// bridgePublicAuthToken is retained for provider/upstream compatibility. It
// is separate from Accounts credential introspection, which authenticates
// inbound app requests when BRIDGE_ACCOUNTS_INTROSPECTION_URL is configured.
func bridgePublicAuthToken() string {
	if token := strings.TrimSpace(os.Getenv("AI_WORKSPACE_AUTH_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("BRIDGE_AUTH_TOKEN"))
}

func bridgeSharedAuthToken() string {
	return bridgePublicAuthToken()
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
	return ""
}

// bridgeGatewayServiceToken is only for bridge-to-OpenClaw service traffic.
// A dedicated token wins. The legacy public-token fallback is retained only
// for standalone/dev deployments; UAT config supplies a dedicated value.
func bridgeGatewayServiceToken() string {
	if token := strings.TrimSpace(os.Getenv("OPENCLAW_GATEWAY_SERVICE_TOKEN")); token != "" {
		return token
	}
	return bridgePublicAuthToken()
}

func defaultDistributedNodes() []DistributedNodeConfig {
	return []DistributedNodeConfig{
		{
			ID:             "xworkmate-bridge",
			Role:           "primary",
			PublicBaseURL:  "https://xworkmate-bridge.svc.plus",
			BridgeEndpoint: "http://172.29.10.1:8787",
		},
		{
			ID:             "cn-xworkmate-bridge",
			Role:           "edge",
			PublicBaseURL:  "https://cn-xworkmate-bridge.svc.plus",
			BridgeEndpoint: "http://172.29.10.2:8787",
		},
	}
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
