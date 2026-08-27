package acp

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"xworkmate-bridge/internal/shared"
)

const (
	taskSessionProxyRequestMaxBytes  = 128 * 1024
	taskSessionProxyResponseMaxBytes = 4 * 1024 * 1024
)

var taskSessionProxyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func newAccountsSessionProxyClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (s *Server) handleTaskSessionAPI(w http.ResponseWriter, r *http.Request) {
	shared.ApplyCORS(w, r, s.allowedOrigins)
	if r.Method == http.MethodOptions {
		if !taskSessionProxyRouteAllowed(http.MethodGet, r.URL.Path) && !taskSessionProxyRouteAllowed(http.MethodPost, r.URL.Path) {
			writeTaskSessionProxyError(w, http.StatusNotFound, "route_not_found", "task session route not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.authorized(r) {
		writeTaskSessionProxyError(w, http.StatusUnauthorized, "missing_bearer_authorization", "missing bearer authorization")
		return
	}
	if !taskSessionProxyRouteAllowed(r.Method, r.URL.Path) {
		writeTaskSessionProxyError(w, http.StatusNotFound, "route_not_found", "task session route not found")
		return
	}
	target, err := accountsSessionProxyTarget(s.accountsSessionAPIURL, r.URL)
	if err != nil || s.accountsSessionClient == nil {
		writeTaskSessionProxyError(w, http.StatusServiceUnavailable, "accounts_session_api_unavailable", "Accounts task session API is not configured")
		return
	}

	body := r.Body
	if r.Method == http.MethodPost {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json") {
			writeTaskSessionProxyError(w, http.StatusUnsupportedMediaType, "json_required", "Content-Type application/json is required")
			return
		}
		if r.ContentLength > taskSessionProxyRequestMaxBytes {
			writeTaskSessionProxyError(w, http.StatusRequestEntityTooLarge, "request_too_large", "task session request exceeds 131072 bytes")
			return
		}
		if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
			writeTaskSessionProxyError(w, http.StatusUnsupportedMediaType, "content_encoding_not_supported", "compressed task session requests are not supported")
			return
		}
		body = http.MaxBytesReader(w, r.Body, taskSessionProxyRequestMaxBytes)
	}
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		writeTaskSessionProxyError(w, http.StatusBadGateway, "accounts_session_api_request_failed", "failed to create Accounts request")
		return
	}
	copyTaskSessionRequestHeaders(upstreamRequest.Header, r.Header)
	upstreamRequest.ContentLength = r.ContentLength
	upstreamResponse, err := s.accountsSessionClient.Do(upstreamRequest)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeTaskSessionProxyError(w, http.StatusRequestEntityTooLarge, "request_too_large", "task session request exceeds 131072 bytes")
			return
		}
		writeTaskSessionProxyError(w, http.StatusBadGateway, "accounts_session_api_unavailable", "Accounts task session API is unavailable")
		return
	}
	defer func() {
		if err := upstreamResponse.Body.Close(); err != nil {
			log.Printf("level=error component=session_proxy event=upstream_body_close_failed error=%q", err)
		}
	}()
	if upstreamResponse.ContentLength > taskSessionProxyResponseMaxBytes {
		writeTaskSessionProxyError(w, http.StatusBadGateway, "accounts_response_too_large", "Accounts task session response exceeds the Bridge limit")
		return
	}
	copyTaskSessionResponseHeaders(w.Header(), upstreamResponse.Header)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(upstreamResponse.StatusCode)
	written, copyErr := io.CopyN(w, upstreamResponse.Body, taskSessionProxyResponseMaxBytes)
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		log.Printf("level=error component=session_proxy event=response_stream_failed path=%q error=%q", r.URL.Path, copyErr)
		return
	}
	if written == taskSessionProxyResponseMaxBytes {
		var overflow [1]byte
		if count, readErr := upstreamResponse.Body.Read(overflow[:]); count > 0 {
			log.Printf("level=error component=session_proxy event=response_limit_exceeded path=%q maxBytes=%d", r.URL.Path, taskSessionProxyResponseMaxBytes)
		} else if readErr != nil && !errors.Is(readErr, io.EOF) {
			log.Printf("level=error component=session_proxy event=response_limit_probe_failed path=%q error=%q", r.URL.Path, readErr)
		}
	}
}

func taskSessionProxyRouteAllowed(method, path string) bool {
	segments := strings.Split(strings.Trim(strings.TrimPrefix(path, "/api/v1/"), "/"), "/")
	for _, segment := range segments {
		if !taskSessionProxyIDPattern.MatchString(segment) {
			return false
		}
	}
	switch {
	case len(segments) == 1 && segments[0] == "namespaces":
		return method == http.MethodGet
	case len(segments) == 3 && segments[0] == "namespaces" && segments[2] == "sessions":
		return method == http.MethodGet || method == http.MethodPost
	case len(segments) == 2 && segments[0] == "sessions":
		return method == http.MethodGet
	case len(segments) == 3 && segments[0] == "sessions" && segments[2] == "events":
		return method == http.MethodGet
	case len(segments) == 3 && segments[0] == "sessions" && segments[2] == "messages":
		return method == http.MethodPost
	default:
		return false
	}
}

func accountsSessionProxyTarget(configuredBase string, incoming *url.URL) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(configuredBase))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("invalid Accounts session API URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + incoming.Path
	base.RawPath = ""
	base.RawQuery = incoming.RawQuery
	return base, nil
}

func copyTaskSessionRequestHeaders(target, source http.Header) {
	for _, name := range []string{"Authorization", "Accept", "Content-Type", "X-Request-Id", "Traceparent", "Tracestate"} {
		if value := source.Get(name); value != "" {
			target.Set(name, value)
		}
	}
}

func copyTaskSessionResponseHeaders(target, source http.Header) {
	for _, name := range []string{"Content-Type", "ETag", "Last-Modified", "Retry-After", "WWW-Authenticate", "X-Request-Id"} {
		if value := source.Get(name); value != "" {
			target.Set(name, value)
		}
	}
}

func writeTaskSessionProxyError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}}); err != nil {
		log.Printf("level=error component=session_proxy event=error_response_failed status=%d error=%q", status, err)
	}
}
