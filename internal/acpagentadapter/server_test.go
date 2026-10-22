package acpagentadapter

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testToken = "adapter-test-token"

func TestMain(m *testing.M) {
	if os.Getenv("ACP_FAKE_AGENT") == "1" {
		runFakeAgent()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runFakeAgent is a minimal ACP v1 agent used as a child process.
func runFakeAgent() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	current := "gw/default"
	configOptions := func() []any {
		return []any{map[string]any{
			"id": "model", "type": "select", "currentValue": current,
			"options": []any{
				map[string]any{"value": "gw/default", "name": "default"},
				map[string]any{"value": "gw/liar", "name": "liar"},
				map[string]any{"group": "gw", "options": []any{map[string]any{"value": "gw/gpt-6.1-sol", "name": "sol"}}},
			},
		}}
	}
	var pendingPromptID any
	for in.Scan() {
		var msg map[string]any
		if err := json.Unmarshal(in.Bytes(), &msg); err != nil {
			continue
		}
		method, _ := msg["method"].(string)
		params, _ := msg["params"].(map[string]any)
		id := msg["id"]
		respond := func(result map[string]any) {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		}
		update := func(text string) {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": "acp-1",
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}},
			}})
		}
		switch method {
		case "initialize":
			respond(map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "fake"}})
		case "session/new":
			current = "gw/default"
			respond(map[string]any{"sessionId": "acp-1", "configOptions": configOptions()})
		case "session/set_config_option":
			if value, _ := params["value"].(string); value != "gw/liar" {
				current = value
			}
			respond(map[string]any{"configOptions": configOptions()})
		case "session/prompt":
			prompt, _ := params["prompt"].([]any)
			first, _ := prompt[0].(map[string]any)
			text, _ := first["text"].(string)
			pendingPromptID = id
			switch text {
			case "edit":
				update("working with " + current + ";")
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": "perm-1", "method": "session/request_permission", "params": map[string]any{
					"sessionId": "acp-1",
					"toolCall":  map[string]any{"toolCallId": "t1", "title": "write main.go", "kind": "edit"},
					"options": []any{
						map[string]any{"optionId": "allow-once", "name": "Allow", "kind": "allow_once"},
						map[string]any{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"},
					},
				}})
			case "wait":
				update("waiting;")
			default:
				respond(map[string]any{"stopReason": "end_turn"})
				pendingPromptID = nil
			}
		case "session/cancel":
			if pendingPromptID != nil {
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": pendingPromptID, "result": map[string]any{"stopReason": "cancelled"}})
				pendingPromptID = nil
			}
		case "session/close":
			respond(map[string]any{})
		case "":
			if id == "perm-1" {
				outcome := "cancelled"
				if result, ok := msg["result"].(map[string]any); ok {
					inner, _ := result["outcome"].(map[string]any)
					if optionID, _ := inner["optionId"].(string); optionID != "" {
						outcome = optionID
					} else if kind, _ := inner["outcome"].(string); kind != "" {
						outcome = kind
					}
				}
				update("outcome=" + outcome)
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": pendingPromptID, "result": map[string]any{"stopReason": "end_turn"}})
				pendingPromptID = nil
			}
		}
	}
}

func newTestAdapter(t *testing.T) (*httptest.Server, *stdioAgent) {
	t.Helper()
	t.Setenv("ACP_FAKE_AGENT", "1")
	agent := newStdioAgent(os.Args[0], nil, "")
	server := NewServer(agent, Options{ProviderID: "deepseek-harness", AuthToken: testToken, PermissionTimeout: 5 * time.Second})
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleWebSocket))
	t.Cleanup(func() {
		httpServer.Close()
		if err := agent.close(); err != nil {
			t.Errorf("close agent: %v", err)
		}
	})
	return httpServer, agent
}

func dial(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	header := http.Header{"Authorization": []string{"Bearer " + testToken}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// roundTrip sends one request and answers permission prompts with reply,
// returning the final result and the permission prompts seen.
func roundTrip(t *testing.T, conn *websocket.Conn, method string, params map[string]any, reply map[string]any) (map[string]any, []map[string]any) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": "req-1", "method": method, "params": params}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var prompts []map[string]any
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		switch msg["method"] {
		case "session/request_permission":
			params, _ := msg["params"].(map[string]any)
			prompts = append(prompts, params)
			if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": reply}); err != nil {
				t.Fatalf("write permission reply: %v", err)
			}
			continue
		case "session/update":
			params, _ := msg["params"].(map[string]any)
			if update, _ := params["update"].(map[string]any); update["sessionUpdate"] == "agent_message_chunk" {
				if params["type"] != "delta" || params["delta"] == "" || params["threadId"] != "thread-1" {
					t.Fatalf("text chunks must be forwarded as deltas for the thread: %+v", params)
				}
			}
			continue
		}
		if msg["id"] == "req-1" {
			result, _ := msg["result"].(map[string]any)
			return result, prompts
		}
	}
}

func startParams(t *testing.T, prompt, model string) map[string]any {
	return map[string]any{"sessionId": "bridge-1", "threadId": "thread-1", "taskPrompt": prompt, "model": model, "workingDirectory": t.TempDir()}
}

func TestPermissionIsRelayedAndBridgeDecisionApplied(t *testing.T) {
	server, _ := newTestAdapter(t)
	conn := dial(t, server)
	result, prompts := roundTrip(t, conn, "session.start", startParams(t, "edit", "gw/gpt-6.1-sol"),
		map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"}})
	if len(prompts) != 1 || prompts[0]["sessionId"] != "bridge-1" {
		t.Fatalf("expected one relayed prompt for bridge session, got %+v", prompts)
	}
	if result["success"] != true || !strings.Contains(result["output"].(string), "outcome=allow-once") {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !strings.Contains(result["output"].(string), "working with gw/gpt-6.1-sol") {
		t.Fatalf("prompt did not run on the bound model: %+v", result)
	}
	binding, _ := result["modelBinding"].(map[string]any)
	if binding["verified"] != true || binding["bound"] != "gw/gpt-6.1-sol" {
		t.Fatalf("expected verified binding, got %+v", binding)
	}
}

func TestLegacyAutoApproveShapeIsDenied(t *testing.T) {
	server, _ := newTestAdapter(t)
	conn := dial(t, server)
	result, _ := roundTrip(t, conn, "session.start", startParams(t, "edit", ""),
		map[string]any{"approved": true, "decision": "approved", "behavior": "allow"})
	if !strings.Contains(result["output"].(string), "outcome=cancelled") {
		t.Fatalf("legacy auto-approve must fail closed, got %+v", result)
	}
	binding, _ := result["modelBinding"].(map[string]any)
	if binding["verified"] != false {
		t.Fatalf("no requested model must not be reported verified: %+v", binding)
	}
}

func TestSelectedOptionMustBeOffered(t *testing.T) {
	server, _ := newTestAdapter(t)
	conn := dial(t, server)
	result, _ := roundTrip(t, conn, "session.start", startParams(t, "edit", ""),
		map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-always"}})
	if !strings.Contains(result["output"].(string), "outcome=cancelled") {
		t.Fatalf("unknown option must fail closed, got %+v", result)
	}
}

func TestModelMustBeAdvertisedAndVerified(t *testing.T) {
	server, _ := newTestAdapter(t)
	conn := dial(t, server)
	result, prompts := roundTrip(t, conn, "session.start", startParams(t, "edit", "gw/claude-opus-5"), nil)
	if result["code"] != CodeModelNotAdvertised || len(prompts) != 0 {
		t.Fatalf("expected MODEL_NOT_ADVERTISED before any prompt, got %+v", result)
	}
	result, _ = roundTrip(t, conn, "session.start", startParams(t, "edit", "gw/liar"), nil)
	if result["code"] != CodeModelBindingUnverified {
		t.Fatalf("expected MODEL_BINDING_UNVERIFIED, got %+v", result)
	}
}

func TestCancelStopsLivePrompt(t *testing.T) {
	server, _ := newTestAdapter(t)
	runner := dial(t, server)
	done := make(chan map[string]any, 1)
	go func() {
		result, _ := roundTrip(t, runner, "session.start", startParams(t, "wait", ""), nil)
		done <- result
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		canceller := dial(t, server)
		result, _ := roundTrip(t, canceller, "session.cancel", map[string]any{"sessionId": "bridge-1"}, nil)
		if result["cancelled"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never became cancellable: %+v", result)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case result := <-done:
		if result["status"] != "cancelled" || result["success"] != false {
			t.Fatalf("expected cancelled result, got %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not end after cancel")
	}
}

func TestWorkspaceAndContinuationGuards(t *testing.T) {
	server, _ := newTestAdapter(t)
	conn := dial(t, server)
	result, _ := roundTrip(t, conn, "session.start", map[string]any{"sessionId": "bridge-1", "taskPrompt": "edit", "workingDirectory": "relative/dir"}, nil)
	if result["code"] != CodeWorkspaceInvalid {
		t.Fatalf("expected WORKSPACE_INVALID, got %+v", result)
	}
	result, _ = roundTrip(t, conn, "session.message", map[string]any{"sessionId": "bridge-2", "taskPrompt": "edit"}, nil)
	if result["code"] != CodeSessionUnavailable {
		t.Fatalf("expected SESSION_CONTINUATION_UNAVAILABLE, got %+v", result)
	}
}

func TestRejectsMissingAuthorization(t *testing.T) {
	server, _ := newTestAdapter(t)
	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err == nil {
		t.Fatal("expected dial failure without token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %+v", resp)
	}
}
