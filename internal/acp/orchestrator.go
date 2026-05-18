package acp

import (
	"context"
	"crypto/sha256"
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
	openClawAgentWaitTimeout             = 9 * time.Minute
	openClawNoDisplayableText            = "OpenClaw completed without displayable output."
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
		defer release()
		if rpcErr := ensureProductionGatewayConnected(o.server, gatewayProvider, notify); rpcErr != nil {
			return nil, rpcErr
		}
		return o.runOpenClawGatewayChat(ctx, params, gatewayProvider, turnID, notify)
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

func (o *SessionOrchestrator) runOpenClawGatewayChat(
	_ context.Context,
	params map[string]any,
	gatewayProvider string,
	turnID string,
	notify func(map[string]any),
) (map[string]any, *shared.RPCError) {
	collector := newOpenClawChatCollector()
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	threadID := strings.TrimSpace(shared.StringArg(params, "threadId", sessionID))
	if sessionID == "" {
		sessionID = threadID
	}
	notifyWithCollection := func(message map[string]any) {
		collector.observe(message)
		if notify == nil {
			return
		}
		if update := openClawGatewaySessionUpdate(message, sessionID, threadID, turnID); update != nil {
			notify(update)
		}
	}
	sessionKey := openClawSessionKey(params, turnID)
	chatParams, rpcErr := openClawChatSendParams(params, turnID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	artifactSinceUnixMs := time.Now().Add(-1 * time.Second).UnixMilli()
	preparedArtifact, prepareErr := o.openClawArtifactPrepare(
		gatewayProvider,
		sessionKey,
		turnID,
		notifyWithCollection,
	)
	if prepareErr != nil {
		return nil, prepareErr
	}
	applyOpenClawPreparedArtifactToChatParams(chatParams, preparedArtifact)
	logOpenClawArtifactSync(gatewayProvider, sessionKey, turnID, "prepare", true, false, false)
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
	runID := strings.TrimSpace(shared.StringArg(sendPayload, "runId", turnID))
	if runID != turnID {
		preparedArtifact, prepareErr = o.openClawArtifactPrepare(
			gatewayProvider,
			sessionKey,
			runID,
			notifyWithCollection,
		)
		if prepareErr != nil {
			return nil, prepareErr
		}
		logOpenClawArtifactSync(gatewayProvider, sessionKey, runID, "prepare", true, false, false)
	}
	waitStarted := time.Now()
	waitResult := o.openClawGatewayRequestWithRetry(
		gatewayProvider,
		"agent.wait",
		map[string]any{
			"runId":     runID,
			"timeoutMs": openClawAgentWaitTimeout.Milliseconds(),
		},
		openClawAgentWaitTimeout,
		notifyWithCollection,
	)
	logOpenClawGatewayTiming(
		gatewayProvider,
		"agent.wait",
		sessionKey,
		runID,
		time.Since(waitStarted),
		waitResult.OK,
	)
	if !waitResult.OK {
		return nil, gatewayRPCError(waitResult.Error, "openclaw agent.wait failed")
	}
	waitPayload := shared.AsMap(waitResult.Payload)
	output := collector.output()
	if output == "" {
		output = firstNonEmptyString(waitPayload, "output", "message", "summary", "assistantText", "text")
	}
	noDisplayableOutput := strings.TrimSpace(output) == ""
	if output == "" {
		output = openClawNoDisplayableText
	}
	result := map[string]any{
		"success":                   true,
		"output":                    output,
		"message":                   output,
		"summary":                   output,
		"turnId":                    turnID,
		"runId":                     runID,
		"mode":                      router.ExecutionTargetGatewayChat,
		"resolvedGatewayProviderId": gatewayProvider,
	}
	mergeOpenClawArtifactPayload(result, waitPayload)
	mergeOpenClawArtifactPayload(result, collector.artifactPayload())
	applyOpenClawPreparedArtifactToResult(result, preparedArtifact)
	artifactPayload := o.openClawArtifactExport(
		gatewayProvider,
		chatParams,
		runID,
		artifactSinceUnixMs,
		preparedArtifact,
		notifyWithCollection,
	)
	mergeOpenClawArtifactPayload(result, artifactPayload)
	result[openClawArtifactExportAttemptedField] = true
	exportedCount := openClawArtifactPayloadCount(result)
	logOpenClawArtifactSync(gatewayProvider, sessionKey, runID, "export", preparedArtifact != nil, exportedCount > 0, exportedCount == 0)
	o.server.decorateOpenClawArtifactDownloadURLs(result, shared.StringArg(chatParams, "sessionKey", ""), runID)
	stripOpenClawArtifactInlineContent(result)
	guardOpenClawNoDisplayableResult(result, noDisplayableOutput)
	if notify != nil {
		notify(shared.NotificationEnvelope("session.update", openClawGatewayCompletedResultUpdate(sessionID, threadID, turnID, result)))
	}
	return result, nil
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

func openClawArtifactPayloadCount(payload map[string]any) int {
	if payload == nil {
		return 0
	}
	remoteWorkingDirectory := strings.TrimSpace(shared.StringArg(payload, "remoteWorkingDirectory", ""))
	return len(extractArtifactPayloads(payload, remoteWorkingDirectory))
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
	sessionKey string,
	runID string,
	notify func(map[string]any),
) (*openClawPreparedArtifactScope, *shared.RPCError) {
	sessionKey = strings.TrimSpace(sessionKey)
	runID = strings.TrimSpace(runID)
	if sessionKey == "" || runID == "" {
		return nil, &shared.RPCError{Code: -32602, Message: "openclaw artifact prepare requires sessionKey and runId"}
	}
	prepareResult := o.openClawGatewayRequestWithRetry(
		gatewayProvider,
		"xworkmate.artifacts.prepare",
		map[string]any{
			"sessionKey": sessionKey,
			"runId":      runID,
		},
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

func applyOpenClawPreparedArtifactToChatParams(chatParams map[string]any, prepared *openClawPreparedArtifactScope) {
	if chatParams == nil || prepared == nil {
		return
	}
	chatParams["artifactScope"] = prepared.ArtifactScope
	chatParams["artifactDirectory"] = prepared.ArtifactDirectory
	chatParams["relativeArtifactDirectory"] = prepared.RelativeArtifactDirectory
	chatParams["artifactScopeKind"] = prepared.ScopeKind
	if prepared.RemoteWorkingDirectory != "" {
		chatParams["remoteWorkingDirectory"] = prepared.RemoteWorkingDirectory
	}
	if prepared.RemoteWorkspaceRefKind != "" {
		chatParams["remoteWorkspaceRefKind"] = prepared.RemoteWorkspaceRefKind
	}
	chatParams["xworkmateArtifacts"] = map[string]any{
		"artifactScope":             prepared.ArtifactScope,
		"artifactDirectory":         prepared.ArtifactDirectory,
		"relativeArtifactDirectory": prepared.RelativeArtifactDirectory,
		"scopeKind":                 prepared.ScopeKind,
	}
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

func openClawChatSendParams(
	params map[string]any,
	turnID string,
) (map[string]any, *shared.RPCError) {
	message := openClawCurrentTurnMessage(params)
	if message == "" {
		return nil, &shared.RPCError{Code: -32602, Message: "OPENCLAW_TASK_PROMPT_REQUIRED"}
	}
	sessionKey := openClawSessionKey(params, turnID)
	chatParams := map[string]any{
		"sessionKey":     sessionKey,
		"message":        message,
		"idempotencyKey": turnID,
	}
	if attachments := shared.ListArg(params, "attachments"); len(attachments) > 0 {
		chatParams["attachments"] = attachments
	}
	if thinking := strings.TrimSpace(shared.StringArg(params, "thinking", "")); thinking != "" {
		chatParams["thinking"] = thinking
	}
	return chatParams, nil
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

func openClawSessionKey(params map[string]any, turnID string) string {
	for _, key := range []string{"threadId", "sessionId"} {
		if value := strings.TrimSpace(shared.StringArg(params, key, "")); value != "" {
			return value
		}
	}
	if trimmed := strings.TrimSpace(turnID); trimmed != "" {
		return trimmed
	}
	return "main"
}

func (o *SessionOrchestrator) openClawArtifactExport(
	gatewayProvider string,
	chatParams map[string]any,
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
		"sessionKey":     sessionKey,
		"runId":          strings.TrimSpace(runID),
		"sinceUnixMs":    sinceUnixMs,
		"maxFiles":       64,
		"maxInlineBytes": 0,
		"includeContent": false,
	}
	if preparedArtifact != nil && strings.TrimSpace(preparedArtifact.ArtifactScope) != "" {
		exportParams["artifactScope"] = strings.TrimSpace(preparedArtifact.ArtifactScope)
	}
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

func guardOpenClawNoDisplayableResult(result map[string]any, noDisplayableOutput bool) {
	if !noDisplayableOutput || result == nil || !parseBool(result["success"]) {
		return
	}
	remoteWorkingDirectory := strings.TrimSpace(shared.StringArg(result, "remoteWorkingDirectory", ""))
	if len(extractArtifactPayloads(result, remoteWorkingDirectory)) > 0 {
		return
	}
	result["success"] = false
	result["status"] = "failed"
	result["code"] = "OPENCLAW_NO_DISPLAYABLE_OUTPUT"
	result["error"] = "openclaw returned no displayable output"
	result["message"] = openClawNoDisplayableText
	result["output"] = openClawNoDisplayableText
	result["summary"] = openClawNoDisplayableText
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
	return &shared.RPCError{Code: -32002, Message: message}
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
	if code == "SOCKET_CLOSED" || code == "SOCKET_FAILURE" || code == "OFFLINE" {
		return true
	}
	message := strings.TrimSpace(strings.ToLower(shared.StringArg(errorPayload, "message", "")))
	return strings.Contains(message, "socket closed")
}

func firstNonEmptyString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(shared.StringArg(values, key, "")); value != "" {
			return value
		}
	}
	return ""
}

type openClawChatCollector struct {
	parts            []string
	final            string
	artifactPayloads []map[string]any
}

func newOpenClawChatCollector() *openClawChatCollector {
	return &openClawChatCollector{}
}

func (c *openClawChatCollector) observe(notification map[string]any) {
	if c == nil {
		return
	}
	event := shared.AsMap(shared.AsMap(notification["params"])["event"])
	if len(event) == 0 {
		return
	}
	payload := shared.AsMap(event["payload"])
	if hasArtifactPayload(payload) {
		c.artifactPayloads = append(c.artifactPayloads, payload)
	}
	if strings.TrimSpace(shared.StringArg(event, "event", "")) != "chat.run" {
		return
	}
	text := firstNonEmptyString(payload, "assistantText", "text", "message", "output", "summary")
	if text == "" {
		return
	}
	if isTerminalGatewayPayload(payload) {
		c.final = text
		return
	}
	c.parts = append(c.parts, text)
}

func (c *openClawChatCollector) output() string {
	if c == nil {
		return ""
	}
	if strings.TrimSpace(c.final) != "" {
		return strings.TrimSpace(c.final)
	}
	return strings.TrimSpace(strings.Join(c.parts, ""))
}

func (c *openClawChatCollector) artifactPayload() map[string]any {
	if c == nil || len(c.artifactPayloads) == 0 {
		return nil
	}
	result := map[string]any{}
	for _, payload := range c.artifactPayloads {
		mergeOpenClawArtifactPayload(result, payload)
	}
	return result
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

func hasArtifactPayload(payload map[string]any) bool {
	if len(payload) == 0 {
		return false
	}
	for _, key := range []string{"artifacts", "files", "attachments", "remoteWorkingDirectory", "remoteWorkspaceRefKind"} {
		if _, ok := payload[key]; ok {
			return true
		}
	}
	return false
}

func isTerminalGatewayPayload(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if value, ok := payload["terminal"].(bool); ok && value {
		return true
	}
	switch strings.TrimSpace(strings.ToLower(shared.StringArg(payload, "state", ""))) {
	case "complete", "completed", "done", "ok", "success", "failed", "error", "timeout", "timed_out", "cancelled", "canceled":
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
	sessionKey := openClawSessionKey(params, turnID)
	runID := strings.TrimSpace(shared.StringArg(result, "runId", turnID))
	chatParams := map[string]any{"sessionKey": sessionKey}
	mergeOpenClawArtifactPayload(result, o.openClawArtifactExport(
		gatewayProvider,
		chatParams,
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
	if parseBool(params["multiAgent"]) {
		return TaskKindMultiAgent
	}
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
