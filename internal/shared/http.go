package shared

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var StandardWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  16 * 1024,
	WriteBufferSize: 16 * 1024,
	CheckOrigin: func(*http.Request) bool {
		return true
	},
}

func ApplyCORS(w http.ResponseWriter, r *http.Request, allowedOrigins []string) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || !OriginAllowed(origin, allowedOrigins) {
		return
	}
	headers := w.Header()
	headers.Set("Access-Control-Allow-Origin", origin)
	headers.Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET")
	headers.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
	headers.Set("Access-Control-Max-Age", "600")
	headers.Add("Vary", "Origin")
	headers.Add("Vary", "Access-Control-Request-Method")
	headers.Add("Vary", "Access-Control-Request-Headers")
}

func OriginAllowed(origin string, allowedOrigins []string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return true
	}
	if len(allowedOrigins) == 0 {
		return true
	}
	for _, allowed := range allowedOrigins {
		if strings.HasSuffix(allowed, ":*") {
			if strings.HasPrefix(origin, strings.TrimSuffix(allowed, "*")) {
				return true
			}
			continue
		}
		if origin == allowed {
			return true
		}
	}
	return false
}

func WriteJSONError(w http.ResponseWriter, requestID any, statusCode int, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(ErrorEnvelope(requestID, code, message))
}

func ParseAllowedOrigins(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		result = append(result, part)
	}
	return result
}

// NewHTTPClient returns an http.Client with a customized high-performance transport.
// It uses a significantly larger connection pool to prevent socket exhaustion and
// performance degradation when hitting the same backend hosts heavily.
func NewHTTPClient(timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 1000
	t.MaxIdleConnsPerHost = 100
	t.IdleConnTimeout = 90 * time.Second

	return &http.Client{
		Timeout:   timeout,
		Transport: t,
	}
}
