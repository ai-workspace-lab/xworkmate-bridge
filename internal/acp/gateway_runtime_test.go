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

func TestReassociateOpenClawTaskDerivesRuntimeBudgetWithoutExplicitBudget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		params map[string]any
		want   int
	}{
		{
			name: "short task load class",
			params: map[string]any{
				"runId":         "run-short",
				"artifactScope": "tasks/main/run-short",
				"taskLoadClass": "short_task",
			},
			want: openClawShortTaskMinutes,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := NewServer()
			sess := server.reassociateOpenClawTask(tc.params)
			if sess == nil {
				t.Fatal("expected reassociated session")
			} else {
				sess.mu.Lock()
				gotTaskBudget := sess.task.RuntimeBudgetMinutes
				gotRecordBudget := sess.openClaw.RuntimeBudgetMinutes
				sess.mu.Unlock()

				if gotTaskBudget != tc.want {
					t.Fatalf("task RuntimeBudgetMinutes = %d, want %d", gotTaskBudget, tc.want)
				}
				if gotRecordBudget != tc.want {
					t.Fatalf("record RuntimeBudgetMinutes = %d, want %d", gotRecordBudget, tc.want)
				}
			}
		})
	}
}
