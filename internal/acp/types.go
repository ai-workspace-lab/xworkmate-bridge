package acp

import (
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
	compat    ProviderCompat
	mu        sync.Mutex
	history   []string
}

type Server struct {
	mu       sync.RWMutex
	config   *BridgeConfig
	sessions map[string]*session

	// Core Control Plane Components
	routingEngine RoutingEngine
	providers     map[string]ProviderCompat
	catalog       *CapabilityCatalog
	orchestrator  *SessionOrchestrator
	memoryService memory.Service

	providerOrder []string
	gateway       *gatewayruntime.Manager

	// Legacy / Common
	authService    interface{} // Minimal auth dependency
	allowedOrigins []string
}
