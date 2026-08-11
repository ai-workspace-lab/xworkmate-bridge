package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

const taskRunDispatchMaxBodyBytes = 64 * 1024

func (s *Server) HandleTaskRunDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeTaskRunHTTPError(w, http.StatusMethodNotAllowed, "TASK_RUN_METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		writeTaskRunHTTPError(w, http.StatusForbidden, "TASK_RUN_BROWSER_CALL_FORBIDDEN", "browser calls are not allowed")
		return
	}
	if !s.taskRunAuthorized(r) {
		writeTaskRunHTTPError(w, http.StatusUnauthorized, "TASK_RUN_SERVICE_AUTH_REQUIRED", "internal service authorization required")
		return
	}
	dispatcher := s.configuredTaskRunDispatcher()
	if dispatcher == nil {
		writeTaskRunHTTPError(w, http.StatusServiceUnavailable, "TASK_RUN_RUNTIME_UNAVAILABLE", "task run runtime unavailable")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, taskRunDispatchMaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var request TaskRunDispatchRequest
	if err := decoder.Decode(&request); err != nil {
		writeTaskRunHTTPError(w, http.StatusBadRequest, "TASK_RUN_INVALID_JSON", "invalid task run dispatch body")
		return
	}
	if err := ensureTaskRunJSONEOF(decoder); err != nil {
		writeTaskRunHTTPError(w, http.StatusBadRequest, "TASK_RUN_INVALID_JSON", "task run dispatch body must contain one JSON object")
		return
	}

	result, err := dispatcher.Dispatch(r.Context(), request)
	if err != nil {
		status, code, message := taskRunDispatchHTTPError(err)
		writeTaskRunHTTPError(w, status, code, message)
		return
	}
	writeTaskRunJSON(w, http.StatusOK, result)
}

func (s *Server) configuredTaskRunDispatcher() *TaskRunDispatcher {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.taskRunDispatcher
}

func (s *Server) taskRunAuthorized(r *http.Request) bool {
	if s == nil || s.taskRunAuthService == nil || r == nil {
		return false
	}
	return s.taskRunAuthService.ValidateAuthorizationHeader(r.Header.Get("Authorization"))
}

func ensureTaskRunJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func taskRunDispatchHTTPError(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrTaskRunDispatchInvalid), errors.Is(err, ErrTaskRunEnvelopeInvalid):
		return http.StatusBadRequest, "TASK_RUN_DISPATCH_INVALID", "invalid task run dispatch"
	case errors.Is(err, ErrTaskRunLeaseExpired):
		return http.StatusConflict, "TASK_RUN_LEASE_EXPIRED", "task run lease expired"
	case errors.Is(err, ErrTaskRunLeaseRejected):
		return http.StatusConflict, "TASK_RUN_LEASE_REJECTED", "task run lease rejected"
	case errors.Is(err, ErrTaskRunRuntimeUnavailable):
		return http.StatusServiceUnavailable, "TASK_RUN_RUNTIME_UNAVAILABLE", "task run runtime unavailable"
	case errors.Is(err, ErrTaskRunCallbackInvalid):
		return http.StatusBadGateway, "TASK_RUN_CALLBACK_INVALID", "ACP returned an invalid task run snapshot"
	case errors.Is(err, ErrTaskRunCallbackWrite):
		return http.StatusBadGateway, "TASK_RUN_CALLBACK_WRITE_FAILED", "task run callback write failed"
	case errors.Is(err, ErrTaskRunExecution):
		return http.StatusBadGateway, "TASK_RUN_EXECUTION_FAILED", "task run execution failed"
	default:
		return http.StatusInternalServerError, "TASK_RUN_INTERNAL_ERROR", "internal task run error"
	}
}

func writeTaskRunHTTPError(w http.ResponseWriter, status int, code string, message string) {
	writeTaskRunJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func writeTaskRunJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("level=error component=task_run_dispatch event=response_write status=%d error=%q", status, fmt.Sprint(err))
	}
}
