package acp

import (
	"testing"
	"time"

	"xworkmate-bridge/internal/shared"
)

func newRunRegistryTestServer(deadline time.Time) (*Server, map[string]any) {
	sess := &session{sessionID: "s1", threadID: "t1"}
	sess.task.RunID = "run-1"
	sess.task.SessionKey = "sk"
	sess.task.GatewayProviderID = "openclaw"
	sess.task.DeadlineAt = deadline
	sess.openClaw = &OpenClawTaskRecord{
		SessionID:         "s1",
		ThreadID:          "t1",
		TurnID:            "turn-1",
		RunID:             "run-1",
		SessionKey:        "sk",
		GatewayProviderID: "openclaw",
		StartedAt:         time.Now().Add(-time.Minute),
		DeadlineAt:        deadline,
	}
	srv := &Server{sessions: map[string]*session{"s1": sess}}
	params := map[string]any{"sessionId": "s1", "runId": "run-1"}
	return srv, params
}

func TestOpenClawTaskGetResultIsTerminal(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"completed", true},
		{"failed", true},
		{"cancelled", true},
		{"interrupted", true},
		{"partially_delivered", true},
		{"running", false},
		{"syncing-artifacts", false},
		{"queued", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := openClawTaskGetResultIsTerminal(map[string]any{"status": tc.status}); got != tc.want {
			t.Errorf("status=%q: got %v, want %v", tc.status, got, tc.want)
		}
	}
}

// T7: gateway 无法确认但 run 仍在预算内 -> 合成 running 句柄续轮询。
func TestGatewayUnconfirmedFallbackWithinBudgetKeepsPolling(t *testing.T) {
	srv, params := newRunRegistryTestServer(time.Now().Add(30 * time.Minute))
	got := srv.openClawTaskGetGatewayUnconfirmedFallback(params, "SOCKET_CLOSED", "socket closed")
	if status := shared.StringArg(got, "status", ""); status != string(TaskStateRunning) {
		t.Fatalf("status = %q, want running", status)
	}
	if !parseBool(got["transportDegraded"]) {
		t.Fatalf("transportDegraded not set: %v", got)
	}
	if shared.StringArg(got, "runId", "") != "run-1" {
		t.Fatalf("runId mismatch: %v", got["runId"])
	}
}

// T9: run 超过 deadline 且 gateway 无法确认 -> 确定性 interrupted 终态。
func TestGatewayUnconfirmedFallbackPastDeadlineInterrupts(t *testing.T) {
	srv, params := newRunRegistryTestServer(time.Now().Add(-time.Minute))
	got := srv.openClawTaskGetGatewayUnconfirmedFallback(params, "SOCKET_CLOSED", "socket closed")
	if status := shared.StringArg(got, "status", ""); status != "interrupted" {
		t.Fatalf("status = %q, want interrupted", status)
	}
	if code := shared.StringArg(got, "code", ""); code != "OPENCLAW_RUN_DEADLINE_EXCEEDED" {
		t.Fatalf("code = %q, want OPENCLAW_RUN_DEADLINE_EXCEEDED", code)
	}
	if parseBool(got["success"]) {
		t.Fatalf("interrupted result must not be success")
	}
	sess := srv.findTaskSession(params)
	if sess == nil || !sess.task.ProgressTerminal || sess.task.State != TaskStateFailed {
		t.Fatalf("session terminal state not recorded: %+v", sess)
	}
}

// T8: 已观察到的终态被缓存，且即使之后 gateway 不可达也优先返回缓存终态。
func TestTerminalResultCachedAndServedAfterGatewayLoss(t *testing.T) {
	srv, params := newRunRegistryTestServer(time.Now().Add(30 * time.Minute))
	terminal := map[string]any{
		"ok":      true,
		"success": true,
		"status":  "completed",
		"runId":   "run-1",
		"message": "done",
	}
	srv.cacheOpenClawTaskGetResultIfTerminal(params, terminal)

	cached, ok := srv.cachedTerminalOpenClawResult(params)
	if !ok {
		t.Fatalf("expected cached terminal result")
	}
	if shared.StringArg(cached, "status", "") != "completed" {
		t.Fatalf("cached status = %q, want completed", cached["status"])
	}

	// 即使 run 已过 deadline + gateway 丢失，也应优先返回缓存终态而非 interrupted。
	sess := srv.findTaskSession(params)
	sess.mu.Lock()
	sess.task.DeadlineAt = time.Now().Add(-time.Hour)
	sess.mu.Unlock()
	got := srv.openClawTaskGetGatewayUnconfirmedFallback(params, "SOCKET_CLOSED", "socket closed")
	if shared.StringArg(got, "status", "") != "completed" {
		t.Fatalf("expected cached completed to win over deadline interrupt, got %v", got["status"])
	}
}

// 同一 session 复用后，旧 run 的终态不得错配给新 runId 的查询。
func TestCachedTerminalNotServedForDifferentRunId(t *testing.T) {
	srv, params := newRunRegistryTestServer(time.Now().Add(30 * time.Minute))
	srv.cacheOpenClawTaskGetResultIfTerminal(params, map[string]any{
		"status": "completed", "success": true, "runId": "run-1",
	})
	// 新一轮查询带不同 runId -> 不应命中旧缓存。
	newParams := map[string]any{"sessionId": "s1", "runId": "run-2"}
	if _, ok := srv.cachedTerminalOpenClawResult(newParams); ok {
		t.Fatalf("stale terminal for run-1 must not be served for run-2")
	}
	// 原 runId 仍应命中。
	if _, ok := srv.cachedTerminalOpenClawResult(params); !ok {
		t.Fatalf("terminal for run-1 should still be served for run-1")
	}
}

// running 结果不应被当作终态缓存。
func TestRunningResultNotCachedAsTerminal(t *testing.T) {
	srv, params := newRunRegistryTestServer(time.Now().Add(30 * time.Minute))
	srv.cacheOpenClawTaskGetResultIfTerminal(params, map[string]any{"status": "running", "runId": "run-1"})
	if _, ok := srv.cachedTerminalOpenClawResult(params); ok {
		t.Fatalf("running result must not be cached as terminal")
	}
}

// 无 per-session 记录时退回旧的 not_found 行为。
func TestGatewayUnconfirmedFallbackWithoutSessionReturnsNotFound(t *testing.T) {
	srv := &Server{sessions: map[string]*session{}}
	got := srv.openClawTaskGetGatewayUnconfirmedFallback(map[string]any{"sessionId": "missing"}, "X", "y")
	if parseBool(got["ok"]) {
		t.Fatalf("expected ok=false not_found, got %v", got)
	}
	if shared.StringArg(got, "status", "") != "not_found" {
		t.Fatalf("status = %q, want not_found", got["status"])
	}
}
