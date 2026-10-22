// Package acpagentadapter exposes any ACP v1 stdio agent (DeepSeek Harness
// `dsh --profile acp`, OpenCode `opencode acp`) on the bridge's provider
// WebSocket dialect without modifying the agent. It adds three guarantees the
// bridge relies on for the Engineer loop:
//
//   - the requested model is bound through the agent's advertised `model`
//     config option and read back, never guessed or substituted;
//   - every session/request_permission is forwarded to the bridge connection
//     and answered only by the bridge's decision (no local auto-approve);
//   - session.cancel maps to ACP session/cancel on the live prompt.
package acpagentadapter

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/service"
	"xworkmate-bridge/internal/shared"
)

const (
	defaultListenAddr        = "127.0.0.1:8795"
	defaultPermissionTimeout = 10 * time.Minute
)

// Failure codes reported in session results.
const (
	CodeModelNotAdvertised     = "MODEL_NOT_ADVERTISED"
	CodeModelBindingUnverified = "MODEL_BINDING_UNVERIFIED"
	CodeWorkspaceInvalid       = "WORKSPACE_INVALID"
	CodeSessionUnavailable     = "SESSION_CONTINUATION_UNAVAILABLE"
	CodeAgentFailed            = "AGENT_FAILED"
)

type Server struct {
	agent             *stdioAgent
	providerID        string
	label             string
	authService       *service.StaticTokenAuthService
	allowedOrigins    []string
	permissionTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*bridgeSession
}

type bridgeSession struct {
	acpSessionID string
	cwd          string
	boundModel   string
}

type Options struct {
	ProviderID        string
	Label             string
	AuthToken         string
	AllowedOrigins    []string
	PermissionTimeout time.Duration
}

func Serve(args []string) error {
	flags := flag.NewFlagSet("adapter acp-agent", flag.ExitOnError)
	listen := flags.String("listen", shared.EnvOrDefault("ACP_AGENT_ADAPTER_LISTEN_ADDR", defaultListenAddr), "listen address")
	command := flags.String("command", shared.EnvOrDefault("ACP_AGENT_ADAPTER_COMMAND", ""), "ACP agent binary, for example dsh or opencode")
	rawArgs := flags.String("args", shared.EnvOrDefault("ACP_AGENT_ADAPTER_ARGS", ""), "ACP agent arguments, for example \"--profile acp\" or \"acp\"")
	providerID := flags.String("provider-id", shared.EnvOrDefault("ACP_AGENT_ADAPTER_PROVIDER_ID", ""), "bridge provider id, for example deepseek-harness")
	label := flags.String("label", shared.EnvOrDefault("ACP_AGENT_ADAPTER_LABEL", ""), "display label")
	workdir := flags.String("workdir", shared.EnvOrDefault("ACP_AGENT_ADAPTER_WORKDIR", ""), "agent process working directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	token := strings.TrimSpace(shared.EnvOrDefault("ACP_AGENT_ADAPTER_AUTH_TOKEN", ""))
	if token == "" {
		return errors.New("ACP_AGENT_ADAPTER_AUTH_TOKEN is required: this adapter fronts an agent that can edit files")
	}
	if strings.TrimSpace(*command) == "" || strings.TrimSpace(*providerID) == "" {
		return errors.New("--command and --provider-id are required")
	}
	agent := newStdioAgent(*command, strings.Fields(*rawArgs), strings.TrimSpace(*workdir))
	server := NewServer(agent, Options{
		ProviderID:        *providerID,
		Label:             *label,
		AuthToken:         token,
		AllowedOrigins:    shared.ParseAllowedOrigins(shared.EnvOrDefault("ACP_AGENT_ADAPTER_ALLOWED_ORIGINS", "")),
		PermissionTimeout: time.Duration(shared.IntArg(shared.EnvOrDefault("ACP_AGENT_ADAPTER_PERMISSION_TIMEOUT_SECONDS", "600"), 600)) * time.Second,
	})
	defer func() {
		if err := agent.close(); err != nil {
			log.Printf("[acp-agent] close agent: %v", err)
		}
	}()
	httpServer := &http.Server{
		Addr: strings.TrimSpace(*listen),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/acp" {
				http.NotFound(w, r)
				return
			}
			server.HandleWebSocket(w, r)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("acp agent adapter failed: %w", err)
	}
	return nil
}

func NewServer(agent *stdioAgent, options Options) *Server {
	timeout := options.PermissionTimeout
	if timeout <= 0 {
		timeout = defaultPermissionTimeout
	}
	label := strings.TrimSpace(options.Label)
	if label == "" {
		label = options.ProviderID
	}
	return &Server{
		agent:             agent,
		providerID:        strings.TrimSpace(options.ProviderID),
		label:             label,
		authService:       service.NewStaticTokenAuthService(strings.TrimSpace(options.AuthToken)),
		allowedOrigins:    options.AllowedOrigins,
		permissionTimeout: timeout,
		sessions:          make(map[string]*bridgeSession),
	}
}

// peer is one bridge WebSocket connection. It serialises writes and lets the
// adapter send requests (permission prompts) and await their responses.
type peer struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[string]chan map[string]any
	closed  chan struct{}
}

func (p *peer) send(message map[string]any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.conn.WriteJSON(message)
}

func (p *peer) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	id := fmt.Sprintf("adapter-%d", p.nextID.Add(1))
	ch := make(chan map[string]any, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()
	if err := p.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case response := <-ch:
		if errPayload, ok := response["error"].(map[string]any); ok {
			return nil, fmt.Errorf("bridge rejected %s: %v", method, errPayload["message"])
		}
		result, _ := response["result"].(map[string]any)
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.closed:
		return nil, errors.New("bridge connection closed")
	}
}

func (p *peer) deliver(response map[string]any) {
	key := fmt.Sprint(response["id"])
	p.mu.Lock()
	ch, ok := p.pending[key]
	p.mu.Unlock()
	if ok {
		ch <- response
	}
}

func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.authService.ValidateAuthorizationHeader(r.Header.Get("Authorization")) {
		shared.WriteJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
		return
	}
	upgrader := shared.StandardWSUpgrader
	upgrader.CheckOrigin = func(req *http.Request) bool {
		return shared.OriginAllowed(req.Header.Get("Origin"), s.allowedOrigins)
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[acp-agent] upgrade failed: %v", err)
		return
	}
	p := &peer{conn: conn, pending: make(map[string]chan map[string]any), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		close(p.closed)
		if err := conn.Close(); err != nil {
			log.Printf("[acp-agent] close websocket: %v", err)
		}
	}()
	var inflight sync.WaitGroup
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			if sendErr := p.send(shared.ErrorEnvelope(nil, -32700, err.Error())); sendErr != nil {
				break
			}
			continue
		}
		method, _ := message["method"].(string)
		if method == "" {
			p.deliver(message)
			continue
		}
		id, hasID := message["id"]
		params, _ := message["params"].(map[string]any)
		inflight.Add(1)
		go func() {
			defer inflight.Done()
			result := s.handleRequest(ctx, strings.TrimSpace(method), params, p)
			if !hasID {
				return
			}
			if err := p.send(shared.ResultEnvelope(id, result)); err != nil {
				log.Printf("[acp-agent] write result for %s: %v", method, err)
			}
		}()
	}
	// The bridge closed the connection: cancel live prompts it owned.
	cancel()
	inflight.Wait()
}

func (s *Server) handleRequest(ctx context.Context, method string, params map[string]any, p *peer) map[string]any {
	switch method {
	case "acp.capabilities":
		init, err := s.agent.initialize(ctx)
		if err != nil {
			return s.failure("", CodeAgentFailed, err.Error())
		}
		return map[string]any{
			"providerId":        s.providerID,
			"label":             s.label,
			"singleAgent":       true,
			"multiAgent":        false,
			"protocolVersion":   init["protocolVersion"],
			"agentInfo":         init["agentInfo"],
			"agentCapabilities": init["agentCapabilities"],
			"permissionRelay":   true,
			"modelBinding":      "config_option",
		}
	case "session.start", "session.message":
		return s.runTurn(ctx, method, params, p)
	case "session.cancel":
		return s.cancel(stringField(params, "sessionId"))
	case "session.close":
		return s.closeSession(ctx, stringField(params, "sessionId"))
	default:
		return s.failure("", CodeAgentFailed, "unsupported method: "+method)
	}
}

func (s *Server) failure(sessionID, code, message string) map[string]any {
	return map[string]any{
		"success":   false,
		"status":    "failed",
		"provider":  s.providerID,
		"mode":      "single-agent",
		"sessionId": sessionID,
		"code":      code,
		"error":     message,
		"message":   message,
	}
}

func (s *Server) runTurn(ctx context.Context, method string, params map[string]any, p *peer) map[string]any {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return s.failure("", CodeAgentFailed, "sessionId is required")
	}
	prompt := shared.AugmentPromptWithAttachments(strings.TrimSpace(shared.StringArg(params, "taskPrompt", "")), params)
	if prompt == "" {
		return s.failure(sessionID, CodeAgentFailed, "taskPrompt is required")
	}
	if _, err := s.agent.initialize(ctx); err != nil {
		return s.failure(sessionID, CodeAgentFailed, "initialize: "+err.Error())
	}

	requestedModel := strings.TrimSpace(shared.StringArg(params, "model", ""))
	state, failure := s.sessionForTurn(ctx, method, sessionID, params, requestedModel)
	if failure != nil {
		return failure
	}

	var (
		outputMu sync.Mutex
		output   strings.Builder
	)
	threadID := stringField(params, "threadId")
	s.agent.setListener(state.acpSessionID, agentListener{
		onUpdate: func(update map[string]any) {
			inner, _ := update["update"].(map[string]any)
			forwarded := map[string]any{
				"sessionId": sessionID,
				"threadId":  threadID,
				"update":    inner,
			}
			if stringField(inner, "sessionUpdate") == "agent_message_chunk" {
				if content, ok := inner["content"].(map[string]any); ok && stringField(content, "type") == "text" {
					text, _ := content["text"].(string)
					outputMu.Lock()
					output.WriteString(text)
					outputMu.Unlock()
					// The app streams `type: delta` updates into the open turn.
					forwarded["type"] = "delta"
					forwarded["delta"] = text
					forwarded["pending"] = true
				}
			}
			if err := p.send(shared.NotificationEnvelope("session/update", forwarded)); err != nil {
				log.Printf("[acp-agent] forward update for %s: %v", sessionID, err)
			}
		},
		onRequest: func(_ context.Context, agentMethod string, agentParams map[string]any) (map[string]any, error) {
			if agentMethod != "session/request_permission" {
				return nil, &rpcError{Code: -32601, Message: "client does not provide " + agentMethod}
			}
			return s.relayPermission(ctx, p, sessionID, agentParams), nil
		},
	})
	defer s.agent.removeListener(state.acpSessionID)

	promptResult, err := s.agent.call(ctx, "session/prompt", map[string]any{
		"sessionId": state.acpSessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": prompt}},
	})
	if err != nil {
		if ctx.Err() != nil {
			if cancelErr := s.agent.notify("session/cancel", map[string]any{"sessionId": state.acpSessionID}); cancelErr != nil {
				log.Printf("[acp-agent] cancel after disconnect: %v", cancelErr)
			}
		}
		return s.failure(sessionID, CodeAgentFailed, "session/prompt: "+err.Error())
	}
	stopReason := stringField(promptResult, "stopReason")
	outputMu.Lock()
	text := strings.TrimSpace(output.String())
	outputMu.Unlock()

	result := map[string]any{
		"provider":          s.providerID,
		"mode":              "single-agent",
		"sessionId":         sessionID,
		"upstreamSessionId": state.acpSessionID,
		"stopReason":        stopReason,
		"output":            text,
		"summary":           text,
		"message":           text,
		"workingDirectory":  state.cwd,
		"modelBinding": map[string]any{
			"requested": requestedModel,
			"bound":     state.boundModel,
			"verified":  requestedModel != "" && requestedModel == state.boundModel,
		},
	}
	switch stopReason {
	case "end_turn":
		result["success"] = true
		result["status"] = "completed"
	case "cancelled":
		result["success"] = false
		result["status"] = "cancelled"
	default:
		result["success"] = false
		result["status"] = "failed"
		result["code"] = CodeAgentFailed
		result["error"] = "agent stopped with " + stopReason
	}
	if method == "session.start" {
		result["started"] = true
	}
	return result
}

// sessionForTurn creates (session.start) or reuses (session.message) the ACP
// session and binds the requested model.
func (s *Server) sessionForTurn(ctx context.Context, method, sessionID string, params map[string]any, requestedModel string) (*bridgeSession, map[string]any) {
	s.mu.Lock()
	existing := s.sessions[sessionID]
	s.mu.Unlock()

	if method == "session.message" {
		if existing == nil {
			return nil, s.failure(sessionID, CodeSessionUnavailable, "no live ACP session for "+sessionID+"; start a new session")
		}
		if requestedModel != "" && requestedModel != existing.boundModel {
			configOptions, failure := s.bindModel(ctx, sessionID, existing.acpSessionID, nil, requestedModel)
			if failure != nil {
				return nil, failure
			}
			existing.boundModel = currentModel(configOptions)
		}
		return existing, nil
	}

	if existing != nil {
		if _, err := s.agent.call(ctx, "session/close", map[string]any{"sessionId": existing.acpSessionID}); err != nil {
			log.Printf("[acp-agent] close replaced session %s: %v", existing.acpSessionID, err)
		}
	}
	cwd := strings.TrimSpace(shared.StringArg(params, "workingDirectory", ""))
	if cwd == "" || !filepath.IsAbs(cwd) {
		return nil, s.failure(sessionID, CodeWorkspaceInvalid, "workingDirectory must be an absolute path authorized for this task")
	}
	created, err := s.agent.call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return nil, s.failure(sessionID, CodeAgentFailed, "session/new: "+err.Error())
	}
	acpSessionID := stringField(created, "sessionId")
	if acpSessionID == "" {
		return nil, s.failure(sessionID, CodeAgentFailed, "session/new returned no sessionId")
	}
	state := &bridgeSession{acpSessionID: acpSessionID, cwd: cwd, boundModel: currentModel(created["configOptions"])}
	if requestedModel != "" {
		configOptions, failure := s.bindModel(ctx, sessionID, acpSessionID, created["configOptions"], requestedModel)
		if failure != nil {
			if _, err := s.agent.call(ctx, "session/close", map[string]any{"sessionId": acpSessionID}); err != nil {
				log.Printf("[acp-agent] close unbound session %s: %v", acpSessionID, err)
			}
			return nil, failure
		}
		state.boundModel = currentModel(configOptions)
	}
	s.mu.Lock()
	s.sessions[sessionID] = state
	s.mu.Unlock()
	return state, nil
}

// bindModel sets the agent's `model` config option to exactly the requested
// advertised value and verifies the agent reports it back.
func (s *Server) bindModel(ctx context.Context, sessionID, acpSessionID string, advertised any, requested string) (any, map[string]any) {
	if advertised != nil && !modelAdvertised(advertised, requested) {
		failure := s.failure(sessionID, CodeModelNotAdvertised, "model option "+requested+" is not advertised by "+s.providerID)
		failure["advertisedModels"] = advertisedModels(advertised)
		return nil, failure
	}
	updated, err := s.agent.call(ctx, "session/set_config_option", map[string]any{
		"sessionId": acpSessionID,
		"configId":  "model",
		"value":     requested,
	})
	if err != nil {
		return nil, s.failure(sessionID, CodeModelNotAdvertised, "session/set_config_option: "+err.Error())
	}
	configOptions := updated["configOptions"]
	if bound := currentModel(configOptions); bound != requested {
		return nil, s.failure(sessionID, CodeModelBindingUnverified, fmt.Sprintf("agent reports model %q after requesting %q", bound, requested))
	}
	return configOptions, nil
}

// relayPermission forwards the agent's permission prompt to the bridge and
// maps the bridge decision onto one of the agent's own option IDs. Anything
// other than an explicit decision (timeout, disconnect, malformed reply)
// becomes the ACP "cancelled" outcome, which denies the tool call.
func (s *Server) relayPermission(ctx context.Context, p *peer, sessionID string, agentParams map[string]any) map[string]any {
	cancelled := map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	options, _ := agentParams["options"].([]any)
	waitCtx, cancel := context.WithTimeout(ctx, s.permissionTimeout)
	defer cancel()
	decision, err := p.request(waitCtx, "session/request_permission", map[string]any{
		"sessionId": sessionID,
		"toolCall":  agentParams["toolCall"],
		"options":   options,
	})
	if err != nil {
		log.Printf("[acp-agent] permission relay for %s ended without a decision: %v", sessionID, err)
		return cancelled
	}
	if outcome, ok := decision["outcome"].(map[string]any); ok {
		switch stringField(outcome, "outcome") {
		case "selected":
			if optionExists(options, stringField(outcome, "optionId")) {
				return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": stringField(outcome, "optionId")}}
			}
			log.Printf("[acp-agent] bridge selected unknown option %q", stringField(outcome, "optionId"))
		case "cancelled":
		default:
			log.Printf("[acp-agent] bridge returned unknown outcome %v", outcome)
		}
	}
	return cancelled
}

func (s *Server) cancel(sessionID string) map[string]any {
	s.mu.Lock()
	state := s.sessions[sessionID]
	s.mu.Unlock()
	if state == nil {
		return map[string]any{"accepted": true, "cancelled": false}
	}
	if err := s.agent.notify("session/cancel", map[string]any{"sessionId": state.acpSessionID}); err != nil {
		return map[string]any{"accepted": false, "cancelled": false, "error": err.Error()}
	}
	return map[string]any{"accepted": true, "cancelled": true}
}

func (s *Server) closeSession(ctx context.Context, sessionID string) map[string]any {
	s.mu.Lock()
	state := s.sessions[sessionID]
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	if state == nil {
		return map[string]any{"accepted": true, "closed": false}
	}
	if _, err := s.agent.call(ctx, "session/close", map[string]any{"sessionId": state.acpSessionID}); err != nil {
		return map[string]any{"accepted": true, "closed": false, "error": err.Error()}
	}
	return map[string]any{"accepted": true, "closed": true}
}

func modelConfigOption(configOptions any) map[string]any {
	list, _ := configOptions.([]any)
	for _, raw := range list {
		option, _ := raw.(map[string]any)
		if stringField(option, "id") == "model" {
			return option
		}
	}
	return nil
}

func currentModel(configOptions any) string {
	option := modelConfigOption(configOptions)
	value, _ := option["currentValue"].(string)
	return value
}

// advertisedModels flattens flat and grouped select options.
func advertisedModels(configOptions any) []string {
	option := modelConfigOption(configOptions)
	var values []string
	var walk func(any)
	walk = func(raw any) {
		list, _ := raw.([]any)
		for _, item := range list {
			entry, _ := item.(map[string]any)
			if value, ok := entry["value"].(string); ok {
				values = append(values, value)
			}
			if nested, ok := entry["options"]; ok {
				walk(nested)
			}
		}
	}
	walk(option["options"])
	return values
}

func modelAdvertised(configOptions any, value string) bool {
	for _, candidate := range advertisedModels(configOptions) {
		if candidate == value {
			return true
		}
	}
	return false
}

func optionExists(options []any, optionID string) bool {
	if optionID == "" {
		return false
	}
	for _, raw := range options {
		option, _ := raw.(map[string]any)
		if stringField(option, "optionId") == optionID {
			return true
		}
	}
	return false
}
