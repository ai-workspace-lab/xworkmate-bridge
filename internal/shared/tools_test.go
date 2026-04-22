package shared

import (
	"context"
	"os"
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

func TestRunProviderCommandCreatesMissingWorkingDirectory(t *testing.T) {
	workspaceRoot := filepath.Join(t.TempDir(), "owners", "local", "user", "thread-1")
	scriptPath := filepath.Join(t.TempDir(), "hermes.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("ACP_HERMES_BIN", scriptPath)

	output, err := RunProviderCommand(
		context.Background(),
		"hermes",
		"sonnet",
		"hello world",
		workspaceRoot,
	)
	if err != nil {
		t.Fatalf("RunProviderCommand() error = %v", err)
	}
	if output != "ok" {
		t.Fatalf("RunProviderCommand() output = %q, want %q", output, "ok")
	}
	if info, err := os.Stat(workspaceRoot); err != nil || !info.IsDir() {
		t.Fatalf("expected working directory to be created, stat err=%v info=%v", err, info)
	}
}
