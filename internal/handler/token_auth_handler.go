package handler

import (
	"encoding/json"
	"net/http"

	"xworkmate-bridge/internal/shared"
	"xworkmate-bridge/internal/service"
)

type TokenAuthHandler struct {
	service *service.StaticTokenAuthService
}

func NewTokenAuthHandler(service *service.StaticTokenAuthService) *TokenAuthHandler {
	return &TokenAuthHandler{service: service}
}

func (h *TokenAuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(shared.ErrorEnvelope(nil, -32000, "auth service unavailable"))
		return
	}
	token := r.Header.Get("Authorization")
	if !h.service.ValidateAuthorizationHeader(token) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// Return JSON error instead of plain text to satisfy Flutter's expectation
		_ = json.NewEncoder(w).Encode(shared.ErrorEnvelope(nil, -32001, "unauthorized"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"ok":      true,
		"type":    "res",
		"payload": map[string]any{"authenticated": true},
	})
}
