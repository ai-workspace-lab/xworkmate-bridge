package acp

import (
	"testing"

	"xworkmate-bridge/internal/gatewayruntime"
)

func TestResolveGatewayReportedRemoteAddressUsesBuiltInOpenClawEndpoint(t *testing.T) {
	t.Parallel()

	server := NewServer()

	got := resolveGatewayReportedRemoteAddress(server, gatewayruntime.ConnectRequest{
		Mode: "openclaw",
		Endpoint: gatewayruntime.Endpoint{
			Host: "127.0.0.1",
			Port: 18789,
			TLS:  false,
		},
	})

	const want = "xworkmate-bridge.svc.plus:443"
	if got != want {
		t.Fatalf("resolveGatewayReportedRemoteAddress() = %q, want %q", got, want)
	}
}

func TestResolveGatewayReportedRemoteAddressNormalizesExplicitPublicRemoteHost(
	t *testing.T,
) {
	t.Parallel()

	server := NewServer()

	got := resolveGatewayReportedRemoteAddress(server, gatewayruntime.ConnectRequest{
		Mode: "openclaw",
		Endpoint: gatewayruntime.Endpoint{
			Host: "openclaw.svc.plus",
			Port: 443,
			TLS:  true,
		},
	})

	const want = "xworkmate-bridge.svc.plus:443"
	if got != want {
		t.Fatalf("resolveGatewayReportedRemoteAddress() = %q, want %q", got, want)
	}
}
