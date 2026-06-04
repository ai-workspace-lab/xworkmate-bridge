package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopCommandEnvPreservesProcessEnvironmentAndOverridesDisplay(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin")
	t.Setenv("HOME", "/home/ubuntu")
	t.Setenv("DISPLAY", ":old")

	env := desktopCommandEnv(":0.0")

	if !envContains(env, "PATH=/usr/local/bin:/usr/bin") {
		t.Fatalf("expected PATH to be preserved, got %#v", env)
	}
	if !envContains(env, "HOME=/home/ubuntu") {
		t.Fatalf("expected HOME to be preserved, got %#v", env)
	}
	if !envContains(env, "DISPLAY=:0.0") {
		t.Fatalf("expected DISPLAY override, got %#v", env)
	}
	if countEnvPrefix(env, "DISPLAY=") != 1 {
		t.Fatalf("expected exactly one DISPLAY entry, got %#v", env)
	}
}

func TestDesktopCommandEnvAddsHomeXauthorityWhenAvailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/local/bin:/usr/bin")
	t.Setenv("DISPLAY", ":old")
	if err := os.WriteFile(filepath.Join(home, ".Xauthority"), []byte("cookie"), 0o600); err != nil {
		t.Fatalf("failed to create Xauthority fixture: %v", err)
	}

	env := desktopCommandEnv(":12")

	if !envContains(env, "DISPLAY=:12") {
		t.Fatalf("expected DISPLAY override, got %#v", env)
	}
	if !envContains(env, "XAUTHORITY="+filepath.Join(home, ".Xauthority")) {
		t.Fatalf("expected XAUTHORITY from HOME, got %#v", env)
	}
}

func TestResolveDesktopDisplayWithProberUsesRequestedExplicitDisplay(t *testing.T) {
	got, ok := resolveDesktopDisplayWithProber(
		":12",
		"",
		[]string{":11"},
		func(display string) bool {
			t.Fatalf("explicit display should not be probed, got %s", display)
			return false
		},
	)

	if !ok || got != ":12" {
		t.Fatalf("expected explicit display :12, got %q ok=%v", got, ok)
	}
}

func TestResolveDesktopDisplayWithProberSelectsActiveSocketDisplay(t *testing.T) {
	got, ok := resolveDesktopDisplayWithProber(
		":0.0",
		"",
		[]string{":12", ":11", ":10"},
		func(display string) bool {
			return display == ":11"
		},
	)

	if !ok || got != ":11" {
		t.Fatalf("expected first probed active display :11, got %q ok=%v", got, ok)
	}
}

func envContains(env []string, expected string) bool {
	for _, item := range env {
		if item == expected {
			return true
		}
	}
	return false
}

func countEnvPrefix(env []string, prefix string) int {
	count := 0
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			count++
		}
	}
	return count
}
