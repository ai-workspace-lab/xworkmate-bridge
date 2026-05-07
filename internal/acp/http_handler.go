package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"xworkmate-bridge/internal/shared"
)

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("xworkmate-bridge is running"))
		case "/api/ping":
			if !s.authorized(r) {
				shared.WriteJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
				return
			}
			info := ParseImageVersionInfo(os.Getenv("IMAGE"))
			resp := map[string]any{
				"status":  "ok",
				"image":   info.ImageRef,
				"tag":     info.Tag,
				"commit":  info.Commit,
				"version": info.Version,
			}
			body, _ := json.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case "/acp/rpc":
			s.HandleRPC(w, r)
		case "/acp":
			s.HandleWebSocket(w, r)
		case openClawArtifactDownloadPath:
			s.HandleOpenClawArtifactDownload(w, r)
		default:
			if r.URL.Path == "/gateway/openclaw" {
				s.HandleOpenClawGatewayRPC(w, r)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/acp-server/") {
				s.HandleDisabledProviderDirectPath(w, r)
				return
			}
			http.NotFound(w, r)
		}
	})
}

func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if !shared.OriginAllowed(origin, s.allowedOrigins) {
		shared.WriteJSONError(w, nil, http.StatusForbidden, -32003, fmt.Sprintf("origin not allowed: %s", origin))
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
		request.Params = injectInboundAuthorizationHeader(request.Params, r.Header.Get("Authorization"))
		response, rpcErr := s.handleRequest(request, notify)
		if request.ID == nil {
			continue
		}
		if rpcErr != nil {
			notify(shared.ErrorEnvelope(request.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data))
			continue
		}
		notify(shared.ResultEnvelope(request.ID, response))
	}
}

func (s *Server) HandleRPC(w http.ResponseWriter, r *http.Request) {
	s.handleRPCWithTransform(w, r, rejectOpenClawTaskSubmitOnCanonicalRPC)
}

func (s *Server) HandleOpenClawGatewayRPC(w http.ResponseWriter, r *http.Request) {
	s.handleRPCWithTransform(w, r, forceOpenClawGatewayRequest)
}

func (s *Server) HandleDisabledProviderDirectPath(w http.ResponseWriter, r *http.Request) {
	shared.ApplyCORS(w, r, s.allowedOrigins)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.authorized(r) {
		shared.WriteJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGone)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    -32004,
			"message": "PROVIDER_DIRECT_PATH_DISABLED: use /acp/rpc provider catalog and routing",
		},
		"type": "res",
		"ok":   false,
	})
}

func (s *Server) handleRPCWithTransform(
	w http.ResponseWriter,
	r *http.Request,
	transform func(shared.RPCRequest) (shared.RPCRequest, *shared.RPCError),
) {
	shared.ApplyCORS(w, r, s.allowedOrigins)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		shared.WriteJSONError(w, nil, http.StatusMethodNotAllowed, -32600, "method not allowed")
		return
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if !shared.OriginAllowed(origin, s.allowedOrigins) {
		shared.WriteJSONError(w, nil, http.StatusForbidden, -32003, fmt.Sprintf("origin not allowed: %s", origin))
		return
	}
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32600, "invalid body")
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(payload))

	if !s.authorized(r) {
		shared.WriteJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
		return
	}
	request, err := shared.DecodeRPCRequest(payload)
	if err != nil {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32700, err.Error())
		return
	}
	request.Params = injectInboundAuthorizationHeader(request.Params, r.Header.Get("Authorization"))
	if transform != nil {
		transformed, rpcErr := transform(request)
		if rpcErr != nil {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(shared.ErrorEnvelope(request.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data))
			return
		}
		request = transformed
	}

	accept := strings.ToLower(r.Header.Get("Accept"))
	stream := strings.Contains(accept, "text/event-stream")
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	}

	streamWriter := newSafeSSEStream(r.Context(), w)
	writeNotification := func(message map[string]any) {
		if !stream {
			return
		}
		streamWriter.write(message)
	}
	defer streamWriter.close()

	response, rpcErr := s.handleRequest(request, writeNotification)
	if request.ID == nil {
		if stream {
			streamWriter.done()
		}
		return
	}
	if rpcErr != nil {
		envelope := shared.ErrorEnvelope(request.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		if stream {
			streamWriter.write(envelope)
			streamWriter.done()
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(envelope)
		return
	}
	if r.URL.Path == "/gateway/openclaw" {
		stripOpenClawArtifactInlineContent(response)
	}
	if stream {
		streamWriter.write(shared.ResultEnvelope(request.ID, response))
		streamWriter.done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(shared.ResultEnvelope(request.ID, response))
}

type safeSSEStream struct {
	ctx     context.Context
	w       http.ResponseWriter
	flusher http.Flusher
	closed  atomic.Bool
	mu      sync.Mutex
}

func newSafeSSEStream(ctx context.Context, w http.ResponseWriter) *safeSSEStream {
	flusher, _ := w.(http.Flusher)
	return &safeSSEStream{ctx: ctx, w: w, flusher: flusher}
}

func (s *safeSSEStream) write(payload map[string]any) bool {
	return s.writeRaw(func() error {
		return shared.WriteSSE(s.w, payload)
	})
}

func (s *safeSSEStream) done() bool {
	return s.writeRaw(func() error {
		_, err := s.w.Write([]byte("data: [DONE]\n\n"))
		return err
	})
}

func (s *safeSSEStream) close() {
	s.closed.Store(true)
}

func (s *safeSSEStream) writeRaw(write func() error) (ok bool) {
	if s == nil || s.closed.Load() {
		return false
	}
	select {
	case <-s.ctx.Done():
		s.closed.Store(true)
		return false
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return false
	}
	defer func() {
		if recover() != nil {
			s.closed.Store(true)
			ok = false
		}
	}()
	if err := write(); err != nil {
		s.closed.Store(true)
		return false
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return true
}

func forceOpenClawGatewayRequest(request shared.RPCRequest) (shared.RPCRequest, *shared.RPCError) {
	method := strings.TrimSpace(request.Method)
	switch method {
	case "session.start", "session.message":
	default:
		return request, &shared.RPCError{Code: -32601, Message: "OPENCLAW_GATEWAY_METHOD_NOT_ALLOWED: " + method}
	}
	params := shared.AsMap(request.Params)
	if params == nil {
		params = map[string]any{}
	}
	if parseBool(params["multiAgent"]) {
		return request, &shared.RPCError{Code: -32602, Message: "OPENCLAW_GATEWAY_CONFLICT: multiAgent is not supported on /gateway/openclaw"}
	}
	if provider := strings.TrimSpace(shared.StringArg(params, "provider", "")); provider != "" {
		return request, &shared.RPCError{Code: -32602, Message: "OPENCLAW_GATEWAY_CONFLICT: provider must not be set on /gateway/openclaw"}
	}
	for _, key := range []string{"executionTarget", "requestedExecutionTarget"} {
		if target := strings.TrimSpace(shared.StringArg(params, key, "")); target != "" && !strings.EqualFold(target, "gateway") {
			return request, &shared.RPCError{Code: -32602, Message: "OPENCLAW_GATEWAY_CONFLICT: " + key + " must be gateway"}
		}
	}
	for _, key := range []string{"preferredGatewayProviderId", "gatewayProviderId", "gatewayProvider"} {
		if provider := strings.TrimSpace(shared.StringArg(params, key, "")); provider != "" && !strings.EqualFold(provider, "openclaw") {
			return request, &shared.RPCError{Code: -32602, Message: "OPENCLAW_GATEWAY_CONFLICT: gateway provider must be openclaw"}
		}
	}
	routing := shared.AsMap(params["routing"])
	if routing == nil {
		routing = map[string]any{}
	}
	if provider := strings.TrimSpace(shared.StringArg(routing, "explicitProviderId", "")); provider != "" {
		return request, &shared.RPCError{Code: -32602, Message: "OPENCLAW_GATEWAY_CONFLICT: explicitProviderId must not be set on /gateway/openclaw"}
	}
	if target := strings.TrimSpace(shared.StringArg(routing, "explicitExecutionTarget", "")); target != "" && !strings.EqualFold(target, "gateway") {
		return request, &shared.RPCError{Code: -32602, Message: "OPENCLAW_GATEWAY_CONFLICT: explicitExecutionTarget must be gateway"}
	}
	for _, key := range []string{"preferredGatewayProviderId", "gatewayProviderId", "gatewayProvider"} {
		if provider := strings.TrimSpace(shared.StringArg(routing, key, "")); provider != "" && !strings.EqualFold(provider, "openclaw") {
			return request, &shared.RPCError{Code: -32602, Message: "OPENCLAW_GATEWAY_CONFLICT: gateway provider must be openclaw"}
		}
	}
	routing["routingMode"] = "explicit"
	routing["explicitExecutionTarget"] = "gateway"
	routing["preferredGatewayProviderId"] = "openclaw"
	delete(routing, "explicitProviderId")
	params["routing"] = routing
	params["requestedExecutionTarget"] = "gateway"
	params["executionTarget"] = "gateway"
	request.Params = params
	return request, nil
}

func rejectOpenClawTaskSubmitOnCanonicalRPC(request shared.RPCRequest) (shared.RPCRequest, *shared.RPCError) {
	method := strings.TrimSpace(request.Method)
	if method != "session.start" && method != "session.message" {
		return request, nil
	}
	params := shared.AsMap(request.Params)
	if parseBool(params["multiAgent"]) || strings.EqualFold(strings.TrimSpace(shared.StringArg(params, "mode", "")), "multi-agent") {
		return request, nil
	}
	if requestUsesOpenClawGatewaySubmit(params) {
		return request, &shared.RPCError{Code: -32602, Message: "OPENCLAW_TASK_ENDPOINT_REQUIRED: use /gateway/openclaw for OpenClaw task submission"}
	}
	return request, nil
}

func requestUsesOpenClawGatewaySubmit(params map[string]any) bool {
	if len(params) == 0 {
		return false
	}
	if requestHasExplicitAgentRouting(params) {
		return false
	}
	for _, key := range []string{"executionTarget", "requestedExecutionTarget"} {
		if isGatewayExecutionTarget(shared.StringArg(params, key, "")) {
			return true
		}
	}
	for _, key := range []string{"gatewayProvider", "gatewayProviderId"} {
		if isOpenClawProvider(shared.StringArg(params, key, "")) {
			return true
		}
	}
	routing := shared.AsMap(params["routing"])
	if isGatewayExecutionTarget(shared.StringArg(routing, "explicitExecutionTarget", "")) {
		return true
	}
	for _, key := range []string{"preferredGatewayProviderId", "gatewayProviderId", "gatewayProvider"} {
		if isOpenClawProvider(shared.StringArg(routing, key, "")) {
			return true
		}
	}
	return false
}

func requestHasExplicitAgentRouting(params map[string]any) bool {
	for _, key := range []string{"executionTarget", "requestedExecutionTarget"} {
		if isAgentExecutionTarget(shared.StringArg(params, key, "")) {
			return true
		}
	}
	if provider := strings.TrimSpace(shared.StringArg(params, "provider", "")); provider != "" && !isOpenClawProvider(provider) {
		return true
	}
	routing := shared.AsMap(params["routing"])
	if isAgentExecutionTarget(shared.StringArg(routing, "explicitExecutionTarget", "")) {
		return true
	}
	if provider := strings.TrimSpace(shared.StringArg(routing, "explicitProviderId", "")); provider != "" && !isOpenClawProvider(provider) {
		return true
	}
	return false
}

func isAgentExecutionTarget(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return normalized == "agent" || normalized == "single-agent" || normalized == "singleagent"
}

func isGatewayExecutionTarget(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "gateway")
}

func isOpenClawProvider(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "openclaw")
}

func (s *Server) authorized(r *http.Request) bool {
	if s == nil {
		return false
	}
	if s.authService == nil {
		return true
	}

	type validator interface {
		ValidateAuthorizationHeader(string) bool
	}

	if v, ok := s.authService.(validator); ok {
		return v.ValidateAuthorizationHeader(r.Header.Get("Authorization"))
	}
	return true
}

func injectInboundAuthorizationHeader(params map[string]any, authorization string) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	authorization = strings.TrimSpace(authorization)
	if authorization != "" {
		params["bridgeAuthorizationHeader"] = authorization
	}
	return params
}
