package acp

import "testing"

func TestResolveDistributedTaskForwardEndpointFromDualNodeTopology(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.Topology = "dual-node"
	config.Distributed.LocalNodeID = "cn-xworkmate-bridge"
	config.Distributed.TaskForwardPeerID = "xworkmate-bridge"
	config.Distributed.Nodes = []DistributedNodeConfig{
		{
			ID:             "xworkmate-bridge",
			BridgeEndpoint: "http://172.29.10.1:8787",
		},
		{
			ID:             "cn-xworkmate-bridge",
			BridgeEndpoint: "http://172.29.10.2:8787",
		},
	}

	if got := resolveDistributedTaskForwardEndpoint(config); got != "http://172.29.10.1:8787" {
		t.Fatalf("resolveDistributedTaskForwardEndpoint() = %q, want %q", got, "http://172.29.10.1:8787")
	}
}

func TestResolveDistributedTaskForwardEndpointDisabledWhenPeerUnset(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.Topology = "dual-node"
	config.Distributed.LocalNodeID = "xworkmate-bridge"
	config.Distributed.Nodes = []DistributedNodeConfig{
		{
			ID:             "xworkmate-bridge",
			BridgeEndpoint: "http://172.29.10.1:8787",
		},
		{
			ID:             "cn-xworkmate-bridge",
			BridgeEndpoint: "http://172.29.10.2:8787",
		},
	}

	if got := resolveDistributedTaskForwardEndpoint(config); got != "" {
		t.Fatalf("resolveDistributedTaskForwardEndpoint() = %q, want empty endpoint", got)
	}
}

func TestResolveDistributedTaskForwardEndpointKeepsExplicitEndpoint(t *testing.T) {
	config := &BridgeConfig{}
	config.Distributed.TaskForwardEndpoint = "https://xworkmate-bridge.svc.plus"
	config.Distributed.Topology = "dual-node"
	config.Distributed.LocalNodeID = "cn-xworkmate-bridge"
	config.Distributed.TaskForwardPeerID = "xworkmate-bridge"
	config.Distributed.Nodes = []DistributedNodeConfig{
		{
			ID:             "xworkmate-bridge",
			BridgeEndpoint: "http://172.29.10.1:8787",
		},
	}

	if got := resolveDistributedTaskForwardEndpoint(config); got != "https://xworkmate-bridge.svc.plus" {
		t.Fatalf("resolveDistributedTaskForwardEndpoint() = %q, want explicit endpoint", got)
	}
}
