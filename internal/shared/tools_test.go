package shared

import (
	"path/filepath"
	"testing"
)

func TestNormalizeProviderWorkingDirectoryPreservesAccessibleDir(t *testing.T) {
	accessible := t.TempDir()

	got, effective := NormalizeProviderWorkingDirectory("opencode", accessible)

	if got != accessible || effective != accessible {
		t.Fatalf("expected accessible dir preserved, got %q %q", got, effective)
	}
}

func TestNormalizeProviderWorkingDirectoryFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	missing := filepath.Join(t.TempDir(), "missing")

	got, effective := NormalizeProviderWorkingDirectory("codex", missing)

	if got != home || effective != home {
		t.Fatalf("expected fallback to home %q, got %q %q", home, got, effective)
	}
}

func TestNormalizeProviderWorkingDirectorySkipsUnknownProvider(t *testing.T) {
	dir := t.TempDir()

	got, effective := NormalizeProviderWorkingDirectory("claude", dir)

	if got != dir || effective != dir {
		t.Fatalf("expected unknown provider to keep dir, got %q %q", got, effective)
	}
}

func TestResolveProviderCommandSupportsHermes(t *testing.T) {
	t.Setenv("ACP_HERMES_BIN", "/usr/local/bin/hermes")

	command, args := ResolveProviderCommand("hermes", "sonnet", "hello world", "/tmp/work")

	if command != "/usr/local/bin/hermes" {
		t.Fatalf("expected hermes binary override, got %q", command)
	}
	if len(args) != 4 {
		t.Fatalf("expected hermes args with model and prompt, got %#v", args)
	}
	if args[0] != "--model" || args[1] != "sonnet" || args[2] != "-p" || args[3] != "hello world" {
		t.Fatalf("unexpected hermes args: %#v", args)
	}
}
