package dirextalkdomain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrAgentExecutionCompletionConflict = errors.New("agent execution completion receipt conflicts with an existing receipt")

// AgentExecutionCompletionReceipt is the minimal Product-owned receipt for
// one terminal Agent execution. Execution output, artifacts and cloud details
// remain Agent-owned and are deliberately absent from this record.
type AgentExecutionCompletionReceipt struct {
	EventID           string `json:"event_id"`
	ExecutionID       string `json:"execution_id"`
	RunID             string `json:"run_id"`
	ConversationID    string `json:"conversation_id"`
	TurnID            string `json:"turn_id"`
	TerminalState     string `json:"terminal_state"`
	CompletedAt       string `json:"completed_at"`
	PayloadDigest     string `json:"payload_digest"`
	OwnerID           string `json:"-"`
	AccountGeneration int64  `json:"-"`
}

// CanonicalAgentExecutionCompletionDigest binds exactly the seven public
// completion fields other than PayloadDigest. Keep the field order aligned
// with dirextalk-agent's CompletionDigest golden contract.
func CanonicalAgentExecutionCompletionDigest(r AgentExecutionCompletionReceipt) (string, error) {
	completedAt, err := canonicalCompletionTime(r.CompletedAt)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		EventID        string `json:"event_id"`
		ExecutionID    string `json:"execution_id"`
		RunID          string `json:"run_id"`
		ConversationID string `json:"conversation_id"`
		TurnID         string `json:"turn_id"`
		TerminalState  string `json:"terminal_state"`
		CompletedAt    string `json:"completed_at"`
	}{
		EventID: r.EventID, ExecutionID: r.ExecutionID, RunID: r.RunID,
		ConversationID: r.ConversationID, TurnID: r.TurnID,
		TerminalState: r.TerminalState,
		CompletedAt:   completedAt,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

// Validate enforces the independent domain boundary behind the private
// Product Capability callback. JSON Schema validation is not an authority for
// identity, canonical timestamps, terminal state, or digest binding.
func (r AgentExecutionCompletionReceipt) Validate() error {
	for name, value := range map[string]string{
		"event_id": r.EventID, "execution_id": r.ExecutionID, "run_id": r.RunID,
		"conversation_id": r.ConversationID, "turn_id": r.TurnID,
	} {
		if !canonicalNonNilUUID(value) {
			return fmt.Errorf("%s must be a canonical lowercase non-nil UUID", name)
		}
	}
	switch r.TerminalState {
	case "succeeded", "failed", "canceled":
	default:
		return errors.New("terminal_state is invalid")
	}
	completedAt, err := canonicalCompletionTime(r.CompletedAt)
	if err != nil {
		return err
	}
	if completedAt != r.CompletedAt {
		return errors.New("completed_at must be canonical UTC RFC3339Nano")
	}
	if len(r.PayloadDigest) != sha256.Size*2 || strings.ToLower(r.PayloadDigest) != r.PayloadDigest {
		return errors.New("payload_digest must be lowercase 64-hex")
	}
	if _, err := hex.DecodeString(r.PayloadDigest); err != nil {
		return errors.New("payload_digest must be lowercase 64-hex")
	}
	want, err := CanonicalAgentExecutionCompletionDigest(r)
	if err != nil || r.PayloadDigest != want {
		return errors.New("payload_digest does not match completion payload")
	}
	if strings.TrimSpace(r.OwnerID) == "" || r.OwnerID != strings.TrimSpace(r.OwnerID) {
		return errors.New("owner_id is required")
	}
	if r.AccountGeneration <= 0 {
		return errors.New("account_generation must be positive")
	}
	return nil
}

func canonicalNonNilUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func canonicalCompletionTime(value string) (string, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.IsZero() {
		return "", errors.New("completed_at must be canonical UTC RFC3339Nano")
	}
	return parsed.UTC().Format(time.RFC3339Nano), nil
}

func (r AgentExecutionCompletionReceipt) PublicPayload() map[string]any {
	return map[string]any{
		"event_id":        r.EventID,
		"execution_id":    r.ExecutionID,
		"run_id":          r.RunID,
		"conversation_id": r.ConversationID,
		"turn_id":         r.TurnID,
		"terminal_state":  r.TerminalState,
		"completed_at":    r.CompletedAt,
		"payload_digest":  r.PayloadDigest,
	}
}
