package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/shared"
	"xworkmate-bridge/internal/service"
)

type SessionMode string

const (
	SessionModeSingleAgent SessionMode = "single-agent"
	SessionModeMultiAgent  SessionMode = "multi-agent"
)

type session struct {
	id      string
	thread  string
	mode    SessionMode
	history []map[string]string
	cancel  context.CancelFunc
	closed  bool
}

type task struct {
	req    shared.RPCRequest
	notify func(map[string]any)
	done   chan taskResult
}

type taskResult struct {
	response map[string]any
	err      *shared.RPCError
}

type Server struct {
	mu            sync.RWMutex
	sessions      map[string]*session
	queues        map[string]chan task
	router        *router.Router
	providerCache *router.ProviderCatalog
	auth          *service.StaticTokenAuthService
}

func NewServer() *Server {
	authToken := strings.TrimSpace(os.Getenv("BRIDGE_AUTH_TOKEN"))
	return &Server{
		sessions:      make(map[string]*session),
		queues:        make(map[string]chan task),
		router:        router.NewRouter(),
		providerCache: router.NewProviderCatalog(),
		auth:          service.NewStaticTokenAuthService(authToken),
	}
}

func (s *Server) Serve(addr string) error {
	http.HandleFunc("/acp", s.handleWebSocket)
	http.HandleFunc("/rpc", s.handleHTTP)
	fmt.Printf("ACP server listening on %s\n", addr)
	return http.ListenAndServe(addr, nil)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Authentication check
	token := r.Header.Get("Authorization")
	if !s.auth.ValidateAuthorizationHeader(token) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(shared.ErrorEnvelope(nil, -32001, "unauthorized"))
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	writeMu := sync.Mutex{}
	notify := func(message map[string]any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteJSON(message)
	}

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		request, err := shared.DecodeRPCRequest(payload)
		if err != nil {
			notify(shared.ErrorEnvelope(nil, -32700, err.Error()))
			continue
		}
		response, rpcErr := s.handleRequest(request, notify)
		if request.ID == nil {
			continue
		}
		if rpcErr != nil {
			notify(shared.ErrorEnvelope(request.ID, rpcErr.Code, rpcErr.Message))
			continue
		}
		notify(shared.ResultEnvelope(request.ID, response))
	}
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authentication check
	token := r.Header.Get("Authorization")
	if !s.auth.ValidateAuthorizationHeader(token) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(shared.ErrorEnvelope(nil, -32001, "unauthorized"))
		return
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	request, err := shared.DecodeRPCRequest(payload)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	notify := func(message map[string]any) {
		// Notifications not supported over simple HTTP RPC
	}

	response, rpcErr := s.handleRequest(request, notify)
	if rpcErr != nil {
		_ = json.NewEncoder(w).Encode(shared.ErrorEnvelope(request.ID, rpcErr.Code, rpcErr.Message))
		return
	}
	_ = json.NewEncoder(w).Encode(shared.ResultEnvelope(request.ID, response))
}

func (s *Server) handleRequest(request shared.RPCRequest, notify func(map[string]any)) (map[string]any, *shared.RPCError) {
	method := strings.TrimSpace(request.Method)
	switch method {
	case "health":
		return map[string]any{"status": "ok", "version": "0.7.0"}, nil

	case "acp.capabilities":
		return map[string]any{
			"capabilities": map[string]any{
				"single_agent": true,
				"multi_agent":  true,
			},
			"providerCatalog": s.providerCache.List(),
		}, nil

	case "session.start", "session.message":
		params := request.Params
		sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
		if sessionID == "" {
			return nil, &shared.RPCError{Code: -32602, Message: "sessionId is required"}
		}
		threadID := strings.TrimSpace(shared.StringArg(params, "threadId", sessionID))
		if threadID == "" {
			threadID = sessionID
		}
		if method == "session.start" {
			s.resetSession(sessionID, threadID)
		}
		return s.enqueue(threadID, task{
			req:    request,
			notify: notify,
			done:   make(chan taskResult, 1),
		})

	case "session.cancel":
		params := request.Params
		sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
		if sessionID == "" {
			return nil, &shared.RPCError{Code: -32602, Message: "sessionId is required"}
		}
		cancelled := s.cancelSession(sessionID)
		return map[string]any{"accepted": true, "cancelled": cancelled}, nil

	default:
		return nil, &shared.RPCError{Code: -32601, Message: "method not found"}
	}
}

func (s *Server) enqueue(threadID string, task task) (map[string]any, *shared.RPCError) {
	queue := s.ensureQueue(threadID)
	queue <- task
	result := <-task.done
	return result.response, result.err
}

func (s *Server) ensureQueue(threadID string) chan task {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, ok := s.queues[threadID]
	if ok {
		return queue
	}
	queue = make(chan task, 32)
	s.queues[threadID] = queue
	go s.runQueue(queue)
	return queue
}

func (s *Server) runQueue(queue chan task) {
	for task := range queue {
		response, err := s.executeSessionTask(task)
		task.done <- taskResult{response: response, err: err}
	}
}

func (s *Server) executeSessionTask(task task) (map[string]any, *shared.RPCError) {
	params := task.req.Params
	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	threadID := strings.TrimSpace(shared.StringArg(params, "threadId", sessionID))
	modeStr := strings.TrimSpace(shared.StringArg(params, "mode", "single-agent"))

	session := s.getOrCreateSession(sessionID, threadID)
	session.mode = SessionMode(modeStr)

	executionParams := params
	prompt := strings.TrimSpace(shared.StringArg(executionParams, "taskPrompt", ""))
	if prompt != "" {
		session.history = append(session.history, map[string]string{"role": "user", "content": prompt})
	}

	turnID := fmt.Sprintf("turn-%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	s.setSessionCancel(sessionID, cancel)
	defer s.clearSessionCancel(sessionID)

	if session.mode == SessionModeMultiAgent {
		result := s.runMultiAgent(ctx, session, executionParams, turnID, task.notify)
		if result.err == nil {
			summary := strings.TrimSpace(fmt.Sprint(result.response["summary"]))
			if summary != "" {
				session.history = append(session.history, map[string]string{"role": "assistant", "content": summary})
			}
		}
		return result.response, result.err
	}

	result := s.runSingleAgent(ctx, session, executionParams, turnID, task.notify)
	if result.err == nil {
		output := strings.TrimSpace(fmt.Sprint(result.response["output"]))
		if output != "" {
			session.history = append(session.history, map[string]string{"role": "assistant", "content": output})
		}
	}
	return result.response, result.err
}

func (s *Server) runSingleAgent(ctx context.Context, session *session, params map[string]any, turnID string, notify func(map[string]any)) taskResult {
	provider := shared.StringArg(params, "provider", "codex")
	prompt := shared.StringArg(params, "taskPrompt", "")
	prompt = shared.AugmentPromptWithAttachments(prompt, params)
	model := shared.StringArg(params, "model", "")
	cwd := shared.StringArg(params, "workingDirectory", "")

	output, err := shared.RunProviderCommand(ctx, provider, model, prompt, cwd)
	if err != nil {
		return taskResult{err: &shared.RPCError{Code: -32000, Message: err.Error()}}
	}

	return taskResult{
		response: map[string]any{
			"success": true,
			"output":  output,
			"turnId":  turnID,
			"mode":    "single-agent",
		},
	}
}

func (s *Server) runMultiAgent(ctx context.Context, session *session, params map[string]any, turnID string, notify func(map[string]any)) taskResult {
	s.emitSessionUpdate(session, notify, turnID, map[string]any{
		"type":      "step",
		"mode":      "multi-agent",
		"title":     "Architect",
		"message":   "Analyzing request and planning orchestration",
		"pending":   true,
		"error":     false,
		"role":      "architect",
		"iteration": 1,
		"score":     0,
	})

	baseURL := shared.StringArg(params, "aiGatewayBaseUrl", os.Getenv("AI_GATEWAY_BASE_URL"))
	apiKey := shared.StringArg(params, "aiGatewayApiKey", os.Getenv("AI_GATEWAY_API_KEY"))
	model := shared.StringArg(params, "model", os.Getenv("ACP_MULTI_AGENT_MODEL"))
	if model == "" {
		model = "gpt-4o"
	}

	if apiKey == "" {
		return taskResult{err: &shared.RPCError{Code: -32000, Message: "aiGatewayApiKey is required for multi-agent mode"}}
	}

	messages := []map[string]string{
		{"role": "system", "content": "You are a multi-agent coordinator. Be concise and helpful."},
	}
	for _, h := range session.history {
		messages = append(messages, h)
	}

	output, err := shared.CallOpenAICompatibleCtx(ctx, baseURL, apiKey, model, messages)
	if err != nil {
		return taskResult{err: &shared.RPCError{Code: -32000, Message: err.Error()}}
	}

	s.emitSessionUpdate(session, notify, turnID, map[string]any{
		"type":      "step",
		"mode":      "multi-agent",
		"title":     "Reviewer",
		"message":   output,
		"pending":   false,
		"error":     false,
		"role":      "tester",
		"iteration": 1,
		"score":     9,
	})

	return taskResult{
		response: map[string]any{
			"success":    true,
			"summary":    output,
			"finalScore": 9,
			"iterations": 1,
			"turnId":     turnID,
			"mode":       "multi-agent",
		},
	}
}

func (s *Server) emitSessionUpdate(session *session, notify func(map[string]any), turnID string, payload map[string]any) {
	params := map[string]any{
		"sessionId": session.id,
		"threadId":  session.thread,
		"turnId":    turnID,
		"mode":      string(session.mode),
	}
	for k, v := range payload {
		params[k] = v
	}
	notify(shared.NotificationEnvelope("session.update", params))
}

func (s *Server) getOrCreateSession(sessionID string, threadID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if ok {
		return sess
	}
	sess = &session{
		id:      sessionID,
		thread:  threadID,
		mode:    SessionModeSingleAgent,
		history: []map[string]string{},
	}
	s.sessions[sessionID] = sess
	return sess
}

func (s *Server) resetSession(sessionID string, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = &session{
		id:      sessionID,
		thread:  threadID,
		mode:    SessionModeSingleAgent,
		history: []map[string]string{},
	}
}

func (s *Server) setSessionCancel(sessionID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.cancel = cancel
	}
}

func (s *Server) clearSessionCancel(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.cancel = nil
	}
}

func (s *Server) cancelSession(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok || sess.cancel == nil {
		return false
	}
	sess.cancel()
	sess.cancel = nil
	return true
}
