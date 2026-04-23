package opencodeadapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"xworkmate-bridge/internal/shared"
)

type opencodeHTTPClient struct {
	mu      sync.Mutex
	command string
	args    []string
	cwd     string
	cmd     *exec.Cmd
	baseURL string
	client  *http.Client
}

func newOpenCodeHTTPClient(command string, args []string) *opencodeHTTPClient {
	return &opencodeHTTPClient{
		command: strings.TrimSpace(command),
		args:    append([]string(nil), args...),
		baseURL: "http://127.0.0.1:38993",
		client:  &http.Client{Timeout: 5 * time.Minute},
	}
}

func (c *opencodeHTTPClient) Initialize() (initializeResult, error) {
	if err := c.ensureStarted(); err != nil {
		return initializeResult{}, err
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/global/health", nil)
	if err != nil {
		return initializeResult{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return initializeResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return initializeResult{}, fmt.Errorf("opencode health failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return initializeResult{
		ProtocolVersion: 1,
		AuthMethods: []map[string]any{
			{"id": "opencode-login", "name": "Login with opencode", "description": "Run `opencode auth login` in the terminal"},
		},
		AgentCapabilities: map[string]any{
			"loadSession":         true,
			"mcpCapabilities":     map[string]any{"http": true, "sse": true},
			"promptCapabilities":  map[string]any{"embeddedContext": true, "image": true},
			"sessionCapabilities": map[string]any{"fork": map[string]any{}, "list": map[string]any{}, "resume": map[string]any{}},
		},
	}, nil
}

func (c *opencodeHTTPClient) Call(method string, params map[string]any) (map[string]any, error) {
	if err := c.ensureStarted(); err != nil {
		return nil, err
	}
	switch strings.TrimSpace(method) {
	case "session.start", "session.message":
		sessionID := strings.TrimSpace(fmt.Sprint(params["sessionId"]))
		prompt := strings.TrimSpace(sharedStringArg(params, "taskPrompt", ""))
		return c.postSessionMessage(sessionID, prompt, params)
	case "session.cancel":
		return map[string]any{"accepted": true, "cancelled": false}, nil
	case "session.close":
		return map[string]any{"accepted": true, "closed": true}, nil
	default:
		return nil, fmt.Errorf("unsupported opencode method: %s", method)
	}
}

func sharedStringArg(params map[string]any, key, fallback string) string {
	if params == nil {
		return fallback
	}
	if value := strings.TrimSpace(fmt.Sprint(params[key])); value != "" {
		return value
	}
	return fallback
}

func (c *opencodeHTTPClient) CreateSession(title string) (string, error) {
	if err := c.ensureStarted(); err != nil {
		return "", err
	}
	body := map[string]any{}
	if strings.TrimSpace(title) != "" {
		body["title"] = strings.TrimSpace(title)
	}
	encoded, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/session", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("opencode create session failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("decode opencode create session response: %w", err)
	}
	if sessionID := extractOpenCodeSessionID(decoded); sessionID != "" {
		return sessionID, nil
	}
	if sessionID := extractOpenCodeSessionID(shared.AsMap(decoded["result"])); sessionID != "" {
		return sessionID, nil
	}
	return "", fmt.Errorf("opencode create session returned no session id")
}

func (c *opencodeHTTPClient) SendMessage(sessionID, prompt string, params map[string]any) (map[string]any, error) {
	if err := c.ensureStarted(); err != nil {
		return nil, err
	}
	return c.postSessionMessage(sessionID, prompt, params)
}

func (c *opencodeHTTPClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
	c.cmd = nil
	return nil
}

func (c *opencodeHTTPClient) ensureStarted() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil {
		return nil
	}
	if c.command == "" {
		return fmt.Errorf("opencode command is empty")
	}
	cmd := exec.Command(c.command, c.args...)
	if strings.TrimSpace(c.cwd) != "" {
		cmd.Dir = strings.TrimSpace(c.cwd)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd = cmd
	return c.waitReady()
}

func (c *opencodeHTTPClient) waitReady() error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, c.baseURL+"/global/health", nil)
		resp, err := c.client.Do(req)
		if err == nil && resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("opencode server did not become ready")
}

func (c *opencodeHTTPClient) postSessionMessage(sessionID, prompt string, params map[string]any) (map[string]any, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	body := map[string]any{
		"parts": []map[string]any{
			{"type": "text", "text": strings.TrimSpace(prompt)},
		},
	}
	if model := strings.TrimSpace(fmt.Sprint(params["model"])); model != "" {
		body["model"] = model
	}
	if agent := strings.TrimSpace(fmt.Sprint(params["agent"])); agent != "" {
		body["agent"] = agent
	}
	if system := strings.TrimSpace(fmt.Sprint(params["system"])); system != "" {
		body["system"] = system
	}
	encoded, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/session/"+sessionID+"/message", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opencode session message failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode opencode response: %w", err)
	}
	text := extractOpenCodeText(decoded)
	result := map[string]any{
		"success":   true,
		"provider":  "opencode",
		"mode":      "single-agent",
		"sessionId": sessionID,
		"output":    text,
		"summary":   text,
		"message":   text,
	}
	return result, nil
}

func extractOpenCodeSessionID(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"sessionId", "session_id", "id"} {
			if sessionID := strings.TrimSpace(fmt.Sprint(v[key])); sessionID != "" {
				return sessionID
			}
		}
	}
	return ""
}

func extractOpenCodeText(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"text", "message", "output", "content", "summary"} {
			if text := extractOpenCodeText(v[key]); text != "" {
				return text
			}
		}
		for _, child := range v {
			if text := extractOpenCodeText(child); text != "" {
				return text
			}
		}
	case []any:
		for _, child := range v {
			if text := extractOpenCodeText(child); text != "" {
				return text
			}
		}
	}
	return ""
}
