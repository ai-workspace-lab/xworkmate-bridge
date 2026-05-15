package acp

import (
	"testing"

	"xworkmate-bridge/internal/gatewayruntime"
)

func TestConfigureProductionOpenClawGatewayRuntimeUsesLongHandshakeWindows(
	t *testing.T,
) {
	t.Parallel()

	manager := gatewayruntime.NewManager()

	configureProductionOpenClawGatewayRuntime(manager)

	if got := manager.ConnectTimeout; got != productionOpenClawGatewayConnectTimeout {
		t.Fatalf("ConnectTimeout = %s, want %s", got, productionOpenClawGatewayConnectTimeout)
	}
	if got := manager.ChallengeTimeout; got != productionOpenClawGatewayChallengeTimeout {
		t.Fatalf("ChallengeTimeout = %s, want %s", got, productionOpenClawGatewayChallengeTimeout)
	}
}

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

	const want = "127.0.0.1:18789"
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
			Host: "xworkmate-bridge.svc.plus",
			Port: 443,
			TLS:  true,
		},
	})

	const want = "127.0.0.1:18789"
	if got != want {
		t.Fatalf("resolveGatewayReportedRemoteAddress() = %q, want %q", got, want)
	}
}
