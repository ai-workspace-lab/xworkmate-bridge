package acp

import (
	"log"
	"strings"
	"time"

	"xworkmate-bridge/internal/shared"
)

// 持久 run 仓 / run 关联与 WS 解耦（T7/T8/T9）。
//
// 背景：OpenClaw gateway turn 采用异步模型——chat.send 快速返回 runId，bridge 把
// run 记录(sess.openClaw)、预算(sess.task.DeadlineAt)、运行句柄(sess.lastResult)
// 落在「按 sessionID 维度」的 per-session store 里（s.sessions），其生命周期独立于
// bridge↔gateway 的 WebSocket 连接。客户端随后轮询 tasks.get。
//
// 此前 tasks.get 每次都强依赖 gateway 应答：一旦 WS 抖动 / 重连后 run 内存态丢失，
// tasks.get 回 not_found 或 socket_closed，已完成的结果就此丢失，客户端要么硬失败、
// 要么（修复前）无限轮询。下列辅助把 tasks.get 改造为「优先用持久 run 仓兜底」：
//
//	T8  已观察到的终态结果缓存进 sess.lastResult，gateway 之后查不到也不丢；
//	T7  gateway 暂时无法确认（unavailable / socket closed / not_found）但 run 仍在预算内时，
//	    合成一个 running 句柄让客户端继续轮询，跨越瞬时抖动（与 WS 生命周期解耦）；
//	T9  run 超过 DeadlineAt 且 gateway 仍无法确认时，回确定性的 interrupted 终态。

// openClawTaskGetResultIsTerminal 判断一个 tasks.get 结果是否表示 run 已结束。
// 注意：仍在 artifact 同步中的结果会被 normalizeOpenClawTaskGetResult 重写为 status=running，
// 因此这里只认显式终态，不会把「同步中」误判为终态。
func openClawTaskGetResultIsTerminal(payload map[string]any) bool {
	switch strings.ToLower(strings.TrimSpace(shared.StringArg(payload, "status", ""))) {
	case string(TaskStateCompleted), string(TaskStateFailed), string(TaskStateCancelled),
		"interrupted", "partially_delivered":
		return true
	}
	return false
}

// cacheOpenClawTaskGetResultIfTerminal 把一次 gateway 确认的终态结果落进 per-session 持久 run 仓（T8）。
func (s *Server) cacheOpenClawTaskGetResultIfTerminal(params map[string]any, payload map[string]any) {
	if len(payload) == 0 || !openClawTaskGetResultIsTerminal(payload) {
		return
	}
	sess := s.findTaskSession(params)
	if sess == nil {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	switch strings.ToLower(strings.TrimSpace(shared.StringArg(payload, "status", ""))) {
	case string(TaskStateFailed):
		sess.task.State = TaskStateFailed
	case string(TaskStateCancelled):
		sess.task.State = TaskStateCancelled
	default:
		sess.task.State = TaskStateCompleted
	}
	sess.task.ProgressTerminal = true
	sess.task.ProgressStage = strings.ToLower(strings.TrimSpace(shared.StringArg(payload, "status", "")))
	sess.task.UpdatedAt = time.Now()
	sess.lastResult = cloneMap(payload)
}

// cachedTerminalOpenClawResult 返回某 run 此前已观察到的终态结果（若有）（T7/T8）。
func (s *Server) cachedTerminalOpenClawResult(params map[string]any) (map[string]any, bool) {
	sess := s.findTaskSession(params)
	if sess == nil {
		return nil, false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return cachedTerminalForRunLocked(sess, params)
}

// cachedTerminalForRunLocked 仅当缓存终态确实属于「本次请求的 runId」时才命中，
// 防止同一 session 复用后把旧 run 的终态错配给新 run。调用方须持有 sess.mu。
func cachedTerminalForRunLocked(sess *session, params map[string]any) (map[string]any, bool) {
	if !sess.task.ProgressTerminal || len(sess.lastResult) == 0 {
		return nil, false
	}
	if !openClawTaskGetResultIsTerminal(sess.lastResult) {
		return nil, false
	}
	requestedRun := strings.TrimSpace(shared.StringArg(params, "runId", ""))
	if requestedRun == "" {
		requestedRun = strings.TrimSpace(shared.StringArg(params, "taskId", ""))
	}
	if requestedRun != "" {
		cachedRun := firstNonEmptyString(sess.lastResult, "runId", "taskId")
		if cachedRun != "" && !strings.EqualFold(cachedRun, requestedRun) {
			return nil, false
		}
	}
	return cloneMap(sess.lastResult), true
}

// openClawTaskGetGatewayUnconfirmedFallback 在 gateway 无法确认 run 时，用持久 run 仓兜底（T7/T9）：
//   - 已有缓存终态     -> 直接返回；
//   - run 仍在预算内   -> 合成 running 句柄，客户端继续轮询，跨越瞬时抖动；
//   - run 超过 deadline -> 回确定性 interrupted 终态。
//
// 没有任何 per-session 记录时退回旧行为（not_found），不改变无状态查询的语义。
func (s *Server) openClawTaskGetGatewayUnconfirmedFallback(params map[string]any, code string, message string) map[string]any {
	notFound := func() map[string]any {
		return map[string]any{
			"ok":      false,
			"status":  "not_found",
			"code":    fallbackString(code, "TASK_LOOKUP_FAILED"),
			"message": fallbackString(message, "openclaw native task lookup failed"),
		}
	}
	sess := s.findTaskSession(params)
	if sess == nil {
		return notFound()
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if cached, ok := cachedTerminalForRunLocked(sess, params); ok {
		return cached
	}
	if sess.openClaw == nil {
		return notFound()
	}
	now := time.Now()
	if !sess.task.DeadlineAt.IsZero() && now.After(sess.task.DeadlineAt) {
		return s.markOpenClawRunDeadlineInterruptedLocked(sess, code, message)
	}
	// 仍在预算内：合成 running 句柄让客户端继续轮询，不因一次瞬时抖动硬失败。
	metricTaskGetUnconfirmedFallbackInc() // T12
	running := openClawRunningTaskResult(sess.openClaw)
	running["transportDegraded"] = true
	if strings.TrimSpace(code) != "" {
		running["transportDegradedCode"] = strings.TrimSpace(code)
	}
	// T11：带 runId 的日志，便于与 App / 插件 / 网关四层按 runId 串联。
	log.Printf("level=warn component=openclaw_run_registry event=tasks_get_unconfirmed_fallback runId=%q openclawSessionKey=%q code=%q",
		sess.openClaw.RunID, sess.openClaw.SessionKey, strings.TrimSpace(code))
	sess.lastResult = cloneMap(running)
	return running
}

// markOpenClawRunDeadlineInterruptedLocked 为「超过预算且 gateway 无法确认」的 run 生成确定性
// interrupted 终态（T9）。调用方须持有 sess.mu。
func (s *Server) markOpenClawRunDeadlineInterruptedLocked(sess *session, code string, message string) map[string]any {
	now := time.Now()
	sess.task.State = TaskStateFailed
	sess.task.ProgressTerminal = true
	sess.task.ProgressStage = "interrupted"
	sess.task.ProgressMessage = "OpenClaw run exceeded its budget and could not be confirmed"
	sess.task.UpdatedAt = now
	metricRunDeadlineInterruptInc() // T12
	// T11：带 runId 的终态日志。
	if sess.openClaw != nil {
		log.Printf("level=warn component=openclaw_run_registry event=run_deadline_interrupt runId=%q openclawSessionKey=%q deadlineAt=%q code=%q",
			sess.openClaw.RunID, sess.openClaw.SessionKey,
			sess.openClaw.DeadlineAt.UTC().Format(time.RFC3339Nano), strings.TrimSpace(code))
	}

	result := map[string]any{
		"ok":                 true,
		"success":            false,
		"status":             "interrupted",
		"event":              "interrupted",
		"pending":            false,
		"code":               "OPENCLAW_RUN_DEADLINE_EXCEEDED",
		"artifactSyncStatus": "interrupted",
		"message":            "OpenClaw 任务超过预算上限且网关无法确认结果，已结束本轮等待。任务可能已在后台完成，请重新发送请求以拿回结果。",
		"artifacts":          []any{},
	}
	if strings.TrimSpace(code) != "" {
		result["gatewayUnconfirmedCode"] = strings.TrimSpace(code)
	}
	if strings.TrimSpace(message) != "" {
		result["gatewayUnconfirmedMessage"] = strings.TrimSpace(message)
	}
	if record := sess.openClaw; record != nil {
		result["runId"] = record.RunID
		result["taskId"] = record.RunID
		result["turnId"] = record.TurnID
		result["sessionId"] = record.SessionID
		result["threadId"] = record.ThreadID
		result["appThreadKey"] = record.ThreadID
		result["openclawSessionKey"] = record.SessionKey
		result["resolvedGatewayProviderId"] = record.GatewayProviderID
		result["startedAt"] = record.StartedAt.UTC().Format(time.RFC3339Nano)
		result["deadlineAt"] = record.DeadlineAt.UTC().Format(time.RFC3339Nano)
	}
	sess.lastResult = cloneMap(result)
	return result
}

func fallbackString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
