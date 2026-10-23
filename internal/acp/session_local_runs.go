package acp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"xworkmate-bridge/internal/shared"
)

type localExecution struct {
	cancel context.CancelFunc
	ctx    context.Context
}

var errLocalEventQuery = errors.New("invalid event query")

// One Bridge owns a local database at a time. The dedicated PostgreSQL lock
// connection prevents a second process from interrupting live work on startup.
func (s *localSessionStore) initializeRuns(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `ALTER TABLE bridge_local_sessions ADD COLUMN IF NOT EXISTS event_seq bigint NOT NULL DEFAULT 0;
 ALTER TABLE bridge_local_sessions ADD COLUMN IF NOT EXISTS run_state text NOT NULL DEFAULT 'idle';
 ALTER TABLE bridge_local_sessions ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();
 CREATE TABLE IF NOT EXISTS bridge_local_runs (
 id text PRIMARY KEY, session_id text NOT NULL REFERENCES bridge_local_sessions(id),
 request_key text NOT NULL, request_hash text NOT NULL, input_ref text NOT NULL,
 receipt_ref text NOT NULL, state text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(session_id,request_key));
 CREATE TABLE IF NOT EXISTS bridge_local_events (
 session_id text NOT NULL REFERENCES bridge_local_sessions(id), seq bigint NOT NULL,
 run_id text NOT NULL REFERENCES bridge_local_runs(id), type text NOT NULL,
 payload_ref text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(session_id,seq));
 CREATE TABLE IF NOT EXISTS bridge_local_cancel_requests (
 session_id text NOT NULL REFERENCES bridge_local_sessions(id), request_key text NOT NULL,
 run_id text NOT NULL REFERENCES bridge_local_runs(id), PRIMARY KEY(session_id,request_key));
 CREATE INDEX IF NOT EXISTS bridge_local_sessions_owner_idx ON bridge_local_sessions(owner_hash,namespace_id,created_at,id);
 CREATE INDEX IF NOT EXISTS bridge_local_runs_session_idx ON bridge_local_runs(session_id,created_at DESC,id DESC);
 CREATE UNIQUE INDEX IF NOT EXISTS bridge_local_runs_one_active_idx ON bridge_local_runs(session_id)
 WHERE state IN ('queued','running','cancel_requested');`)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id FROM bridge_local_runs WHERE state IN ('queued','running','cancel_requested')`)
	if err != nil {
		return err
	}
	type abandoned struct{ id, session string }
	runs := []abandoned{}
	for rows.Next() {
		var a abandoned
		if err := rows.Scan(&a.id, &a.session); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				log.Printf("component=local_session event=rows_close_failed")
			}
			return err
		}
		runs = append(runs, a)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, a := range runs {
		if err := s.finish(a.session, a.id, "failed", map[string]any{"code": "BRIDGE_RESTART_INTERRUPTED", "error": "Bridge restarted before execution settled; task was not reexecuted"}); err != nil {
			return err
		}
	}
	return nil
}

func (s *localSessionStore) Close() error {
	s.mu.Lock()
	s.closed = true
	for _, run := range s.active {
		run.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	if s.lock != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := s.lock.ExecContext(ctx, `SELECT pg_advisory_unlock(38483538)`)
		cancel()
		if err != nil {
			log.Printf("component=local_session event=unlock_failed")
		}
		if err := s.lock.Close(); err != nil {
			return err
		}
	}
	return s.db.Close()
}

func rollbackLocal(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		log.Printf("component=local_session event=rollback_failed")
	}
}

// Bodies (including event text, diagnostics and the original receipt) are immutable
// files. PostgreSQL contains only references, ordering and lifecycle metadata.
func (s *localSessionStore) putBody(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if len(data) > taskSessionProxyResponseMaxBytes/2 {
		return "", errors.New("session body exceeds limit")
	}
	ref := uuid.NewString() + ".json"
	f, err := os.OpenFile(filepath.Join(s.dir, ref), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if syncErr != nil {
		return "", syncErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return "", err
	}
	syncErr = directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil {
		return "", syncErr
	}
	return ref, closeErr
}

func (s *localSessionStore) readBody(ref string) (map[string]any, error) {
	if filepath.Base(ref) != ref || !strings.HasSuffix(ref, ".json") {
		return nil, errors.New("invalid body reference")
	}
	info, err := os.Lstat(filepath.Join(s.dir, ref))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > taskSessionProxyResponseMaxBytes/2 {
		return nil, errors.New("unsafe session body")
	}
	data, err := os.ReadFile(filepath.Join(s.dir, ref))
	if err != nil {
		return nil, err
	}
	var body map[string]any
	err = json.Unmarshal(data, &body)
	return body, err
}

func (s *localSessionStore) appendEvent(ctx context.Context, tx *sql.Tx, session, run, kind string, payload map[string]any) (map[string]any, error) {
	payload["taskRunId"] = run
	payload["schemaVersion"] = 1
	ref, err := s.putBody(payload)
	if err != nil {
		return nil, err
	}
	var seq int64
	var created time.Time
	err = tx.QueryRowContext(ctx, `UPDATE bridge_local_sessions SET event_seq=event_seq+1,updated_at=now() WHERE id=$1 RETURNING event_seq,updated_at`, session).Scan(&seq, &created)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO bridge_local_events(session_id,seq,run_id,type,payload_ref,created_at) VALUES($1,$2,$3,$4,$5,$6)`, session, seq, run, kind, ref, created)
	return map[string]any{"seq": seq, "type": kind, "payload": payload, "createdAt": created.UTC().Format(time.RFC3339Nano)}, err
}

func (s *localSessionStore) events(ctx context.Context, session string, q url.Values) (map[string]any, error) {
	after := int64(0)
	limit := 100
	var err error
	if q.Has("after_seq") {
		after, err = strconv.ParseInt(q.Get("after_seq"), 10, 64)
		if err != nil || after < 0 {
			return nil, errLocalEventQuery
		}
	}
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 500 {
			return nil, errLocalEventQuery
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,type,payload_ref,created_at FROM bridge_local_events WHERE session_id=$1 AND seq>$2 ORDER BY seq LIMIT $3`, session, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("component=local_session event=rows_close_failed")
		}
	}()
	events := []any{}
	last := after
	size := 0
	for rows.Next() {
		var seq int64
		var kind, ref string
		var created time.Time
		if err := rows.Scan(&seq, &kind, &ref, &created); err != nil {
			return nil, err
		}
		payload, err := s.readBody(ref)
		if err != nil {
			return nil, err
		}
		event := map[string]any{"seq": seq, "type": kind, "payload": payload, "createdAt": created.UTC().Format(time.RFC3339Nano)}
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		if size+len(encoded) > taskSessionProxyResponseMaxBytes-1024 {
			break
		}
		size += len(encoded)
		events = append(events, event)
		last = seq
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"events": events, "lastEventSeq": last}, nil
}

type localMessage struct {
	ClientRequestID string         `json:"clientRequestId"`
	Text            string         `json:"text"`
	Routing         map[string]any `json:"routing"`
	Run             map[string]any `json:"run"`
}

func decodeLocalPost(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	encoding := strings.TrimSpace(r.Header.Get("Content-Encoding"))
	if err != nil || !strings.EqualFold(media, "application/json") || (encoding != "" && !strings.EqualFold(encoding, "identity")) {
		writeTaskSessionProxyError(w, 415, "json_required", "uncompressed application/json required")
		return false
	}
	if r.ContentLength > taskSessionProxyRequestMaxBytes {
		writeTaskSessionProxyError(w, 413, "request_too_large", "task session request exceeds 131072 bytes")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskSessionProxyRequestMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeTaskSessionProxyError(w, 413, "request_too_large", "task session request exceeds 131072 bytes")
			return false
		}
		writeTaskSessionProxyError(w, 400, "invalid_request", "invalid request body")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeTaskSessionProxyError(w, 400, "invalid_request", "exactly one JSON object required")
		return false
	}
	return true
}

func (s *Server) handleLocalRun(w http.ResponseWriter, r *http.Request, owner string, parts []string) {
	store := s.localSessionStore
	if parts[2] == "runs" {
		var body struct {
			ClientRequestID string `json:"clientRequestId"`
		}
		if !decodeLocalPost(w, r, &body) {
			return
		}
		if !taskSessionProxyIDPattern.MatchString(body.ClientRequestID) {
			writeTaskSessionProxyError(w, 400, "invalid_request", "clientRequestId required")
			return
		}
		response, status, err := store.cancelRun(r.Context(), owner, parts[1], parts[3], body.ClientRequestID)
		if err != nil {
			writeTaskSessionProxyError(w, status, "cancel_failed", err.Error())
			return
		}
		writeLocalJSON(w, status, response)
		return
	}
	var body localMessage
	if !decodeLocalPost(w, r, &body) {
		return
	}
	if !taskSessionProxyIDPattern.MatchString(body.ClientRequestID) || strings.TrimSpace(body.Text) == "" {
		writeTaskSessionProxyError(w, 400, "invalid_request", "text and clientRequestId required")
		return
	}
	// No local scheduler, planner, role resolver or filesystem-path routing. Only
	// existing execution routing hints are passed to the canonical orchestrator.
	for key, value := range body.Routing {
		switch key {
		case "role", "roleMode", "roleModel", "productMode", "specialty", "dataClass", "projectId", "workspace", "attachments", "followUp", "previousTurnFailed", "model", "routingMode", "explicitExecutionTarget", "preferredGatewayProviderId", "preferredProviderId":
		default:
			writeTaskSessionProxyError(w, 422, "unsupported_routing", "routing hint not supported by standalone executor: "+key)
			return
		}
		if key == "workspace" {
			v, ok := value.(string)
			if !ok || strings.ContainsAny(v, "/\\") || v == ".." {
				writeTaskSessionProxyError(w, 400, "invalid_workspace", "workspace must be a key")
				return
			}
		}
	}
	// The default immediate run remains compatible; delayed/priority scheduling
	// belongs to a scheduler and is explicitly rejected by this provider.
	for key, value := range body.Run {
		if key != "priority" || value != float64(0) {
			writeTaskSessionProxyError(w, 422, "scheduling_not_supported", "standalone executor only supports immediate runs with priority 0")
			return
		}
	}
	if s.orchestrator == nil {
		writeTaskSessionProxyError(w, 503, "executor_unavailable", "execution pipeline not initialized")
		return
	}
	receipt, status, err := s.acceptLocalMessage(r.Context(), owner, parts[1], body)
	if err != nil {
		writeTaskSessionProxyError(w, status, "message_rejected", err.Error())
		return
	}
	writeLocalJSON(w, status, receipt)
}

func writeLocalJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("component=local_session event=response_write_failed")
	}
}

func (s *Server) acceptLocalMessage(ctx context.Context, owner, session string, body localMessage) (map[string]any, int, error) {
	store := s.localSessionStore
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil, 503, errors.New("session store closed")
	}
	// Check lock ownership before each new dispatch. A lost lock connection must
	// never allow a former owner to begin another external side effect.
	var one int
	if err := store.lock.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return nil, 503, errors.New("session ownership connection lost")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 503, errors.New("session storage unavailable")
	}
	defer rollbackLocal(tx)
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM bridge_local_sessions WHERE id=$1 AND owner_hash=$2 FOR UPDATE`, session, owner).Scan(&id); err != nil {
		return nil, 404, errors.New("session not found")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, 400, err
	}
	digest := sha256.Sum256(encoded)
	hash := hex.EncodeToString(digest[:])
	var oldHash, receiptRef string
	err = tx.QueryRowContext(ctx, `SELECT request_hash,receipt_ref FROM bridge_local_runs WHERE session_id=$1 AND request_key=$2`, session, body.ClientRequestID).Scan(&oldHash, &receiptRef)
	if err == nil {
		if hash != oldHash {
			return nil, 409, errors.New("clientRequestId already used with different content")
		}
		receipt, err := store.readBody(receiptRef)
		if err != nil {
			return nil, 503, errors.New("persisted receipt unavailable")
		}
		return receipt, 200, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, 503, errors.New("session storage unavailable")
	}
	var busy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM bridge_local_runs WHERE session_id=$1 AND state IN ('queued','running','cancel_requested'))`, session).Scan(&busy); err != nil {
		return nil, 503, errors.New("session storage unavailable")
	}
	if busy {
		return nil, 409, errors.New("session already has an active run")
	}
	var previous int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM bridge_local_runs WHERE session_id=$1`, session).Scan(&previous); err != nil {
		return nil, 503, errors.New("session storage unavailable")
	}
	inputRef, err := store.putBody(body)
	if err != nil {
		return nil, 503, errors.New("message body storage unavailable")
	}
	run := uuid.NewString()
	// Placeholder is replaced before commit; no reader can observe it.
	_, err = tx.ExecContext(ctx, `INSERT INTO bridge_local_runs(id,session_id,request_key,request_hash,input_ref,receipt_ref,state) VALUES($1,$2,$3,$4,$5,'','queued')`, run, session, body.ClientRequestID, hash, inputRef)
	if err != nil {
		return nil, 503, errors.New("run storage unavailable")
	}
	event, err := store.appendEvent(ctx, tx, session, run, "message.created", map[string]any{"text": body.Text, "clientRequestId": body.ClientRequestID})
	if err != nil {
		return nil, 503, errors.New("event storage unavailable")
	}
	queued, err := store.appendEvent(ctx, tx, session, run, "run.queued", map[string]any{})
	if err != nil {
		return nil, 503, errors.New("event storage unavailable")
	}
	receipt := map[string]any{"sessionId": session, "namespaceId": "personal", "snapshotVersion": queued["seq"].(int64) + 1, "event": event, "taskRun": map[string]any{"id": run, "state": "queued"}}
	receiptRef, err = store.putBody(receipt)
	if err != nil {
		return nil, 503, errors.New("receipt storage unavailable")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE bridge_local_runs SET receipt_ref=$2 WHERE id=$1`, run, receiptRef); err != nil {
		return nil, 503, errors.New("receipt storage unavailable")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE bridge_local_sessions SET run_state='queued' WHERE id=$1`, session); err != nil {
		return nil, 503, errors.New("session storage unavailable")
	}
	if err = tx.Commit(); err != nil {
		return nil, 503, errors.New("message commit unconfirmed; retry with same clientRequestId")
	}
	executionCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	store.active[run] = &localExecution{ctx: executionCtx, cancel: cancel}
	store.wg.Add(1)
	method := "session.start"
	if previous > 0 {
		method = "session.message"
	}
	go s.executeLocal(executionCtx, session, run, method, body)
	return receipt, 202, nil
}

func (s *Server) executeLocal(ctx context.Context, session, run, method string, body localMessage) {
	store := s.localSessionStore
	defer store.wg.Done()
	defer func() {
		store.mu.Lock()
		if active := store.active[run]; active != nil {
			active.cancel()
			delete(store.active, run)
		}
		store.mu.Unlock()
	}()
	if err := store.transition(session, run, "running", "run.running", map[string]any{}); err != nil {
		log.Printf("component=local_session event=run_start_persist_failed")
		if err := store.finish(session, run, "failed", map[string]any{"code": "RUN_START_PERSIST_FAILED", "error": "execution was not dispatched because running state could not be persisted"}); err != nil {
			log.Printf("component=local_session event=run_finish_persist_failed")
		}
		return
	}
	params := map[string]any{"sessionId": session, "threadId": session, "taskPrompt": body.Text, "routing": body.Routing}
	// Use the existing resolver's configured gateway (DSH in standalone setups).
	// No model/provider selection is implemented in the persistence layer.
	params["executionTarget"] = "gateway"
	if model, ok := body.Routing["model"].(string); ok {
		params["model"] = model
	}
	var result map[string]any
	var rpcErr *shared.RPCError
	// Process owns provider routing and execution; this layer only journals its outcome.
	func() {
		defer func() {
			if recover() != nil {
				rpcErr = &shared.RPCError{Code: -32002, Message: "executor panicked"}
			}
		}()
		if ctx.Err() == nil {
			result, rpcErr = s.orchestrator.Process(ctx, method, params, nil)
		}
	}()
	state := "failed"
	payload := map[string]any{"code": "EXECUTION_FAILED", "error": "executor did not return a completed result"}
	if errors.Is(ctx.Err(), context.Canceled) {
		state = "cancelled"
		payload = map[string]any{"code": "CONTEXT_CANCELLED"}
	} else if ctx.Err() != nil {
		payload = map[string]any{"code": "EXECUTION_TIMEOUT", "error": "execution deadline exceeded"}
	} else if rpcErr != nil {
		payload = map[string]any{"code": fmt.Sprint(rpcErr.Code), "error": rpcErr.Message}
	} else {
		payload = result
		if payload == nil {
			payload = map[string]any{}
		}
		if result["success"] == true && result["status"] == "completed" {
			state = "completed"
		} else {
			if code, ok := payload["unavailableCode"].(string); ok && code != "" {
				payload["code"] = code
				payload["error"] = payload["unavailableMessage"]
			}
			if payload["error"] == nil {
				payload["error"] = "executor did not complete successfully"
			}
			if payload["code"] == nil {
				payload["code"] = "EXECUTOR_NOT_COMPLETED"
			}
		}
		if executor, ok := payload["resolvedProviderId"].(string); ok {
			payload["executor"] = executor
		}
		if model, ok := payload["resolvedModel"].(string); ok {
			payload["resolvedModelId"] = model
		}
	}
	if err := store.finish(session, run, state, payload); err != nil {
		log.Printf("component=local_session event=run_finish_persist_failed")
		// Recording failure is not executor retry: never repeat an external side
		// effect just because its result could not be durably journaled.
		if err := store.finish(session, run, "failed", map[string]any{"code": "RESULT_PERSIST_FAILED", "error": "executor returned but its result could not be durably recorded"}); err != nil {
			log.Printf("component=local_session event=run_failure_persist_failed")
		}
	}
}

func (s *localSessionStore) transition(session, run, state, kind string, payload map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackLocal(tx)
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM bridge_local_sessions WHERE id=$1 FOR UPDATE`, session).Scan(&id); err != nil {
		return err
	}
	var old string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM bridge_local_runs WHERE id=$1 AND session_id=$2`, run, session).Scan(&old); err != nil {
		return err
	}
	if old != "queued" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bridge_local_runs SET state=$2 WHERE id=$1`, run, state); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bridge_local_sessions SET run_state=$2 WHERE id=$1`, session, state); err != nil {
		return err
	}
	if _, err := s.appendEvent(ctx, tx, session, run, kind, payload); err != nil {
		return err
	}
	if _, err := s.appendEvent(ctx, tx, session, run, "run.progressed", map[string]any{"phase": "started"}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *localSessionStore) finish(session, run, state string, payload map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackLocal(tx)
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM bridge_local_sessions WHERE id=$1 FOR UPDATE`, session).Scan(&id); err != nil {
		return err
	}
	var old string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM bridge_local_runs WHERE id=$1 AND session_id=$2`, run, session).Scan(&old); err != nil {
		return err
	}
	if old != "queued" && old != "running" && old != "cancel_requested" {
		return nil
	}
	// Cancel and completion linearize under this same session lock. A requested
	// cancellation becomes terminal only after the execution goroutine returns.
	if old == "cancel_requested" && payload["code"] != "BRIDGE_RESTART_INTERRUPTED" {
		state = "cancelled"
		payload = map[string]any{"code": "CONTEXT_CANCELLED"}
	}
	if state == "completed" {
		text := shared.StringArg(payload, "output", "")
		if text == "" {
			text = shared.StringArg(payload, "summary", "")
		}
		if _, err := s.appendEvent(ctx, tx, session, run, "assistant.message.created", map[string]any{"text": text}); err != nil {
			return err
		}
	}
	if _, err := s.appendEvent(ctx, tx, session, run, "run."+state, payload); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bridge_local_runs SET state=$2 WHERE id=$1`, run, state); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bridge_local_sessions SET run_state=$2 WHERE id=$1`, session, state); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *localSessionStore) cancelRun(ctx context.Context, owner, session, run, key string) (map[string]any, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 503, errors.New("session storage unavailable")
	}
	defer rollbackLocal(tx)
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM bridge_local_sessions WHERE id=$1 AND owner_hash=$2 FOR UPDATE`, session, owner).Scan(&id); err != nil {
		return nil, 404, errors.New("session not found")
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM bridge_local_runs WHERE id=$1 AND session_id=$2`, run, session).Scan(&state); err != nil {
		return nil, 404, errors.New("run not found")
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT run_id FROM bridge_local_cancel_requests WHERE session_id=$1 AND request_key=$2`, session, key).Scan(&previous)
	if err == nil && previous != run {
		return nil, 409, errors.New("clientRequestId already used for another run")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 503, errors.New("cancel storage unavailable")
	}
	active := s.active[run]
	if state == "queued" || state == "running" {
		if active == nil {
			return nil, 409, errors.New("execution context unavailable; restart required to reconcile")
		}
		if _, err := s.appendEvent(ctx, tx, session, run, "run.cancel_requested", map[string]any{"clientRequestId": key}); err != nil {
			return nil, 503, errors.New("cancel event unavailable")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE bridge_local_runs SET state='cancel_requested' WHERE id=$1`, run); err != nil {
			return nil, 503, errors.New("cancel storage unavailable")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE bridge_local_sessions SET run_state='cancel_requested' WHERE id=$1`, session); err != nil {
			return nil, 503, errors.New("cancel storage unavailable")
		}
		state = "cancel_requested"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO bridge_local_cancel_requests(session_id,request_key,run_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, session, key, run); err != nil {
		return nil, 503, errors.New("cancel storage unavailable")
	}
	if err := tx.Commit(); err != nil {
		return nil, 503, errors.New("cancel commit unconfirmed; retry same clientRequestId")
	}
	cancelledContext := false
	if state == "cancel_requested" && active != nil {
		active.cancel()
		cancelledContext = active.ctx.Err() != nil
	}
	return map[string]any{"taskRun": map[string]any{"id": run, "state": state}, "contextCancelled": cancelledContext}, 200, nil
}
