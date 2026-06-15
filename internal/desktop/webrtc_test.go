package desktop

import (
	"testing"
	"time"
)

func TestIsDesktopInputDataChannelLabelAllowsReliableAndMoveChannels(t *testing.T) {
	if !isDesktopInputDataChannelLabel(desktopReliableInputChannelLabel) {
		t.Fatalf("expected reliable input channel label to be accepted")
	}
	if !isDesktopInputDataChannelLabel(desktopMoveInputChannelLabel) {
		t.Fatalf("expected mouse move input channel label to be accepted")
	}
	if isDesktopInputDataChannelLabel("debug") {
		t.Fatalf("expected unrelated data channel label to be ignored")
	}
}

func TestWaitForICEGatheringCompleteReturnsWhenDoneCloses(t *testing.T) {
	done := make(chan struct{})
	close(done)

	if err := waitForICEGatheringComplete(done, time.Second); err != nil {
		t.Fatalf("expected closed gathering channel to succeed: %v", err)
	}
}

func TestWaitForICEGatheringCompleteTimesOut(t *testing.T) {
	done := make(chan struct{})

	start := time.Now()
	err := waitForICEGatheringComplete(done, 10*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout helper waited too long: %s", elapsed)
	}
}
