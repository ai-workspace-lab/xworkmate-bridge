package acp

import (
	"context"
	"xworkmate-bridge/internal/shared"
)

// ProviderAdapter 是所有 Upstream 的归一化接口
type ProviderAdapter interface {
	ID() string
	Metadata() map[string]any
	// Execute 处理 Start 和 Message 的执行语义
	Execute(ctx context.Context, sessionID string, threadID string, method string, params map[string]any) (<-chan SessionEvent, error)
	Cancel(ctx context.Context, sessionID string) error
	Probe(ctx context.Context) (bool, string)
}

// SessionEvent 归一化所有的中间输出和最终结果
type SessionEvent struct {
	Type      string           `json:"type"` // chunk, status, result, error
	Payload   map[string]any   `json:"payload"`
	Error     *shared.RPCError `json:"error,omitempty"`
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
