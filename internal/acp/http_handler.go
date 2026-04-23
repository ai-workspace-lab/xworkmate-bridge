package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"

	"xworkmate-bridge/internal/shared"
)

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
		shared.WriteJSONError(w, nil, http.StatusOK, rpcErr.Code, rpcErr.Message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(shared.ResultEnvelope(nil, result))
}

func (s *Server) HandleGatewayRPCAlias(w http.ResponseWriter, r *http.Request) {
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
	shared.ApplyCORS(w, r, s.allowedOrigins)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		shared.WriteJSONError(w, nil, http.StatusMethodNotAllowed, -32600, "method not allowed")
		return
	}
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32600, "invalid body")
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(payload))

	if !s.authorized(r) {
		var temp struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(payload, &temp)
		method := strings.TrimSpace(temp.Method)
		if method != "acp.capabilities" && method != "health" {
			shared.WriteJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
			return
		}
	}
	request, err := shared.DecodeRPCRequest(payload)
	if err != nil {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32700, err.Error())
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
	request.Params = injectInboundAuthorizationHeader(params, r.Header.Get("Authorization"))
	response, rpcErr := s.handleRequest(request, nil)
	if request.ID == nil {
		return
	}
	if rpcErr != nil {
		shared.WriteJSONError(w, request.ID, http.StatusOK, rpcErr.Code, rpcErr.Message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(shared.ResultEnvelope(request.ID, response))
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
			notify(shared.ErrorEnvelope(request.ID, rpcErr.Code, rpcErr.Message))
			continue
		}
		notify(shared.ResultEnvelope(request.ID, response))
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
		var temp struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(payload, &temp)
		method := strings.TrimSpace(temp.Method)
		if method != "acp.capabilities" && method != "health" {
			shared.WriteJSONError(w, nil, http.StatusUnauthorized, -32001, "missing bearer authorization")
			return
		}
	}
	request, err := shared.DecodeRPCRequest(payload)
	if err != nil {
		shared.WriteJSONError(w, nil, http.StatusBadRequest, -32700, err.Error())
		return
	}
	request.Params = injectInboundAuthorizationHeader(request.Params, r.Header.Get("Authorization"))

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
			if flusher != nil { flusher.Flush() }
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(envelope)
		return
	}
	if stream {
		shared.WriteSSE(w, shared.ResultEnvelope(request.ID, response))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil { flusher.Flush() }
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
