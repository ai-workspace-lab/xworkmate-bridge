package acp

import (
	"testing"

	"xworkmate-bridge/internal/shared"
)

// AI Workspace v2.1: role selection and dispatch live in the DSH orchestrator
// with Accounts as task state. Bridge rejects ACP turns that still ask for a
// role instead of selecting, and keeps no role task snapshot.
func TestRoleRoutedACPTurnsAreRejectedAsMoved(t *testing.T) {
	server := NewServer()
	for _, routing := range []map[string]any{
		{"role": "engineer"},
		{"roleMode": "auto"},
		{"role": "engineer", "roleMode": "manual", "roleModel": "gpt-6.1-sol"},
	} {
		result, rpcErr := server.handleRequest(shared.RPCRequest{
			ID:     "role-turn",
			Method: "session.start",
			Params: map[string]any{"sessionId": "s-role", "threadId": "s-role", "taskPrompt": "fix it", "routing": routing},
		}, func(map[string]any) {})
		if rpcErr != nil {
			t.Fatalf("routing %v: unexpected rpc error %v", routing, rpcErr)
		}
		if result["unavailableCode"] != unavailableRoleRoutingMoved {
			t.Fatalf("routing %v: want %s, got %v", routing, unavailableRoleRoutingMoved, result)
		}
		if _, ok := result["roleSelection"]; ok {
			t.Fatalf("routing %v: Bridge must not report a role selection: %v", routing, result)
		}
	}
	resolved, rpcErr := server.handleRequest(shared.RPCRequest{ID: "resolve", Method: "xworkmate.routing.resolve", Params: map[string]any{"routing": map[string]any{"role": "engineer"}}}, nil)
	if rpcErr != nil || resolved["unavailableCode"] != unavailableRoleRoutingMoved {
		t.Fatalf("routing.resolve: %v %v", resolved, rpcErr)
	}
}

func TestCapabilitiesNoLongerAdvertiseRoleRoutingOrPermissionRelay(t *testing.T) {
	server := NewServer()
	capabilities, rpcErr := server.handleRequest(shared.RPCRequest{ID: "caps", Method: "acp.capabilities"}, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	for _, key := range []string{"roleRouting", "permissionRelay"} {
		if _, ok := capabilities[key]; ok {
			t.Fatalf("capabilities still advertise %s", key)
		}
	}
	for _, method := range []string{"xworkmate.permissions.respond", "xworkmate.permissions.list"} {
		if _, rpcErr := server.handleRequest(shared.RPCRequest{ID: method, Method: method, Params: map[string]any{"sessionId": "s"}}, nil); rpcErr == nil {
			t.Fatalf("%s must no longer be served", method)
		}
	}
}
