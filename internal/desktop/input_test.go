package desktop

import (
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
