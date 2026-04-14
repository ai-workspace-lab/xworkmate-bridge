package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
)

const (
	inboundAuthorizationHeaderKey = "bridgeAuthorizationHeader"
	externalACPHTTPTimeout        = 5 * time.Minute
)

func buildResolvedExecutionParams(
	params map[string]any,
	resolved router.Result,
) map[string]any {
	next := make(map[string]any, len(params)+8)
	for key, value := range params {
		next[key] = value
	}
	switch resolved.ResolvedExecutionTarget {
	case router.ExecutionTargetGateway:
		next["mode"] = router.ExecutionTargetGatewayChat
	case router.ExecutionTargetMultiAgent:
		next["mode"] = router.ExecutionTargetMultiAgent
	default:
		next["mode"] = router.ExecutionTargetSingleAgent
	}
	if strings.TrimSpace(resolved.ResolvedProviderID) != "" {
		next["provider"] = strings.TrimSpace(resolved.ResolvedProviderID)
	}
	if strings.TrimSpace(resolved.ResolvedGatewayProviderID) != "" {
		next["gatewayProvider"] = strings.TrimSpace(resolved.ResolvedGatewayProviderID)
	}
	if strings.TrimSpace(resolved.ResolvedModel) != "" {
		next["model"] = strings.TrimSpace(resolved.ResolvedModel)
	}
	if len(resolved.ResolvedSkills) > 0 {
		next["selectedSkills"] = append([]string(nil), resolved.ResolvedSkills...)
	}
	next["resolvedExecutionTarget"] = resolved.ResolvedExecutionTarget
	next["resolvedProviderId"] = resolved.ResolvedProviderID
	next["resolvedGatewayProviderId"] = resolved.ResolvedGatewayProviderID
	next["resolvedModel"] = resolved.ResolvedModel
	next["resolvedSkills"] = append([]string(nil), resolved.ResolvedSkills...)
	return next
}

func injectInboundAuthorizationHeader(params map[string]any, authorization string) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	authorization = strings.TrimSpace(authorization)
	if authorization != "" {
		params[inboundAuthorizationHeaderKey] = authorization
	}
	return params
}

func (s *Server) runGateway(
	ctx context.Context,
	method string,
	session *session,
	params map[string]any,
	turnID string,
	notify func(map[string]any),
) taskResult {
	_ = ctx
	gatewayProvider := strings.TrimSpace(shared.StringArg(params, "gatewayProvider", ""))
	if gatewayProvider == "" {
		gatewayProvider = router.GatewayProviderOpenClaw
	}
	result := s.gateway.RequestByMode(
		gatewayProvider,
		method,
		params,
		2*time.Minute,
		notify,
	)
	if !result.OK {
		errMessage := strings.TrimSpace(shared.StringArg(result.Error, "message", "gateway execution failed"))
		s.emitSessionUpdate(session, notify, turnID, map[string]any{
			"type":    "status",
			"event":   "completed",
			"message": errMessage,
			"pending": false,
			"error":   true,
		})
		return taskResult{
			response: map[string]any{
				"success": false,
				"error":   errMessage,
				"turnId":  turnID,
				"mode":    router.ExecutionTargetGatewayChat,
			},
		}
	}
	payload := asMap(result.Payload)
	if len(payload) == 0 {
		payload = map[string]any{
			"success": true,
			"turnId":  turnID,
			"mode":    router.ExecutionTargetGatewayChat,
		}
	}
	if _, ok := payload["turnId"]; !ok {
		payload["turnId"] = turnID
	}
	if _, ok := payload["mode"]; !ok {
		payload["mode"] = router.ExecutionTargetGatewayChat
	}
	return taskResult{response: payload}
}

func (s *Server) runSingleAgentViaExternalProvider(
	ctx context.Context,
	provider syncedProvider,
	method string,
	params map[string]any,
	notify func(map[string]any),
) (map[string]any, error) {
	endpoint := resolveSingleAgentForwardEndpoint(provider)
	if endpoint == "" {
		return nil, fmt.Errorf("external provider endpoint is missing")
	}
	forwardParams := sanitizeExternalACPParams(method, params)
	collector := &externalACPNotificationCollector{}
	combinedNotify := func(message map[string]any) {
		collector.observe(message)
		if notify != nil {
			notify(message)
		}
	}
	authorization := firstNonEmptyString(
		strings.TrimSpace(provider.AuthorizationHeader),
		strings.TrimSpace(shared.StringArg(params, inboundAuthorizationHeaderKey, "")),
	)
	response, err := requestExternalACP(
		ctx,
		endpoint,
		authorization,
		method,
		forwardParams,
		combinedNotify,
	)
	if err != nil {
		return nil, err
	}
	result := asMap(response["result"])
	if len(result) == 0 {
		result = response
	}
	return enrichSingleAgentResultArtifacts(collector.apply(result), forwardParams), nil
}

func (s *Server) probeExternalProvider(
	ctx context.Context,
	provider syncedProvider,
	params map[string]any,
) (map[string]any, error) {
	endpoint := resolveSingleAgentForwardEndpoint(provider)
	if endpoint == "" {
		return nil, fmt.Errorf("external provider endpoint is missing")
	}
	authorization := firstNonEmptyString(
		strings.TrimSpace(provider.AuthorizationHeader),
		strings.TrimSpace(shared.StringArg(params, inboundAuthorizationHeaderKey, "")),
	)
	response, err := requestExternalACP(
		ctx,
		endpoint,
		authorization,
		"acp.capabilities",
		map[string]any{},
		nil,
	)
	if err != nil {
		return nil, err
	}
	result := asMap(response["result"])
	if len(result) == 0 {
		return nil, fmt.Errorf("external provider probe missing result payload")
	}
	return result, nil
}

func resolveSingleAgentForwardEndpoint(provider syncedProvider) string {
	return strings.TrimSpace(provider.Endpoint)
}

func sanitizeExternalACPParams(method string, params map[string]any) map[string]any {
	if len(params) == 0 {
		return map[string]any{}
	}
	next := make(map[string]any, len(params))
	for key, value := range params {
		next[key] = value
	}
	// Internal routing/runtime fields must not leak into external provider payloads.
	delete(next, "metadata")
	delete(next, "resolvedExecutionTarget")
	delete(next, "resolvedProviderId")
	delete(next, "resolvedGatewayProviderId")
	delete(next, "resolvedModel")
	delete(next, "resolvedSkills")
	normalizedMethod := strings.TrimSpace(method)
	// Bridge-compatible upstream ACP services may need the inbound bearer token
	// in params so they can forward auth again during nested provider execution.
	if normalizedMethod != "session.start" && normalizedMethod != "session.message" {
		delete(next, inboundAuthorizationHeaderKey)
	}
	// Gateway-only fields are irrelevant in ACP single-agent forwarding.
	if normalizedMethod == "session.start" || normalizedMethod == "session.message" {
		delete(next, "executionTarget")
		delete(next, "agentId")
	}
	return next
}

func requestExternalACP(
	ctx context.Context,
	endpoint,
	authorization,
	method string,
	params map[string]any,
	notify func(map[string]any),
) (map[string]any, error) {
	parsed, err := httpOrWebsocketEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	switch parsed.Scheme {
	case "http", "https":
		return requestExternalACPHTTP(ctx, parsed, authorization, method, params)
	default:
		return requestExternalACPWebSocket(ctx, parsed, authorization, method, params, notify)
	}
}

func requestExternalACPHTTP(
	ctx context.Context,
	endpoint *urlSpec,
	authorization,
	method string,
	params map[string]any,
) (map[string]any, error) {
	requestBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("req-%d", time.Now().UnixNano()),
		"method":  method,
		"params":  params,
	})
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint.httpRPCEndpoint(),
		strings.NewReader(string(requestBody)),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	if normalized := normalizeAuthorizationHeader(authorization); normalized != "" {
		req.Header.Set("Authorization", normalized)
	}
	response, err := (&http.Client{Timeout: externalACPHTTPTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = response.Body.Close()
	}()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	if errPayload := asMap(decoded["error"]); len(errPayload) > 0 {
		return nil, fmt.Errorf(
			"%s",
			strings.TrimSpace(shared.StringArg(errPayload, "message", "external ACP request failed")),
		)
	}
	return decoded, nil
}

func normalizeAuthorizationHeader(raw string) string {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return ""
	}
	if strings.Contains(normalized, " ") {
		return normalized
	}
	return "Bearer " + normalized
}

type externalACPNotificationCollector struct {
	deltas           strings.Builder
	lastMessage      string
	turnID           string
	workingDirectory string
}

func (c *externalACPNotificationCollector) observe(notification map[string]any) {
	method := strings.TrimSpace(shared.StringArg(notification, "method", ""))
	if method != "session.update" && method != "acp.session.update" {
		return
	}
	params := asMap(notification["params"])
	if len(params) == 0 {
		return
	}
	if turnID := strings.TrimSpace(shared.StringArg(params, "turnId", "")); turnID != "" {
		c.turnID = turnID
	}
	for _, key := range []string{"resolvedWorkingDirectory", "effectiveWorkingDirectory", "workingDirectory"} {
		if workingDirectory := strings.TrimSpace(shared.StringArg(params, key, "")); workingDirectory != "" {
			c.workingDirectory = workingDirectory
			break
		}
	}
	if delta := strings.TrimSpace(shared.StringArg(params, "delta", "")); delta != "" {
		if c.deltas.Len() > 0 {
			c.deltas.WriteString("\n")
		}
		c.deltas.WriteString(delta)
	}
	message := strings.TrimSpace(shared.StringArg(params, "message", ""))
	if message == "" {
		message = strings.TrimSpace(shared.StringArg(asMap(params["message"]), "content", ""))
	}
	if message != "" && message != "session started" && message != "single-agent completed" {
		c.lastMessage = message
	}
}

func (c *externalACPNotificationCollector) apply(result map[string]any) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	text := strings.TrimSpace(shared.StringArg(result, "output", ""))
	if text == "" {
		text = strings.TrimSpace(shared.StringArg(result, "summary", ""))
	}
	if text == "" {
		text = strings.TrimSpace(shared.StringArg(result, "message", ""))
	}
	if text == "" {
		text = strings.TrimSpace(c.deltas.String())
	}
	if text == "" {
		text = strings.TrimSpace(c.lastMessage)
	}
	if text != "" {
		if _, exists := result["output"]; !exists {
			result["output"] = text
		}
		if _, exists := result["summary"]; !exists {
			result["summary"] = text
		}
	}
	if _, exists := result["turnId"]; !exists && strings.TrimSpace(c.turnID) != "" {
		result["turnId"] = strings.TrimSpace(c.turnID)
	}
	if _, exists := result["resolvedWorkingDirectory"]; !exists && strings.TrimSpace(c.workingDirectory) != "" {
		result["resolvedWorkingDirectory"] = strings.TrimSpace(c.workingDirectory)
	}
	return result
}

func enrichSingleAgentResultArtifacts(result map[string]any, requestParams map[string]any) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	remoteWorkingDirectory := firstNonEmptyString(
		shared.StringArg(result, "remoteWorkingDirectory", ""),
		shared.StringArg(asMap(result["remoteExecution"]), "remoteWorkingDirectory", ""),
		shared.StringArg(result, "resolvedWorkingDirectory", ""),
		shared.StringArg(result, "effectiveWorkingDirectory", ""),
		shared.StringArg(requestParams, "workingDirectory", ""),
	)
	remoteWorkspaceRefKind := firstNonEmptyString(
		shared.StringArg(result, "remoteWorkspaceRefKind", ""),
		shared.StringArg(asMap(result["remoteExecution"]), "remoteWorkspaceRefKind", ""),
		"remotePath",
	)
	if strings.TrimSpace(shared.StringArg(result, "resultSummary", "")) == "" {
		if summary := firstNonEmptyString(
			shared.StringArg(result, "summary", ""),
			shared.StringArg(result, "output", ""),
			shared.StringArg(result, "message", ""),
		); summary != "" {
			result["resultSummary"] = summary
		}
	}
	result["remoteWorkingDirectory"] = remoteWorkingDirectory
	result["remoteWorkspaceRefKind"] = remoteWorkspaceRefKind
	result["remoteExecution"] = map[string]any{
		"remoteWorkingDirectory": remoteWorkingDirectory,
		"remoteWorkspaceRefKind": remoteWorkspaceRefKind,
		"provider":               shared.StringArg(result, "provider", ""),
		"turnId":                 shared.StringArg(result, "turnId", ""),
	}
	if len(asSlice(result["artifacts"])) == 0 {
		result["artifacts"] = collectInlineArtifactsPayload(requestParams, result)
	}
	return result
}

func collectInlineArtifactsPayload(requestParams, result map[string]any) []map[string]any {
	roots := []string{
		shared.StringArg(requestParams, "workingDirectory", ""),
		shared.StringArg(result, "resolvedWorkingDirectory", ""),
		shared.StringArg(result, "effectiveWorkingDirectory", ""),
	}
	seen := map[string]struct{}{}
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		entries := buildArtifactsForRoot(root)
		if len(entries) > 0 {
			return entries
		}
	}
	return []map[string]any{}
}

func buildArtifactsForRoot(root string) []map[string]any {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return []map[string]any{}
	}
	type candidate struct {
		absolute string
		relative string
		modTime  time.Time
		size     int64
	}
	const maxFiles = 24
	const maxInlineBytes = 2 * 1024 * 1024
	ignoredDirs := map[string]struct{}{
		".git": {}, ".dart_tool": {}, "build": {}, "node_modules": {},
	}
	candidates := make([]candidate, 0, maxFiles)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if _, ignored := ignoredDirs[d.Name()]; ignored && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxInlineBytes {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relative = filepath.ToSlash(strings.TrimSpace(relative))
		if relative == "" || strings.HasPrefix(relative, "../") {
			return nil
		}
		candidates = append(candidates, candidate{
			absolute: path,
			relative: relative,
			modTime:  info.ModTime(),
			size:     info.Size(),
		})
		return nil
	})
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	if len(candidates) > maxFiles {
		candidates = candidates[:maxFiles]
	}
	artifacts := make([]map[string]any, 0, len(candidates))
	for _, item := range candidates {
		content, err := os.ReadFile(item.absolute)
		if err != nil {
			continue
		}
		contentType := mime.TypeByExtension(filepath.Ext(item.absolute))
		if strings.TrimSpace(contentType) == "" {
			contentType = "application/octet-stream"
		}
		encoding := "base64"
		payload := base64.StdEncoding.EncodeToString(content)
		if isInlineTextArtifact(item.relative, contentType) {
			encoding = "utf8"
			payload = string(content)
		}
		artifacts = append(artifacts, map[string]any{
			"relativePath": item.relative,
			"label":        filepath.Base(item.absolute),
			"contentType":  contentType,
			"encoding":     encoding,
			"content":      payload,
			"sizeBytes":    item.size,
		})
	}
	return artifacts
}

func isInlineTextArtifact(path, contentType string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/") {
		return true
	}
	switch ext {
	case ".md", ".markdown", ".txt", ".log", ".json", ".yaml", ".yml", ".csv", ".html", ".htm":
		return true
	default:
		return false
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func asSlice(value any) []any {
	if value == nil {
		return nil
	}
	items, _ := value.([]any)
	return items
}

func requestExternalACPWebSocket(
	ctx context.Context,
	endpoint *urlSpec,
	authorization,
	method string,
	params map[string]any,
	notify func(map[string]any),
) (map[string]any, error) {
	headers := http.Header{}
	if strings.TrimSpace(authorization) != "" {
		headers.Set("Authorization", strings.TrimSpace(authorization))
	}
	conn, _, err := websocket.DefaultDialer.DialContext(
		ctx,
		endpoint.webSocketEndpoint(),
		headers,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.Close()
	}()

	requestID := fmt.Sprintf("req-%d", time.Now().UnixNano())
	if err := conn.WriteJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  method,
		"params":  params,
	}); err != nil {
		return nil, err
	}

	for {
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			return nil, err
		}
		var payload map[string]any
		if err := conn.ReadJSON(&payload); err != nil {
			return nil, err
		}
		if strings.TrimSpace(shared.StringArg(payload, "id", "")) == requestID &&
			(payload["result"] != nil || payload["error"] != nil) {
			if errPayload := asMap(payload["error"]); len(errPayload) > 0 {
				return nil, fmt.Errorf(
					"%s",
					strings.TrimSpace(shared.StringArg(errPayload, "message", "external ACP request failed")),
				)
			}
			return payload, nil
		}
		if notify != nil && strings.TrimSpace(shared.StringArg(payload, "method", "")) != "" {
			notify(payload)
		}
	}
}

type urlSpec struct {
	Scheme string
	Host   string
	Port   string
	Path   string
}

func httpOrWebsocketEndpoint(raw string) (*urlSpec, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("missing external ACP endpoint")
	}
	parsed, err := url.ParseRequestURI(trimmed)
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme != "http" && scheme != "https" && scheme != "ws" && scheme != "wss" {
		return nil, fmt.Errorf("unsupported external ACP scheme: %s", scheme)
	}
	return &urlSpec{
		Scheme: scheme,
		Host:   parsed.Host,
		Path:   strings.TrimRight(parsed.Path, "/"),
	}, nil
}

func (u *urlSpec) basePath() string {
	path := strings.TrimSpace(u.Path)
	if path == "" || path == "/" {
		return ""
	}
	if strings.HasSuffix(path, "/acp/rpc") {
		path = strings.TrimSuffix(path, "/acp/rpc")
	} else if strings.HasSuffix(path, "/acp") {
		path = strings.TrimSuffix(path, "/acp")
	}
	path = strings.TrimRight(path, "/")
	if path == "" || path == "/" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

func (u *urlSpec) httpRPCEndpoint() string {
	scheme := u.Scheme
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
	basePath := u.basePath()
	if basePath == "" {
		basePath = "/acp/rpc"
	} else {
		basePath += "/acp/rpc"
	}
	return fmt.Sprintf("%s://%s%s", scheme, u.Host, basePath)
}

func (u *urlSpec) webSocketEndpoint() string {
	scheme := u.Scheme
	switch scheme {
	case "http":
		scheme = "ws"
	case "https":
		scheme = "wss"
	}
	basePath := u.basePath()
	if basePath == "" {
		basePath = "/acp"
	} else {
		basePath += "/acp"
	}
	return fmt.Sprintf("%s://%s%s", scheme, u.Host, basePath)
}
