package rolepolicy

import (
	"path/filepath"
	"strings"
	"time"
)

// Role modes.
const (
	ModeAuto   = "auto"
	ModeManual = "manual"
)

// Rejection codes returned when no dispatch may happen.
const (
	CodePolicyDisabled        = "policy_disabled"
	CodeRoleRequired          = "role_required"
	CodeRoleUnknown           = "role_unknown"
	CodeRoleNotEnabled        = "role_not_enabled"
	CodeSpecialtyRequired     = "specialty_required"
	CodeWorkspaceUnauthorized = "workspace_unauthorized"
	CodeBudgetUnconfigured    = "budget_unconfigured"
	CodeModelUnknown          = "model_unknown"
	CodeNoEligibleCandidate   = "no_eligible_candidate"
)

// Exclusion codes recorded per candidate.
const (
	ExclusionPendingMapping       = "pending_mapping"
	ExclusionExecutorUnavailable  = "executor_unavailable"
	ExclusionExecutorCapability   = "executor_capability_unverified"
	ExclusionCatalogUnavailable   = "catalog_unavailable"
	ExclusionCatalogStale         = "catalog_stale"
	ExclusionNotInLiveCatalog     = "not_in_live_catalog"
	ExclusionCapabilityUnverified = "capability_unverified"
	ExclusionContextUnknown       = "context_unknown"
	ExclusionContextInsufficient  = "context_insufficient"
	ExclusionEgressNotAllowed     = "egress_not_allowed"
)

// CatalogSnapshot is one live /v1/models observation for a connection.
type CatalogSnapshot struct {
	ConnectionID string
	FetchedAt    time.Time
	ModelIDs     []string
	Err          string
}

func (s CatalogSnapshot) contains(id string) bool {
	for _, candidate := range s.ModelIDs {
		if candidate == id {
			return true
		}
	}
	return false
}

// Request is the task contract the selector evaluates.
type Request struct {
	Role     string
	RoleMode string
	// ManualModel is a model key from Policy.Models chosen by the user.
	ManualModel        string
	Specialty          string
	DataClass          string
	WorkingDirectory   string
	PromptBytes        int
	AvailableProviders []string
	Now                time.Time
}

type Exclusion struct {
	Model    string `json:"model"`
	Executor string `json:"executor,omitempty"`
	Code     string `json:"code"`
	Detail   string `json:"detail,omitempty"`
}

// Decision is either a selection (Selected) or a rejection with Code.
type Decision struct {
	Selected        bool        `json:"selected"`
	Code            string      `json:"code,omitempty"`
	Message         string      `json:"message,omitempty"`
	PolicyVersion   string      `json:"policyVersion"`
	Role            string      `json:"role,omitempty"`
	RoleMode        string      `json:"roleMode"`
	Executor        string      `json:"executor,omitempty"`
	ProviderID      string      `json:"providerId,omitempty"`
	Connection      string      `json:"connection,omitempty"`
	ModelKey        string      `json:"modelKey,omitempty"`
	ModelID         string      `json:"modelId,omitempty"`
	ModelOption     string      `json:"modelOption,omitempty"`
	Tools           []string    `json:"tools,omitempty"`
	EstimatedTokens int         `json:"estimatedTokens,omitempty"`
	Reasons         []string    `json:"reasons,omitempty"`
	Excluded        []Exclusion `json:"excluded,omitempty"`
	EnabledRoles    []string    `json:"enabledRoles,omitempty"`
	NextSteps       []string    `json:"nextSteps,omitempty"`
}

// Select evaluates hard gates in order and picks the first passing
// candidate in the user-confirmed preference order. It has no side effects.
func Select(policy *Policy, catalogs map[string]CatalogSnapshot, req Request) Decision {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	mode := strings.TrimSpace(req.RoleMode)
	if mode != ModeManual {
		mode = ModeAuto
	}
	decision := Decision{RoleMode: mode}
	if policy == nil || !policy.Enabled {
		if policy != nil {
			decision.PolicyVersion = policy.Version
		}
		return reject(decision, CodePolicyDisabled, "role policy is not enabled", "enable the reviewed policy before routing by role")
	}
	decision.PolicyVersion = policy.Version
	decision.EnabledRoles = policy.EnabledRoles()

	roleID := strings.ToLower(strings.TrimSpace(req.Role))
	if roleID == "" {
		if mode == ModeManual || len(decision.EnabledRoles) != 1 {
			return reject(decision, CodeRoleRequired, "role must be chosen", "pick one of the enabled roles")
		}
		roleID = decision.EnabledRoles[0]
		decision.Reasons = append(decision.Reasons, "auto: "+roleID+" is the only enabled role")
	}
	decision.Role = roleID
	if !knownRole(roleID) {
		return reject(decision, CodeRoleUnknown, "unknown role "+roleID, "pick one of the enabled roles")
	}
	role, ok := policy.Roles[roleID]
	if !ok || !role.Enabled {
		return reject(decision, CodeRoleNotEnabled, "role "+roleID+" is not enabled in this policy", "pick one of the enabled roles")
	}
	if role.RequiresSpecialty && strings.TrimSpace(req.Specialty) == "" {
		return reject(decision, CodeSpecialtyRequired, "specialist tasks need a specialty", "state the specialty")
	}
	if role.RequiresWorkspace {
		dir := strings.TrimSpace(req.WorkingDirectory)
		if dir == "" || !filepath.IsAbs(dir) {
			return reject(decision, CodeWorkspaceUnauthorized, "role "+roleID+" needs an authorized absolute workspace", "open or authorize a workspace")
		}
	}
	if policy.Budget.Mode != BudgetModeGatewayQuota {
		return reject(decision, CodeBudgetUnconfigured, "paid dispatch is disabled by budget.mode="+policy.Budget.Mode, "configure the connection quota and set budget.mode=gateway_quota")
	}

	candidates := role.Preferences
	if mode == ModeManual {
		key := strings.TrimSpace(req.ManualModel)
		if model, ok := policy.Models[key]; !ok || model.CandidateOnly {
			return reject(decision, CodeModelUnknown, "model "+key+" is not a selectable entry in the policy registry", "choose a registered model or finish its activation steps")
		}
		candidates = []string{key}
	}

	estimate := req.PromptBytes/2 + policy.Limits.ContextReserveTokens
	decision.EstimatedTokens = estimate
	available := make(map[string]bool, len(req.AvailableProviders))
	for _, id := range req.AvailableProviders {
		available[strings.TrimSpace(id)] = true
	}
	maxAge := time.Duration(policy.Limits.CatalogMaxAgeSeconds) * time.Second

	for _, key := range candidates {
		model := policy.Models[key]
		for _, execID := range role.Executors {
			exec := policy.Executors[execID]
			exclusion := evaluate(policy, role, key, model, execID, exec, catalogs, available, req, estimate, maxAge, now)
			if exclusion != nil {
				decision.Excluded = append(decision.Excluded, *exclusion)
				continue
			}
			modelID := *model.Bindings[exec.Connection].GatewayModelID
			decision.Selected = true
			decision.Executor = execID
			decision.ProviderID = exec.ProviderID
			decision.Connection = exec.Connection
			decision.ModelKey = key
			decision.ModelID = modelID
			decision.ModelOption = strings.ReplaceAll(exec.ModelOptionTemplate, "{model_id}", modelID)
			decision.Tools = append([]string(nil), role.Tools...)
			decision.Reasons = append(decision.Reasons,
				mode+": "+key+" is the first candidate in "+roleID+" order that passed every hard gate on "+execID)
			return decision
		}
	}
	return reject(decision, CodeNoEligibleCandidate, "no candidate passed the hard gates",
		"verify missing capabilities", "switch to a connected executor", "choose a model manually", "narrow the task")
}

func evaluate(
	policy *Policy,
	role RoleProfile,
	key string,
	model ModelEntry,
	execID string,
	exec Executor,
	catalogs map[string]CatalogSnapshot,
	available map[string]bool,
	req Request,
	estimate int,
	maxAge time.Duration,
	now time.Time,
) *Exclusion {
	exclude := func(code, detail string) *Exclusion {
		return &Exclusion{Model: key, Executor: execID, Code: code, Detail: detail}
	}
	if !available[exec.ProviderID] {
		return exclude(ExclusionExecutorUnavailable, "provider "+exec.ProviderID+" is not connected")
	}
	for _, capability := range role.RequiredExecutorCapabilities {
		if !exec.Capabilities[capability].Verified(now) {
			return exclude(ExclusionExecutorCapability, capability+" is "+exec.Capabilities[capability].EffectiveState(now))
		}
	}
	binding, ok := model.Bindings[exec.Connection]
	if !ok || binding.GatewayModelID == nil || strings.TrimSpace(*binding.GatewayModelID) == "" {
		return exclude(ExclusionPendingMapping, "no observed gateway model id on "+exec.Connection)
	}
	modelID := *binding.GatewayModelID
	snapshot, ok := catalogs[exec.Connection]
	if !ok || snapshot.Err != "" {
		detail := "no live catalog for " + exec.Connection
		if ok {
			detail = snapshot.Err
		}
		return exclude(ExclusionCatalogUnavailable, detail)
	}
	if now.Sub(snapshot.FetchedAt) > maxAge {
		return exclude(ExclusionCatalogStale, "catalog fetched at "+snapshot.FetchedAt.UTC().Format(time.RFC3339))
	}
	if !snapshot.contains(modelID) {
		return exclude(ExclusionNotInLiveCatalog, modelID+" is not returned by "+exec.Connection+" /v1/models")
	}
	for _, capability := range role.RequiredModelCapabilities {
		if !binding.Capabilities[capability].Verified(now) {
			return exclude(ExclusionCapabilityUnverified, capability+" is "+binding.Capabilities[capability].EffectiveState(now))
		}
	}
	if !binding.ContextWindow.Verified(now) {
		return exclude(ExclusionContextUnknown, "context window is "+binding.ContextWindow.EffectiveState(now))
	}
	window, ok := binding.ContextWindow.intValue()
	if !ok {
		return exclude(ExclusionContextUnknown, "context window value is not a positive number")
	}
	if window < estimate {
		return exclude(ExclusionContextInsufficient, "estimated tokens exceed the verified window")
	}
	if !dataClassAllowed(policy.Connections[exec.Connection], req.DataClass) {
		return exclude(ExclusionEgressNotAllowed, "data class "+dataClassOrDefault(req.DataClass)+" may not be sent to "+exec.Connection)
	}
	return nil
}

func dataClassOrDefault(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unclassified"
	}
	return strings.TrimSpace(value)
}

func dataClassAllowed(conn Connection, dataClass string) bool {
	want := dataClassOrDefault(dataClass)
	for _, allowed := range conn.AllowedDataClasses {
		if allowed == want {
			return true
		}
	}
	return false
}

func reject(decision Decision, code, message string, nextSteps ...string) Decision {
	decision.Selected = false
	decision.Code = code
	decision.Message = message
	decision.NextSteps = append(decision.NextSteps, nextSteps...)
	return decision
}
