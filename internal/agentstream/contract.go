// Package agentstream contains the transport-neutral Native Agent stream
// contract. Runtime execution and durable turn storage live in dirextalk-agent;
// message-server keeps only these DTOs at its realtime/action boundary.
package agentstream

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// Event is one Native Agent runner stream event.
type Event struct {
	Event string
	// Seq is the positive remote-operation cursor for a durable stream. It is
	// zero for non-durable streams and for malformed upstream cursor values.
	Seq  int64
	Data map[string]any
}

// State is the remote durable-turn state projected on the websocket facade.
type State string

const (
	StateAccepted    State = "accepted"
	StateRunning     State = "running"
	StateSucceeded   State = "succeeded"
	StateFailed      State = "failed"
	StateStopped     State = "stopped"
	StateInterrupted State = "interrupted"
)

func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateStopped, StateInterrupted:
		return true
	default:
		return false
	}
}

const (
	EventAccepted = "accepted"
	EventRuntime  = "runtime"
	EventError    = "error"
)

var (
	ErrTurnIDReused = errors.New("M_TURN_ID_REUSED")
	ErrTurnNotFound = errors.New("M_TURN_NOT_FOUND")
	validID         = regexp.MustCompile(`^[A-Za-z0-9._:@!/-]{1,256}$`)
)

// Turn is the redacted remote-turn projection used by the public websocket
// facade. Digest and credential fields are retained for wire compatibility but
// are never populated by message-server's external runner.
type Turn struct {
	OwnerID              string
	TurnID               string
	IdempotencyKey       string
	ConversationID       string
	Action               string
	ModelProfileID       string
	ModelProfileRevision int64
	CredentialVersion    int64
	Digest               [32]byte
	State                State
	Revision             int64
	Error                string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// EventRecord is one persisted remote-turn event projected by the external
// runner. It is intentionally separate from Event, which is the simple stream
// callback DTO.
type EventRecord struct {
	OwnerID        string
	TurnID         string
	ConversationID string
	Seq            int64
	Kind           string
	Event          string
	Data           map[string]any
	CreatedAt      time.Time
}

// StreamEvent is the callback DTO consumed by the realtime websocket facade.
type StreamEvent struct {
	Kind           string
	Turn           Turn
	TurnID         string
	IdempotencyKey string
	ConversationID string
	Revision       int64
	Seq            int64
	Event          string
	Data           map[string]any
}

// RuntimeEvent is retained as a neutral callback shape for adapters that
// translate an external operation stream into StreamEvent values.
type RuntimeEvent struct {
	Event string
	Data  map[string]any
}

// ConversationID returns the canonical conversation key accepted by the
// public stream facade. The external runner validates ownership and format;
// this helper only provides deterministic alias selection for error frames.
func ConversationID(params map[string]any) string {
	for _, key := range []string{"conversation_id", "thread_id", "room_id", "memory_key"} {
		if value, ok := params[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return "default"
}

// ValidID reports whether an owner/turn/conversation identifier is safe for
// the external durable stream boundary.
func ValidID(value string) bool {
	return validID.MatchString(strings.TrimSpace(value))
}
