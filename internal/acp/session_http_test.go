package acp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xworkmate-bridge/internal/service"
	"xworkmate-bridge/internal/sessionstore"
)

type fixedPrincipalResolver struct{ accountID string }

func (r fixedPrincipalResolver) ResolveAuthorizationHeader(context.Context, string) (service.AuthorizationPrincipal, error) {
	return service.AuthorizationPrincipal{AccountID: r.accountID}, nil
}

type recordingSessionStore struct {
	accountID string
	afterSeq  int64
	limit     int
	append    sessionstore.AppendMessageInput
	result    sessionstore.AppendMessageResult
	getErr    error
}

func (*recordingSessionStore) Close() error { return nil }
func (s *recordingSessionStore) ListNamespaces(_ context.Context, accountID string) ([]sessionstore.Namespace, error) {
	s.accountID = accountID
	return []sessionstore.Namespace{}, nil
}
func (*recordingSessionStore) CreateSession(context.Context, string, string, sessionstore.CreateSessionInput) (sessionstore.Snapshot, error) {
	return sessionstore.Snapshot{}, nil
}
func (*recordingSessionStore) ListSessions(context.Context, string, string) ([]sessionstore.Snapshot, error) {
	return []sessionstore.Snapshot{}, nil
}
func (s *recordingSessionStore) GetSession(_ context.Context, accountID, _ string) (sessionstore.Snapshot, error) {
	s.accountID = accountID
	return sessionstore.Snapshot{}, s.getErr
}

func TestTaskSessionOwnershipMismatchIsNotFound(t *testing.T) {
	store := &recordingSessionStore{getErr: sessionstore.ErrNotFound}
	server := newTaskSessionTestServer(store, "account-from-introspection")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/foreign-session", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusNotFound || store.accountID != "account-from-introspection" {
		t.Fatalf("status/account = %d/%q, body = %s", response.Code, store.accountID, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"task_session_not_found"`) {
		t.Fatalf("unexpected error envelope: %s", response.Body.String())
	}
}
func (s *recordingSessionStore) ListEvents(_ context.Context, accountID, _ string, afterSeq int64, limit int) ([]sessionstore.Event, int64, error) {
	s.accountID, s.afterSeq, s.limit = accountID, afterSeq, limit
	return []sessionstore.Event{{Seq: 8, Type: "message.created", Payload: map[string]any{"schemaVersion": 1}, CreatedAt: time.Unix(1, 0).UTC()}}, 8, nil
}
func (s *recordingSessionStore) AppendMessage(_ context.Context, accountID, _ string, input sessionstore.AppendMessageInput) (sessionstore.AppendMessageResult, error) {
	s.accountID, s.append = accountID, input
	return s.result, nil
}
func (*recordingSessionStore) RecordACPUpdate(context.Context, string, sessionstore.ACPUpdate) error {
	return nil
}

func newTaskSessionTestServer(store sessionstore.Store, accountID string) *Server {
	return &Server{sessionStore: store, authService: fixedPrincipalResolver{accountID: accountID}}
}

func TestTaskSessionEventsUsePrincipalScopeAndOrderedCursor(t *testing.T) {
	store := &recordingSessionStore{}
	server := newTaskSessionTestServer(store, "account-authoritative")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/session-1/events?after_seq=7&limit=25&accountId=forged", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if store.accountID != "account-authoritative" || store.afterSeq != 7 || store.limit != 25 {
		t.Fatalf("store scope/cursor = %q/%d/%d", store.accountID, store.afterSeq, store.limit)
	}
	if !strings.Contains(response.Body.String(), `"lastEventSeq":8`) {
		t.Fatalf("missing replay watermark: %s", response.Body.String())
	}
}

func TestTaskSessionMessageContractRejectsArtifactFields(t *testing.T) {
	store := &recordingSessionStore{}
	server := newTaskSessionTestServer(store, "account-1")
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/session-1/messages", strings.NewReader(`{"clientRequestId":"request-1","text":"hello","artifactUrl":"https://example.invalid/file"}`))
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if store.append.ClientRequestID != "" {
		t.Fatal("artifact-bearing request reached persistence")
	}
	if !strings.Contains(response.Body.String(), `"code":"invalid_json"`) {
		t.Fatalf("unexpected error envelope: %s", response.Body.String())
	}
}

func TestTaskSessionMessageReturnsFrozenResultShape(t *testing.T) {
	now := time.Unix(10, 0).UTC()
	store := &recordingSessionStore{result: sessionstore.AppendMessageResult{
		SessionID: "session-1", NamespaceID: "namespace-1", SnapshotVersion: 2,
		Event:   sessionstore.Event{Seq: 2, Type: "message.created", Payload: map[string]any{"schemaVersion": 1, "text": "hello"}, CreatedAt: now},
		TaskRun: sessionstore.TaskRun{ID: "run-1", State: "queued", CreatedAt: now, UpdatedAt: now},
	}}
	server := newTaskSessionTestServer(store, "account-1")
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/session-1/messages", strings.NewReader(`{"clientRequestId":"request-1","text":"hello","run":{"priority":5}}`))
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if store.append.ClientRequestID != "request-1" || store.append.Priority != 5 {
		t.Fatalf("append input = %#v", store.append)
	}
	for _, expected := range []string{`"snapshotVersion":2`, `"type":"message.created"`, `"state":"queued"`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("body missing %s: %s", expected, response.Body.String())
		}
	}
}
