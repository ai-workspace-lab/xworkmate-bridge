package opencodeadapter

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"xworkmate-bridge/internal/service"
	"xworkmate-bridge/internal/shared"
)

const (
	defaultListenAddr = "127.0.0.1:38992"
	defaultProviderID = "opencode"
	defaultLabel      = "OpenCode"
)

type Server struct {
	client         openCodeClient
	authService    *service.StaticTokenAuthService
	providerID     string
	providerLabel  string
	allowedOrigins []string
	sessions       map[string]*opencodeSessionState
	sessionsMu     sync.Mutex
}

func Serve(args []string) error {
	flags := flag.NewFlagSet("adapter opencode", flag.ExitOnError)
	listen := flags.String(
		"listen",
		strings.TrimSpace(shared.EnvOrDefault("OPENCODE_ADAPTER_LISTEN_ADDR", defaultListenAddr)),
		"OpenCode ACP adapter listen address",
	)
	binary := flags.String(
		"opencode-bin",
		strings.TrimSpace(shared.EnvOrDefault("OPENCODE_ADAPTER_BIN", shared.EnvOrDefault("ACP_OPENCODE_BIN", "opencode"))),
		"OpenCode CLI binary path",
	)
	cwd := flags.String(
		"cwd",
		strings.TrimSpace(shared.EnvOrDefault("OPENCODE_ADAPTER_CWD", "/home/ubuntu/.opencode")),
		"OpenCode ACP working directory",
	)
	_ = flags.Parse(args)

	client := newOpenCodeHTTPClient(*binary, []string{"serve", "--hostname", "127.0.0.1", "--port", "38993", "--print-logs"})
	client.cwd = strings.TrimSpace(*cwd)
	defer func() { _ = client.Close() }()

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
		return fmt.Errorf("opencode adapter failed: %w", err)
	}
	return nil
}

func NewServer(client openCodeClient) *Server {
	return &Server{
		client:         client,
		authService:    service.NewStaticTokenAuthService(strings.TrimSpace(shared.EnvOrDefault("OPENCODE_ADAPTER_AUTH_TOKEN", ""))),
		providerID:     strings.TrimSpace(shared.EnvOrDefault("OPENCODE_ADAPTER_PROVIDER_ID", defaultProviderID)),
		providerLabel:  strings.TrimSpace(shared.EnvOrDefault("OPENCODE_ADAPTER_PROVIDER_LABEL", defaultLabel)),
		allowedOrigins: shared.ParseAllowedOrigins(strings.TrimSpace(shared.EnvOrDefault("OPENCODE_ADAPTER_ALLOWED_ORIGINS", "https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*"))),
		sessions:       make(map[string]*opencodeSessionState),
	}
}

func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !shared.OriginAllowed(r.Header.Get("Origin"), s.allowedOrigins) {
		shared.WriteJSONError(w, nil, http.StatusForbidden, -32003, fmt.Sprintf("origin not allowed: %s", strings.TrimSpace(r.Header.Get("Origin"))))
		return
	}
	if !s.authorized(r) {
		shared.WriteJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
		return
	}
	upgrader := shared.StandardWSUpgrader
	upgrader.CheckOrigin = func(req *http.Request) bool {
		return shared.OriginAllowed(req.Header.Get("Origin"), s.allowedOrigins) && s.authorized(req)
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		request, err := shared.DecodeRPCRequest(payload)
		if err != nil {
			_ = conn.WriteJSON(shared.ErrorEnvelope(nil, -32700, err.Error()))
			continue
		}
		response := s.handleRequest(request)
		if request.ID != nil {
			_ = conn.WriteJSON(shared.ResultEnvelope(request.ID, response))
		}
	}
}

func (s *Server) HandleRPC(w http.ResponseWriter, r *http.Request) {
	shared.ApplyCORS(w, r, s.allowedOrigins)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		shared.WriteJSONError(w, nil, http.StatusMethodNotAllowed, -32600, "method not allowed")
		return
	}
	if !shared.OriginAllowed(r.Header.Get("Origin"), s.allowedOrigins) {
		shared.WriteJSONError(w, nil, http.StatusForbidden, -32003, fmt.Sprintf("origin not allowed: %s", strings.TrimSpace(r.Header.Get("Origin"))))
		return
	}
	if !s.authorized(r) {
		shared.WriteJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
		return
	}
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32600, "invalid body")
		return
	}
	request, err := shared.DecodeRPCRequest(payload)
	if err != nil {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32700, err.Error())
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
		return map[string]any{"success": true, "provider": s.providerID, "mode": "single-agent", "accepted": true, "cancelled": false}
	case "session.close":
		sessionID := strings.TrimSpace(shared.StringArg(request.Params, "sessionId", ""))
		return map[string]any{"success": true, "provider": s.providerID, "mode": "single-agent", "accepted": true, "closed": s.closeSession(sessionID)}
	default:
		resp, err := s.client.Call(request.Method, request.Params)
		if err != nil {
			return map[string]any{"success": false, "error": err.Error()}
		}
		result := shared.AsMap(resp["result"])
		if len(result) == 0 {
			result = resp
		}
		return result
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
		"success": true,
		"result":  result,
	}
}

type opencodeSessionState struct {
	upstreamSessionID string
	lastOutput        string
}

type initializeResult struct {
	ProtocolVersion   int              `json:"protocolVersion"`
	AuthMethods       []map[string]any `json:"authMethods"`
	AgentCapabilities map[string]any   `json:"agentCapabilities"`
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
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	if sessionID == "" {
		return map[string]any{
			"success":  false,
			"provider": s.providerID,
			"mode":     "single-agent",
			"error":    "sessionId is required",
		}
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
	state := s.getOrCreateSession(sessionID)
	if method == "session.start" {
		state = s.resetSession(sessionID)
	}
	if state.upstreamSessionID == "" {
		upstreamSessionID, err := s.client.CreateSession(strings.TrimSpace(shared.StringArg(params, "title", sessionID)))
		if err != nil {
			return map[string]any{"success": false, "provider": s.providerID, "mode": "single-agent", "error": err.Error()}
		}
		upstreamSessionID = sanitizeOpenCodeProviderSessionID(upstreamSessionID)
		if upstreamSessionID == "" {
			return map[string]any{"success": false, "provider": s.providerID, "mode": "single-agent", "error": "opencode create session returned no session id"}
		}
		state.upstreamSessionID = upstreamSessionID
		s.setSession(sessionID, state)
	}
	response, err := s.client.SendMessage(state.upstreamSessionID, taskPrompt, params)
	if err != nil {
		return map[string]any{"success": false, "provider": s.providerID, "mode": "single-agent", "error": err.Error(), "upstreamSessionId": state.upstreamSessionID}
	}
	output := strings.TrimSpace(shared.StringArg(response, "message", ""))
	if output == "" {
		output = strings.TrimSpace(shared.StringArg(response, "output", ""))
	}
	if output == "" {
		output = strings.TrimSpace(shared.StringArg(response, "summary", ""))
	}
	if output == "" {
		output = strings.TrimSpace(shared.StringArg(response, "text", ""))
	}
	if output == "" {
		if result := shared.AsMap(response["result"]); len(result) > 0 {
			output = strings.TrimSpace(shared.StringArg(result, "message", ""))
			if output == "" {
				output = strings.TrimSpace(shared.StringArg(result, "output", ""))
			}
		}
	}
	if output == "" {
		return map[string]any{"success": false, "provider": s.providerID, "mode": "single-agent", "error": "opencode returned empty response", "upstreamSessionId": state.upstreamSessionID, "upstream": response}
	}
	state.lastOutput = output
	s.setSession(sessionID, state)
	result := map[string]any{
		"success":           true,
		"provider":          s.providerID,
		"mode":              "single-agent",
		"sessionId":         sessionID,
		"upstreamSessionId": state.upstreamSessionID,
		"output":            output,
		"summary":           output,
		"message":           output,
	}
	if method == "session.start" {
		result["started"] = true
	}
	return result
}

func (s *Server) closeSession(sessionID string) bool {
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

func (s *Server) getOrCreateSession(sessionID string) *opencodeSessionState {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	state := s.sessions[sessionID]
	if state == nil {
		state = &opencodeSessionState{}
		s.sessions[sessionID] = state
	}
	return state
}

func (s *Server) setSession(sessionID string, state *opencodeSessionState) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.sessions[sessionID] = state
}

func (s *Server) resetSession(sessionID string) *opencodeSessionState {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	state := &opencodeSessionState{}
	s.sessions[sessionID] = state
	return state
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

type openCodeClient interface {
	Initialize() (initializeResult, error)
	Call(method string, params map[string]any) (map[string]any, error)
	CreateSession(title string) (string, error)
	SendMessage(sessionID, prompt string, params map[string]any) (map[string]any, error)
	Close() error
}
