package acp

import (
	"net/http/httptest"
	"testing"

	"xworkmate-bridge/internal/shared"
)

func TestDistributedTaskRouterFromDualNodePeerID(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.Topology = "dual-node"
	config.Distributed.LocalNodeID = "cn-xworkmate-bridge"
	config.Distributed.TaskForwardPeerID = "xworkmate-bridge"

	router := newDistributedTaskRouter(distributedTaskRouterConfig{Config: config, Token: "token"})
	if router == nil {
		t.Fatal("expected router")
	}
	decision, ok, err := router.forwardDecision(httptest.NewRequest("POST", "/gateway/openclaw", nil), sessionStart("s1"))
	if err != nil {
		t.Fatalf("forwardDecision() error = %v", err)
	}
	if !ok {
		t.Fatal("expected forward decision")
	}
	if decision.targetNodeID != "xworkmate-bridge" || decision.nextHopID != "xworkmate-bridge" {
		t.Fatalf("decision = %#v, want target and next hop xworkmate-bridge", decision)
	}
	if decision.endpoint != "http://172.29.10.1:8787" {
		t.Fatalf("endpoint = %q, want private main bridge endpoint", decision.endpoint)
	}
}

func TestDistributedTaskRouterDisabledWhenPeerUnset(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.Topology = "dual-node"
	config.Distributed.LocalNodeID = "xworkmate-bridge"

	if router := newDistributedTaskRouter(distributedTaskRouterConfig{Config: config, Token: "token"}); router != nil {
		t.Fatalf("newDistributedTaskRouter() = %#v, want nil", router)
	}
}

func TestDistributedTaskRouterPrefersConfiguredNodeOverDefault(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.Topology = "dual-node"
	config.Distributed.LocalNodeID = "cn-xworkmate-bridge"
	config.Distributed.TaskForwardPeerID = "xworkmate-bridge"
	config.Distributed.Nodes = []DistributedNodeConfig{
		{
			ID:             "xworkmate-bridge",
			BridgeEndpoint: "http://172.29.10.11:8787",
		},
	}

	router := newDistributedTaskRouter(distributedTaskRouterConfig{Config: config, Token: "token"})
	decision, ok, err := router.forwardDecision(httptest.NewRequest("POST", "/acp/rpc", nil), sessionStart("s1"))
	if err != nil {
		t.Fatalf("forwardDecision() error = %v", err)
	}
	if !ok {
		t.Fatal("expected forward decision")
	}
	if decision.endpoint != "http://172.29.10.11:8787" {
		t.Fatalf("endpoint = %q, want configured endpoint", decision.endpoint)
	}
}

func TestDistributedTaskRouterSelectorRoundRobinKeepsSessionAffinity(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.LocalNodeID = "edge-cn"
	config.Distributed.Nodes = []DistributedNodeConfig{
		{ID: "edge-cn", Role: "edge", Zone: "cn", BridgeEndpoint: "http://172.29.10.2:8787"},
		{ID: "worker-a", Role: "executor", Zone: "global", BridgeEndpoint: "http://172.29.10.11:8787", Capabilities: []string{"openclaw"}},
		{ID: "worker-b", Role: "executor", Zone: "global", BridgeEndpoint: "http://172.29.10.12:8787", Capabilities: []string{"openclaw"}},
	}
	config.Distributed.Forwarding.Rules = []DistributedForwardRuleConfig{
		{
			Methods: []string{"session.start", "session.message"},
			Target: DistributedForwardTargetConfig{
				Selector: DistributedForwardSelectorConfig{Role: "executor", Capability: "openclaw"},
				Strategy: "round_robin",
			},
		},
	}

	router := newDistributedTaskRouter(distributedTaskRouterConfig{Config: config, Token: "token"})
	first, ok, err := router.forwardDecision(httptest.NewRequest("POST", "/acp/rpc", nil), sessionStart("s1"))
	if err != nil {
		t.Fatalf("first forwardDecision() error = %v", err)
	}
	if !ok || first.targetNodeID != "worker-a" {
		t.Fatalf("first decision = %#v, want worker-a", first)
	}
	secondSession, ok, err := router.forwardDecision(httptest.NewRequest("POST", "/acp/rpc", nil), sessionStart("s2"))
	if err != nil {
		t.Fatalf("second forwardDecision() error = %v", err)
	}
	if !ok || secondSession.targetNodeID != "worker-b" {
		t.Fatalf("second session decision = %#v, want worker-b", secondSession)
	}
	followUp, ok, err := router.forwardDecision(httptest.NewRequest("POST", "/acp/rpc", nil), sessionMessage("s1"))
	if err != nil {
		t.Fatalf("follow-up forwardDecision() error = %v", err)
	}
	if !ok || followUp.targetNodeID != "worker-a" {
		t.Fatalf("follow-up decision = %#v, want sticky worker-a", followUp)
	}
}

func TestDistributedTaskRouterUsesExplicitNextHopRoute(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.LocalNodeID = "edge-cn"
	config.Distributed.Nodes = []DistributedNodeConfig{
		{ID: "edge-cn", Role: "edge", BridgeEndpoint: "http://172.29.10.2:8787"},
		{ID: "hub-main", Role: "hub", BridgeEndpoint: "http://172.29.10.1:8787"},
		{ID: "worker-eu", Role: "executor", BridgeEndpoint: "http://172.29.10.30:8787"},
	}
	config.Distributed.Forwarding.Rules = []DistributedForwardRuleConfig{
		{
			Methods: []string{"session.start"},
			Target:  DistributedForwardTargetConfig{NodeID: "worker-eu"},
		},
	}
	config.Distributed.Forwarding.Routes = []DistributedRouteConfig{
		{TargetNodeID: "worker-eu", NextHopNodeID: "hub-main"},
	}

	router := newDistributedTaskRouter(distributedTaskRouterConfig{Config: config, Token: "token"})
	decision, ok, err := router.forwardDecision(httptest.NewRequest("POST", "/acp/rpc", nil), sessionStart("s1"))
	if err != nil {
		t.Fatalf("forwardDecision() error = %v", err)
	}
	if !ok {
		t.Fatal("expected forward decision")
	}
	if decision.targetNodeID != "worker-eu" || decision.nextHopID != "hub-main" || decision.endpoint != "http://172.29.10.1:8787" {
		t.Fatalf("decision = %#v, want target worker-eu via hub-main", decision)
	}
}

func TestDistributedTaskRouterStopsWhenForwardedTargetIsLocal(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.LocalNodeID = "hub-main"
	config.Distributed.Nodes = []DistributedNodeConfig{
		{ID: "hub-main", Role: "hub", BridgeEndpoint: "http://172.29.10.1:8787"},
		{ID: "worker-eu", Role: "executor", BridgeEndpoint: "http://172.29.10.30:8787"},
	}
	config.Distributed.Forwarding.Rules = []DistributedForwardRuleConfig{
		{Methods: []string{"session.start"}, Target: DistributedForwardTargetConfig{NodeID: "worker-eu"}},
	}

	router := newDistributedTaskRouter(distributedTaskRouterConfig{Config: config, Token: "token"})
	req := httptest.NewRequest("POST", "/acp/rpc", nil)
	req.Header.Set(distributedForwardedHeader, "1")
	req.Header.Set(distributedTargetHeader, "hub-main")
	if _, ok, err := router.forwardDecision(req, sessionStart("s1")); err != nil || ok {
		t.Fatalf("forwardDecision() ok=%t err=%v, want local execution", ok, err)
	}
}

func TestDistributedTaskRouterRejectsHopLimitExceeded(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.LocalNodeID = "hub-main"
	config.Distributed.Forwarding.HopLimit = 1
	config.Distributed.Nodes = []DistributedNodeConfig{
		{ID: "hub-main", Role: "hub", BridgeEndpoint: "http://172.29.10.1:8787"},
		{ID: "worker-eu", Role: "executor", BridgeEndpoint: "http://172.29.10.30:8787"},
	}
	config.Distributed.Forwarding.Rules = []DistributedForwardRuleConfig{
		{Methods: []string{"session.start"}, Target: DistributedForwardTargetConfig{NodeID: "worker-eu"}},
	}

	router := newDistributedTaskRouter(distributedTaskRouterConfig{Config: config, Token: "token"})
	req := httptest.NewRequest("POST", "/acp/rpc", nil)
	req.Header.Set(distributedForwardedHeader, "1")
	req.Header.Set(distributedTargetHeader, "worker-eu")
	req.Header.Set(distributedHopHeader, "1")
	if _, ok, err := router.forwardDecision(req, sessionStart("s1")); err == nil || ok {
		t.Fatalf("forwardDecision() ok=%t err=%v, want hop limit error", ok, err)
	}
}

func TestDistributedForwardURLRejectsPublicEndpoint(t *testing.T) {
	if _, err := distributedForwardURL("https://xworkmate-bridge.svc.plus", "/acp/rpc"); err == nil {
		t.Fatal("expected public endpoint rejection")
	}
}

func sessionStart(sessionID string) shared.RPCRequest {
	return shared.RPCRequest{
		ID:     sessionID,
		Method: "session.start",
		Params: map[string]any{"sessionId": sessionID, "threadId": "thread-" + sessionID},
	}
}

func sessionMessage(sessionID string) shared.RPCRequest {
	return shared.RPCRequest{
		ID:     sessionID + "-message",
		Method: "session.message",
		Params: map[string]any{"sessionId": sessionID, "threadId": "thread-" + sessionID},
	}
}
