// Package agentcoreturns owns the Message Server's small Core turn ledger.
// It deliberately stores no prompt, message body, key, or upstream secret.
package agentcoreturns

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrConflict                  = errors.New("agent core turn idempotency conflict")
	ErrNotFound                  = errors.New("agent core turn not found")
	ErrTerminal                  = errors.New("agent core turn is already terminal")
	ErrDigestRequired            = errors.New("agent core request digest is required")
	ErrDigestMismatch            = errors.New("agent core request digest mismatch")
	ErrReconciliationRequired    = errors.New("agent core turn requires original request reconciliation")
	ErrPersistenceReconciliation = errors.New("agent core ledger persistence requires reconciliation")
)

type Record struct {
	OwnerID, ClientTurnID, CoreTurnID, CoreProfileID, ConversationID, Status string
	RequestDigest                                                            []byte
	LastSequence, CoreRevision, ModelProfileRevision                         int64
	LastEventKind                                                            string
	TerminalCode, TerminalSummary                                            string
	CreatedAt, UpdatedAt                                                     time.Time
}

type Ledger struct {
	db     *sql.DB
	mu     sync.Mutex
	memory map[string]Record
}

func New(db *sql.DB) *Ledger { return &Ledger{db: db, memory: map[string]Record{}} }
func Digest(v any) []byte    { sum := sha256.Sum256([]byte(encode(v))); return sum[:] }
func encode(v any) string    { b, _ := json.Marshal(v); return string(b) }

// DigestParams hashes only immutable initial request parameters. Reconnect
// transport fields must never change the idempotency identity.
func DigestParams(params map[string]any) []byte {
	canonical := make(map[string]any, len(params))
	for key, value := range params {
		if key == "request_digest" || key == "after_seq" || key == "after_sequence" {
			continue
		}
		canonical[key] = value
	}
	return Digest(canonical)
}

func (l *Ledger) Reserve(ctx context.Context, rec Record) (Record, bool, error) {
	if l == nil || strings.TrimSpace(rec.OwnerID) == "" || strings.TrimSpace(rec.ClientTurnID) == "" {
		return Record{}, false, ErrConflict
	}
	if len(rec.RequestDigest) != sha256.Size {
		return Record{}, false, ErrConflict
	}
	if rec.Status == "" {
		rec.Status = "accepted"
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	rec.UpdatedAt = rec.CreatedAt
	if l.db == nil {
		return l.reserveMemory(rec)
	}
	var old Record
	err := l.db.QueryRowContext(ctx, `SELECT owner_id,client_turn_id,core_turn_id,core_profile_id,conversation_id,request_digest,status,last_sequence,core_revision,model_profile_revision,last_event_kind,terminal_code,terminal_summary,created_at,updated_at FROM p2p_agent_core_turns WHERE owner_id=$1 AND client_turn_id=$2`, rec.OwnerID, rec.ClientTurnID).Scan(&old.OwnerID, &old.ClientTurnID, &old.CoreTurnID, &old.CoreProfileID, &old.ConversationID, &old.RequestDigest, &old.Status, &old.LastSequence, &old.CoreRevision, &old.ModelProfileRevision, &old.LastEventKind, &old.TerminalCode, &old.TerminalSummary, &old.CreatedAt, &old.UpdatedAt)
	if err == nil {
		if string(old.RequestDigest) != string(rec.RequestDigest) {
			return Record{}, false, ErrConflict
		}
		return old, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, err
	}
	_, err = l.db.ExecContext(ctx, `INSERT INTO p2p_agent_core_turns(owner_id,client_turn_id,core_turn_id,core_profile_id,conversation_id,request_digest,status,last_sequence,core_revision,model_profile_revision,last_event_kind,terminal_code,terminal_summary,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, rec.OwnerID, rec.ClientTurnID, rec.CoreTurnID, rec.CoreProfileID, rec.ConversationID, rec.RequestDigest, rec.Status, rec.LastSequence, rec.CoreRevision, rec.ModelProfileRevision, rec.LastEventKind, rec.TerminalCode, rec.TerminalSummary, rec.CreatedAt, rec.UpdatedAt)
	if err != nil {
		// Another owner-session goroutine may have won the idempotency insert.
		// Re-read it so same-digest requests remain safe replays.
		if replay, readErr := l.Get(ctx, rec.OwnerID, rec.ClientTurnID); readErr == nil {
			if string(replay.RequestDigest) != string(rec.RequestDigest) {
				return Record{}, false, ErrConflict
			}
			return replay, true, nil
		}
		return Record{}, false, err
	}
	return rec, false, nil
}

func (l *Ledger) reserveMemory(rec Record) (Record, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := rec.OwnerID + "\x00" + rec.ClientTurnID
	if old, ok := l.memory[key]; ok {
		if string(old.RequestDigest) != string(rec.RequestDigest) {
			return Record{}, false, ErrConflict
		}
		return old, true, nil
	}
	l.memory[key] = clone(rec)
	return rec, false, nil
}
func (l *Ledger) Get(ctx context.Context, owner, client string) (Record, error) {
	if l.db == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		r, ok := l.memory[owner+"\x00"+client]
		if !ok {
			return Record{}, ErrNotFound
		}
		return clone(r), nil
	}
	var r Record
	err := l.db.QueryRowContext(ctx, `SELECT owner_id,client_turn_id,core_turn_id,core_profile_id,conversation_id,request_digest,status,last_sequence,core_revision,model_profile_revision,last_event_kind,terminal_code,terminal_summary,created_at,updated_at FROM p2p_agent_core_turns WHERE owner_id=$1 AND client_turn_id=$2`, owner, client).Scan(&r.OwnerID, &r.ClientTurnID, &r.CoreTurnID, &r.CoreProfileID, &r.ConversationID, &r.RequestDigest, &r.Status, &r.LastSequence, &r.CoreRevision, &r.ModelProfileRevision, &r.LastEventKind, &r.TerminalCode, &r.TerminalSummary, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	return r, err
}
func (l *Ledger) Update(ctx context.Context, owner, client string, coreTurn, conversation, status string, seq, coreRevision, modelProfileRevision int64, eventKind, terminalCode, terminalSummary string) error {
	if l.db == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		key := owner + "\x00" + client
		r, ok := l.memory[key]
		if !ok {
			return ErrNotFound
		}
		if seq < r.LastSequence || (seq == r.LastSequence && coreRevision <= r.CoreRevision) {
			return nil
		}
		if isTerminal(r.Status) && (!isTerminal(status) || terminalClass(r.Status) != terminalClass(status)) {
			return nil
		}
		if r.CoreTurnID == "" && coreTurn != "" {
			r.CoreTurnID = coreTurn
		}
		if r.ConversationID == "" && conversation != "" {
			r.ConversationID = conversation
		}
		if !isTerminal(r.Status) {
			r.Status = status
		}
		r.LastSequence = seq
		if coreRevision > r.CoreRevision {
			r.CoreRevision = coreRevision
		}
		if modelProfileRevision > r.ModelProfileRevision {
			r.ModelProfileRevision = modelProfileRevision
		}
		if eventKind != "" {
			r.LastEventKind = eventKind
		}
		if isTerminal(status) && terminalClass(r.Status) == terminalClass(status) {
			r.TerminalCode = terminalCode
			r.TerminalSummary = terminalSummary
		}
		r.UpdatedAt = time.Now().UTC()
		l.memory[key] = r
		return nil
	}
	_, err := l.db.ExecContext(ctx, `UPDATE p2p_agent_core_turns SET core_turn_id=CASE WHEN core_turn_id='' THEN COALESCE(NULLIF($3,''),core_turn_id) ELSE core_turn_id END,conversation_id=CASE WHEN conversation_id='' THEN COALESCE(NULLIF($4,''),conversation_id) ELSE conversation_id END,status=CASE WHEN status IN ('completed','done','succeeded','failed','canceled','uncertain') THEN status ELSE $5 END,last_sequence=$6,core_revision=GREATEST(core_revision,$7),model_profile_revision=GREATEST(model_profile_revision,$8),last_event_kind=CASE WHEN $9 <> '' THEN $9 ELSE last_event_kind END,terminal_code=CASE WHEN $5 IN ('completed','done','succeeded','failed','canceled','uncertain') AND (status NOT IN ('completed','done','succeeded','failed','canceled','uncertain') OR (status IN ('completed','done','succeeded') AND $5 IN ('completed','done','succeeded')) OR (status='failed' AND $5='failed') OR (status='canceled' AND $5='canceled') OR (status='uncertain' AND $5='uncertain')) THEN $10 ELSE terminal_code END,terminal_summary=CASE WHEN $5 IN ('completed','done','succeeded','failed','canceled','uncertain') AND (status NOT IN ('completed','done','succeeded','failed','canceled','uncertain') OR (status IN ('completed','done','succeeded') AND $5 IN ('completed','done','succeeded')) OR (status='failed' AND $5='failed') OR (status='canceled' AND $5='canceled') OR (status='uncertain' AND $5='uncertain')) THEN $11 ELSE terminal_summary END,updated_at=$12 WHERE owner_id=$1 AND client_turn_id=$2 AND ($6 > last_sequence OR ($6 = last_sequence AND $7 > core_revision)) AND (status NOT IN ('completed','done','succeeded','failed','canceled','uncertain') OR (status IN ('completed','done','succeeded') AND $5 IN ('completed','done','succeeded')) OR (status='failed' AND $5='failed') OR (status='canceled' AND $5='canceled') OR (status='uncertain' AND $5='uncertain'))`, owner, client, coreTurn, conversation, status, seq, coreRevision, modelProfileRevision, eventKind, terminalCode, terminalSummary, time.Now().UTC())
	return err
}

func isTerminal(status string) bool {
	return status == "completed" || status == "done" || status == "succeeded" || status == "failed" || status == "canceled" || status == "uncertain"
}
func terminalClass(status string) string {
	switch status {
	case "completed", "done", "succeeded":
		return "completed"
	default:
		return status
	}
}
func clone(r Record) Record  { r.RequestDigest = append([]byte(nil), r.RequestDigest...); return r }
func DigestHex(v any) string { return hex.EncodeToString(Digest(v)) }
