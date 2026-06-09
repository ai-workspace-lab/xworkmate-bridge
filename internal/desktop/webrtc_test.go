package desktop

import "testing"

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
