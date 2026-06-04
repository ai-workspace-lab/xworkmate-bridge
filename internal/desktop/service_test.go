package desktop

import "testing"

func TestNormalizeRTPPortUsesDesktopDefault(t *testing.T) {
	if got := normalizeRTPPort(0); got != DefaultRTPPort {
		t.Fatalf("expected default RTP port %d, got %d", DefaultRTPPort, got)
	}
	if got := normalizeRTPPort(-1); got != DefaultRTPPort {
		t.Fatalf("expected default RTP port %d for negative port, got %d", DefaultRTPPort, got)
	}
	if got := normalizeRTPPort(6004); got != 6004 {
		t.Fatalf("expected explicit RTP port to be preserved, got %d", got)
	}
}

func TestStopSessionsOnPortReleasesOnlyMatchingRTPPort(t *testing.T) {
	svc := &Service{
		sessions: map[string]*DesktopSession{
			"old-5004": {
				SessionID: "old-5004",
				Port:      DefaultRTPPort,
			},
			"other-6004": {
				SessionID: "other-6004",
				Port:      6004,
			},
		},
	}

	svc.stopSessionsOnPortLocked(DefaultRTPPort)

	if _, ok := svc.sessions["old-5004"]; ok {
		t.Fatalf("expected old session on RTP port %d to be removed", DefaultRTPPort)
	}
	if _, ok := svc.sessions["other-6004"]; !ok {
		t.Fatalf("expected session on a different RTP port to remain")
	}
}
