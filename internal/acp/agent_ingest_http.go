package acp

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"xworkmate-bridge/internal/shared"
)

// One-way forwarding of agent session facts to the QMD shared task context.
//
// ChatGPT / Claude web and mobile extensions cannot read local session
// directories, so they submit extracted facts here and Bridge forwards them to
// QMD, which owns the schema and the merge rules. Bridge stores nothing, and
// the route is write-only: there is no path through Bridge that reads shared
// context back.
const (
	agentAPIPrefix                = "/api/v1/agent/"
	agentIngestPath               = "/api/v1/agent/ingest"
	agentIngestRequestMaxBytes    = 128 * 1024
	agentIngestResponseMaxBytes   = 64 * 1024
	agentIngestUpstreamTimeoutSec = 15
)

func newQMDIngestClient() *http.Client {
	return &http.Client{
		Timeout: agentIngestUpstreamTimeoutSec * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (s *Server) handleAgentIngest(w http.ResponseWriter, r *http.Request) {
	shared.ApplyCORS(w, r, s.allowedOrigins)
	if r.URL.Path != agentIngestPath {
		writeTaskSessionProxyError(w, http.StatusNotFound, "route_not_found", "agent route not found")
		return
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeTaskSessionProxyError(w, http.StatusMethodNotAllowed, "method_not_allowed", "agent ingest is write-only")
		return
	}
	if !taskSessionBearerPresent(r.Header.Get("Authorization")) || !s.authorized(r) {
		writeTaskSessionProxyError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer authorization")
		return
	}
	target, err := qmdIngestTarget(s.qmdIngestAPIURL)
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
	for _, name := range []string{"Content-Type", "X-Request-Id", "Traceparent", "Tracestate"} {
		if value := r.Header.Get(name); value != "" {
			upstreamRequest.Header.Set(name, value)
		}
	}
	// The caller's credential authenticates them to Bridge only; QMD trusts Bridge.
	upstreamRequest.Header.Set("Authorization", "Bearer "+s.qmdIngestToken)
	upstreamRequest.ContentLength = r.ContentLength

	upstreamResponse, err := s.qmdIngestClient.Do(upstreamRequest)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeTaskSessionProxyError(w, http.StatusRequestEntityTooLarge, "request_too_large", "agent ingest request exceeds 131072 bytes")
			return
		}
		log.Printf("level=error component=agent_ingest event=upstream_failed error=%q", err)
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_ingest_unavailable", "QMD ingest is unavailable")
		return
	}
	defer func() {
		if err := upstreamResponse.Body.Close(); err != nil {
			log.Printf("level=error component=agent_ingest event=upstream_body_close_failed error=%q", err)
		}
	}()
	if upstreamResponse.ContentLength > agentIngestResponseMaxBytes {
		writeTaskSessionProxyError(w, http.StatusBadGateway, "qmd_response_too_large", "QMD ingest response exceeds the Bridge limit")
		return
	}
	if contentType := upstreamResponse.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(upstreamResponse.StatusCode)
	if _, err := io.Copy(w, io.LimitReader(upstreamResponse.Body, agentIngestResponseMaxBytes)); err != nil {
		log.Printf("level=error component=agent_ingest event=response_stream_failed error=%q", err)
	}
}

func qmdIngestTarget(configuredBase string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(configuredBase))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("invalid QMD ingest API URL")
	}
	trimmed := strings.TrimRight(base.Path, "/")
	if !strings.HasSuffix(trimmed, agentIngestPath) {
		trimmed = trimmed + agentIngestPath
	}
	base.Path = trimmed
	base.RawPath = ""
	return base, nil
}
