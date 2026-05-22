package acp

import (
	"testing"
	"time"
)

func TestOpenClawGatewayAdmissionGateDefaultsToSingleActiveTask(t *testing.T) {
	gate := newOpenClawGatewayAdmissionGate(&BridgeConfig{})

	if gate.maxActive != 1 {
		t.Fatalf("expected single active OpenClaw task by default, got %d", gate.maxActive)
	}
	if gate.maxQueued != defaultOpenClawGatewayMaxQueued {
		t.Fatalf("expected default max queued, got %d", gate.maxQueued)
	}
	if gate.timeout != defaultOpenClawGatewayQueueWait {
		t.Fatalf("expected default queue timeout, got %s", gate.timeout)
	}
}

func TestOpenClawGatewayAdmissionGateUsesConfigValues(t *testing.T) {
	maxActive := 3
	maxQueued := 7
	gate := newOpenClawGatewayAdmissionGate(&BridgeConfig{
		OpenClawGateway: OpenClawGatewayConfig{
			MaxActive:    &maxActive,
			MaxQueued:    &maxQueued,
			QueueTimeout: "45s",
		},
	})

	if gate.maxActive != 3 {
		t.Fatalf("expected max active from config, got %d", gate.maxActive)
	}
	if gate.maxQueued != 7 {
		t.Fatalf("expected max queued from config, got %d", gate.maxQueued)
	}
	if gate.timeout != 45*time.Second {
		t.Fatalf("expected queue timeout from config, got %s", gate.timeout)
	}
}

func TestOpenClawGatewayAdmissionGateFallsBackToEnvWithoutConfig(t *testing.T) {
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_ACTIVE", "4")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_MAX_QUEUED", "0")
	t.Setenv("XWORKMATE_BRIDGE_OPENCLAW_GATEWAY_QUEUE_TIMEOUT", "30s")

	gate := newOpenClawGatewayAdmissionGate(&BridgeConfig{})

	if gate.maxActive != 4 {
		t.Fatalf("expected max active from env, got %d", gate.maxActive)
	}
	if gate.maxQueued != 0 {
		t.Fatalf("expected max queued from env, got %d", gate.maxQueued)
	}
	if gate.timeout != 30*time.Second {
		t.Fatalf("expected queue timeout from env, got %s", gate.timeout)
	}
}
