package acp

import (
	"context"
	"sync"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/memory"
)

type session struct {
	sessionID string
	threadID  string
	mode      string
	provider  string // The Provider ID
	target    string // The Execution Target ID
	adapter   ProviderAdapter
	cancel    context.CancelFunc
	closed    bool
	mu        sync.Mutex
	history   []string
}

type Server struct {
	mu              sync.RWMutex
	config          *BridgeConfig
	sessions        map[string]*session
	
	// Core Control Plane Components
	routingEngine   RoutingEngine
	adapters        map[string]ProviderAdapter
	catalog         *CapabilityCatalog
	orchestrator    *SessionOrchestrator
	memoryService   memory.Service
	
	providerOrder    []string
	sessionToAdapter map[string]ProviderAdapter
	gateway          *gatewayruntime.Manager

	// Legacy / Common
	authService     interface{} // Minimal auth dependency
	allowedOrigins  []string
}
