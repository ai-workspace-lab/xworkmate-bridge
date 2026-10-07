package acp

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"xworkmate-bridge/internal/rolepolicy"
	"xworkmate-bridge/internal/shared"
)

const (
	rolePolicyPathEnv         = "BRIDGE_ROLE_POLICY_PATH"
	permissionTimeoutEnv      = "BRIDGE_PERMISSION_TIMEOUT_SECONDS"
	roleCatalogFetchTimeout   = 10 * time.Second
	roleCatalogErrorRetry     = 30 * time.Second
	roleTaskEventHistoryLimit = 100

	unavailableRolePolicyUnconfigured = "ROLE_POLICY_UNCONFIGURED"
	unavailableRolePolicyInvalid      = "ROLE_POLICY_INVALID"
	unavailableRoleSelectionRejected  = "ROLE_SELECTION_REJECTED"
)

// roleRouting owns the loaded policy and the live catalog snapshots. The
// bridge keeps them in memory only; the policy file is deployed config.
type roleRouting struct {
	policy  *rolepolicy.Policy
	loadErr error
	client  *http.Client

	mu       sync.Mutex
	catalogs map[string]rolepolicy.CatalogSnapshot
}

// roleTaskState is attached to a session whose turn was routed by role.
type roleTaskState struct {
	decision  rolepolicy.Decision
	cancelRun context.CancelFunc
	events    []map[string]any
}

func loadRoleRouting() *roleRouting {
	routing := &roleRouting{
		client:   shared.NewHTTPClient(roleCatalogFetchTimeout),
		catalogs: make(map[string]rolepolicy.CatalogSnapshot),
	}
	path := strings.TrimSpace(os.Getenv(rolePolicyPathEnv))
	if path == "" {
		return routing
	}
	routing.policy, routing.loadErr = rolepolicy.Load(path)
	if routing.loadErr != nil {
		log.Printf("level=error component=role_routing event=policy_load_failed path=%q error=%q", path, routing.loadErr)
	} else {
		log.Printf("level=info component=role_routing event=policy_loaded path=%q version=%q enabled=%t", path, routing.policy.Version, routing.policy.Enabled)
	}
	return routing
}

func permissionTimeoutFromEnv() time.Duration {
	seconds := shared.IntArg(shared.EnvOrDefault(permissionTimeoutEnv, ""), 0)
	if seconds <= 0 {
		return defaultPermissionDecisionTimeout
	}
	return time.Duration(seconds) * time.Second
}

func roleRoutingRequested(routingParams map[string]any) bool {
	return strings.TrimSpace(shared.StringArg(routingParams, "role", "")) != "" ||
		strings.TrimSpace(shared.StringArg(routingParams, "roleMode", "")) != ""
}

// liveCatalogs returns a snapshot per connection, refreshing entries older
// than half the policy's max age (or failed ones after a short delay).
func (r *roleRouting) liveCatalogs(ctx context.Context, policy *rolepolicy.Policy, now time.Time) map[string]rolepolicy.CatalogSnapshot {
	refreshAfter := time.Duration(policy.Limits.CatalogMaxAgeSeconds) * time.Second / 2
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]rolepolicy.CatalogSnapshot, len(policy.Connections))
	for id, conn := range policy.Connections {
		snapshot, ok := r.catalogs[id]
		stale := !ok || now.Sub(snapshot.FetchedAt) > refreshAfter ||
			(snapshot.Err != "" && now.Sub(snapshot.FetchedAt) > roleCatalogErrorRetry)
		if stale {
			fetchCtx, cancel := context.WithTimeout(ctx, roleCatalogFetchTimeout)
			snapshot = rolepolicy.FetchCatalog(fetchCtx, r.client, id, conn, now)
			cancel()
			if snapshot.Err != "" {
				log.Printf("level=warn component=role_routing event=catalog_fetch_failed connection=%q error=%q", id, snapshot.Err)
			}
			r.catalogs[id] = snapshot
		}
		result[id] = snapshot
	}
	return result
}

func (s *Server) resolveRoleRouting(ctx context.Context, params map[string]any, routingParams map[string]any) RoutingResult {
	unavailable := func(code, message string) RoutingResult {
		return RoutingResult{Status: "unavailable", UnavailableCode: code, UnavailableMsg: message, SkillResolutionSource: "none"}
	}
	if s.roles == nil || (s.roles.policy == nil && s.roles.loadErr == nil) {
		return unavailable(unavailableRolePolicyUnconfigured, rolePolicyPathEnv+" is not set")
	}
	if s.roles.loadErr != nil {
		return unavailable(unavailableRolePolicyInvalid, s.roles.loadErr.Error())
	}
	policy := s.roles.policy
	now := time.Now()
	var catalogs map[string]rolepolicy.CatalogSnapshot
	if policy.Enabled {
		catalogs = s.roles.liveCatalogs(ctx, policy, now)
	}
	decision := rolepolicy.Select(policy, catalogs, rolepolicy.Request{
		Role:               shared.StringArg(routingParams, "role", ""),
		RoleMode:           shared.StringArg(routingParams, "roleMode", ""),
		ManualModel:        shared.StringArg(routingParams, "roleModel", ""),
		Specialty:          shared.StringArg(routingParams, "specialty", ""),
		DataClass:          shared.StringArg(routingParams, "dataClass", ""),
		WorkingDirectory:   strings.TrimSpace(shared.StringArg(params, "workingDirectory", "")),
		PromptBytes:        len(shared.StringArg(params, "taskPrompt", "")),
		AvailableProviders: s.getAvailableProviderIDs(),
		Now:                now,
	})
	if !decision.Selected {
		result := unavailable(unavailableRoleSelectionRejected, decision.Code+": "+decision.Message)
		result.RoleDecision = &decision
		return result
	}
	return RoutingResult{
		TargetID:              "single-agent",
		ProviderID:            decision.ProviderID,
		Model:                 decision.ModelOption,
		Status:                "available",
		SkillResolutionSource: "none",
		RoleDecision:          &decision,
	}
}

// roleRoutingCapabilities tells the app which roles and models it may offer
// for auto/manual selection. It exposes no secrets and no gateway tokens.
func (s *Server) roleRoutingCapabilities() map[string]any {
	if s.roles == nil || (s.roles.policy == nil && s.roles.loadErr == nil) {
		return map[string]any{"configured": false}
	}
	if s.roles.loadErr != nil {
		return map[string]any{"configured": true, "valid": false, "error": s.roles.loadErr.Error()}
	}
	policy := s.roles.policy
	roles := make([]any, 0, len(policy.Roles))
	for _, roleID := range []string{rolepolicy.RoleChat, rolepolicy.RoleWorker, rolepolicy.RoleEngineer, rolepolicy.RoleArchitect, rolepolicy.RoleResearcher, rolepolicy.RoleSpecialist} {
		profile, ok := policy.Roles[roleID]
		if !ok {
			continue
		}
		models := make([]any, 0, len(profile.Preferences))
		for _, key := range profile.Preferences {
			models = append(models, map[string]any{"key": key, "displayName": policy.Models[key].DisplayName})
		}
		roles = append(roles, map[string]any{
			"role":              roleID,
			"enabled":           profile.Enabled,
			"models":            models,
			"executors":         append([]string(nil), profile.Executors...),
			"requiresWorkspace": profile.RequiresWorkspace,
			"requiresSpecialty": profile.RequiresSpecialty,
		})
	}
	return map[string]any{
		"configured":    true,
		"valid":         true,
		"enabled":       policy.Enabled,
		"policyVersion": policy.Version,
		"budgetMode":    policy.Budget.Mode,
		"enabledRoles":  policy.EnabledRoles(),
		"roles":         roles,
		"modes":         []string{rolepolicy.ModeAuto, rolepolicy.ModeManual},
	}
}

func decisionSummary(decision rolepolicy.Decision) map[string]any {
	return map[string]any{
		"selected":        decision.Selected,
		"code":            decision.Code,
		"message":         decision.Message,
		"policyVersion":   decision.PolicyVersion,
		"role":            decision.Role,
		"roleMode":        decision.RoleMode,
		"executor":        decision.Executor,
		"providerId":      decision.ProviderID,
		"connection":      decision.Connection,
		"modelKey":        decision.ModelKey,
		"modelId":         decision.ModelID,
		"modelOption":     decision.ModelOption,
		"tools":           append([]string(nil), decision.Tools...),
		"estimatedTokens": decision.EstimatedTokens,
		"reasons":         append([]string(nil), decision.Reasons...),
		"excluded":        decision.Excluded,
		"enabledRoles":    append([]string(nil), decision.EnabledRoles...),
		"nextSteps":       append([]string(nil), decision.NextSteps...),
	}
}

func roleTaskPhaseTerminal(phase string) bool {
	switch phase {
	case "completed", "failed", "cancelled", "rejected":
		return true
	default:
		return false
	}
}

// emitTaskEvent publishes one unified task event and records it on the
// session so a reconnecting client can replay it through xworkmate.tasks.get.
func (s *Server) emitTaskEvent(notify func(map[string]any), sess *session, turnID string, phase string, detail map[string]any) {
	sess.mu.Lock()
	task := map[string]any{
		"taskId":    turnID,
		"sessionId": sess.sessionID,
		"threadId":  sess.threadID,
		"phase":     phase,
		"at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	if sess.role != nil {
		decision := sess.role.decision
		task["role"] = decision.Role
		task["roleMode"] = decision.RoleMode
		task["executor"] = decision.Executor
		task["providerId"] = decision.ProviderID
		task["modelKey"] = decision.ModelKey
		task["modelId"] = decision.ModelID
		task["policyVersion"] = decision.PolicyVersion
	}
	if len(detail) > 0 {
		task["detail"] = detail
	}
	event := map[string]any{
		"type":      "task",
		"event":     "task." + phase,
		"sessionId": sess.sessionID,
		"threadId":  sess.threadID,
		"pending":   !roleTaskPhaseTerminal(phase),
		"error":     phase == "failed" || phase == "rejected",
		"task":      task,
	}
	if sess.role != nil {
		sess.role.events = append(sess.role.events, cloneMap(event))
		if overflow := len(sess.role.events) - roleTaskEventHistoryLimit; overflow > 0 {
			sess.role.events = sess.role.events[overflow:]
		}
	}
	sess.mu.Unlock()
	s.emitSessionUpdate(notify, turnID, event)
}

// runRoleTask executes a role-routed turn on its single selected executor.
// It never retries, switches model or falls back; the permission relay is
// always on and cancel aborts the live run.
func (o *SessionOrchestrator) runRoleTask(
	ctx context.Context,
	method string,
	params map[string]any,
	res RoutingResult,
	sess *session,
	compat ProviderCompat,
	turnID string,
	notify func(map[string]any),
) (map[string]any, *shared.RPCError) {
	server := o.server
	sessionID := sess.sessionID
	threadID := sess.threadID
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	sess.mu.Lock()
	sess.role = &roleTaskState{decision: *res.RoleDecision, cancelRun: cancelRun}
	sess.mu.Unlock()
	publish := func(phase string, detail map[string]any) {
		server.emitTaskEvent(notify, sess, turnID, phase, detail)
	}
	publish("selected", decisionSummary(*res.RoleDecision))
	runCtx = withPermissionRelay(runCtx, &permissionRelay{
		broker:    server.permissions,
		sessionID: sessionID,
		turnID:    turnID,
		publish:   publish,
	})

	runParams := cloneMap(params)
	runParams["model"] = res.Model
	sink := func(update map[string]any) {
		server.emitSessionUpdate(notify, turnID, update)
	}
	publish("started", nil)
	var (
		result map[string]any
		err    error
	)
	switch method {
	case "session.start":
		result, err = compat.StartSession(runCtx, sessionID, threadID, runParams, sink)
	case "session.message":
		result, err = compat.SendMessage(runCtx, sessionID, threadID, runParams, sink)
	default:
		err = fmt.Errorf("unsupported session method: %s", method)
	}

	if err != nil && runCtx.Err() != nil {
		cancelled := map[string]any{
			"success":       false,
			"status":        string(TaskStateCancelled),
			"code":          "TASK_CANCELLED",
			"message":       "task cancelled before the executor returned",
			"turnId":        turnID,
			"roleSelection": decisionSummary(*res.RoleDecision),
		}
		applyResolvedRouting(cancelled, res)
		sess.mu.Lock()
		sess.task.State = TaskStateCancelled
		sess.task.UpdatedAt = time.Now()
		sess.lastResult = cloneMap(cancelled)
		sess.mu.Unlock()
		publish("cancelled", nil)
		return cancelled, nil
	}
	if err != nil {
		sess.mu.Lock()
		sess.task.State = TaskStateFailed
		sess.task.UpdatedAt = time.Now()
		sess.mu.Unlock()
		publish("failed", map[string]any{"error": err.Error()})
		if continuationErr, ok := asSessionContinuationUnavailableError(err); ok {
			return nil, sessionContinuationUnavailableRPCError(continuationErr)
		}
		return nil, &shared.RPCError{Code: -32002, Message: "EXECUTION_FAILED: " + err.Error()}
	}

	executorStatus := strings.TrimSpace(shared.StringArg(result, "status", ""))
	binding := shared.AsMap(result["modelBinding"])
	normalized := o.normalizeResult(sess, result, res, turnID, params)
	normalized["roleSelection"] = decisionSummary(*res.RoleDecision)
	normalized["resolvedRole"] = res.RoleDecision.Role
	normalized["resolvedModelId"] = res.RoleDecision.ModelID
	normalized["modelBindingVerified"] = parseBool(binding["verified"]) &&
		strings.TrimSpace(shared.StringArg(binding, "bound", "")) == res.Model

	phase := "completed"
	switch {
	case executorStatus == string(TaskStateCancelled):
		phase = "cancelled"
		normalized["status"] = string(TaskStateCancelled)
		normalized["success"] = false
		sess.mu.Lock()
		sess.task.State = TaskStateCancelled
		sess.mu.Unlock()
	case !parseBool(normalized["success"]):
		phase = "failed"
	}
	sess.mu.Lock()
	sess.lastResult = cloneMap(normalized)
	sess.mu.Unlock()
	publish(phase, map[string]any{
		"status":               normalized["status"],
		"code":                 normalized["code"],
		"modelBindingVerified": normalized["modelBindingVerified"],
	})
	return normalized, nil
}

func applyResolvedRouting(result map[string]any, res RoutingResult) {
	result["resolvedExecutionTarget"] = res.TargetID
	result["resolvedProviderId"] = res.ProviderID
	result["resolvedGatewayProviderId"] = res.GatewayProviderID
	result["resolvedModel"] = res.Model
	result["resolvedSkills"] = append([]string(nil), res.Skills...)
	if res.RoleDecision != nil {
		result["resolvedRole"] = res.RoleDecision.Role
		result["resolvedModelId"] = res.RoleDecision.ModelID
	}
}

// roleTaskSnapshot answers xworkmate.tasks.get for role-routed sessions so the
// app can recover status, events, pending approvals and the last result
// after a reconnect.
func (s *Server) roleTaskSnapshot(params map[string]any) (map[string]any, bool) {
	sess := s.findTaskSession(params)
	if sess == nil {
		return nil, false
	}
	sess.mu.Lock()
	if sess.role == nil {
		sess.mu.Unlock()
		return nil, false
	}
	snapshot := cloneMap(sess.lastResult)
	if snapshot == nil {
		snapshot = map[string]any{}
	}
	events := make([]any, 0, len(sess.role.events))
	for _, event := range sess.role.events {
		events = append(events, cloneMap(event))
	}
	snapshot["sessionId"] = sess.sessionID
	snapshot["threadId"] = sess.threadID
	snapshot["turnId"] = sess.task.TurnID
	snapshot["status"] = string(sess.task.State)
	snapshot["roleSelection"] = decisionSummary(sess.role.decision)
	snapshot["resolvedRole"] = sess.role.decision.Role
	snapshot["taskEvents"] = events
	sessionID := sess.sessionID
	sess.mu.Unlock()
	snapshot["pendingPermissions"] = s.permissions.list(sessionID)
	return snapshot, true
}

// cancelRoleRun aborts the live role-routed run of a session, if any, and
// denies its pending permission prompts.
func (s *Server) cancelRoleRun(sessionID string) bool {
	s.mu.RLock()
	sess := s.sessions[sessionID]
	s.mu.RUnlock()
	s.permissions.cancelSession(sessionID)
	if sess == nil {
		return false
	}
	sess.mu.Lock()
	var cancelRun context.CancelFunc
	if sess.role != nil {
		cancelRun = sess.role.cancelRun
	}
	sess.mu.Unlock()
	if cancelRun == nil {
		return false
	}
	cancelRun()
	return true
}
