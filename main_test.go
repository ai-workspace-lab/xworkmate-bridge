package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeJSON(t *testing.T) {
	t.Parallel()

	payload, err := parseClaudeJSON("log line\n{\"result\":\"review ok\",\"is_error\":false}\n")
	if err != nil {
		t.Fatalf("parseClaudeJSON returned error: %v", err)
	}
	if got := payload["result"]; got != "review ok" {
		t.Fatalf("unexpected result: %v", got)
	}
}

func TestCallOpenAICompatible(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected auth header: %s", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if got := body["model"]; got != "qwen2.5-coder:latest" {
			t.Fatalf("unexpected model: %v", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": "review ok",
					},
				},
			},
		})
	}))
	defer server.Close()

	output, err := callOpenAICompatible(
		server.URL,
		"test-key",
		"qwen2.5-coder:latest",
		[]map[string]string{
			{"role": "user", "content": "hello"},
		},
	)
	if err != nil {
		t.Fatalf("callOpenAICompatible returned error: %v", err)
	}
	if output != "review ok" {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestHandleChatToolRequiresPrompt(t *testing.T) {
	t.Setenv("LLM_API_KEY", "test-key")
	t.Setenv("LLM_BASE_URL", "http://127.0.0.1:11434/v1")

	_, err := handleChatTool(map[string]any{})
	if err == nil || err.Error() != "prompt is required" {
		t.Fatalf("expected prompt error, got %v", err)
	}
}

func TestParseClaudeJSONReturnsErrorForPlainText(t *testing.T) {
	t.Parallel()

	_, err := parseClaudeJSON("plain text only\n")
	if err == nil {
		t.Fatal("expected parse error for plain text output")
	}
}

func TestCallOpenAICompatibleReturnsStatusError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := callOpenAICompatible(
		server.URL,
		"test-key",
		"qwen2.5-coder:latest",
		[]map[string]string{{"role": "user", "content": "hello"}},
	)
	if err == nil || err.Error() == "" {
		t.Fatal("expected non-2xx status error")
	}
}

func TestRunClaudeReviewSurfacesCliExitFailure(t *testing.T) {
	tempDir := t.TempDir()
	cliPath := filepath.Join(tempDir, "claude")
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\necho boom >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	t.Setenv("CLAUDE_BIN", cliPath)

	_, err := runClaudeReview("review this", "", "", "", 2*time.Second)
	if err == nil || err.Error() == "" {
		t.Fatal("expected cli failure")
	}
}

func TestRunClaudeReviewSurfacesNonJSONStdout(t *testing.T) {
	tempDir := t.TempDir()
	cliPath := filepath.Join(tempDir, "claude")
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\necho plain-text-output\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}
	t.Setenv("CLAUDE_BIN", cliPath)

	_, err := runClaudeReview("review this", "", "", "", 2*time.Second)
	if err == nil || err.Error() == "" {
		t.Fatal("expected non-json stdout error")
	}
}

func TestPrintBridgeVersionInfoMatchesPingContract(t *testing.T) {
	t.Setenv("IMAGE", "ghcr.io/x-evor/xworkmate-bridge:0123456789abcdef0123456789abcdef01234567")

	output := captureStdout(t, printBridgeVersionInfo)

	var payload map[string]any
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("decode version payload: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("expected status ok, got %#v", payload["status"])
	}
	if payload["image"] != "ghcr.io/x-evor/xworkmate-bridge:0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("unexpected image ref: %#v", payload["image"])
	}
	if payload["tag"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("unexpected tag: %#v", payload["tag"])
	}
	if payload["commit"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("unexpected commit: %#v", payload["commit"])
	}
	if payload["version"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("unexpected version: %#v", payload["version"])
	}
}

func TestPrintBridgeVersionInfoWithoutImageEnv(t *testing.T) {
	t.Setenv("IMAGE", "")

	output := captureStdout(t, printBridgeVersionInfo)
	if !strings.Contains(output, `"status":"ok"`) {
		t.Fatalf("unexpected output: %q", output)
	}
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = old
	}()

	done := make(chan string, 1)
	go func() {
		var builder strings.Builder
		_, _ = io.Copy(&builder, r)
		done <- builder.String()
	}()

	callErr := fn()
	_ = w.Close()
	out := <-done
	_ = r.Close()

	if callErr != nil {
		t.Fatalf("call failed: %v", callErr)
	}
	return strings.TrimSpace(out)
}
