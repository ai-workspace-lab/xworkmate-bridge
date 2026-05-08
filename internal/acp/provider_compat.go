package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/shared"
)

type externalACPCompat struct {
	providerID string
	label      string
	endpoint   string
	authHeader string
	category   string
	client     *http.Client
}

type codexCompat struct {
	*externalACPCompat
	mu      sync.Mutex
	threads map[string]string
}
type opencodeCompat struct{ *externalACPCompat }
type geminiCompat struct{ *externalACPCompat }
type hermesCompat struct{ *externalACPCompat }

type sessionContinuationUnavailableError struct {
	providerID string
	sessionID  string
	threadID   string
	reason     string
}

func (e sessionContinuationUnavailableError) Error() string {
	if e.reason != "" {
		return "SESSION_CONTINUATION_UNAVAILABLE: " + e.reason
	}
	return "SESSION_CONTINUATION_UNAVAILABLE: provider session state is unavailable"
}

func newSessionContinuationUnavailableError(providerID, sessionID, threadID, reason string) error {
	return sessionContinuationUnavailableError{
		providerID: strings.TrimSpace(providerID),
		sessionID:  strings.TrimSpace(sessionID),
		threadID:   strings.TrimSpace(threadID),
		reason:     strings.TrimSpace(reason),
	}
}

func asSessionContinuationUnavailableError(err error) (sessionContinuationUnavailableError, bool) {
	if err == nil {
		return sessionContinuationUnavailableError{}, false
	}
	if typed, ok := err.(sessionContinuationUnavailableError); ok {
		return typed, true
	}
	if typed, ok := err.(*sessionContinuationUnavailableError); ok && typed != nil {
		return *typed, true
	}
	return sessionContinuationUnavailableError{}, false
}

func newProviderCompat(provider syncedProvider) ProviderCompat {
	base := &externalACPCompat{
		providerID: provider.ProviderID,
		label:      provider.Label,
		endpoint:   resolveSingleAgentForwardEndpoint(provider),
		authHeader: provider.AuthorizationHeader,
		category:   providerCategory(provider.ProviderID),
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
	switch provider.ProviderID {
	case "gemini":
		return &geminiCompat{externalACPCompat: base}
	case "opencode":
		return &opencodeCompat{externalACPCompat: base}
	case "hermes":
		return &hermesCompat{externalACPCompat: base}
	default:
		return &codexCompat{
			externalACPCompat: base,
			threads:           make(map[string]string),
		}
	}
}

func providerCategory(providerID string) string {
	switch providerID {
	case "gemini", "hermes", "opencode":
		return "protocol-adapter"
	default:
		return "native"
	}
}

func (c *externalACPCompat) ID() string { return c.providerID }

func (c *externalACPCompat) Metadata() map[string]any {
	return map[string]any{
		"providerId": c.providerID,
		"label":      c.label,
		"category":   c.category,
		"transport":  c.transport(),
	}
}

func (c *externalACPCompat) Probe(ctx context.Context) ProviderProbeResult {
	_, err := c.rpcCall(ctx, "acp.capabilities", nil, nil)
	if err != nil {
		return ProviderProbeResult{Available: false, Status: err.Error()}
	}
	return ProviderProbeResult{Available: true, Status: "ok"}
}

func (c *codexCompat) Probe(ctx context.Context) ProviderProbeResult {
	_, err := c.codexCall(ctx, "initialize", codexInitializeParams(), nil)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already initialized") {
			return ProviderProbeResult{Available: true, Status: "ok"}
		}
		return ProviderProbeResult{Available: false, Status: err.Error()}
	}
	return ProviderProbeResult{Available: true, Status: "ok"}
}

func (c *codexCompat) StartSession(ctx context.Context, sessionID string, threadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	if c.transport() == "ws" {
		return c.startSessionWS(ctx, sessionID, threadID, params, sink)
	}
	thread, err := c.codexCall(ctx, "thread/start", codexThreadStartParams(params), nil)
	if err != nil {
		return nil, err
	}
	codexThreadID := codexThreadIDFromResult(thread)
	if codexThreadID == "" {
		return nil, fmt.Errorf("codex thread/start response missing thread id")
	}
	c.rememberThread(sessionID, threadID, codexThreadID)
	return c.startTurn(ctx, codexThreadID, params, sink)
}

func (c *codexCompat) SendMessage(ctx context.Context, sessionID string, threadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	if c.transport() == "ws" {
		return c.sendMessageWS(ctx, sessionID, threadID, params, sink)
	}
	codexThreadID := c.lookupThread(sessionID, threadID)
	if codexThreadID == "" {
		return nil, newSessionContinuationUnavailableError(
			c.providerID,
			sessionID,
			threadID,
			"provider thread mapping is missing",
		)
	}
	thread, err := c.codexCall(ctx, "thread/resume", map[string]any{"threadId": codexThreadID}, nil)
	if err != nil {
		return nil, err
	}
	if resolved := codexThreadIDFromResult(thread); resolved != "" {
		codexThreadID = resolved
		c.rememberThread(sessionID, threadID, codexThreadID)
	}
	return c.startTurn(ctx, codexThreadID, params, sink)
}

func (c *codexCompat) startSessionWS(ctx context.Context, sessionID string, threadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	return c.withInitializedCodexWS(ctx, func(conn *websocket.Conn) (map[string]any, error) {
		thread, err := c.writeAndReadWSRPC(ctx, conn, "thread/start", codexThreadStartParams(params), nil)
		if err != nil {
			return nil, err
		}
		codexThreadID := codexThreadIDFromResult(thread)
		if codexThreadID == "" {
			return nil, fmt.Errorf("codex thread/start response missing thread id")
		}
		c.rememberThread(sessionID, threadID, codexThreadID)
		return c.startTurnOnWS(ctx, conn, codexThreadID, params, sink)
	})
}

func (c *codexCompat) sendMessageWS(ctx context.Context, sessionID string, threadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	codexThreadID := c.lookupThread(sessionID, threadID)
	if codexThreadID == "" {
		return nil, newSessionContinuationUnavailableError(
			c.providerID,
			sessionID,
			threadID,
			"provider thread mapping is missing",
		)
	}
	return c.withInitializedCodexWS(ctx, func(conn *websocket.Conn) (map[string]any, error) {
		thread, err := c.writeAndReadWSRPC(ctx, conn, "thread/resume", map[string]any{"threadId": codexThreadID}, nil)
		if err != nil {
			return nil, err
		}
		if resolved := codexThreadIDFromResult(thread); resolved != "" {
			codexThreadID = resolved
			c.rememberThread(sessionID, threadID, codexThreadID)
		}
		return c.startTurnOnWS(ctx, conn, codexThreadID, params, sink)
	})
}

func (c *codexCompat) CloseSession(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	delete(c.threads, sessionID)
	c.mu.Unlock()
	return nil
}

func (c *codexCompat) CancelSession(ctx context.Context, sessionID string) error {
	codexThreadID := c.lookupThread(sessionID, "")
	if codexThreadID == "" {
		return nil
	}
	_, err := c.codexCall(ctx, "turn/interrupt", map[string]any{"threadId": codexThreadID}, nil)
	return err
}

func (c *codexCompat) startTurn(ctx context.Context, codexThreadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	result, err := c.codexCall(
		ctx,
		"turn/start",
		map[string]any{
			"threadId": codexThreadID,
			"input":    codexUserInput(params),
		},
		sink,
	)
	if err != nil {
		return nil, err
	}
	return c.finalizeCodexTurnResult(codexThreadID, result), nil
}

func (c *codexCompat) startTurnOnWS(ctx context.Context, conn *websocket.Conn, codexThreadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	result, err := c.writeAndReadWSRPC(
		ctx,
		conn,
		"turn/start",
		map[string]any{
			"threadId": codexThreadID,
			"input":    codexUserInput(params),
		},
		sink,
	)
	if err != nil {
		return nil, err
	}
	return c.finalizeCodexTurnResult(codexThreadID, result), nil
}

func (c *codexCompat) finalizeCodexTurnResult(codexThreadID string, result map[string]any) map[string]any {
	if _, ok := result["output"]; !ok {
		if summary := strings.TrimSpace(shared.StringArg(result, "summary", "")); summary != "" {
			result["output"] = summary
		}
	}
	result["providerThreadId"] = codexThreadID
	if codexDisplayText(result) == "" && !isProviderFailureResult(result) {
		result["success"] = false
		result["error"] = "codex returned no displayable output"
		result["message"] = "codex returned no displayable output"
	}
	return result
}

func codexDisplayText(result map[string]any) string {
	for _, key := range []string{"output", "summary", "message"} {
		if text := strings.TrimSpace(shared.StringArg(result, key, "")); text != "" && !isGenericHermesAckText(text) {
			return text
		}
	}
	return ""
}

func isProviderFailureResult(result map[string]any) bool {
	if result == nil {
		return false
	}
	if value, ok := result["success"]; ok && !parseBool(value) {
		return true
	}
	for _, key := range []string{"error", "errorMessage", "unavailableMessage"} {
		if strings.TrimSpace(shared.StringArg(result, key, "")) != "" {
			return true
		}
	}
	return false
}

func (c *codexCompat) codexCall(ctx context.Context, method string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	if c.transport() != "ws" {
		return c.rpcCall(ctx, method, params, sink)
	}
	return c.callWSRPCWithInitialize(ctx, method, params, sink)
}

func (c *codexCompat) callWSRPCWithInitialize(ctx context.Context, method string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	return c.withInitializedCodexWS(ctx, func(conn *websocket.Conn) (map[string]any, error) {
		return c.writeAndReadWSRPC(ctx, conn, method, params, sink)
	})
}

func (c *codexCompat) withInitializedCodexWS(ctx context.Context, run func(*websocket.Conn) (map[string]any, error)) (map[string]any, error) {
	headers := http.Header{}
	if c.authHeader != "" {
		headers.Set("Authorization", c.authHeader)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.endpoint, headers)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	if _, err := c.writeAndReadWSRPC(ctx, conn, "initialize", codexInitializeParams(), nil); err != nil {
		return nil, err
	}
	return run(conn)
}

func (c *codexCompat) writeAndReadWSRPC(ctx context.Context, conn *websocket.Conn, method string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  method,
		"params":  params,
	}
	if err := conn.WriteJSON(request); err != nil {
		return nil, err
	}

	collector := &externalACPNotificationCollector{}
	var pendingTurn map[string]any
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}

		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return nil, fmt.Errorf("failed to decode websocket rpc response: %w", err)
		}

		methodName := strings.TrimSpace(shared.StringArg(decoded, "method", ""))
		if methodName != "" {
			collector.observe(decoded)
			if isExternalSessionUpdateMethod(methodName) && sink != nil {
				update := shared.AsMap(decoded["params"])
				if len(update) > 0 {
					sink(update)
				}
			}
			if pendingTurn != nil && isCodexTurnCompletedNotification(decoded, pendingTurn) {
				return collector.apply(pendingTurn), nil
			}
			continue
		}

		if fmt.Sprintf("%v", decoded["id"]) != requestID {
			continue
		}

		result, err := parseExternalRPCResult(decoded)
		if err != nil {
			return nil, err
		}
		if method == "turn/start" && isCodexTurnInProgress(result) {
			pendingTurn = collector.apply(result)
			continue
		}
		return collector.apply(result), nil
	}
}

func isCodexTurnInProgress(result map[string]any) bool {
	if result == nil {
		return false
	}
	turn := shared.AsMap(result["turn"])
	if len(turn) == 0 {
		return false
	}
	status := strings.TrimSpace(shared.StringArg(turn, "status", ""))
	return status == "" || strings.EqualFold(status, "inProgress") || strings.EqualFold(status, "running")
}

func isCodexTurnCompletedNotification(notification map[string]any, pendingTurn map[string]any) bool {
	if notification == nil || pendingTurn == nil {
		return false
	}
	if strings.TrimSpace(shared.StringArg(notification, "method", "")) != "turn/completed" {
		return false
	}
	params := shared.AsMap(notification["params"])
	turn := shared.AsMap(params["turn"])
	if len(turn) == 0 {
		return true
	}
	pending := shared.AsMap(pendingTurn["turn"])
	pendingID := strings.TrimSpace(shared.StringArg(pending, "id", ""))
	completedID := strings.TrimSpace(shared.StringArg(turn, "id", ""))
	if pendingID == "" || completedID == "" || pendingID == completedID {
		pendingTurn["turn"] = turn
		return true
	}
	return false
}

func (c *codexCompat) rememberThread(sessionID string, threadID string, codexThreadID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sessionID != "" {
		c.threads[sessionID] = codexThreadID
	}
	if threadID != "" {
		c.threads[threadID] = codexThreadID
	}
}

func (c *codexCompat) lookupThread(sessionID string, threadID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sessionID != "" {
		if value := strings.TrimSpace(c.threads[sessionID]); value != "" {
			return value
		}
	}
	if threadID != "" {
		return strings.TrimSpace(c.threads[threadID])
	}
	return ""
}

func codexThreadStartParams(params map[string]any) map[string]any {
	result := map[string]any{}
	if cwd := strings.TrimSpace(shared.StringArg(params, "workingDirectory", "")); cwd != "" {
		result["cwd"] = cwd
	}
	return result
}

func codexInitializeParams() map[string]any {
	return map[string]any{
		"clientInfo": map[string]any{
			"name":    "xworkmate-bridge",
			"version": "1.1.0",
		},
	}
}

func codexUserInput(params map[string]any) []any {
	input := map[string]any{
		"type": "text",
		"text": shared.StringArg(params, "taskPrompt", ""),
	}
	if attachments := anyList(params["attachments"]); len(attachments) > 0 {
		input["attachments"] = attachments
	}
	return []any{input}
}

func codexThreadIDFromResult(result map[string]any) string {
	for _, key := range []string{"threadId", "id"} {
		if value := strings.TrimSpace(shared.StringArg(result, key, "")); value != "" {
			return value
		}
	}
	thread := shared.AsMap(result["thread"])
	for _, key := range []string{"id", "threadId"} {
		if value := strings.TrimSpace(shared.StringArg(thread, key, "")); value != "" {
			return value
		}
	}
	return ""
}

func anyList(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, item)
		}
		return result
	default:
		return nil
	}
}

func (c *externalACPCompat) StartSession(ctx context.Context, sessionID string, threadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	return c.rpcCall(ctx, "session.start", params, sink)
}

func (c *externalACPCompat) SendMessage(ctx context.Context, sessionID string, threadID string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	return c.rpcCall(ctx, "session.message", params, sink)
}

func (c *externalACPCompat) CancelSession(ctx context.Context, sessionID string) error {
	_, err := c.rpcCall(ctx, "session.cancel", map[string]any{"sessionId": sessionID}, nil)
	return err
}

func (c *externalACPCompat) CloseSession(ctx context.Context, sessionID string) error {
	_, err := c.rpcCall(ctx, "session.close", map[string]any{"sessionId": sessionID}, nil)
	return err
}

func (c *externalACPCompat) rpcCall(ctx context.Context, method string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	switch c.transport() {
	case "ws":
		return c.callWSRPC(ctx, method, params, sink)
	default:
		return c.callHTTPRPC(ctx, method, params)
	}
}

func (c *externalACPCompat) transport() string {
	parsed, err := url.Parse(strings.TrimSpace(c.endpoint))
	if err != nil {
		return "http"
	}
	switch strings.ToLower(parsed.Scheme) {
	case "ws", "wss":
		return "ws"
	default:
		return "http"
	}
}

func (c *externalACPCompat) callHTTPRPC(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	requestBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  method,
		"params":  params,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}

	response, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return nil, fmt.Errorf("rpc request failed (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("failed to decode rpc response: %w", err)
	}
	return parseExternalRPCResult(decoded)
}

func (c *externalACPCompat) callWSRPC(ctx context.Context, method string, params map[string]any, sink SessionNotificationSink) (map[string]any, error) {
	headers := http.Header{}
	if c.authHeader != "" {
		headers.Set("Authorization", c.authHeader)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.endpoint, headers)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  method,
		"params":  params,
	}
	if err := conn.WriteJSON(request); err != nil {
		return nil, err
	}

	collector := &externalACPNotificationCollector{}
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}

		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return nil, fmt.Errorf("failed to decode websocket rpc response: %w", err)
		}

		methodName := strings.TrimSpace(shared.StringArg(decoded, "method", ""))
		if methodName != "" {
			collector.observe(decoded)
			if isExternalSessionUpdateMethod(methodName) && sink != nil {
				update := shared.AsMap(decoded["params"])
				if len(update) > 0 {
					sink(update)
				}
			}
			continue
		}

		if fmt.Sprintf("%v", decoded["id"]) != requestID {
			continue
		}

		result, err := parseExternalRPCResult(decoded)
		if err != nil {
			return nil, err
		}
		return collector.apply(result), nil
	}
}

func isExternalSessionUpdateMethod(method string) bool {
	switch strings.TrimSpace(method) {
	case "session.update", "acp.session.update", "session/update":
		return true
	default:
		return strings.HasPrefix(method, "item/") || strings.HasPrefix(method, "turn/")
	}
}

func parseExternalRPCResult(decoded map[string]any) (map[string]any, error) {
	if decoded == nil {
		return map[string]any{}, nil
	}
	if errPayload := shared.AsMap(decoded["error"]); len(errPayload) > 0 {
		message := strings.TrimSpace(shared.StringArg(errPayload, "message", "upstream rpc error"))
		if message == "" {
			message = "upstream rpc error"
		}
		return nil, fmt.Errorf("%s", message)
	}
	result := shared.AsMap(decoded["result"])
	if len(result) > 0 {
		return result, nil
	}
	if ok, _ := decoded["ok"].(bool); ok {
		payload := shared.AsMap(decoded["payload"])
		if len(payload) > 0 {
			return payload, nil
		}
	}
	return map[string]any{}, nil
}
