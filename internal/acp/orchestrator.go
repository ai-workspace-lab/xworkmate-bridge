package acp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xworkmate-bridge/internal/memory"
	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

type SessionOrchestrator struct {
	server *Server
}

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
	if rpcErr := ensureProductionGatewayConnected(o.server, gatewayProvider, notify); rpcErr != nil {
		return nil, rpcErr
	}
	params = withResolvedGatewayProvider(params, gatewayProvider)
	if isOpenClawMode(gatewayProvider) && isSessionTaskMethod(method) {
		return o.runOpenClawGatewayChat(ctx, params, gatewayProvider, turnID, notify)
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
	notifyWithCollection := func(message map[string]any) {
		collector.observe(message)
		if notify != nil {
			notify(message)
		}
	}
	artifactDeliveryRequired := openClawArtifactDeliveryRequired(params)
	sessionKey := openClawSessionKey(params, turnID)
	artifactRunID := turnID
	var preparedArtifact *openClawPreparedArtifactScope
	if artifactDeliveryRequired {
		var rpcErr *shared.RPCError
		preparedArtifact, rpcErr = o.openClawArtifactPrepare(
			gatewayProvider,
			sessionKey,
			artifactRunID,
			notifyWithCollection,
		)
		if rpcErr != nil {
			return nil, rpcErr
		}
	}
	chatParams, rpcErr := openClawChatSendParams(params, turnID, preparedArtifact)
	if rpcErr != nil {
		return nil, rpcErr
	}
	artifactSinceUnixMs := time.Now().Add(-1 * time.Second).UnixMilli()
	sendResult := o.server.gateway.RequestByMode(
		gatewayProvider,
		"chat.send",
		chatParams,
		2*time.Minute,
		notifyWithCollection,
	)
	if !sendResult.OK {
		return nil, gatewayRPCError(sendResult.Error, "openclaw chat.send failed")
	}
	sendPayload := shared.AsMap(sendResult.Payload)
	runID := strings.TrimSpace(shared.StringArg(sendPayload, "runId", turnID))
	waitResult := o.server.gateway.RequestByMode(
		gatewayProvider,
		"agent.wait",
		map[string]any{
			"runId":     runID,
			"timeoutMs": 120000,
		},
		2*time.Minute,
		notifyWithCollection,
	)
	if !waitResult.OK {
		return nil, gatewayRPCError(waitResult.Error, "openclaw agent.wait failed")
	}
	waitPayload := shared.AsMap(waitResult.Payload)
	output := collector.output()
	if output == "" {
		output = firstNonEmptyString(waitPayload, "output", "message", "summary", "assistantText", "text")
	}
	if output == "" {
		output = "OpenClaw completed without displayable output."
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
	mergeOpenClawArtifactPayload(result, o.openClawArtifactExport(
		gatewayProvider,
		chatParams,
		artifactRunID,
		artifactSinceUnixMs,
		preparedArtifact,
		artifactDeliveryRequired,
		notifyWithCollection,
	))
	o.server.decorateOpenClawArtifactDownloadURLs(result, shared.StringArg(chatParams, "sessionKey", ""), artifactRunID)
	stripOpenClawArtifactInlineContent(result)
	guardOpenClawArtifactResult(result, artifactDeliveryRequired)
	return result, nil
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
	ArtifactScope     string
	ArtifactDirectory string
	ScopeKind         string
}

func openClawChatSendParams(
	params map[string]any,
	turnID string,
	preparedArtifact *openClawPreparedArtifactScope,
) (map[string]any, *shared.RPCError) {
	message := firstNonEmptyString(params, "taskPrompt", "prompt", "message")
	if message == "" {
		return nil, &shared.RPCError{Code: -32602, Message: "OPENCLAW_TASK_PROMPT_REQUIRED"}
	}
	if openClawArtifactDeliveryRequired(params) {
		message = withOpenClawArtifactDeliveryInstructions(message, preparedArtifact)
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

func openClawArtifactDeliveryRequired(params map[string]any) bool {
	text := strings.ToLower(firstNonEmptyString(params, "taskPrompt", "prompt", "message"))
	if strings.TrimSpace(text) == "" {
		return false
	}
	fileSignals := []string{
		"ppt", "pptx", "powerpoint", "slide", "slides",
		"pdf", "docx", "word", "xlsx", "excel",
		"artifact", "artifacts", "file", "files", "download", "attachment", "asset", "output",
		"image", "photo", "picture", "screenshot", "video", "audio", "csv", "json", "html",
		"zip", "tar", "archive", "dataset", "report", "document", "markdown", "code",
		"文件", "制品", "产物", "下载", "附件", "素材", "输出", "图片", "截图", "图像",
		"视频", "音频", "压缩包", "数据集", "文档", "报告", "演示", "幻灯片", "表格", "代码",
	}
	actionSignals := []string{
		"create", "generate", "build", "make", "write", "export", "output", "deliver", "download",
		"save", "produce", "render", "attach", "return",
		"生成", "制作", "输出", "导出", "下载", "交付", "收取", "保存", "渲染", "返回", "提供",
	}
	hasFileSignal := false
	for _, signal := range fileSignals {
		if strings.Contains(text, signal) {
			hasFileSignal = true
			break
		}
	}
	if !hasFileSignal {
		return false
	}
	for _, signal := range actionSignals {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func withOpenClawArtifactDeliveryInstructions(
	message string,
	preparedArtifact *openClawPreparedArtifactScope,
) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return message
	}
	lines := []string{
		"XWorkmate artifact delivery requirements:",
		"- Create the requested files as real files before finishing.",
	}
	if preparedArtifact != nil && strings.TrimSpace(preparedArtifact.ArtifactDirectory) != "" {
		lines = append(lines,
			"- Write every deliverable file into this exact directory:",
			fmt.Sprintf("  `%s`", strings.TrimSpace(preparedArtifact.ArtifactDirectory)),
			"- Do not write deliverable files outside that directory.",
		)
	}
	lines = append(lines,
		"- If multiple formats are requested, write each requested format as a separate file with the correct extension.",
		"- Do not claim that files are ready, downloadable, or clickable unless the files actually exist on disk.",
		"- In the final response, list only the real file names you created. Do not invent download links.",
	)
	return message + "\n\n" + strings.Join(lines, "\n")
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

func (o *SessionOrchestrator) openClawArtifactPrepare(
	gatewayProvider string,
	sessionKey string,
	runID string,
	notify func(map[string]any),
) (*openClawPreparedArtifactScope, *shared.RPCError) {
	sessionKey = strings.TrimSpace(sessionKey)
	runID = strings.TrimSpace(runID)
	if sessionKey == "" || runID == "" {
		return nil, &shared.RPCError{Code: -32602, Message: "OPENCLAW_ARTIFACT_SCOPE_REQUIRED"}
	}
	prepareResult := o.server.gateway.RequestByMode(
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
	payload := shared.AsMap(prepareResult.Payload)
	prepared := &openClawPreparedArtifactScope{
		ArtifactScope:     strings.TrimSpace(shared.StringArg(payload, "artifactScope", "")),
		ArtifactDirectory: strings.TrimSpace(shared.StringArg(payload, "artifactDirectory", "")),
		ScopeKind:         strings.TrimSpace(shared.StringArg(payload, "scopeKind", "")),
	}
	if prepared.ArtifactScope == "" || prepared.ArtifactDirectory == "" {
		return nil, &shared.RPCError{Code: -32002, Message: "openclaw artifact prepare returned invalid scope"}
	}
	return prepared, nil
}

func (o *SessionOrchestrator) openClawArtifactExport(
	gatewayProvider string,
	chatParams map[string]any,
	runID string,
	sinceUnixMs int64,
	preparedArtifact *openClawPreparedArtifactScope,
	latestIfEmpty bool,
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
	if latestIfEmpty {
		exportParams["latestIfEmpty"] = true
	}
	exportResult := o.server.gateway.RequestByMode(
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

func guardOpenClawArtifactResult(result map[string]any, artifactDeliveryRequired bool) {
	if !artifactDeliveryRequired || result == nil {
		return
	}
	remoteWorkingDirectory := strings.TrimSpace(shared.StringArg(result, "remoteWorkingDirectory", ""))
	if len(extractArtifactPayloads(result, remoteWorkingDirectory)) > 0 {
		return
	}
	message := "未检测到 OpenClaw 本轮导出的实际文件。已阻止口头下载声明进入 artifacts 面板；请重新执行并要求 OpenClaw 在 workspace 中真实生成文件。"
	result["success"] = false
	result["status"] = "artifact_missing"
	result["output"] = message
	result["message"] = message
	result["summary"] = message
	result["artifactWarnings"] = appendArtifactList(
		result["artifactWarnings"],
		[]any{"OpenClaw artifact export returned no files for a file-delivery request."},
	)
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
	message := strings.TrimSpace(shared.StringArg(errorPayload, "message", fallback))
	if message == "" {
		message = fallback
	}
	return &shared.RPCError{Code: -32002, Message: message}
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
	result["status"] = "completed"
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

func openClawArtifactResponse(result map[string]any, routing RoutingResult, params map[string]any) bool {
	if routing.TargetID != "gateway" {
		return false
	}
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
	return strings.HasPrefix(strings.TrimSpace(shared.StringArg(result, "artifactScope", "")), ".xworkmate/artifacts/tasks/")
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
