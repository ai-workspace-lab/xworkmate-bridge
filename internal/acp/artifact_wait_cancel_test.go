package acp

import (
	"context"
	"testing"
	"time"
	"xworkmate-bridge/internal/shared"
)

func artifactWaitFixture() map[string]any {
	return map[string]any{"status": "running", "taskStatus": "completed", "success": true, "terminal": true, "terminalSource": "agent_end", "pending": true, "artifactSyncStatus": "syncing", "runId": "run-1", "openclawSessionKey": "sk"}
}
func TestArtifactWaitCancelConfirmedScopedAndStable(t *testing.T) {
	s, p := newRunRegistryTestServer(time.Now().Add(time.Minute))
	s.cacheOpenClawTaskGetResultIfTerminal(p, artifactWaitFixture())
	r := s.handleTaskCancel(context.Background(), p, nil)
	if !parseBool(r["cancelled"]) || shared.StringArg(r, "cancellationScope", "") != "artifact-wait" {
		t.Fatalf("not confirmed artifact wait: %v", r)
	}
	got, ok := s.cachedTerminalOpenClawResult(p)
	if !ok || got["status"] != "cancelled" || parseBool(got["success"]) || parseBool(got["pending"]) {
		t.Fatalf("get still pending: %v", got)
	}
	if _, exists := got["workerStopped"]; exists {
		t.Fatal("cannot claim worker stopped")
	}
	s.cacheOpenClawTaskGetResultIfTerminal(p, map[string]any{"status": "completed", "runId": "run-1", "success": true})
	again := s.handleTaskCancel(context.Background(), p, nil)
	if !parseBool(again["cancelled"]) {
		t.Fatalf("duplicate cancellation unstable: %v", again)
	}
	got, _ = s.cachedTerminalOpenClawResult(p)
	if got["status"] != "cancelled" {
		t.Fatalf("stale native completed overwrote cancellation: %v", got)
	}
	if _, ok = s.cachedTerminalOpenClawResult(map[string]any{"sessionId": "s1", "runId": "run-2"}); ok {
		t.Fatal("wrong run got cancellation")
	}
}
func TestArtifactWaitCancelRequiresNativeTerminalScope(t *testing.T) {
	for _, kind := range []string{"wrong-run", "wrong-session", "failed", "active", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			s, p := newRunRegistryTestServer(time.Now().Add(time.Minute))
			v := artifactWaitFixture()
			switch kind {
			case "wrong-run":
				v["runId"] = "run-2"
			case "wrong-session":
				v["openclawSessionKey"] = "other"
			case "failed":
				v["taskStatus"] = "failed"
				v["success"] = false
			case "active":
				v["taskStatus"] = "running"
				v["terminal"] = false
			case "unknown":
				delete(v, "terminalSource")
			}
			s.cacheOpenClawTaskGetResultIfTerminal(p, v)
			r := s.handleTaskCancel(context.Background(), p, nil)
			if parseBool(r["cancelled"]) {
				t.Fatalf("unproven cancellation: %v", r)
			}
			if _, ok := s.cachedTerminalOpenClawResult(p); ok {
				t.Fatal("unproven terminal cached")
			}
		})
	}
}

func TestArtifactWaitCancelDoesNotRewriteNativeFailedOrWrongRequestedScope(t *testing.T) {
	s, p := newRunRegistryTestServer(time.Now().Add(time.Minute))
	s.cacheOpenClawTaskGetResultIfTerminal(p, map[string]any{"status": "failed", "success": false, "runId": "run-1", "error": "UPSTREAM_MODEL_REQUEST_FAILED (HTTP 403)"})
	r := s.handleTaskCancel(context.Background(), p, nil)
	if parseBool(r["cancelled"]) {
		t.Fatalf("native failure rewritten: %v", r)
	}
	got, _ := s.cachedTerminalOpenClawResult(p)
	if got["status"] != "failed" {
		t.Fatalf("native failure lost: %v", got)
	}
	for _, bad := range []map[string]any{{"sessionId": "s1", "runId": "run-2"}, {"sessionId": "s1", "runId": "run-1", "openclawSessionKey": "other"}} {
		srv, good := newRunRegistryTestServer(time.Now().Add(time.Minute))
		srv.cacheOpenClawTaskGetResultIfTerminal(bad, artifactWaitFixture())
		r = srv.handleTaskCancel(context.Background(), bad, nil)
		if parseBool(r["accepted"]) {
			t.Fatalf("wrong requested scope accepted: %v", r)
		}
		if _, ok := srv.cachedTerminalOpenClawResult(good); ok {
			t.Fatal("wrong scope cached a terminal")
		}
	}
}
