// Package agentturns owns durable Native Agent turn lifecycle and execution
// coordination. PostgreSQL-backed implementations remain in p2p/storage.
package agentturns

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

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
	bearerValue     = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
)

type Turn struct {
	OwnerID              string
	TurnID               string
	ConversationID       string
	Action               string
	ModelProfileID       string
	ModelProfileRevision int64
	CredentialVersion    int64
	Digest               [32]byte
	State                State
	Error                string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Candidate struct {
	OwnerID              string
	TurnID               string
	ConversationID       string
	Action               string
	ModelProfileID       string
	ModelProfileRevision int64
	CredentialVersion    int64
	Digest               [32]byte
}

type Reservation struct {
	Turn    Turn
	Created bool
}

type Event struct {
	OwnerID        string
	TurnID         string
	ConversationID string
	Seq            int64
	Kind           string
	Event          string
	Data           map[string]any
	CreatedAt      time.Time
}

type Store interface {
	ReserveAgentTurn(context.Context, Candidate) (Reservation, error)
	GetAgentTurn(context.Context, string, string) (Turn, bool, error)
	ListAgentTurns(context.Context, string, string, int) ([]Turn, error)
	ListAgentTurnEvents(context.Context, string, string, int64) ([]Event, error)
	MarkAgentTurnRunning(context.Context, string, string) (Turn, bool, error)
	AppendAgentTurnEvent(context.Context, string, string, string, string, map[string]any) (Event, error)
	FinishAgentTurn(context.Context, string, string, State, string, string, map[string]any, string) (Turn, Event, bool, error)
	StopAgentTurn(context.Context, string, string) (Turn, Event, bool, error)
	InterruptAgentTurns(context.Context) (int64, error)
}

type Request struct {
	OwnerID              string
	TurnID               string
	ConversationID       string
	Action               string
	ModelProfileID       string
	ModelProfileRevision int64
	CredentialVersion    int64
	Digest               [32]byte
	AfterSeq             int64
}

func (r Request) WithAfterSeq(after int64) Request {
	r.AfterSeq = after
	return r
}

func (r Request) Validate() error {
	if !validID.MatchString(strings.TrimSpace(r.OwnerID)) {
		return fmt.Errorf("owner_id is invalid")
	}
	if !validID.MatchString(strings.TrimSpace(r.TurnID)) {
		return fmt.Errorf("turn_id is invalid")
	}
	if !validID.MatchString(strings.TrimSpace(r.ConversationID)) {
		return fmt.Errorf("conversation_id is invalid")
	}
	if strings.TrimSpace(r.Action) == "" {
		return fmt.Errorf("action is required")
	}
	if r.AfterSeq < 0 {
		return fmt.Errorf("after_seq must be non-negative")
	}
	return nil
}

type RuntimeEvent struct {
	Event string
	Data  map[string]any
}

type StreamEvent struct {
	Kind           string
	Turn           Turn
	TurnID         string
	ConversationID string
	Seq            int64
	Event          string
	Data           map[string]any
}

type Runner func(context.Context, func(RuntimeEvent) error) error
type Emitter func(StreamEvent) error

func ValidID(value string) bool {
	return validID.MatchString(strings.TrimSpace(value))
}

func RequestDigest(action string, params map[string]any) ([32]byte, error) {
	canonical := map[string]any{
		"action": strings.TrimSpace(action),
		"params": secretFreeValue(params),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode turn request digest: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

func SanitizeData(data map[string]any) map[string]any {
	clean, _ := secretFreeValue(data).(map[string]any)
	if clean == nil {
		return map[string]any{}
	}
	return clean
}

func secretFreeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := make(map[string]any, len(keys))
		for _, key := range keys {
			if secretKey(key) || key == "after_seq" {
				continue
			}
			normalizedKey := strings.ToLower(strings.TrimSpace(key))
			if normalizedKey == "arguments" || normalizedKey == "raw_args" || normalizedKey == "output" {
				result[key] = secretFreeJSONText(typed[key])
			} else {
				result[key] = secretFreeValue(typed[key])
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for i := range typed {
			result[i] = secretFreeValue(typed[i])
		}
		return result
	case string:
		return bearerValue.ReplaceAllString(typed, "${1}[REDACTED]")
	default:
		return secretFreeConcreteValue(value)
	}
}

func secretFreeConcreteValue(value any) any {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return value
	}
	switch reflected.Kind() {
	case reflect.Map:
		if reflected.Type().Key().Kind() != reflect.String {
			return value
		}
		if reflected.IsNil() {
			return nil
		}
		keys := reflected.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		result := make(map[string]any, len(keys))
		for _, reflectedKey := range keys {
			key := reflectedKey.String()
			if secretKey(key) || key == "after_seq" {
				continue
			}
			item := reflected.MapIndex(reflectedKey).Interface()
			normalizedKey := strings.ToLower(strings.TrimSpace(key))
			if normalizedKey == "arguments" || normalizedKey == "raw_args" || normalizedKey == "output" {
				result[key] = secretFreeJSONText(item)
			} else {
				result[key] = secretFreeValue(item)
			}
		}
		return result
	case reflect.Slice:
		if reflected.IsNil() {
			return nil
		}
		if reflected.Type().Elem().Kind() == reflect.Uint8 {
			return value
		}
		fallthrough
	case reflect.Array:
		result := make([]any, reflected.Len())
		for i := 0; i < reflected.Len(); i++ {
			result[i] = secretFreeValue(reflected.Index(i).Interface())
		}
		return result
	default:
		return value
	}
}

func secretFreeJSONText(value any) any {
	text, ok := value.(string)
	if !ok {
		return secretFreeValue(value)
	}
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return bearerValue.ReplaceAllString(text, "${1}[REDACTED]")
	}
	var decoded any
	if json.Unmarshal([]byte(trimmed), &decoded) != nil {
		return bearerValue.ReplaceAllString(text, "${1}[REDACTED]")
	}
	encoded, err := json.Marshal(secretFreeValue(decoded))
	if err != nil {
		return text
	}
	return string(encoded)
}

func secretKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "authorization" || key == "cookie" || key == "headers" || key == "token" || key == "credential" || key == "credentials" {
		return true
	}
	for _, marker := range []string{"api_key", "access_token", "bearer", "password", "secret", "private_key"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
