package acp

import (
	"strings"
	"testing"
	"xworkmate-bridge/internal/shared"
)

func TestProductCapabilityRejectsInvalidContract(t *testing.T) {
	for _, capability := range []any{
		"code", map[string]any{"schemaVersion": 2, "mode": "code"},
		map[string]any{"schemaVersion": 1, "mode": "bot"},
		map[string]any{"schemaVersion": 1, "mode": "work", "model": 3},
		map[string]any{"schemaVersion": 1, "mode": "code", "command": "sh"},
		map[string]any{"schemaVersion": 1.5, "mode": "code"},
		map[string]any{"schemaVersion": 1, "mode": "chat", "model": "vendor/direct"},
	} {
		_, err := openClawProductCapability(map[string]any{"metadata": map[string]any{"xworkmateProductCapability": capability}})
		if err == nil {
			t.Fatalf("invalid contract accepted: %#v", capability)
		}
	}
}
func TestProductCapabilityForwarding(t *testing.T) {
	for _, mode := range []string{"chat", "work", "code"} {
		params := map[string]any{"sessionId": "app-thread", "taskPrompt": "do the task", "metadata": map[string]any{"xworkmateProductCapability": map[string]any{"schemaVersion": 1, "mode": mode, "model": "xworkmate/allowed-model"}}}
		capability, err := openClawProductCapability(params)
		if err != nil {
			t.Fatal(err)
		}
		if capability["mode"] != mode {
			t.Fatal(capability)
		}
		prepared := openClawSessionPrepareParams(params, "agent:main:app-thread", "run-1", openClawArtifactContract{})
		got, ok := prepared["productCapability"].(map[string]any)
		if !ok || got["mode"] != mode || got["model"] != "xworkmate/allowed-model" {
			t.Fatalf("lost product metadata: %#v", prepared)
		}
		chat, rpcErr := openClawChatSendParamsWithSessionKey(params, "run-1", "agent:main:app-thread")
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		if _, ok := chat["productCapability"]; ok {
			t.Fatal("custom fields must not leak into native chat.send")
		}
		receipt, _ := chat["systemProvenanceReceipt"].(string)
		if mode != "chat" && !strings.Contains(receipt, "xworkmate_worker") {
			t.Fatalf("worker guidance missing for %s", mode)
		}
	}
}

func TestProductModelUsesMappedGatewaySessionBeforeSend(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "rejected"}[reject], func(t *testing.T) {
			gateway := newAcpFakeOpenClawGateway(t)
			defer gateway.Close()
			gateway.rejectModelPatch.Store(reject)
			t.Setenv("GATEWAY_RPC_URL", gateway.URL())
			t.Setenv("BRIDGE_AUTH_TOKEN", "fixture-bridge")
			server := NewServer()
			_, err := server.executeSessionTask(task{req: shared.RPCRequest{Method: "session.start", Params: map[string]any{
				"sessionId": "product-thread", "threadId": "product-thread", "taskPrompt": "say pong",
				"metadata": map[string]any{"xworkmateProductCapability": map[string]any{"schemaVersion": 1, "mode": "chat", "model": "xworkmate/allowed-model"}},
				"routing":  map[string]any{"routingMode": "explicit", "explicitExecutionTarget": "gateway", "preferredGatewayProviderId": "openclaw"},
			}}})
			patch, ok := gateway.lastModelPatchParams.Load().(map[string]any)
			if !ok || patch["key"] != "agent:main:product-thread" || patch["model"] != "xworkmate/allowed-model" {
				t.Fatalf("model not patched against mapped session: %#v", patch)
			}
			if reject {
				if err == nil || gateway.ChatSendCount() != 0 {
					t.Fatal("rejected model must prevent execution")
				}
			} else {
				if err != nil || gateway.ChatSendCount() != 1 {
					t.Fatalf("valid model did not execute: %v", err)
				}
			}
			methods := gateway.Methods()
			if len(methods) < 3 || methods[2] != "sessions.patch" {
				t.Fatalf("wrong order: %v", methods)
			}
		})
	}
}
