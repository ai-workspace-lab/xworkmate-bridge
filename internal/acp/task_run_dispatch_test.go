package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTaskRunDispatcherRejectsExpiredOrUnverifiedLeaseBeforeExecution(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	cases := []struct {
		name      string
		envelope  TaskRunEnvelope
		verifyErr error
		wantErr   error
	}{
		{
			name:     "expired",
			envelope: taskRunEnvelopeWithExpiry(validTaskRunEnvelope(now), now),
			wantErr:  ErrTaskRunLeaseExpired,
		},
		{
			name:      "old fence",
			envelope:  validTaskRunEnvelope(now),
			verifyErr: ErrTaskRunLeaseRejected,
			wantErr:   ErrTaskRunLeaseRejected,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := &fakeTaskRunExecutor{result: map[string]any{"status": "completed", "output": "done"}}
			verifier := &fakeTaskRunLeaseVerifier{err: tc.verifyErr}
			writer := &fakeTaskRunCallbackWriter{}
			dispatcher := newTaskRunDispatcher(executor, verifier, writer, func() time.Time { return now })

			_, err := dispatcher.Dispatch(context.Background(), TaskRunDispatchRequest{
				TaskRunEnvelope: tc.envelope,
				Method:          "session.message",
				Params:          map[string]any{"taskPrompt": "hello"},
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
			if executor.calls != 0 {
				t.Fatalf("executor called for rejected lease: %d", executor.calls)
			}
			if len(writer.callbacks) != 0 {
				t.Fatalf("callback written for rejected lease: %#v", writer.callbacks)
			}
		})
	}
}

func TestTaskRunDispatchHTTPBoundaryRequiresServiceIdentity(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	executor := &fakeTaskRunExecutor{result: map[string]any{"status": "completed", "output": "done"}}
	dispatcher := newTaskRunDispatcher(
		executor,
		&fakeTaskRunLeaseVerifier{},
		&fakeTaskRunCallbackWriter{},
		func() time.Time { return now },
	)
	server := &Server{
		taskRunDispatcher: dispatcher,
		taskRunAuthService: fixedTaskRunAuthorization{
			header: "Bearer internal-service-token",
		},
	}
	body, err := json.Marshal(TaskRunDispatchRequest{
		TaskRunEnvelope: validTaskRunEnvelope(now),
		Method:          "session.message",
		Params:          map[string]any{"taskPrompt": "hello"},
	})
	if err != nil {
		t.Fatalf("marshal dispatch request: %v", err)
	}

	unauthorized := httptest.NewRecorder()
	unauthorizedRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/internal/task-runs/dispatch",
		bytes.NewReader(body),
	)
	unauthorizedRequest.Header.Set("Authorization", "Bearer public-user-token")
	server.Handler().ServeHTTP(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", unauthorized.Code, unauthorized.Body.String())
	}
	if executor.calls != 0 {
		t.Fatalf("unauthorized request reached executor: %d", executor.calls)
	}

	authorized := httptest.NewRecorder()
	authorizedRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/internal/task-runs/dispatch",
		bytes.NewReader(body),
	)
	authorizedRequest.Header.Set("Authorization", "Bearer internal-service-token")
	server.Handler().ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", authorized.Code, authorized.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("expected one authorized execution, got %d", executor.calls)
	}
}

func TestTaskRunDispatchHTTPBoundaryFailsClosedWithoutRuntime(t *testing.T) {
	server := &Server{
		taskRunAuthService: fixedTaskRunAuthorization{
			header: "Bearer internal-service-token",
		},
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/internal/task-runs/dispatch",
		bytes.NewBufferString(`{}`),
	)
	request.Header.Set("Authorization", "Bearer internal-service-token")
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestTaskRunDispatchHTTPBoundaryDoesNotAcceptPublicBridgeToken(t *testing.T) {
	t.Setenv("AI_WORKSPACE_AUTH_TOKEN", "public-user-token")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_REVIEW_AUTH_TOKEN", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-service-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()

	publicRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/internal/task-runs/dispatch",
		bytes.NewBufferString(`{}`),
	)
	publicRequest.Header.Set("Authorization", "Bearer public-user-token")
	publicRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(publicRecorder, publicRequest)
	if publicRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected public token rejection, got %d: %s", publicRecorder.Code, publicRecorder.Body.String())
	}

	internalRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/internal/task-runs/dispatch",
		bytes.NewBufferString(`{}`),
	)
	internalRequest.Header.Set("Authorization", "Bearer internal-service-token")
	internalRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(internalRecorder, internalRequest)
	if internalRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected dedicated token to pass auth and reach unavailable runtime, got %d: %s", internalRecorder.Code, internalRecorder.Body.String())
	}
}

func TestTaskRunDispatchHTTPBoundaryFailsClosedWhenInternalTokenMatchesPublicToken(t *testing.T) {
	t.Setenv("AI_WORKSPACE_AUTH_TOKEN", "shared-token")
	t.Setenv("BRIDGE_AUTH_TOKEN", "")
	t.Setenv("BRIDGE_REVIEW_AUTH_TOKEN", "")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "shared-token")
	t.Setenv("BRIDGE_CONFIG_PATH", "../../example/config.yaml")
	server := NewServer()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1/api/internal/task-runs/dispatch",
		bytes.NewBufferString(`{}`),
	)
	request.Header.Set("Authorization", "Bearer shared-token")
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected shared public/internal token to fail closed, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestTaskRunDispatcherUsesAuthoritativeIdentityAtACPBoundary(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	executor := &fakeTaskRunExecutor{result: map[string]any{
		"status": "completed",
		"output": "done",
		"runId":  "bridge-run-1",
		"artifacts": []any{
			map[string]any{"path": "/private/result.png", "content": "must-not-be-callback-data"},
		},
	}}
	verifier := &fakeTaskRunLeaseVerifier{}
	writer := &fakeTaskRunCallbackWriter{}
	dispatcher := newTaskRunDispatcher(executor, verifier, writer, func() time.Time { return now })
	envelope := validTaskRunEnvelope(now)

	result, err := dispatcher.Dispatch(context.Background(), TaskRunDispatchRequest{
		TaskRunEnvelope: envelope,
		Method:          "session.message",
		Params: map[string]any{
			"taskPrompt": "hello",
			"metadata":   map[string]any{"requestSource": "scheduler"},
		},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if verifier.calls != 2 {
		t.Fatalf("expected verification before execution and terminal callback, got %d", verifier.calls)
	}
	verified := verifier.envelopes[0]
	if verified.AccountID != envelope.AccountID || verified.NamespaceID != envelope.NamespaceID ||
		verified.SessionID != envelope.SessionID || verified.Fence != envelope.Fence ||
		verified.LeaseToken != envelope.LeaseToken {
		t.Fatalf("lease verifier did not receive authoritative ownership and lease fields: %#v", verified)
	}
	if executor.calls != 1 {
		t.Fatalf("expected one ACP call, got %d", executor.calls)
	}
	if executor.params["sessionId"] != envelope.SessionID ||
		executor.params["threadId"] != envelope.SessionID ||
		executor.params["namespaceId"] != envelope.NamespaceID ||
		executor.params["accountId"] != envelope.AccountID {
		t.Fatalf("ACP params are not authoritative: %#v", executor.params)
	}
	metadata, ok := executor.params["metadata"].(map[string]any)
	if !ok || metadata["xworkmateTaskRunId"] != envelope.TaskRunID || metadata["xworkmateFence"] != envelope.Fence {
		t.Fatalf("managed metadata missing: %#v", executor.params["metadata"])
	}
	if _, ok := metadata["xworkmateLeaseToken"]; ok {
		t.Fatalf("lease secret crossed ACP boundary: %#v", metadata)
	}
	if _, ok := executor.params["leaseToken"]; ok {
		t.Fatalf("lease secret crossed ACP params boundary: %#v", executor.params)
	}
	if result.State != TaskStateCompleted || result.BridgeTaskRef != "bridge-run-1" {
		t.Fatalf("unexpected dispatch result: %#v", result)
	}
	if len(writer.callbacks) != 2 {
		t.Fatalf("expected running and terminal callbacks, got %#v", writer.callbacks)
	}
	terminal := writer.callbacks[1]
	if terminal.EventType != TaskRunEventCompleted || terminal.TaskRunID != envelope.TaskRunID ||
		terminal.Fence != envelope.Fence || terminal.LeaseToken != envelope.LeaseToken {
		t.Fatalf("conditional terminal callback contract missing: %#v", terminal)
	}
	if terminal.Payload.Message != "done" || terminal.BridgeTaskRef != "bridge-run-1" {
		t.Fatalf("terminal callback missing lightweight result: %#v", terminal)
	}
	encodedTerminal, err := json.Marshal(terminal)
	if err != nil {
		t.Fatalf("marshal terminal callback: %v", err)
	}
	if bytes.Contains(encodedTerminal, []byte("artifacts")) ||
		bytes.Contains(encodedTerminal, []byte("must-not-be-callback-data")) {
		t.Fatalf("artifact data leaked into callback: %s", encodedTerminal)
	}
	condition := writer.conditions[1]
	if condition.TaskRunID != envelope.TaskRunID || condition.ExpectedFence != envelope.Fence ||
		condition.ExpectedLeaseToken != envelope.LeaseToken ||
		!condition.ExpectedLeaseExpiresAt.Equal(envelope.LeaseExpiresAt) || !condition.LeaseValidAt.Equal(now) {
		t.Fatalf("conditional writer key missing: %#v", condition)
	}
}

func TestTaskRunDispatcherRejectsClientOverrideAndMultiAgentParams(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	cases := []struct {
		name   string
		params map[string]any
	}{
		{name: "namespace override", params: map[string]any{"taskPrompt": "hello", "namespaceId": "forged"}},
		{name: "session override", params: map[string]any{"taskPrompt": "hello", "sessionId": "forged"}},
		{name: "multi agent", params: map[string]any{"taskPrompt": "hello", "multiAgent": true}},
		{name: "orchestration mode", params: map[string]any{"taskPrompt": "hello", "orchestrationMode": "parallel"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := &fakeTaskRunExecutor{}
			dispatcher := newTaskRunDispatcher(
				executor,
				&fakeTaskRunLeaseVerifier{},
				&fakeTaskRunCallbackWriter{},
				func() time.Time { return now },
			)
			_, err := dispatcher.Dispatch(context.Background(), TaskRunDispatchRequest{
				TaskRunEnvelope: validTaskRunEnvelope(now),
				Method:          "session.message",
				Params:          tc.params,
			})
			if !errors.Is(err, ErrTaskRunDispatchInvalid) {
				t.Fatalf("expected invalid dispatch, got %v", err)
			}
			if executor.calls != 0 {
				t.Fatalf("executor called for invalid dispatch: %d", executor.calls)
			}
		})
	}
}

func TestTaskRunDispatcherRejectsTerminalCallbackAfterLeaseExpiry(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	clock := now
	writer := &fakeTaskRunCallbackWriter{}
	dispatcher := newTaskRunDispatcher(
		&fakeTaskRunExecutor{},
		&fakeTaskRunLeaseVerifier{},
		writer,
		func() time.Time { return clock },
	)
	envelope := taskRunEnvelopeWithExpiry(validTaskRunEnvelope(now), now.Add(time.Minute))
	clock = now.Add(time.Minute)

	_, err := dispatcher.ReportTaskSnapshot(context.Background(), envelope, map[string]any{
		"status": "completed",
		"output": "late result",
	})
	if !errors.Is(err, ErrTaskRunLeaseExpired) {
		t.Fatalf("expected expired callback rejection, got %v", err)
	}
	if len(writer.callbacks) != 0 {
		t.Fatalf("expired callback reached writer: %#v", writer.callbacks)
	}
}

func TestTaskRunDispatcherPropagatesConditionalWriterFenceRejection(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	writer := &fakeTaskRunCallbackWriter{err: ErrTaskRunLeaseRejected}
	dispatcher := newTaskRunDispatcher(
		&fakeTaskRunExecutor{},
		&fakeTaskRunLeaseVerifier{},
		writer,
		func() time.Time { return now },
	)

	_, err := dispatcher.ReportTaskSnapshot(
		context.Background(),
		validTaskRunEnvelope(now),
		map[string]any{"status": "completed", "output": "stale result"},
	)
	if !errors.Is(err, ErrTaskRunLeaseRejected) {
		t.Fatalf("expected conditional writer rejection, got %v", err)
	}
	if len(writer.callbacks) != 0 {
		t.Fatalf("rejected callback was recorded: %#v", writer.callbacks)
	}
}

func TestTaskRunServerExecutorCallsExistingACPOrchestrator(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	provider := &recordingTaskRunProvider{}
	server := &Server{
		sessions:      map[string]*session{},
		providers:     map[string]ProviderCompat{"test-provider": provider},
		routingEngine: fixedTaskRunRoutingEngine{},
	}
	server.orchestrator = NewSessionOrchestrator(server)
	dispatcher := newTaskRunDispatcher(
		serverTaskRunACPExecutor{server: server},
		&fakeTaskRunLeaseVerifier{},
		&fakeTaskRunCallbackWriter{},
		func() time.Time { return now },
	)
	envelope := validTaskRunEnvelope(now)

	_, err := dispatcher.Dispatch(context.Background(), TaskRunDispatchRequest{
		TaskRunEnvelope: envelope,
		Method:          "session.start",
		Params:          map[string]any{"taskPrompt": "hello"},
	})
	if err != nil {
		t.Fatalf("dispatch through server executor: %v", err)
	}
	if provider.calls != 1 || provider.sessionID != envelope.SessionID || provider.threadID != envelope.SessionID {
		t.Fatalf("existing ACP orchestrator was not called with managed identity: %#v", provider)
	}
}

func validTaskRunEnvelope(now time.Time) TaskRunEnvelope {
	return TaskRunEnvelope{
		TaskRunID:      "run-1",
		AccountID:      "account-1",
		NamespaceID:    "namespace-1",
		SessionID:      "session-1",
		ThreadID:       "session-1",
		Fence:          7,
		LeaseToken:     "lease-secret",
		LeaseExpiresAt: now.Add(5 * time.Minute),
	}
}

func taskRunEnvelopeWithExpiry(envelope TaskRunEnvelope, expiry time.Time) TaskRunEnvelope {
	envelope.LeaseExpiresAt = expiry
	return envelope
}

type fakeTaskRunExecutor struct {
	result map[string]any
	err    error
	calls  int
	method string
	params map[string]any
}

func (f *fakeTaskRunExecutor) ExecuteTaskRun(
	_ context.Context,
	method string,
	params map[string]any,
	_ SessionNotificationSink,
) (map[string]any, error) {
	f.calls++
	f.method = method
	f.params = params
	return f.result, f.err
}

type fakeTaskRunLeaseVerifier struct {
	err       error
	calls     int
	envelopes []TaskRunEnvelope
}

func (f *fakeTaskRunLeaseVerifier) VerifyTaskRunLease(
	_ context.Context,
	envelope TaskRunEnvelope,
	_ time.Time,
) error {
	f.calls++
	f.envelopes = append(f.envelopes, envelope)
	return f.err
}

type fakeTaskRunCallbackWriter struct {
	err        error
	conditions []TaskRunLeaseCondition
	callbacks  []TaskRunStatusCallback
}

type fixedTaskRunAuthorization struct {
	header string
}

func (a fixedTaskRunAuthorization) ValidateAuthorizationHeader(header string) bool {
	return header == a.header
}

func (f *fakeTaskRunCallbackWriter) WriteTaskRunCallback(
	_ context.Context,
	condition TaskRunLeaseCondition,
	callback TaskRunStatusCallback,
) error {
	if f.err != nil {
		return f.err
	}
	f.conditions = append(f.conditions, condition)
	f.callbacks = append(f.callbacks, callback)
	return nil
}

type fixedTaskRunRoutingEngine struct{}

func (fixedTaskRunRoutingEngine) Resolve(context.Context, map[string]any) (RoutingResult, error) {
	return RoutingResult{
		TargetID:   "agent",
		ProviderID: "test-provider",
		Status:     "available",
	}, nil
}

type recordingTaskRunProvider struct {
	calls     int
	sessionID string
	threadID  string
}

func (p *recordingTaskRunProvider) ID() string { return "test-provider" }

func (p *recordingTaskRunProvider) Metadata() map[string]any { return nil }

func (p *recordingTaskRunProvider) Probe(context.Context) ProviderProbeResult {
	return ProviderProbeResult{Available: true, Status: "available"}
}

func (p *recordingTaskRunProvider) StartSession(
	_ context.Context,
	sessionID string,
	threadID string,
	_ map[string]any,
	_ SessionNotificationSink,
) (map[string]any, error) {
	p.calls++
	p.sessionID = sessionID
	p.threadID = threadID
	return map[string]any{"status": "completed", "success": true, "output": "done"}, nil
}

func (p *recordingTaskRunProvider) SendMessage(
	context.Context,
	string,
	string,
	map[string]any,
	SessionNotificationSink,
) (map[string]any, error) {
	return nil, errors.New("unexpected session.message")
}

func (p *recordingTaskRunProvider) CancelSession(context.Context, string) error { return nil }

func (p *recordingTaskRunProvider) CloseSession(context.Context, string) error { return nil }
