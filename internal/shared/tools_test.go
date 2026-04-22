package shared

import (
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

func TestEnsureWorkingDirectoryExistsCreatesMissingWorkingDirectory(t *testing.T) {
	workspaceRoot := filepath.Join(t.TempDir(), "owners", "local", "user", "thread-1")

	if err := ensureWorkingDirectoryExists(workspaceRoot); err != nil {
		t.Fatalf("ensureWorkingDirectoryExists() error = %v", err)
	}
	if info, err := os.Stat(workspaceRoot); err != nil || !info.IsDir() {
		t.Fatalf("expected working directory to be created, stat err=%v info=%v", err, info)
	}
}
