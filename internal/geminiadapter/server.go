package geminiadapter

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
	defaultListenAddr = "127.0.0.1:8791"
	defaultProviderID = "gemini"
	defaultLabel      = "Gemini"
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
	lastOutput         string
	lastUpstreamMethod string
}

func Serve(args []string) error {
	flags := flag.NewFlagSet("gemini-acp-adapter", flag.ExitOnError)
	listen := flags.String(
		"listen",
		strings.TrimSpace(shared.EnvOrDefault("GEMINI_ADAPTER_LISTEN_ADDR", defaultListenAddr)),
		"Gemini ACP adapter listen address",
	)
	binary := flags.String(
		"gemini-bin",
		strings.TrimSpace(shared.EnvOrDefault("GEMINI_ADAPTER_BIN", shared.EnvOrDefault("ACP_GEMINI_BIN", "gemini"))),
		"Gemini CLI binary path",
	)
	rawArgs := flags.String(
		"gemini-args",
		strings.TrimSpace(shared.EnvOrDefault("GEMINI_ADAPTER_ARGS", "--experimental-acp")),
		"Gemini CLI arguments",
	)
	_ = flags.Parse(args)

	client := newStdioRPCClient(
		*binary,
		strings.Fields(strings.TrimSpace(*rawArgs)),
		nil,
		shared.IntArg(shared.EnvOrDefault("GEMINI_ADAPTER_PROTOCOL_VERSION", "1"), 1),
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
		return fmt.Errorf("gemini adapter failed: %w", err)
	}
	return nil
}

func NewServer(client rpcClient) *Server {
	return &Server{
		client:         client,
		authService:    service.NewStaticTokenAuthService(strings.TrimSpace(shared.EnvOrDefault("GEMINI_ADAPTER_AUTH_TOKEN", ""))),
		providerID:     strings.TrimSpace(shared.EnvOrDefault("GEMINI_ADAPTER_PROVIDER_ID", defaultProviderID)),
		providerLabel:  strings.TrimSpace(shared.EnvOrDefault("GEMINI_ADAPTER_PROVIDER_LABEL", defaultLabel)),
		allowedOrigins: parseAllowedOrigins(strings.TrimSpace(shared.EnvOrDefault("GEMINI_ADAPTER_ALLOWED_ORIGINS", "https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*"))),
		upstreamMethod: strings.TrimSpace(shared.EnvOrDefault("GEMINI_ADAPTER_UPSTREAM_METHOD", "")),
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
	case "gemini.initialize":
		return s.handleInitialize()
	case "gemini.raw":
		return s.handleRaw(request.Params)
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

func (s *Server) handleInitialize() map[string]any {
	result, err := s.client.Initialize()
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	return map[string]any{
		"success": true,
		"result":  result,
	}
}

func (s *Server) handleRaw(params map[string]any) map[string]any {
	method := strings.TrimSpace(shared.StringArg(params, "method", ""))
	upstreamParams, _ := params["params"].(map[string]any)
	if method == "" {
		return map[string]any{"success": false, "error": "method is required"}
	}
	if _, err := s.client.Initialize(); err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	response, err := s.client.Call(method, upstreamParams)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	return map[string]any{"success": true, "response": response}
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
		return s.handleConfiguredUpstreamSessionRequest(upstreamMethod, params)
	}
	return s.handleCompatSessionRequest(method, params)
}

func (s *Server) handleConfiguredUpstreamSessionRequest(upstreamMethod string, params map[string]any) map[string]any {
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
			"error":          strings.TrimSpace(shared.StringArg(errPayload, "message", "upstream gemini acp error")),
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

func (s *Server) handleCompatSessionRequest(method string, params map[string]any) map[string]any {
	if s.sessionRunner == nil {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    "gemini session runner is not configured",
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
	sessionsHistory = append(sessionsHistory, taskPrompt)
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
	state.history = sessionsHistory
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
	headers.Add("Vary", "Access-Control-Request-Method")
	headers.Add("Vary", "Access-Control-Request-Headers")
}

func (s *Server) authorized(r *http.Request) bool {
	if s == nil {
		return false
	}
	if s.authService == nil {
		return false
	}
	return s.authService.ValidateAuthorizationHeader(r.Header.Get("Authorization"))
}

func (s *Server) writeJSONError(w http.ResponseWriter, requestID any, statusCode int, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(shared.ErrorEnvelope(requestID, code, message))
}
