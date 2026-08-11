package acp

import (
	"testing"
	"time"
)

func TestTaskRunEnvelopeRejectsMissingIdentityAndExpiredLease(t *testing.T) {
	now := time.Unix(100, 0)
	cases := []struct {
		name string
		env  TaskRunEnvelope
	}{
		{name: "missing task id", env: TaskRunEnvelope{SessionID: "s-1", ThreadID: "s-1", NamespaceID: "ns-1", Fence: 1, LeaseToken: "lease", LeaseExpiresAt: now.Add(time.Minute)}},
		{name: "missing namespace", env: TaskRunEnvelope{TaskRunID: "run-1", SessionID: "s-1", ThreadID: "s-1", Fence: 1, LeaseToken: "lease", LeaseExpiresAt: now.Add(time.Minute)}},
		{name: "expired", env: TaskRunEnvelope{TaskRunID: "run-1", SessionID: "s-1", ThreadID: "s-1", NamespaceID: "ns-1", Fence: 1, LeaseToken: "lease", LeaseExpiresAt: now.Add(-time.Second)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.env.Validate(now); err == nil {
				t.Fatal("expected envelope validation error")
			}
		})
	}
}

func TestTaskRunEnvelopeRejectsClientIdentityOverrides(t *testing.T) {
	now := time.Unix(100, 0)
	env := TaskRunEnvelope{
		TaskRunID:      "run-1",
		AccountID:      "account-1",
		NamespaceID:    "ns-authoritative",
		SessionID:      "session-1",
		ThreadID:       "session-1",
		Fence:          4,
		LeaseToken:     "lease-token",
		LeaseExpiresAt: now.Add(time.Minute),
	}
	cases := []struct {
		name   string
		params map[string]any
	}{
		{name: "namespace", params: map[string]any{"namespaceId": "ns-client-forged"}},
		{name: "account", params: map[string]any{"accountId": "account-client-forged"}},
		{name: "session", params: map[string]any{"sessionId": "session-client-forged"}},
		{name: "thread", params: map[string]any{"threadId": "thread-client-forged"}},
		{name: "task run", params: map[string]any{"taskRunId": "run-client-forged"}},
		{name: "fence", params: map[string]any{"fence": int64(99)}},
		{name: "metadata namespace", params: map[string]any{"metadata": map[string]any{"xworkmateNamespaceId": "ns-client-forged"}}},
		{name: "metadata fence", params: map[string]any{"metadata": map[string]any{"xworkmateFence": int64(99)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := env.EnrichParams(tc.params, now); err == nil {
				t.Fatal("expected authoritative identity conflict")
			}
		})
	}
}

func TestTaskRunEnvelopeEnrichesACPParamsWithoutForwardingLeaseSecret(t *testing.T) {
	now := time.Unix(100, 0)
	env := TaskRunEnvelope{
		TaskRunID:      "run-1",
		AccountID:      "account-1",
		NamespaceID:    "ns-authoritative",
		SessionID:      "session-1",
		ThreadID:       "session-1",
		Fence:          4,
		LeaseToken:     "lease-token",
		LeaseExpiresAt: now.Add(time.Minute),
	}
	params, err := env.EnrichParams(map[string]any{
		"sessionId": "session-1",
		"threadId":  "session-1",
		"metadata":  map[string]any{"user": "value"},
	}, now)
	if err != nil {
		t.Fatalf("enrich params: %v", err)
	}
	if params["namespaceId"] != "ns-authoritative" || params["accountId"] != "account-1" {
		t.Fatalf("authoritative identity missing: %#v", params)
	}
	metadata, ok := params["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing: %#v", params["metadata"])
	}
	if metadata["xworkmateTaskRunId"] != "run-1" || metadata["xworkmateFence"] != int64(4) {
		t.Fatalf("lease metadata missing: %#v", metadata)
	}
	if _, ok := metadata["xworkmateLeaseToken"]; ok {
		t.Fatalf("lease secret must not cross the ACP boundary: %#v", metadata)
	}
}
