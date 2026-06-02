package acp

import (
	"context"
	"strings"
	"time"

	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

const (
	openClawTaskProbeTimeout    = 2 * time.Second
	openClawTaskProbeTimeoutMs  = 1000
	openClawTaskMonitorInterval = time.Second
	openClawShortTaskMinutes    = 10
	openClawLongTaskMinutes     = 30
	openClawComplexTaskMinutes  = 60
)

type OpenClawTaskRecord struct {
	SessionID            string
	ThreadID             string
	TurnID               string
	RunID                string
	SessionKey           string
	GatewayProviderID    string
	TaskLoadClass        string
	ArtifactSinceUnixMs  int64
	RuntimeBudgetMinutes int
	StartedAt            time.Time
	DeadlineAt           time.Time
	LastProbeAt          time.Time
	ProgressStage        string
	ProgressMessage      string
	ProgressTerminal     bool
	ChatParams           map[string]any
	PreparedArtifact     *openClawPreparedArtifactScope
	ArtifactContract     openClawArtifactContract
	ResolvedModel        string
	ResolvedSkills       []string
	MonitorStarted       bool
	ProbeInFlight        bool
	AdmissionRelease     func()
}

func openClawTaskRuntimePolicy(params map[string]any, chatParams map[string]any, contract openClawArtifactContract) (string, int) {
	message := strings.TrimSpace(shared.StringArg(chatParams, "message", ""))
	if message == "" {
		message = openClawCurrentTurnMessage(params)
	}
	lower := strings.ToLower(message)
	metadataClass := strings.TrimSpace(shared.StringArg(shared.AsMap(params["metadata"]), "taskLoadClass", ""))
	if metadataClass == "complex_long_chain_task" || contract.ComplexLongChain || openClawMessageContainsAny(lower, []string{
		"复杂链路", "多章节", "每章", "拆章节", "汇总排版", "gpt images", "images2", "image generation", "视频", "渲染", "hyperframes", "remotion", "ffmpeg",
	}) {
		return "complex_chain_task", openClawComplexTaskMinutes
	}
	if len(contract.RequiredFinalExtensions) > 0 || openClawMessageContainsAny(lower, []string{
		"生成文件", "同步生成文件", "产物", "附件", "pdf", "docx", "ppt", "pptx", "markdown", ".md", "png", "jpg", "jpeg", "mp4",
	}) || len(shared.ListArg(params, "attachments"))+len(shared.ListArg(params, "inlineAttachments")) >= 2 {
		return "long_task", openClawLongTaskMinutes
	}
	return "short_task", openClawShortTaskMinutes
}

func openClawRunningTaskResult(record *OpenClawTaskRecord) map[string]any {
	if record == nil {
		return map[string]any{"success": true, "status": "running", "mode": router.ExecutionTargetGatewayChat}
	}
	result := map[string]any{
		"success":                   true,
		"status":                    string(TaskStateRunning),
		"turnId":                    record.TurnID,
		"runId":                     record.RunID,
		"sessionId":                 record.SessionID,
		"threadId":                  record.ThreadID,
		"sessionKey":                record.SessionKey,
		"mode":                      router.ExecutionTargetGatewayChat,
		"resolvedGatewayProviderId": record.GatewayProviderID,
		"taskLoadClass":             record.TaskLoadClass,
		"runtimeBudgetMinutes":      record.RuntimeBudgetMinutes,
		"startedAt":                 record.StartedAt.UTC().Format(time.RFC3339Nano),
		"deadlineAt":                record.DeadlineAt.UTC().Format(time.RFC3339Nano),
		"progress":                  openClawTaskProgress(record),
	}
	if record.PreparedArtifact != nil {
		applyOpenClawPreparedArtifactToResult(result, record.PreparedArtifact)
	}
	if len(record.ArtifactContract.RequiredFinalExtensions) > 0 {
		result["requiredArtifactExtensions"] = append([]string(nil), record.ArtifactContract.RequiredFinalExtensions...)
	}
	if len(record.ArtifactContract.ExpectedArtifactExtensions) > 0 {
		result["expectedArtifactExtensions"] = append([]string(nil), record.ArtifactContract.ExpectedArtifactExtensions...)
	}
	return result
}

func openClawTaskProgress(record *OpenClawTaskRecord) map[string]any {
	now := time.Now()
	stage := strings.TrimSpace(record.ProgressStage)
	if stage == "" {
		stage = "running"
	}
	message := strings.TrimSpace(record.ProgressMessage)
	if message == "" {
		message = "OpenClaw task is running"
	}
	return map[string]any{
		"stage":         stage,
		"message":       message,
		"elapsedMs":     maxInt64(0, now.Sub(record.StartedAt).Milliseconds()),
		"budgetMs":      (time.Duration(record.RuntimeBudgetMinutes) * time.Minute).Milliseconds(),
		"lastProbeAtMs": record.LastProbeAt.UnixMilli(),
		"terminal":      record.ProgressTerminal,
	}
}

func maxInt64(a int64, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (o *SessionOrchestrator) startOpenClawTaskMonitor(sess *session) {
	if sess == nil {
		return
	}
	sess.mu.Lock()
	record := sess.openClaw
	if record == nil || record.MonitorStarted || isTerminalTaskState(sess.task.State) {
		sess.mu.Unlock()
		return
	}
	record.MonitorStarted = true
	sess.mu.Unlock()

	go func() {
		defer o.releaseOpenClawAdmission(sess)
		time.Sleep(openClawTaskMonitorInterval)
		for {
			sess.mu.Lock()
			state := sess.task.State
			deadline := sess.task.DeadlineAt
			sess.mu.Unlock()
			if isTerminalTaskState(state) {
				return
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				o.failOpenClawTask(sess, "TASK_SLA_EXPIRED", "OpenClaw task exceeded its runtime SLA")
				return
			}
			o.probeOpenClawTask(context.Background(), sess, nil)
			sess.mu.Lock()
			state = sess.task.State
			sess.mu.Unlock()
			if isTerminalTaskState(state) {
				return
			}
			time.Sleep(openClawTaskMonitorInterval)
		}
	}()
}

func isTerminalTaskState(state TaskState) bool {
	return state == TaskStateCompleted || state == TaskStateFailed || state == TaskStateCancelled
}

func (o *SessionOrchestrator) releaseOpenClawAdmission(sess *session) {
	if sess == nil {
		return
	}
	var release func()
	sess.mu.Lock()
	if sess.openClaw != nil {
		release = sess.openClaw.AdmissionRelease
		sess.openClaw.AdmissionRelease = nil
	}
	sess.mu.Unlock()
	if release != nil {
		release()
	}
}

func (o *SessionOrchestrator) failOpenClawTask(sess *session, code string, message string) map[string]any {
	if sess == nil {
		return map[string]any{"success": false, "status": string(TaskStateFailed), "code": code, "message": message}
	}
	if strings.TrimSpace(message) == "" {
		message = code
	}
	sess.mu.Lock()
	turnID := sess.task.TurnID
	runID := sess.task.RunID
	gatewayProviderID := sess.task.GatewayProviderID
	sess.task.State = TaskStateFailed
	sess.task.UpdatedAt = time.Now()
	sess.task.ProgressStage = "failed"
	sess.task.ProgressMessage = message
	sess.task.ProgressTerminal = true
	if sess.openClaw != nil {
		sess.openClaw.ProgressStage = "failed"
		sess.openClaw.ProgressMessage = message
		sess.openClaw.ProgressTerminal = true
		sess.openClaw.ProbeInFlight = false
	}
	result := map[string]any{
		"success":                   false,
		"status":                    string(TaskStateFailed),
		"code":                      code,
		"error":                     message,
		"message":                   message,
		"summary":                   message,
		"output":                    message,
		"turnId":                    turnID,
		"runId":                     runID,
		"mode":                      router.ExecutionTargetGatewayChat,
		"resolvedGatewayProviderId": gatewayProviderID,
	}
	sess.lastResult = cloneMap(result)
	sess.mu.Unlock()
	o.releaseOpenClawAdmission(sess)
	return result
}

func (o *SessionOrchestrator) probeOpenClawTask(ctx context.Context, sess *session, notify func(map[string]any)) map[string]any {
	if sess == nil {
		return map[string]any{"status": "not_found"}
	}
	sess.mu.Lock()
	record := sess.openClaw
	if record == nil {
		snapshot := openClawSessionSnapshotLocked(sess)
		sess.mu.Unlock()
		return snapshot
	}
	if isTerminalTaskState(sess.task.State) {
		snapshot := openClawSessionSnapshotLocked(sess)
		sess.mu.Unlock()
		return snapshot
	}
	if !record.DeadlineAt.IsZero() && time.Now().After(record.DeadlineAt) {
		sess.mu.Unlock()
		return o.failOpenClawTask(sess, "TASK_SLA_EXPIRED", "OpenClaw task exceeded its runtime SLA")
	}
	if record.ProbeInFlight {
		result := openClawRunningTaskResult(record)
		sess.lastResult = cloneMap(result)
		sess.mu.Unlock()
		return result
	}
	record.ProbeInFlight = true
	record.LastProbeAt = time.Now()
	record.ProgressStage = "probing"
	record.ProgressMessage = "Checking OpenClaw task status"
	sess.task.LastProbeAt = record.LastProbeAt
	sess.task.ProgressStage = record.ProgressStage
	sess.task.ProgressMessage = record.ProgressMessage
	gatewayProvider := record.GatewayProviderID
	runID := record.RunID
	sessionID := record.SessionID
	threadID := record.ThreadID
	turnID := record.TurnID
	sess.mu.Unlock()

	collector := newOpenClawChatCollector()
	notifyWithCollection := func(message map[string]any) {
		collector.observe(message)
		if notify == nil {
			return
		}
		if update := openClawGatewaySessionUpdate(message, sessionID, threadID, turnID); update != nil {
			notify(update)
		}
	}
	waitStarted := time.Now()
	waitResult := o.openClawGatewayRequestWithRetry(
		gatewayProvider,
		"agent.wait",
		map[string]any{
			"runId":     runID,
			"timeoutMs": openClawTaskProbeTimeoutMs,
		},
		openClawTaskProbeTimeout,
		notifyWithCollection,
	)
	logOpenClawGatewayTiming(
		gatewayProvider,
		"agent.wait.probe",
		sessionID,
		runID,
		time.Since(waitStarted),
		waitResult.OK,
	)
	if !waitResult.OK {
		if openClawProbeStillRunning(waitResult.Error) {
			return openClawMarkProbeRunning(sess)
		}
		code := strings.TrimSpace(shared.StringArg(waitResult.Error, "code", "OPENCLAW_WAIT_FAILED"))
		message := strings.TrimSpace(shared.StringArg(waitResult.Error, "message", "openclaw wait failed"))
		return o.failOpenClawTask(sess, code, message)
	}
	waitPayload := shared.AsMap(waitResult.Payload)
	if !openClawWaitPayloadTerminal(waitPayload) && !collector.isTerminal() {
		return openClawMarkProbeRunning(sess)
	}
	return o.completeOpenClawTask(sess, waitPayload, collector, notify)
}

func openClawMarkProbeRunning(sess *session) map[string]any {
	sess.mu.Lock()
	if sess.openClaw != nil {
		sess.openClaw.ProgressStage = "running"
		sess.openClaw.ProgressMessage = "OpenClaw task is still running"
		sess.openClaw.ProbeInFlight = false
	}
	sess.task.ProgressStage = "running"
	sess.task.ProgressMessage = "OpenClaw task is still running"
	sess.task.UpdatedAt = time.Now()
	result := openClawRunningTaskResult(sess.openClaw)
	sess.lastResult = cloneMap(result)
	sess.mu.Unlock()
	return result
}

func openClawWaitPayloadTerminal(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if value, ok := payload["terminal"].(bool); ok {
		return value
	}
	for _, key := range []string{"status", "state", "phase"} {
		switch strings.TrimSpace(strings.ToLower(shared.StringArg(payload, key, ""))) {
		case "complete", "completed", "done", "final", "success", "succeeded", "failed", "failure", "error", "timeout", "timed_out", "cancelled", "canceled":
			return true
		}
	}
	return false
}

func openClawProbeStillRunning(errorPayload map[string]any) bool {
	code := strings.TrimSpace(strings.ToUpper(shared.StringArg(errorPayload, "code", "")))
	if code == "TIMEOUT" || code == "RPC_TIMEOUT" || code == "REQUEST_TIMEOUT" ||
		code == "OFFLINE" || code == "SOCKET_FAILURE" || code == "SOCKET_CLOSED" {
		return true
	}
	message := strings.TrimSpace(strings.ToLower(shared.StringArg(errorPayload, "message", "")))
	return strings.Contains(message, "timeout") || strings.Contains(message, "timed out")
}

func (o *SessionOrchestrator) completeOpenClawTask(
	sess *session,
	waitPayload map[string]any,
	collector *openClawChatCollector,
	notify func(map[string]any),
) map[string]any {
	if sess == nil {
		return map[string]any{"status": "not_found"}
	}
	sess.mu.Lock()
	record := sess.openClaw
	if record == nil {
		snapshot := openClawSessionSnapshotLocked(sess)
		sess.mu.Unlock()
		return snapshot
	}
	if isTerminalTaskState(sess.task.State) {
		record.ProbeInFlight = false
		snapshot := openClawSessionSnapshotLocked(sess)
		sess.mu.Unlock()
		return snapshot
	}
	sess.mu.Unlock()

	output := ""
	if collector != nil {
		output = collector.output()
	}
	if output == "" {
		output = firstNonEmptyString(waitPayload, "output", "message", "summary", "assistantText", "text")
	}
	noDisplayableOutput := strings.TrimSpace(output) == ""
	if output == "" {
		output = openClawNoDisplayableText
	}
	result := map[string]any{
		"success":                   true,
		"status":                    string(TaskStateCompleted),
		"output":                    output,
		"message":                   output,
		"summary":                   output,
		"turnId":                    record.TurnID,
		"runId":                     record.RunID,
		"sessionId":                 record.SessionID,
		"threadId":                  record.ThreadID,
		"sessionKey":                record.SessionKey,
		"mode":                      router.ExecutionTargetGatewayChat,
		"resolvedExecutionTarget":   router.ExecutionTargetGatewayChat,
		"resolvedProviderId":        record.GatewayProviderID,
		"resolvedGatewayProviderId": record.GatewayProviderID,
		"resolvedModel":             record.ResolvedModel,
		"resolvedSkills":            append([]string(nil), record.ResolvedSkills...),
		"taskLoadClass":             record.TaskLoadClass,
		"runtimeBudgetMinutes":      record.RuntimeBudgetMinutes,
	}
	mergeOpenClawArtifactPayload(result, waitPayload)
	if collector != nil {
		mergeOpenClawArtifactPayload(result, collector.artifactPayload())
	}
	applyOpenClawPreparedArtifactToResult(result, record.PreparedArtifact)
	artifactPayload := o.openClawArtifactExport(
		record.GatewayProviderID,
		record.ChatParams,
		record.RunID,
		record.ArtifactSinceUnixMs,
		record.PreparedArtifact,
		notify,
	)
	mergeOpenClawArtifactPayload(result, artifactPayload)
	result[openClawArtifactExportAttemptedField] = true
	exportedCount := openClawArtifactPayloadCount(result)
	logOpenClawArtifactSync(record.GatewayProviderID, record.SessionKey, record.RunID, "export", record.PreparedArtifact != nil, exportedCount > 0, exportedCount == 0)
	o.server.decorateOpenClawArtifactDownloadURLs(result, record.SessionKey, record.RunID)
	stripOpenClawArtifactInlineContent(result)
	applyOpenClawArtifactContractResult(result, record.ArtifactContract)
	guardOpenClawAgentFailedBeforeReplyResult(result)
	guardOpenClawNoDisplayableResult(result, noDisplayableOutput)
	delete(result, openClawArtifactExportAttemptedField)

	success := parseBool(result["success"])
	state := TaskStateCompleted
	stage := "completed"
	if !success {
		state = TaskStateFailed
		stage = "failed"
	}
	artifactRecord := buildArtifactRecord(sess, result, output)
	if len(artifactRecord.Artifacts) > 0 {
		result["artifacts"] = artifactRecord.Artifacts
	}
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
	sess.task.State = state
	sess.task.UpdatedAt = time.Now()
	sess.task.ProgressStage = stage
	sess.task.ProgressMessage = output
	sess.task.ProgressTerminal = true
	if sess.openClaw != nil {
		sess.openClaw.ProgressStage = stage
		sess.openClaw.ProgressMessage = output
		sess.openClaw.ProgressTerminal = true
		sess.openClaw.ProbeInFlight = false
	}
	if output != "" {
		sess.history = append(sess.history, "ASSISTANT: "+output)
	}
	sess.artifacts = artifactRecord
	sess.lastResult = cloneMap(result)
	sess.mu.Unlock()
	o.releaseOpenClawAdmission(sess)
	if notify != nil {
		notify(shared.NotificationEnvelope("session.update", openClawGatewayCompletedResultUpdate(record.SessionID, record.ThreadID, record.TurnID, result)))
	}
	return result
}

func openClawSessionSnapshotLocked(sess *session) map[string]any {
	payload := map[string]any{
		"status":    string(sess.task.State),
		"sessionId": sess.sessionID,
		"threadId":  sess.threadID,
		"task":      openClawTaskMapLocked(sess),
	}
	if len(sess.lastResult) > 0 {
		payload["result"] = cloneMap(sess.lastResult)
	}
	if len(sess.artifacts.Artifacts) > 0 ||
		sess.artifacts.RemoteWorkingDirectory != "" ||
		sess.artifacts.RemoteWorkspaceRefKind != "" ||
		sess.artifacts.ResultSummary != "" {
		payload["artifacts"] = map[string]any{
			"items":                  cloneMapSlice(sess.artifacts.Artifacts),
			"remoteWorkingDirectory": sess.artifacts.RemoteWorkingDirectory,
			"remoteWorkspaceRefKind": sess.artifacts.RemoteWorkspaceRefKind,
			"resultSummary":          sess.artifacts.ResultSummary,
			"updatedAt":              sess.artifacts.UpdatedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	if sess.openClaw != nil {
		payload["progress"] = openClawTaskProgress(sess.openClaw)
	}
	return payload
}

func openClawTaskMapLocked(sess *session) map[string]any {
	task := sess.task
	payload := map[string]any{
		"sessionId":            task.SessionID,
		"threadId":             task.ThreadID,
		"turnId":               task.TurnID,
		"runId":                task.RunID,
		"sessionKey":           task.SessionKey,
		"provider":             task.Provider,
		"target":               task.Target,
		"gatewayProviderId":    task.GatewayProviderID,
		"state":                string(task.State),
		"kind":                 string(task.Kind),
		"taskLoadClass":        task.TaskLoadClass,
		"artifactScope":        task.ArtifactScope,
		"artifactDirectory":    task.ArtifactDirectory,
		"runtimeBudgetMinutes": task.RuntimeBudgetMinutes,
		"updatedAt":            task.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if !task.StartedAt.IsZero() {
		payload["startedAt"] = task.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if !task.DeadlineAt.IsZero() {
		payload["deadlineAt"] = task.DeadlineAt.UTC().Format(time.RFC3339Nano)
	}
	if !task.LastProbeAt.IsZero() {
		payload["lastProbeAt"] = task.LastProbeAt.UTC().Format(time.RFC3339Nano)
	}
	if task.ProgressStage != "" || task.ProgressMessage != "" {
		payload["progress"] = map[string]any{
			"stage":    task.ProgressStage,
			"message":  task.ProgressMessage,
			"terminal": task.ProgressTerminal,
		}
	}
	return payload
}
