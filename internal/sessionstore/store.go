package sessionstore

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("task session not found")
	ErrConflict = errors.New("task session conflict")
)

type Namespace struct {
	NamespaceID string    `json:"namespaceId"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type TaskRun struct {
	ID            string     `json:"id"`
	State         string     `json:"state"`
	BridgeTaskRef string     `json:"bridgeTaskRef,omitempty"`
	Priority      int        `json:"priority"`
	NotBefore     *time.Time `json:"notBefore,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

type Snapshot struct {
	SessionID       string         `json:"sessionId"`
	NamespaceID     string         `json:"namespaceId"`
	Title           string         `json:"title,omitempty"`
	SnapshotVersion int64          `json:"snapshotVersion"`
	LastEventSeq    int64          `json:"lastEventSeq"`
	LifecycleState  string         `json:"lifecycleState"`
	Context         map[string]any `json:"context"`
	TaskRun         *TaskRun       `json:"taskRun,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
}

type Event struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"createdAt"`
}

type CreateSessionInput struct {
	Title string
}

type AppendMessageInput struct {
	ClientRequestID string
	Text            string
	Priority        int
	NotBefore       *time.Time
}

type AppendMessageResult struct {
	SessionID       string  `json:"sessionId"`
	NamespaceID     string  `json:"namespaceId"`
	SnapshotVersion int64   `json:"snapshotVersion"`
	Event           Event   `json:"event"`
	TaskRun         TaskRun `json:"taskRun"`
	Existing        bool    `json:"-"`
}

// ACPUpdate is deliberately narrower than an ACP request/result. It cannot
// represent artifact content, paths, manifests, or download URLs.
type ACPUpdate struct {
	SessionID    string
	ThreadID     string
	Method       string
	Text         string
	Provider     string
	Target       string
	TaskRunID    string
	TaskRunState string
	OccurredAt   time.Time
}

type Store interface {
	Close() error
	ListNamespaces(context.Context, string) ([]Namespace, error)
	CreateSession(context.Context, string, string, CreateSessionInput) (Snapshot, error)
	ListSessions(context.Context, string, string) ([]Snapshot, error)
	GetSession(context.Context, string, string) (Snapshot, error)
	ListEvents(context.Context, string, string, int64, int) ([]Event, int64, error)
	AppendMessage(context.Context, string, string, AppendMessageInput) (AppendMessageResult, error)
	RecordACPUpdate(context.Context, string, ACPUpdate) error
}
