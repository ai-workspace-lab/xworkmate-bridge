package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"xworkmate-bridge/internal/desktop"
	"xworkmate-bridge/internal/shared"

	"github.com/pion/webrtc/v4"
)

func (s *Server) handleRequest(request shared.RPCRequest, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	method := strings.TrimSpace(request.Method)
	ctx := context.Background()

	switch method {
	case "health":
		return map[string]any{"status": "ok", "version": "0.7.0", "role": "acp-control-plane"}, nil

	case "system.logs":
		return map[string]any{
			"bridgeStatus":  "ok",
			"gatewayStatus": s.gatewayStatusForSystemLogs(),
			"bridgeLogs":    shared.GlobalLogBuffer.GetLines(),
		}, nil

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

	case "xworkmate.desktop.offer", "xworkmate.desktop.ice", "xworkmate.desktop.close":
		return s.handleDesktopMethod(ctx, method, request.Params, notify)

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
	params = s.taskGetParamsWithSessionScope(params)
	gatewayProvider := strings.TrimSpace(shared.StringArg(params, "gatewayProviderId", ""))
	if gatewayProvider == "" {
		gatewayProvider = strings.TrimSpace(shared.StringArg(params, "resolvedGatewayProviderId", ""))
	}
	if gatewayProvider == "" {
		gatewayProvider = "openclaw"
	}
	if rpcErr := ensureProductionGatewayConnected(s, gatewayProvider, notify); rpcErr != nil {
		return map[string]any{
			"ok":      false,
			"status":  "not_found",
			"code":    "GATEWAY_UNAVAILABLE",
			"message": rpcErr.Message,
		}
	}
	result := s.gateway.RequestByMode(
		gatewayProvider,
		"xworkmate.tasks.get",
		openClawTaskLookupParams(params),
		30*time.Second,
		notify,
	)
	if result.OK {
		payload := shared.AsMap(result.Payload)
		activeOpenClawTask := s.activeOpenClawTaskRecord(params)
		s.mergeOpenClawTaskGetArtifactExport(payload, params, gatewayProvider, notify)
		payload = normalizeOpenClawTaskGetResult(params, payload, gatewayProvider, activeOpenClawTask)
		sessionKey := firstNonEmptyString(payload, "openclawSessionKey", "sessionKey")
		if sessionKey == "" {
			sessionKey = strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", ""))
		}
		runID := firstNonEmptyString(payload, "runId", "taskId")
		if runID == "" {
			runID = strings.TrimSpace(shared.StringArg(params, "runId", ""))
		}
		s.decorateOpenClawArtifactDownloadURLs(payload, sessionKey, runID)
		stripOpenClawArtifactInlineContent(payload)
		return payload
	}
	message := strings.TrimSpace(shared.StringArg(result.Error, "message", "openclaw native task lookup failed"))
	code := strings.TrimSpace(shared.StringArg(result.Error, "code", "TASK_LOOKUP_FAILED"))
	return map[string]any{
		"ok":      false,
		"status":  "not_found",
		"code":    code,
		"message": message,
	}
}

func (s *Server) taskGetParamsWithSessionScope(params map[string]any) map[string]any {
	next := make(map[string]any, len(params)+8)
	for key, value := range params {
		next[key] = value
	}
	sess := s.findTaskSession(params)
	if sess == nil {
		return next
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if strings.TrimSpace(shared.StringArg(next, "runId", "")) == "" {
		next["runId"] = sess.task.RunID
	}
	if strings.TrimSpace(shared.StringArg(next, "taskId", "")) == "" {
		next["taskId"] = sess.task.RunID
	}
	if strings.TrimSpace(shared.StringArg(next, "gatewayProviderId", "")) == "" {
		next["gatewayProviderId"] = sess.task.GatewayProviderID
	}
	if strings.TrimSpace(shared.StringArg(next, "artifactScope", "")) == "" {
		next["artifactScope"] = sess.task.ArtifactScope
	}
	if strings.TrimSpace(shared.StringArg(next, "artifactDirectory", "")) == "" {
		next["artifactDirectory"] = sess.task.ArtifactDirectory
	}
	if _, ok := next["requiresArtifactExport"]; !ok && sess.openClaw != nil && sess.openClaw.RequiresArtifactExport {
		next["requiresArtifactExport"] = true
	}
	if _, ok := next["expectedArtifactDirs"]; !ok && sess.openClaw != nil && len(sess.openClaw.ExpectedArtifactDirs) > 0 {
		next["expectedArtifactDirs"] = append([]string(nil), sess.openClaw.ExpectedArtifactDirs...)
	}
	if _, ok := next["requiredArtifactExtensions"]; !ok && sess.openClaw != nil && len(sess.openClaw.RequiredArtifactExts) > 0 {
		next["requiredArtifactExtensions"] = append([]string(nil), sess.openClaw.RequiredArtifactExts...)
	}
	if strings.TrimSpace(shared.StringArg(next, "openclawSessionKey", "")) == "" {
		next["openclawSessionKey"] = sess.task.SessionKey
	}
	if strings.TrimSpace(shared.StringArg(next, "appThreadKey", "")) == "" {
		next["appThreadKey"] = sess.threadID
	}
	return next
}

func (s *Server) mergeOpenClawTaskGetArtifactExport(payload map[string]any, params map[string]any, gatewayProvider string, notify func(map[string]any)) {
	if len(payload) == 0 || s == nil || s.orchestrator == nil {
		return
	}
	status := strings.ToLower(strings.TrimSpace(shared.StringArg(payload, "status", "")))
	if status == string(TaskStateRunning) || status == string(TaskStateFailed) || status == string(TaskStateCancelled) {
		return
	}
	if !openClawTaskGetRequiresArtifactExport(params, payload) {
		return
	}
	success := true
	if value, ok := payload["success"]; ok {
		success = parseBool(value)
	}
	if !success {
		return
	}
	remoteWorkingDirectory := strings.TrimSpace(shared.StringArg(payload, "remoteWorkingDirectory", ""))
	sessionKey := firstNonEmptyString(payload, "openclawSessionKey", "sessionKey")
	if sessionKey == "" {
		sessionKey = strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", ""))
	}
	runID := firstNonEmptyString(payload, "runId", "taskId")
	if runID == "" {
		runID = strings.TrimSpace(shared.StringArg(params, "runId", ""))
	}
	artifactScope := firstNonEmptyString(payload, "artifactScope")
	if artifactScope == "" {
		artifactScope = strings.TrimSpace(shared.StringArg(params, "artifactScope", ""))
	}
	artifactDirectory := firstNonEmptyString(payload, "artifactDirectory")
	if artifactDirectory == "" {
		artifactDirectory = strings.TrimSpace(shared.StringArg(params, "artifactDirectory", ""))
	}
	if sessionKey == "" || runID == "" || artifactScope == "" || artifactDirectory == "" {
		return
	}
	prepared := &openClawPreparedArtifactScope{
		RemoteWorkingDirectory: remoteWorkingDirectory,
		RemoteWorkspaceRefKind: strings.TrimSpace(shared.StringArg(payload, "remoteWorkspaceRefKind", "")),
		ArtifactScope:          artifactScope,
		ArtifactDirectory:      artifactDirectory,
		ScopeKind:              "task",
	}
	exportParams := map[string]any{
		"openclawSessionKey": sessionKey,
		"runId":              runID,
		"artifactScope":      artifactScope,
		"sinceUnixMs":        0,
		"maxFiles":           64,
		"maxInlineBytes":     0,
		"includeContent":     false,
	}
	if expectedDirs := openClawTaskGetExpectedArtifactDirs(params, payload); len(expectedDirs) > 0 {
		exportParams["expectedArtifactDirs"] = expectedDirs
	}
	if requiredExts := openClawTaskGetRequiredArtifactExtensions(params, payload); len(requiredExts) > 0 {
		exportParams["requiredArtifactExtensions"] = append([]string(nil), requiredExts...)
	}
	exportPayload := s.orchestrator.openClawArtifactExportRequest(gatewayProvider, exportParams, notify)
	if openClawArtifactExportPayloadAuthoritative(exportPayload) {
		replaceOpenClawArtifactPayload(payload, exportPayload)
	} else {
		mergeOpenClawArtifactPayload(payload, exportPayload)
	}
	applyOpenClawPreparedArtifactToResult(payload, prepared)
	s.decorateOpenClawArtifactDownloadURLs(payload, sessionKey, runID)
	stripOpenClawArtifactInlineContent(payload)
}

func (s *Server) activeOpenClawTaskRecord(params map[string]any) *OpenClawTaskRecord {
	sess := s.findTaskSession(params)
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.task.State != TaskStateRunning {
		return nil
	}
	if sess.openClaw == nil {
		return nil
	}
	record := *sess.openClaw
	return &record
}

func normalizeOpenClawTaskGetResult(params map[string]any, payload map[string]any, gatewayProvider string, activeRecord *OpenClawTaskRecord) map[string]any {
	if len(payload) == 0 {
		return payload
	}
	artifactScope := firstNonEmptyString(payload, "artifactScope")
	if artifactScope == "" {
		artifactScope = strings.TrimSpace(shared.StringArg(params, "artifactScope", ""))
	}
	artifactDirectory := firstNonEmptyString(payload, "artifactDirectory")
	if artifactDirectory == "" {
		artifactDirectory = strings.TrimSpace(shared.StringArg(params, "artifactDirectory", ""))
	}
	if artifactScope == "" && artifactDirectory == "" {
		return payload
	}
	remoteWorkingDirectory := strings.TrimSpace(shared.StringArg(payload, "remoteWorkingDirectory", ""))
	artifacts := extractArtifactPayloads(payload, remoteWorkingDirectory)
	requiredExts := openClawTaskGetRequiredArtifactExtensions(params, payload)
	if openClawUnknownArtifactEvidence(payload, artifacts) {
		return adjudicateOpenClawUnknownArtifactEvidence(params, payload, gatewayProvider, activeRecord, artifacts, requiredExts, artifactScope, artifactDirectory)
	}
	applyOpenClawConstraintDeliveryStatus(payload)
	if strings.TrimSpace(shared.StringArg(payload, "status", "")) == "partially_delivered" {
		return payload
	}
	if len(artifacts) > 0 && openClawArtifactsSatisfyRequiredExtensions(artifacts, requiredExts) {
		return payload
	}
	status := strings.ToLower(strings.TrimSpace(shared.StringArg(payload, "status", "")))
	success := true
	if value, ok := payload["success"]; ok {
		success = parseBool(value)
	}
	if !success || status == string(TaskStateRunning) || status == string(TaskStateFailed) || status == string(TaskStateCancelled) {
		return payload
	}
	if !openClawTaskGetRequiresArtifactExport(params, payload) {
		return payload
	}
	runID := firstNonEmptyString(payload, "runId", "taskId")
	if runID == "" {
		runID = strings.TrimSpace(shared.StringArg(params, "runId", ""))
	}
	sessionKey := firstNonEmptyString(payload, "openclawSessionKey", "sessionKey")
	if sessionKey == "" {
		sessionKey = strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", ""))
	}
	payload["success"] = true
	payload["status"] = string(TaskStateRunning)
	payload["event"] = string(TaskStateRunning)
	payload["pending"] = true
	payload["artifactSyncStatus"] = "syncing"
	payload["message"] = "OpenClaw task completed; waiting for artifact export."
	payload["runId"] = runID
	payload["taskId"] = runID
	payload["openclawSessionKey"] = sessionKey
	if strings.TrimSpace(shared.StringArg(payload, "appThreadKey", "")) == "" {
		payload["appThreadKey"] = strings.TrimSpace(shared.StringArg(params, "appThreadKey", ""))
	}
	payload["artifactScope"] = artifactScope
	payload["artifactDirectory"] = artifactDirectory
	if len(requiredExts) > 0 {
		payload["requiredArtifactExtensions"] = append([]string(nil), requiredExts...)
	}
	if strings.TrimSpace(shared.StringArg(payload, "resolvedGatewayProviderId", "")) == "" {
		payload["resolvedGatewayProviderId"] = gatewayProvider
	}
	payload["progress"] = map[string]any{
		"stage":    "syncing-artifacts",
		"message":  "Waiting for OpenClaw artifact export.",
		"terminal": false,
	}
	return payload
}

func openClawUnknownArtifactEvidence(payload map[string]any, artifacts []map[string]any) bool {
	if strings.ToLower(strings.TrimSpace(shared.StringArg(payload, "status", ""))) != "unknown" {
		return false
	}
	evidence := strings.ToLower(strings.TrimSpace(shared.StringArg(payload, "evidence", "")))
	if evidence == "artifacts_present" {
		return true
	}
	return parseBool(payload["artifactsPresent"]) ||
		shared.IntArg(shared.StringArg(payload, "artifactCount", ""), 0) > 0 ||
		len(artifacts) > 0
}

func adjudicateOpenClawUnknownArtifactEvidence(
	params map[string]any,
	payload map[string]any,
	gatewayProvider string,
	activeRecord *OpenClawTaskRecord,
	artifacts []map[string]any,
	requiredExts []string,
	artifactScope string,
	artifactDirectory string,
) map[string]any {
	if openClawTaskRecordStillActive(activeRecord) {
		running := openClawRunningTaskResult(activeRecord)
		running["artifactEvidence"] = "artifacts_present"
		if strings.TrimSpace(shared.StringArg(running, "resolvedGatewayProviderId", "")) == "" {
			running["resolvedGatewayProviderId"] = gatewayProvider
		}
		return running
	}
	runID := firstNonEmptyString(payload, "runId", "taskId")
	if runID == "" {
		runID = strings.TrimSpace(shared.StringArg(params, "runId", ""))
	}
	sessionKey := firstNonEmptyString(payload, "openclawSessionKey", "sessionKey")
	if sessionKey == "" {
		sessionKey = strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", ""))
	}
	if len(requiredExts) > 0 && openClawArtifactsSatisfyRequiredExtensions(artifacts, requiredExts) {
		payload["success"] = true
		payload["successSource"] = "inferred"
		payload["status"] = string(TaskStateCompleted)
		payload["event"] = string(TaskStateCompleted)
		payload["pending"] = false
	} else {
		payload["success"] = false
		payload["status"] = string(TaskStateFailed)
		payload["event"] = string(TaskStateFailed)
		payload["pending"] = false
		payload["code"] = "OPENCLAW_TERMINAL_WITHOUT_EVIDENCE"
		payload["error"] = "OPENCLAW_TERMINAL_WITHOUT_EVIDENCE"
		payload["message"] = "OPENCLAW_TERMINAL_WITHOUT_EVIDENCE"
		if len(requiredExts) > 0 {
			payload["missingRequiredExtensions"] = openClawMissingRequiredExtensions(artifacts, requiredExts)
		}
	}
	payload["runId"] = runID
	payload["taskId"] = runID
	payload["openclawSessionKey"] = sessionKey
	if strings.TrimSpace(shared.StringArg(payload, "appThreadKey", "")) == "" {
		payload["appThreadKey"] = strings.TrimSpace(shared.StringArg(params, "appThreadKey", ""))
	}
	payload["artifactScope"] = artifactScope
	payload["artifactDirectory"] = artifactDirectory
	if len(requiredExts) > 0 {
		payload["requiredArtifactExtensions"] = append([]string(nil), requiredExts...)
	}
	if strings.TrimSpace(shared.StringArg(payload, "resolvedGatewayProviderId", "")) == "" {
		payload["resolvedGatewayProviderId"] = gatewayProvider
	}
	return payload
}

func openClawTaskRecordStillActive(record *OpenClawTaskRecord) bool {
	if record == nil {
		return false
	}
	return record.DeadlineAt.IsZero() || time.Now().Before(record.DeadlineAt)
}

func openClawTaskGetRequiresArtifactExport(params map[string]any, payload map[string]any) bool {
	if parseBool(params["requiresArtifactExport"]) || parseBool(payload["requiresArtifactExport"]) {
		return true
	}
	if parseBool(params["requiresExportBeforeFinalResponse"]) || parseBool(payload["requiresExportBeforeFinalResponse"]) {
		return true
	}
	return len(shared.ListArg(params, "expectedArtifactDirs")) > 0 ||
		len(shared.ListArg(payload, "expectedArtifactDirs")) > 0 ||
		len(shared.ListArg(params, "requiredArtifactExtensions")) > 0 ||
		len(shared.ListArg(payload, "requiredArtifactExtensions")) > 0
}

func openClawTaskGetExpectedArtifactDirs(params map[string]any, payload map[string]any) []any {
	seen := map[string]bool{}
	result := []any{}
	for _, values := range [][]any{
		shared.ListArg(params, "expectedArtifactDirs"),
		shared.ListArg(payload, "expectedArtifactDirs"),
	} {
		for _, value := range values {
			item := strings.TrimSpace(fmt.Sprint(value))
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func openClawArtifactExportPayloadAuthoritative(payload map[string]any) bool {
	if len(payload) == 0 {
		return false
	}
	if strings.TrimSpace(shared.StringArg(payload, "remoteWorkingDirectory", "")) != "" {
		return true
	}
	if strings.TrimSpace(shared.StringArg(payload, "artifactScope", "")) != "" {
		return true
	}
	_, hasArtifacts := payload["artifacts"]
	_, hasFiles := payload["files"]
	_, hasAttachments := payload["attachments"]
	return hasArtifacts || hasFiles || hasAttachments
}

func replaceOpenClawArtifactPayload(result map[string]any, source map[string]any) {
	if result == nil {
		return
	}
	for _, key := range []string{"artifacts", "files", "attachments"} {
		delete(result, key)
	}
	mergeOpenClawArtifactPayload(result, source)
}

func openClawTaskGetRequiredArtifactExtensions(params map[string]any, payload map[string]any) []string {
	return normalizeOpenClawArtifactExtList(openClawTaskGetMergedList(params, payload, "requiredArtifactExtensions"))
}

func openClawTaskGetMergedList(params map[string]any, payload map[string]any, key string) []any {
	seen := map[string]bool{}
	result := []any{}
	for _, values := range [][]any{
		shared.ListArg(params, key),
		shared.ListArg(payload, key),
	} {
		for _, value := range values {
			item := strings.TrimSpace(fmt.Sprint(value))
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func openClawArtifactsSatisfyRequiredExtensions(artifacts []map[string]any, requiredExts []string) bool {
	if len(requiredExts) == 0 {
		return true
	}
	return len(openClawMissingRequiredExtensions(artifacts, requiredExts)) == 0
}

func openClawMissingRequiredExtensions(artifacts []map[string]any, requiredExts []string) []any {
	missing := make([]any, 0)
	for _, ext := range requiredExts {
		normalized := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
		if normalized == "" {
			continue
		}
		found := false
		for _, artifact := range artifacts {
			relativePath := strings.ToLower(strings.TrimSpace(shared.StringArg(artifact, "relativePath", "")))
			if strings.HasSuffix(relativePath, "."+normalized) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, normalized)
		}
	}
	return missing
}

func (s *Server) handleTaskCancel(ctx context.Context, params map[string]any, notify func(map[string]any)) map[string]any {
	sess := s.findTaskSession(params)
	runID := strings.TrimSpace(shared.StringArg(params, "runId", ""))
	gatewayProvider := strings.TrimSpace(shared.StringArg(params, "gatewayProviderId", ""))
	if gatewayProvider == "" {
		gatewayProvider = strings.TrimSpace(shared.StringArg(params, "resolvedGatewayProviderId", ""))
	}
	if sess != nil {
		sess.mu.Lock()
		if runID == "" {
			runID = sess.task.RunID
		}
		if gatewayProvider == "" {
			gatewayProvider = sess.task.GatewayProviderID
		}
		sess.task.State = TaskStateCancelled
		sess.task.UpdatedAt = time.Now()
		sess.task.ProgressStage = "cancelled"
		sess.task.ProgressMessage = "OpenClaw task cancelled"
		sess.task.ProgressTerminal = true
		if sess.openClaw != nil {
			sess.openClaw.ProgressStage = "cancelled"
			sess.openClaw.ProgressMessage = "OpenClaw task cancelled"
		}
		sess.mu.Unlock()
	}
	if gatewayProvider == "" {
		gatewayProvider = "openclaw"
	}
	if strings.TrimSpace(runID) != "" && s.gateway != nil {
		_ = s.gateway.RequestByMode(
			gatewayProvider,
			"agent.cancel",
			map[string]any{"runId": runID},
			5*time.Second,
			notify,
		)
	}
	return map[string]any{"accepted": strings.TrimSpace(runID) != "", "runId": runID}
}

func (s *Server) findTaskSession(params map[string]any) *session {
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	threadID := strings.TrimSpace(shared.StringArg(params, "threadId", ""))
	turnID := strings.TrimSpace(shared.StringArg(params, "turnId", ""))
	runID := strings.TrimSpace(shared.StringArg(params, "runId", ""))
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
			(runID != "" && candidate.task.RunID == runID)
		candidate.mu.Unlock()
		if matches {
			return candidate
		}
	}
	return nil
}

func openClawTaskLookupParams(params map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{
		"appThreadKey",
		"openclawSessionKey",
		"runId",
		"taskId",
		"includeArtifacts",
		"includeContent",
		"expectedArtifactDirs",
		"workspaceDir",
		"artifactScope",
		"artifactDirectory",
		"requiresArtifactExport",
	} {
		if value, ok := params[key]; ok {
			result[key] = value
		}
	}
	if strings.TrimSpace(shared.StringArg(result, "workspaceDir", "")) == "" {
		if workspaceDir := openClawArtifactWorkspaceDir(params); workspaceDir != "" {
			result["workspaceDir"] = workspaceDir
		}
	}
	return result
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
	_, existed := s.sessions[sessionID]
	if existed {
		delete(s.sessions, sessionID)
	}
	s.mu.Unlock()
	return existed
}

func (s *Server) handleDesktopMethod(ctx context.Context, method string, params map[string]any, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	if sessionID == "" {
		sessionID = "default"
	}

	srv := desktop.GetService()

	switch method {
	case "xworkmate.desktop.offer":
		sdpOffer := strings.TrimSpace(shared.StringArg(params, "sdpOffer", ""))
		if sdpOffer == "" {
			return nil, &shared.RPCError{Code: -32602, Message: "sdpOffer is required"}
		}
		// Pion WebRTC strict parser requires SDP strings to end with \r\n
		if !strings.HasSuffix(sdpOffer, "\r\n") {
			sdpOffer += "\r\n"
		}

		display := strings.TrimSpace(shared.StringArg(params, "display", ""))
		width := shared.IntArg(shared.StringArg(params, "width", ""), 1280)
		height := shared.IntArg(shared.StringArg(params, "height", ""), 720)
		fps := shared.IntArg(shared.StringArg(params, "fps", ""), 30)
		bitrate := shared.IntArg(shared.StringArg(params, "bitrate", ""), 2000)
		useGPU := shared.BoolArg(shared.StringArg(params, "useGpu", ""), false)

		var iceServers []string
		if rawIce, ok := params["iceServers"].([]any); ok {
			for _, ice := range rawIce {
				if s, ok := ice.(string); ok {
					iceServers = append(iceServers, s)
				}
			}
		}

		cfg := desktop.PipelineConfig{
			Display:  display,
			Port:     desktop.DefaultRTPPort,
			Width:    width,
			Height:   height,
			FPS:      fps,
			Bitrate:  bitrate,
			UseGPU:   useGPU,
			ToolType: "auto",
		}

		sess, err := srv.StartSession(sessionID, cfg, iceServers)
		if err != nil {
			return nil, &shared.RPCError{Code: -32001, Message: fmt.Sprintf("failed to start desktop session: %v", err)}
		}

		sdpAnswer, err := sess.WebRTC.ProcessOffer(sdpOffer)
		if err != nil {
			srv.StopSession(sessionID)
			return nil, &shared.RPCError{Code: -32002, Message: fmt.Sprintf("failed to process SDP offer: %v", err)}
		}
		if err := srv.StartCapture(sessionID); err != nil {
			srv.StopSession(sessionID)
			return nil, &shared.RPCError{Code: -32004, Message: fmt.Sprintf("failed to start desktop capture: %v", err)}
		}

		return map[string]any{
			"sessionId": sessionID,
			"sdpAnswer": sdpAnswer,
		}, nil

	case "xworkmate.desktop.ice":
		candidateData, ok := params["candidate"].(map[string]any)
		if !ok {
			return nil, &shared.RPCError{Code: -32602, Message: "candidate object is required"}
		}

		var candidate webrtc.ICECandidateInit
		bytes, err := json.Marshal(candidateData)
		if err != nil {
			return nil, &shared.RPCError{Code: -32602, Message: fmt.Sprintf("failed to marshal candidate: %v", err)}
		}
		if err := json.Unmarshal(bytes, &candidate); err != nil {
			return nil, &shared.RPCError{Code: -32602, Message: fmt.Sprintf("failed to unmarshal candidate: %v", err)}
		}

		if err := srv.AddICECandidate(sessionID, candidate); err != nil {
			return nil, &shared.RPCError{Code: -32003, Message: fmt.Sprintf("failed to add ICE candidate: %v", err)}
		}

		return map[string]any{"status": "ok"}, nil

	case "xworkmate.desktop.close":
		srv.StopSession(sessionID)
		return map[string]any{"status": "closed"}, nil

	default:
		return nil, &shared.RPCError{Code: -32601, Message: fmt.Sprintf("unknown desktop method: %s", method)}
	}
}
