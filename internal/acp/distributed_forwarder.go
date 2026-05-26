package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"xworkmate-bridge/internal/shared"
)

const distributedForwardedHeader = "X-XWorkmate-Bridge-Forwarded"

type distributedTaskForwarderConfig struct {
	Endpoint string
	Token    string
}

type distributedTaskForwarder struct {
	endpoint   string
	token      string
	httpClient *http.Client
}

func newDistributedTaskForwarder(config distributedTaskForwarderConfig) *distributedTaskForwarder {
	endpoint := strings.TrimRight(strings.TrimSpace(config.Endpoint), "/")
	if endpoint == "" {
		return nil
	}
	return &distributedTaskForwarder{
		endpoint: endpoint,
		token:    strings.TrimSpace(config.Token),
		httpClient: &http.Client{
			Timeout: openClawAgentWaitMaxTimeout + openClawAgentWaitHTTPMargin,
		},
	}
}

func (f *distributedTaskForwarder) shouldForward(r *http.Request, request shared.RPCRequest) bool {
	if f == nil || strings.TrimSpace(f.endpoint) == "" || r == nil {
		return false
	}
	if strings.TrimSpace(r.Header.Get(distributedForwardedHeader)) != "" {
		return false
	}
	method := strings.TrimSpace(request.Method)
	return method == "session.start" || method == "session.message"
}

func (f *distributedTaskForwarder) forward(ctx context.Context, w http.ResponseWriter, r *http.Request, request shared.RPCRequest) bool {
	if !f.shouldForward(r, request) {
		return false
	}
	payload, err := json.Marshal(request)
	if err != nil {
		shared.WriteJSONError(w, request.ID, http.StatusInternalServerError, -32603, "TASK_FORWARD_ENCODE_FAILED")
		return true
	}
	forwardURL, err := f.forwardURL(r.URL.Path)
	if err != nil {
		shared.WriteJSONError(w, request.ID, http.StatusBadGateway, -32060, err.Error())
		return true
	}
	outbound, err := http.NewRequestWithContext(ctx, http.MethodPost, forwardURL, bytes.NewReader(payload))
	if err != nil {
		shared.WriteJSONError(w, request.ID, http.StatusBadGateway, -32060, "TASK_FORWARD_REQUEST_BUILD_FAILED: "+err.Error())
		return true
	}
	outbound.Header.Set("Content-Type", "application/json")
	outbound.Header.Set(distributedForwardedHeader, "1")
	copyForwardHeader(outbound.Header, r.Header, "Accept")
	copyForwardHeader(outbound.Header, r.Header, "Origin")
	if f.token != "" {
		outbound.Header.Set("Authorization", distributedForwardBearerHeader(f.token))
	}

	response, err := f.httpClient.Do(outbound)
	if err != nil {
		shared.WriteJSONError(w, request.ID, http.StatusBadGateway, -32060, "TASK_FORWARD_FAILED: "+err.Error())
		return true
	}
	defer func() { _ = response.Body.Close() }()
	copyForwardResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	return true
}

func (f *distributedTaskForwarder) forwardURL(path string) (string, error) {
	base, err := url.Parse(f.endpoint)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("TASK_FORWARD_ENDPOINT_INVALID: %s", f.endpoint)
	}
	if !distributedForwardEndpointEncryptedOrPrivate(base) {
		return "", fmt.Errorf("TASK_FORWARD_ENDPOINT_INSECURE: use https or a private VPN endpoint")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(path, "/")
	base.RawQuery = ""
	return base.String(), nil
}

func distributedForwardEndpointEncryptedOrPrivate(endpoint *url.URL) bool {
	if endpoint == nil {
		return false
	}
	if strings.EqualFold(endpoint.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(endpoint.Scheme, "http") {
		return false
	}
	host := strings.Trim(endpoint.Hostname(), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func distributedForwardBearerHeader(token string) string {
	token = strings.TrimSpace(token)
	if token == "" || strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return token
	}
	return "Bearer " + token
}

func copyForwardHeader(dst http.Header, src http.Header, key string) {
	if value := strings.TrimSpace(src.Get(key)); value != "" {
		dst.Set(key, value)
	}
}

func copyForwardResponseHeaders(dst http.Header, src http.Header) {
	for _, key := range []string{"Content-Type", "Cache-Control", "Connection"} {
		if value := strings.TrimSpace(src.Get(key)); value != "" {
			dst.Set(key, value)
		}
	}
}
