package acp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/shared"
)

// fakeACPAgentUpstream imitates `adapter acp-agent`: it asks for one
// permission per prompt and reports the decision it received.
type fakeACPAgentUpstream struct {
	server      *httptest.Server
	mu          sync.Mutex
	startParams map[string]any
	decisions   []map[string]any
	block       bool
	cancelled   chan struct{}
}

func newFakeACPAgentUpstream(t *testing.T, block bool) *fakeACPAgentUpstream {
	t.Helper()
	upstream := &fakeACPAgentUpstream{block: block, cancelled: make(chan struct{}, 4)}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		var request map[string]any
		if err := conn.ReadJSON(&request); err != nil {
			return
		}
		method, _ := request["method"].(string)
		params := shared.AsMap(request["params"])
		switch method {
		case "session.start", "session.message":
		case "session.cancel":
			upstream.cancelled <- struct{}{}
			_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{"accepted": true, "cancelled": true}})
			return
		default:
			_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{}})
			return
		}
		upstream.mu.Lock()
		upstream.startParams = params
		upstream.mu.Unlock()
		_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": params["sessionId"],
			"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "editing"}},
		}})
		_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": "perm-1", "method": "session/request_permission", "params": map[string]any{
			"sessionId": params["sessionId"],
			"toolCall":  map[string]any{"toolCallId": "t1", "title": "write main.go", "kind": "edit"},
			"options": []any{
				map[string]any{"optionId": "allow-once", "name": "Allow", "kind": "allow_once"},
				map[string]any{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"},
			},
		}})
		var reply map[string]any
		if err := conn.ReadJSON(&reply); err != nil {
			return
		}
		upstream.mu.Lock()
		upstream.decisions = append(upstream.decisions, shared.AsMap(reply["result"]))
		upstream.mu.Unlock()
		if upstream.block {
			_, _, _ = conn.ReadMessage()
			return
		}
		model := shared.StringArg(params, "model", "")
		_ = conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{
			"success":      true,
			"status":       "completed",
			"output":       "done",
			"modelBinding": map[string]any{"requested": model, "bound": model, "verified": true},
		}})
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *fakeACPAgentUpstream) wsURL() string {
	return "ws" + strings.TrimPrefix(u.server.URL, "http")
}

func writeEngineerPolicy(t *testing.T, gatewayURL string) string {
	t.Helper()
	valid := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	fact := func(value any) map[string]any {
		return map[string]any{"value": value, "state": "verified", "source": "test", "valid_until": valid}
	}
	policy := map[string]any{
		"version": "test-engineer-1",
		"enabled": true,
		"budget":  map[string]any{"mode": "gateway_quota"},
		"limits":  map[string]any{"context_reserve_tokens": 1000, "catalog_max_age_seconds": 600},
		"connections": map[string]any{
			"ai-internal": map[string]any{"base_url": gatewayURL + "/v1", "token_env": "ROLE_TEST_GATEWAY_TOKEN", "allowed_data_classes": []string{"unclassified"}},
		},
		"executors": map[string]any{
			"deepseek-harness": map[string]any{
				"kind": "agent", "provider_id": "deepseek-harness", "connection": "ai-internal", "model_option_template": "ai-internal/{model_id}",
				"capabilities": map[string]any{"workspace_edit": fact(true), "test_runner": fact(true), "permission_relay": fact(true)},
			},
		},
		"models": map[string]any{
			"gpt-6.1-sol": map[string]any{"display_name": "GPT 6.1 Sol", "bindings": map[string]any{
				"ai-internal": map[string]any{"gateway_model_id": "gpt-6.1-sol", "capabilities": map[string]any{"tool_calling": fact(true)}, "context_window": fact(400000)},
			}},
		},
		"roles": map[string]any{
			"engineer": map[string]any{
				"enabled": true, "preferences": []string{"gpt-6.1-sol"}, "executors": []string{"deepseek-harness"},
				"required_model_capabilities":    []string{"tool_calling"},
				"required_executor_capabilities": []string{"workspace_edit", "test_runner", "permission_relay"},
				"requires_workspace":             true,
				"product_modes":                  []string{"code"},
			},
		},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "role-policy.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newRoleRoutingTestServer(t *testing.T, upstream *fakeACPAgentUpstream, catalogIDs ...string) *Server {
	t.Helper()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer gateway-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		items := make([]any, 0, len(catalogIDs))
		for _, id := range catalogIDs {
			items = append(items, map[string]any{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(gateway.Close)
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	t.Setenv("DEEPSEEK_HARNESS_RPC_URL", upstream.wsURL())
	t.Setenv("ROLE_TEST_GATEWAY_TOKEN", "gateway-token")
	t.Setenv(rolePolicyPathEnv, writeEngineerPolicy(t, gateway.URL))
	return NewServer()
}

type eventRecorder struct {
	mu     sync.Mutex
	events []map[string]any
	seen   chan map[string]any
}

func newEventRecorder() *eventRecorder {
	return &eventRecorder{seen: make(chan map[string]any, 64)}
}

func (r *eventRecorder) notify(message map[string]any) {
	params := shared.AsMap(message["params"])
	if params["type"] != "task" {
		return
	}
	r.mu.Lock()
	r.events = append(r.events, params)
	r.mu.Unlock()
	r.seen <- params
}

func (r *eventRecorder) waitFor(t *testing.T, event string) map[string]any {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case params := <-r.seen:
			if params["event"] == event {
				return params
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", event)
		}
	}
}

func (r *eventRecorder) phases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	phases := make([]string, 0, len(r.events))
	for _, event := range r.events {
		phases = append(phases, shared.StringArg(event, "event", ""))
	}
	return phases
}

func engineerStartRequest(workspace string) shared.RPCRequest {
	return shared.RPCRequest{ID: "start-1", Method: "session.start", Params: map[string]any{
		"sessionId":        "eng-session",
		"threadId":         "eng-thread",
		"taskPrompt":       "fix the failing test",
		"workingDirectory": workspace,
		"routing":          map[string]any{"role": "engineer", "roleMode": "auto"},
	}}
}

func TestEngineerLoopRelaysPermissionAndReportsBinding(t *testing.T) {
	upstream := newFakeACPAgentUpstream(t, false)
	server := newRoleRoutingTestServer(t, upstream, "gpt-6.1-sol")
	recorder := newEventRecorder()

	type outcome struct {
		result map[string]any
		err    *shared.RPCError
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := server.handleRequest(engineerStartRequest(t.TempDir()), recorder.notify)
		done <- outcome{result, err}
	}()

	requested := recorder.waitFor(t, "task.permission_requested")
	detail := shared.AsMap(shared.AsMap(requested["task"])["detail"])
	requestID := shared.StringArg(detail, "requestId", "")
	if requestID == "" || shared.AsMap(detail["toolCall"])["title"] != "write main.go" {
		t.Fatalf("permission event lacks request details: %#v", requested)
	}
	pending, rpcErr := server.handleRequest(shared.RPCRequest{Method: "xworkmate.permissions.list", Params: map[string]any{"sessionId": "eng-session"}}, nil)
	if rpcErr != nil || len(anyList(pending["items"])) != 1 {
		t.Fatalf("expected one pending permission, got %#v %#v", pending, rpcErr)
	}
	if _, rpcErr := server.handleRequest(shared.RPCRequest{Method: "xworkmate.permissions.respond", Params: map[string]any{
		"sessionId": "eng-session", "requestId": requestID, "optionId": "allow-always",
	}}, nil); rpcErr == nil {
		t.Fatalf("an option the upstream did not offer must be refused")
	}
	if _, rpcErr := server.handleRequest(shared.RPCRequest{Method: "xworkmate.permissions.respond", Params: map[string]any{
		"sessionId": "other-session", "requestId": requestID, "optionId": "allow-once",
	}}, nil); rpcErr == nil {
		t.Fatalf("a different session must not answer this prompt")
	}
	if _, rpcErr := server.handleRequest(shared.RPCRequest{Method: "xworkmate.permissions.respond", Params: map[string]any{
		"sessionId": "eng-session", "requestId": requestID, "optionId": "allow-once",
	}}, nil); rpcErr != nil {
		t.Fatalf("respond: %#v", rpcErr)
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("engineer task did not finish")
	}
	if got.err != nil {
		t.Fatalf("unexpected rpc error: %#v", got.err)
	}
	result := got.result
	if result["resolvedRole"] != "engineer" || result["resolvedProviderId"] != "deepseek-harness" || result["resolvedModelId"] != "gpt-6.1-sol" {
		t.Fatalf("missing role/model resolution: %#v", result)
	}
	if result["modelBindingVerified"] != true || result["success"] != true {
		t.Fatalf("expected verified successful run: %#v", result)
	}
	upstream.mu.Lock()
	sentModel := shared.StringArg(upstream.startParams, "model", "")
	decisions := append([]map[string]any(nil), upstream.decisions...)
	upstream.mu.Unlock()
	if sentModel != "ai-internal/gpt-6.1-sol" {
		t.Fatalf("executor received model %q", sentModel)
	}
	if len(decisions) != 1 || shared.AsMap(decisions[0]["outcome"])["optionId"] != "allow-once" {
		t.Fatalf("upstream did not receive the user's decision: %#v", decisions)
	}
	wantPhases := []string{"task.selected", "task.started", "task.permission_requested", "task.permission_resolved", "task.completed"}
	if got := recorder.phases(); strings.Join(got, ",") != strings.Join(wantPhases, ",") {
		t.Fatalf("unexpected task phases %v", got)
	}

	snapshot, rpcErr := server.handleRequest(shared.RPCRequest{Method: "xworkmate.tasks.get", Params: map[string]any{"sessionId": "eng-session"}}, nil)
	if rpcErr != nil {
		t.Fatalf("tasks.get: %#v", rpcErr)
	}
	if snapshot["status"] != "completed" || snapshot["resolvedRole"] != "engineer" || len(anyList(snapshot["taskEvents"])) != len(wantPhases) {
		t.Fatalf("recovery snapshot incomplete: %#v", snapshot)
	}
}

func TestEngineerLoopCancelDeniesPendingPermissionAndStopsRun(t *testing.T) {
	upstream := newFakeACPAgentUpstream(t, true)
	server := newRoleRoutingTestServer(t, upstream, "gpt-6.1-sol")
	recorder := newEventRecorder()
	done := make(chan map[string]any, 1)
	go func() {
		result, err := server.handleRequest(engineerStartRequest(t.TempDir()), recorder.notify)
		if err != nil {
			t.Errorf("unexpected rpc error: %#v", err)
		}
		done <- result
	}()
	recorder.waitFor(t, "task.permission_requested")

	cancelResult, rpcErr := server.handleRequest(shared.RPCRequest{Method: "session.cancel", Params: map[string]any{"sessionId": "eng-session"}}, nil)
	if rpcErr != nil || cancelResult["runCancelled"] != true {
		t.Fatalf("expected live run cancel, got %#v %#v", cancelResult, rpcErr)
	}
	select {
	case result := <-done:
		if result["status"] != "cancelled" || result["success"] != false {
			t.Fatalf("expected cancelled result, got %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the run")
	}
	select {
	case <-upstream.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive session.cancel")
	}
	upstream.mu.Lock()
	decisions := append([]map[string]any(nil), upstream.decisions...)
	upstream.mu.Unlock()
	// Cancel either delivers an explicit "cancelled" outcome or closes the
	// socket first (the adapter then answers the agent with "cancelled");
	// it must never deliver an allow.
	for _, decision := range decisions {
		if shared.AsMap(decision["outcome"])["outcome"] != "cancelled" {
			t.Fatalf("pending permission must be denied on cancel: %#v", decisions)
		}
	}
	resolved := shared.AsMap(shared.AsMap(findEvent(recorder, "task.permission_resolved")["task"])["detail"])
	if resolved["reason"] != "run_cancelled" && resolved["reason"] != "user" {
		t.Fatalf("unexpected resolution reason: %#v", resolved)
	}
	if phases := recorder.phases(); phases[len(phases)-1] != "task.cancelled" {
		t.Fatalf("last phase should be cancelled: %v", phases)
	}
}

func TestEngineerLoopRejectsModelMissingFromLiveCatalog(t *testing.T) {
	upstream := newFakeACPAgentUpstream(t, false)
	server := newRoleRoutingTestServer(t, upstream, "some-other-model")
	recorder := newEventRecorder()
	result, rpcErr := server.handleRequest(engineerStartRequest(t.TempDir()), recorder.notify)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %#v", rpcErr)
	}
	if result["unavailableCode"] != unavailableRoleSelectionRejected {
		t.Fatalf("expected rejection, got %#v", result)
	}
	selection := shared.AsMap(result["roleSelection"])
	if selection["code"] != "no_eligible_candidate" {
		t.Fatalf("expected no_eligible_candidate, got %#v", selection)
	}
	upstream.mu.Lock()
	dispatched := upstream.startParams != nil
	upstream.mu.Unlock()
	if dispatched {
		t.Fatal("a rejected selection must not dispatch")
	}
	if phases := recorder.phases(); len(phases) != 1 || phases[0] != "task.rejected" {
		t.Fatalf("expected one rejected event, got %v", phases)
	}
}

func TestEngineerLoopRequiresWorkspace(t *testing.T) {
	upstream := newFakeACPAgentUpstream(t, false)
	server := newRoleRoutingTestServer(t, upstream, "gpt-6.1-sol")
	request := engineerStartRequest("")
	result, rpcErr := server.handleRequest(request, nil)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %#v", rpcErr)
	}
	if shared.AsMap(result["roleSelection"])["code"] != "workspace_unauthorized" {
		t.Fatalf("expected workspace rejection, got %#v", result)
	}
}

func TestCapabilitiesAdvertiseRoleRouting(t *testing.T) {
	upstream := newFakeACPAgentUpstream(t, false)
	server := newRoleRoutingTestServer(t, upstream, "gpt-6.1-sol")
	capabilities, rpcErr := server.handleRequest(shared.RPCRequest{Method: "acp.capabilities"}, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	routing := shared.AsMap(capabilities["roleRouting"])
	if routing["enabled"] != true || routing["policyVersion"] != "test-engineer-1" {
		t.Fatalf("unexpected role routing capabilities: %#v", routing)
	}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "gateway-token") {
		t.Fatal("capabilities must not leak gateway credentials")
	}
}

func TestRoleRoutingWithoutPolicyIsExplicit(t *testing.T) {
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	t.Setenv(rolePolicyPathEnv, "")
	server := NewServer()
	result, rpcErr := server.handleRequest(engineerStartRequest(t.TempDir()), nil)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %#v", rpcErr)
	}
	if result["unavailableCode"] != unavailableRolePolicyUnconfigured {
		t.Fatalf("expected ROLE_POLICY_UNCONFIGURED, got %#v", result)
	}
}

func findEvent(recorder *eventRecorder, name string) map[string]any {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	for _, event := range recorder.events {
		if event["event"] == name {
			return event
		}
	}
	return nil
}

func TestEngineerLoopRejectsRoleOutsideProductMode(t *testing.T) {
	upstream := newFakeACPAgentUpstream(t, false)
	server := newRoleRoutingTestServer(t, upstream, "gpt-6.1-sol")
	request := engineerStartRequest(t.TempDir())
	request.Params["metadata"] = map[string]any{
		"xworkmateProductCapability": map[string]any{"schemaVersion": 1, "mode": "work"},
	}
	result, rpcErr := server.handleRequest(request, nil)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %#v", rpcErr)
	}
	if shared.AsMap(result["roleSelection"])["code"] != "role_not_allowed_for_mode" {
		t.Fatalf("expected role_not_allowed_for_mode, got %#v", result)
	}
	upstream.mu.Lock()
	dispatched := upstream.startParams != nil
	upstream.mu.Unlock()
	if dispatched {
		t.Fatalf("a rejected role turn must not reach the executor")
	}
}
