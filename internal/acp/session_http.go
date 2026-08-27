package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"xworkmate-bridge/internal/service"
	"xworkmate-bridge/internal/sessionstore"
	"xworkmate-bridge/internal/shared"
)

const taskSessionRequestMaxBytes = 128 * 1024

var taskSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type authorizationPrincipalResolver interface {
	ResolveAuthorizationHeader(context.Context, string) (service.AuthorizationPrincipal, error)
}

func (s *Server) handleTaskSessionAPI(w http.ResponseWriter, r *http.Request) {
	shared.ApplyCORS(w, r, s.allowedOrigins)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.sessionStore == nil {
		writeTaskSessionError(w, http.StatusServiceUnavailable, "session_store_unavailable", "task session persistence is not configured")
		return
	}
	accountID, ok := s.resolveTaskSessionAccount(r)
	if !ok {
		writeTaskSessionError(w, http.StatusUnauthorized, "authorization_principal_required", "a verified account principal is required")
		return
	}
	segments := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/"), "/")
	for index := range segments {
		if !taskSessionIDPattern.MatchString(segments[index]) {
			writeTaskSessionError(w, http.StatusBadRequest, "invalid_path", "invalid task session path")
			return
		}
	}
	switch {
	case len(segments) == 1 && segments[0] == "namespaces":
		s.handleNamespaceList(w, r, accountID)
	case len(segments) == 3 && segments[0] == "namespaces" && segments[2] == "sessions":
		s.handleNamespaceSessions(w, r, accountID, segments[1])
	case len(segments) == 2 && segments[0] == "sessions":
		s.handleSessionSnapshot(w, r, accountID, segments[1])
	case len(segments) == 3 && segments[0] == "sessions" && segments[2] == "events":
		s.handleSessionEvents(w, r, accountID, segments[1])
	case len(segments) == 3 && segments[0] == "sessions" && segments[2] == "messages":
		s.handleSessionMessages(w, r, accountID, segments[1])
	default:
		writeTaskSessionError(w, http.StatusNotFound, "route_not_found", "task session route not found")
	}
}

func (s *Server) resolveTaskSessionAccount(r *http.Request) (string, bool) {
	resolver, ok := s.authService.(authorizationPrincipalResolver)
	if !ok {
		return "", false
	}
	principal, err := resolver.ResolveAuthorizationHeader(r.Context(), r.Header.Get("Authorization"))
	if err != nil || strings.TrimSpace(principal.AccountID) == "" {
		return "", false
	}
	return strings.TrimSpace(principal.AccountID), true
}

func (s *Server) recordTaskSessionRPCUpdate(r *http.Request, request shared.RPCRequest, result map[string]any, rpcErr *shared.RPCError) {
	if s.sessionStore == nil || (request.Method != "session.start" && request.Method != "session.message") {
		return
	}
	accountID, ok := s.resolveTaskSessionAccount(r)
	if !ok {
		return
	}
	params := shared.AsMap(request.Params)
	state := "completed"
	if rpcErr != nil {
		state = "failed"
	} else if resultState := strings.TrimSpace(shared.StringArg(result, "status", "")); resultState != "" {
		state = resultState
	}
	runID := strings.TrimSpace(shared.StringArg(result, "runId", ""))
	if runID == "" {
		runID = strings.TrimSpace(shared.StringArg(result, "taskId", ""))
	}
	update := sessionstore.ACPUpdate{
		SessionID:    strings.TrimSpace(shared.StringArg(params, "sessionId", "")),
		ThreadID:     strings.TrimSpace(shared.StringArg(params, "threadId", "")),
		Method:       request.Method,
		Text:         strings.TrimSpace(shared.StringArg(params, "taskPrompt", "")),
		Provider:     strings.TrimSpace(shared.StringArg(result, "resolvedProviderId", "")),
		Target:       strings.TrimSpace(shared.StringArg(result, "resolvedExecutionTarget", "")),
		TaskRunID:    runID,
		TaskRunState: state,
		OccurredAt:   time.Now().UTC(),
	}
	if err := s.sessionStore.RecordACPUpdate(r.Context(), accountID, update); err != nil {
		log.Printf("level=error component=session_store event=rpc_update_failed sessionId=%q error=%q", update.SessionID, err)
	}
}

func (s *Server) handleNamespaceList(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodGet {
		writeTaskSessionError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	items, err := s.sessionStore.ListNamespaces(r.Context(), accountID)
	if err != nil {
		writeTaskSessionError(w, http.StatusInternalServerError, "session_store_unavailable", "task session store unavailable")
		return
	}
	writeTaskSessionJSON(w, http.StatusOK, map[string]any{"namespaces": items})
}

func (s *Server) handleNamespaceSessions(w http.ResponseWriter, r *http.Request, accountID, namespaceID string) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.sessionStore.ListSessions(r.Context(), accountID, namespaceID)
		if err != nil {
			writeTaskSessionError(w, http.StatusInternalServerError, "session_store_unavailable", "task session store unavailable")
			return
		}
		writeTaskSessionJSON(w, http.StatusOK, map[string]any{"sessions": items})
	case http.MethodPost:
		var request struct {
			Title string `json:"title"`
		}
		if !decodeTaskSessionJSON(w, r, &request) {
			return
		}
		if len([]rune(request.Title)) > 256 {
			writeTaskSessionError(w, http.StatusBadRequest, "invalid_session", "title must not exceed 256 characters")
			return
		}
		snapshot, err := s.sessionStore.CreateSession(r.Context(), accountID, namespaceID, sessionstore.CreateSessionInput{Title: request.Title})
		if err != nil {
			writeTaskSessionStoreError(w, err)
			return
		}
		writeTaskSessionJSON(w, http.StatusCreated, snapshot)
	default:
		writeTaskSessionError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleSessionSnapshot(w http.ResponseWriter, r *http.Request, accountID, sessionID string) {
	if r.Method != http.MethodGet {
		writeTaskSessionError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	snapshot, err := s.sessionStore.GetSession(r.Context(), accountID, sessionID)
	if err != nil {
		writeTaskSessionStoreError(w, err)
		return
	}
	writeTaskSessionJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request, accountID, sessionID string) {
	if r.Method != http.MethodGet {
		writeTaskSessionError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	afterSeq, err := parseNonNegativeInt64(r.URL.Query().Get("after_seq"), 0)
	if err != nil {
		writeTaskSessionError(w, http.StatusBadRequest, "invalid_cursor", "after_seq must be a non-negative integer")
		return
	}
	limit, err := parseEventLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeTaskSessionError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 500")
		return
	}
	events, lastSeq, err := s.sessionStore.ListEvents(r.Context(), accountID, sessionID, afterSeq, limit)
	if err != nil {
		writeTaskSessionStoreError(w, err)
		return
	}
	writeTaskSessionJSON(w, http.StatusOK, map[string]any{"events": events, "lastEventSeq": lastSeq})
}

func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request, accountID, sessionID string) {
	if r.Method != http.MethodPost {
		writeTaskSessionError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	var request struct {
		ClientRequestID string `json:"clientRequestId"`
		Text            string `json:"text"`
		Run             *struct {
			Priority  int        `json:"priority"`
			NotBefore *time.Time `json:"notBefore,omitempty"`
		} `json:"run,omitempty"`
	}
	if !decodeTaskSessionJSON(w, r, &request) {
		return
	}
	request.ClientRequestID = strings.TrimSpace(request.ClientRequestID)
	request.Text = strings.TrimSpace(request.Text)
	if !taskSessionIDPattern.MatchString(request.ClientRequestID) || request.Text == "" || len([]byte(request.Text)) > 64*1024 {
		writeTaskSessionError(w, http.StatusBadRequest, "invalid_message", "clientRequestId and text are required; text must not exceed 65536 bytes")
		return
	}
	input := sessionstore.AppendMessageInput{ClientRequestID: request.ClientRequestID, Text: request.Text}
	if request.Run != nil {
		if request.Run.Priority < -100 || request.Run.Priority > 100 {
			writeTaskSessionError(w, http.StatusBadRequest, "invalid_priority", "run priority must be between -100 and 100")
			return
		}
		input.Priority = request.Run.Priority
		input.NotBefore = request.Run.NotBefore
	}
	result, err := s.sessionStore.AppendMessage(r.Context(), accountID, sessionID, input)
	if err != nil {
		writeTaskSessionStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Existing {
		status = http.StatusOK
	}
	writeTaskSessionJSON(w, status, result)
}

func decodeTaskSessionJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, taskSessionRequestMaxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeTaskSessionError(w, http.StatusBadRequest, "invalid_json", "invalid request payload")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeTaskSessionError(w, http.StatusBadRequest, "invalid_json", "request must contain one JSON object")
		return false
	}
	return true
}

func parseNonNegativeInt64(value string, fallback int64) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("invalid non-negative integer")
	}
	return parsed, nil
}

func parseEventLimit(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return 100, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > 500 {
		return 0, errors.New("invalid limit")
	}
	return parsed, nil
}

func writeTaskSessionStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessionstore.ErrNotFound):
		// Ownership mismatches deliberately collapse to not-found.
		writeTaskSessionError(w, http.StatusNotFound, "task_session_not_found", "task session not found")
	case errors.Is(err, sessionstore.ErrConflict):
		writeTaskSessionError(w, http.StatusConflict, "task_session_conflict", "task session conflict")
	default:
		writeTaskSessionError(w, http.StatusInternalServerError, "session_store_unavailable", "task session store unavailable")
	}
}

func writeTaskSessionError(w http.ResponseWriter, status int, code, message string) {
	writeTaskSessionJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func writeTaskSessionJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("level=error component=session_api event=response_encode_failed error=%q", err)
	}
}
