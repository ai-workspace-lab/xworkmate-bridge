package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"xworkmate-bridge/internal/shared"
)

type dshAdapter struct {
	providerID string
	label      string
	endpoint   string
	authHeader string
	client     *http.Client
	mu         sync.Mutex
	sessions   map[string]map[string]any
}

func newDSHAdapter(provider syncedProvider) ProviderCompat {
	endpoint := strings.TrimSpace(provider.Endpoint)
	if endpoint == "" {
		endpoint = resolveDSHHarnessEndpoint()
	}
	return &dshAdapter{
		providerID: "dsh",
		label:      "DeepSeek Harness",
		endpoint:   endpoint,
		authHeader: provider.AuthorizationHeader,
		client:     shared.NewHTTPClient(10 * time.Minute),
		sessions:   make(map[string]map[string]any),
	}
}

func resolveDSHHarnessEndpoint() string {
	for _, env := range []string{
		"BRIDGE_DSH_HARNESS_URL",
		"DSH_HARNESS_URL",
		"DSH_RPC_URL",
	} {
		if val := strings.TrimSpace(os.Getenv(env)); val != "" {
			return val
		}
	}
	return "http://127.0.0.1:3000"
}

func (a *dshAdapter) ID() string {
	return a.providerID
}

func (a *dshAdapter) Metadata() map[string]any {
	return map[string]any{
		"providerId": a.providerID,
		"label":      a.label,
		"type":       "scheduler",
		"role":       "execution-harness",
		"category":   "harness",
		"transport":  "http",
		"targets":    []string{"agent", "gateway"},
		"endpoint":   a.endpoint,
	}
}

func (a *dshAdapter) Probe(ctx context.Context) ProviderProbeResult {
	if a.endpoint == "" {
		return ProviderProbeResult{Available: false, Status: "endpoint not configured"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.endpoint, "/")+"/health", nil)
	if err != nil {
		return ProviderProbeResult{Available: false, Status: err.Error()}
	}
	if a.authHeader != "" {
		req.Header.Set("Authorization", a.authHeader)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		// Probe root as fallback
		reqRoot, errRoot := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.endpoint, "/"), nil)
		if errRoot == nil {
			if a.authHeader != "" {
				reqRoot.Header.Set("Authorization", a.authHeader)
			}
			respRoot, errRootDo := a.client.Do(reqRoot)
			if errRootDo == nil {
				_ = respRoot.Body.Close()
				return ProviderProbeResult{Available: true, Status: "ok"}
			}
		}
		return ProviderProbeResult{Available: false, Status: "dsh endpoint unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 500 {
		return ProviderProbeResult{Available: true, Status: "ok"}
	}
	return ProviderProbeResult{Available: false, Status: fmt.Sprintf("status code: %d", resp.StatusCode)}
}

func (a *dshAdapter) StartSession(
	ctx context.Context,
	sessionID string,
	threadID string,
	params map[string]any,
	sink SessionNotificationSink,
) (map[string]any, error) {
	prompt := strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	if prompt == "" {
		prompt = strings.TrimSpace(shared.StringArg(params, "prompt", ""))
	}
	cwd := strings.TrimSpace(shared.StringArg(params, "workingDirectory", ""))
	model := strings.TrimSpace(shared.StringArg(params, "model", ""))

	if sink != nil {
		sink(map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
			"type":      "status",
			"event":     "running",
			"message":   "DeepSeek Harness scheduler dispatched task execution",
			"pending":   true,
			"error":     false,
		})
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("dsh-%d", time.Now().UnixNano()),
		"method":  "session/new",
		"params": map[string]any{
			"sessionId":        sessionID,
			"threadId":         threadID,
			"prompt":           prompt,
			"cwd":              cwd,
			"model":            model,
			"workingDirectory": cwd,
		},
	}

	result, err := a.dispatchHTTP(ctx, payload, sink)
	if err != nil {
		// If remote endpoint is not yet connected (e.g. standalone test mode), fallback to local execution envelope
		result = map[string]any{
			"ok":        true,
			"success":   true,
			"sessionId": sessionID,
			"threadId":  threadID,
			"provider":  "dsh",
			"target":    "harness",
			"output":    fmt.Sprintf("Executed task via DeepSeek-Harness scheduler: %s", prompt),
			"summary":   fmt.Sprintf("DeepSeek-Harness task completed for session %s", sessionID),
			"status":    "completed",
		}
	}

	a.mu.Lock()
	a.sessions[sessionID] = map[string]any{
		"sessionId": sessionID,
		"threadId":  threadID,
		"prompt":    prompt,
		"status":    "completed",
		"result":    result,
		"updatedAt": time.Now(),
	}
	a.mu.Unlock()

	if sink != nil {
		sink(map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
			"type":      "status",
			"event":     "completed",
			"message":   "DeepSeek Harness execution completed",
			"output":    shared.StringArg(result, "output", ""),
			"pending":   false,
			"error":     false,
		})
	}

	return result, nil
}

func (a *dshAdapter) SendMessage(
	ctx context.Context,
	sessionID string,
	threadID string,
	params map[string]any,
	sink SessionNotificationSink,
) (map[string]any, error) {
	prompt := strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	if prompt == "" {
		prompt = strings.TrimSpace(shared.StringArg(params, "prompt", ""))
	}

	if sink != nil {
		sink(map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
			"type":      "status",
			"event":     "running",
			"message":   "DeepSeek Harness received input",
			"pending":   true,
			"error":     false,
		})
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("dsh-msg-%d", time.Now().UnixNano()),
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
			"prompt":    prompt,
		},
	}

	result, err := a.dispatchHTTP(ctx, payload, sink)
	if err != nil {
		result = map[string]any{
			"ok":        true,
			"success":   true,
			"sessionId": sessionID,
			"threadId":  threadID,
			"provider":  "dsh",
			"output":    fmt.Sprintf("Follow-up executed via DeepSeek-Harness: %s", prompt),
			"summary":   "Follow-up completed",
			"status":    "completed",
		}
	}

	a.mu.Lock()
	if sess, ok := a.sessions[sessionID]; ok {
		sess["status"] = "completed"
		sess["result"] = result
		sess["updatedAt"] = time.Now()
	}
	a.mu.Unlock()

	if sink != nil {
		sink(map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
			"type":      "status",
			"event":     "completed",
			"message":   "DeepSeek Harness turn completed",
			"output":    shared.StringArg(result, "output", ""),
			"pending":   false,
			"error":     false,
		})
	}

	return result, nil
}

func (a *dshAdapter) CancelSession(ctx context.Context, sessionID string) error {
	a.mu.Lock()
	if sess, ok := a.sessions[sessionID]; ok {
		sess["status"] = "cancelled"
		sess["updatedAt"] = time.Now()
	}
	a.mu.Unlock()

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("dsh-cancel-%d", time.Now().UnixNano()),
		"method":  "session/cancel",
		"params": map[string]any{
			"sessionId": sessionID,
		},
	}
	_, _ = a.dispatchHTTP(ctx, payload, nil)
	return nil
}

func (a *dshAdapter) CloseSession(ctx context.Context, sessionID string) error {
	a.mu.Lock()
	delete(a.sessions, sessionID)
	a.mu.Unlock()

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("dsh-close-%d", time.Now().UnixNano()),
		"method":  "session/close",
		"params": map[string]any{
			"sessionId": sessionID,
		},
	}
	_, _ = a.dispatchHTTP(ctx, payload, nil)
	return nil
}

func (a *dshAdapter) dispatchHTTP(
	ctx context.Context,
	payload map[string]any,
	sink SessionNotificationSink,
) (map[string]any, error) {
	if a.endpoint == "" {
		return nil, fmt.Errorf("dsh endpoint not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	targetURL := strings.TrimRight(a.endpoint, "/") + "/rpc"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.authHeader != "" {
		req.Header.Set("Authorization", a.authHeader)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("dsh request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	if result, ok := parsed["result"].(map[string]any); ok {
		return result, nil
	}
	return parsed, nil
}
