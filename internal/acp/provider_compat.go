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

type codexCompat struct{ *externalACPCompat }
type opencodeCompat struct{ *externalACPCompat }
type geminiCompat struct{ *externalACPCompat }
type hermesCompat struct{ *externalACPCompat }

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
		return &codexCompat{externalACPCompat: base}
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
	defer response.Body.Close()

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
		return false
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
