package acp

import (
	"encoding/json"
	"net/http"
	"strings"

	"xworkmate-bridge/internal/shared"
)

func (s *Server) HandleBridgeBootstrapHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"bridgeOrigin": bridgePublicBaseURL(),
		"issuedBy":     "xworkmate-bridge",
	})
}

func bridgePublicBaseURL() string {
	value := strings.TrimSpace(shared.EnvOrDefault("BRIDGE_SERVER_URL", "https://xworkmate-bridge.svc.plus"))
	if value == "" {
		return "https://xworkmate-bridge.svc.plus"
	}
	return strings.TrimRight(value, "/")
}
