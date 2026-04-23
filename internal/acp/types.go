package acp

import (
	"context"
	"sync"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/service"
	"xworkmate-bridge/internal/shared"
)

type session struct {
	sessionID string
	threadID  string
	mode      string
	provider  string
	history   []string
	seq       int
	cancel    context.CancelFunc
	closed    bool
}

type task struct {
	req    shared.RPCRequest
	notify func(map[string]any)
	done   chan taskResult
}

type taskResult struct {
	response map[string]any
	err      *shared.RPCError
}

type Server struct {
	mu              sync.Mutex
	config          *BridgeConfig
	sessions        map[string]*session
	queues          map[string]chan task
	gateway         *gatewayruntime.Manager
	providerCatalog map[string]syncedProvider
	providerOrder   []string
	authService     *service.StaticTokenAuthService
	allowedOrigins  []string
}
