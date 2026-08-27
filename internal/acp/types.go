package acp

import (
	"sync"
	"time"

	"xworkmate-bridge/internal/gatewayruntime"
	"xworkmate-bridge/internal/memory"
	"xworkmate-bridge/internal/sessionstore"
)

type TaskState string

const (
	TaskStateQueued    TaskState = "queued"
	TaskStateRunning   TaskState = "running"
	TaskStateCompleted TaskState = "completed"
	TaskStateFailed    TaskState = "failed"
	TaskStateCancelled TaskState = "cancelled"
)

type TaskKind string

const (
	TaskKindSingleAgent TaskKind = "single-agent"
	TaskKindGateway     TaskKind = "gateway"
)

type authorizationHeaderValidator interface {
	ValidateAuthorizationHeader(string) bool
}

type ControlPlaneSession struct {
	ControlPlaneSessionID string
	ThreadID              string
	ProviderSessionID     string
	RequestedWorkingDir   string
	RemoteWorkingDirHint  string
	UpdatedAt             time.Time
}

type QueuedTask struct {
	SessionID            string
	ThreadID             string
	TurnID               string
	RunID                string
	SessionKey           string
	Provider             string
	Target               string
	GatewayProviderID    string
	State                TaskState
	Kind                 TaskKind
	TaskLoadClass        string
	ArtifactScope        string
	ArtifactDirectory    string
	RuntimeBudgetMinutes int
	StartedAt            time.Time
	UpdatedAt            time.Time
	DeadlineAt           time.Time
	ProgressStage        string
	ProgressMessage      string
	ProgressTerminal     bool
}

type ArtifactRecord struct {
	SessionID              string
	ThreadID               string
	ResultSummary          string
	Artifacts              []map[string]any
	RemoteWorkingDirectory string
	RemoteWorkspaceRefKind string
	UpdatedAt              time.Time
}

type session struct {
	sessionID  string
	threadID   string
	mode       string
	provider   string // The Provider ID
	target     string // The Execution Target ID
	compat     ProviderCompat
	mu         sync.Mutex
	history    []string
	control    ControlPlaneSession
	task       QueuedTask
	artifacts  ArtifactRecord
	lastResult map[string]any
	openClaw   *OpenClawTaskRecord
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

	providerOrder      []string
	gateway            *gatewayruntime.Manager
	openClawGate       *openClawGatewayAdmissionGate
	jobs               *jobManager
	taskRouter         *distributedTaskRouter
	taskRunDispatcher  *TaskRunDispatcher
	taskRunAuthService authorizationHeaderValidator

	// Legacy / Common
	authService    interface{} // Minimal auth dependency
	allowedOrigins []string
	sessionStore   sessionstore.Store
}
