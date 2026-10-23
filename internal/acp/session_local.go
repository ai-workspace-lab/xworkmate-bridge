package acp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type localSessionStore struct {
	db            *sql.DB
	dir           string
	lock          *sql.Conn
	mu            sync.Mutex
	active        map[string]*localExecution
	wg            sync.WaitGroup
	closed        bool
	ownerID       string
	ownershipLost bool
	watchCancel   context.CancelFunc
	watchDone     chan struct{}
}

func openLocalSessionStore(ctx context.Context, dsn, dir string) (*localSessionStore, error) {
	if dsn == "" || !filepath.IsAbs(dir) {
		return nil, errors.New("session storage configuration required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("session directory must be private and not a symlink")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	lock, err := db.Conn(ctx)
	if err != nil {
		if closeErr := db.Close(); closeErr != nil {
			log.Printf("component=local_session event=database_close_failed")
		}
		return nil, err
	}
	var acquired bool
	err = lock.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(38483538)`).Scan(&acquired)
	if err != nil || !acquired {
		if closeErr := lock.Close(); closeErr != nil {
			log.Printf("component=local_session event=lock_close_failed")
		}
		if closeErr := db.Close(); closeErr != nil {
			log.Printf("component=local_session event=database_close_failed")
		}
		return nil, errors.New("local session database already in use")
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS bridge_local_sessions (
 id text PRIMARY KEY, owner_hash text NOT NULL, namespace_id text NOT NULL,
 create_key text NOT NULL, title_hash text NOT NULL, body_ref text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(owner_hash, namespace_id, create_key))`)
	if err != nil {
		if closeErr := lock.Close(); closeErr != nil {
			log.Printf("component=local_session event=lock_close_failed")
		}
		if closeErr := db.Close(); closeErr != nil {
			return nil, closeErr
		}
		return nil, err
	}
	store := &localSessionStore{db: db, dir: dir, lock: lock, active: make(map[string]*localExecution), ownerID: uuid.NewString()}
	if err := store.initializeRuns(ctx); err != nil {
		if closeErr := store.Close(); closeErr != nil {
			log.Printf("component=local_session event=store_close_failed")
		}
		return nil, err
	}
	watchCtx, watchCancel := context.WithCancel(context.Background())
	store.watchCancel = watchCancel
	store.watchDone = make(chan struct{})
	go store.watchOwnership(watchCtx)
	return store, nil
}

func localSessionOwner(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if !taskSessionBearerPresent(header) {
		return "", false
	}
	token := strings.TrimSpace(header[len("Bearer "):])
	valid := false
	for _, expected := range bridgeInboundAuthTokens() {
		valid = subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 || valid
	}
	// This is an explicitly single-user installation identity, not a credential
	// fingerprint. Every accepted static token belongs to this configured owner.
	identity := strings.TrimSpace(os.Getenv("BRIDGE_SESSION_OWNER_ID"))
	if identity == "" {
		identity = "local-single-user"
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:]), valid
}

func (s *Server) handleLocalSessionAPI(w http.ResponseWriter, r *http.Request) {
	owner, valid := localSessionOwner(r.Header.Get("Authorization"))
	if !valid {
		writeTaskSessionProxyError(w, 401, "invalid_bridge_authorization", "valid configured Bridge bearer required")
		return
	}
	s.localSessionOnce.Do(func() {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// Never log driver errors: connection strings can contain credentials.
		store, err := openLocalSessionStore(ctx, strings.TrimSpace(os.Getenv("BRIDGE_SESSION_PG_URL")), strings.TrimSpace(os.Getenv("BRIDGE_SESSION_DIR")))
		if err == nil {
			s.localSessionStore = store
		}
	})
	store := s.localSessionStore
	if store == nil {
		writeTaskSessionProxyError(w, 503, "local_session_storage_unavailable", "private session directory and PostgreSQL configuration required; restart after correcting configuration")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	respond := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("level=error component=local_session event=response_write_failed")
			return
		}
	}
	fail := func() {
		writeTaskSessionProxyError(w, 503, "local_session_storage_unavailable", "session storage operation failed")
	}
	if len(parts) == 1 {
		respond(200, map[string]any{"namespaces": []any{map[string]any{"namespaceId": "personal", "slug": "personal", "name": "Personal"}}})
		return
	}
	if parts[0] == "namespaces" {
		if parts[1] != "personal" {
			writeTaskSessionProxyError(w, 404, "namespace_not_found", "namespace not found")
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				Title           string `json:"title"`
				ClientRequestID string `json:"clientRequestId"`
			}
			if !decodeLocalPost(w, r, &body) {
				return
			}
			key := body.ClientRequestID
			if key == "" {
				key = uuid.NewString()
			}
			if !taskSessionProxyIDPattern.MatchString(key) {
				writeTaskSessionProxyError(w, 400, "invalid_request", "invalid clientRequestId")
				return
			}
			digest := sha256.Sum256([]byte(body.Title))
			titleHash := hex.EncodeToString(digest[:])
			id := uuid.NewString()
			ref, err := store.putBody(map[string]any{"title": body.Title})
			if err != nil {
				fail()
				return
			}
			var storedHash string
			err = store.db.QueryRowContext(ctx, `INSERT INTO bridge_local_sessions(id,owner_hash,namespace_id,create_key,title_hash,body_ref) VALUES($1,$2,'personal',$3,$4,$5) ON CONFLICT(owner_hash,namespace_id,create_key) DO UPDATE SET create_key=EXCLUDED.create_key RETURNING id,title_hash`, id, owner, key, titleHash, ref).Scan(&id, &storedHash)
			if err != nil {
				fail()
				return
			}
			if storedHash != titleHash {
				writeTaskSessionProxyError(w, 409, "idempotency_conflict", "clientRequestId already used with different content")
				return
			}
			snapshot, err := store.snapshot(ctx, owner, id)
			if err != nil {
				fail()
				return
			}
			respond(201, snapshot)
			return
		}
		rows, err := store.db.QueryContext(ctx, `SELECT id FROM bridge_local_sessions WHERE owner_hash=$1 AND namespace_id='personal' ORDER BY created_at,id LIMIT 100`, owner)
		if err != nil {
			fail()
			return
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				if closeErr := rows.Close(); closeErr != nil {
					log.Printf("component=local_session event=rows_close_failed")
				}
				fail()
				return
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil || closeErr != nil {
			fail()
			return
		}
		sessions := []any{}
		for _, id := range ids {
			snapshot, err := store.snapshot(ctx, owner, id)
			if err != nil {
				fail()
				return
			}
			sessions = append(sessions, snapshot)
		}
		respond(200, map[string]any{"sessions": sessions})
		return
	}
	snapshot, err := store.snapshot(ctx, owner, parts[1])
	if errors.Is(err, sql.ErrNoRows) {
		writeTaskSessionProxyError(w, 404, "session_not_found", "session not found")
		return
	}
	if err != nil {
		fail()
		return
	}
	if len(parts) == 2 {
		respond(200, snapshot)
		return
	}
	if parts[2] == "events" {
		page, err := store.events(ctx, parts[1], r.URL.Query())
		if err != nil {
			if errors.Is(err, errLocalEventQuery) {
				writeTaskSessionProxyError(w, 400, "invalid_event_query", "invalid event query")
			} else {
				fail()
			}
			return
		}
		respond(200, page)
		return
	}
	s.handleLocalRun(w, r.WithContext(ctx), owner, parts)
}

func (s *localSessionStore) snapshot(ctx context.Context, owner, id string) (map[string]any, error) {
	var ref string
	var created, updated time.Time
	var seq int64
	var state, runID, runState string
	// One statement gives the App a consistent cursor and latest run state.
	err := s.db.QueryRowContext(ctx, `SELECT s.body_ref,s.created_at,s.updated_at,s.event_seq,s.run_state,
 COALESCE(r.id,''),COALESCE(r.state,'') FROM bridge_local_sessions s
 LEFT JOIN LATERAL (SELECT id,state FROM bridge_local_runs WHERE session_id=s.id ORDER BY created_at DESC,id DESC LIMIT 1) r ON true
 WHERE s.id=$1 AND s.owner_hash=$2`, id, owner).Scan(&ref, &created, &updated, &seq, &state, &runID, &runState)
	if err != nil {
		return nil, err
	}
	body, err := s.readBody(ref)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"sessionId": id, "namespaceId": "personal", "title": body["title"], "snapshotVersion": seq + 1, "lastEventSeq": seq, "lifecycleState": "active", "context": map[string]any{"lastRunState": state}, "createdAt": created.UTC().Format(time.RFC3339Nano), "updatedAt": updated.UTC().Format(time.RFC3339Nano)}
	if runID != "" {
		result["taskRun"] = map[string]any{"id": runID, "state": runState}
	}
	return result, nil
}
