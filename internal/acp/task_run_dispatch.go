package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	taskRunCallbackSchemaVersion = 1
	taskRunCallbackMaxBytes      = 16 * 1024
)

var (
	ErrTaskRunDispatchInvalid    = errors.New("invalid task run dispatch")
	ErrTaskRunLeaseRejected      = errors.New("task run lease rejected")
	ErrTaskRunRuntimeUnavailable = errors.New("task run runtime unavailable")
	ErrTaskRunCallbackInvalid    = errors.New("invalid task run callback")
	ErrTaskRunCallbackWrite      = errors.New("task run callback write failed")
	ErrTaskRunExecution          = errors.New("task run execution failed")
)

type TaskRunEventType string

const (
	TaskRunEventRunning    TaskRunEventType = "run.running"
	TaskRunEventProgressed TaskRunEventType = "run.progressed"
	TaskRunEventCompleted  TaskRunEventType = "run.completed"
	TaskRunEventFailed     TaskRunEventType = "run.failed"
	TaskRunEventCancelled  TaskRunEventType = "run.cancelled"
)

// TaskRunDispatchRequest is the service-to-service claim handoff accepted by
// the managed Bridge runtime. The lease identity is flattened in JSON so the
// claim envelope has one canonical representation across scheduler and Bridge.
type TaskRunDispatchRequest struct {
	TaskRunEnvelope
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// TaskRunLeaseCondition is the exact conditional-update key a callback writer
// must use. A writer must return ErrTaskRunLeaseRejected unless the current row
// matches taskRunId + expected fence/token/expiry and remains valid at
// LeaseValidAt.
type TaskRunLeaseCondition struct {
	TaskRunID              string    `json:"taskRunId"`
	ExpectedFence          int64     `json:"expectedFence"`
	ExpectedLeaseToken     string    `json:"expectedLeaseToken"`
	ExpectedLeaseExpiresAt time.Time `json:"expectedLeaseExpiresAt"`
	LeaseValidAt           time.Time `json:"leaseValidAt"`
}

type TaskRunCallbackPayload struct {
	SchemaVersion int    `json:"schemaVersion"`
	Message       string `json:"message,omitempty"`
	ErrorCode     string `json:"errorCode,omitempty"`
	ErrorMessage  string `json:"errorMessage,omitempty"`
}

// TaskRunStatusCallback deliberately contains no artifact, attachment, tool
// log, or filesystem fields. BridgeTaskRef is an opaque lookup reference owned
// by the Bridge/OpenClaw artifact domain.
type TaskRunStatusCallback struct {
	TaskRunEnvelope
	EventType     TaskRunEventType       `json:"eventType"`
	State         TaskState              `json:"state"`
	Payload       TaskRunCallbackPayload `json:"payload"`
	BridgeTaskRef string                 `json:"bridgeTaskRef,omitempty"`
	OccurredAt    time.Time              `json:"occurredAt"`
}

type TaskRunDispatchResult struct {
	TaskRunID     string    `json:"taskRunId"`
	NamespaceID   string    `json:"namespaceId"`
	SessionID     string    `json:"sessionId"`
	State         TaskState `json:"state"`
	BridgeTaskRef string    `json:"bridgeTaskRef,omitempty"`
}

// TaskRunACPExecutor is the only managed-task dependency on the ACP runtime.
// Implementations must execute the already-enriched params without recovering
// identity from a client request or local session cache.
type TaskRunACPExecutor interface {
	ExecuteTaskRun(
		ctx context.Context,
		method string,
		params map[string]any,
		notify SessionNotificationSink,
	) (map[string]any, error)
}

// TaskRunLeaseVerifier authorizes account + namespace + session ownership and
// verifies the active fence/token/expiry against the Bridge-hosted repository.
type TaskRunLeaseVerifier interface {
	VerifyTaskRunLease(ctx context.Context, envelope TaskRunEnvelope, now time.Time) error
}

// TaskRunCallbackWriter persists a lightweight run event using condition as
// the SQL update predicate. It must never persist artifact bodies from ACP.
type TaskRunCallbackWriter interface {
	WriteTaskRunCallback(
		ctx context.Context,
		condition TaskRunLeaseCondition,
		callback TaskRunStatusCallback,
	) error
}

type TaskRunDispatcher struct {
	executor  TaskRunACPExecutor
	leases    TaskRunLeaseVerifier
	callbacks TaskRunCallbackWriter
	now       func() time.Time
}

func NewTaskRunDispatcher(
	executor TaskRunACPExecutor,
	leases TaskRunLeaseVerifier,
	callbacks TaskRunCallbackWriter,
) *TaskRunDispatcher {
	return newTaskRunDispatcher(executor, leases, callbacks, time.Now)
}

// ConfigureTaskRunRuntime wires the Bridge-hosted scheduler repository into
// the existing ACP orchestrator. Until both dependencies are configured, the
// internal dispatch endpoint fails closed with runtime unavailable.
func (s *Server) ConfigureTaskRunRuntime(
	leases TaskRunLeaseVerifier,
	callbacks TaskRunCallbackWriter,
) error {
	if s == nil || s.orchestrator == nil || leases == nil || callbacks == nil {
		return ErrTaskRunRuntimeUnavailable
	}
	dispatcher := NewTaskRunDispatcher(
		serverTaskRunACPExecutor{server: s},
		leases,
		callbacks,
	)
	s.mu.Lock()
	s.taskRunDispatcher = dispatcher
	s.mu.Unlock()
	return nil
}

func newTaskRunDispatcher(
	executor TaskRunACPExecutor,
	leases TaskRunLeaseVerifier,
	callbacks TaskRunCallbackWriter,
	now func() time.Time,
) *TaskRunDispatcher {
	return &TaskRunDispatcher{
		executor:  executor,
		leases:    leases,
		callbacks: callbacks,
		now:       now,
	}
}

func (d *TaskRunDispatcher) Dispatch(
	ctx context.Context,
	request TaskRunDispatchRequest,
) (TaskRunDispatchResult, error) {
	if err := d.ready(); err != nil {
		return TaskRunDispatchResult{}, err
	}
	method := strings.TrimSpace(request.Method)
	if method != "session.start" && method != "session.message" {
		return TaskRunDispatchResult{}, fmt.Errorf("%w: unsupported ACP method", ErrTaskRunDispatchInvalid)
	}
	if strings.TrimSpace(taskRunString(request.Params, "taskPrompt")) == "" {
		return TaskRunDispatchResult{}, fmt.Errorf("%w: taskPrompt is required", ErrTaskRunDispatchInvalid)
	}
	if taskRunHasOrchestrationParams(request.Params) {
		return TaskRunDispatchResult{}, fmt.Errorf("%w: multi-agent orchestration is not supported", ErrTaskRunDispatchInvalid)
	}

	now := d.now().UTC()
	params, err := request.TaskRunEnvelope.EnrichParams(request.Params, now)
	if err != nil {
		return TaskRunDispatchResult{}, fmt.Errorf("%w: %w", ErrTaskRunDispatchInvalid, err)
	}
	if err := d.verifyLease(ctx, request.TaskRunEnvelope, now); err != nil {
		return TaskRunDispatchResult{}, err
	}
	running := taskRunStatusCallback(
		request.TaskRunEnvelope,
		TaskRunEventRunning,
		TaskStateRunning,
		TaskRunCallbackPayload{SchemaVersion: taskRunCallbackSchemaVersion},
		"",
		now,
	)
	if err := d.writeCallback(ctx, running); err != nil {
		return TaskRunDispatchResult{}, err
	}

	snapshot, executionErr := d.executor.ExecuteTaskRun(ctx, method, params, nil)
	if executionErr != nil {
		callbackErr := d.reportExecutionFailure(ctx, request.TaskRunEnvelope, executionErr)
		wrappedExecutionErr := fmt.Errorf("%w: %v", ErrTaskRunExecution, executionErr)
		if callbackErr != nil {
			return TaskRunDispatchResult{}, errors.Join(wrappedExecutionErr, callbackErr)
		}
		return TaskRunDispatchResult{}, wrappedExecutionErr
	}
	return d.ReportTaskSnapshot(ctx, request.TaskRunEnvelope, snapshot)
}

// ReportTaskSnapshot is also the async completion boundary: a Bridge task
// poller can feed an existing ACP/OpenClaw snapshot here without re-executing
// the task. The active lease is re-verified before every status write.
func (d *TaskRunDispatcher) ReportTaskSnapshot(
	ctx context.Context,
	envelope TaskRunEnvelope,
	snapshot map[string]any,
) (TaskRunDispatchResult, error) {
	if err := d.ready(); err != nil {
		return TaskRunDispatchResult{}, err
	}
	now := d.now().UTC()
	if err := d.verifyLease(ctx, envelope, now); err != nil {
		return TaskRunDispatchResult{}, err
	}
	callback, err := taskRunCallbackFromSnapshot(envelope, snapshot, now)
	if err != nil {
		return TaskRunDispatchResult{}, err
	}
	if err := d.writeCallback(ctx, callback); err != nil {
		return TaskRunDispatchResult{}, err
	}
	return TaskRunDispatchResult{
		TaskRunID:     envelope.TaskRunID,
		NamespaceID:   envelope.NamespaceID,
		SessionID:     envelope.SessionID,
		State:         callback.State,
		BridgeTaskRef: callback.BridgeTaskRef,
	}, nil
}

func (d *TaskRunDispatcher) reportExecutionFailure(
	ctx context.Context,
	envelope TaskRunEnvelope,
	executionErr error,
) error {
	_, err := d.ReportTaskSnapshot(ctx, envelope, map[string]any{
		"status": "failed",
		"code":   "ACP_EXECUTION_FAILED",
		"error":  executionErr.Error(),
	})
	return err
}

func (d *TaskRunDispatcher) ready() error {
	if d == nil || d.executor == nil || d.leases == nil || d.callbacks == nil || d.now == nil {
		return ErrTaskRunRuntimeUnavailable
	}
	return nil
}

func (d *TaskRunDispatcher) verifyLease(
	ctx context.Context,
	envelope TaskRunEnvelope,
	now time.Time,
) error {
	if err := envelope.Validate(now); err != nil {
		return err
	}
	if err := d.leases.VerifyTaskRunLease(ctx, envelope, now); err != nil {
		return fmt.Errorf("verify task run lease: %w", err)
	}
	return nil
}

func (d *TaskRunDispatcher) writeCallback(ctx context.Context, callback TaskRunStatusCallback) error {
	if err := callback.Validate(d.now().UTC()); err != nil {
		return err
	}
	condition := TaskRunLeaseCondition{
		TaskRunID:              callback.TaskRunID,
		ExpectedFence:          callback.Fence,
		ExpectedLeaseToken:     callback.LeaseToken,
		ExpectedLeaseExpiresAt: callback.LeaseExpiresAt,
		LeaseValidAt:           callback.OccurredAt,
	}
	if err := d.callbacks.WriteTaskRunCallback(ctx, condition, callback); err != nil {
		if errors.Is(err, ErrTaskRunLeaseRejected) || errors.Is(err, ErrTaskRunLeaseExpired) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrTaskRunCallbackWrite, err)
	}
	return nil
}

func (c TaskRunStatusCallback) Validate(now time.Time) error {
	if err := c.TaskRunEnvelope.Validate(now); err != nil {
		return err
	}
	if c.Payload.SchemaVersion != taskRunCallbackSchemaVersion || c.OccurredAt.IsZero() {
		return fmt.Errorf("%w: schemaVersion and occurredAt are required", ErrTaskRunCallbackInvalid)
	}
	expectedState, ok := taskRunEventState(c.EventType)
	if !ok || c.State != expectedState {
		return fmt.Errorf("%w: eventType and state do not match", ErrTaskRunCallbackInvalid)
	}
	payload, err := json.Marshal(c.Payload)
	if err != nil {
		return fmt.Errorf("%w: payload cannot be encoded", ErrTaskRunCallbackInvalid)
	}
	if len(payload) > taskRunCallbackMaxBytes {
		return fmt.Errorf("%w: payload exceeds %d bytes", ErrTaskRunCallbackInvalid, taskRunCallbackMaxBytes)
	}
	return nil
}

func taskRunCallbackFromSnapshot(
	envelope TaskRunEnvelope,
	snapshot map[string]any,
	now time.Time,
) (TaskRunStatusCallback, error) {
	status := strings.ToLower(strings.TrimSpace(taskRunString(snapshot, "status")))
	var eventType TaskRunEventType
	var state TaskState
	payload := TaskRunCallbackPayload{SchemaVersion: taskRunCallbackSchemaVersion}
	switch status {
	case string(TaskStateQueued), string(TaskStateRunning):
		eventType = TaskRunEventProgressed
		state = TaskStateRunning
		payload.Message = strings.TrimSpace(taskRunString(snapshot, "message"))
	case string(TaskStateCompleted):
		eventType = TaskRunEventCompleted
		state = TaskStateCompleted
		payload.Message = strings.TrimSpace(taskRunString(snapshot, "output"))
	case string(TaskStateFailed):
		eventType = TaskRunEventFailed
		state = TaskStateFailed
		payload.ErrorCode = strings.TrimSpace(taskRunString(snapshot, "code"))
		payload.ErrorMessage = strings.TrimSpace(taskRunString(snapshot, "error"))
	case string(TaskStateCancelled):
		eventType = TaskRunEventCancelled
		state = TaskStateCancelled
		payload.Message = strings.TrimSpace(taskRunString(snapshot, "message"))
	default:
		return TaskRunStatusCallback{}, fmt.Errorf("%w: unsupported snapshot status %q", ErrTaskRunCallbackInvalid, status)
	}
	return taskRunStatusCallback(
		envelope,
		eventType,
		state,
		payload,
		strings.TrimSpace(taskRunString(snapshot, "runId")),
		now,
	), nil
}

func taskRunStatusCallback(
	envelope TaskRunEnvelope,
	eventType TaskRunEventType,
	state TaskState,
	payload TaskRunCallbackPayload,
	bridgeTaskRef string,
	occurredAt time.Time,
) TaskRunStatusCallback {
	return TaskRunStatusCallback{
		TaskRunEnvelope: envelope,
		EventType:       eventType,
		State:           state,
		Payload:         payload,
		BridgeTaskRef:   bridgeTaskRef,
		OccurredAt:      occurredAt,
	}
}

func taskRunEventState(eventType TaskRunEventType) (TaskState, bool) {
	switch eventType {
	case TaskRunEventRunning, TaskRunEventProgressed:
		return TaskStateRunning, true
	case TaskRunEventCompleted:
		return TaskStateCompleted, true
	case TaskRunEventFailed:
		return TaskStateFailed, true
	case TaskRunEventCancelled:
		return TaskStateCancelled, true
	default:
		return "", false
	}
}

func taskRunHasOrchestrationParams(params map[string]any) bool {
	if _, exists := params["multiAgent"]; exists {
		return true
	}
	if _, exists := params["orchestrationMode"]; exists {
		return true
	}
	mode := strings.ToLower(strings.TrimSpace(taskRunString(params, "mode")))
	return mode == "multi-agent" || mode == "multi_agent" || mode == "multiagent"
}

func taskRunString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return value
}

type serverTaskRunACPExecutor struct {
	server *Server
}

func (e serverTaskRunACPExecutor) ExecuteTaskRun(
	ctx context.Context,
	method string,
	params map[string]any,
	notify SessionNotificationSink,
) (map[string]any, error) {
	if e.server == nil || e.server.orchestrator == nil {
		return nil, ErrTaskRunRuntimeUnavailable
	}
	result, rpcErr := e.server.orchestrator.Process(ctx, method, params, notify)
	if rpcErr != nil {
		return nil, fmt.Errorf("ACP error %d: %s", rpcErr.Code, rpcErr.Message)
	}
	return result, nil
}
