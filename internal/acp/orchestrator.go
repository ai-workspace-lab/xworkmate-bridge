package acp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/memory"
	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

type SessionOrchestrator struct {
	server *Server
}

const (
	openClawInlineAttachmentMaxFileBytes  = 10 * 1024 * 1024
	openClawInlineAttachmentMaxTotalBytes = 25 * 1024 * 1024
)

const (
	openClawAgentWaitDefaultTimeout      = 6 * time.Minute
	openClawAgentWaitMaxTimeout          = time.Hour
	openClawAgentWaitHTTPMargin          = time.Minute
	openClawNoDisplayableText            = "OpenClaw completed without displayable output."
	openClawRequiredArtifactMissingText  = "OpenClaw completed without required final artifacts."
	openClawArtifactExportAttemptedField = "_openClawArtifactExportAttempted"
)

func NewSessionOrchestrator(server *Server) *SessionOrchestrator {
	return &SessionOrchestrator{server: server}
}

func (o *SessionOrchestrator) Process(ctx context.Context, method string, params map[string]any, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	res, err := o.server.routingEngine.Resolve(ctx, params)
	if err != nil {
		if err.Error() == "ROUTING_REQUIRED" {
			return nil, &shared.RPCError{Code: -32602, Message: err.Error()}
		}
		return nil, &shared.RPCError{Code: -32602, Message: "ROUTING_FAILED: " + err.Error()}
	}

	if res.Status == "unavailable" {
		return o.formatUnavailable(res), nil
	}

	sessionID := shared.StringArg(params, "sessionId", "")
	threadID := shared.StringArg(params, "threadId", sessionID)
	turnID := fmt.Sprintf("turn-%d", time.Now().UnixNano())

	sess := o.server.getOrCreateSession(sessionID, threadID)
	sess.mu.Lock()
	sess.target = res.TargetID
	sess.provider = res.ProviderID
	sess.mode = res.TargetID
	sess.control.ControlPlaneSessionID = sessionID
	sess.control.ThreadID = threadID
	sess.control.RequestedWorkingDir = strings.TrimSpace(shared.StringArg(params, "workingDirectory", ""))
	sess.control.RemoteWorkingDirHint = strings.TrimSpace(shared.StringArg(params, "remoteWorkingDirectoryHint", ""))
	sess.control.UpdatedAt = time.Now()
	sess.task = QueuedTask{
		SessionID: sessionID,
		ThreadID:  threadID,
		TurnID:    turnID,
		Provider:  res.ProviderID,
		Target:    res.TargetID,
		State:     TaskStateRunning,
		Kind:      taskKindFromParams(params, res),
		UpdatedAt: time.Now(),
	}
	prompt := strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	if prompt != "" {
		sess.history = append(sess.history, "USER: "+prompt)
	}
	sess.mu.Unlock()

	o.server.emitSessionUpdate(notify, turnID, map[string]any{
		"type":    "status",
		"event":   "started",
		"message": "session started",
		"pending": true,
		"error":   false,
	})

	if res.TargetID == "gateway" {
		result, rpcErr := o.runGateway(ctx, method, params, res, turnID, notify)
		if rpcErr != nil {
			failed := o.normalizeGatewayFailureResult(sess, rpcErr, res, turnID)
			if (isOpenClawProvider(res.GatewayProviderID) || isOpenClawProvider(res.ProviderID)) &&
				!openClawGatewayBusyError(rpcErr) {
				o.server.emitSessionUpdate(notify, turnID, openClawGatewayCompletedResultUpdate(sessionID, threadID, turnID, failed))
			}
			return nil, rpcErr
		}
		return o.normalizeResult(sess, result, res, turnID, params), nil
	}

	compat, ok := o.server.providers[res.ProviderID]
	if !ok {
		return nil, &shared.RPCError{Code: -32001, Message: "PROVIDER_NOT_FOUND: " + res.ProviderID}
	}

	sess.mu.Lock()
	sess.compat = compat
	sess.mu.Unlock()

	sink := func(update map[string]any) {
		o.server.emitSessionUpdate(notify, turnID, update)
	}

	var result map[string]any
	switch method {
	case "session.start":
		result, err = compat.StartSession(ctx, sessionID, threadID, params, sink)
	case "session.message":
		result, err = compat.SendMessage(ctx, sessionID, threadID, params, sink)
	default:
		err = fmt.Errorf("unsupported session method: %s", method)
	}
	if err != nil {
		sess.mu.Lock()
		sess.task.State = TaskStateFailed
		sess.task.UpdatedAt = time.Now()
		sess.mu.Unlock()
		if continuationErr, ok := asSessionContinuationUnavailableError(err); ok {
			return nil, sessionContinuationUnavailableRPCError(continuationErr)
		}
		return nil, &shared.RPCError{Code: -32002, Message: "EXECUTION_FAILED: " + err.Error()}
	}

	return o.normalizeResult(sess, result, res, turnID, params), nil
}

func openClawGatewayBusyError(rpcErr *shared.RPCError) bool {
	if rpcErr == nil {
		return false
	}
	data := shared.AsMap(rpcErr.Data)
	return strings.TrimSpace(shared.StringArg(data, "code", "")) == "OPENCLAW_GATEWAY_BUSY"
}

func (o *SessionOrchestrator) normalizeGatewayFailureResult(
	sess *session,
	rpcErr *shared.RPCError,
	routing RoutingResult,
	turnID string,
) map[string]any {
	message := "gateway execution failed"
	code := "GATEWAY_EXECUTION_FAILED"
	if rpcErr != nil {
		if strings.TrimSpace(rpcErr.Message) != "" {
			message = strings.TrimSpace(rpcErr.Message)
		}
		data := shared.AsMap(rpcErr.Data)
		if dataCode := strings.TrimSpace(shared.StringArg(data, "code", "")); dataCode != "" {
			code = dataCode
		}
	}
	result := map[string]any{
		"success":                   false,
		"status":                    "failed",
		"code":                      code,
		"error":                     message,
		"message":                   message,
		"summary":                   message,
		"output":                    message,
		"turnId":                    turnID,
		"mode":                      router.ExecutionTargetGatewayChat,
		"resolvedExecutionTarget":   routing.TargetID,
		"resolvedProviderId":        routing.ProviderID,
		"resolvedGatewayProviderId": routing.GatewayProviderID,
		"resolvedModel":             routing.Model,
		"resolvedSkills":            append([]string(nil), routing.Skills...),
	}
	artifactRecord := buildArtifactRecord(sess, result, message)
	if artifactRecord.RemoteWorkingDirectory != "" {
		result["remoteWorkingDirectory"] = artifactRecord.RemoteWorkingDirectory
	}
	if artifactRecord.RemoteWorkspaceRefKind != "" {
		result["remoteWorkspaceRefKind"] = artifactRecord.RemoteWorkspaceRefKind
	}
	if artifactRecord.ResultSummary != "" && strings.TrimSpace(shared.StringArg(result, "resultSummary", "")) == "" {
		result["resultSummary"] = artifactRecord.ResultSummary
	}

	sess.mu.Lock()
	sess.task.State = TaskStateFailed
	sess.task.UpdatedAt = time.Now()
	sess.lastResult = cloneMap(result)
	sess.artifacts = artifactRecord
	sess.mu.Unlock()

	return result
}

func (o *SessionOrchestrator) runGateway(
	ctx context.Context,
	method string,
	params map[string]any,
	routing RoutingResult,
	turnID string,
	notify func(map[string]any),
) (map[string]any, *shared.RPCError) {
	if o.server.gateway == nil {
		return nil, &shared.RPCError{Code: -32001, Message: "GATEWAY_NOT_INITIALIZED"}
	}

	gatewayProvider := resolvedGatewayProviderID(params, routing)
	if gatewayProvider == "" {
		return nil, &shared.RPCError{Code: -32602, Message: "GATEWAY_PROVIDER_REQUIRED"}
	}
	params = withResolvedGatewayProvider(params, gatewayProvider)
	if isOpenClawMode(gatewayProvider) && isSessionTaskMethod(method) {
		sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
		threadID := strings.TrimSpace(shared.StringArg(params, "threadId", sessionID))
		release, rpcErr := o.server.openClawGate.acquire(
			ctx,
			func(position int, queued int) {
				o.server.emitSessionUpdate(notify, turnID, map[string]any{
					"sessionId": sessionID,
					"threadId":  threadID,
					"type":      "status",
					"event":     "queued",
					"message":   "OpenClaw gateway task queued",
					"pending":   true,
					"error":     false,
					"queue": map[string]any{
						"position": position,
						"queued":   queued,
					},
				})
			},
			func(active int) {
				o.server.emitSessionUpdate(notify, turnID, map[string]any{
					"sessionId": sessionID,
					"threadId":  threadID,
					"type":      "status",
					"event":     "running",
					"message":   "OpenClaw gateway task admitted",
					"pending":   true,
					"error":     false,
					"queue": map[string]any{
						"active": active,
					},
				})
			},
		)
		if rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := ensureProductionGatewayConnected(o.server, gatewayProvider, notify); rpcErr != nil {
			release()
			return nil, rpcErr
		}
		result, rpcErr := o.startOpenClawGatewayTask(ctx, params, routing, gatewayProvider, turnID, release, notify)
		if rpcErr != nil {
			release()
			return nil, rpcErr
		}
		return result, nil
	}
	if rpcErr := ensureProductionGatewayConnected(o.server, gatewayProvider, notify); rpcErr != nil {
		return nil, rpcErr
	}
	result := o.server.gateway.RequestByMode(
		gatewayProvider,
		method,
		params,
		2*time.Minute,
		notify,
	)
	if !result.OK {
		errMessage := strings.TrimSpace(shared.StringArg(result.Error, "message", "gateway execution failed"))
		return nil, &shared.RPCError{Code: -32002, Message: errMessage}
	}
	payload := shared.AsMap(result.Payload)
	if len(payload) == 0 {
		payload = map[string]any{
			"success": true,
			"turnId":  turnID,
			"mode":    router.ExecutionTargetGatewayChat,
		}
	}
	if _, ok := payload["turnId"]; !ok {
		payload["turnId"] = turnID
	}
	if _, ok := payload["mode"]; !ok {
		payload["mode"] = router.ExecutionTargetGatewayChat
	}
	return payload, nil
}

func (o *SessionOrchestrator) startOpenClawGatewayTask(
	_ context.Context,
	params map[string]any,
	routing RoutingResult,
	gatewayProvider string,
	turnID string,
	releaseAdmission func(),
	notify func(map[string]any),
) (map[string]any, *shared.RPCError) {
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	threadID := strings.TrimSpace(shared.StringArg(params, "threadId", sessionID))
	if sessionID == "" {
		sessionID = threadID
	}
	notifyWithCollection := func(message map[string]any) {
		if notify == nil {
			return
		}
		if update := openClawGatewaySessionUpdate(message, sessionID, threadID, turnID); update != nil {
			notify(update)
		}
	}
	sessionKey := o.openClawSessionKey(params, turnID)
	params = withOpenClawWritableWorkspace(params, openClawAppThreadKey(params))
	chatParams, rpcErr := openClawChatSendParamsWithSessionKey(params, turnID, sessionKey)
	if rpcErr != nil {
		return nil, rpcErr
	}
	artifactContract := openClawArtifactContractForParams(params, chatParams)
	preparedArtifact, prepareErr := o.openClawArtifactPrepare(
		gatewayProvider,
		params,
		sessionKey,
		turnID,
		artifactContract,
		notifyWithCollection,
	)
	if prepareErr != nil {
		return nil, prepareErr
	}
	logOpenClawArtifactSync(gatewayProvider, sessionKey, turnID, "prepare", true, false, false)
	applyOpenClawPreparedArtifactToChatParams(chatParams, preparedArtifact, sessionKey, turnID, artifactContract)
	sendStarted := time.Now()
	sendResult := o.openClawGatewayRequestWithRetry(
		gatewayProvider,
		"chat.send",
		chatParams,
		2*time.Minute,
		notifyWithCollection,
	)
	logOpenClawGatewayTiming(
		gatewayProvider,
		"chat.send",
		sessionKey,
		turnID,
		time.Since(sendStarted),
		sendResult.OK,
	)
	if !sendResult.OK {
		return nil, gatewayRPCError(sendResult.Error, "openclaw chat.send failed")
	}
	sendPayload := shared.AsMap(sendResult.Payload)
	if rpcErr := validateOpenClawAcceptedSessionKey(sendPayload, sessionKey); rpcErr != nil {
		return nil, rpcErr
	}
	runID := strings.TrimSpace(shared.StringArg(sendPayload, "runId", turnID))
	if runID != turnID {
		preparedArtifact, prepareErr = o.openClawArtifactPrepare(
			gatewayProvider,
			params,
			sessionKey,
			runID,
			artifactContract,
			notifyWithCollection,
		)
		if prepareErr != nil {
			return nil, prepareErr
		}
		logOpenClawArtifactSync(gatewayProvider, sessionKey, runID, "prepare", true, false, false)
		applyOpenClawPreparedArtifactToChatParams(chatParams, preparedArtifact, sessionKey, runID, artifactContract)
	}
	taskLoadClass, runtimeBudgetMinutes := openClawTaskRuntimePolicy(params, chatParams, artifactContract)
	startedAt := time.Now()
	record := &OpenClawTaskRecord{
		SessionID:            sessionID,
		ThreadID:             threadID,
		TurnID:               turnID,
		RunID:                runID,
		SessionKey:           sessionKey,
		GatewayProviderID:    gatewayProvider,
		TaskLoadClass:        taskLoadClass,
		RuntimeBudgetMinutes: runtimeBudgetMinutes,
		StartedAt:            startedAt,
		DeadlineAt:           startedAt.Add(time.Duration(runtimeBudgetMinutes) * time.Minute),
		ProgressStage:        "running",
		ProgressMessage:      "OpenClaw task accepted",
		PreparedArtifact:     preparedArtifact,
		ResolvedModel:        routing.Model,
		ResolvedSkills:       append([]string(nil), routing.Skills...),
	}
	sess := o.server.getOrCreateSession(sessionID, threadID)
	sess.mu.Lock()
	sess.task.RunID = runID
	sess.task.SessionKey = sessionKey
	sess.task.GatewayProviderID = gatewayProvider
	sess.task.TaskLoadClass = taskLoadClass
	sess.task.ArtifactScope = strings.TrimSpace(preparedArtifact.ArtifactScope)
	sess.task.ArtifactDirectory = strings.TrimSpace(preparedArtifact.ArtifactDirectory)
	sess.task.RuntimeBudgetMinutes = runtimeBudgetMinutes
	sess.task.StartedAt = startedAt
	sess.task.DeadlineAt = record.DeadlineAt
	sess.task.ProgressStage = "running"
	sess.task.ProgressMessage = "OpenClaw task accepted"
	sess.openClaw = record
	running := openClawRunningTaskResult(record)
	sess.lastResult = cloneMap(running)
	sess.mu.Unlock()
	if releaseAdmission != nil {
		releaseAdmission()
	}
	if notify != nil {
		notify(shared.NotificationEnvelope("session.update", map[string]any{
			"sessionId":            sessionID,
			"threadId":             threadID,
			"turnId":               turnID,
			"runId":                runID,
			"type":                 "status",
			"event":                "running",
			"message":              "OpenClaw task accepted",
			"pending":              true,
			"error":                false,
			"status":               string(TaskStateRunning),
			"runtimeBudgetMinutes": runtimeBudgetMinutes,
			"progress":             running["progress"],
		}))
	}
		return running, nil
	}

func openClawGatewayCompletedResultUpdate(sessionID string, threadID string, turnID string, result map[string]any) map[string]any {
	success := true
	if value, ok := result["success"].(bool); ok {
		success = value
	}
	update := map[string]any{
		"sessionId": sessionID,
		"threadId":  threadID,
		"turnId":    turnID,
		"type":      "status",
		"event":     "completed",
		"pending":   false,
		"error":     !success,
		"result":    result,
	}
	if output := firstNonEmptyString(result, "output", "message", "summary", "text"); output != "" {
		update["message"] = output
		update["text"] = output
	}
	for _, key := range []string{
		"success",
		"status",
		"code",
		"artifacts",
		"files",
		"attachments",
		"artifactWarnings",
		"remoteWorkingDirectory",
		"remoteWorkspaceRefKind",
		"resolvedGatewayProviderId",
		"mode",
		"runId",
	} {
		if value, ok := result[key]; ok {
			update[key] = value
		}
	}
	return update
}

func logOpenClawGatewayTiming(
	gatewayProvider string,
	method string,
	sessionKey string,
	runID string,
	duration time.Duration,
	ok bool,
) {
	log.Printf(
		"level=info component=openclaw_gateway event=request_timing provider=%q method=%q sessionId=%q runId=%q durationMs=%d ok=%t",
		gatewayProvider,
		method,
		sessionKey,
		runID,
		duration.Milliseconds(),
		ok,
	)
}

func logOpenClawArtifactSync(
	gatewayProvider string,
	sessionKey string,
	runID string,
	stage string,
	prepared bool,
	exported bool,
	empty bool,
) {
	log.Printf(
		"level=info component=openclaw_gateway event=artifact_sync provider=%q sessionId=%q runId=%q stage=%q prepared=%t exported=%t empty=%t",
		gatewayProvider,
		sessionKey,
		runID,
		stage,
		prepared,
		exported,
		empty,
	)
}

func isSessionTaskMethod(method string) bool {
	switch strings.TrimSpace(method) {
	case "session.start", "session.message":
		return true
	default:
		return false
	}
}

type openClawPreparedArtifactScope struct {
	RemoteWorkingDirectory    string
	RemoteWorkspaceRefKind    string
	ArtifactScope             string
	ArtifactDirectory         string
	RelativeArtifactDirectory string
	ScopeKind                 string
}

func openClawPreparedArtifactScopeFromPayload(payload map[string]any) *openClawPreparedArtifactScope {
	if payload == nil {
		return nil
	}
	prepared := &openClawPreparedArtifactScope{
		RemoteWorkingDirectory:    strings.TrimSpace(shared.StringArg(payload, "remoteWorkingDirectory", "")),
		RemoteWorkspaceRefKind:    strings.TrimSpace(shared.StringArg(payload, "remoteWorkspaceRefKind", "")),
		ArtifactScope:             strings.TrimSpace(shared.StringArg(payload, "artifactScope", "")),
		ArtifactDirectory:         strings.TrimSpace(shared.StringArg(payload, "artifactDirectory", "")),
		RelativeArtifactDirectory: strings.TrimSpace(shared.StringArg(payload, "relativeArtifactDirectory", "")),
		ScopeKind:                 strings.TrimSpace(shared.StringArg(payload, "scopeKind", "")),
	}
	if prepared.ArtifactScope == "" || prepared.ArtifactDirectory == "" {
		return nil
	}
	if prepared.ScopeKind == "" {
		prepared.ScopeKind = "task"
	}
	return prepared
}

func (o *SessionOrchestrator) openClawArtifactPrepare(
	gatewayProvider string,
	params map[string]any,
	sessionKey string,
	runID string,
	artifactContract openClawArtifactContract,
	notify func(map[string]any),
) (*openClawPreparedArtifactScope, *shared.RPCError) {
	sessionKey = strings.TrimSpace(sessionKey)
	runID = strings.TrimSpace(runID)
	if sessionKey == "" || runID == "" {
		return nil, &shared.RPCError{Code: -32602, Message: "openclaw artifact prepare requires openclawSessionKey and runId"}
	}
	prepareParams := openClawSessionPrepareParams(params, sessionKey, runID, artifactContract)
	prepareResult := o.openClawGatewayRequestWithRetry(
		gatewayProvider,
		"xworkmate.session.prepare",
		prepareParams,
		30*time.Second,
		notify,
	)
	if !prepareResult.OK {
		return nil, gatewayRPCError(prepareResult.Error, "openclaw artifact prepare failed")
	}
	prepared := openClawPreparedArtifactScopeFromPayload(shared.AsMap(prepareResult.Payload))
	if prepared == nil {
		return nil, &shared.RPCError{Code: -32002, Message: "openclaw artifact prepare returned no scoped artifact directory"}
	}
	return prepared, nil
}

func openClawSessionPrepareParams(params map[string]any, openClawSessionKey string, runID string, artifactContract openClawArtifactContract) map[string]any {
	appThreadKey := openClawAppThreadKey(params)
	result := map[string]any{
		"schemaVersion":      1,
		"appThreadKey":       appThreadKey,
		"openclawSessionKey": strings.TrimSpace(openClawSessionKey),
		"runId":              strings.TrimSpace(runID),
		"requestId":          strings.TrimSpace(runID),
		"externalTaskId":     strings.TrimSpace(runID),
	}
	if len(artifactContract.ExpectedArtifactDirs) > 0 {
		result["expectedArtifactDirs"] = append([]string(nil), artifactContract.ExpectedArtifactDirs...)
	}
	if sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", "")); sessionID != "" {
		result["sessionId"] = sessionID
	}
	if threadID := strings.TrimSpace(shared.StringArg(params, "threadId", "")); threadID != "" {
		result["threadId"] = threadID
	}
	return result
}

func openClawAppThreadKey(params map[string]any) string {
	if value := strings.TrimSpace(shared.StringArg(params, "appThreadKey", "")); value != "" {
		return value
	}
	metadata := shared.AsMap(params["metadata"])
	for _, key := range []string{"appThreadKey"} {
		if value := strings.TrimSpace(shared.StringArg(metadata, key, "")); value != "" {
			return value
		}
	}
	contract := shared.AsMap(metadata["xworkmateTaskArtifactContract"])
	if value := strings.TrimSpace(shared.StringArg(contract, "appThreadKey", "")); value != "" {
		return value
	}
	for _, key := range []string{"threadId", "sessionId"} {
		if value := strings.TrimSpace(shared.StringArg(params, key, "")); value != "" {
			return value
		}
	}
	return "main"
}

func applyOpenClawPreparedArtifactToResult(result map[string]any, prepared *openClawPreparedArtifactScope) {
	if result == nil || prepared == nil {
		return
	}
	if strings.TrimSpace(shared.StringArg(result, "remoteWorkingDirectory", "")) == "" && prepared.RemoteWorkingDirectory != "" {
		result["remoteWorkingDirectory"] = prepared.RemoteWorkingDirectory
	}
	if strings.TrimSpace(shared.StringArg(result, "remoteWorkspaceRefKind", "")) == "" && prepared.RemoteWorkspaceRefKind != "" {
		result["remoteWorkspaceRefKind"] = prepared.RemoteWorkspaceRefKind
	}
	if strings.TrimSpace(shared.StringArg(result, "artifactScope", "")) == "" {
		result["artifactScope"] = prepared.ArtifactScope
	}
	if strings.TrimSpace(shared.StringArg(result, "artifactDirectory", "")) == "" {
		result["artifactDirectory"] = prepared.ArtifactDirectory
	}
	if strings.TrimSpace(shared.StringArg(result, "relativeArtifactDirectory", "")) == "" && prepared.RelativeArtifactDirectory != "" {
		result["relativeArtifactDirectory"] = prepared.RelativeArtifactDirectory
	}
	if strings.TrimSpace(shared.StringArg(result, "scopeKind", "")) == "" {
		result["scopeKind"] = prepared.ScopeKind
	}
}

func applyOpenClawPreparedArtifactToChatParams(
	chatParams map[string]any,
	prepared *openClawPreparedArtifactScope,
	sessionKey string,
	runID string,
	contract openClawArtifactContract,
) {
	if chatParams == nil || prepared == nil || strings.TrimSpace(prepared.ArtifactDirectory) == "" {
		return
	}
	receipt := openClawArtifactSystemProvenanceReceipt(prepared, sessionKey, runID, contract)
	if receipt == "" {
		return
	}
	existing := strings.TrimSpace(shared.StringArg(chatParams, "systemProvenanceReceipt", ""))
	if existing != "" {
		chatParams["systemProvenanceReceipt"] = existing + "\n\n" + receipt
		return
	}
	chatParams["systemProvenanceReceipt"] = receipt
}

func openClawArtifactSystemProvenanceReceipt(
	prepared *openClawPreparedArtifactScope,
	sessionKey string,
	runID string,
	contract openClawArtifactContract,
) string {
	if prepared == nil {
		return ""
	}
	artifactDirectory := strings.TrimSpace(prepared.ArtifactDirectory)
	artifactScope := strings.TrimSpace(prepared.ArtifactScope)
	if artifactDirectory == "" || artifactScope == "" {
		return ""
	}
	lines := []string{
		"XWorkmate task artifact context:",
		"- Treat artifactDirectory as the working directory for all files generated in this turn.",
		"- Write final artifacts directly under artifactDirectory using relative paths such as assets/images/... or prompts/....",
		"- Do not create a nested task_artifacts/<session> directory inside artifactDirectory.",
		"- Environment contract for shell commands:",
		"  export XWORKMATE_TASK_ARTIFACT_DIR=" + shellSingleQuote(artifactDirectory),
		"  export XWORKMATE_ARTIFACT_DIRECTORY=" + shellSingleQuote(artifactDirectory),
		"  export XWORKMATE_ARTIFACT_SCOPE=" + shellSingleQuote(artifactScope),
		"  export XWORKMATE_SESSION_KEY=" + shellSingleQuote(strings.TrimSpace(sessionKey)),
		"  export XWORKMATE_RUN_ID=" + shellSingleQuote(strings.TrimSpace(runID)),
		"  cd " + shellSingleQuote(artifactDirectory),
		"artifactDirectory: " + artifactDirectory,
		"artifactScope: " + artifactScope,
	}
	if relative := strings.TrimSpace(prepared.RelativeArtifactDirectory); relative != "" {
		lines = append(lines, "relativeArtifactDirectory: "+relative)
	}
	if remote := strings.TrimSpace(prepared.RemoteWorkingDirectory); remote != "" {
		lines = append(lines, "remoteWorkingDirectory: "+remote)
	}
	if contract.ComplexLongChain {
		lines = append(lines,
			"",
			"Complex XWorkmate artifact contract:",
			"- Persist the stage plan, intermediate outputs, generated assets, final manifest, and final deliverables inside artifactDirectory.",
			"- Use clear relative paths that match the task structure instead of global cache or temporary directories.",
			"- Do not report completion until requested final deliverables are present in artifactDirectory.",
		)
	}
	return strings.Join(lines, "\n")
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type openClawArtifactContract struct {
	TaskLoadClass        string
	ComplexLongChain     bool
	ExpectedArtifactDirs []string
	SourceMessage        string
}

func openClawArtifactContractForParams(params map[string]any, chatParams map[string]any) openClawArtifactContract {
	metadata := shared.AsMap(params["metadata"])
	taskLoadClass := strings.TrimSpace(shared.StringArg(metadata, "taskLoadClass", ""))
	message := strings.TrimSpace(shared.StringArg(chatParams, "message", ""))
	if message == "" {
		message = openClawCurrentTurnMessage(params)
	}
	lowerMessage := strings.ToLower(message)
	contract := shared.AsMap(metadata["xworkmateTaskArtifactContract"])
	expectedDirs := normalizeOpenClawDirList(shared.ListArg(contract, "expectedArtifactDirs"))
	complex := taskLoadClass == "complex_long_chain_task" || isOpenClawLongArtifactTask(lowerMessage)
	return openClawArtifactContract{
		TaskLoadClass:        taskLoadClass,
		ComplexLongChain:     complex,
		ExpectedArtifactDirs: expectedDirs,
		SourceMessage:        message,
	}
}

func normalizeOpenClawDirList(values []any) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		dir := strings.TrimSpace(fmt.Sprint(value))
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		result = append(result, dir)
	}
	return result
}

func openClawChatSendParams(
	params map[string]any,
	turnID string,
) (map[string]any, *shared.RPCError) {
	return openClawChatSendParamsWithSessionKey(params, turnID, openClawAgentMainSessionKey(openClawAppThreadKey(params)))
}

func openClawChatSendParamsWithSessionKey(
	params map[string]any,
	turnID string,
	sessionKey string,
) (map[string]any, *shared.RPCError) {
	message := openClawCurrentTurnMessage(params)
	if message == "" {
		return nil, &shared.RPCError{Code: -32602, Message: "OPENCLAW_TASK_PROMPT_REQUIRED"}
	}
	chatParams := map[string]any{
		"sessionKey":     sessionKey,
		"message":        message,
		"idempotencyKey": turnID,
	}
	attachments := openClawNonEmptyPathAttachments(params)
	inlineAttachments, rpcErr := materializeOpenClawInlineAttachments(params, turnID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	attachments = append(attachments, inlineAttachments...)
	if len(attachments) > 0 {
		chatParams["attachments"] = attachments
		chatParams["message"] = shared.AugmentPromptWithAttachments(
			message,
			map[string]any{"attachments": attachments},
		)
	}
	if thinking := strings.TrimSpace(shared.StringArg(params, "thinking", "")); thinking != "" {
		chatParams["thinking"] = thinking
	}
	return chatParams, nil
}

func withOpenClawWritableWorkspace(params map[string]any, appThreadKey string) map[string]any {
	workingDirectory := strings.TrimSpace(shared.StringArg(params, "workingDirectory", ""))
	remoteHint := strings.TrimSpace(shared.StringArg(params, "remoteWorkingDirectoryHint", ""))
	ownerScoped := firstOwnerScopedWorkspace(workingDirectory, remoteHint)
	if ownerScoped == "" {
		return params
	}
	writable := openClawWritableWorkspaceForOwnerPath(ownerScoped, appThreadKey)
	if writable == "" || writable == ownerScoped {
		return params
	}
	next := make(map[string]any, len(params)+1)
	for key, value := range params {
		next[key] = value
	}
	if workingDirectory == ownerScoped {
		next["workingDirectory"] = writable
	}
	if remoteHint == ownerScoped {
		next["remoteWorkingDirectoryHint"] = writable
	}
	for _, key := range []string{"taskPrompt", "prompt", "message"} {
		if value, ok := next[key].(string); ok && strings.Contains(value, ownerScoped) {
			next[key] = strings.ReplaceAll(value, ownerScoped, writable)
		}
	}
	return next
}

func firstOwnerScopedWorkspace(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(filepath.Clean(trimmed), "/owners/") {
			return trimmed
		}
	}
	return ""
}

func openClawWritableWorkspaceForOwnerPath(ownerPath string, sessionKey string) string {
	root := strings.TrimSpace(os.Getenv("OPENCLAW_WRITABLE_WORKSPACE_ROOT"))
	if root == "" {
		root = "/home/ubuntu/.openclaw/workspace/task_artifacts"
	}
	root = strings.TrimRight(filepath.Clean(root), string(os.PathSeparator))
	if root == "" || root == "." || root == string(os.PathSeparator) {
		return ""
	}
	leaf := safeOpenClawAttachmentPathSegment(sessionKey, "task")
	if leaf == "task" {
		leaf = safeOpenClawAttachmentPathSegment(filepath.Base(filepath.Clean(ownerPath)), "task")
	}
	return filepath.Join(root, leaf)
}

func openClawNonEmptyPathAttachments(params map[string]any) []any {
	rawAttachments := shared.ListArg(params, "attachments")
	if len(rawAttachments) == 0 {
		return nil
	}
	attachments := make([]any, 0, len(rawAttachments))
	for _, raw := range rawAttachments {
		attachment := shared.AsMap(raw)
		if len(attachment) == 0 {
			continue
		}
		if strings.TrimSpace(shared.StringArg(attachment, "path", "")) == "" {
			continue
		}
		attachments = append(attachments, map[string]any{
			"name":        strings.TrimSpace(shared.StringArg(attachment, "name", "attachment")),
			"description": strings.TrimSpace(shared.StringArg(attachment, "description", "")),
			"path":        strings.TrimSpace(shared.StringArg(attachment, "path", "")),
		})
	}
	return attachments
}

func materializeOpenClawInlineAttachments(params map[string]any, turnID string) ([]any, *shared.RPCError) {
	rawAttachments := shared.ListArg(params, "inlineAttachments")
	if len(rawAttachments) == 0 {
		return nil, nil
	}
	workingDirectory := strings.TrimSpace(shared.StringArg(params, "workingDirectory", ""))
	if workingDirectory == "" {
		return nil, &shared.RPCError{Code: -32602, Message: "OPENCLAW_ATTACHMENT_WORKING_DIRECTORY_REQUIRED"}
	}
	workingDirectory = openClawAttachmentWorkingDirectory(params, workingDirectory)
	attachmentDirectory := filepath.Join(
		workingDirectory,
		".xworkmate",
		"attachments",
		safeOpenClawAttachmentPathSegment(turnID, "turn"),
	)
	if err := os.MkdirAll(attachmentDirectory, 0o755); err != nil {
		return nil, &shared.RPCError{Code: -32002, Message: "OPENCLAW_ATTACHMENT_DIRECTORY_FAILED: " + err.Error()}
	}
	attachments := make([]any, 0, len(rawAttachments))
	totalBytes := 0
	for index, raw := range rawAttachments {
		attachment := shared.AsMap(raw)
		if len(attachment) == 0 {
			continue
		}
		name := safeOpenClawAttachmentFileName(
			shared.StringArg(attachment, "name", shared.StringArg(attachment, "fileName", "attachment")),
		)
		mimeType := strings.TrimSpace(shared.StringArg(attachment, "mimeType", shared.StringArg(attachment, "description", "")))
		content := strings.TrimSpace(shared.StringArg(attachment, "content", ""))
		if content == "" {
			continue
		}
		bytes, err := decodeOpenClawInlineAttachmentContent(content)
		if err != nil {
			return nil, &shared.RPCError{Code: -32602, Message: "OPENCLAW_ATTACHMENT_INVALID_BASE64: " + name}
		}
		if len(bytes) > openClawInlineAttachmentMaxFileBytes {
			return nil, &shared.RPCError{Code: -32602, Message: "OPENCLAW_ATTACHMENT_FILE_TOO_LARGE: " + name}
		}
		if totalBytes+len(bytes) > openClawInlineAttachmentMaxTotalBytes {
			return nil, &shared.RPCError{Code: -32602, Message: "OPENCLAW_ATTACHMENT_TOTAL_TOO_LARGE"}
		}
		totalBytes += len(bytes)
		path := filepath.Join(attachmentDirectory, fmt.Sprintf("%02d-%s", index+1, name))
		if err := os.WriteFile(path, bytes, 0o600); err != nil {
			return nil, &shared.RPCError{Code: -32002, Message: "OPENCLAW_ATTACHMENT_WRITE_FAILED: " + err.Error()}
		}
		attachments = append(attachments, map[string]any{
			"name":        name,
			"description": mimeType,
			"path":        path,
			"sizeBytes":   len(bytes),
			"source":      "inlineAttachment",
		})
	}
	return attachments, nil
}

func openClawAttachmentWorkingDirectory(params map[string]any, workingDirectory string) string {
	candidate := strings.TrimSpace(workingDirectory)
	remoteHint := strings.TrimSpace(shared.StringArg(params, "remoteWorkingDirectoryHint", ""))
	if remoteHint == "" || remoteHint == candidate {
		return candidate
	}
	if isDesktopLocalWorkspacePath(candidate) {
		return remoteHint
	}
	return candidate
}

func isDesktopLocalWorkspacePath(path string) bool {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	return strings.HasPrefix(cleaned, "/Users/") ||
		strings.HasPrefix(cleaned, "/Volumes/")
}

func decodeOpenClawInlineAttachmentContent(content string) ([]byte, error) {
	normalized := strings.TrimSpace(content)
	if comma := strings.LastIndex(normalized, ","); comma >= 0 {
		normalized = strings.TrimSpace(normalized[comma+1:])
	}
	return base64.StdEncoding.DecodeString(normalized)
}

func safeOpenClawAttachmentPathSegment(value string, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		trimmed = fallback
	}
	var builder strings.Builder
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('-')
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return fallback
	}
	return result
}

func safeOpenClawAttachmentFileName(value string) string {
	name := strings.TrimSpace(filepath.Base(value))
	if name == "." || name == string(os.PathSeparator) || name == "" {
		name = "attachment"
	}
	name = strings.ReplaceAll(name, string(os.PathSeparator), "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	return name
}

func openClawAgentWaitTimeout(params map[string]any, chatParams map[string]any) time.Duration {
	message := strings.TrimSpace(shared.StringArg(chatParams, "message", ""))
	if message == "" {
		message = openClawCurrentTurnMessage(params)
	}
	timeout := openClawAgentWaitDefaultTimeout

	lowerMessage := strings.ToLower(message)
	if strings.TrimSpace(shared.StringArg(shared.AsMap(params["metadata"]), "taskLoadClass", "")) == "complex_long_chain_task" ||
		isOpenClawLongArtifactTask(lowerMessage) {
		return openClawAgentWaitMaxTimeout
	}
	for _, keyword := range []string{
		"video",
		"mp4",
		"hyperframes",
		"remotion",
		"ffmpeg",
		"render",
		"视频",
		"渲染",
		"口播",
		"字幕",
	} {
		if strings.Contains(lowerMessage, keyword) {
			timeout += 8 * time.Minute
			break
		}
	}
	if strings.Contains(lowerMessage, "it-infra-evolution-video") ||
		strings.Contains(lowerMessage, "ai-tech-news-video") ||
		strings.Contains(lowerMessage, "product-intro-video") {
		timeout += 4 * time.Minute
	}
	if len([]rune(message)) > 1200 {
		timeout += 2 * time.Minute
	}
	if attachments := shared.ListArg(params, "attachments"); len(attachments) > 0 {
		timeout += time.Duration(min(len(attachments), 6)) * time.Minute
	}
	if timeout > openClawAgentWaitMaxTimeout {
		return openClawAgentWaitMaxTimeout
	}
	return timeout
}

func isOpenClawLongArtifactTask(message string) bool {
	hasDocumentOutput := openClawMessageContainsAny(message, []string{
		"pdf",
		"ppt",
		"pptx",
		"powerpoint",
		"docx",
		"document",
		"文档",
		"文件",
		"输出",
		"导出",
	})
	hasImageWork := openClawMessageContainsAny(message, []string{
		"gpt images",
		"gpt-images",
		"images2",
		"image",
		"images",
		"illustration",
		"插图",
		"配图",
		"图片",
		"生成图",
	})
	hasMultiStageWork := openClawMessageContainsAny(message, []string{
		"chapter",
		"section",
		"codex",
		"artifact export",
		"章节",
		"每章",
		"拆分",
		"多章节",
		"汇总",
		"排版",
	})
	return hasDocumentOutput && hasImageWork && hasMultiStageWork
}

func openClawMessageContainsAny(message string, keywords []string) bool {
	for _, keyword := range keywords {
		if strings.Contains(message, keyword) {
			return true
		}
	}
	return false
}

func openClawCurrentTurnMessage(params map[string]any) string {
	if params == nil {
		return ""
	}
	for _, key := range []string{"taskPrompt", "prompt", "message"} {
		if text := strings.TrimSpace(strings.Join(openClawTextFragments(params[key]), "\n")); text != "" {
			return text
		}
	}
	if text := strings.TrimSpace(strings.Join(openClawLatestUserMessageText(params["messages"]), "\n")); text != "" {
		return text
	}
	for _, key := range []string{"input", "content"} {
		if text := strings.TrimSpace(strings.Join(openClawTextFragments(params[key]), "\n")); text != "" {
			return text
		}
	}
	return ""
}

func openClawLatestUserMessageText(raw any) []string {
	messages, ok := raw.([]any)
	if !ok || len(messages) == 0 {
		return nil
	}
	var fallback []string
	for index := len(messages) - 1; index >= 0; index-- {
		message := shared.AsMap(messages[index])
		if len(message) == 0 {
			continue
		}
		text := compactOpenClawTexts(openClawMessageText(message))
		if len(text) == 0 {
			continue
		}
		if fallback == nil {
			fallback = text
		}
		role := strings.ToLower(strings.TrimSpace(shared.StringArg(message, "role", "")))
		switch role {
		case "user", "human", "client":
			return text
		}
	}
	return fallback
}

func openClawMessageText(message map[string]any) []string {
	if len(message) == 0 {
		return nil
	}
	texts := make([]string, 0, 4)
	for _, key := range []string{"content", "parts", "text", "message"} {
		texts = append(texts, openClawTextFragments(message[key])...)
	}
	return texts
}

func openClawTextFragments(raw any) []string {
	switch value := raw.(type) {
	case nil:
		return nil
	case string:
		if text := strings.TrimSpace(value); text != "" {
			return []string{text}
		}
	case []any:
		texts := make([]string, 0, len(value))
		for _, item := range value {
			texts = append(texts, openClawTextFragments(item)...)
		}
		return compactOpenClawTexts(texts)
	case map[string]any:
		texts := make([]string, 0, len(value))
		for _, key := range []string{"text", "content", "message", "value"} {
			texts = append(texts, openClawTextFragments(value[key])...)
		}
		return compactOpenClawTexts(texts)
	}
	return nil
}

func compactOpenClawTexts(texts []string) []string {
	if len(texts) == 0 {
		return nil
	}
	result := make([]string, 0, len(texts))
	for _, text := range texts {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func (o *SessionOrchestrator) openClawSessionKey(params map[string]any, turnID string) string {
	if explicit := strings.TrimSpace(shared.StringArg(params, "openclawSessionKey", "")); explicit != "" {
		return explicit
	}
	return openClawAgentMainSessionKey(openClawAppThreadKey(params))
}

func openClawAgentMainSessionKey(appThreadKey string) string {
	appThreadKey = strings.TrimSpace(appThreadKey)
	if appThreadKey == "" {
		appThreadKey = "main"
	}
	return "agent:main:" + appThreadKey
}

func validateOpenClawAcceptedSessionKey(payload map[string]any, expectedSessionKey string) *shared.RPCError {
	actual := strings.TrimSpace(shared.StringArg(payload, "sessionKey", ""))
	expected := strings.TrimSpace(expectedSessionKey)
	if actual == "" || expected == "" || actual == expected {
		return nil
	}
	return &shared.RPCError{
		Code: -32002,
		Message: fmt.Sprintf(
			"OPENCLAW_SESSION_MISMATCH: expected %s but OpenClaw accepted %s",
			expected,
			actual,
		),
		Data: map[string]any{
			"code":                "OPENCLAW_SESSION_MISMATCH",
			"expectedSessionKey":  expected,
			"acceptedSessionKey":  actual,
			"expectedOpenClawKey": expected,
			"actualOpenClawKey":   actual,
		},
	}
}

func (o *SessionOrchestrator) openClawArtifactExport(
	gatewayProvider string,
	chatParams map[string]any,
	artifactContract openClawArtifactContract,
	runID string,
	sinceUnixMs int64,
	preparedArtifact *openClawPreparedArtifactScope,
	notify func(map[string]any),
) map[string]any {
	sessionKey := strings.TrimSpace(shared.StringArg(chatParams, "sessionKey", ""))
	if sessionKey == "" || strings.TrimSpace(runID) == "" {
		return nil
	}
	exportParams := map[string]any{
		"openclawSessionKey": sessionKey,
		"runId":              strings.TrimSpace(runID),
		"sinceUnixMs":        sinceUnixMs,
		"maxFiles":           64,
		"maxInlineBytes":     0,
		"includeContent":     false,
	}
	if preparedArtifact != nil && strings.TrimSpace(preparedArtifact.ArtifactScope) != "" {
		exportParams["artifactScope"] = strings.TrimSpace(preparedArtifact.ArtifactScope)
	}
	if len(artifactContract.ExpectedArtifactDirs) > 0 {
		exportParams["expectedArtifactDirs"] = append([]string(nil), artifactContract.ExpectedArtifactDirs...)
	}
	payload := o.openClawArtifactExportRequest(gatewayProvider, exportParams, notify)
	return payload
}

func (o *SessionOrchestrator) openClawArtifactExportRequest(
	gatewayProvider string,
	exportParams map[string]any,
	notify func(map[string]any),
) map[string]any {
	exportResult := o.openClawGatewayRequestWithRetry(
		gatewayProvider,
		"xworkmate.artifacts.export",
		exportParams,
		30*time.Second,
		notify,
	)
	if exportResult.OK {
		return shared.AsMap(exportResult.Payload)
	}
	message := strings.TrimSpace(shared.StringArg(exportResult.Error, "message", ""))
	if message == "" {
		message = "openclaw artifact export unavailable"
	}
	return map[string]any{
		"artifactWarnings": []any{message},
	}
}

func mergeOpenClawArtifactPayload(result map[string]any, source map[string]any) {
	if result == nil || len(source) == 0 {
		return
	}
	if strings.TrimSpace(shared.StringArg(result, "remoteWorkingDirectory", "")) == "" {
		if remoteWorkingDirectory := strings.TrimSpace(shared.StringArg(source, "remoteWorkingDirectory", "")); remoteWorkingDirectory != "" {
			result["remoteWorkingDirectory"] = remoteWorkingDirectory
		}
	}
	if strings.TrimSpace(shared.StringArg(result, "remoteWorkspaceRefKind", "")) == "" {
		if remoteWorkspaceRefKind := strings.TrimSpace(shared.StringArg(source, "remoteWorkspaceRefKind", "")); remoteWorkspaceRefKind != "" {
			result["remoteWorkspaceRefKind"] = remoteWorkspaceRefKind
		}
	}
	if strings.TrimSpace(shared.StringArg(result, "artifactScope", "")) == "" {
		if artifactScope := strings.TrimSpace(shared.StringArg(source, "artifactScope", "")); artifactScope != "" {
			result["artifactScope"] = artifactScope
		}
	}
	if strings.TrimSpace(shared.StringArg(result, "scopeKind", "")) == "" {
		if scopeKind := strings.TrimSpace(shared.StringArg(source, "scopeKind", "")); scopeKind != "" {
			result["scopeKind"] = scopeKind
		}
	}
	for _, key := range []string{"artifacts", "files", "attachments", "artifactWarnings", "warnings"} {
		merged := appendArtifactList(result[key], source[key])
		if len(merged) > 0 {
			if key == "warnings" {
				result["artifactWarnings"] = appendArtifactList(result["artifactWarnings"], source[key])
				continue
			}
			result[key] = merged
		}
	}
}

func appendArtifactList(existing any, incoming any) []any {
	merged := make([]any, 0)
	switch typed := existing.(type) {
	case []any:
		merged = append(merged, typed...)
	case []map[string]any:
		for _, item := range typed {
			merged = append(merged, item)
		}
	}
	switch typed := incoming.(type) {
	case []any:
		merged = append(merged, typed...)
	case []map[string]any:
		for _, item := range typed {
			merged = append(merged, item)
		}
	}
	return merged
}

func gatewayRPCError(errorPayload map[string]any, fallback string) *shared.RPCError {
	if isOpenClawRetryableGatewayError(errorPayload) {
		return &shared.RPCError{
			Code:    -32002,
			Message: "OPENCLAW_GATEWAY_SOCKET_CLOSED: OpenClaw gateway connection closed during task execution",
			Data: map[string]any{
				"code":          "OPENCLAW_GATEWAY_SOCKET_CLOSED",
				"originalCode":  strings.TrimSpace(shared.StringArg(errorPayload, "code", "")),
				"originalError": strings.TrimSpace(shared.StringArg(errorPayload, "message", "")),
			},
		}
	}
	message := strings.TrimSpace(shared.StringArg(errorPayload, "message", fallback))
	if message == "" {
		message = fallback
	}
	data := map[string]any{}
	if code := strings.TrimSpace(shared.StringArg(errorPayload, "code", "")); code != "" {
		data["code"] = code
	}
	if len(data) == 0 {
		return &shared.RPCError{Code: -32002, Message: message}
	}
	return &shared.RPCError{Code: -32002, Message: message, Data: data}
}

func sessionContinuationUnavailableRPCError(err sessionContinuationUnavailableError) *shared.RPCError {
	return &shared.RPCError{
		Code:    -32002,
		Message: "SESSION_CONTINUATION_UNAVAILABLE: provider session state is unavailable",
		Data: map[string]any{
			"code":       "SESSION_CONTINUATION_UNAVAILABLE",
			"sessionId":  err.sessionID,
			"threadId":   err.threadID,
			"providerId": err.providerID,
			"reason":     err.reason,
		},
	}
}

func (o *SessionOrchestrator) openClawGatewayRequestWithRetry(
	gatewayProvider string,
	method string,
	params map[string]any,
	timeout time.Duration,
	notify func(map[string]any),
) gatewayruntime.RequestResult {
	result := o.server.gateway.RequestByMode(
		gatewayProvider,
		method,
		params,
		timeout,
		notify,
	)
	if result.OK || !isOpenClawRetryableGatewayError(result.Error) {
		return result
	}
	if rpcErr := ensureProductionGatewayConnected(o.server, gatewayProvider, notify); rpcErr != nil {
		return result
	}
	return o.server.gateway.RequestByMode(
		gatewayProvider,
		method,
		params,
		timeout,
		notify,
	)
}

func isOpenClawRetryableGatewayError(errorPayload map[string]any) bool {
	code := strings.TrimSpace(strings.ToUpper(shared.StringArg(errorPayload, "code", "")))
	if code == "SOCKET_CLOSED" || code == "SOCKET_FAILURE" || code == "OFFLINE" || code == "INVALID_HANDSHAKE" {
		return true
	}
	message := strings.TrimSpace(strings.ToLower(shared.StringArg(errorPayload, "message", "")))
	return strings.Contains(message, "socket closed") ||
		strings.Contains(message, "invalid handshake") ||
		strings.Contains(message, "first request must be connect")
}

func firstNonEmptyString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(shared.StringArg(values, key, "")); value != "" {
			return value
		}
	}
	return ""
}

func openClawGatewaySessionUpdate(notification map[string]any, sessionID string, threadID string, turnID string) map[string]any {
	params := shared.AsMap(notification["params"])
	event := shared.AsMap(params["event"])
	if strings.TrimSpace(shared.StringArg(event, "event", "")) != "chat.run" {
		return nil
	}
	payload := shared.AsMap(event["payload"])
	text := firstNonEmptyString(payload, "assistantText", "text", "message", "output", "summary")
	if text == "" {
		return nil
	}
	update := map[string]any{
		"sessionId": sessionID,
		"threadId":  threadID,
		"turnId":    turnID,
		"type":      "delta",
		"event":     "delta",
		"delta":     text,
		"text":      text,
		"pending":   true,
		"error":     false,
	}
	if isTerminalGatewayPayload(payload) {
		update["type"] = "status"
		update["event"] = "completed"
		update["message"] = text
		update["pending"] = false
		if strings.EqualFold(strings.TrimSpace(shared.StringArg(payload, "state", "")), "error") {
			update["error"] = true
		}
	}
	return shared.NotificationEnvelope("session.update", update)
}

func isTerminalGatewayPayload(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if value, ok := payload["terminal"].(bool); ok && value {
		return true
	}
	switch strings.TrimSpace(strings.ToLower(shared.StringArg(payload, "state", ""))) {
	case "complete", "completed", "done", "final", "ok", "success", "failed", "error", "timeout", "timed_out", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func resolvedGatewayProviderID(params map[string]any, routing RoutingResult) string {
	for _, value := range []string{
		routing.GatewayProviderID,
		shared.StringArg(params, "gatewayProvider", ""),
		shared.StringArg(params, "gatewayProviderId", ""),
	} {
		if provider := strings.TrimSpace(value); provider != "" {
			return provider
		}
	}
	routingParams := shared.AsMap(params["routing"])
	for _, key := range []string{
		"gatewayProvider",
		"gatewayProviderId",
		"preferredGatewayProviderId",
	} {
		if provider := strings.TrimSpace(shared.StringArg(routingParams, key, "")); provider != "" {
			return provider
		}
	}
	return ""
}

func withResolvedGatewayProvider(params map[string]any, gatewayProvider string) map[string]any {
	next := make(map[string]any, len(params)+2)
	for key, value := range params {
		next[key] = value
	}
	next["gatewayProvider"] = gatewayProvider
	next["gatewayProviderId"] = gatewayProvider
	return next
}

func (o *SessionOrchestrator) formatUnavailable(res RoutingResult) map[string]any {
	return map[string]any{
		"success":                   false,
		"status":                    "unavailable",
		"unavailable":               true,
		"unavailableCode":           res.UnavailableCode,
		"unavailableMessage":        res.UnavailableMsg,
		"resolvedExecutionTarget":   res.TargetID,
		"resolvedProviderId":        res.ProviderID,
		"resolvedGatewayProviderId": res.GatewayProviderID,
		"resolvedModel":             res.Model,
		"resolvedSkills":            append([]string(nil), res.Skills...),
	}
}

func (o *SessionOrchestrator) normalizeResult(sess *session, result map[string]any, routing RoutingResult, turnID string, params map[string]any) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	if routing.TargetID == "gateway" && strings.TrimSpace(shared.StringArg(result, "status", "")) == string(TaskStateRunning) {
		result["turnId"] = turnID
		result["success"] = true
		result["resolvedExecutionTarget"] = routing.TargetID
		result["resolvedProviderId"] = routing.ProviderID
		result["resolvedGatewayProviderId"] = routing.GatewayProviderID
		result["resolvedModel"] = routing.Model
		result["resolvedSkills"] = append([]string(nil), routing.Skills...)
		sess.mu.Lock()
		sess.task.State = TaskStateRunning
		sess.task.UpdatedAt = time.Now()
		sess.lastResult = cloneMap(result)
		sess.mu.Unlock()
		return result
	}
	if openClawArtifactResponse(result, routing, params) {
		o.completeOpenClawScopedArtifactExport(result, params, openClawGatewayProviderForArtifacts(result, routing, params), turnID)
	}
	delete(result, openClawArtifactExportAttemptedField)

	successValue, hasSuccess := result["success"]
	success := !hasSuccess || parseBool(successValue)

	output := strings.TrimSpace(shared.StringArg(result, "output", ""))
	if output == "" {
		output = strings.TrimSpace(shared.StringArg(result, "summary", ""))
	}
	if output == "" && success {
		output = strings.TrimSpace(shared.StringArg(result, "message", ""))
	}

	sess.mu.Lock()
	if output != "" {
		sess.history = append(sess.history, "ASSISTANT: "+output)
	}
	sess.task.State = TaskStateCompleted
	sess.task.UpdatedAt = time.Now()
	sess.mu.Unlock()

	result["turnId"] = turnID
	if strings.TrimSpace(shared.StringArg(result, "status", "")) == "" {
		result["status"] = "completed"
	}
	if !hasSuccess {
		result["success"] = true
	}
	result["resolvedExecutionTarget"] = routing.TargetID
	result["resolvedProviderId"] = routing.ProviderID
	result["resolvedGatewayProviderId"] = routing.GatewayProviderID
	result["resolvedModel"] = routing.Model
	result["resolvedSkills"] = append([]string(nil), routing.Skills...)
	if output != "" {
		result["output"] = output
		if _, ok := result["summary"]; !ok {
			result["summary"] = output
		}
	}
	if output == "" && routing.TargetID != "gateway" && !parseBool(result["success"]) {
		result["status"] = "failed"
	} else if output == "" && routing.TargetID != "gateway" {
		result["success"] = false
		result["status"] = "failed"
		result["error"] = "provider returned no displayable output"
		result["message"] = "provider returned no displayable output"
	}
	if !parseBool(result["success"]) {
		sess.mu.Lock()
		sess.task.State = TaskStateFailed
		sess.task.UpdatedAt = time.Now()
		sess.mu.Unlock()
	}

	artifactRecord := buildArtifactRecord(sess, result, output)
	if artifactRecord.RemoteWorkingDirectory != "" {
		result["remoteWorkingDirectory"] = artifactRecord.RemoteWorkingDirectory
	}
	if artifactRecord.RemoteWorkspaceRefKind != "" {
		result["remoteWorkspaceRefKind"] = artifactRecord.RemoteWorkspaceRefKind
	}
	if artifactRecord.ResultSummary != "" && strings.TrimSpace(shared.StringArg(result, "resultSummary", "")) == "" {
		result["resultSummary"] = artifactRecord.ResultSummary
	}
	if len(artifactRecord.Artifacts) > 0 {
		result["artifacts"] = artifactRecord.Artifacts
	}
	if openClawArtifactResponse(result, routing, params) {
		stripOpenClawArtifactInlineContent(result)
	}

	workingDirectory := shared.StringArg(params, "workingDirectory", "")
	routingParams := shared.AsMap(params["routing"])
	routingMode := strings.TrimSpace(shared.StringArg(routingParams, "routingMode", ""))
	if workingDirectory != "" && routingMode == "auto" {
		_ = o.server.memoryService.RecordSuccess(workingDirectory, memory.SuccessEntry{
			ResolvedExecutionTarget: routing.TargetID,
			ResolvedProviderID:      routing.ProviderID,
			ResolvedModel:           routing.Model,
			ResolvedSkills:          routing.Skills,
			Summary:                 output,
		})
	}

	sess.mu.Lock()
	sess.lastResult = cloneMap(result)
	sess.mu.Unlock()

	return result
}

func (o *SessionOrchestrator) completeOpenClawScopedArtifactExport(
	result map[string]any,
	params map[string]any,
	gatewayProvider string,
	turnID string,
) {
	if result == nil || o.server == nil || o.server.gateway == nil {
		return
	}
	if !parseBool(result["success"]) {
		return
	}
	remoteWorkingDirectory := strings.TrimSpace(shared.StringArg(result, "remoteWorkingDirectory", ""))
	if len(extractArtifactPayloads(result, remoteWorkingDirectory)) > 0 {
		return
	}
	if parseBool(result[openClawArtifactExportAttemptedField]) {
		return
	}
	preparedArtifact := openClawPreparedArtifactScopeFromPayload(result)
	if preparedArtifact == nil {
		return
	}
	sessionKey := o.openClawSessionKey(params, turnID)
	runID := strings.TrimSpace(shared.StringArg(result, "runId", turnID))
	chatParams := map[string]any{"sessionKey": sessionKey}
	artifactContract := openClawArtifactContractForParams(params, chatParams)
	mergeOpenClawArtifactPayload(result, o.openClawArtifactExport(
		gatewayProvider,
		chatParams,
		artifactContract,
		runID,
		0,
		preparedArtifact,
		nil,
	))
	o.server.decorateOpenClawArtifactDownloadURLs(result, sessionKey, runID)
	stripOpenClawArtifactInlineContent(result)
}

func openClawGatewayProviderForArtifacts(result map[string]any, routing RoutingResult, params map[string]any) string {
	for _, provider := range []string{
		routing.GatewayProviderID,
		shared.StringArg(result, "resolvedGatewayProviderId", ""),
		shared.StringArg(result, "gatewayProviderId", ""),
		shared.StringArg(result, "gatewayProvider", ""),
		shared.StringArg(params, "gatewayProviderId", ""),
		shared.StringArg(params, "gatewayProvider", ""),
	} {
		if isOpenClawMode(provider) {
			return strings.TrimSpace(provider)
		}
	}
	routingParams := shared.AsMap(params["routing"])
	for _, key := range []string{"preferredGatewayProviderId", "gatewayProviderId", "gatewayProvider"} {
		if provider := strings.TrimSpace(shared.StringArg(routingParams, key, "")); isOpenClawMode(provider) {
			return provider
		}
	}
	return "openclaw"
}

func openClawArtifactResponse(result map[string]any, routing RoutingResult, params map[string]any) bool {
	for _, provider := range []string{
		routing.GatewayProviderID,
		shared.StringArg(result, "resolvedGatewayProviderId", ""),
		shared.StringArg(result, "gatewayProviderId", ""),
		shared.StringArg(result, "gatewayProvider", ""),
		shared.StringArg(params, "gatewayProviderId", ""),
		shared.StringArg(params, "gatewayProvider", ""),
	} {
		if isOpenClawMode(provider) {
			return true
		}
	}
	routingParams := shared.AsMap(params["routing"])
	for _, key := range []string{"preferredGatewayProviderId", "gatewayProviderId", "gatewayProvider"} {
		if isOpenClawMode(shared.StringArg(routingParams, key, "")) {
			return true
		}
	}
	if strings.HasPrefix(strings.TrimSpace(shared.StringArg(result, "artifactScope", "")), "tasks/") {
		return true
	}
	for _, key := range []string{"artifacts", "files", "attachments"} {
		if openClawArtifactListHasScopedArtifact(result[key]) {
			return true
		}
	}
	return false
}

func openClawArtifactListHasScopedArtifact(raw any) bool {
	switch values := raw.(type) {
	case []map[string]any:
		for _, artifact := range values {
			if openClawArtifactHasBridgeDownloadRef(artifact) {
				return true
			}
		}
	case []any:
		for _, item := range values {
			artifact := shared.AsMap(item)
			if openClawArtifactHasBridgeDownloadRef(artifact) {
				return true
			}
		}
	}
	return false
}

func openClawArtifactHasBridgeDownloadRef(artifact map[string]any) bool {
	if strings.HasPrefix(strings.TrimSpace(shared.StringArg(artifact, "artifactScope", "")), "tasks/") {
		return true
	}
	downloadURL := strings.TrimSpace(shared.StringArg(artifact, "downloadUrl", ""))
	if downloadURL == "" {
		downloadURL = strings.TrimSpace(shared.StringArg(artifact, "downloadURL", ""))
	}
	return strings.Contains(downloadURL, "/artifacts/openclaw/download")
}

func taskKindFromParams(params map[string]any, routing RoutingResult) TaskKind {
	if routing.TargetID == "gateway" {
		return TaskKindGateway
	}
	return TaskKindSingleAgent
}

func buildArtifactRecord(sess *session, result map[string]any, output string) ArtifactRecord {
	record := ArtifactRecord{
		SessionID:     sess.sessionID,
		ThreadID:      sess.threadID,
		ResultSummary: strings.TrimSpace(output),
		UpdatedAt:     time.Now(),
	}
	if record.ResultSummary == "" {
		record.ResultSummary = strings.TrimSpace(shared.StringArg(result, "resultSummary", ""))
	}
	if record.ResultSummary == "" {
		record.ResultSummary = strings.TrimSpace(shared.StringArg(result, "summary", ""))
	}
	record.RemoteWorkingDirectory = strings.TrimSpace(shared.StringArg(result, "remoteWorkingDirectory", ""))
	if record.RemoteWorkingDirectory == "" {
		record.RemoteWorkingDirectory = strings.TrimSpace(sess.control.RemoteWorkingDirHint)
	}
	record.RemoteWorkspaceRefKind = strings.TrimSpace(shared.StringArg(result, "remoteWorkspaceRefKind", ""))
	if record.RemoteWorkspaceRefKind == "" && record.RemoteWorkingDirectory != "" {
		record.RemoteWorkspaceRefKind = "remotePath"
	}
	record.Artifacts = extractArtifactPayloads(result, record.RemoteWorkingDirectory)
	sess.mu.Lock()
	sess.artifacts = record
	sess.control.UpdatedAt = record.UpdatedAt
	sess.mu.Unlock()
	return record
}

func extractArtifactPayloads(result map[string]any, remoteWorkingDirectory string) []map[string]any {
	artifacts := make([]map[string]any, 0)
	for _, key := range []string{"artifacts", "files", "attachments"} {
		rawArtifacts := result[key]
		items, ok := rawArtifacts.([]any)
		if !ok {
			if typed, ok := rawArtifacts.([]map[string]any); ok {
				for _, item := range typed {
					if artifact := normalizeArtifactPayload(item, remoteWorkingDirectory); len(artifact) > 0 {
						artifacts = append(artifacts, artifact)
					}
				}
			}
			continue
		}
		for _, item := range items {
			if mapped := shared.AsMap(item); len(mapped) > 0 {
				if artifact := normalizeArtifactPayload(mapped, remoteWorkingDirectory); len(artifact) > 0 {
					artifacts = append(artifacts, artifact)
				}
			}
		}
	}
	return artifacts
}

func normalizeArtifactPayload(item map[string]any, remoteWorkingDirectory string) map[string]any {
	artifact := make(map[string]any, len(item)+4)
	for key, value := range item {
		artifact[key] = value
	}
	relativePath := strings.TrimSpace(shared.StringArg(artifact, "relativePath", ""))
	if relativePath == "" {
		relativePath = strings.TrimSpace(shared.StringArg(artifact, "path", ""))
	}
	if relativePath == "" {
		relativePath = strings.TrimSpace(shared.StringArg(artifact, "name", ""))
	}
	downloadURL := artifactDownloadURL(artifact)
	if relativePath == "" && downloadURL != "" {
		relativePath = artifactRelativePathFromDownloadURL(downloadURL)
	}
	relativePath = safeArtifactRelativePath(remoteWorkingDirectory, relativePath)
	if relativePath == "" {
		return nil
	}
	artifact["relativePath"] = relativePath
	if downloadURL != "" {
		artifact["downloadUrl"] = downloadURL
		delete(artifact, "downloadURL")
		delete(artifact, "download_url")
	}
	if strings.TrimSpace(shared.StringArg(artifact, "label", "")) == "" {
		artifact["label"] = filepath.Base(relativePath)
	}
	if strings.TrimSpace(shared.StringArg(artifact, "contentType", "")) == "" {
		artifact["contentType"] = artifactContentType(relativePath)
	}
	return artifact
}

func artifactDownloadURL(artifact map[string]any) string {
	for _, key := range []string{"downloadUrl", "downloadURL", "download_url"} {
		if value := strings.TrimSpace(shared.StringArg(artifact, key, "")); value != "" {
			return value
		}
	}
	return ""
}

func artifactRelativePathFromDownloadURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	base := strings.TrimSpace(filepath.Base(parsed.Path))
	if base == "" || base == "." || base == "/" {
		sum := sha256.Sum256([]byte(raw))
		base = fmt.Sprintf("artifact-%x.bin", sum[:6])
	}
	return base
}

func safeArtifactRelativePath(root string, rawPath string) string {
	path := strings.TrimSpace(rawPath)
	if path == "" || strings.Contains(path, "\x00") {
		return ""
	}
	path = filepath.ToSlash(path)
	if strings.TrimSpace(root) != "" && filepath.IsAbs(path) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return ""
		}
		path = filepath.ToSlash(rel)
	}
	path = filepath.Clean(filepath.FromSlash(path))
	if path == "." || filepath.IsAbs(path) || strings.HasPrefix(path, ".."+string(os.PathSeparator)) || path == ".." {
		return ""
	}
	return filepath.ToSlash(path)
}

func artifactContentType(relativePath string) string {
	switch strings.ToLower(filepath.Ext(relativePath)) {
	case ".pdf":
		return "application/pdf"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".txt", ".md":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

func (s *Server) getOrCreateSession(sessionID, threadID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		sess = &session{
			sessionID: sessionID,
			threadID:  threadID,
		}
		s.sessions[sessionID] = sess
	}
	return sess
}

func (s *Server) emitSessionUpdate(notify func(map[string]any), turnID string, update map[string]any) {
	if notify == nil || update == nil {
		return
	}
	update["turnId"] = turnID
	notify(shared.NotificationEnvelope("session.update", update))
}
