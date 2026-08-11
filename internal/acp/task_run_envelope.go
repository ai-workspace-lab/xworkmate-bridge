package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	TaskRunID      string    `json:"taskRunId"`
	AccountID      string    `json:"accountId"`
	NamespaceID    string    `json:"namespaceId"`
	SessionID      string    `json:"sessionId"`
	ThreadID       string    `json:"threadId"`
	Fence          int64     `json:"fence"`
	LeaseToken     string    `json:"leaseToken"`
	LeaseExpiresAt time.Time `json:"leaseExpiresAt"`
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
	for key, expected := range map[string]string{
		"taskRunId":   e.TaskRunID,
		"accountId":   e.AccountID,
		"namespaceId": e.NamespaceID,
		"sessionId":   e.SessionID,
		"threadId":    e.ThreadID,
	} {
		if err := validateTaskRunStringIdentity(params, key, expected); err != nil {
			return nil, err
		}
	}
	if err := validateTaskRunFence(params, "fence", e.Fence); err != nil {
		return nil, err
	}
	for _, key := range []string{"leaseToken", "leaseExpiresAt"} {
		if _, exists := params[key]; exists {
			return nil, fmt.Errorf("%w: %s is reserved for the managed lease", ErrTaskRunEnvelopeInvalid, key)
		}
	}

	result := make(map[string]any, len(params)+6)
	for key, value := range params {
		result[key] = value
	}
	result["taskRunId"] = e.TaskRunID
	result["accountId"] = e.AccountID
	result["sessionId"] = e.SessionID
	result["threadId"] = e.ThreadID
	result["namespaceId"] = e.NamespaceID
	result["fence"] = e.Fence
	metadata := map[string]any{}
	if raw, exists := result["metadata"]; exists {
		existing, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: metadata must be an object", ErrTaskRunEnvelopeInvalid)
		}
		for key, value := range existing {
			metadata[key] = value
		}
	}
	for key, expected := range map[string]string{
		"xworkmateTaskRunId":   e.TaskRunID,
		"xworkmateAccountId":   e.AccountID,
		"xworkmateNamespaceId": e.NamespaceID,
	} {
		if err := validateTaskRunStringIdentity(metadata, key, expected); err != nil {
			return nil, err
		}
	}
	if err := validateTaskRunFence(metadata, "xworkmateFence", e.Fence); err != nil {
		return nil, err
	}
	if _, exists := metadata["xworkmateLeaseToken"]; exists {
		return nil, fmt.Errorf("%w: lease token must not cross the ACP boundary", ErrTaskRunEnvelopeInvalid)
	}
	metadata["xworkmateTaskRunId"] = e.TaskRunID
	metadata["xworkmateAccountId"] = e.AccountID
	metadata["xworkmateNamespaceId"] = e.NamespaceID
	metadata["xworkmateFence"] = e.Fence
	metadata["xworkmateLeaseExpiresAt"] = e.LeaseExpiresAt.UTC().Format(time.RFC3339Nano)
	result["metadata"] = metadata
	return result, nil
}

func validateTaskRunStringIdentity(values map[string]any, key string, expected string) error {
	value, exists := values[key]
	if !exists {
		return nil
	}
	actual, ok := value.(string)
	if !ok || strings.TrimSpace(actual) != expected {
		return fmt.Errorf("%w: %s does not match the managed task run", ErrTaskRunEnvelopeInvalid, key)
	}
	return nil
}

func validateTaskRunFence(values map[string]any, key string, expected int64) error {
	value, exists := values[key]
	if !exists {
		return nil
	}
	actual, ok := taskRunInt64(value)
	if !ok || actual != expected {
		return fmt.Errorf("%w: %s does not match the managed task run", ErrTaskRunEnvelopeInvalid, key)
	}
	return nil
}

func taskRunInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if math.Trunc(typed) != typed || typed > math.MaxInt64 || typed < math.MinInt64 {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}
