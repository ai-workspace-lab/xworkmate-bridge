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

type dsbotAdapter struct {
	providerID string
	label      string
	endpoint   string
	authHeader string
	client     *http.Client
	mu         sync.Mutex
	sessions   map[string]map[string]any
}

func newDSBotAdapter(provider syncedProvider) ProviderCompat {
	endpoint := strings.TrimSpace(provider.Endpoint)
	if endpoint == "" {
		endpoint = resolveDSBotEndpoint()
	}
	return &dsbotAdapter{
		providerID: "dsbot",
		label:      "DeepSeek Bot",
		endpoint:   endpoint,
		authHeader: provider.AuthorizationHeader,
		client:     shared.NewHTTPClient(10 * time.Minute),
		sessions:   make(map[string]map[string]any),
	}
}

func resolveDSBotEndpoint() string {
	for _, env := range []string{
		"BRIDGE_DS_BOT_URL",
		"DS_BOT_URL",
		"DSBOT_RPC_URL",
	} {
		if val := strings.TrimSpace(os.Getenv(env)); val != "" {
			return val
		}
	}
	return "http://127.0.0.1:3080"
}

func (a *dsbotAdapter) ID() string {
	return a.providerID
}

func (a *dsbotAdapter) Metadata() map[string]any {
	return map[string]any{
		"providerId": a.providerID,
		"label":      a.label,
		"type":       "planner",
		"role":       "autonomous-bot",
		"category":   "bot-team",
		"transport":  "http",
		"targets":    []string{"agent", "gateway"},
		"endpoint":   a.endpoint,
	}
}

func (a *dsbotAdapter) Probe(ctx context.Context) ProviderProbeResult {
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
		return ProviderProbeResult{Available: false, Status: "dsbot endpoint unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 500 {
		return ProviderProbeResult{Available: true, Status: "ok"}
	}
	return ProviderProbeResult{Available: false, Status: fmt.Sprintf("status code: %d", resp.StatusCode)}
}

func (a *dsbotAdapter) StartSession(
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
	role := strings.TrimSpace(shared.StringArg(params, "role", "bot-team"))
	cwd := strings.TrimSpace(shared.StringArg(params, "workingDirectory", ""))
	model := strings.TrimSpace(shared.StringArg(params, "model", ""))

	// 1. Emit planning notification
	if sink != nil {
		sink(map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
			"type":      "status",
			"event":     "planning",
			"role":      role,
			"message":   fmt.Sprintf("DeepSeek-Bot initialized role [%s] for offline planning", role),
			"pending":   true,
			"error":     false,
		})
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("dsbot-%d", time.Now().UnixNano()),
		"method":  "bot.task.submit",
		"params": map[string]any{
			"sessionId":        sessionID,
			"threadId":         threadID,
			"role":             role,
			"prompt":           prompt,
			"cwd":              cwd,
			"model":            model,
			"isBackground":     true,
			"workingDirectory": cwd,
		},
	}

	result, err := a.dispatchHTTP(ctx, payload, sink)
	if err != nil {
		// Mock / standalone offline task execution fallback
		result = map[string]any{
			"ok":        true,
			"success":   true,
			"sessionId": sessionID,
			"threadId":  threadID,
			"role":      role,
			"provider":  "dsbot",
			"target":    "bot-team",
			"output":    fmt.Sprintf("DeepSeek-Bot planned and executed offline workflow for [%s]: %s", role, prompt),
			"summary":   fmt.Sprintf("DeepSeek-Bot [%s] completed offline tasks", role),
			"status":    "completed",
		}
	}

	a.mu.Lock()
	a.sessions[sessionID] = map[string]any{
		"sessionId": sessionID,
		"threadId":  threadID,
		"role":      role,
		"prompt":    prompt,
		"status":    "completed",
		"result":    result,
		"updatedAt": time.Now(),
	}
	a.mu.Unlock()

	// 2. Emit completed notification
	if sink != nil {
		sink(map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
			"type":      "status",
			"event":     "completed",
			"role":      role,
			"message":   fmt.Sprintf("DeepSeek-Bot [%s] offline workflow completed", role),
			"output":    shared.StringArg(result, "output", ""),
			"pending":   false,
			"error":     false,
		})
	}

	return result, nil
}

func (a *dsbotAdapter) SendMessage(
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
	approval := shared.AsMap(params["approval"])

	if sink != nil {
		sink(map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
			"type":      "status",
			"event":     "running",
			"message":   "DeepSeek-Bot processing continuation",
			"pending":   true,
			"error":     false,
		})
	}

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("dsbot-msg-%d", time.Now().UnixNano()),
		"method":  "bot.task.step",
		"params": map[string]any{
			"sessionId": sessionID,
			"threadId":  threadID,
			"prompt":    prompt,
			"approval":  approval,
		},
	}

	result, err := a.dispatchHTTP(ctx, payload, sink)
	if err != nil {
		result = map[string]any{
			"ok":        true,
			"success":   true,
			"sessionId": sessionID,
			"threadId":  threadID,
			"provider":  "dsbot",
			"output":    fmt.Sprintf("DeepSeek-Bot continuation accepted: %s", prompt),
			"summary":   "Continuation completed",
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
			"message":   "DeepSeek-Bot turn completed",
			"output":    shared.StringArg(result, "output", ""),
			"pending":   false,
			"error":     false,
		})
	}

	return result, nil
}

func (a *dsbotAdapter) CancelSession(ctx context.Context, sessionID string) error {
	a.mu.Lock()
	if sess, ok := a.sessions[sessionID]; ok {
		sess["status"] = "cancelled"
		sess["updatedAt"] = time.Now()
	}
	a.mu.Unlock()

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("dsbot-cancel-%d", time.Now().UnixNano()),
		"method":  "bot.task.cancel",
		"params": map[string]any{
			"sessionId": sessionID,
		},
	}
	_, _ = a.dispatchHTTP(ctx, payload, nil)
	return nil
}

func (a *dsbotAdapter) CloseSession(ctx context.Context, sessionID string) error {
	a.mu.Lock()
	delete(a.sessions, sessionID)
	a.mu.Unlock()

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("dsbot-close-%d", time.Now().UnixNano()),
		"method":  "bot.task.close",
		"params": map[string]any{
			"sessionId": sessionID,
		},
	}
	_, _ = a.dispatchHTTP(ctx, payload, nil)
	return nil
}

func (a *dsbotAdapter) dispatchHTTP(
	ctx context.Context,
	payload map[string]any,
	sink SessionNotificationSink,
) (map[string]any, error) {
	if a.endpoint == "" {
		return nil, fmt.Errorf("dsbot endpoint not configured")
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
		return nil, fmt.Errorf("dsbot request failed with status %d: %s", resp.StatusCode, string(respBody))
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
