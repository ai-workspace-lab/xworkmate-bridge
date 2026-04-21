package acp

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/dispatch"
	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/mounts"
	"xworkmate-bridge/internal/router"
	"xworkmate-bridge/internal/service"
	"xworkmate-bridge/internal/shared"
)

type session struct {
	sessionID string
	threadID  string
	mode      string
	provider  string
	history   []string
	seq       int
	cancel    context.CancelFunc
	closed    bool
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
	mu              sync.Mutex
	sessions        map[string]*session
	queues          map[string]chan task
	gateway         *gatewayruntime.Manager
	providerCatalog map[string]syncedProvider
	providerOrder   []string
	authService     *service.StaticTokenAuthService
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  16 * 1024,
	WriteBufferSize: 16 * 1024,
	CheckOrigin: func(*http.Request) bool {
		return true
	},
}

func Serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := flags.String(
		"listen",
		shared.EnvOrDefault("ACP_LISTEN_ADDR", "127.0.0.1:8787"),
		"ACP listen address",
	)
	_ = flags.Parse(args)

	server := NewServer()
	httpServer := &http.Server{
		Addr:         strings.TrimSpace(*listen),
		Handler:      server.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	if err := httpServer.ListenAndServe(); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("ACP server failed: %w", err)
	}
	return nil
}

func NewServer() *Server {
	providerCatalog, providerOrder := newProductionProviderCatalog()
	return &Server{
		sessions:        make(map[string]*session),
		queues:          make(map[string]chan task),
		gateway:         gatewayruntime.NewManager(),
		providerCatalog: providerCatalog,
		providerOrder:   providerOrder,
		authService:     service.NewStaticTokenAuthService(strings.TrimSpace(shared.EnvOrDefault("BRIDGE_AUTH_TOKEN", ""))),
	}
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if providerID, ok := parseProviderACPRPCPath(r.URL.Path); ok {
			s.HandleProviderRPC(w, r, providerID)
			return
		}
		if providerID, ok := parseProviderBarePath(r.URL.Path); ok {
			s.HandleProviderAlias(w, r, providerID)
			return
		}
		if strings.TrimSpace(r.URL.Path) == "/gateway/openclaw" {
			s.HandleGatewayAlias(w, r)
			return
		}
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("xworkmate-bridge is running"))
		case "/api/ping":
			info := parseImageVersionInfo(os.Getenv("IMAGE"))
			resp := map[string]any{
				"status":  "ok",
				"image":   info.ImageRef,
				"tag":     info.Tag,
				"commit":  info.Commit,
				"version": info.Version,
			}
			body, err := json.Marshal(resp)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case "/bridge/bootstrap/health":
			s.HandleBridgeBootstrapHealth(w, r)
		case "/acp/rpc":
			s.HandleRPC(w, r)
		case "/acp":
			s.HandleWebSocket(w, r)
		case "/gateway/openclaw/acp/rpc":
			s.HandleGatewayRPCAlias(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func parseProviderBarePath(pathValue string) (string, bool) {
	trimmed := strings.Trim(path.Clean(strings.TrimSpace(pathValue)), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 {
		return "", false
	}
	if parts[0] != "acp-server" {
		return "", false
	}
	switch parts[1] {
	case "codex", "opencode", "gemini", "hermes":
		return parts[1], true
	default:
		return "", false
	}
}

func parseProviderACPRPCPath(path string) (string, bool) {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 4 {
		return "", false
	}
	if parts[0] != "acp-server" || parts[2] != "acp" || parts[3] != "rpc" {
		return "", false
	}
	switch parts[1] {
	case "codex", "opencode", "gemini", "hermes":
		return parts[1], true
	default:
		return "", false
	}
}

func (s *Server) HandleProviderAlias(w http.ResponseWriter, r *http.Request, providerID string) {
	if r.Method == http.MethodGet {
		s.writeAliasCapabilities(w, providerID, "agent")
		return
	}
	s.HandleProviderRPC(w, r, providerID)
}

func (s *Server) HandleGatewayAlias(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.writeAliasCapabilities(w, "openclaw", "gateway")
		return
	}
	s.HandleGatewayRPCAlias(w, r)
}

func (s *Server) writeAliasCapabilities(w http.ResponseWriter, providerID, target string) {
	result, rpcErr := s.handleRequest(shared.RPCRequest{
		JSONRPC: "2.0",
		Method:  "acp.capabilities",
		Params: map[string]any{
			"preferredExecutionTarget": target,
			"preferredProviderId":      providerID,
		},
	}, nil)
	if rpcErr != nil {
		s.writeJSONError(w, nil, http.StatusOK, rpcErr.Code, rpcErr.Message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(shared.ResultEnvelope(nil, result))
}

func (s *Server) HandleGatewayRPCAlias(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/gateway/openclaw" && r.URL.Path != "/gateway/openclaw/acp/rpc" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		r = r.Clone(r.Context())
		r.URL.Path = "/acp/rpc"
		s.HandleRPC(w, r)
		return
	}
	s.HandleRPC(w, r)
}

func (s *Server) HandleProviderRPC(w http.ResponseWriter, r *http.Request, providerID string) {
	if r.Method == http.MethodGet {
		http.NotFound(w, r)
		return
	}
	s.applyCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		s.writeJSONError(
			w,
			nil,
			http.StatusMethodNotAllowed,
			-32600,
			"method not allowed",
		)
		return
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if !s.originAllowed(origin) {
		s.writeJSONError(
			w,
			nil,
			http.StatusForbidden,
			-32003,
			fmt.Sprintf("origin not allowed: %s", origin),
		)
		return
	}
	if !s.authorized(r) {
		s.writeJSONError(
			w,
			nil,
			http.StatusUnauthorized,
			-32001,
			"missing bearer authorization",
		)
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
	params := request.Params
	if params == nil {
		params = map[string]any{}
	}
	params["routing"] = map[string]any{
		"routingMode":           "explicit",
		"explicitExecutionTarget": "singleAgent",
		"explicitProviderId":    providerID,
	}
	request.Params = injectInboundAuthorizationHeader(
		params,
		r.Header.Get("Authorization"),
	)
	response, rpcErr := s.handleRequest(request, nil)
	if request.ID == nil {
		return
	}
	if rpcErr != nil {
		s.writeJSONError(w, request.ID, http.StatusOK, rpcErr.Code, rpcErr.Message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(shared.ResultEnvelope(request.ID, response))
}

func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if !s.originAllowed(origin) {
		s.writeJSONError(
			w,
			nil,
			http.StatusForbidden,
			-32003,
			fmt.Sprintf("origin not allowed: %s", origin),
		)
		return
	}
	if !s.authorized(r) {
		s.writeJSONError(
			w,
			nil,
			http.StatusUnauthorized,
			-32001,
			"missing bearer authorization",
		)
		return
	}
	upgrader := wsUpgrader
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
		request.Params = injectInboundAuthorizationHeader(
			request.Params,
			r.Header.Get("Authorization"),
		)
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

func (s *Server) HandleRPC(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		s.writeJSONError(
			w,
			nil,
			http.StatusMethodNotAllowed,
			-32600,
			"method not allowed",
		)
		return
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if !s.originAllowed(origin) {
		s.writeJSONError(
			w,
			nil,
			http.StatusForbidden,
			-32003,
			fmt.Sprintf("origin not allowed: %s", origin),
		)
		return
	}
	if !s.authorized(r) {
		s.writeJSONError(
			w,
			nil,
			http.StatusUnauthorized,
			-32001,
			"missing bearer authorization",
		)
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
	request.Params = injectInboundAuthorizationHeader(
		request.Params,
		r.Header.Get("Authorization"),
	)

	accept := strings.ToLower(r.Header.Get("Accept"))
	stream := strings.Contains(accept, "text/event-stream")
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	}

	flusher, _ := w.(http.Flusher)
	writeNotification := func(message map[string]any) {
		if !stream {
			return
		}
		shared.WriteSSE(w, message)
		if flusher != nil {
			flusher.Flush()
		}
	}

	response, rpcErr := s.handleRequest(request, writeNotification)
	if request.ID == nil {
		if stream {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
		return
	}
	if rpcErr != nil {
		envelope := shared.ErrorEnvelope(request.ID, rpcErr.Code, rpcErr.Message)
		if stream {
			shared.WriteSSE(w, envelope)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(envelope)
		return
	}
	if stream {
		shared.WriteSSE(w, shared.ResultEnvelope(request.ID, response))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(shared.ResultEnvelope(request.ID, response))
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

func (s *Server) handleRequest(
	request shared.RPCRequest,
	notify func(map[string]any),
) (map[string]any, *shared.RPCError) {
	method := strings.TrimSpace(request.Method)
	switch method {
	case "health":
		return map[string]any{"status": "ok", "version": "0.7.0"}, nil
	case "acp.capabilities":
		providerCatalog := s.availableProviderCatalog()
		gatewayProviders := availableGatewayProviderCatalog()
		singleAgent := len(providerCatalog) > 0
		availableExecutionTargets := availableExecutionTargets(
			providerCatalog,
			gatewayProviders,
		)
		multiAgent := shared.BoolArg(
			shared.EnvOrDefault("ACP_MULTI_AGENT_ENABLED", "true"),
			true,
		)
		result := map[string]any{
			"singleAgent":               singleAgent,
			"multiAgent":                multiAgent,
			"availableExecutionTargets": availableExecutionTargets,
			"providerCatalog":           providerCatalog,
			"gatewayProviders":          gatewayProviders,
			"capabilities": map[string]any{
				"single_agent":              singleAgent,
				"multi_agent":               multiAgent,
				"availableExecutionTargets": availableExecutionTargets,
				"providerCatalog":           providerCatalog,
				"gatewayProviders":          gatewayProviders,
			},
		}
		return result, nil
	case "session.start", "session.message":
		params := request.Params
		sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
		if sessionID == "" {
			return nil, &shared.RPCError{
				Code:    -32602,
				Message: "sessionId is required",
			}
		}
		threadID := strings.TrimSpace(
			shared.StringArg(params, "threadId", sessionID),
		)
		if threadID == "" {
			threadID = sessionID
		}
		if method == "session.start" {
			s.resetSession(sessionID, threadID)
		}
		result, rpcErr := s.enqueue(threadID, task{
			req:    request,
			notify: notify,
			done:   make(chan taskResult, 1),
		})
		if rpcErr != nil {
			return nil, rpcErr
		}
		return result, nil
	case "session.cancel":
		params := request.Params
		sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
		if sessionID == "" {
			return nil, &shared.RPCError{
				Code:    -32602,
				Message: "sessionId is required",
			}
		}
		cancelled := s.cancelSession(sessionID)
		return map[string]any{"accepted": true, "cancelled": cancelled}, nil
	case "session.close":
		params := request.Params
		sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
		if sessionID == "" {
			return nil, &shared.RPCError{
				Code:    -32602,
				Message: "sessionId is required",
			}
		}
		closed := s.closeSession(sessionID)
		return map[string]any{"accepted": true, "closed": closed}, nil
	case "xworkmate.dispatch.resolve":
		return handleDispatchResolve(request.Params), nil
	case "xworkmate.routing.resolve":
		result, _ := resolveRoutingMetadataWithProviders(
			request.Params,
			s.availableProviders(),
		)
		return mergeRoutingResponse(map[string]any{"ok": true}, result), nil
	case "xworkmate.provider.probe":
		providerID := strings.TrimSpace(shared.StringArg(request.Params, "providerId", ""))
		if providerID == "" {
			return nil, &shared.RPCError{
				Code:    -32602,
				Message: "providerId is required",
			}
		}
		provider, ok := s.syncedProviderByID(providerID)
		if !ok {
			return map[string]any{
				"success":    false,
				"providerId": providerID,
				"error":      "provider is not advertised by the bridge",
			}, nil
		}
		result, err := s.probeExternalProvider(context.Background(), provider, request.Params)
		if err != nil {
			return map[string]any{
				"success":    false,
				"providerId": providerID,
				"error":      err.Error(),
			}, nil
		}
		return map[string]any{
			"success":      true,
			"providerId":   providerID,
			"probeMethod":  "acp.capabilities",
			"capabilities": result,
		}, nil
	case "xworkmate.mounts.reconcile":
		return handleMountReconcile(request.Params), nil
	case "xworkmate.gateway.connect":
		return handleGatewayConnect(s, request.Params, notify), nil
	case "xworkmate.gateway.request":
		return handleGatewayRequest(s, request.Params, notify), nil
	case "xworkmate.gateway.disconnect":
		return handleGatewayDisconnect(s, request.Params, notify), nil
	default:
		return nil, &shared.RPCError{
			Code:    -32601,
			Message: fmt.Sprintf("unknown method: %s", method),
		}
	}
}

func handleDispatchResolve(params map[string]any) map[string]any {
	providers := parseDispatchProviders(params["providers"])
	requiredCapabilities := parseStringSlice(params["requiredCapabilities"])
	preferredProviderID := strings.TrimSpace(
		shared.StringArg(params, "preferredProviderId", ""),
	)
	request := dispatch.Request{
		Providers:            providers,
		PreferredProviderID:  preferredProviderID,
		RequiredCapabilities: requiredCapabilities,
	}
	if nodeState := parseDispatchNodeState(params["nodeState"]); nodeState != nil {
		request.NodeState = nodeState
	}
	if nodeInfo := parseDispatchNodeInfo(params["nodeInfo"]); nodeInfo != nil {
		request.NodeInfo = nodeInfo
	}
	return dispatch.ResultMap(dispatch.Resolve(request))
}

func parseDispatchProviders(raw any) []dispatch.Provider {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	providers := make([]dispatch.Provider, 0, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(shared.StringArg(entry, "id", ""))
		if id == "" {
			continue
		}
		providers = append(providers, dispatch.Provider{
			ID:           id,
			Name:         strings.TrimSpace(shared.StringArg(entry, "name", "")),
			DefaultArgs:  parseStringSlice(entry["defaultArgs"]),
			Capabilities: parseStringSlice(entry["capabilities"]),
		})
	}
	return providers
}

func parseDispatchNodeState(raw any) *dispatch.NodeState {
	entry, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return &dispatch.NodeState{
		SelectedAgentID: strings.TrimSpace(
			shared.StringArg(entry, "selectedAgentId", ""),
		),
		GatewayConnected: shared.BoolArg(
			fmt.Sprint(entry["gatewayConnected"]),
			false,
		),
		ExecutionTarget: strings.TrimSpace(
			shared.StringArg(entry, "executionTarget", ""),
		),
		RuntimeMode:   strings.TrimSpace(shared.StringArg(entry, "runtimeMode", "")),
		BridgeEnabled: shared.BoolArg(fmt.Sprint(entry["bridgeEnabled"]), false),
		BridgeState:   strings.TrimSpace(shared.StringArg(entry, "bridgeState", "")),
		ResolvedCodexCLIPath: strings.TrimSpace(
			shared.StringArg(entry, "resolvedCodexCliPath", ""),
		),
		ConfiguredCodexCLIPath: strings.TrimSpace(
			shared.StringArg(entry, "configuredCodexCliPath", ""),
		),
	}
}

func parseDispatchNodeInfo(raw any) *dispatch.NodeInfo {
	entry, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return &dispatch.NodeInfo{
		ID:      strings.TrimSpace(shared.StringArg(entry, "id", "")),
		Name:    strings.TrimSpace(shared.StringArg(entry, "name", "")),
		Version: strings.TrimSpace(shared.StringArg(entry, "version", "")),
	}
}

func parseStringSlice(raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(list))
	for _, item := range list {
		value := strings.TrimSpace(fmt.Sprint(item))
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values
}

func handleMountReconcile(params map[string]any) map[string]any {
	config := parseMountConfig(params["config"])
	request := mounts.Request{
		Config:                 config,
		AIGatewayURL:           strings.TrimSpace(shared.StringArg(params, "aiGatewayUrl", "")),
		ConfiguredCodexCLIPath: strings.TrimSpace(shared.StringArg(params, "configuredCodexCliPath", "")),
		CodexHome:              strings.TrimSpace(shared.StringArg(params, "codexHome", "")),
		OpencodeHome:           strings.TrimSpace(shared.StringArg(params, "opencodeHome", "")),
		OpenClawHome:           strings.TrimSpace(shared.StringArg(params, "openclawHome", "")),
		Aris:                   parseMountArisInput(params["aris"]),
	}
	return mounts.ResultMap(mounts.Reconcile(request))
}

func parseMountConfig(raw any) mounts.Config {
	entry, ok := raw.(map[string]any)
	if !ok {
		return mounts.Config{}
	}
	managedMCPServers := parseMountManagedServers(entry["managedMcpServers"])
	return mounts.Config{
		AutoSync:          shared.BoolArg(fmt.Sprint(entry["autoSync"]), false),
		UsesAris:          shared.BoolArg(fmt.Sprint(entry["usesAris"]), false),
		ManagedMCPServers: managedMCPServers,
	}
}

func parseMountManagedServers(raw any) []mounts.ManagedMCPServer {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	servers := make([]mounts.ManagedMCPServer, 0, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(shared.StringArg(entry, "id", ""))
		if id == "" {
			continue
		}
		servers = append(servers, mounts.ManagedMCPServer{
			ID:        id,
			Name:      strings.TrimSpace(shared.StringArg(entry, "name", "")),
			Transport: strings.TrimSpace(shared.StringArg(entry, "transport", "")),
			Command:   strings.TrimSpace(shared.StringArg(entry, "command", "")),
			URL:       strings.TrimSpace(shared.StringArg(entry, "url", "")),
			Args:      parseStringSlice(entry["args"]),
			Enabled:   shared.BoolArg(fmt.Sprint(entry["enabled"]), true),
		})
	}
	return servers
}

func parseMountArisInput(raw any) mounts.ArisInput {
	entry, ok := raw.(map[string]any)
	if !ok {
		return mounts.ArisInput{}
	}
	return mounts.ArisInput{
		Available:         shared.BoolArg(fmt.Sprint(entry["available"]), false),
		BundleVersion:     strings.TrimSpace(shared.StringArg(entry, "bundleVersion", "")),
		LLMChatServerPath: strings.TrimSpace(shared.StringArg(entry, "llmChatServerPath", "")),
		SkillCount:        shared.IntArg(fmt.Sprint(entry["skillCount"]), 0),
		BridgeAvailable:   shared.BoolArg(fmt.Sprint(entry["bridgeAvailable"]), false),
		Error:             strings.TrimSpace(shared.StringArg(entry, "error", "")),
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
	resolvedRouting, hasResolvedRouting := resolveRoutingMetadataWithProviders(
		params,
		s.availableProviders(),
	)
	if !hasResolvedRouting {
		return nil, &shared.RPCError{
			Code:    -32602,
			Message: "ROUTING_REQUIRED",
		}
	}

	sessionID := strings.TrimSpace(shared.StringArg(params, "sessionId", ""))
	threadID := strings.TrimSpace(shared.StringArg(params, "threadId", sessionID))
	if resolvedRouting.Unavailable {
		response := mergeRoutingResponse(map[string]any{
			"success":            false,
			"error":              resolvedRouting.UnavailableMessage,
			"unavailable":        true,
			"unavailableCode":    resolvedRouting.UnavailableCode,
			"unavailableMessage": resolvedRouting.UnavailableMessage,
		}, resolvedRouting)
		return response, nil
	}
	executionParams := buildResolvedExecutionParams(params, resolvedRouting)
	mode := strings.TrimSpace(shared.StringArg(executionParams, "mode", "single-agent"))
	provider := strings.TrimSpace(shared.StringArg(executionParams, "provider", ""))

	session := s.getOrCreateSession(sessionID, threadID)
	session.mode = mode
	if provider != "" {
		session.provider = provider
	}

	prompt := strings.TrimSpace(shared.StringArg(executionParams, "taskPrompt", ""))
	if prompt != "" {
		session.history = append(session.history, "USER: "+prompt)
	}
	turnID := fmt.Sprintf("turn-%d", time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	s.setSessionCancel(sessionID, cancel)
	defer s.clearSessionCancel(sessionID)

	notify := task.notify
	s.emitSessionUpdate(session, notify, turnID, map[string]any{
		"type":    "status",
		"event":   "started",
		"message": "session started",
		"pending": true,
		"error":   false,
	})

	if mode == router.ExecutionTargetGatewayChat || mode == router.ExecutionTargetGateway {
		result := s.runGateway(
			ctx,
			task.req.Method,
			session,
			executionParams,
			turnID,
			notify,
		)
		if result.err != nil {
			return nil, result.err
		}
		result.response = mergeRoutingResponse(result.response, resolvedRouting)
		return result.response, nil
	}

	if mode == "multi-agent" {
		result := s.runMultiAgent(ctx, session, executionParams, turnID, notify)
		if result.err != nil {
			return nil, result.err
		}
		result.response = mergeRoutingResponse(result.response, resolvedRouting)
		if err := recordRoutingSuccess(params, resolvedRouting, result.response); err != nil {
			return nil, &shared.RPCError{Code: -32001, Message: err.Error()}
		}
		return result.response, nil
	}

	result := s.runSingleAgent(
		ctx,
		task.req.Method,
		session,
		executionParams,
		turnID,
		notify,
	)
	if result.err != nil {
		return nil, result.err
	}
	result.response = mergeRoutingResponse(result.response, resolvedRouting)
	if err := recordRoutingSuccess(params, resolvedRouting, result.response); err != nil {
		return nil, &shared.RPCError{Code: -32001, Message: err.Error()}
	}
	return result.response, nil
}

func (s *Server) runSingleAgent(
	ctx context.Context,
	method string,
	session *session,
	params map[string]any,
	turnID string,
	notify func(map[string]any),
) taskResult {
	provider := session.provider
	if provider == "" {
		provider = strings.TrimSpace(shared.StringArg(params, "provider", "codex"))
	}
	workingDirectory := strings.TrimSpace(
		shared.StringArg(params, "workingDirectory", ""),
	)
	_, effectiveWorkingDirectory := shared.NormalizeProviderWorkingDirectory(
		provider,
		workingDirectory,
	)

	if syncedProvider, ok := s.syncedProviderByID(provider); ok {
		response, err := s.runSingleAgentViaExternalProvider(
			ctx,
			syncedProvider,
			method,
			params,
			notify,
		)
		if err == nil {
			result := asMap(response["result"])
			if len(result) == 0 {
				result = response
			}
			if _, exists := result["provider"]; !exists {
				result["provider"] = provider
			}
			if _, exists := result["mode"]; !exists {
				result["mode"] = "single-agent"
			}
			if _, exists := result["turnId"]; !exists {
				result["turnId"] = turnID
			}
			if _, exists := result["effectiveWorkingDirectory"]; !exists && effectiveWorkingDirectory != "" {
				result["effectiveWorkingDirectory"] = effectiveWorkingDirectory
			}
			return taskResult{response: enrichSingleAgentResultArtifacts(result, params)}
		}
		s.emitSessionUpdate(session, notify, turnID, map[string]any{
			"type":    "status",
			"event":   "completed",
			"message": err.Error(),
			"pending": false,
			"error":   true,
		})
		return taskResult{
			response: map[string]any{
				"success":  false,
				"error":    err.Error(),
				"turnId":   turnID,
				"mode":     "single-agent",
				"provider": provider,
			},
		}
	}

	s.emitSessionUpdate(session, notify, turnID, map[string]any{
		"type":    "status",
		"event":   "completed",
		"message": "provider is not advertised by the bridge",
		"pending": false,
		"error":   true,
	})
	return taskResult{
		response: map[string]any{
			"success":  false,
			"error":    "provider is not advertised by the bridge",
			"turnId":   turnID,
			"mode":     "single-agent",
			"provider": provider,
		},
	}
}

func (s *Server) runMultiAgent(
	ctx context.Context,
	session *session,
	params map[string]any,
	turnID string,
	notify func(map[string]any),
) taskResult {
	prompt := shared.ComposeHistoryPrompt(session.history)
	if prompt == "" {
		prompt = strings.TrimSpace(shared.StringArg(params, "taskPrompt", ""))
	}
	prompt = shared.AugmentPromptWithAttachments(prompt, params)

	baseURL := shared.NormalizeBaseURL(
		shared.StringArg(params, "aiGatewayBaseUrl", os.Getenv("AI_GATEWAY_BASE_URL")),
	)
	apiKey := strings.TrimSpace(shared.StringArg(params, "aiGatewayApiKey", os.Getenv("AI_GATEWAY_API_KEY")))
	model := strings.TrimSpace(
		shared.StringArg(
			params,
			"model",
			shared.EnvOrDefault("ACP_MULTI_AGENT_MODEL", "gpt-4o"),
		),
	)
	if model == "" {
		model = "gpt-4o"
	}

	s.emitSessionUpdate(session, notify, turnID, map[string]any{
		"type":      "step",
		"mode":      "multi-agent",
		"title":     "Planner",
		"message":   "Preparing multi-agent run",
		"pending":   false,
		"error":     false,
		"role":      "architect",
		"iteration": 1,
		"score":     0,
	})

	if apiKey == "" {
		errMsg := "aiGatewayApiKey is required for multi-agent mode"
		s.emitSessionUpdate(session, notify, turnID, map[string]any{
			"type":    "status",
			"mode":    "multi-agent",
			"message": errMsg,
			"pending": false,
			"error":   true,
		})
		return taskResult{
			response: map[string]any{
				"success": false,
				"error":   errMsg,
				"turnId":  turnID,
				"mode":    "multi-agent",
			},
		}
	}

	messages := []map[string]string{
		{
			"role":    "system",
			"content": "You are a multi-agent coordinator. Return concise actionable output.",
		},
		{"role": "user", "content": prompt},
	}
	output, err := shared.CallOpenAICompatibleCtx(
		ctx,
		baseURL,
		apiKey,
		model,
		messages,
	)
	if err != nil {
		s.emitSessionUpdate(session, notify, turnID, map[string]any{
			"type":    "status",
			"mode":    "multi-agent",
			"message": err.Error(),
			"pending": false,
			"error":   true,
		})
		return taskResult{
			response: map[string]any{
				"success": false,
				"error":   err.Error(),
				"turnId":  turnID,
				"mode":    "multi-agent",
			},
		}
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

func (s *Server) emitSessionUpdate(
	session *session,
	notify func(map[string]any),
	turnID string,
	payload map[string]any,
) {
	if notify == nil {
		return
	}
	s.mu.Lock()
	session.seq++
	seq := session.seq
	s.mu.Unlock()
	params := map[string]any{
		"sessionId": session.sessionID,
		"threadId":  session.threadID,
		"turnId":    turnID,
		"seq":       seq,
	}
	for key, value := range payload {
		params[key] = value
	}
	notify(shared.NotificationEnvelope("session.update", params))
}

func (s *Server) getOrCreateSession(sessionID, threadID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[sessionID]; ok {
		if threadID != "" {
			session.threadID = threadID
		}
		session.closed = false
		return session
	}
	session := &session{sessionID: sessionID, threadID: threadID}
	s.sessions[sessionID] = session
	return session
}

func (s *Server) resetSession(sessionID, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = &session{
		sessionID: sessionID,
		threadID:  threadID,
		history:   []string{},
	}
}

func (s *Server) setSessionCancel(sessionID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[sessionID]; ok {
		session.cancel = cancel
	}
}

func (s *Server) clearSessionCancel(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[sessionID]; ok {
		session.cancel = nil
	}
}

func (s *Server) cancelSession(sessionID string) bool {
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	cancel := session.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

func (s *Server) closeSession(sessionID string) bool {
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	cancel := session.cancel
	session.closed = true
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}
