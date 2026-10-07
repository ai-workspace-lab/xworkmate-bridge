package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"xworkmate-bridge/internal/shared"
)

const defaultPermissionDecisionTimeout = 10 * time.Minute

// permissionBroker holds upstream permission prompts until the user answers
// them from the app. Any connection may answer (the app's task stream is a
// one-way SSE response), so pending prompts are server-wide and keyed by an
// unguessable request ID scoped to its session.
type permissionBroker struct {
	timeout time.Duration
	mu      sync.Mutex
	pending map[string]*pendingPermission
}

type pendingPermission struct {
	RequestID string
	SessionID string
	TurnID    string
	ToolCall  map[string]any
	Options   []any
	CreatedAt time.Time
	decision  chan map[string]any
}

func newPermissionBroker(timeout time.Duration) *permissionBroker {
	if timeout <= 0 {
		timeout = defaultPermissionDecisionTimeout
	}
	return &permissionBroker{timeout: timeout, pending: make(map[string]*pendingPermission)}
}

func newPermissionRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	return "perm-" + hex.EncodeToString(buf)
}

func (p *pendingPermission) snapshot() map[string]any {
	return map[string]any{
		"requestId": p.RequestID,
		"sessionId": p.SessionID,
		"turnId":    p.TurnID,
		"toolCall":  p.ToolCall,
		"options":   p.Options,
		"createdAt": p.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// await registers a prompt, publishes it through onRequested and blocks until
// the user decides, the run is cancelled or the timeout elapses. Anything but
// an explicit selection returns the ACP "cancelled" outcome (deny).
func (b *permissionBroker) await(
	ctx context.Context,
	sessionID string,
	turnID string,
	upstreamParams map[string]any,
	onRequested func(map[string]any),
	onResolved func(map[string]any),
) map[string]any {
	pending := &pendingPermission{
		RequestID: newPermissionRequestID(),
		SessionID: sessionID,
		TurnID:    turnID,
		ToolCall:  shared.AsMap(upstreamParams["toolCall"]),
		Options:   anyList(upstreamParams["options"]),
		CreatedAt: time.Now(),
		decision:  make(chan map[string]any, 1),
	}
	b.mu.Lock()
	b.pending[pending.RequestID] = pending
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, pending.RequestID)
		b.mu.Unlock()
	}()
	onRequested(pending.snapshot())

	outcome := map[string]any{"outcome": "cancelled"}
	reason := "cancelled"
	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case decided := <-pending.decision:
		outcome = decided
		reason = "user"
	case <-ctx.Done():
		reason = "run_cancelled"
	case <-timer.C:
		reason = "timeout"
	}
	resolved := pending.snapshot()
	resolved["outcome"] = outcome
	resolved["reason"] = reason
	onResolved(resolved)
	return map[string]any{"outcome": outcome}
}

// respond applies the user's decision. optionID must be one of the options
// the upstream offered; an empty optionID with cancel=true denies.
func (b *permissionBroker) respond(sessionID, requestID, optionID string, cancel bool) error {
	b.mu.Lock()
	pending, ok := b.pending[strings.TrimSpace(requestID)]
	b.mu.Unlock()
	if !ok {
		return errors.New("PERMISSION_REQUEST_NOT_FOUND")
	}
	if strings.TrimSpace(sessionID) != pending.SessionID {
		return errors.New("PERMISSION_SESSION_MISMATCH")
	}
	var outcome map[string]any
	switch {
	case cancel:
		outcome = map[string]any{"outcome": "cancelled"}
	case permissionOptionOffered(pending.Options, optionID):
		outcome = map[string]any{"outcome": "selected", "optionId": strings.TrimSpace(optionID)}
	default:
		return errors.New("PERMISSION_OPTION_NOT_OFFERED")
	}
	select {
	case pending.decision <- outcome:
		return nil
	default:
		return errors.New("PERMISSION_ALREADY_DECIDED")
	}
}

// cancelSession denies every pending prompt of a session.
func (b *permissionBroker) cancelSession(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, pending := range b.pending {
		if pending.SessionID != sessionID {
			continue
		}
		select {
		case pending.decision <- map[string]any{"outcome": "cancelled"}:
		default:
		}
	}
}

func (b *permissionBroker) list(sessionID string) []any {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := make([]any, 0)
	for _, pending := range b.pending {
		if sessionID == "" || pending.SessionID == sessionID {
			items = append(items, pending.snapshot())
		}
	}
	return items
}

func permissionOptionOffered(options []any, optionID string) bool {
	optionID = strings.TrimSpace(optionID)
	if optionID == "" {
		return false
	}
	for _, raw := range options {
		if strings.TrimSpace(shared.StringArg(shared.AsMap(raw), "optionId", "")) == optionID {
			return true
		}
	}
	return false
}

// permissionRelay travels in the run context of role-routed tasks. Its
// presence switches upstream permission prompts from the legacy auto-approve
// path to the user decision path.
type permissionRelay struct {
	broker    *permissionBroker
	sessionID string
	turnID    string
	publish   func(phase string, payload map[string]any)
}

type permissionRelayKey struct{}

func withPermissionRelay(ctx context.Context, relay *permissionRelay) context.Context {
	return context.WithValue(ctx, permissionRelayKey{}, relay)
}

func permissionRelayFrom(ctx context.Context) *permissionRelay {
	relay, _ := ctx.Value(permissionRelayKey{}).(*permissionRelay)
	return relay
}

// answerExternalPermissionRequest replies to an upstream permission prompt.
// Role-routed runs wait for the user's decision; other runs keep the legacy
// auto-approve reply (see docs/architecture/role-routing-engineer-loop.md,
// "Compatibility scope", for owner and exit criteria).
func answerExternalPermissionRequest(ctx context.Context, conn *websocket.Conn, request map[string]any) error {
	if conn == nil || request == nil || request["id"] == nil {
		return nil
	}
	relay := permissionRelayFrom(ctx)
	if relay == nil {
		return writeExternalPermissionApproval(conn, request)
	}
	result := relay.broker.await(
		ctx,
		relay.sessionID,
		relay.turnID,
		shared.AsMap(request["params"]),
		func(payload map[string]any) { relay.publish("permission_requested", payload) },
		func(payload map[string]any) { relay.publish("permission_resolved", payload) },
	)
	if ctx.Err() != nil {
		log.Printf("[permission] session=%s run ended before decision was sent upstream", relay.sessionID)
	}
	return conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": result})
}
