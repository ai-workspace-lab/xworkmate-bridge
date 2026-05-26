package acp

import (
	"context"
	"testing"
	"time"
)

func TestProbeProviderForBootstrapTimesOut(t *testing.T) {
	start := time.Now()
	result := probeProviderForBootstrap(
		blockingProviderCompat{},
		20*time.Millisecond,
	)

	if result.Available {
		t.Fatalf("expected timed out provider to be unavailable")
	}
	if result.Status != context.DeadlineExceeded.Error() {
		t.Fatalf("expected deadline status, got %q", result.Status)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("bootstrap probe blocked too long: %s", elapsed)
	}
}

type blockingProviderCompat struct{}

func (blockingProviderCompat) ID() string { return "blocking" }

func (blockingProviderCompat) Metadata() map[string]any {
	return map[string]any{"providerId": "blocking"}
}

func (blockingProviderCompat) Probe(context.Context) ProviderProbeResult {
	select {}
}

func (blockingProviderCompat) StartSession(
	context.Context,
	string,
	string,
	map[string]any,
	SessionNotificationSink,
) (map[string]any, error) {
	return nil, nil
}

func (blockingProviderCompat) SendMessage(
	context.Context,
	string,
	string,
	map[string]any,
	SessionNotificationSink,
) (map[string]any, error) {
	return nil, nil
}

func (blockingProviderCompat) CancelSession(context.Context, string) error {
	return nil
}

func (blockingProviderCompat) CloseSession(context.Context, string) error {
	return nil
}
