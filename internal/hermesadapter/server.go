package hermesadapter

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/service"
	"xworkmate-bridge/internal/shared"
)

const (
	defaultListenAddr = "127.0.0.1:3920"
	defaultProviderID = "hermes"
	defaultLabel      = "Hermes"
)

type Server struct {
	client         rpcClient
	authService    *service.StaticTokenAuthService
	providerID     string
	providerLabel  string
	allowedOrigins []string
	upstreamMethod string
	sessionRunner  func(context.Context, string, string, string) (string, error)
	sessionsMu     sync.Mutex
	sessions       map[string]*adapterSession
}

var adapterWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  16 * 1024,
	WriteBufferSize: 16 * 1024,
	CheckOrigin: func(*http.Request) bool {
		return true
	},
}

type adapterSession struct {
	history            []string
	model              string
	workingDirectory   string
	upstreamSessionID  string
	lastOutput         string
	lastUpstreamMethod string
}

func Serve(args []string) error {
	flags := flag.NewFlagSet("hermes-acp-adapter", flag.ExitOnError)
	listen := flags.String(
		"listen",
		strings.TrimSpace(shared.EnvOrDefault("HERMES_ADAPTER_LISTEN_ADDR", defaultListenAddr)),
		"Hermes ACP adapter listen address",
	)
	binary := flags.String(
		"hermes-bin",
		strings.TrimSpace(shared.EnvOrDefault("HERMES_ADAPTER_BIN", shared.EnvOrDefault("ACP_HERMES_BIN", "hermes"))),
		"Hermes CLI binary path",
	)
	rawArgs := flags.String(
		"hermes-args",
		strings.TrimSpace(shared.EnvOrDefault("HERMES_ADAPTER_ARGS", "acp")),
		"Hermes CLI arguments",
	)
	_ = flags.Parse(args)

	client := newStdioRPCClient(
		*binary,
		strings.Fields(strings.TrimSpace(*rawArgs)),
		nil,
		shared.IntArg(shared.EnvOrDefault("HERMES_ADAPTER_PROTOCOL_VERSION", "1"), 1),
	)
	defer func() {
		_ = client.Close()
	}()

	server := NewServer(client)
	httpServer := &http.Server{
		Addr: strings.TrimSpace(*listen),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/acp/rpc":
				server.HandleRPC(w, r)
			case "/acp":
				server.HandleWebSocket(w, r)
			default:
				http.NotFound(w, r)
			}
		}),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("hermes adapter failed: %w", err)
	}
	return nil
}

func NewServer(client rpcClient) *Server {
	return &Server{
		client:         client,
		authService:    service.NewStaticTokenAuthService(strings.TrimSpace(shared.EnvOrDefault("HERMES_ADAPTER_AUTH_TOKEN", ""))),
		providerID:     strings.TrimSpace(shared.EnvOrDefault("HERMES_ADAPTER_PROVIDER_ID", defaultProviderID)),
		providerLabel:  strings.TrimSpace(shared.EnvOrDefault("HERMES_ADAPTER_PROVIDER_LABEL", defaultLabel)),
		allowedOrigins: parseAllowedOrigins(strings.TrimSpace(shared.EnvOrDefault("HERMES_ADAPTER_ALLOWED_ORIGINS", "https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*"))),
		upstreamMethod: strings.TrimSpace(shared.EnvOrDefault("HERMES_ADAPTER_UPSTREAM_METHOD", "prompt")),
		sessionRunner: func(ctx context.Context, model, prompt, workingDirectory string) (string, error) {
			return shared.RunProviderCommand(
				ctx,
				defaultProviderID,
				model,
				prompt,
				workingDirectory,
			)
		},
		sessions: make(map[string]*adapterSession),
	}
}

func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r.Header.Get("Origin")) {
		s.writeJSONError(w, nil, http.StatusForbidden, -32003, fmt.Sprintf("origin not allowed: %s", strings.TrimSpace(r.Header.Get("Origin"))))
		return
	}
	if !s.authorized(r) {
		s.writeJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
		return
	}
	upgrader := adapterWSUpgrader
	upgrader.CheckOrigin = func(req *http.Request) bool {
		return s.originAllowed(req.Header.Get("Origin")) && s.authorized(req)
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() {
		_ = conn.Close()
	}()

	var writeMu sync.Mutex
	notify := func(message map[string]any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteJSON(message)
	}

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		request, err := shared.DecodeRPCRequest(payload)
		if err != nil {
			notify(shared.ErrorEnvelope(nil, -32700, err.Error()))
			continue
		}
		response := s.handleRequest(request)
		if request.ID == nil {
			continue
		}
		notify(shared.ResultEnvelope(request.ID, response))
	}
}

func (s *Server) HandleRPC(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		s.writeJSONError(w, nil, http.StatusMethodNotAllowed, -32600, "method not allowed")
		return
	}
	if !s.originAllowed(r.Header.Get("Origin")) {
		s.writeJSONError(w, nil, http.StatusForbidden, -32003, fmt.Sprintf("origin not allowed: %s", strings.TrimSpace(r.Header.Get("Origin"))))
		return
	}
	if !s.authorized(r) {
		s.writeJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
		return
	}
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSONError(w, nil, http.StatusBadRequest, -32600, "invalid body")
		return
	}
	request, err := shared.DecodeRPCRequest(payload)
	if err != nil {
		s.writeJSONError(w, nil, http.StatusBadRequest, -32700, err.Error())
		return
	}
	result := s.handleRequest(request)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(shared.ResultEnvelope(request.ID, result))
}

func (s *Server) handleRequest(request shared.RPCRequest) map[string]any {
	switch strings.TrimSpace(request.Method) {
	case "acp.capabilities":
		return s.handleCapabilities()
	case "session.start", "session.message":
		return s.handleSessionRequest(request.Method, request.Params)
	case "session.cancel":
		return map[string]any{"accepted": true, "cancelled": false}
	case "session.close":
		sessionID := strings.TrimSpace(shared.StringArg(request.Params, "sessionId", ""))
		return map[string]any{"accepted": true, "closed": s.closeSession(sessionID)}
	default:
		return map[string]any{
			"success": false,
			"error":   fmt.Sprintf("unsupported method: %s", strings.TrimSpace(request.Method)),
		}
	}
}

func (s *Server) handleCapabilities() map[string]any {
	result, err := s.client.Initialize()
	if err != nil {
		return map[string]any{
			"singleAgent": false,
			"multiAgent":  false,
			"providers":   []string{},
			"capabilities": map[string]any{
				"single_agent": false,
				"multi_agent":  false,
				"providers":    []string{},
			},
			"success": false,
			"error":   err.Error(),
		}
	}
	return map[string]any{
		"singleAgent": true,
		"multiAgent":  false,
		"providers":   []string{s.providerID},
		"capabilities": map[string]any{
			"single_agent": true,
			"multi_agent":  false,
			"providers":    []string{s.providerID},
		},
		"provider": map[string]any{
			"id":    s.providerID,
			"label": s.providerLabel,
		},
		"upstream": map[string]any{
			"protocolVersion":   result.ProtocolVersion,
			"authMethods":       result.AuthMethods,
			"agentCapabilities": result.AgentCapabilities,
		},
	}
}

func (s *Server) handleSessionRequest(method string, params map[string]any) map[string]any {
	if _, err := s.client.Initialize(); err != nil {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    err.Error(),
		}
	}
	upstreamMethod := s.upstreamMethod
	if upstreamMethod != "" {
		return s.handleConfiguredUpstreamSessionRequest(method, upstreamMethod, params)
	}
	return s.handleCompatSessionRequest(method, params)
}

func (s *Server) handleConfiguredUpstreamSessionRequest(method, upstreamMethod string, params map[string]any) map[string]any {
	if strings.TrimSpace(strings.ToLower(upstreamMethod)) == "prompt" {
		return s.handleHermesPromptUpstreamSessionRequest(method, params)
	}
	response, err := s.client.Call(upstreamMethod, params)
	if err != nil {
		return map[string]any{
			"success":        false,
			"provider":       s.providerID,
			"mode":           "single-agent",
			"error":          err.Error(),
			"upstreamMethod": upstreamMethod,
		}
	}
	result, _ := response["result"].(map[string]any)
	if len(result) > 0 {
		if _, ok := result["provider"]; !ok {
			result["provider"] = s.providerID
		}
		if _, ok := result["mode"]; !ok {
			result["mode"] = "single-agent"
		}
		return result
	}
	if errPayload, ok := response["error"].(map[string]any); ok && len(errPayload) > 0 {
		return map[string]any{
			"success":        false,
			"provider":       s.providerID,
			"mode":           "single-agent",
			"error":          strings.TrimSpace(shared.StringArg(errPayload, "message", "upstream hermes acp error")),
			"upstreamMethod": upstreamMethod,
			"upstreamError":  errPayload,
		}
	}
	return map[string]any{
		"success":        true,
		"provider":       s.providerID,
		"mode":           "single-agent",
		"upstreamMethod": upstreamMethod,
		"upstream":       response,
	}
}

func (s *Server) handleHermesPromptUpstreamSessionRequest(method string, params map[string]any) map[string]any {
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	if sessionID == "" {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    "sessionId is required",
		}
	}

	state := s.getOrCreateSession(sessionID)
	if method == "session.start" {
		state = s.resetSession(sessionID)
	}
	taskPrompt := strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	taskPrompt = shared.AugmentPromptWithAttachments(taskPrompt, params)
	if taskPrompt == "" {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    "taskPrompt is required",
		}
	}

	workingDirectory := strings.TrimSpace(shared.StringArg(params, "workingDirectory", ""))
	if workingDirectory == "" {
		workingDirectory = state.workingDirectory
	}
	if workingDirectory == "" {
		workingDirectory = "."
	}

	if state.upstreamSessionID == "" || method == "session.start" {
		newSessionResp, err := s.client.Call("new_session", map[string]any{
			"cwd": workingDirectory,
		})
		if err != nil {
			return map[string]any{
				"success":  false,
				"provider": s.providerID,
				"mode":     "single-agent",
				"error":    err.Error(),
			}
		}
		state.upstreamSessionID = extractHermesUpstreamSessionID(newSessionResp)
		if state.upstreamSessionID == "" {
			return map[string]any{
				"success":  false,
				"provider": s.providerID,
				"mode":     "single-agent",
				"error":    "hermes upstream did not return a session id",
			}
		}
	}

	s.sessionsMu.Lock()
	current := s.sessions[sessionID]
	if current == nil {
		current = &adapterSession{}
		s.sessions[sessionID] = current
	}
	current.upstreamSessionID = state.upstreamSessionID
	current.workingDirectory = workingDirectory
	current.model = strings.TrimSpace(shared.StringArg(params, "model", current.model))
	s.sessionsMu.Unlock()

	var outputParts []string
	notificationHandler := func(notification map[string]any) {
		text := extractHermesSessionUpdateText(notification)
		if text != "" {
			outputParts = append(outputParts, text)
		}
	}
	s.client.SetNotificationHandler(notificationHandler)
	defer s.client.SetNotificationHandler(nil)

	promptPayload := []map[string]any{
		{
			"type": "text",
			"text": taskPrompt,
		},
	}
	response, err := s.client.Call("prompt", map[string]any{
		"sessionId": state.upstreamSessionID,
		"prompt":    promptPayload,
	})
	if err != nil {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    err.Error(),
		}
	}

	output := strings.TrimSpace(strings.Join(outputParts, ""))
	if output == "" {
		if resultMap, ok := response["result"].(map[string]any); ok {
			for _, key := range []string{"output", "finalResponse", "final_response", "text", "message"} {
				if candidate := strings.TrimSpace(shared.StringArg(resultMap, key, "")); candidate != "" {
					output = candidate
					break
				}
			}
		}
	}
	if output == "" {
		return map[string]any{
			"success":        false,
			"provider":       s.providerID,
			"mode":           "single-agent",
			"error":          "hermes upstream returned empty response",
			"upstreamMethod": "prompt",
			"upstream":       response,
		}
	}

	s.sessionsMu.Lock()
	current = s.sessions[sessionID]
	if current == nil {
		current = &adapterSession{}
		s.sessions[sessionID] = current
	}
	current.history = append(current.history, "USER: "+taskPrompt, "ASSISTANT: "+output)
	current.lastOutput = output
	current.lastUpstreamMethod = "prompt"
	s.sessionsMu.Unlock()

	result := map[string]any{
		"success":        true,
		"provider":       s.providerID,
		"mode":           "single-agent",
		"output":         output,
		"sessionId":      sessionID,
		"upstreamMethod": "prompt",
	}
	if workingDirectory != "" {
		result["effectiveWorkingDirectory"] = workingDirectory
	}
	if state.upstreamSessionID != "" {
		result["upstreamSessionId"] = state.upstreamSessionID
	}
	return result
}

func extractHermesUpstreamSessionID(response map[string]any) string {
	for _, key := range []string{"sessionId", "session_id", "id"} {
		if value := strings.TrimSpace(shared.StringArg(asMap(response["result"]), key, "")); value != "" {
			return value
		}
		if value := strings.TrimSpace(shared.StringArg(response, key, "")); value != "" {
			return value
		}
	}
	return ""
}

func extractHermesSessionUpdateText(notification map[string]any) string {
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
	if updateKind := strings.TrimSpace(shared.StringArg(update, "sessionUpdate", "")); updateKind == "" || updateKind == "agent_message_chunk" || updateKind == "agent_message_text" {
		if text := extractHermesTextValue(update); text != "" {
			return text
		}
		if text := extractHermesTextValue(payload); text != "" {
			return text
		}
	}
	return ""
}

func extractHermesTextValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		var builder strings.Builder
		for _, key := range []string{"text", "message", "content", "delta", "value"} {
			if text := extractHermesTextValue(v[key]); text != "" {
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
			if text := extractHermesTextValue(child); text != "" {
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
			if text := extractHermesTextValue(child); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	default:
		return ""
	}
}

func asMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return nil
}

func (s *Server) handleCompatSessionRequest(method string, params map[string]any) map[string]any {
	if s.sessionRunner == nil {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    "hermes session runner is not configured",
		}
	}
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	if sessionID == "" {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    "sessionId is required",
		}
	}
	state := s.getOrCreateSession(sessionID)
	if method == "session.start" {
		state = s.resetSession(sessionID)
	}
	taskPrompt := strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	taskPrompt = shared.AugmentPromptWithAttachments(taskPrompt, params)
	if taskPrompt == "" {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    "taskPrompt is required",
		}
	}

	model := strings.TrimSpace(shared.StringArg(params, "model", ""))
	if model == "" {
		model = state.model
	}
	workingDirectory := strings.TrimSpace(shared.StringArg(params, "workingDirectory", ""))
	if workingDirectory == "" {
		workingDirectory = state.workingDirectory
	}

	sessionsHistory := append([]string(nil), state.history...)
	sessionsHistory = append(sessionsHistory, "USER: "+taskPrompt)
	composedPrompt := shared.ComposeHistoryPrompt(sessionsHistory)
	output, err := s.sessionRunner(context.Background(), model, composedPrompt, workingDirectory)
	if err != nil {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    err.Error(),
		}
	}

	s.sessionsMu.Lock()
	state = s.sessions[sessionID]
	if state == nil {
		state = &adapterSession{}
		s.sessions[sessionID] = state
	}
	state.history = append(sessionsHistory, "ASSISTANT: "+output)
	state.model = model
	state.workingDirectory = workingDirectory
	state.lastOutput = output
	state.lastUpstreamMethod = "prompt"
	s.sessionsMu.Unlock()

	result := map[string]any{
		"success":        true,
		"provider":       s.providerID,
		"mode":           "single-agent",
		"output":         output,
		"sessionId":      sessionID,
		"upstreamMethod": "prompt",
	}
	if workingDirectory != "" {
		result["effectiveWorkingDirectory"] = workingDirectory
	}
	if model != "" {
		result["resolvedModel"] = model
	}
	return result
}

func (s *Server) getOrCreateSession(sessionID string) *adapterSession {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	state := s.sessions[sessionID]
	if state == nil {
		state = &adapterSession{}
		s.sessions[sessionID] = state
	}
	return &adapterSession{
		history:            append([]string(nil), state.history...),
		model:              state.model,
		workingDirectory:   state.workingDirectory,
		upstreamSessionID:  state.upstreamSessionID,
		lastOutput:         state.lastOutput,
		lastUpstreamMethod: state.lastUpstreamMethod,
	}
}

func (s *Server) resetSession(sessionID string) *adapterSession {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	state := &adapterSession{}
	s.sessions[sessionID] = state
	return state
}

func (s *Server) closeSession(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return false
	}
	delete(s.sessions, sessionID)
	return true
}

func parseAllowedOrigins(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		result = append(result, part)
	}
	return result
}

func (s *Server) originAllowed(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return true
	}
	for _, allowed := range s.allowedOrigins {
		if strings.HasSuffix(allowed, ":*") {
			if strings.HasPrefix(origin, strings.TrimSuffix(allowed, "*")) {
				return true
			}
			continue
		}
		if origin == allowed {
			return true
		}
	}
	return false
}

func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || !s.originAllowed(origin) {
		return
	}
	headers := w.Header()
	headers.Set("Access-Control-Allow-Origin", origin)
	headers.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	headers.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
	headers.Set("Access-Control-Max-Age", "600")
	headers.Add("Vary", "Origin")
}

func (s *Server) authorized(r *http.Request) bool {
	if s == nil {
		return false
	}
	if s.authService == nil {
		return true
	}
	return s.authService.ValidateAuthorizationHeader(r.Header.Get("Authorization"))
}

func (s *Server) writeJSONError(w http.ResponseWriter, id any, status int, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(shared.ErrorEnvelope(id, code, message))
}
