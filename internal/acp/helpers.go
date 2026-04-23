package acp

import (
	"fmt"
	"net/url"
	"strings"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/shared"
	"xworkmate-bridge/internal/skills"
)

func resolveSingleAgentForwardEndpoint(provider syncedProvider) string {
	endpoint := strings.TrimSpace(provider.Endpoint)
	if endpoint == "" {
		return ""
	}

	// For compatibility with tests expecting specific protocol mappings
	if provider.ProviderID == "opencode" || provider.ProviderID == "gemini" {
		if strings.HasPrefix(endpoint, "ws") {
			endpoint = "http" + strings.TrimPrefix(endpoint, "ws")
		}
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}

	isWS := strings.HasPrefix(parsed.Scheme, "ws")
	isHTTP := strings.HasPrefix(parsed.Scheme, "http")

	path := strings.TrimRight(parsed.Path, "/")

	if isWS && !strings.Contains(path, "/acp") {
		parsed.Path = path + "/acp"
	} else if isHTTP && !strings.Contains(path, "/acp/rpc") {
		parsed.Path = path + "/acp/rpc"
	}

	return parsed.String()
}

func normalizeAuthorizationHeader(raw string) string {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(normalized), "bearer ") {
		return normalized
	}
	return "Bearer " + normalized
}

type externalACPNotificationCollector struct {
	deltas      strings.Builder
	lastMessage string
	turnID      string
}

func (c *externalACPNotificationCollector) observe(notification map[string]any) {
	method := strings.TrimSpace(stringValue(notification["method"]))
	if method != "session.update" && method != "acp.session.update" && method != "session/update" {
		return
	}
	params := asMap(notification["params"])
	if len(params) == 0 {
		return
	}
	if turnID := strings.TrimSpace(stringValue(params["turnId"])); turnID != "" {
		c.turnID = turnID
	}
	updateText := extractExternalACPNotificationText(notification)
	if updateText == "" {
		return
	}
	if c.deltas.Len() > 0 {
		c.deltas.WriteString("\n")
	}
	c.deltas.WriteString(updateText)
	c.lastMessage = updateText
}

func (c *externalACPNotificationCollector) apply(result map[string]any) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	text := strings.TrimSpace(c.deltas.String())
	if isGenericHermesAckText(text) {
		text = ""
	}
	if text == "" {
		text = strings.TrimSpace(c.lastMessage)
		if isGenericHermesAckText(text) {
			text = ""
		}
	}
	if text == "" {
		for _, candidate := range []string{
			strings.TrimSpace(stringValue(result["output"])),
			strings.TrimSpace(stringValue(result["summary"])),
			strings.TrimSpace(stringValue(result["message"])),
		} {
			if candidate == "" || isGenericHermesAckText(candidate) {
				continue
			}
			text = candidate
			break
		}
	}
	if text != "" {
		result["output"] = text
		result["summary"] = text
	}
	if _, exists := result["turnId"]; !exists && strings.TrimSpace(c.turnID) != "" {
		result["turnId"] = strings.TrimSpace(c.turnID)
	}
	return result
}

func isGenericHermesAckText(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "", "ok", "session started", "single-agent completed":
		return true
	default:
		return false
	}
}

func extractExternalACPNotificationText(notification map[string]any) string {
	if notification == nil {
		return ""
	}
	payload := asMap(notification["params"])
	if len(payload) == 0 {
		payload = notification
	}
	update := asMap(payload["update"])
	if len(update) == 0 {
		update = payload
	}
	updateKind := strings.TrimSpace(stringValue(update["sessionUpdate"]))
	switch updateKind {
	case "available_commands_update":
		return ""
	case "session_started", "session completed", "single-agent completed":
		return ""
	}
	if text := extractExternalACPTextValue(update); text != "" {
		return text
	}
	if text := extractExternalACPTextValue(payload); text != "" {
		return text
	}
	return ""
}

func extractExternalACPTextValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		var builder strings.Builder
		for _, key := range []string{"text", "message", "content", "delta", "value"} {
			if text := extractExternalACPTextValue(v[key]); text != "" {
				if builder.Len() > 0 {
					builder.WriteString(" ")
				}
				builder.WriteString(text)
			}
		}
		if builder.Len() > 0 {
			return strings.TrimSpace(builder.String())
		}
		for key, child := range v {
			if key == "text" || key == "message" || key == "content" || key == "delta" || key == "value" || key == "sessionId" || key == "session_id" || key == "sessionUpdate" || key == "session_update" {
				continue
			}
			if text := extractExternalACPTextValue(child); text != "" {
				if builder.Len() > 0 {
					builder.WriteString(" ")
				}
				builder.WriteString(text)
			}
		}
		return strings.TrimSpace(builder.String())
	case []any:
		var parts []string
		for _, child := range v {
			if text := extractExternalACPTextValue(child); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	default:
		return ""
	}
}

func parseGatewayRuntimeStringSlice(value any) []string {
	list, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			return append([]string(nil), typed...)
		}
		return nil
	}
	result := make([]string, 0, len(list))
	for _, item := range list {
		text := strings.TrimSpace(stringValue(item))
		if text == "" {
			continue
		}
		result = append(result, text)
	}
	return result
}

func parseBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return shared.BoolArg(typed, false)
	case float64:
		return typed != 0
	case int:
		return typed != 0
	default:
		return false
	}
}

func parsePositiveInt(value any) int {
	switch typed := value.(type) {
	case int:
		if typed > 0 {
			return typed
		}
	case int64:
		if typed > 0 {
			return int(typed)
		}
	case float64:
		if typed > 0 {
			return int(typed)
		}
	case string:
		return shared.IntArg(typed, 0)
	}
	return 0
}

func resolveGatewayReportedRemoteAddress(
	server *Server,
	request gatewayruntime.ConnectRequest,
) string {
	if strings.TrimSpace(strings.ToLower(request.Mode)) != "openclaw" {
		return ""
	}
	gatewayURL := resolveURL(server.config.Upstream.GatewayURL, "GATEWAY_RPC_URL")
	if gatewayURL == "" {
		return "127.0.0.1:18789"
	}
	return publicEndpointAddressLabel(gatewayURL)
}

func publicEndpointAddressLabel(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.TrimSpace(parsed.Hostname()) == "" {
		return ""
	}
	host := strings.TrimSpace(parsed.Hostname())
	port := strings.TrimSpace(parsed.Port())
	if port == "" {
		switch strings.TrimSpace(strings.ToLower(parsed.Scheme)) {
		case "https", "wss":
			port = "443"
		case "http", "ws":
			port = "80"
		}
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func asMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	if typed, ok := value.(map[string]interface{}); ok {
		return typed
	}
	return nil
}

func parseSkillsCandidates(raw []any) []skills.Candidate {
	result := make([]skills.Candidate, 0, len(raw))
	for _, item := range raw {
		m := asMap(item)
		if m == nil {
			continue
		}
		result = append(result, skills.Candidate{
			ID:          shared.StringArg(m, "id", ""),
			Label:       shared.StringArg(m, "label", ""),
			Description: shared.StringArg(m, "description", ""),
			Installed:   parseBool(m["installed"]),
		})
	}
	return result
}
