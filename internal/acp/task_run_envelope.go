package acp

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrTaskRunEnvelopeInvalid = errors.New("invalid task run envelope")
	ErrTaskRunLeaseExpired    = errors.New("task run lease expired")
)

// TaskRunEnvelope is the signed/internal handoff from the durable scheduler
// to a bridge executor. Client-provided routing metadata is never authoritative
// once this envelope has been accepted.
type TaskRunEnvelope struct {
	TaskRunID      string
	AccountID      string
	NamespaceID    string
	SessionID      string
	ThreadID       string
	Fence          int64
	LeaseToken     string
	LeaseExpiresAt time.Time
}

func (e TaskRunEnvelope) Validate(now time.Time) error {
	if strings.TrimSpace(e.TaskRunID) == "" ||
		strings.TrimSpace(e.AccountID) == "" ||
		strings.TrimSpace(e.NamespaceID) == "" ||
		strings.TrimSpace(e.SessionID) == "" ||
		strings.TrimSpace(e.ThreadID) == "" ||
		e.ThreadID != e.SessionID ||
		e.Fence <= 0 ||
		strings.TrimSpace(e.LeaseToken) == "" ||
		e.LeaseExpiresAt.IsZero() {
		return fmt.Errorf("%w: missing or mismatched identity", ErrTaskRunEnvelopeInvalid)
	}
	if !now.Before(e.LeaseExpiresAt) {
		return ErrTaskRunLeaseExpired
	}
	return nil
}

func (e TaskRunEnvelope) EnrichParams(params map[string]any, now time.Time) (map[string]any, error) {
	if err := e.Validate(now); err != nil {
		return nil, err
	}
	result := make(map[string]any, len(params)+2)
	for key, value := range params {
		result[key] = value
	}
	if sessionID, ok := result["sessionId"].(string); ok && strings.TrimSpace(sessionID) != "" && sessionID != e.SessionID {
		return nil, fmt.Errorf("%w: sessionId does not match task run", ErrTaskRunEnvelopeInvalid)
	}
	if threadID, ok := result["threadId"].(string); ok && strings.TrimSpace(threadID) != "" && threadID != e.ThreadID {
		return nil, fmt.Errorf("%w: threadId does not match task run", ErrTaskRunEnvelopeInvalid)
	}
	result["sessionId"] = e.SessionID
	result["threadId"] = e.ThreadID
	result["namespaceId"] = e.NamespaceID
	metadata := map[string]any{}
	if existing, ok := result["metadata"].(map[string]any); ok {
		for key, value := range existing {
			metadata[key] = value
		}
	}
	metadata["xworkmateTaskRunId"] = e.TaskRunID
	metadata["xworkmateAccountId"] = e.AccountID
	metadata["xworkmateNamespaceId"] = e.NamespaceID
	metadata["xworkmateFence"] = e.Fence
	metadata["xworkmateLeaseToken"] = e.LeaseToken
	result["metadata"] = metadata
	return result, nil
}
