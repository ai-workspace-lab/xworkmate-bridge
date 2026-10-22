package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"xworkmate-bridge/internal/shared"
)

func TestDSHAdapterProbe(t *testing.T) {
	// 1. Healthy server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	adapter := newDSHAdapter(syncedProvider{
		ProviderID: "dsh",
		Label:      "DeepSeek Harness",
		Endpoint:   server.URL,
		Enabled:    true,
	})

	probe := adapter.Probe(context.Background())
	if !probe.Available {
		t.Fatalf("expected available probe, got %#v", probe)
	}

	// 2. Unhealthy server
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer badServer.Close()

	badAdapter := newDSHAdapter(syncedProvider{
		ProviderID: "dsh",
		Label:      "DeepSeek Harness",
		Endpoint:   badServer.URL,
		Enabled:    true,
	})

	badProbe := badAdapter.Probe(context.Background())
	if badProbe.Available {
		t.Fatalf("expected unavailable probe for 503, got %#v", badProbe)
	}
}

func TestDSHAdapterStartSessionAndSendMessage(t *testing.T) {
	var receivedMethods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method := shared.StringArg(req, "method", "")
		receivedMethods = append(receivedMethods, method)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result": map[string]any{
				"ok":      true,
				"output":  "DSH harness completed task: " + method,
				"status":  "completed",
				"success": true,
			},
		})
	}))
	defer server.Close()

	adapter := newDSHAdapter(syncedProvider{
		ProviderID: "dsh",
		Label:      "DeepSeek Harness",
		Endpoint:   server.URL,
		Enabled:    true,
	})

	var sinkEvents []map[string]any
	sink := func(update map[string]any) {
		sinkEvents = append(sinkEvents, update)
	}

	// Start Session
	res, err := adapter.StartSession(context.Background(), "sess-1", "th-1", map[string]any{
		"taskPrompt":       "Analyze repository architecture",
		"workingDirectory": "/workspace",
	}, sink)
	if err != nil {
		t.Fatalf("StartSession error: %v", err)
	}
	if !parseBool(res["success"]) {
		t.Fatalf("expected success true, got %#v", res)
	}
	if len(sinkEvents) < 2 {
		t.Fatalf("expected at least 2 sink events (running, completed), got %d", len(sinkEvents))
	}

	// Send Message
	msgRes, err := adapter.SendMessage(context.Background(), "sess-1", "th-1", map[string]any{
		"taskPrompt": "Continue next step",
	}, sink)
	if err != nil {
		t.Fatalf("SendMessage error: %v", err)
	}
	if !parseBool(msgRes["success"]) {
		t.Fatalf("expected success true, got %#v", msgRes)
	}

	// Cancel Session
	if err := adapter.CancelSession(context.Background(), "sess-1"); err != nil {
		t.Fatalf("CancelSession error: %v", err)
	}

	if len(receivedMethods) < 3 {
		t.Fatalf("expected at least 3 RPC dispatches, got %v", receivedMethods)
	}
}

func TestDSBotAdapterStartSessionAndSendMessage(t *testing.T) {
	var receivedRoles []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		params := shared.AsMap(req["params"])
		role := shared.StringArg(params, "role", "")
		receivedRoles = append(receivedRoles, role)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result": map[string]any{
				"ok":      true,
				"role":    role,
				"output":  "DS-Bot planned workflow for role: " + role,
				"status":  "completed",
				"success": true,
			},
		})
	}))
	defer server.Close()

	adapter := newDSBotAdapter(syncedProvider{
		ProviderID: "dsbot",
		Label:      "DeepSeek Bot",
		Endpoint:   server.URL,
		Enabled:    true,
	})

	var sinkEvents []map[string]any
	sink := func(update map[string]any) {
		sinkEvents = append(sinkEvents, update)
	}

	res, err := adapter.StartSession(context.Background(), "sess-bot-1", "th-bot-1", map[string]any{
		"taskPrompt": "Decompose multi-agent tasks",
		"role":       "planner",
	}, sink)
	if err != nil {
		t.Fatalf("StartSession error: %v", err)
	}
	if !parseBool(res["success"]) {
		t.Fatalf("expected success, got %#v", res)
	}

	// Check planning event
	foundPlanning := false
	for _, ev := range sinkEvents {
		if ev["event"] == "planning" && ev["role"] == "planner" {
			foundPlanning = true
			break
		}
	}
	if !foundPlanning {
		t.Fatalf("expected planning sink event with role planner, got %v", sinkEvents)
	}

	// Send approval message
	approvalRes, err := adapter.SendMessage(context.Background(), "sess-bot-1", "th-bot-1", map[string]any{
		"approval": map[string]any{
			"decision": "approved",
		},
	}, sink)
	if err != nil {
		t.Fatalf("SendMessage error: %v", err)
	}
	if !parseBool(approvalRes["success"]) {
		t.Fatalf("expected success, got %#v", approvalRes)
	}
}

func TestDSHRoutingAndSessionExecution(t *testing.T) {
	t.Setenv("BRIDGE_ROUTER_TYPE", "dsh")

	server := NewServer()

	// 1. Test routing resolution defaults to dsh
	resolveRes, err := server.handleRequest(shared.RPCRequest{
		Method: "xworkmate.routing.resolve",
		Params: map[string]any{
			"taskPrompt":       "Build backend service",
			"workingDirectory": "/workspace",
			"routing": map[string]any{
				"routingMode": "explicit",
				"explicitExecutionTarget": "gateway",
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("routing.resolve error: %v", err)
	}
	if resolveRes["resolvedGatewayProviderId"] != "dsh" {
		t.Fatalf("expected resolvedGatewayProviderId to be dsh, got %v", resolveRes["resolvedGatewayProviderId"])
	}

	// 2. Test session.prepare returns ready without OpenClaw
	prepareRes, rpcErr := server.handleSessionPrepare(context.Background(), map[string]any{
		"gatewayProviderId": "dsh",
	}, nil)
	if rpcErr != nil {
		t.Fatalf("session.prepare rpcErr: %v", rpcErr)
	}
	if !parseBool(prepareRes["ready"]) {
		t.Fatalf("expected session.prepare ready true, got %#v", prepareRes)
	}

	// 3. Test session.start with dsh
	var sessionEvents []map[string]any
	notify := func(notification map[string]any) {
		sessionEvents = append(sessionEvents, notification)
	}
	startRes, err := server.handleRequest(shared.RPCRequest{
		Method: "session.start",
		Params: map[string]any{
			"sessionId":        "sess-dsh-run-1",
			"threadId":         "thread-dsh-run-1",
			"taskPrompt":       "Run unit tests via DSH harness",
			"workingDirectory": "/workspace",
			"routing": map[string]any{
				"routingMode": "explicit",
				"explicitExecutionTarget": "gateway",
				"preferredGatewayProviderId": "dsh",
			},
		},
	}, notify)
	if err != nil {
		t.Fatalf("session.start error: %v", err)
	}
	if startRes == nil {
		t.Fatal("expected non-nil startRes")
	}

	// 4. Test tasks.get returns completed state without OpenClaw
	taskGetRes := server.handleTaskGet(context.Background(), map[string]any{
		"sessionId":         "sess-dsh-run-1",
		"gatewayProviderId": "dsh",
	}, nil)
	if taskGetRes["status"] != "completed" {
		t.Fatalf("expected status completed, got %v", taskGetRes["status"])
	}

	// 5. Test tasks.cancel accepts cancellation without OpenClaw
	taskCancelRes := server.handleTaskCancel(context.Background(), map[string]any{
		"sessionId":         "sess-dsh-run-1",
		"runId":             "run-123",
		"gatewayProviderId": "dsh",
	}, nil)
	if !parseBool(taskCancelRes["accepted"]) {
		t.Fatalf("expected cancel accepted true, got %#v", taskCancelRes)
	}
}
