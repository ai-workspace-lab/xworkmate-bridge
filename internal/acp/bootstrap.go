package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/memory"
	"xworkmate-bridge/internal/shared"
)

// ProxyAdapter implements ProviderAdapter by forwarding to existing RPC endpoints
type ProxyAdapter struct {
	providerID string
	endpoint   string
	authHeader string
}

func (a *ProxyAdapter) ID() string { return a.providerID }
func (a *ProxyAdapter) Metadata() map[string]any {
	return map[string]any{
		"providerId": a.providerID,
		"type":       "rpc-proxy",
	}
}

func (a *ProxyAdapter) Execute(ctx context.Context, sessionID string, threadID string, method string, params map[string]any) (<-chan SessionEvent, error) {
	// Simple forwarding logic for RPC-based upstreams
	return a.forward(ctx, method, params)
}

func (a *ProxyAdapter) Cancel(ctx context.Context, sessionID string) error {
	_, err := a.rpcCall(ctx, "session.cancel", map[string]any{"sessionId": sessionID})
	return err
}

func (a *ProxyAdapter) Probe(ctx context.Context) (bool, string) {
	_, err := a.rpcCall(ctx, "health", nil)
	if err != nil {
		return false, err.Error()
	}
	return true, "ok"
}

func (a *ProxyAdapter) forward(ctx context.Context, method string, params map[string]any) (<-chan SessionEvent, error) {
	ch := make(chan SessionEvent, 10)
	
	go func() {
		defer close(ch)
		
		resp, err := a.rpcCall(ctx, method, params)
		if err != nil {
			ch <- SessionEvent{Type: "error", Error: &shared.RPCError{Code: -32002, Message: err.Error()}}
			return
		}
		
		ch <- SessionEvent{Type: "result", Payload: shared.AsMap(resp["result"])}
	}()
	
	return ch, nil
}

func (a *ProxyAdapter) rpcCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	requestBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      fmt.Sprintf("req-%d", time.Now().UnixNano()),
		"method":  method,
		"params":  params,
	})
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		a.endpoint,
		strings.NewReader(string(requestBody)),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	if a.authHeader != "" {
		req.Header.Set("Authorization", a.authHeader)
	}
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return nil, fmt.Errorf("RPC request failed (%d): %s", response.StatusCode, string(body))
	}

	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("failed to decode RPC response: %w", err)
	}
	return decoded, nil
}

// Bootstrap initializes the control plane components
func (s *Server) Bootstrap() {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, providerCatalog, providerOrder := newProductionProviderCatalog()
	s.config = config
	s.providerOrder = providerOrder

	if s.gateway == nil {
		s.gateway = gatewayruntime.NewManager()
	}

	homeDir, _ := os.UserHomeDir()
	s.memoryService = memory.NewService(homeDir)

	s.routingEngine = &DefaultRoutingEngine{server: s}
	s.orchestrator = NewSessionOrchestrator(s)
	s.adapters = make(map[string]ProviderAdapter)
	s.sessionToAdapter = make(map[string]ProviderAdapter)
	s.sessions = make(map[string]*session)

	for id, p := range providerCatalog {
		if p.Enabled {
			s.adapters[id] = &ProxyAdapter{
				providerID: id,
				endpoint:   resolveSingleAgentForwardEndpoint(p),
				authHeader: p.AuthorizationHeader,
			}
		}
	}

	// Build Initial Catalog
	s.catalog = &CapabilityCatalog{
		ProviderCatalog: make([]any, 0),
		GatewayProviders: []any{
			map[string]any{
				"providerId": "openclaw",
				"label":      "OpenClaw",
				"targets":    []string{"gateway"},
				"providerDisplay": map[string]any{"logoEmoji": "🦞"},
			},
		},
		AvailableExecutionTargets: []any{"agent", "gateway"},
	}
	
	for _, id := range providerOrder {
		p, ok := providerCatalog[id]
		if !ok || !p.Enabled {
			continue
		}
		category := "native"
		if id == "gemini" || id == "hermes" {
			category = "protocol-adapter"
		}
		s.catalog.ProviderCatalog = append(s.catalog.ProviderCatalog, map[string]any{
			"providerId": id,
			"label":      p.Label,
			"targets":    []string{"agent"},
			"category":   category,
		})
	}
}

func (s *Server) getAvailableProviderIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for _, id := range s.providerOrder {
		if _, ok := s.adapters[id]; ok {
			ids = append(ids, id)
		}
	}
	// Fallback to random order if order is missing but adapters exist
	if len(ids) == 0 {
		for id := range s.adapters {
			ids = append(ids, id)
		}
	}
	return ids
}
