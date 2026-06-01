package acp

import (
	"testing"

	"xworkmate-bridge/internal/gatewayruntime"
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
