package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/shared"
)

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			if strings.HasPrefix(r.URL.Path, "/acp-server/") {
				s.handleLegacyACPServer(w, r)
				return
			}
			http.NotFound(w, r)
		}
	})
}

func (s *Server) handleLegacyACPServer(w http.ResponseWriter, r *http.Request) {
	providerID := strings.TrimPrefix(strings.TrimSpace(r.URL.Path), "/acp-server/")
	providerID = strings.Trim(providerID, "/")
	if providerID == "" {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	compat := s.providers[providerID]
	s.mu.RUnlock()
	if compat == nil {
		http.NotFound(w, r)
		return
	}

	if websocket.IsWebSocketUpgrade(r) {
		s.proxyLegacyACPServerWebSocket(w, r, compat)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"legacy":     true,
		"providerId": providerID,
		"label":      compat.Metadata()["label"],
		"transport":   compat.Metadata()["transport"],
	})
}

func (s *Server) proxyLegacyACPServerWebSocket(w http.ResponseWriter, r *http.Request, compat ProviderCompat) {
	external, ok := compat.(*externalACPCompat)
	if !ok || external == nil {
		http.Error(w, "legacy websocket proxy unavailable", http.StatusNotImplemented)
		return
	}

	upstreamURL := strings.TrimSpace(external.endpoint)
	if upstreamURL == "" {
		http.NotFound(w, r)
		return
	}
	if _, err := url.Parse(upstreamURL); err != nil {
		http.Error(w, "invalid upstream endpoint", http.StatusBadGateway)
		return
	}

	headers := http.Header{}
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		headers.Set("Authorization", auth)
	} else if external.authHeader != "" {
		headers.Set("Authorization", external.authHeader)
	}

	upstream, _, err := websocket.DefaultDialer.Dial(upstreamURL, headers)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	upgrader := shared.StandardWSUpgrader
	upgrader.CheckOrigin = func(req *http.Request) bool {
		return true
	}
	downstream, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = downstream.Close() }()

	errCh := make(chan error, 2)
	go func() { errCh <- copyWSMessages(downstream, upstream) }()
	go func() { errCh <- copyWSMessages(upstream, downstream) }()
	<-errCh
}

func copyWSMessages(dst, src *websocket.Conn) error {
	for {
		messageType, payload, err := src.ReadMessage()
		if err != nil {
			return err
		}
		if err := dst.WriteMessage(messageType, payload); err != nil {
			return err
		}
	}
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
