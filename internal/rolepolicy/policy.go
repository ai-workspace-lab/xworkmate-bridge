// Package rolepolicy holds the editable role policy and the pure selector that
// turns a task contract into one executor + model decision or an explicit
// rejection. It never dispatches, retries or falls back on its own.
package rolepolicy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Role identifiers. Roles describe how a task is organised and accepted; they
// never grant permissions and never imply that one model is stronger.
const (
	RoleChat       = "chat"
	RoleWorker     = "worker"
	RoleEngineer   = "engineer"
	RoleArchitect  = "architect"
	RoleResearcher = "researcher"
	RoleSpecialist = "specialist"
)

// Evidence states for a capability fact.
const (
	StateVerified = "verified"
	StateDeclared = "declared"
	StateUnknown  = "unknown"
)

// Budget modes.
const (
	// BudgetModeDisabled refuses every paid dispatch. It is the default.
	BudgetModeDisabled = "disabled"
	// BudgetModeGatewayQuota relies on the connection's own pre-deducted token
	// quota (new-api) as the hard cap. The policy author opts in explicitly.
	BudgetModeGatewayQuota = "gateway_quota"
)

type Policy struct {
	Version     string                 `json:"version"`
	Enabled     bool                   `json:"enabled"`
	Roles       map[string]RoleProfile `json:"roles"`
	Executors   map[string]Executor    `json:"executors"`
	Connections map[string]Connection  `json:"connections"`
	Models      map[string]ModelEntry  `json:"models"`
	Budget      Budget                 `json:"budget"`
	Limits      Limits                 `json:"limits"`
}

type RoleProfile struct {
	Enabled bool `json:"enabled"`
	// Preferences is the user-confirmed model order (keys of Policy.Models).
	Preferences []string `json:"preferences"`
	// Executors lists executor keys allowed for this role, in order.
	Executors []string `json:"executors"`
	// RequiredModelCapabilities must be verified on the model binding.
	RequiredModelCapabilities []string `json:"required_model_capabilities"`
	// RequiredExecutorCapabilities must be verified on the executor.
	RequiredExecutorCapabilities []string `json:"required_executor_capabilities"`
	RequiresWorkspace            bool     `json:"requires_workspace"`
	RequiresSpecialty            bool     `json:"requires_specialty"`
	// Tools is the executor tool whitelist for this role.
	Tools []string `json:"tools"`
}

type Executor struct {
	// ProviderID is the bridge provider that runs this executor.
	ProviderID string `json:"provider_id"`
	// Connection is the model connection the executor is configured against.
	Connection string `json:"connection"`
	// ModelOptionTemplate maps a gateway model ID to the executor's advertised
	// ACP model option value; "{model_id}" is replaced verbatim.
	ModelOptionTemplate string          `json:"model_option_template"`
	Capabilities        map[string]Fact `json:"capabilities"`
}

type Connection struct {
	BaseURL string `json:"base_url"`
	// TokenEnv names the environment variable materialised from a SecretRef.
	TokenEnv string `json:"token_env"`
	// AllowedDataClasses lists data classes that may be sent to this
	// connection and its upstream providers.
	AllowedDataClasses []string `json:"allowed_data_classes"`
}

type ModelEntry struct {
	DisplayName string `json:"display_name"`
	// CandidateOnly entries are listed for evaluation and can never appear in
	// a role preference list, so they cannot become an implicit fallback.
	CandidateOnly bool `json:"candidate_only"`
	// Bindings is keyed by connection ID. A nil GatewayModelID means the
	// model is pending_mapping on that connection.
	Bindings map[string]ModelBinding `json:"bindings"`
}

type ModelBinding struct {
	GatewayModelID *string         `json:"gateway_model_id"`
	Capabilities   map[string]Fact `json:"capabilities"`
	// ContextWindow is the evidence-backed token window; Value must be a number.
	ContextWindow Fact `json:"context_window"`
}

type Fact struct {
	Value      any       `json:"value"`
	State      string    `json:"state"`
	Source     string    `json:"source"`
	CheckedAt  time.Time `json:"checked_at"`
	ValidUntil time.Time `json:"valid_until"`
}

type Budget struct {
	Mode string `json:"mode"`
}

type Limits struct {
	// ContextReserveTokens is added to the prompt estimate for system prompt,
	// tool payloads, output reserve and safety margin.
	ContextReserveTokens int `json:"context_reserve_tokens"`
	// CatalogMaxAge bounds how old a live catalog snapshot may be.
	CatalogMaxAgeSeconds int `json:"catalog_max_age_seconds"`
}

// Verified reports whether the fact is verified and still valid at now.
func (f Fact) Verified(now time.Time) bool {
	if f.State != StateVerified {
		return false
	}
	if f.ValidUntil.IsZero() || !now.Before(f.ValidUntil) {
		return false
	}
	return true
}

// EffectiveState collapses expired verified facts back to unknown.
func (f Fact) EffectiveState(now time.Time) string {
	switch f.State {
	case StateVerified:
		if f.Verified(now) {
			return StateVerified
		}
		return StateUnknown
	case StateDeclared:
		return StateDeclared
	default:
		return StateUnknown
	}
}

func (f Fact) intValue() (int, bool) {
	switch v := f.Value.(type) {
	case float64:
		return int(v), v > 0
	case int:
		return v, v > 0
	default:
		return 0, false
	}
}

// Load reads a policy JSON file and validates its references.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read role policy: %w", err)
	}
	return Parse(data)
}

// Parse decodes and validates a policy document.
func Parse(data []byte) (*Policy, error) {
	var policy Policy
	if err := json.Unmarshal(data, &policy); err != nil {
		return nil, fmt.Errorf("decode role policy: %w", err)
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &policy, nil
}

// Validate checks internal references so a broken policy fails at load time
// instead of producing silent mis-selection.
func (p *Policy) Validate() error {
	if strings.TrimSpace(p.Version) == "" {
		return fmt.Errorf("role policy: version is required")
	}
	switch p.Budget.Mode {
	case BudgetModeDisabled, BudgetModeGatewayQuota:
	default:
		return fmt.Errorf("role policy: unsupported budget.mode %q", p.Budget.Mode)
	}
	if p.Limits.ContextReserveTokens <= 0 {
		return fmt.Errorf("role policy: limits.context_reserve_tokens must be positive")
	}
	if p.Limits.CatalogMaxAgeSeconds <= 0 {
		return fmt.Errorf("role policy: limits.catalog_max_age_seconds must be positive")
	}
	for id, conn := range p.Connections {
		if strings.TrimSpace(conn.BaseURL) == "" {
			return fmt.Errorf("role policy: connection %q has no base_url", id)
		}
		if strings.TrimSpace(conn.TokenEnv) == "" {
			return fmt.Errorf("role policy: connection %q has no token_env", id)
		}
	}
	for id, exec := range p.Executors {
		if strings.TrimSpace(exec.ProviderID) == "" {
			return fmt.Errorf("role policy: executor %q has no provider_id", id)
		}
		if _, ok := p.Connections[exec.Connection]; !ok {
			return fmt.Errorf("role policy: executor %q references unknown connection %q", id, exec.Connection)
		}
		if !strings.Contains(exec.ModelOptionTemplate, "{model_id}") {
			return fmt.Errorf("role policy: executor %q model_option_template must contain {model_id}", id)
		}
	}
	for key, model := range p.Models {
		for connID := range model.Bindings {
			if _, ok := p.Connections[connID]; !ok {
				return fmt.Errorf("role policy: model %q binds unknown connection %q", key, connID)
			}
		}
	}
	for roleID, role := range p.Roles {
		if !knownRole(roleID) {
			return fmt.Errorf("role policy: unknown role %q", roleID)
		}
		for _, key := range role.Preferences {
			model, ok := p.Models[key]
			if !ok {
				return fmt.Errorf("role policy: role %q prefers unknown model %q", roleID, key)
			}
			if model.CandidateOnly {
				return fmt.Errorf("role policy: role %q prefers candidate_only model %q", roleID, key)
			}
		}
		for _, execID := range role.Executors {
			if _, ok := p.Executors[execID]; !ok {
				return fmt.Errorf("role policy: role %q references unknown executor %q", roleID, execID)
			}
		}
	}
	return nil
}

func knownRole(role string) bool {
	switch role {
	case RoleChat, RoleWorker, RoleEngineer, RoleArchitect, RoleResearcher, RoleSpecialist:
		return true
	default:
		return false
	}
}

// EnabledRoles returns enabled role IDs in a stable order.
func (p *Policy) EnabledRoles() []string {
	var roles []string
	for _, role := range []string{RoleChat, RoleWorker, RoleEngineer, RoleArchitect, RoleResearcher, RoleSpecialist} {
		if profile, ok := p.Roles[role]; ok && profile.Enabled {
			roles = append(roles, role)
		}
	}
	return roles
}
