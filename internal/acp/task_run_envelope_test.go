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

func TestTaskRunEnvelopeEnrichesACPParamsWithoutAcceptingClientNamespace(t *testing.T) {
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
		"sessionId":   "session-1",
		"threadId":    "session-1",
		"namespaceId": "ns-client-forged",
		"metadata":    map[string]any{"user": "value"},
	}, now)
	if err != nil {
		t.Fatalf("enrich params: %v", err)
	}
	if params["namespaceId"] != "ns-authoritative" {
		t.Fatalf("namespace was not overwritten: %#v", params["namespaceId"])
	}
	metadata, ok := params["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing: %#v", params["metadata"])
	}
	if metadata["xworkmateTaskRunId"] != "run-1" || metadata["xworkmateFence"] != int64(4) {
		t.Fatalf("lease metadata missing: %#v", metadata)
	}
}
