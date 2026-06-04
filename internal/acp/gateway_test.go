package acp

import (
	"path/filepath"
	"testing"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/shared"
)

func TestApplyProductionGatewayRoutingPreservesGatewayURLPath(t *testing.T) {
	t.Setenv("GATEWAY_RPC_URL", "ws://127.0.0.1:18789/gateway/openclaw")
	server := NewServer()

	request := applyProductionGatewayRouting(
		server,
		gatewayruntime.ConnectRequest{
			Mode: "openclaw",
			Endpoint: gatewayruntime.Endpoint{
				Host: "xworkmate-bridge.svc.plus",
				Port: 443,
				TLS:  true,
			},
		},
	)

	if request.Endpoint.Host != "127.0.0.1" {
		t.Fatalf("expected gateway host from env, got %#v", request.Endpoint)
	}
	if request.Endpoint.Port != 18789 {
		t.Fatalf("expected gateway port from env, got %#v", request.Endpoint)
	}
	if request.Endpoint.TLS {
		t.Fatalf("expected plaintext local gateway endpoint, got %#v", request.Endpoint)
	}
	if request.Endpoint.Path != "/gateway/openclaw" {
		t.Fatalf("expected gateway URL path to be preserved, got %#v", request.Endpoint)
	}
}

func TestApplyProductionGatewayRoutingDefaultsOpenClawEndpoint(t *testing.T) {
	t.Setenv("GATEWAY_RPC_URL", "")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	server := NewServer()

	request := applyProductionGatewayRouting(
		server,
		gatewayruntime.ConnectRequest{Mode: "openclaw"},
	)

	if request.Endpoint.Host != "127.0.0.1" {
		t.Fatalf("expected built-in gateway host, got %#v", request.Endpoint)
	}
	if request.Endpoint.Port != 18789 {
		t.Fatalf("expected built-in gateway port, got %#v", request.Endpoint)
	}
	if request.Endpoint.TLS {
		t.Fatalf("expected built-in gateway endpoint to use local plaintext, got %#v", request.Endpoint)
	}
}

func TestSystemLogsConnectsProductionGatewayForStatus(t *testing.T) {
	gateway := newAcpFakeOpenClawGateway(t)
	defer gateway.Close()

	t.Setenv("GATEWAY_RPC_URL", gateway.URL())
	t.Setenv("BRIDGE_AUTH_TOKEN", "bridge-test-token")
	t.Setenv("BRIDGE_CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_IDENTITY_PATH", filepath.Join(t.TempDir(), "openclaw-device.json"))
	resetBridgeGatewayIdentityForTest()
	t.Cleanup(resetBridgeGatewayIdentityForTest)

	server := NewServer()

	result, rpcErr := server.handleRequest(
		shared.RPCRequest{
			ID:     "status",
			Method: "system.logs",
			Params: map[string]any{},
		},
		func(map[string]any) {},
	)
	if rpcErr != nil {
		t.Fatalf("system.logs returned rpc error: %#v", rpcErr)
	}
	if got := result["gatewayStatus"]; got != "connected" {
		t.Fatalf("expected gatewayStatus connected, got %#v", result)
	}
	if got := gateway.ConnectCount(); got != 1 {
		t.Fatalf("expected one gateway connect attempt, got %d", got)
	}

	result, rpcErr = server.handleRequest(
		shared.RPCRequest{
			ID:     "status-again",
			Method: "system.logs",
			Params: map[string]any{},
		},
		func(map[string]any) {},
	)
	if rpcErr != nil {
		t.Fatalf("second system.logs returned rpc error: %#v", rpcErr)
	}
	if got := result["gatewayStatus"]; got != "connected" {
		t.Fatalf("expected second gatewayStatus connected, got %#v", result)
	}
	if got := gateway.ConnectCount(); got != 1 {
		t.Fatalf("expected connected status to reuse gateway session, got %d connect attempts", got)
	}
}
