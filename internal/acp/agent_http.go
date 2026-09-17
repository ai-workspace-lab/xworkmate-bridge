package acp

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"xworkmate-bridge/internal/shared"
)

// Stateless forwarding of the shared agent task context to QMD.
//
// Every client — ChatGPT / Claude web and mobile extensions and CLI / APP
// plugins (Claude Code, Codex, Antigravity, OpenCode) — syncs both ways through
// these routes: it reads tasks, task lists and shared memory, and submits
// session facts. QMD owns the schema and the merge rules; Bridge authenticates
// the caller, forwards allowlisted routes with QMD's own credential, and stores
// nothing.
//
// Remote mode: /api/v1/agent/mcp passes MCP Streamable HTTP through to QMD's
// /mcp so clients can use QMD's MCP tools when QMD runs on another host.
const (
	agentAPIPrefix             = "/api/v1/agent/"
	agentIngestPath            = "/api/v1/agent/ingest"
	agentCatalogPath           = "/api/v1/agent/catalog"
	agentThreadsPath           = "/api/v1/agent/threads"
	agentMemoryPath            = "/api/v1/agent/memory"
	agentSyncPath              = "/api/v1/agent/sync"
	agentMCPPath               = "/api/v1/agent/mcp"
	qmdMCPPath                 = "/mcp"
	agentIngestRequestMaxBytes = 128 * 1024
	// Read routes are paged by QMD (at most 200 records per page).
	agentReadResponseMaxBytes  = 2 * 1024 * 1024
	agentWriteResponseMaxBytes = 64 * 1024
	agentMCPRequestMaxBytes    = 1024 * 1024
	agentMCPResponseMaxBytes   = 4 * 1024 * 1024
	agentUpstreamTimeout       = 15 * time.Second
)

var agentBriefingPathPattern = regexp.MustCompile(`^/api/v1/agent/threads/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/briefing$`)

// agentReadQueryParams lists, per read route, the query parameters QMD accepts.
// Anything else is dropped rather than forwarded.
var agentReadQueryParams = map[string][]string{
	agentCatalogPath: {"scope", "limit", "offset"},
	agentThreadsPath: {"scope", "state", "limit", "offset"},
	agentMemoryPath:  {"q", "scope", "kind", "limit", "offset"},
	agentSyncPath:    {"cursor", "limit"},
	"briefing":       {"events"},
}

func newQMDIngestClient() *http.Client {
	return &http.Client{
		Timeout: agentUpstreamTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// newQMDMCPClient has no overall timeout: an MCP GET may hold an SSE stream
// open. Connecting and receiving response headers are still bounded, and the
// stream ends when the caller disconnects.
func newQMDMCPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (s *Server) handleAgentAPI(w http.ResponseWriter, r *http.Request) {
	shared.ApplyCORS(w, r, s.allowedOrigins)
	path := r.URL.Path
	switch {
	case path == agentIngestPath:
		s.handleAgentIngest(w, r)
	case path == agentMCPPath:
		s.handleAgentMCP(w, r)
	case path == agentCatalogPath || path == agentThreadsPath || path == agentMemoryPath || path == agentSyncPath:
		s.handleAgentRead(w, r, agentReadQueryParams[path])
	case agentBriefingPathPattern.MatchString(path):
		s.handleAgentRead(w, r, agentReadQueryParams["briefing"])
	default:
		writeTaskSessionProxyError(w, http.StatusNotFound, "route_not_found", "agent route not found")
	}
}

// agentPreflight answers OPTIONS, enforces the method and the caller's bearer.
// It reports whether the request should continue.
func (s *Server) agentPreflight(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	allow := strings.Join(methods, ", ")
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", allow)
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	allowed := false
	for _, method := range methods {
		if r.Method == method {
			allowed = true
			break
		}
	}
	if !allowed {
		w.Header().Set("Allow", allow)
		writeTaskSessionProxyError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this agent route")
		return false
	}
	if !taskSessionBearerPresent(r.Header.Get("Authorization")) || !s.authorized(r) {
		writeTaskSessionProxyError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer authorization")
		return false
	}
	return true
}

func (s *Server) handleAgentIngest(w http.ResponseWriter, r *http.Request) {
	if !s.agentPreflight(w, r, http.MethodPost) {
		return
	}
	target, err := qmdEndpointTarget(s.qmdIngestAPIURL, agentIngestPath)
	if err != nil || s.qmdIngestToken == "" || s.qmdIngestClient == nil {
		writeTaskSessionProxyError(w, http.StatusServiceUnavailable, "qmd_ingest_unavailable", "QMD ingest is not configured")
		return
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json") {
		writeTaskSessionProxyError(w, http.StatusUnsupportedMediaType, "json_required", "Content-Type application/json is required")
		return
	}
	if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		writeTaskSessionProxyError(w, http.StatusUnsupportedMediaType, "content_encoding_not_supported", "compressed agent ingest requests are not supported")
		return
	}
	if r.ContentLength > agentIngestRequestMaxBytes {
		writeTaskSessionProxyError(w, http.StatusRequestEntityTooLarge, "request_too_large", "agent ingest request exceeds 131072 bytes")
		return
	}
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), http.MaxBytesReader(w, r.Body, agentIngestRequestMaxBytes))
	if err != nil {
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_ingest_request_failed", "failed to create QMD request")
		return
	}
	copyAgentHeaders(upstreamRequest.Header, r.Header, "Content-Type", "X-Request-Id", "Traceparent", "Tracestate")
	// The caller's credential authenticates them to Bridge only; QMD trusts Bridge.
	upstreamRequest.Header.Set("Authorization", "Bearer "+s.qmdIngestToken)
	upstreamRequest.ContentLength = r.ContentLength
	s.forwardAgentBuffered(w, r, s.qmdIngestClient, upstreamRequest, agentWriteResponseMaxBytes, "agent_ingest")
}

func (s *Server) handleAgentRead(w http.ResponseWriter, r *http.Request, params []string) {
	if !s.agentPreflight(w, r, http.MethodGet) {
		return
	}
	target, err := qmdEndpointTarget(s.qmdIngestAPIURL, r.URL.Path)
	if err != nil || s.qmdIngestToken == "" || s.qmdIngestClient == nil {
		writeTaskSessionProxyError(w, http.StatusServiceUnavailable, "qmd_read_unavailable", "QMD is not configured")
		return
	}
	incoming := r.URL.Query()
	query := url.Values{}
	for _, name := range params {
		if values, ok := incoming[name]; ok && len(values) > 0 {
			query.Set(name, values[0])
		}
	}
	target.RawQuery = query.Encode()
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_read_request_failed", "failed to create QMD request")
		return
	}
	copyAgentHeaders(upstreamRequest.Header, r.Header, "Accept", "X-Request-Id", "Traceparent", "Tracestate")
	upstreamRequest.Header.Set("Authorization", "Bearer "+s.qmdIngestToken)
	s.forwardAgentBuffered(w, r, s.qmdIngestClient, upstreamRequest, agentReadResponseMaxBytes, "agent_read")
}

// handleAgentMCP passes MCP Streamable HTTP through to QMD. Session identity
// travels in the Mcp-Session-Id header, so every request of a session must
// reach the same QMD instance (single instance, or sticky routing upstream).
func (s *Server) handleAgentMCP(w http.ResponseWriter, r *http.Request) {
	if !s.agentPreflight(w, r, http.MethodPost, http.MethodGet, http.MethodDelete) {
		return
	}
	target, err := qmdEndpointTarget(s.qmdIngestAPIURL, qmdMCPPath)
	if err != nil || s.qmdMCPToken == "" || s.qmdMCPClient == nil {
		writeTaskSessionProxyError(w, http.StatusServiceUnavailable, "qmd_mcp_unavailable", "QMD MCP is not configured")
		return
	}
	var body io.Reader
	if r.Method == http.MethodPost {
		if r.ContentLength > agentMCPRequestMaxBytes {
			writeTaskSessionProxyError(w, http.StatusRequestEntityTooLarge, "request_too_large", "MCP request exceeds 1048576 bytes")
			return
		}
		body = http.MaxBytesReader(w, r.Body, agentMCPRequestMaxBytes)
	}
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_mcp_request_failed", "failed to create QMD request")
		return
	}
	copyAgentHeaders(upstreamRequest.Header, r.Header,
		"Content-Type", "Accept", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id",
		"X-Request-Id", "Traceparent", "Tracestate")
	upstreamRequest.Header.Set("Authorization", "Bearer "+s.qmdMCPToken)
	if r.Method == http.MethodPost {
		upstreamRequest.ContentLength = r.ContentLength
	}

	upstreamResponse, err := s.qmdMCPClient.Do(upstreamRequest)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeTaskSessionProxyError(w, http.StatusRequestEntityTooLarge, "request_too_large", "MCP request exceeds 1048576 bytes")
			return
		}
		log.Printf("level=error component=agent_mcp event=upstream_failed error=%q", err)
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_mcp_unavailable", "QMD MCP is unavailable")
		return
	}
	defer closeAgentBody(upstreamResponse, "agent_mcp")

	copyAgentHeaders(w.Header(), upstreamResponse.Header, "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Retry-After")
	w.Header().Set("Cache-Control", "no-store")

	if strings.HasPrefix(strings.ToLower(upstreamResponse.Header.Get("Content-Type")), "text/event-stream") {
		w.WriteHeader(upstreamResponse.StatusCode)
		streamAgentResponse(w, upstreamResponse.Body)
		return
	}
	if upstreamResponse.ContentLength > agentMCPResponseMaxBytes {
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_response_too_large", "QMD MCP response exceeds the Bridge limit")
		return
	}
	writeAgentLimitedBody(w, upstreamResponse, agentMCPResponseMaxBytes, "agent_mcp")
}

func (s *Server) forwardAgentBuffered(w http.ResponseWriter, r *http.Request, client *http.Client, upstreamRequest *http.Request, maxBytes int64, component string) {
	upstreamResponse, err := client.Do(upstreamRequest)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeTaskSessionProxyError(w, http.StatusRequestEntityTooLarge, "request_too_large", "agent request exceeds the Bridge limit")
			return
		}
		log.Printf("level=error component=%s event=upstream_failed path=%q error=%q", component, r.URL.Path, err)
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_unavailable", "QMD is unavailable")
		return
	}
	defer closeAgentBody(upstreamResponse, component)
	if upstreamResponse.ContentLength > maxBytes {
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_response_too_large", "QMD response exceeds the Bridge limit")
		return
	}
	if contentType := upstreamResponse.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeAgentLimitedBody(w, upstreamResponse, maxBytes, component)
}

// writeAgentLimitedBody buffers up to maxBytes so an oversized body of unknown
// length becomes a clean 502 instead of a truncated 200.
func writeAgentLimitedBody(w http.ResponseWriter, upstreamResponse *http.Response, maxBytes int64, component string) {
	body, err := io.ReadAll(io.LimitReader(upstreamResponse.Body, maxBytes+1))
	if err != nil {
		log.Printf("level=error component=%s event=upstream_read_failed error=%q", component, err)
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_unavailable", "QMD response could not be read")
		return
	}
	if int64(len(body)) > maxBytes {
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_response_too_large", "QMD response exceeds the Bridge limit")
		return
	}
	w.WriteHeader(upstreamResponse.StatusCode)
	if _, err := w.Write(body); err != nil {
		log.Printf("level=error component=%s event=response_write_failed error=%q", component, err)
	}
}

func streamAgentResponse(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				log.Printf("level=warn component=agent_mcp event=stream_write_failed error=%q", err)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				log.Printf("level=warn component=agent_mcp event=stream_read_failed error=%q", readErr)
			}
			return
		}
	}
}

func copyAgentHeaders(target, source http.Header, names ...string) {
	for _, name := range names {
		if value := source.Get(name); value != "" {
			target.Set(name, value)
		}
	}
}

func closeAgentBody(response *http.Response, component string) {
	if err := response.Body.Close(); err != nil {
		log.Printf("level=warn component=%s event=upstream_body_close_failed error=%q", component, err)
	}
}

// qmdEndpointTarget joins the configured QMD origin (optionally with a base
// path) and an endpoint path. A base that already ends in the ingest path, as
// early deployments configured it, is reduced to its origin.
func qmdEndpointTarget(configuredBase string, endpointPath string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(configuredBase))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("invalid QMD API URL")
	}
	trimmed := strings.TrimSuffix(strings.TrimRight(base.Path, "/"), agentIngestPath)
	base.Path = strings.TrimRight(trimmed, "/") + endpointPath
	base.RawPath = ""
	return base, nil
}
