package acp

import (
	"context"
)

type SessionNotificationSink func(map[string]any)

type ProviderProbeResult struct {
	Available bool   `json:"available"`
	Status    string `json:"status"`
}

// ProviderCompat 是 bridge 依赖的唯一 provider 兼容接口。
// stdio / 进程 / 协议细节必须收敛在 compat/runtime 内部。
type ProviderCompat interface {
	ID() string
	Metadata() map[string]any
	Probe(ctx context.Context) ProviderProbeResult
	StartSession(ctx context.Context, sessionID string, threadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error)
	SendMessage(ctx context.Context, sessionID string, threadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error)
	CancelSession(ctx context.Context, sessionID string) error
	CloseSession(ctx context.Context, sessionID string) error
}

// RoutingEngine 路由确权引擎
type RoutingEngine interface {
	Resolve(ctx context.Context, intent map[string]any) (RoutingResult, error)
}

type RoutingResult struct {
	TargetID          string   `json:"resolvedExecutionTarget"`
	ProviderID        string   `json:"resolvedProviderId"`
	GatewayProviderID string   `json:"resolvedGatewayProviderId"`
	Model             string   `json:"resolvedModel"`
	Skills            []string `json:"resolvedSkills"`
	Status            string   `json:"status"` // available, unavailable
	UnavailableCode   string   `json:"unavailableCode,omitempty"`
	UnavailableMsg    string   `json:"unavailableMessage,omitempty"`
	
	// Enhanced metadata for tests and advanced clients
	SkillResolutionSource string `json:"skillResolutionSource,omitempty"`
	NeedsSkillInstall     bool   `json:"needsSkillInstall,omitempty"`
	SkillInstallRequestID string `json:"skillInstallRequestId,omitempty"`
}
