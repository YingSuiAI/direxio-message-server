package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// AgentConfirmationDDL is migration text for the owner-scoped confirmation
// ledger. It is intentionally exported so migration registration can remain
// under the existing storage migration owner.
const AgentConfirmationDDL = `
CREATE TABLE IF NOT EXISTS agent_confirmations (
 confirmation_id uuid PRIMARY KEY, owner_id text NOT NULL, operation_domain text NOT NULL,
	 target_id text NOT NULL, target_revision bigint NOT NULL, binding_digest text NOT NULL, binding_json jsonb NOT NULL DEFAULT '{}'::jsonb,
 task_id uuid NOT NULL, state text NOT NULL CHECK(state IN ('pending','confirmed','consumed','rejected','expired')),
 revision bigint NOT NULL DEFAULT 1, expires_at timestamptz NOT NULL, reservation_json jsonb,
 terminal_reason text NOT NULL DEFAULT '', created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS agent_confirmations_owner_confirmation_idx ON agent_confirmations(owner_id,confirmation_id);
CREATE UNIQUE INDEX IF NOT EXISTS agent_confirmations_live_target_idx ON agent_confirmations(owner_id,operation_domain,target_id) WHERE state IN ('pending','confirmed') OR (state='consumed' AND reservation_json IS NOT NULL);
CREATE TABLE IF NOT EXISTS agent_confirmation_replays (
 owner_id text NOT NULL, operation text NOT NULL, idempotency_key uuid NOT NULL,
 request_digest text NOT NULL, response_json jsonb NOT NULL, created_at timestamptz NOT NULL,
 PRIMARY KEY(owner_id,operation,idempotency_key)
);
CREATE INDEX IF NOT EXISTS agent_confirmations_expiry_idx ON agent_confirmations(state,expires_at);`

type DatabaseConfirmationStore struct{ db *sql.DB }

const maxOverdueSweepCandidates = 64

func NewDatabaseConfirmationStore(db *sql.DB) *DatabaseConfirmationStore {
	return &DatabaseConfirmationStore{db: db}
}

type confirmationRowScanner interface {
	Scan(...any) error
}

func scanConfirmation(row confirmationRowScanner) (confirmation.Confirmation, error) {
	var v confirmation.Confirmation
	var state string
	var raw []byte
	err := row.Scan(
		&v.ID, &v.OwnerID, &v.Binding.OperationDomain, &v.Binding.TargetID,
		&v.Binding.TargetRevision, &v.Binding.Digest, &raw, &v.TaskID, &state,
		&v.Revision, &v.CreatedAt, &v.UpdatedAt, &v.ExpiresAt, &v.TerminalReason,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return v, confirmation.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	v.State = confirmation.State(state)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &v.Binding); err != nil {
			return confirmation.Confirmation{}, err
		}
	}
	v.ConfirmationID = v.ID
	return v, nil
}

func getConfirmationTx(ctx context.Context, tx *sql.Tx, owner, id string) (confirmation.Confirmation, error) {
	return scanConfirmation(tx.QueryRowContext(ctx, `SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations WHERE ($1='' OR owner_id=$1) AND confirmation_id=$2`, owner, id))
}

func getConfirmationForUpdateTx(ctx context.Context, tx *sql.Tx, owner, id string) (confirmation.Confirmation, error) {
	return scanConfirmation(tx.QueryRowContext(ctx, `SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations WHERE ($1='' OR owner_id=$1) AND confirmation_id=$2 FOR UPDATE`, owner, id))
}

type confirmationTaskRow struct {
	OwnerID      string
	Status       string
	Attempt      int
	LeaseEpoch   int64
	Revision     int64
	LeaseExpires sql.NullTime
}

func confirmationIdentityTx(ctx context.Context, tx *sql.Tx, owner, id string) (string, string, error) {
	var resolvedOwner, taskID string
	err := tx.QueryRowContext(ctx, `SELECT owner_id,task_id::text FROM agent_confirmations WHERE confirmation_id=$1 AND ($2='' OR owner_id=$2)`, id, owner).Scan(&resolvedOwner, &taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", confirmation.ErrNotFound
	}
	return resolvedOwner, taskID, err
}

func lockConfirmationTaskTx(ctx context.Context, tx *sql.Tx, owner, taskID string) (confirmationTaskRow, error) {
	var row confirmationTaskRow
	err := tx.QueryRowContext(ctx, `SELECT owner_id,status,attempt,lease_epoch,revision,lease_expires_at FROM agent_tasks WHERE task_id=$1 AND ($2='' OR owner_id=$2) FOR UPDATE`, taskID, owner).Scan(
		&row.OwnerID, &row.Status, &row.Attempt, &row.LeaseEpoch, &row.Revision, &row.LeaseExpires,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return confirmationTaskRow{}, confirmation.ErrNotFound
	}
	return row, err
}

func commandConfirmationID(id, confirmationID string) string {
	if value := strings.TrimSpace(confirmationID); value != "" {
		return value
	}
	return strings.TrimSpace(id)
}

func validConfirmationUUID(value string) bool {
	value = strings.TrimSpace(value)
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func lockConfirmationReplayTx(ctx context.Context, tx *sql.Tx, owner, operation, key string) error {
	key = strings.TrimSpace(key)
	if strings.TrimSpace(owner) == "" || !validConfirmationUUID(key) {
		return confirmation.ErrInvalid
	}
	lockKey := canonicalAdvisoryLockIdentity("agent-confirmation", strings.TrimSpace(owner), operation, key)
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey)
	return err
}

func readConfirmationReplayTx(ctx context.Context, tx *sql.Tx, owner, operation, key string, digest confirmation.Digest) (confirmation.Confirmation, bool, error) {
	var storedDigest string
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM agent_confirmation_replays WHERE owner_id=$1 AND operation=$2 AND idempotency_key=$3`, owner, operation, key).Scan(&storedDigest, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return confirmation.Confirmation{}, false, nil
	}
	if err != nil {
		return confirmation.Confirmation{}, false, err
	}
	if storedDigest != string(digest) {
		return confirmation.Confirmation{}, true, confirmation.ErrIdempotencyConflict
	}
	var terminal struct {
		Confirmation *confirmation.Confirmation `json:"confirmation"`
		Error        string                     `json:"error,omitempty"`
	}
	if json.Unmarshal(raw, &terminal) == nil && terminal.Confirmation != nil {
		switch terminal.Error {
		case "expired":
			return *terminal.Confirmation, true, confirmation.ErrExpired
		case "":
			return *terminal.Confirmation, true, nil
		}
		return confirmation.Confirmation{}, true, confirmation.ErrConflict
	}
	var out confirmation.Confirmation
	if json.Unmarshal(raw, &out) != nil {
		return confirmation.Confirmation{}, true, confirmation.ErrConflict
	}
	return out, true, nil
}

func saveConfirmationReplayTx(ctx context.Context, tx *sql.Tx, owner, operation, key string, digest confirmation.Digest, value confirmation.Confirmation, at time.Time) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_confirmation_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,$5,$6)`, owner, operation, key, string(digest), raw, at.UTC())
	return err
}

func saveConfirmationReplayTerminalTx(ctx context.Context, tx *sql.Tx, owner, operation, key string, digest confirmation.Digest, value confirmation.Confirmation, terminalError string, at time.Time) error {
	raw, err := json.Marshal(struct {
		Confirmation confirmation.Confirmation `json:"confirmation"`
		Error        string                    `json:"error"`
	}{Confirmation: value, Error: terminalError})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_confirmation_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,$5,$6)`, owner, operation, key, string(digest), raw, at.UTC())
	return err
}

func replayConfirmationMutationTx(ctx context.Context, tx *sql.Tx, owner, operation, key string, digest confirmation.Digest, allowEmpty bool) (confirmation.Confirmation, bool, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" && allowEmpty {
		return confirmation.Confirmation{}, false, false, nil
	}
	if !validConfirmationUUID(key) {
		return confirmation.Confirmation{}, false, false, confirmation.ErrInvalid
	}
	if err := lockConfirmationReplayTx(ctx, tx, owner, operation, key); err != nil {
		return confirmation.Confirmation{}, false, false, err
	}
	replay, found, err := readConfirmationReplayTx(ctx, tx, owner, operation, key, digest)
	return replay, found, true, err
}
func (s *DatabaseConfirmationStore) Request(ctx context.Context, c confirmation.RequestCommand) (confirmation.Confirmation, error) {
	b, err := c.Binding.Normalize()
	if err != nil {
		return confirmation.Confirmation{}, confirmation.ErrInvalid
	}
	c.OwnerID = strings.TrimSpace(c.OwnerID)
	c.TaskID = strings.TrimSpace(c.TaskID)
	c.IdempotencyKey = strings.TrimSpace(c.IdempotencyKey)
	c.Binding = b
	c.RequestDigest = confirmation.RequestDigestForRequest(c)
	if c.OwnerID == "" || !validConfirmationUUID(c.TaskID) || !validConfirmationUUID(c.IdempotencyKey) {
		return confirmation.Confirmation{}, confirmation.ErrInvalid
	}
	raw, _ := json.Marshal(b)
	at := c.At.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	expiresAt := c.ExpiresAt.UTC()
	if c.ExpiresAt.IsZero() || !expiresAt.After(at) {
		return confirmation.Confirmation{}, confirmation.ErrInvalid
	}
	// Release an expired live target before the new insert. This is a bounded
	// owner/domain/target sweep; each candidate uses ExpireAt so task,
	// workload and deployment projections transition atomically.
	if err = s.expireOverdue(ctx, c.OwnerID, string(b.OperationDomain), b.TargetID, at, 1); err != nil {
		return confirmation.Confirmation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return confirmation.Confirmation{}, err
	}
	defer tx.Rollback()
	if err = lockConfirmationReplayTx(ctx, tx, c.OwnerID, "request", c.IdempotencyKey); err != nil {
		return confirmation.Confirmation{}, err
	}
	if replay, found, replayErr := readConfirmationReplayTx(ctx, tx, c.OwnerID, "request", c.IdempotencyKey, c.RequestDigest); found || replayErr != nil {
		return replay, replayErr
	}
	// Lock the task and reject approvals for work that is no longer waiting.
	taskRow, err := lockConfirmationTaskTx(ctx, tx, c.OwnerID, c.TaskID)
	if err != nil {
		return confirmation.Confirmation{}, err
	}
	if taskRow.Status != "waiting_user" {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	id := uuid.New()
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9,$10,$10)`, id, c.OwnerID, b.OperationDomain, b.TargetID, b.TargetRevision, b.Digest, raw, c.TaskID, expiresAt, at); err != nil {
		return confirmation.Confirmation{}, mapConfirmationError(err)
	}
	out := confirmation.Confirmation{ID: id.String(), ConfirmationID: id.String(), OwnerID: c.OwnerID, Binding: b, TaskID: c.TaskID, State: confirmation.StatePending, Revision: 1, ExpiresAt: expiresAt, CreatedAt: at, UpdatedAt: at}
	if err = saveConfirmationReplayTx(ctx, tx, c.OwnerID, "request", c.IdempotencyKey, c.RequestDigest, out, at); err != nil {
		return confirmation.Confirmation{}, err
	}
	if err = tx.Commit(); err != nil {
		return confirmation.Confirmation{}, err
	}
	return out, nil
}
func (s *DatabaseConfirmationStore) get(ctx context.Context, owner, id string) (confirmation.Confirmation, error) {
	return scanConfirmation(s.db.QueryRowContext(ctx, `SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations WHERE ($1='' OR owner_id=$1) AND confirmation_id=$2`, owner, id))
}
func (s *DatabaseConfirmationStore) Get(ctx context.Context, id string) (confirmation.Confirmation, error) {
	// The legacy unscoped read cannot safely infer an owner for a mutation.
	// Public owner-scoped callers should use GetForOwner below.
	return s.get(ctx, "", id)
}

// GetForOwner returns an owner-fenced confirmation and lazily terminalizes an
// overdue active card through ExpireAt's atomic task/workload transition.
func (s *DatabaseConfirmationStore) GetForOwner(ctx context.Context, owner, id string) (confirmation.Confirmation, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || !validConfirmationUUID(id) {
		return confirmation.Confirmation{}, confirmation.ErrInvalid
	}
	value, err := s.get(ctx, owner, id)
	if err != nil {
		return confirmation.Confirmation{}, err
	}
	now := time.Now().UTC()
	if (value.State == confirmation.StatePending || value.State == confirmation.StateConfirmed) && !value.ExpiresAt.After(now) {
		if err = s.ExpireAt(ctx, value.OwnerID, value.ID, now); err != nil {
			return confirmation.Confirmation{}, err
		}
		return s.get(ctx, owner, id)
	}
	return value, nil
}
func (s *DatabaseConfirmationStore) Confirm(ctx context.Context, c confirmation.ConfirmCommand) (confirmation.Confirmation, error) {
	c.OwnerID = strings.TrimSpace(c.OwnerID)
	c.ConfirmationID = commandConfirmationID(c.ID, c.ConfirmationID)
	c.ID = c.ConfirmationID
	c.IdempotencyKey = strings.TrimSpace(c.IdempotencyKey)
	c.RequestDigest = confirmation.RequestDigestForConfirm(c)
	binding, bindingErr := c.Binding.Normalize()
	if c.OwnerID == "" || !validConfirmationUUID(c.ID) || c.ExpectedRevision < 1 || bindingErr != nil {
		return confirmation.Confirmation{}, confirmation.ErrInvalid
	}
	c.Binding = binding
	at := c.At.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	defer tx.Rollback()
	replay, found, replayEnabled, e := replayConfirmationMutationTx(ctx, tx, c.OwnerID, "confirm", c.IdempotencyKey, c.RequestDigest, true)
	if found || e != nil {
		return replay, e
	}
	if e = lockAWSPreProviderByConfirmationTx(ctx, tx, c.OwnerID, c.ID); e != nil {
		return confirmation.Confirmation{}, e
	}
	_, taskID, e := confirmationIdentityTx(ctx, tx, c.OwnerID, c.ID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	taskRow, e := lockConfirmationTaskTx(ctx, tx, c.OwnerID, taskID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	stored, e := getConfirmationForUpdateTx(ctx, tx, c.OwnerID, c.ID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if stored.TaskID != taskID {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if stored.Revision != c.ExpectedRevision {
		return confirmation.Confirmation{}, confirmation.ErrRevisionConflict
	}
	if stored.State != confirmation.StatePending || stored.Binding.Digest != c.Binding.Digest || !stored.Binding.Equal(c.Binding) {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if !stored.ExpiresAt.After(at) {
		if e = expireConfirmationAndTaskTx(ctx, tx, stored, taskRow, confirmation.ReasonExpired, at); e != nil {
			return confirmation.Confirmation{}, e
		}
		var expired confirmation.Confirmation
		expired, e = getConfirmationTx(ctx, tx, c.OwnerID, c.ID)
		if e != nil {
			return confirmation.Confirmation{}, e
		}
		if replayEnabled {
			if e = saveConfirmationReplayTerminalTx(ctx, tx, c.OwnerID, "confirm", c.IdempotencyKey, c.RequestDigest, expired, "expired", at); e != nil {
				return confirmation.Confirmation{}, e
			}
		}
		if e = tx.Commit(); e != nil {
			return confirmation.Confirmation{}, e
		}
		return expired, confirmation.ErrExpired
	}
	if taskRow.Status != "waiting_user" {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	result, e := tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='confirmed',revision=revision+1,updated_at=$1 WHERE confirmation_id=$2 AND owner_id=$3 AND state='pending' AND revision=$4`, at, c.ID, c.OwnerID, c.ExpectedRevision)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return confirmation.Confirmation{}, confirmation.ErrRevisionConflict
	}
	result, e = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='queued',available_at=$1,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$1 WHERE task_id=$2 AND owner_id=$3 AND status='waiting_user'`, at, taskID, c.OwnerID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'confirmation_confirmed','queued','{}',$1 FROM agent_tasks WHERE task_id=$2 AND owner_id=$3`, at, taskID, c.OwnerID); e != nil {
		return confirmation.Confirmation{}, e
	}
	out, e := getConfirmationTx(ctx, tx, c.OwnerID, c.ID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if replayEnabled {
		if e = saveConfirmationReplayTx(ctx, tx, c.OwnerID, "confirm", c.IdempotencyKey, c.RequestDigest, out, at); e != nil {
			return confirmation.Confirmation{}, e
		}
	}
	if e = tx.Commit(); e != nil {
		return confirmation.Confirmation{}, e
	}
	return out, nil
}
func (s *DatabaseConfirmationStore) Reject(ctx context.Context, c confirmation.RejectCommand) (confirmation.Confirmation, error) {
	c.OwnerID = strings.TrimSpace(c.OwnerID)
	c.ConfirmationID = commandConfirmationID(c.ID, c.ConfirmationID)
	c.ID = c.ConfirmationID
	c.IdempotencyKey = strings.TrimSpace(c.IdempotencyKey)
	c.Reason = strings.TrimSpace(c.Reason)
	c.Note = strings.TrimSpace(c.Note)
	c.RequestDigest = confirmation.RequestDigestForReject(c)
	if c.OwnerID == "" || !validConfirmationUUID(c.ID) || !validConfirmationUUID(c.IdempotencyKey) || c.ExpectedRevision < 1 || len(c.Reason) > 256 || len(c.Note) > 256 || strings.ContainsAny(c.Reason+c.Note, "\r\n") {
		return confirmation.Confirmation{}, confirmation.ErrInvalid
	}
	at := c.At.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	defer tx.Rollback()
	replay, found, _, e := replayConfirmationMutationTx(ctx, tx, c.OwnerID, "reject", c.IdempotencyKey, c.RequestDigest, false)
	if found || e != nil {
		return replay, e
	}
	if e = lockAWSPreProviderByConfirmationTx(ctx, tx, c.OwnerID, c.ID); e != nil {
		return confirmation.Confirmation{}, e
	}
	_, taskID, e := confirmationIdentityTx(ctx, tx, c.OwnerID, c.ID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	taskRow, e := lockConfirmationTaskTx(ctx, tx, c.OwnerID, taskID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	stored, e := getConfirmationForUpdateTx(ctx, tx, c.OwnerID, c.ID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if stored.TaskID != taskID {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if stored.Revision != c.ExpectedRevision {
		return confirmation.Confirmation{}, confirmation.ErrRevisionConflict
	}
	if stored.State != confirmation.StatePending && !(stored.State == confirmation.StateConfirmed && taskRow.Status == "queued") {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if !stored.ExpiresAt.After(at) {
		if e = expireConfirmationAndTaskTx(ctx, tx, stored, taskRow, confirmation.ReasonExpired, at); e != nil {
			return confirmation.Confirmation{}, e
		}
		var expired confirmation.Confirmation
		expired, e = getConfirmationTx(ctx, tx, c.OwnerID, c.ID)
		if e != nil {
			return confirmation.Confirmation{}, e
		}
		if e = saveConfirmationReplayTerminalTx(ctx, tx, c.OwnerID, "reject", c.IdempotencyKey, c.RequestDigest, expired, "expired", at); e != nil {
			return confirmation.Confirmation{}, e
		}
		if e = tx.Commit(); e != nil {
			return confirmation.Confirmation{}, e
		}
		return expired, confirmation.ErrExpired
	}
	if taskRow.Status != "waiting_user" && taskRow.Status != "queued" {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if e = terminalizeAWSPreProviderChangeTx(ctx, tx, stored, taskRow, confirmation.ReasonUserRejected, at); e != nil {
		return confirmation.Confirmation{}, e
	}
	result, e := tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='rejected',terminal_reason=$1,revision=revision+1,updated_at=$2 WHERE confirmation_id=$3 AND owner_id=$4 AND state IN ('pending','confirmed') AND revision=$5`, c.Reason, at, c.ID, c.OwnerID, c.ExpectedRevision)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return confirmation.Confirmation{}, confirmation.ErrRevisionConflict
	}
	result, e = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='canceled',failure_code='user_rejected',failure_summary=$1,lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$2 WHERE task_id=$3 AND owner_id=$4 AND status IN ('waiting_user','queued')`, c.Reason, at, taskID, c.OwnerID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if e = terminalizeConfirmationsTx(ctx, tx, taskID, confirmation.ReasonUserRejected, at); e != nil {
		return confirmation.Confirmation{}, e
	}
	if strings.HasPrefix(stored.Binding.OperationDomain, "workload:") {
		if e = terminalizeWorkloadOperationTx(ctx, tx, stored, "rejected", "user_rejected", c.Reason, at); e != nil {
			return confirmation.Confirmation{}, e
		}
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'confirmation_rejected','canceled',jsonb_build_object('reason',$2::text),$3 FROM agent_tasks WHERE task_id=$1 AND owner_id=$4`, taskID, c.Reason, at, c.OwnerID); e != nil {
		return confirmation.Confirmation{}, e
	}
	out, e := getConfirmationTx(ctx, tx, c.OwnerID, c.ID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if e = saveConfirmationReplayTx(ctx, tx, c.OwnerID, "reject", c.IdempotencyKey, c.RequestDigest, out, at); e != nil {
		return confirmation.Confirmation{}, e
	}
	if e = tx.Commit(); e != nil {
		return confirmation.Confirmation{}, e
	}
	return out, nil
}
func (s *DatabaseConfirmationStore) Consume(ctx context.Context, c confirmation.ConsumeCommand) (confirmation.Confirmation, error) {
	c.OwnerID = strings.TrimSpace(c.OwnerID)
	c.ConfirmationID = commandConfirmationID(c.ID, c.ConfirmationID)
	c.ID = c.ConfirmationID
	c.TaskID = strings.TrimSpace(c.TaskID)
	c.IdempotencyKey = strings.TrimSpace(c.IdempotencyKey)
	c.RequestDigest = confirmation.RequestDigestForConsume(c)
	binding, bindingErr := c.Binding.Normalize()
	if c.OwnerID == "" || !validConfirmationUUID(c.ID) || !validConfirmationUUID(c.TaskID) || !validConfirmationUUID(c.IdempotencyKey) ||
		c.Attempt == 0 || c.LeaseEpoch == 0 || c.ExpectedRevision < 1 || c.ExpectedTaskRevision < 1 || bindingErr != nil {
		return confirmation.Confirmation{}, confirmation.ErrInvalid
	}
	c.Binding = binding
	at := c.At.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	defer tx.Rollback()
	replay, found, _, e := replayConfirmationMutationTx(ctx, tx, c.OwnerID, "consume", c.IdempotencyKey, c.RequestDigest, false)
	if found || e != nil {
		return replay, e
	}
	taskRow, e := lockConfirmationTaskTx(ctx, tx, c.OwnerID, c.TaskID)
	if e != nil {
		return confirmation.Confirmation{}, confirmation.ErrTaskFenceConflict
	}
	if taskRow.Status != "running" || taskRow.Attempt != int(c.Attempt) || taskRow.LeaseEpoch != int64(c.LeaseEpoch) ||
		taskRow.Revision != c.ExpectedTaskRevision || !taskRow.LeaseExpires.Valid || !taskRow.LeaseExpires.Time.After(at) {
		return confirmation.Confirmation{}, confirmation.ErrTaskFenceConflict
	}
	stored, e := getConfirmationForUpdateTx(ctx, tx, c.OwnerID, c.ID)
	if e != nil {
		if errors.Is(e, confirmation.ErrNotFound) {
			return confirmation.Confirmation{}, confirmation.ErrConflict
		}
		return confirmation.Confirmation{}, e
	}
	if stored.TaskID != c.TaskID || stored.Revision != c.ExpectedRevision || stored.State != confirmation.StateConfirmed ||
		stored.Binding.Digest != c.Binding.Digest || !stored.Binding.Equal(c.Binding) {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if !stored.ExpiresAt.After(at) {
		if e = expireConfirmationAndTaskTx(ctx, tx, stored, taskRow, confirmation.ReasonExpired, at); e != nil {
			return confirmation.Confirmation{}, e
		}
		var expired confirmation.Confirmation
		expired, e = getConfirmationTx(ctx, tx, c.OwnerID, c.ID)
		if e != nil {
			return confirmation.Confirmation{}, e
		}
		if e = saveConfirmationReplayTerminalTx(ctx, tx, c.OwnerID, "consume", c.IdempotencyKey, c.RequestDigest, expired, "expired", at); e != nil {
			return confirmation.Confirmation{}, e
		}
		if e = tx.Commit(); e != nil {
			return confirmation.Confirmation{}, e
		}
		return expired, confirmation.ErrExpired
	}
	r, e := tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='consumed',reservation_json=jsonb_build_object('task_id',$1::uuid,'attempt',$2::integer,'lease_epoch',$3::bigint,'task_revision',$4::bigint),revision=revision+1,updated_at=$5 WHERE confirmation_id=$6 AND owner_id=$7 AND state='confirmed' AND revision=$8`, c.TaskID, c.Attempt, c.LeaseEpoch, c.ExpectedTaskRevision, at, c.ID, c.OwnerID, c.ExpectedRevision)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	out, e := getConfirmationTx(ctx, tx, c.OwnerID, c.ID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if e = saveConfirmationReplayTx(ctx, tx, c.OwnerID, "consume", c.IdempotencyKey, c.RequestDigest, out, at); e != nil {
		return confirmation.Confirmation{}, e
	}
	if e = tx.Commit(); e != nil {
		return confirmation.Confirmation{}, e
	}
	return out, nil
}
func (s *DatabaseConfirmationStore) Release(ctx context.Context, c confirmation.ReleaseCommand) (confirmation.Confirmation, error) {
	_, e := s.db.ExecContext(ctx, `UPDATE agent_confirmations SET reservation_json=NULL,updated_at=$1 WHERE confirmation_id=$2 AND owner_id=$3`, c.At.UTC(), c.ID, c.OwnerID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	return s.get(ctx, c.OwnerID, c.ID)
}

func (s *DatabaseConfirmationStore) ReleaseReservation(ctx context.Context, c confirmation.ReleaseReservationCommand) (confirmation.Confirmation, error) {
	c.ConfirmationID = strings.TrimSpace(c.ConfirmationID)
	c.TaskID = strings.TrimSpace(c.TaskID)
	c.IdempotencyKey = strings.TrimSpace(c.IdempotencyKey)
	c.RequestDigest = confirmation.RequestDigestForRelease(c)
	if !validConfirmationUUID(c.ConfirmationID) || !validConfirmationUUID(c.TaskID) || !validConfirmationUUID(c.IdempotencyKey) ||
		c.AcquiredAttempt == 0 || c.AcquiredLeaseEpoch == 0 || c.TerminalAttempt == 0 || c.TerminalLeaseEpoch == 0 || c.ExpectedTaskRevision < 1 {
		return confirmation.Confirmation{}, confirmation.ErrInvalid
	}
	at := time.Now().UTC()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	defer tx.Rollback()
	owner, taskID, e := confirmationIdentityTx(ctx, tx, "", c.ConfirmationID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	replay, found, _, e := replayConfirmationMutationTx(ctx, tx, owner, "release", c.IdempotencyKey, c.RequestDigest, false)
	if found || e != nil {
		return replay, e
	}
	if taskID != c.TaskID {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	taskRow, e := lockConfirmationTaskTx(ctx, tx, owner, c.TaskID)
	if e != nil {
		return confirmation.Confirmation{}, confirmation.ErrTaskFenceConflict
	}
	if (taskRow.Status != "succeeded" && taskRow.Status != "failed" && taskRow.Status != "canceled") ||
		taskRow.Attempt != int(c.TerminalAttempt) || taskRow.LeaseEpoch != int64(c.TerminalLeaseEpoch) || taskRow.Revision != c.ExpectedTaskRevision {
		return confirmation.Confirmation{}, confirmation.ErrTaskFenceConflict
	}
	stored, e := getConfirmationForUpdateTx(ctx, tx, owner, c.ConfirmationID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if stored.State != confirmation.StateConsumed || stored.TaskID != c.TaskID {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	var raw []byte
	if e = tx.QueryRowContext(ctx, `SELECT reservation_json FROM agent_confirmations WHERE confirmation_id=$1 AND owner_id=$2`, c.ConfirmationID, owner).Scan(&raw); e != nil {
		return confirmation.Confirmation{}, e
	}
	var r struct {
		TaskID   string `json:"task_id"`
		Attempt  uint32 `json:"attempt"`
		Epoch    uint64 `json:"lease_epoch"`
		Revision int64  `json:"task_revision"`
	}
	if len(raw) > 0 {
		if json.Unmarshal(raw, &r) != nil || r.TaskID != c.TaskID || r.Attempt != c.AcquiredAttempt || r.Epoch != c.AcquiredLeaseEpoch || r.Revision < 1 {
			return confirmation.Confirmation{}, confirmation.ErrConflict
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE agent_confirmations SET reservation_json=NULL,revision=revision+1,updated_at=$1 WHERE confirmation_id=$2 AND owner_id=$3 AND state='consumed' AND revision=$4 AND reservation_json IS NOT NULL`, at, c.ConfirmationID, owner, stored.Revision)
		if updateErr != nil {
			return confirmation.Confirmation{}, updateErr
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return confirmation.Confirmation{}, confirmation.ErrConflict
		}
	}
	out, e := getConfirmationTx(ctx, tx, owner, c.ConfirmationID)
	if e != nil {
		return confirmation.Confirmation{}, e
	}
	if e = saveConfirmationReplayTx(ctx, tx, owner, "release", c.IdempotencyKey, c.RequestDigest, out, at); e != nil {
		return confirmation.Confirmation{}, e
	}
	if e = tx.Commit(); e != nil {
		return confirmation.Confirmation{}, e
	}
	return out, nil
}
func expireConfirmationAndTaskTx(ctx context.Context, tx *sql.Tx, stored confirmation.Confirmation, taskRow confirmationTaskRow, reason string, at time.Time) error {
	if err := terminalizeAWSPreProviderChangeTx(ctx, tx, stored, taskRow, reason, at); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='expired',terminal_reason=$1,revision=revision+1,updated_at=$2 WHERE confirmation_id=$3 AND owner_id=$4 AND state IN ('pending','confirmed') AND revision=$5`, reason, at.UTC(), stored.ID, stored.OwnerID, stored.Revision)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return confirmation.ErrRevisionConflict
	}

	taskWasRunning := taskRow.Status == "running"
	taskIsMutable := taskWasRunning || taskRow.Status == "queued" || taskRow.Status == "waiting_user"
	if taskIsMutable {
		result, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='failed',failure_code=$1,failure_summary=$1,lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$2 WHERE task_id=$3 AND owner_id=$4 AND status=$5`, reason, at.UTC(), stored.TaskID, stored.OwnerID, taskRow.Status)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return confirmation.ErrTaskFenceConflict
		}
		if taskWasRunning {
			if _, err = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true`, at.UTC()); err != nil {
				return err
			}
		}
	}
	if err = terminalizeConfirmationsTx(ctx, tx, stored.TaskID, reason, at.UTC()); err != nil {
		return err
	}
	if strings.HasPrefix(stored.Binding.OperationDomain, "workload:") {
		if err = terminalizeWorkloadOperationTx(ctx, tx, stored, "expired", reason, reason, at.UTC()); err != nil {
			return err
		}
	}
	if taskIsMutable {
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,$1::text,'failed',jsonb_build_object('reason',$1::text),$2 FROM agent_tasks WHERE task_id=$3 AND owner_id=$4`, reason, at.UTC(), stored.TaskID, stored.OwnerID); err != nil {
			return err
		}
	}
	return nil
}

// terminalizeWorkloadOperationTx keeps the workload operation projection in
// lockstep with its confirmation/task terminal transition. Workload
// operations have their own live index; leaving one in waiting_user would
// incorrectly block a subsequent operation for the same workload forever.
// The operation row is locked by task identity (the task was already locked
// by the caller), then transitioned and given one terminal event in the same
// transaction. Non-workload confirmations simply have no matching row.
func terminalizeWorkloadOperationTx(ctx context.Context, tx *sql.Tx, stored confirmation.Confirmation, status, code, summary string, at time.Time) error {
	owner, taskID, confirmationID, targetID := stored.OwnerID, stored.TaskID, stored.ID, stored.Binding.TargetID
	rows, err := tx.QueryContext(ctx, `SELECT operation_id::text,workload_id::text,confirmation_id::text,plan_revision,operation,status,dispatch_state,revision,expected_workload_revision
		FROM core_workload_operations WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, owner, taskID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return err
		}
		return confirmation.ErrNotFound
	}
	var operationID, workloadID, operationConfirmationID, operationKind, operationStatus, dispatchState string
	var planRevision, revision, expectedWorkloadRevision int64
	if err = rows.Scan(&operationID, &workloadID, &operationConfirmationID, &planRevision, &operationKind, &operationStatus, &dispatchState, &revision, &expectedWorkloadRevision); err != nil {
		return err
	}
	if rows.Next() {
		return confirmation.ErrConflict
	}
	if err = rows.Err(); err != nil {
		return err
	}
	expectedOperation := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(stored.Binding.OperationDomain, "workload:")))
	if expectedOperation == "" || operationConfirmationID != confirmationID || workloadID != targetID ||
		planRevision != stored.Binding.TargetRevision || strings.ToLower(strings.TrimSpace(operationKind)) != expectedOperation {
		return confirmation.ErrConflict
	}
	if operationStatus != "waiting_user" || dispatchState != "prepared" || revision < 1 {
		return confirmation.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_workload_operations
		SET status=$1,dispatch_state='terminal',failure_code=$2,failure_summary=$3,
			revision=revision+1,updated_at=$4
		WHERE owner_id=$5 AND operation_id=$6 AND task_id=$7 AND status='waiting_user'
		  AND dispatch_state='prepared' AND revision=$8`, status, code, summary, at.UTC(), owner, operationID, taskID, revision)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return confirmation.ErrRevisionConflict
	}
	if operationKind == "apply" && (status == "expired" || status == "rejected" || status == "canceled") {
		result, err = tx.ExecContext(ctx, `UPDATE core_workloads SET state='failed',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND workload_id=$3 AND state='pending' AND revision=$4 AND (actual_snapshot_json IS NULL OR actual_snapshot_json='{}'::jsonb OR actual_snapshot_json='null'::jsonb)`, at.UTC(), owner, workloadID, expectedWorkloadRevision)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			var currentState string
			var currentRevision int64
			if err = tx.QueryRowContext(ctx, `SELECT state,revision FROM core_workloads WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`, owner, workloadID).Scan(&currentState, &currentRevision); err != nil {
				return err
			}
			if currentRevision != expectedWorkloadRevision || (currentState != "destroyed" && currentState != "ready" && currentState != "failed") {
				return confirmation.ErrConflict
			}
		}
	}
	var sequence uint64
	if err = tx.QueryRowContext(ctx, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence)
		VALUES($1,$2,2)
		ON CONFLICT(owner_id,operation_id) DO UPDATE
		SET next_sequence=core_workload_event_counters.next_sequence+1
		RETURNING next_sequence-1`, owner, operationID).Scan(&sequence); err != nil {
		return err
	}
	return insertWorkloadEventTx(ctx, tx, owner, operationID, sequence, "terminal", status, summary, nil, at.UTC())
}

func (s *DatabaseConfirmationStore) ExpireAt(ctx context.Context, owner, id string, at time.Time) error {
	owner = strings.TrimSpace(owner)
	id = strings.TrimSpace(id)
	if owner == "" || !validConfirmationUUID(id) {
		return confirmation.ErrInvalid
	}
	at = at.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockAWSPreProviderByConfirmationTx(ctx, tx, owner, id); err != nil {
		return err
	}
	_, taskID, err := confirmationIdentityTx(ctx, tx, owner, id)
	if errors.Is(err, confirmation.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	taskRow, err := lockConfirmationTaskTx(ctx, tx, owner, taskID)
	if err != nil {
		return err
	}
	stored, err := getConfirmationForUpdateTx(ctx, tx, owner, id)
	if err != nil {
		return err
	}
	if stored.TaskID != taskID {
		return confirmation.ErrConflict
	}
	if (stored.State != confirmation.StatePending && stored.State != confirmation.StateConfirmed) || stored.ExpiresAt.After(at) {
		return nil
	}
	if err = expireConfirmationAndTaskTx(ctx, tx, stored, taskRow, confirmation.ReasonExpired, at); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DatabaseConfirmationStore) Expire(ctx context.Context, c confirmation.ExpireCommand) (confirmation.Confirmation, error) {
	c.ConfirmationID = strings.TrimSpace(c.ConfirmationID)
	c.IdempotencyKey = strings.TrimSpace(c.IdempotencyKey)
	c.Reason = strings.TrimSpace(c.Reason)
	c.RequestDigest = confirmation.RequestDigestForExpire(c)
	if !validConfirmationUUID(c.ConfirmationID) || !validConfirmationUUID(c.IdempotencyKey) || c.ExpectedRevision < 1 ||
		(c.Reason != confirmation.ReasonExpired && c.Reason != confirmation.ReasonStale) {
		return confirmation.Confirmation{}, confirmation.ErrInvalid
	}
	at := c.At.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return confirmation.Confirmation{}, err
	}
	defer tx.Rollback()
	if err = lockAWSPreProviderByConfirmationTx(ctx, tx, "", c.ConfirmationID); err != nil {
		return confirmation.Confirmation{}, err
	}
	owner, taskID, err := confirmationIdentityTx(ctx, tx, "", c.ConfirmationID)
	if err != nil {
		return confirmation.Confirmation{}, err
	}
	replay, found, _, err := replayConfirmationMutationTx(ctx, tx, owner, "expire", c.IdempotencyKey, c.RequestDigest, false)
	if found || err != nil {
		return replay, err
	}
	taskRow, err := lockConfirmationTaskTx(ctx, tx, owner, taskID)
	if err != nil {
		return confirmation.Confirmation{}, err
	}
	stored, err := getConfirmationForUpdateTx(ctx, tx, owner, c.ConfirmationID)
	if err != nil {
		return confirmation.Confirmation{}, err
	}
	if stored.TaskID != taskID {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if stored.Revision != c.ExpectedRevision {
		return confirmation.Confirmation{}, confirmation.ErrRevisionConflict
	}
	if stored.State != confirmation.StatePending && stored.State != confirmation.StateConfirmed {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if c.Reason == confirmation.ReasonExpired && stored.ExpiresAt.After(at) {
		return confirmation.Confirmation{}, confirmation.ErrConflict
	}
	if err = expireConfirmationAndTaskTx(ctx, tx, stored, taskRow, c.Reason, at); err != nil {
		return confirmation.Confirmation{}, err
	}
	out, err := getConfirmationTx(ctx, tx, owner, c.ConfirmationID)
	if err != nil {
		return confirmation.Confirmation{}, err
	}
	if err = saveConfirmationReplayTx(ctx, tx, owner, "expire", c.IdempotencyKey, c.RequestDigest, out, at); err != nil {
		return confirmation.Confirmation{}, err
	}
	if err = tx.Commit(); err != nil {
		return confirmation.Confirmation{}, err
	}
	return out, nil
}
func (s *DatabaseConfirmationStore) List(ctx context.Context, q confirmation.ListQuery) (confirmation.Page, error) {
	owner := strings.TrimSpace(q.OwnerID)
	domain := strings.TrimSpace(q.Domain)
	targetID := strings.TrimSpace(q.TargetID)
	if owner == "" {
		return confirmation.Page{}, confirmation.ErrInvalid
	}
	pageSize := q.PageSize
	if pageSize == 0 {
		pageSize = q.Limit
	}
	if pageSize == 0 {
		pageSize = 50
	}
	if pageSize < 1 || pageSize > 100 {
		return confirmation.Page{}, confirmation.ErrInvalid
	}
	states := append([]confirmation.State(nil), q.States...)
	if q.State != nil {
		states = append(states, *q.State)
	}
	seen := make(map[confirmation.State]struct{}, len(states))
	normalizedStates := make([]confirmation.State, 0, len(states))
	for _, state := range states {
		if !confirmation.ValidState(state) {
			return confirmation.Page{}, confirmation.ErrInvalid
		}
		if _, ok := seen[state]; ok {
			continue
		}
		seen[state] = struct{}{}
		normalizedStates = append(normalizedStates, state)
	}
	sort.Slice(normalizedStates, func(i, j int) bool { return normalizedStates[i] < normalizedStates[j] })
	stateValues := make([]string, 0, len(normalizedStates))
	for _, state := range normalizedStates {
		stateValues = append(stateValues, string(state))
	}
	filter := confirmation.ListFilterDigest(domain, targetID, normalizedStates)
	cursor := databaseConfirmationCursor{}
	hasCursor := false
	if token := strings.TrimSpace(q.PageToken); token != "" {
		if len(token) > 4096 {
			return confirmation.Page{}, confirmation.ErrInvalid
		}
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(decoded) > 2048 || json.Unmarshal(decoded, &cursor) != nil ||
			cursor.OwnerID != owner || cursor.Filter != filter || cursor.CreatedAt.IsZero() || !validConfirmationUUID(cursor.ID) {
			return confirmation.Page{}, confirmation.ErrInvalid
		}
		hasCursor = true
	}
	cursorTime := cursor.CreatedAt.UTC()
	cursorID := cursor.ID
	if !hasCursor {
		cursorID = uuid.Nil.String()
	}
	cutoff := time.Now().UTC()
	if err := s.expireOverdue(ctx, owner, domain, targetID, cutoff, pageSize+1); err != nil {
		return confirmation.Page{}, err
	}
	rows, e := s.db.QueryContext(ctx, `SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason
FROM agent_confirmations
WHERE owner_id=$1
  AND ($2='' OR operation_domain=$2)
  AND ($3='' OR target_id=$3)
  AND (cardinality($4::text[])=0 OR state=ANY($4::text[]))
  AND (NOT $5 OR created_at>$6 OR (created_at=$6 AND confirmation_id>$7::uuid))
  AND NOT (state IN ('pending','confirmed') AND expires_at <= $9)
ORDER BY created_at ASC,confirmation_id ASC
LIMIT $8`, owner, domain, targetID, pq.Array(stateValues), hasCursor, cursorTime, cursorID, pageSize+1, cutoff)
	if e != nil {
		return confirmation.Page{}, e
	}
	defer rows.Close()
	out := make([]confirmation.Confirmation, 0, pageSize+1)
	for rows.Next() {
		value, scanErr := scanConfirmation(rows)
		if scanErr != nil {
			return confirmation.Page{}, scanErr
		}
		if value.OwnerID != owner {
			return confirmation.Page{}, confirmation.ErrConflict
		}
		out = append(out, value)
	}
	if e = rows.Err(); e != nil {
		return confirmation.Page{}, e
	}
	page := confirmation.Page{Confirmations: out}
	if len(out) > pageSize {
		last := out[pageSize-1]
		encoded, encodeErr := json.Marshal(databaseConfirmationCursor{
			OwnerID:   owner,
			CreatedAt: last.CreatedAt.UTC(),
			ID:        last.ID,
			Filter:    filter,
		})
		if encodeErr != nil {
			return confirmation.Page{}, encodeErr
		}
		page.Confirmations = out[:pageSize]
		page.NextPageToken = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}

// ExpireOverdue performs a bounded owner-scoped maintenance sweep. Runtime
// schedulers may call it independently of a read path; each candidate still
// goes through ExpireAt and its task-first transaction.
func (s *DatabaseConfirmationStore) ExpireOverdue(ctx context.Context, owner string, at time.Time) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return confirmation.ErrInvalid
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.expireOverdue(ctx, owner, "", "", at.UTC(), maxOverdueSweepCandidates)
}

// expireOverdue terminalizes owner-scoped active cards before the list query.
// ExpireAt owns the task/workload/deployment transition and lock order; this
// scan only discovers candidates so pagination and state filters remain the
// canonical query semantics after those transitions commit.
func (s *DatabaseConfirmationStore) expireOverdue(ctx context.Context, owner, domain, targetID string, at time.Time, budget int) error {
	if budget < 1 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT confirmation_id::text
FROM agent_confirmations
WHERE owner_id=$1
  AND ($2='' OR operation_domain=$2)
  AND ($3='' OR target_id=$3)
  AND state IN ('pending','confirmed')
  AND expires_at <= $4
ORDER BY created_at ASC,confirmation_id ASC
LIMIT $5`, owner, domain, targetID, at.UTC(), budget)
	if err != nil {
		return err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.ExpireAt(ctx, owner, id, at); err != nil {
			return err
		}
	}
	return nil
}

type databaseConfirmationCursor struct {
	OwnerID   string              `json:"owner_id"`
	CreatedAt time.Time           `json:"created_at"`
	ID        string              `json:"id"`
	Filter    confirmation.Digest `json:"filter"`
}

func mapConfirmationError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique") {
		return confirmation.ErrConflict
	}
	return err
}

var _ confirmation.Repository = (*DatabaseConfirmationStore)(nil)
