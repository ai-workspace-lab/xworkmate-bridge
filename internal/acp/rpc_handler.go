package acp

import (
	"context"
	"fmt"
	"strings"
	"time"
	"xworkmate-bridge/internal/shared"
)

func (s *Server) handleRequest(request shared.RPCRequest, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	method := strings.TrimSpace(request.Method)
	ctx := context.Background()

	switch method {
	case "health":
		return map[string]any{"status": "ok", "version": "0.7.0", "role": "acp-control-plane"}, nil

	case "acp.capabilities":
		return s.catalog.Get(), nil

	case "session.start", "session.message":
		return s.orchestrator.Process(ctx, method, request.Params, notify)

	case "session.cancel":
		sessionID := shared.StringArg(request.Params, "sessionId", "")
		s.cancelSession(ctx, sessionID)
		return map[string]any{"accepted": true}, nil

	case "session.close":
		sessionID := shared.StringArg(request.Params, "sessionId", "")
		closed := s.closeSession(ctx, sessionID)
		return map[string]any{"accepted": true, "closed": closed}, nil

	case "xworkmate.routing.resolve":
		res, err := s.routingEngine.Resolve(ctx, request.Params)
		if err != nil {
			return nil, &shared.RPCError{Code: -32602, Message: err.Error()}
		}
		return map[string]any{
			"resolvedExecutionTarget":   res.TargetID,
			"resolvedProviderId":        res.ProviderID,
			"resolvedGatewayProviderId": res.GatewayProviderID,
			"resolvedModel":             res.Model,
			"resolvedSkills":            res.Skills,
			"status":                    res.Status,
			"unavailable":               res.Status == "unavailable",
			"unavailableCode":           res.UnavailableCode,
			"unavailableMessage":        res.UnavailableMsg,
			"skillResolutionSource":     res.SkillResolutionSource,
			"needsSkillInstall":         res.NeedsSkillInstall,
			"skillInstallRequestId":     res.SkillInstallRequestID,
		}, nil

	case "xworkmate.gateway.connect", "xworkmate.gateway.request", "xworkmate.gateway.disconnect":
		// Gateway 语义由专门的 Gateway 组件通过 Adapter 处理
		return s.handleGatewayMethod(ctx, method, request.Params, notify)

	case "xworkmate.jobs.submit", "xworkmate.jobs.get", "xworkmate.jobs.list", "xworkmate.jobs.stats":
		return s.handleJobMethod(ctx, method, request.Params, notify)

	case "xworkmate.tasks.get":
		return s.handleTaskGet(ctx, request.Params, notify), nil

	case "xworkmate.tasks.cancel":
		return s.handleTaskCancel(ctx, request.Params, notify), nil

	case "xworkmate.tools.invoke":
		return s.invokeOpenClawTool(ctx, request.Params)

	default:
		return nil, &shared.RPCError{
			Code:    -32601,
			Message: fmt.Sprintf("unknown method: %s", method),
		}
	}
}

func (s *Server) handleTaskGet(ctx context.Context, params map[string]any, notify func(map[string]any)) map[string]any {
	sess := s.findTaskSession(params)
	if sess == nil {
		sess = s.reassociateOpenClawTask(params)
	}
	if sess == nil {
		return map[string]any{"status": "not_found"}
	}
	return s.orchestrator.probeOpenClawTask(ctx, sess, notify)
}

func (s *Server) handleTaskCancel(ctx context.Context, params map[string]any, notify func(map[string]any)) map[string]any {
	sess := s.findTaskSession(params)
	if sess == nil {
		sess = s.reassociateOpenClawTask(params)
	}
	if sess == nil {
		return map[string]any{"accepted": false, "status": "not_found"}
	}
	sess.mu.Lock()
	gatewayProvider := sess.task.GatewayProviderID
	runID := sess.task.RunID
	sess.task.State = TaskStateCancelled
	sess.task.UpdatedAt = time.Now()
	sess.task.ProgressStage = "cancelled"
	sess.task.ProgressMessage = "OpenClaw task cancelled"
	sess.task.ProgressTerminal = true
	if sess.openClaw != nil {
		sess.openClaw.ProgressStage = "cancelled"
		sess.openClaw.ProgressMessage = "OpenClaw task cancelled"
		sess.openClaw.ProgressTerminal = true
	}
	snapshot := openClawSessionSnapshotLocked(sess)
	sess.mu.Unlock()
	s.orchestrator.releaseOpenClawAdmission(sess)
	if strings.TrimSpace(gatewayProvider) != "" && strings.TrimSpace(runID) != "" && s.gateway != nil {
		_ = s.gateway.RequestByMode(
			gatewayProvider,
			"agent.cancel",
			map[string]any{"runId": runID},
			5*time.Second,
			notify,
		)
	}
	snapshot["accepted"] = true
	return snapshot
}

func (s *Server) findTaskSession(params map[string]any) *session {
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	threadID := strings.TrimSpace(shared.StringArg(params, "threadId", ""))
	turnID := strings.TrimSpace(shared.StringArg(params, "turnId", ""))
	runID := strings.TrimSpace(shared.StringArg(params, "runId", ""))
	artifactScope := strings.TrimSpace(shared.StringArg(params, "artifactScope", ""))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sessionID != "" && s.sessions[sessionID] != nil {
		return s.sessions[sessionID]
	}
	for _, candidate := range s.sessions {
		if candidate == nil {
			continue
		}
		candidate.mu.Lock()
		matches := (threadID != "" && candidate.threadID == threadID) ||
			(turnID != "" && candidate.task.TurnID == turnID) ||
			(runID != "" && candidate.task.RunID == runID) ||
			(artifactScope != "" && candidate.task.ArtifactScope == artifactScope)
		candidate.mu.Unlock()
		if matches {
			return candidate
		}
	}
	return nil
}

func (s *Server) reassociateOpenClawTask(params map[string]any) *session {
	runID := strings.TrimSpace(shared.StringArg(params, "runId", ""))
	artifactScope := strings.TrimSpace(shared.StringArg(params, "artifactScope", ""))
	if runID == "" || artifactScope == "" {
		return nil
	}
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	threadID := strings.TrimSpace(shared.StringArg(params, "threadId", sessionID))
	if sessionID == "" {
		sessionID = threadID
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(shared.StringArg(params, "sessionKey", ""))
	}
	if sessionID == "" {
		sessionID = "openclaw:" + runID
	}
	if threadID == "" {
		threadID = sessionID
	}
	turnID := strings.TrimSpace(shared.StringArg(params, "turnId", runID))
	sessionKey := strings.TrimSpace(shared.StringArg(params, "sessionKey", threadID))
	gatewayProvider := strings.TrimSpace(shared.StringArg(params, "gatewayProviderId", "openclaw"))
	budget := shared.IntArg(shared.StringArg(params, "runtimeBudgetMinutes", ""), openClawComplexTaskMinutes)
	now := time.Now()
	prepared := &openClawPreparedArtifactScope{
		ArtifactScope:             artifactScope,
		ArtifactDirectory:         strings.TrimSpace(shared.StringArg(params, "artifactDirectory", "")),
		RelativeArtifactDirectory: artifactScope,
		ScopeKind:                 "task",
		RemoteWorkingDirectory:    strings.TrimSpace(shared.StringArg(params, "remoteWorkingDirectory", "")),
		RemoteWorkspaceRefKind:    strings.TrimSpace(shared.StringArg(params, "remoteWorkspaceRefKind", "")),
	}
	contract := openClawArtifactContract{
		TaskLoadClass:              strings.TrimSpace(shared.StringArg(params, "taskLoadClass", "")),
		ExpectedArtifactExtensions: normalizeOpenClawExtensionList(shared.ListArg(params, "expectedArtifactExtensions")),
		RequiredFinalExtensions:    normalizeOpenClawExtensionList(shared.ListArg(params, "requiredArtifactExtensions")),
	}
	sess := s.getOrCreateSession(sessionID, threadID)
	sess.mu.Lock()
	sess.provider = gatewayProvider
	sess.target = "gateway"
	sess.mode = "gateway"
	sess.task = QueuedTask{
		SessionID:            sessionID,
		ThreadID:             threadID,
		TurnID:               turnID,
		RunID:                runID,
		SessionKey:           sessionKey,
		Provider:             gatewayProvider,
		Target:               "gateway",
		GatewayProviderID:    gatewayProvider,
		State:                TaskStateRunning,
		Kind:                 TaskKindGateway,
		TaskLoadClass:        contract.TaskLoadClass,
		ArtifactScope:        artifactScope,
		ArtifactDirectory:    prepared.ArtifactDirectory,
		RuntimeBudgetMinutes: budget,
		StartedAt:            now,
		DeadlineAt:           now.Add(time.Duration(budget) * time.Minute),
		UpdatedAt:            now,
		ProgressStage:        "reassociated",
		ProgressMessage:      "OpenClaw task reassociated from task handle",
	}
	sess.openClaw = &OpenClawTaskRecord{
		SessionID:            sessionID,
		ThreadID:             threadID,
		TurnID:               turnID,
		RunID:                runID,
		SessionKey:           sessionKey,
		GatewayProviderID:    gatewayProvider,
		TaskLoadClass:        contract.TaskLoadClass,
		ArtifactSinceUnixMs:  0,
		RuntimeBudgetMinutes: budget,
		StartedAt:            now,
		DeadlineAt:           now.Add(time.Duration(budget) * time.Minute),
		ProgressStage:        "reassociated",
		ProgressMessage:      "OpenClaw task reassociated from task handle",
		ChatParams:           map[string]any{"sessionKey": sessionKey},
		PreparedArtifact:     prepared,
		ArtifactContract:     contract,
	}
	sess.lastResult = openClawRunningTaskResult(sess.openClaw)
	sess.mu.Unlock()
	return sess
}

func (s *Server) cancelSession(ctx context.Context, sessionID string) {
	s.mu.RLock()
	sess, ok := s.sessions[sessionID]
	s.mu.RUnlock()
	if ok && sess != nil && sess.compat != nil {
		sess.mu.Lock()
		sess.task.State = TaskStateCancelled
		sess.task.UpdatedAt = time.Now()
		sess.mu.Unlock()
		_ = sess.compat.CancelSession(ctx, sessionID)
	}
}

func (s *Server) closeSession(ctx context.Context, sessionID string) bool {
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	if ok && sess != nil && sess.compat != nil {
		sess.mu.Lock()
		sess.task.State = TaskStateCancelled
		sess.task.UpdatedAt = time.Now()
		sess.mu.Unlock()
		_ = sess.compat.CloseSession(ctx, sessionID)
	}
	return ok
}
