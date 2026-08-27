package sessionstore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

const DatabaseURLEnv = "XWORKMATE_SESSION_DATABASE_URL"

//go:embed migrations/*.sql
var migrations embed.FS

type PostgresStore struct {
	db *sql.DB
}

func OpenFromEnv(ctx context.Context) (*PostgresStore, error) {
	databaseURL := strings.TrimSpace(os.Getenv(DatabaseURLEnv))
	if databaseURL == "" {
		return nil, nil
	}
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", DatabaseURLEnv, err)
	}
	store := &PostgresStore{db: db}
	if err := db.PingContext(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("connect session database: %v; close database: %w", err, closeErr)
		}
		return nil, fmt.Errorf("connect session database: %w", err)
	}
	if err := store.migrate(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("%v; close session database: %w", err, closeErr)
		}
		return nil, err
	}
	return store, nil
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read session migrations: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		statement, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read session migration %s: %w", entry.Name(), err)
		}
		if _, err := s.db.ExecContext(ctx, string(statement)); err != nil {
			return fmt.Errorf("apply session migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (s *PostgresStore) Close() error {
	if s != nil && s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *PostgresStore) ListNamespaces(ctx context.Context, accountID string) ([]Namespace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT namespace_id, name, created_at, updated_at FROM xworkmate_session_namespaces WHERE account_id=$1 ORDER BY updated_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Namespace, 0)
	for rows.Next() {
		var item Namespace
		if err := rows.Scan(&item.NamespaceID, &item.Name, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresStore) CreateSession(ctx context.Context, accountID, namespaceID string, input CreateSessionInput) (Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, err
	}
	defer rollbackTransaction(tx)
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO xworkmate_session_namespaces(account_id, namespace_id, name, created_at, updated_at) VALUES($1,$2,$2,$3,$3) ON CONFLICT(account_id,namespace_id) DO UPDATE SET updated_at=EXCLUDED.updated_at`, accountID, namespaceID, now); err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{SessionID: uuid.NewString(), NamespaceID: namespaceID, Title: strings.TrimSpace(input.Title), SnapshotVersion: 1, LastEventSeq: 1, LifecycleState: "active", Context: map[string]any{"schemaVersion": 1, "messages": []any{}}, CreatedAt: now, UpdatedAt: now}
	contextJSON, err := json.Marshal(snapshot.Context)
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO xworkmate_task_sessions(session_id,account_id,namespace_id,title,lifecycle_state,snapshot_version,last_event_seq,context,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`, snapshot.SessionID, accountID, namespaceID, snapshot.Title, snapshot.LifecycleState, snapshot.SnapshotVersion, snapshot.LastEventSeq, contextJSON, now); err != nil {
		return Snapshot{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO xworkmate_session_events(session_id,seq,event_type,payload,created_at) VALUES($1,1,'session.created',$2,$3)`, snapshot.SessionID, []byte(`{"schemaVersion":1}`), now); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *PostgresStore) ListSessions(ctx context.Context, accountID, namespaceID string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, snapshotSelect+` WHERE s.account_id=$1 AND s.namespace_id=$2 ORDER BY s.updated_at DESC`, accountID, namespaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Snapshot, 0)
	for rows.Next() {
		item, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresStore) GetSession(ctx context.Context, accountID, sessionID string) (Snapshot, error) {
	snapshot, err := scanSnapshot(s.db.QueryRowContext(ctx, snapshotSelect+` WHERE s.account_id=$1 AND s.session_id=$2`, accountID, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return snapshot, err
}

const snapshotSelect = `SELECT s.session_id,s.namespace_id,s.title,s.snapshot_version,s.last_event_seq,s.lifecycle_state,s.context,s.created_at,s.updated_at,r.run_id,r.state,r.bridge_task_ref,r.priority,r.not_before,r.created_at,r.updated_at FROM xworkmate_task_sessions s LEFT JOIN xworkmate_task_runs r ON r.run_id=s.current_task_run_id`

type rowScanner interface{ Scan(...any) error }

func scanSnapshot(row rowScanner) (Snapshot, error) {
	var snapshot Snapshot
	var contextJSON []byte
	var runID, runState, bridgeTaskRef *string
	var priority *int
	var notBefore, runCreatedAt, runUpdatedAt *time.Time
	err := row.Scan(&snapshot.SessionID, &snapshot.NamespaceID, &snapshot.Title, &snapshot.SnapshotVersion, &snapshot.LastEventSeq, &snapshot.LifecycleState, &contextJSON, &snapshot.CreatedAt, &snapshot.UpdatedAt, &runID, &runState, &bridgeTaskRef, &priority, &notBefore, &runCreatedAt, &runUpdatedAt)
	if err != nil {
		return Snapshot{}, err
	}
	if err := json.Unmarshal(contextJSON, &snapshot.Context); err != nil {
		return Snapshot{}, fmt.Errorf("decode session context: %w", err)
	}
	if runID != nil {
		snapshot.TaskRun = &TaskRun{ID: *runID, State: valueOr(runState, "queued"), BridgeTaskRef: valueOr(bridgeTaskRef, ""), Priority: intValueOr(priority, 0), NotBefore: notBefore, CreatedAt: timeValueOr(runCreatedAt), UpdatedAt: timeValueOr(runUpdatedAt)}
	}
	return snapshot, nil
}

func (s *PostgresStore) ListEvents(ctx context.Context, accountID, sessionID string, afterSeq int64, limit int) ([]Event, int64, error) {
	var lastEventSeq int64
	if err := s.db.QueryRowContext(ctx, `SELECT last_event_seq FROM xworkmate_task_sessions WHERE account_id=$1 AND session_id=$2`, accountID, sessionID).Scan(&lastEventSeq); errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNotFound
	} else if err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,event_type,payload,created_at FROM xworkmate_session_events WHERE session_id=$1 AND seq>$2 ORDER BY seq LIMIT $3`, sessionID, afterSeq, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var payload []byte
		if err := rows.Scan(&event.Seq, &event.Type, &payload, &event.CreatedAt); err != nil {
			return nil, 0, err
		}
		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			return nil, 0, err
		}
		events = append(events, event)
	}
	return events, lastEventSeq, rows.Err()
}

func (s *PostgresStore) AppendMessage(ctx context.Context, accountID, sessionID string, input AppendMessageInput) (AppendMessageResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AppendMessageResult{}, err
	}
	defer rollbackTransaction(tx)
	var namespaceID string
	var version, lastSeq int64
	var contextJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT namespace_id,snapshot_version,last_event_seq,context FROM xworkmate_task_sessions WHERE account_id=$1 AND session_id=$2 FOR UPDATE`, accountID, sessionID).Scan(&namespaceID, &version, &lastSeq, &contextJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return AppendMessageResult{}, ErrNotFound
	}
	if err != nil {
		return AppendMessageResult{}, err
	}
	if existing, ok, err := loadExistingMessage(ctx, tx, sessionID, input.ClientRequestID); err != nil {
		return AppendMessageResult{}, err
	} else if ok {
		existing.NamespaceID = namespaceID
		existing.SnapshotVersion = version
		existing.Existing = true
		return existing, tx.Commit()
	}
	now := time.Now().UTC()
	messageID, runID := uuid.NewString(), uuid.NewString()
	payload := map[string]any{"schemaVersion": 1, "messageId": messageID, "clientRequestId": input.ClientRequestID, "role": "user", "text": input.Text}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return AppendMessageResult{}, err
	}
	var contextValue map[string]any
	if err := json.Unmarshal(contextJSON, &contextValue); err != nil {
		return AppendMessageResult{}, err
	}
	messages, _ := contextValue["messages"].([]any)
	contextValue["messages"] = append(messages, map[string]any{"id": messageID, "role": "user", "text": input.Text, "createdAt": now})
	contextJSON, err = json.Marshal(contextValue)
	if err != nil {
		return AppendMessageResult{}, err
	}
	event := Event{Seq: lastSeq + 1, Type: "message.created", Payload: payload, CreatedAt: now}
	runEventPayload, err := json.Marshal(map[string]any{"schemaVersion": 1, "runId": runID, "state": "queued", "priority": input.Priority})
	if err != nil {
		return AppendMessageResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO xworkmate_task_runs(run_id,session_id,state,priority,not_before,created_at,updated_at) VALUES($1,$2,'queued',$3,$4,$5,$5)`, runID, sessionID, input.Priority, input.NotBefore, now); err != nil {
		return AppendMessageResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO xworkmate_session_messages(message_id,session_id,task_run_id,client_request_id,role,text,created_at) VALUES($1,$2,$3,$4,'user',$5,$6)`, messageID, sessionID, runID, input.ClientRequestID, input.Text, now); err != nil {
		return AppendMessageResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO xworkmate_session_events(session_id,seq,event_type,payload,created_at) VALUES($1,$2,$3,$4,$5)`, sessionID, event.Seq, event.Type, payloadJSON, now); err != nil {
		return AppendMessageResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO xworkmate_session_events(session_id,seq,event_type,payload,created_at) VALUES($1,$2,'run.queued',$3,$4)`, sessionID, event.Seq+1, runEventPayload, now); err != nil {
		return AppendMessageResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE xworkmate_task_sessions SET snapshot_version=snapshot_version+1,last_event_seq=$2,context=$3,current_task_run_id=$4,updated_at=$5 WHERE session_id=$1`, sessionID, event.Seq+1, contextJSON, runID, now); err != nil {
		return AppendMessageResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AppendMessageResult{}, err
	}
	return AppendMessageResult{SessionID: sessionID, NamespaceID: namespaceID, SnapshotVersion: version + 1, Event: event, TaskRun: TaskRun{ID: runID, State: "queued", Priority: input.Priority, NotBefore: input.NotBefore, CreatedAt: now, UpdatedAt: now}}, nil
}

func loadExistingMessage(ctx context.Context, tx *sql.Tx, sessionID, clientRequestID string) (AppendMessageResult, bool, error) {
	var result AppendMessageResult
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT e.seq,e.payload,e.created_at,r.run_id,r.state,r.bridge_task_ref,r.priority,r.not_before,r.created_at,r.updated_at FROM xworkmate_session_messages m JOIN xworkmate_session_events e ON e.session_id=m.session_id AND e.payload->>'messageId'=m.message_id::text JOIN xworkmate_task_runs r ON r.run_id=m.task_run_id WHERE m.session_id=$1 AND m.client_request_id=$2`, sessionID, clientRequestID).Scan(&result.Event.Seq, &payload, &result.Event.CreatedAt, &result.TaskRun.ID, &result.TaskRun.State, &result.TaskRun.BridgeTaskRef, &result.TaskRun.Priority, &result.TaskRun.NotBefore, &result.TaskRun.CreatedAt, &result.TaskRun.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AppendMessageResult{}, false, nil
	}
	if err != nil {
		return AppendMessageResult{}, false, err
	}
	result.SessionID = sessionID
	result.Event.Type = "message.created"
	if err := json.Unmarshal(payload, &result.Event.Payload); err != nil {
		return AppendMessageResult{}, false, err
	}
	return result, true, nil
}

func (s *PostgresStore) RecordACPUpdate(ctx context.Context, accountID string, update ACPUpdate) error {
	// ACP persistence is intentionally reduced to conversational/task metadata.
	// No generic params/result JSON is accepted here, preventing artifacts from
	// crossing the storage boundary.
	if strings.TrimSpace(update.SessionID) == "" {
		return nil
	}
	// REST-created sessions are the source of ownership. ACP may update one but
	// cannot claim an arbitrary session ID or namespace.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackTransaction(tx)
	var currentRunID sql.NullString
	var lastSeq int64
	err = tx.QueryRowContext(ctx, `SELECT current_task_run_id::text,last_event_seq FROM xworkmate_task_sessions WHERE account_id=$1 AND session_id=$2 FOR UPDATE`, accountID, update.SessionID).Scan(&currentRunID, &lastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !currentRunID.Valid {
		return nil
	}
	state := strings.TrimSpace(update.TaskRunState)
	if state == "" {
		state = "running"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE xworkmate_task_runs SET state=$1,bridge_task_ref=CASE WHEN $2='' THEN bridge_task_ref ELSE $2 END,updated_at=$3 WHERE run_id=$4`, state, update.TaskRunID, update.OccurredAt, currentRunID.String); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"schemaVersion": 1, "runId": currentRunID.String, "bridgeTaskRef": update.TaskRunID, "state": state})
	if err != nil {
		return err
	}
	nextSeq := lastSeq + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO xworkmate_session_events(session_id,seq,event_type,payload,created_at) VALUES($1,$2,'run.state_changed',$3,$4)`, update.SessionID, nextSeq, payload, update.OccurredAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE xworkmate_task_sessions SET snapshot_version=snapshot_version+1,last_event_seq=$2,updated_at=$3 WHERE session_id=$1`, update.SessionID, nextSeq, update.OccurredAt); err != nil {
		return err
	}
	return tx.Commit()
}

func valueOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}
func intValueOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}
func timeValueOr(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func rollbackTransaction(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		log.Printf("level=error component=session_store event=transaction_rollback_failed error=%q", err)
	}
}
