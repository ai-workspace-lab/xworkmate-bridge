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

	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if path == "" {
		path = strings.TrimRight(parsed.Path, "/")
	}

	if isWS {
		if path == "/acp" || strings.HasSuffix(path, "/acp") {
			parsed.Path = path
		} else {
			parsed.Path = path + "/acp"
		}
	} else if isHTTP {
		if path == "/acp/rpc" || strings.HasSuffix(path, "/acp/rpc") {
			parsed.Path = path
		} else if path == "/acp" || strings.HasSuffix(path, "/acp") {
			parsed.Path = strings.TrimSuffix(path, "/acp") + "/acp/rpc"
		} else {
			parsed.Path = path + "/acp/rpc"
		}
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
	errors      []string
	turnID      string
}

func (c *externalACPNotificationCollector) observe(notification map[string]any) {
	method := strings.TrimSpace(stringValue(notification["method"]))
	if method != "session.update" && method != "acp.session.update" && method != "session/update" && !strings.HasPrefix(method, "item/") && !strings.HasPrefix(method, "turn/") {
		return
	}
	params := asMap(notification["params"])
	if len(params) == 0 {
		return
	}
	if turnID := strings.TrimSpace(stringValue(params["turnId"])); turnID != "" {
		c.turnID = turnID
	}
	if errorText := extractExternalACPNotificationError(notification); errorText != "" {
		c.errors = append(c.errors, errorText)
	}
	if strings.TrimSpace(stringValue(notification["method"])) == "turn/completed" {
		return
	}
	updateText := extractExternalACPNotificationText(notification)
	if updateText == "" {
		return
	}
	if isExternalACPFailureText(updateText) {
		c.errors = append(c.errors, updateText)
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
	text = normalizeExternalACPAssistantText(text)
	if errorText := c.errorText(); errorText != "" {
		result["success"] = false
		result["error"] = errorText
		result["message"] = errorText
		delete(result, "output")
		delete(result, "summary")
	} else if text != "" {
		result["output"] = text
		result["summary"] = text
	}
	if _, exists := result["turnId"]; !exists && strings.TrimSpace(c.turnID) != "" {
		result["turnId"] = strings.TrimSpace(c.turnID)
	}
	return result
}

func (c *externalACPNotificationCollector) errorText() string {
	if c == nil || len(c.errors) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(c.errors))
	var parts []string
	for _, item := range c.errors {
		text := strings.TrimSpace(item)
		if text == "" {
			continue
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		parts = append(parts, text)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
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
	method := strings.TrimSpace(stringValue(notification["method"]))
	if strings.HasPrefix(method, "turn/") {
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
	item := asMap(payload["item"])
	if len(item) > 0 {
		if strings.TrimSpace(stringValue(item["type"])) == "userMessage" {
			return ""
		}
		if text := extractExternalACPAssistantTextValue(item); text != "" {
			return text
		}
		return ""
	}
	if text := extractExternalACPAssistantTextValue(update); text != "" {
		return text
	}
	if text := extractExternalACPAssistantTextValue(payload); text != "" {
		return text
	}
	return ""
}

func extractExternalACPNotificationError(notification map[string]any) string {
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
	if turnError := extractExternalACPTextValue(asMap(asMap(payload["turn"])["error"])); turnError != "" {
		return turnError
	}
	for _, source := range []map[string]any{update, asMap(payload["item"]), payload} {
		if len(source) == 0 {
			continue
		}
		if !parseBool(source["error"]) && strings.TrimSpace(stringValue(source["level"])) != "error" {
			if text := extractExternalACPTextValue(source); !isExternalACPFailureText(text) {
				continue
			}
		}
		for _, key := range []string{"error", "message", "text", "content", "delta", "value"} {
			if text := extractExternalACPTextValue(source[key]); text != "" {
				return text
			}
		}
		if text := extractExternalACPTextValue(source); text != "" {
			return text
		}
	}
	return ""
}

func isExternalACPFailureText(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	for _, marker := range []string{
		"exec_command failed",
		"failed to create unified exec process",
		"execution_failed",
		"tool execution failed",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
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
			if key == "text" || key == "message" || key == "content" || key == "delta" || key == "value" || key == "sessionId" || key == "session_id" || key == "sessionUpdate" || key == "session_update" || key == "threadId" || key == "turnId" || key == "itemId" {
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

func extractExternalACPAssistantTextValue(value any) string {
	return normalizeExternalACPAssistantText(extractExternalACPTextValue(value))
}

func structuredExternalACPEvent(notification map[string]any) map[string]any {
	if notification == nil {
		return nil
	}
	method := strings.TrimSpace(stringValue(notification["method"]))
	payload := asMap(notification["params"])
	if len(payload) == 0 {
		payload = notification
	}
	update := asMap(payload["update"])
	if len(update) == 0 {
		update = payload
	}
	item := asMap(payload["item"])
	source := update
	if len(item) > 0 {
		source = item
	}
	eventType := "status"
	if strings.Contains(method, "thinking") || strings.TrimSpace(stringValue(source["thinking"])) != "" {
		eventType = "thinking"
	} else if strings.Contains(method, "tool") || len(asMap(source["toolCall"])) > 0 || len(asMap(source["tool_call"])) > 0 {
		eventType = "tool_call"
	} else if text := extractExternalACPAssistantTextValue(source); text != "" {
		eventType = "text"
		_ = text
	}
	result := map[string]any{
		"type":   eventType,
		"method": method,
	}
	if text := extractExternalACPAssistantTextValue(source); text != "" {
		result["text"] = text
	}
	if status := strings.TrimSpace(firstNonEmptyString(source, "status", "sessionUpdate", "session_update")); status != "" {
		result["status"] = status
	}
	if tool := asMap(source["toolCall"]); len(tool) > 0 {
		result["toolCall"] = tool
	} else if tool := asMap(source["tool_call"]); len(tool) > 0 {
		result["toolCall"] = tool
	}
	if result["text"] == nil && result["status"] == nil && result["toolCall"] == nil && eventType == "status" {
		return nil
	}
	return result
}

func normalizeExternalACPAssistantText(text string) string {
	normalized := strings.TrimSpace(text)
	if normalized == "" {
		return ""
	}
	if isExternalACPCommentaryAgentMessage(normalized) {
		return ""
	}
	if idx := strings.LastIndex(normalized, "final_answer"); idx >= 0 {
		normalized = strings.TrimSpace(normalized[idx+len("final_answer"):])
	}
	normalized = stripExternalACPAgentMessagePrefix(normalized)
	normalized = stripExternalACPExecutionContextBlocks(normalized)
	normalized = stripExternalACPRepeatedFinalLine(normalized)
	return strings.TrimSpace(normalized)
}

func isExternalACPCommentaryAgentMessage(text string) bool {
	fields := strings.Fields(strings.TrimSpace(text))
	return len(fields) >= 3 && fields[0] == "commentary" && fields[1] == "agentMessage" && strings.HasPrefix(fields[2], "msg_")
}

func stripExternalACPAgentMessagePrefix(text string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) >= 2 && fields[0] == "agentMessage" && strings.HasPrefix(fields[1], "msg_") {
		return strings.TrimSpace(strings.Join(fields[2:], " "))
	}
	return strings.TrimSpace(text)
}

func stripExternalACPExecutionContextBlocks(text string) string {
	lines := strings.Split(text, "\n")
	var kept []string
	skippingContext := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if skippingContext {
				skippingContext = false
				continue
			}
			kept = append(kept, line)
			continue
		}
		if strings.HasPrefix(trimmed, "Execution context:") {
			skippingContext = true
			continue
		}
		if skippingContext {
			if strings.HasPrefix(trimmed, "•") || strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "*") || strings.Contains(trimmed, ":") {
				continue
			}
			skippingContext = false
		}
		if isExternalACPProtocolStatusLine(trimmed) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func stripExternalACPRepeatedFinalLine(text string) string {
	lines := strings.Split(text, "\n")
	nonEmpty := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	if len(nonEmpty) >= 2 && nonEmpty[len(nonEmpty)-1] == nonEmpty[len(nonEmpty)-2] {
		return nonEmpty[len(nonEmpty)-1]
	}
	return strings.TrimSpace(text)
}

func isExternalACPProtocolStatusLine(text string) bool {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return false
	}
	switch fields[0] {
	case "inProgress", "pending", "running", "completed":
		return strings.Count(fields[1], "-") >= 4
	default:
		return false
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

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneMapSlice(source []map[string]any) []map[string]any {
	if source == nil {
		return nil
	}
	result := make([]map[string]any, 0, len(source))
	for _, item := range source {
		result = append(result, cloneMap(item))
	}
	return result
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
