package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

// AgentTaskDDL is returned to migration owners; registration intentionally
// remains in storage_migrations.go's caller-owned surface.
const AgentTaskDDL = `
CREATE TABLE IF NOT EXISTS agent_tasks (
 task_id uuid PRIMARY KEY,
 owner_id text NOT NULL,
 spec_json jsonb NOT NULL,
 status text NOT NULL CHECK (status IN ('queued','running','waiting_user','succeeded','failed','canceled')),
 attempt integer NOT NULL DEFAULT 0,
 lease_epoch bigint NOT NULL DEFAULT 0,
 lease_holder text NOT NULL DEFAULT '',
 lease_expires_at timestamptz,
 revision bigint NOT NULL DEFAULT 1,
 progress_sequence bigint NOT NULL DEFAULT 0,
 available_at timestamptz NOT NULL,
 result_json jsonb,
 failure_code text NOT NULL DEFAULT '',
 failure_summary text NOT NULL DEFAULT '',
 retry_of_task_id uuid,
 execution_started_at timestamptz,
 execution_deadline_at timestamptz,
 deleted_at timestamptz,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS agent_tasks_owner_idem_idx ON agent_tasks(owner_id, (spec_json->>'idempotency_key'));
CREATE UNIQUE INDEX IF NOT EXISTS agent_tasks_owner_task_idx ON agent_tasks(owner_id,task_id);
CREATE INDEX IF NOT EXISTS agent_tasks_due_idx ON agent_tasks(status,available_at,task_id);
CREATE TABLE IF NOT EXISTS agent_task_events (owner_id text NOT NULL, task_id uuid NOT NULL, sequence bigint NOT NULL, event_type text NOT NULL, status text NOT NULL, payload_json jsonb, occurred_at timestamptz NOT NULL, PRIMARY KEY(owner_id,task_id,sequence), FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS agent_task_execution_snapshots (owner_id text NOT NULL, task_id uuid NOT NULL, snapshot_json jsonb NOT NULL, snapshot_digest text NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(owner_id,task_id), FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS agent_task_model_rounds (owner_id text NOT NULL, task_id uuid NOT NULL, attempt integer NOT NULL, round integer NOT NULL, lease_epoch bigint NOT NULL, input_digest text NOT NULL, state text NOT NULL, response_json jsonb, updated_at timestamptz NOT NULL, PRIMARY KEY(owner_id,task_id,attempt,round), FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS agent_task_tool_calls (owner_id text NOT NULL, task_id uuid NOT NULL, attempt integer NOT NULL, call_id text NOT NULL, lease_epoch bigint NOT NULL, input_digest text NOT NULL, state text NOT NULL, response_json jsonb, updated_at timestamptz NOT NULL, PRIMARY KEY(owner_id,task_id,attempt,call_id), FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS agent_task_runtime_concurrency (singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton), running_count integer NOT NULL DEFAULT 0, max_concurrent integer NOT NULL DEFAULT 1, revision bigint NOT NULL DEFAULT 1, updated_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE IF NOT EXISTS agent_schedule_occurrences (occurrence_id uuid PRIMARY KEY, schedule_id uuid NOT NULL, owner_id text NOT NULL, scheduled_for timestamptz NOT NULL, task_id uuid NOT NULL, run_id uuid NOT NULL, created_at timestamptz NOT NULL, UNIQUE(owner_id,schedule_id,scheduled_for), FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id));
CREATE TABLE IF NOT EXISTS agent_task_replays (owner_id text NOT NULL, operation text NOT NULL, idempotency_key uuid NOT NULL, request_digest text NOT NULL, response_json jsonb NOT NULL, created_at timestamptz NOT NULL, PRIMARY KEY(owner_id,operation,idempotency_key));`

// DatabaseTaskStore is a PostgreSQL adapter. It uses FOR UPDATE SKIP LOCKED
// for queue claims and revision/lease predicates for the concurrency fence.
type DatabaseTaskStore struct{ db *sql.DB }

func NewDatabaseTaskStore(db *sql.DB) *DatabaseTaskStore { return &DatabaseTaskStore{db: db} }

type databaseTaskRowScanner interface {
	Scan(...any) error
}

const databaseTaskSelect = `SELECT owner_id,spec_json,status,attempt,lease_epoch,revision,progress_sequence,available_at,lease_holder,lease_expires_at,result_json,failure_code,failure_summary,execution_started_at,execution_deadline_at,retry_of_task_id::text,created_at,updated_at,deleted_at FROM agent_tasks`

func scanDatabaseTask(row databaseTaskRowScanner, id string) (task.Task, error) {
	var value task.Task
	var raw, result []byte
	var status, holder, code, summary string
	var epoch, revision, sequence int64
	var attempt int
	var lease, deleted, started, deadline *time.Time
	var retryOf *string
	err := row.Scan(
		&value.OwnerID, &raw, &status, &attempt, &epoch, &revision, &sequence,
		&value.AvailableAt, &holder, &lease, &result, &code, &summary, &started,
		&deadline, &retryOf, &value.CreatedAt, &value.UpdatedAt, &deleted,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, task.ErrNotFound
	}
	if err != nil {
		return task.Task{}, err
	}
	if err = json.Unmarshal(raw, &value.Spec); err != nil {
		return task.Task{}, task.ErrInvalid
	}
	value.ID = id
	value.Status = task.Status(status)
	value.Attempt = uint32(attempt)
	value.LeaseEpoch = uint64(epoch)
	value.Revision = uint64(revision)
	value.ProgressSequence = uint64(sequence)
	value.FailureCode = code
	value.FailureSummary = summary
	value.AvailableAt = value.AvailableAt.UTC()
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	if deleted != nil {
		normalized := deleted.UTC()
		value.DeletedAt = &normalized
	}
	if started != nil {
		normalized := started.UTC()
		value.ExecutionStartedAt = &normalized
	}
	if deadline != nil {
		normalized := deadline.UTC()
		value.ExecutionDeadlineAt = &normalized
	}
	if retryOf != nil {
		value.RetryOfTaskID = *retryOf
	}
	if len(result) > 0 {
		var parsed task.Result
		if json.Unmarshal(result, &parsed) != nil {
			return task.Task{}, task.ErrInvalid
		}
		value.Result = &parsed
	}
	if lease != nil {
		value.Lease = &task.Lease{TaskID: id, Attempt: value.Attempt, Epoch: value.LeaseEpoch, Holder: holder, ExpiresAt: lease.UTC()}
	}
	return value, nil
}

func getDatabaseTaskTx(ctx context.Context, tx *sql.Tx, owner, id string, lock bool) (task.Task, error) {
	query := databaseTaskSelect + ` WHERE task_id=$1 AND owner_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanDatabaseTask(tx.QueryRowContext(ctx, query, id, owner), id)
}

// databaseTaskIsExecutionStageTx prevents the generic task terminalizers from
// splitting execution.v2's stage/run/receipt transaction. The coordinator
// owns that terminal transition once it exists.
func databaseTaskIsExecutionStageTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(spec_json->>'kind','') FROM agent_tasks WHERE task_id=$1 FOR UPDATE`, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		// Preserve the legacy terminalizer result (its guarded UPDATE reports
		// the normal lease conflict for an absent/stale task).
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return task.TaskKind(strings.TrimSpace(kind)) == task.TaskKindExecutionStage, nil
}

func taskOwnerTx(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT owner_id FROM agent_tasks WHERE task_id=$1`, id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", task.ErrNotFound
	}
	return owner, err
}

func normalizeDatabaseTaskUUID(value string) (string, error) {
	id, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil || id == uuid.Nil {
		return "", task.ErrInvalid
	}
	return id.String(), nil
}

func taskCancelRequestDigest(command task.CancelCommand) string {
	return task.Digest(struct {
		TaskID           string `json:"task_id"`
		ExpectedRevision uint64 `json:"expected_revision"`
		Reason           string `json:"reason"`
	}{
		TaskID:           command.TaskID,
		ExpectedRevision: command.Mutation.ExpectedRevision,
		Reason:           command.Reason,
	})
}

func taskRetryRequestDigest(command task.RetryCommand) string {
	return task.Digest(struct {
		TaskID           string `json:"task_id"`
		ExpectedRevision uint64 `json:"expected_revision"`
	}{
		TaskID:           command.TaskID,
		ExpectedRevision: command.Mutation.ExpectedRevision,
	})
}

func lockTaskReplayTx(ctx context.Context, tx *sql.Tx, owner, operation, key string) error {
	if strings.TrimSpace(owner) == "" {
		return task.ErrInvalid
	}
	canonicalKey, err := normalizeDatabaseTaskUUID(key)
	if err != nil || canonicalKey != key {
		return task.ErrInvalid
	}
	lockKey := canonicalAdvisoryLockIdentity("agent-task", strings.TrimSpace(owner), operation, canonicalKey)
	_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey)
	return err
}

func readTaskReplayTx(ctx context.Context, tx *sql.Tx, owner, operation, key, digest string) (task.Task, bool, error) {
	var storedDigest string
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM agent_task_replays WHERE owner_id=$1 AND operation=$2 AND idempotency_key=$3`, owner, operation, key).Scan(&storedDigest, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, false, nil
	}
	if err != nil {
		return task.Task{}, false, err
	}
	if storedDigest != digest {
		return task.Task{}, true, task.ErrConflict
	}
	var out task.Task
	if json.Unmarshal(raw, &out) != nil || out.OwnerID != owner || out.Validate() != nil {
		return task.Task{}, true, task.ErrConflict
	}
	return out, true, nil
}

func saveTaskReplayTx(ctx context.Context, tx *sql.Tx, owner, operation, key, digest string, value task.Task, at time.Time) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_task_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,$5,$6)`, owner, operation, key, digest, raw, at.UTC())
	return err
}

func deterministicDatabaseTaskID(owner, idempotencyKey string) string {
	return uuid.NewSHA1(uuid.Nil, []byte(owner+"\x00task\x00"+idempotencyKey)).String()
}

func (s *DatabaseTaskStore) Create(ctx context.Context, c task.CreateCommand) (task.Task, error) {
	if s == nil || s.db == nil {
		return task.Task{}, task.ErrInvalid
	}
	if c.Spec.Validate() != nil {
		return task.Task{}, task.ErrInvalid
	}
	if c.Spec.Kind == task.TaskKindExecutionStage {
		return task.Task{}, task.ErrConflict
	}
	now := c.At.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	id := deterministicDatabaseTaskID(c.OwnerID, c.Spec.IdempotencyKey)
	raw, _ := json.Marshal(c.Spec)
	_, e := s.db.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,$5,$5) ON CONFLICT(owner_id,(spec_json->>'idempotency_key')) DO NOTHING`, id, c.OwnerID, raw, c.Spec.AvailableAt.UTC(), now)
	if e != nil {
		return task.Task{}, e
	}
	return s.Get(ctx, c.OwnerID, id)
}
func (s *DatabaseTaskStore) Get(ctx context.Context, owner, id string) (task.Task, error) {
	if owner == "" {
		if e := s.db.QueryRowContext(ctx, `SELECT owner_id FROM agent_tasks WHERE task_id=$1`, id).Scan(&owner); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return task.Task{}, task.ErrNotFound
			}
			return task.Task{}, e
		}
	}
	return scanDatabaseTask(s.db.QueryRowContext(ctx, databaseTaskSelect+` WHERE task_id=$1 AND owner_id=$2`, id, owner), id)
}
func (s *DatabaseTaskStore) Claim(ctx context.Context, c task.ClaimCommand) (task.Task, task.Lease, error) {
	if s == nil || s.db == nil || c.Holder == "" || c.LeaseTTL <= 0 || c.At.IsZero() || c.At.Location() != time.UTC {
		return task.Task{}, task.Lease{}, task.ErrInvalid
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, task.Lease{}, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_runtime_concurrency(singleton,running_count,max_concurrent,revision,updated_at) VALUES(true,0,1,1,$1) ON CONFLICT(singleton) DO NOTHING`, c.At.UTC()); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	var id string
	var rev int64
	var epoch int64
	var running, max int
	err := tx.QueryRowContext(ctx, `SELECT running_count,max_concurrent FROM agent_task_runtime_concurrency WHERE singleton=true FOR UPDATE`).Scan(&running, &max)
	if err != nil {
		return task.Task{}, task.Lease{}, err
	}
	if running >= max {
		return task.Task{}, task.Lease{}, task.ErrConflict
	}
	err = tx.QueryRowContext(ctx, `SELECT task_id::text,revision,lease_epoch FROM agent_tasks WHERE task_id=$1 AND owner_id=$2 AND status='queued' AND available_at<= $3 AND deleted_at IS NULL AND COALESCE(spec_json->>'kind','') <> 'execution_stage' FOR UPDATE SKIP LOCKED`, c.TaskID, c.OwnerID, c.At.UTC()).Scan(&id, &rev, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, task.Lease{}, task.ErrConflict
	}
	if err != nil {
		return task.Task{}, task.Lease{}, err
	}
	if uint64(rev) != c.ExpectedRevision || uint64(epoch)+1 != c.LeaseEpoch {
		return task.Task{}, task.Lease{}, task.ErrLeaseConflict
	}
	until := c.At.UTC().Add(c.LeaseTTL)
	_, e = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='running',attempt=GREATEST(attempt,1),lease_epoch=$2,lease_holder=$3,lease_expires_at=$4,execution_started_at=COALESCE(execution_started_at,$5),revision=revision+1,updated_at=$4 WHERE task_id=$1`, id, c.LeaseEpoch, c.Holder, until, c.At.UTC())
	if e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=running_count+1,revision=revision+1,updated_at=$1 WHERE singleton=true`, c.At.UTC()); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE agent_tasks SET progress_sequence=progress_sequence+1 WHERE task_id=$1`, id); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'claimed','running',jsonb_build_object('holder',$2::text,'lease_epoch',$3::bigint),$4 FROM agent_tasks WHERE task_id=$1`, id, c.Holder, c.LeaseEpoch, c.At.UTC()); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if e = tx.Commit(); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	t, e := s.Get(ctx, c.OwnerID, id)
	if e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if t.Lease == nil {
		return task.Task{}, task.Lease{}, task.ErrLeaseConflict
	}
	return t, *t.Lease, nil
}

func (s *DatabaseTaskStore) List(ctx context.Context, q task.TaskListQuery) ([]task.Task, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT task_id::text FROM agent_tasks WHERE owner_id=$1 AND ($2='' OR status=$2) AND ($3 OR deleted_at IS NULL) ORDER BY created_at,task_id LIMIT $4`, q.OwnerID, statusValue(q.Status), q.IncludeDeleted, q.Limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []task.Task{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		v, e := s.Get(ctx, q.OwnerID, id)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func statusValue(s *task.Status) string {
	if s == nil {
		return ""
	}
	return string(*s)
}
func (s *DatabaseTaskStore) Reclaim(ctx context.Context, c task.ReclaimCommand) (task.Task, task.Lease, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, task.Lease{}, e
	}
	defer tx.Rollback()
	var id string
	var rev, epoch int64
	err := tx.QueryRowContext(ctx, `SELECT task_id::text,revision,lease_epoch FROM agent_tasks WHERE task_id=$1 AND owner_id=$2 AND status='running' AND lease_expires_at<=$3 AND COALESCE(spec_json->>'kind','') <> 'execution_stage' FOR UPDATE SKIP LOCKED`, c.TaskID, c.OwnerID, c.At.UTC()).Scan(&id, &rev, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, task.Lease{}, task.ErrConflict
	}
	if err != nil {
		return task.Task{}, task.Lease{}, err
	}
	if uint64(rev) != c.ExpectedRevision || uint64(epoch)+1 != c.LeaseEpoch {
		return task.Task{}, task.Lease{}, task.ErrLeaseConflict
	}
	until := c.At.UTC().Add(c.LeaseTTL)
	if _, e = tx.ExecContext(ctx, `UPDATE agent_tasks SET lease_epoch=$2,lease_holder=$3,lease_expires_at=$4,revision=revision+1,updated_at=$4 WHERE task_id=$1`, id, c.LeaseEpoch, c.Holder, until); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE agent_tasks SET progress_sequence=progress_sequence+1 WHERE task_id=$1`, id); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'reclaimed','running',jsonb_build_object('holder',$2::text,'lease_epoch',$3::bigint),$4 FROM agent_tasks WHERE task_id=$1`, id, c.Holder, c.LeaseEpoch, c.At.UTC()); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if e = tx.Commit(); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	t, e := s.Get(ctx, c.OwnerID, id)
	if e != nil {
		return task.Task{}, task.Lease{}, e
	}
	return t, *t.Lease, nil
}
func (s *DatabaseTaskStore) Renew(ctx context.Context, c task.RenewLeaseCommand) (task.Lease, error) {
	until := c.At.UTC().Add(c.LeaseTTL)
	r, e := s.db.ExecContext(ctx, `UPDATE agent_tasks SET lease_expires_at=$1,revision=revision+1,updated_at=$1 WHERE task_id=$2 AND status='running' AND lease_holder=$3 AND lease_epoch=$4 AND revision=$5 AND lease_expires_at>$6 AND COALESCE(spec_json->>'kind','') <> 'execution_stage'`, until, c.TaskID, c.Holder, c.LeaseEpoch, c.ExpectedRevision, c.At.UTC())
	if e != nil {
		return task.Lease{}, e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return task.Lease{}, task.ErrLeaseConflict
	}
	return task.Lease{TaskID: c.TaskID, Attempt: c.Attempt, Epoch: c.LeaseEpoch, Holder: c.Holder, ExpiresAt: until}, nil
}
func (s *DatabaseTaskStore) AppendProgress(ctx context.Context, c task.ProgressCommand) (task.Task, task.Progress, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, task.Progress{}, e
	}
	defer tx.Rollback()
	var owner string
	var seq, rev int64
	var attempt int
	var epoch int64
	var exp time.Time
	e = tx.QueryRowContext(ctx, `SELECT owner_id,progress_sequence,revision,attempt,lease_epoch,lease_expires_at FROM agent_tasks WHERE task_id=$1 AND COALESCE(spec_json->>'kind','') <> 'execution_stage' FOR UPDATE`, c.TaskID).Scan(&owner, &seq, &rev, &attempt, &epoch, &exp)
	if e != nil {
		return task.Task{}, task.Progress{}, e
	}
	if uint64(seq) != c.ExpectedSequence || uint64(rev) != c.ExpectedRevision || uint64(epoch) != c.LeaseEpoch || int(c.Attempt) != attempt || !c.Progress.At.UTC().Before(exp) {
		return task.Task{}, task.Progress{}, task.ErrLeaseConflict
	}
	p := c.Progress
	p.Sequence = uint64(seq) + 1
	p.TaskID = c.TaskID
	p.Attempt = uint32(attempt)
	raw, _ := json.Marshal(p)
	_, e = tx.ExecContext(ctx, `UPDATE agent_tasks SET progress_sequence=progress_sequence+1,revision=revision+1,updated_at=$2 WHERE task_id=$1`, c.TaskID, c.Progress.At.UTC())
	if e == nil {
		_, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,$2,'progress',$3,$4,$5 FROM agent_tasks WHERE task_id=$1`, c.TaskID, p.Sequence, p.Status, raw, c.Progress.At.UTC())
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		return task.Task{}, task.Progress{}, e
	}
	v, e := s.Get(ctx, owner, c.TaskID)
	return v, p, e
}
func (s *DatabaseTaskStore) Events(ctx context.Context, owner, id string, after uint64, limit int) ([]task.Event, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT e.sequence,e.event_type,e.status,e.payload_json,e.occurred_at FROM agent_task_events e WHERE e.owner_id=$1 AND e.task_id=$2 AND e.sequence>$3 ORDER BY e.sequence LIMIT $4`, owner, id, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []task.Event{}
	for rows.Next() {
		var v task.Event
		if e = rows.Scan(&v.Sequence, &v.Type, &v.Status, &v.Payload, &v.At); e != nil {
			return nil, e
		}
		v.TaskID = id
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *DatabaseTaskStore) transition(ctx context.Context, c task.Fence, status task.Status, code string) (task.Task, error) {
	at := time.Now().UTC()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, e
	}
	defer tx.Rollback()
	r, e := tx.ExecContext(ctx, `UPDATE agent_tasks SET status=$1,failure_code=$2,lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$3 WHERE task_id=$4 AND status='running' AND attempt=$5 AND lease_epoch=$6 AND revision=$7 AND lease_expires_at>$3 AND COALESCE(spec_json->>'kind','') <> 'execution_stage'`, status, code, at, c.TaskID, c.Attempt, c.LeaseEpoch, c.ExpectedRevision)
	if e != nil {
		return task.Task{}, e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return task.Task{}, task.ErrLeaseConflict
	}
	if _, e = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true`, at); e != nil {
		return task.Task{}, e
	}
	if e = terminalizeConfirmationsTx(ctx, tx, c.TaskID, code, at); e != nil {
		return task.Task{}, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,$2::text,$3::text,jsonb_build_object('code',$4::text),$5 FROM agent_tasks WHERE task_id=$1`, c.TaskID, string(status), string(status), code, at); e != nil {
		return task.Task{}, e
	}
	if e = tx.Commit(); e != nil {
		return task.Task{}, e
	}
	return s.Get(ctx, "", c.TaskID)
}

// terminalizeConfirmationsTx closes unconsumed approvals and releases any live
// consumed reservation in the same transaction as the task terminal state.
// The consumed state itself is retained as the immutable execution fact.
func terminalizeConfirmationsTx(ctx context.Context, tx *sql.Tx, taskID, reason string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE agent_confirmations
SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END,
    terminal_reason=CASE WHEN state IN ('pending','confirmed') THEN $2 ELSE terminal_reason END,
    revision=revision+1,
    updated_at=$3,
    reservation_json=NULL
WHERE task_id=$1
  AND (state IN ('pending','confirmed') OR (state='consumed' AND reservation_json IS NOT NULL))`, taskID, reason, at.UTC())
	return err
}
func (s *DatabaseTaskStore) WaitUser(ctx context.Context, c task.WaitUserCommand) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	r, e := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='waiting_user',lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$1 WHERE task_id=$2 AND status='running' AND attempt=$3 AND lease_epoch=$4 AND revision=$5 AND lease_expires_at>$1 AND COALESCE(spec_json->>'kind','') <> 'execution_stage'`, c.At.UTC(), c.TaskID, c.Attempt, c.LeaseEpoch, c.ExpectedRevision)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return task.ErrLeaseConflict
	}
	if _, e = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true`, c.At.UTC()); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'waiting_user','waiting_user',jsonb_build_object('reason',$2::text),$3 FROM agent_tasks WHERE task_id=$1`, c.TaskID, c.Reason, c.At.UTC()); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *DatabaseTaskStore) Resume(ctx context.Context, c task.ResumeCommand) error {
	at := time.Now().UTC()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	r, e := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='queued',available_at=$1,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$1 WHERE task_id=$2 AND owner_id=$3 AND status='waiting_user' AND revision=$4 AND COALESCE(spec_json->>'kind','') <> 'execution_stage'`, at, c.TaskID, c.OwnerID, c.ExpectedRevision)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return task.ErrRevisionConflict
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'resumed','queued','{}',$1 FROM agent_tasks WHERE task_id=$2`, at, c.TaskID); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *DatabaseTaskStore) Complete(ctx context.Context, c task.CompleteCommand) (task.Task, error) {
	if c.Result.Validate() != nil {
		return task.Task{}, task.ErrInvalid
	}
	raw, _ := json.Marshal(c.Result)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, e
	}
	defer tx.Rollback()
	if executionStage, e := databaseTaskIsExecutionStageTx(ctx, tx, c.TaskID); e != nil {
		return task.Task{}, e
	} else if executionStage {
		return task.Task{}, task.ErrConflict
	}
	r, e := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='succeeded',result_json=$1,lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$2 WHERE task_id=$3 AND status='running' AND attempt=$4 AND lease_epoch=$5 AND revision=$6 AND lease_expires_at>$2`, raw, c.At.UTC(), c.TaskID, c.Attempt, c.LeaseEpoch, c.ExpectedRevision)
	if e != nil {
		return task.Task{}, e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return task.Task{}, task.ErrLeaseConflict
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'completed','succeeded',result_json,$1 FROM agent_tasks WHERE task_id=$2`, c.At.UTC(), c.TaskID)
	if e != nil {
		return task.Task{}, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true`, c.At.UTC()); e != nil {
		return task.Task{}, e
	}
	if e = terminalizeConfirmationsTx(ctx, tx, c.TaskID, "succeeded", c.At.UTC()); e != nil {
		return task.Task{}, e
	}
	if e = tx.Commit(); e != nil {
		return task.Task{}, e
	}
	return s.Get(ctx, "", c.TaskID)
}
func (s *DatabaseTaskStore) Fail(ctx context.Context, c task.FailCommand) (task.Task, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, e
	}
	defer tx.Rollback()
	if executionStage, e := databaseTaskIsExecutionStageTx(ctx, tx, c.TaskID); e != nil {
		return task.Task{}, e
	} else if executionStage {
		return task.Task{}, task.ErrConflict
	}
	r, e := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='failed',failure_code=$1,failure_summary=$2,lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$3 WHERE task_id=$4 AND status='running' AND attempt=$5 AND lease_epoch=$6 AND revision=$7 AND lease_expires_at>$3`, c.ErrorCode, c.ErrorSummary, c.At.UTC(), c.TaskID, c.Attempt, c.LeaseEpoch, c.ExpectedRevision)
	if e != nil {
		return task.Task{}, e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return task.Task{}, task.ErrLeaseConflict
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'failed','failed',jsonb_build_object('code',failure_code,'summary',failure_summary),$1 FROM agent_tasks WHERE task_id=$2`, c.At.UTC(), c.TaskID)
	if e != nil {
		return task.Task{}, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true`, c.At.UTC()); e != nil {
		return task.Task{}, e
	}
	if e = terminalizeConfirmationsTx(ctx, tx, c.TaskID, c.ErrorCode, c.At.UTC()); e != nil {
		return task.Task{}, e
	}
	if e = tx.Commit(); e != nil {
		return task.Task{}, e
	}
	return s.Get(ctx, "", c.TaskID)
}
func (s *DatabaseTaskStore) Cancel(ctx context.Context, c task.CancelCommand) (task.Task, error) {
	if s == nil || s.db == nil {
		return task.Task{}, task.ErrInvalid
	}
	c.OwnerID = strings.TrimSpace(c.OwnerID)
	taskID, err := normalizeDatabaseTaskUUID(c.TaskID)
	if err != nil {
		return task.Task{}, err
	}
	key, err := normalizeDatabaseTaskUUID(c.Mutation.IdempotencyKey)
	if err != nil || c.OwnerID == "" || c.Mutation.ExpectedRevision == 0 || c.At.IsZero() || c.At.Location() != time.UTC {
		return task.Task{}, task.ErrInvalid
	}
	if c.ExpectedRevision != 0 && c.ExpectedRevision != c.Mutation.ExpectedRevision {
		return task.Task{}, task.ErrConflict
	}
	c.TaskID = taskID
	c.ExpectedRevision = c.Mutation.ExpectedRevision
	c.Mutation.IdempotencyKey = key
	c.Mutation.RequestDigest = taskCancelRequestDigest(c)
	if c.Mutation.Validate() != nil {
		return task.Task{}, task.ErrInvalid
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, e
	}
	defer tx.Rollback()
	if executionStage, checkErr := databaseTaskIsExecutionStageTx(ctx, tx, c.TaskID); checkErr != nil {
		return task.Task{}, checkErr
	} else if executionStage {
		return task.Task{}, task.ErrConflict
	}
	if e = lockTaskReplayTx(ctx, tx, c.OwnerID, "cancel", c.Mutation.IdempotencyKey); e != nil {
		return task.Task{}, e
	}
	if replay, found, replayErr := readTaskReplayTx(ctx, tx, c.OwnerID, "cancel", c.Mutation.IdempotencyKey, c.Mutation.RequestDigest); found || replayErr != nil {
		return replay, replayErr
	}
	current, e := getDatabaseTaskTx(ctx, tx, c.OwnerID, c.TaskID, true)
	if e != nil {
		return task.Task{}, e
	}
	if current.Revision != c.Mutation.ExpectedRevision {
		return task.Task{}, task.ErrRevisionConflict
	}
	if current.Status == task.StatusSucceeded || current.Status == task.StatusFailed || current.Status == task.StatusCanceled {
		return task.Task{}, task.ErrTerminal
	}
	running := current.Status == task.StatusRunning
	if running && (current.Lease == nil || current.Lease.Epoch != current.LeaseEpoch) {
		return task.Task{}, task.ErrLeaseConflict
	}
	result, e := tx.ExecContext(ctx, `UPDATE agent_tasks
SET status='canceled',
    failure_code='canceled',
    failure_summary=$1,
    lease_epoch=CASE WHEN status='running' THEN lease_epoch+1 ELSE lease_epoch END,
    lease_holder='',
    lease_expires_at=NULL,
    revision=revision+1,
    progress_sequence=progress_sequence+1,
    updated_at=$2
WHERE task_id=$3 AND owner_id=$4 AND revision=$5 AND status=$6`, c.Reason, c.At.UTC(), c.TaskID, c.OwnerID, c.Mutation.ExpectedRevision, string(current.Status))
	if e != nil {
		return task.Task{}, e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return task.Task{}, task.ErrRevisionConflict
	}
	if running {
		result, e = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true`, c.At.UTC())
		if e != nil {
			return task.Task{}, e
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return task.Task{}, task.ErrConflict
		}
	}
	if e = terminalizeConfirmationsTx(ctx, tx, c.TaskID, "canceled", c.At.UTC()); e != nil {
		return task.Task{}, e
	}
	result, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'canceled','canceled',jsonb_build_object('reason',$2::text),$3 FROM agent_tasks WHERE task_id=$1 AND owner_id=$4`, c.TaskID, c.Reason, c.At.UTC(), c.OwnerID)
	if e != nil {
		return task.Task{}, e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return task.Task{}, task.ErrConflict
	}
	out, e := getDatabaseTaskTx(ctx, tx, c.OwnerID, c.TaskID, false)
	if e != nil {
		return task.Task{}, e
	}
	if e = saveTaskReplayTx(ctx, tx, c.OwnerID, "cancel", c.Mutation.IdempotencyKey, c.Mutation.RequestDigest, out, c.At.UTC()); e != nil {
		return task.Task{}, e
	}
	if e = tx.Commit(); e != nil {
		return task.Task{}, e
	}
	return out, nil
}
func (s *DatabaseTaskStore) Timeout(ctx context.Context, c task.TimeoutCommand) error {
	_, e := s.Fail(ctx, task.FailCommand{Fence: c.Fence, ErrorCode: "task_timed_out", ErrorSummary: "task timed out", At: c.At})
	return e
}
func (s *DatabaseTaskStore) Delete(ctx context.Context, c task.DeleteCommand) error {
	current, e := s.Get(ctx, c.OwnerID, c.TaskID)
	if e != nil {
		return e
	}
	if current.Spec.Kind == task.TaskKindExecutionStage {
		return task.ErrConflict
	}
	r, e := s.db.ExecContext(ctx, `UPDATE agent_tasks SET deleted_at=$1,revision=revision+1,updated_at=$1 WHERE task_id=$2 AND owner_id=$3 AND revision=$4 AND status IN ('succeeded','failed','canceled')`, c.At.UTC(), c.TaskID, c.OwnerID, c.ExpectedRevision)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return task.ErrRevisionConflict
	}
	return nil
}

// The methods below are the canonical task.Store adapter. The short methods
// above are retained for compatibility with the original storage draft.
func (s *DatabaseTaskStore) CreateTask(ctx context.Context, c task.CreateTaskCommand) (task.Task, error) {
	if s == nil || s.db == nil || strings.TrimSpace(c.OwnerID) == "" || c.Validate() != nil {
		return task.Task{}, task.ErrInvalid
	}
	if !task.ValidUUID(c.Mutation.IdempotencyKey) {
		return task.Task{}, task.ErrInvalid
	}
	if c.Spec.Kind == task.TaskKindExecutionStage {
		return task.Task{}, task.ErrConflict
	}
	at := time.Now().UTC()
	raw, _ := json.Marshal(c.Spec)
	id := deterministicDatabaseTaskID(c.OwnerID, c.Mutation.IdempotencyKey)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, e
	}
	defer tx.Rollback()
	if e = lockTaskReplayTx(ctx, tx, c.OwnerID, "create", c.Mutation.IdempotencyKey); e != nil {
		return task.Task{}, e
	}
	if replay, found, replayErr := readTaskReplayTx(ctx, tx, c.OwnerID, "create", c.Mutation.IdempotencyKey, c.Mutation.RequestDigest); found || replayErr != nil {
		return replay, replayErr
	}
	result, e := tx.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,$5,$5) ON CONFLICT(owner_id,(spec_json->>'idempotency_key')) DO NOTHING`, id, c.OwnerID, raw, c.Spec.AvailableAt.UTC(), at)
	if e != nil {
		return task.Task{}, e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return task.Task{}, task.ErrConflict
	}
	out, e := getDatabaseTaskTx(ctx, tx, c.OwnerID, id, false)
	if e != nil {
		return task.Task{}, e
	}
	if out.Status != task.StatusQueued || out.Validate() != nil {
		return task.Task{}, task.ErrConflict
	}
	if e = saveTaskReplayTx(ctx, tx, c.OwnerID, "create", c.Mutation.IdempotencyKey, c.Mutation.RequestDigest, out, at); e != nil {
		return task.Task{}, e
	}
	if e = tx.Commit(); e != nil {
		return task.Task{}, e
	}
	return out, nil
}

func oldTaskID(raw []byte) string {
	var v struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.TaskID
}

func (s *DatabaseTaskStore) LookupMutation(ctx context.Context, owner, operation string) (task.MutationRecord, error) {
	var r task.MutationRecord
	var key string
	err := s.db.QueryRowContext(ctx, `SELECT operation,idempotency_key::text,request_digest,response_json,created_at FROM agent_task_replays WHERE owner_id=$1 AND operation=$2 ORDER BY created_at DESC LIMIT 1`, owner, operation).Scan(&r.Operation, &key, &r.Digest, &r.Response, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, task.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.IdempotencyKey, r.CreatedAt = key, r.CreatedAt.UTC()
	return r, nil
}

func (s *DatabaseTaskStore) CommitMutation(ctx context.Context, r task.MutationRecord) (task.MutationRecord, error) {
	if r.Validate() != nil {
		return task.MutationRecord{}, task.ErrInvalid
	}
	id, err := uuid.Parse(r.IdempotencyKey)
	if err != nil {
		return task.MutationRecord{}, task.ErrInvalid
	}
	owner, kind := "", ""
	if err = s.db.QueryRowContext(ctx, `SELECT owner_id,COALESCE(spec_json->>'kind','') FROM agent_tasks WHERE task_id=$1`, oldTaskID(r.Response)).Scan(&owner, &kind); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return task.MutationRecord{}, err
	}
	if strings.TrimSpace(kind) == string(task.TaskKindExecutionStage) {
		return task.MutationRecord{}, task.ErrConflict
	}
	if owner == "" {
		return r, nil
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_task_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(owner_id,operation,idempotency_key) DO UPDATE SET request_digest=EXCLUDED.request_digest,response_json=EXCLUDED.response_json`, owner, r.Operation, id, r.Digest, r.Response, r.CreatedAt.UTC())
	return r, err
}
func (s *DatabaseTaskStore) GetTask(ctx context.Context, id string) (task.Task, error) {
	return s.Get(ctx, "", id)
}
func (s *DatabaseTaskStore) ListTasks(ctx context.Context, q task.TaskListQuery) ([]task.Task, string, error) {
	v, e := s.List(ctx, q)
	return v, "", e
}
func (s *DatabaseTaskStore) DeleteTask(ctx context.Context, c task.DeleteTaskCommand) (task.DeletedTaskResponse, error) {
	cur, e := s.GetTask(ctx, c.TaskID)
	if e != nil {
		return task.DeletedTaskResponse{}, e
	}
	if e = s.Delete(ctx, task.DeleteCommand{TaskID: c.TaskID, OwnerID: cur.OwnerID, ExpectedRevision: c.Mutation.ExpectedRevision, At: c.At}); e != nil {
		return task.DeletedTaskResponse{}, e
	}
	v, e := s.Get(ctx, cur.OwnerID, c.TaskID)
	if e != nil {
		return task.DeletedTaskResponse{}, e
	}
	return task.DeletedTaskResponse{TaskID: v.ID, DeletedAt: *v.DeletedAt, Revision: v.Revision, Tombstone: true}, nil
}
func (s *DatabaseTaskStore) ClaimTask(ctx context.Context, c task.ClaimCommand) (task.Task, task.Lease, error) {
	return s.Claim(ctx, c)
}
func (s *DatabaseTaskStore) ReclaimTask(ctx context.Context, c task.ReclaimCommand) (task.Task, task.Lease, error) {
	return s.Reclaim(ctx, c)
}
func (s *DatabaseTaskStore) RenewLease(ctx context.Context, c task.RenewLeaseCommand) (task.Lease, error) {
	return s.Renew(ctx, c)
}
func (s *DatabaseTaskStore) AppendProgressCanonical(ctx context.Context, c task.ProgressCommand) (task.Task, task.Progress, error) {
	return s.AppendProgress(ctx, c)
}
func (s *DatabaseTaskStore) ListProgress(ctx context.Context, id string, after uint64, limit int) ([]task.Progress, string, error) {
	ev, e := s.Events(ctx, "", id, after, limit)
	if e != nil {
		return nil, "", e
	}
	out := make([]task.Progress, 0, len(ev))
	for _, x := range ev {
		var p task.Progress
		_ = json.Unmarshal(x.Payload, &p)
		p.TaskID = id
		p.Sequence = x.Sequence
		p.At = x.At
		p.Status = x.Status
		out = append(out, p)
	}
	return out, "", nil
}
func (s *DatabaseTaskStore) WaitTask(ctx context.Context, c task.WaitUserCommand) error {
	return s.WaitUser(ctx, c)
}
func (s *DatabaseTaskStore) ResumeTask(ctx context.Context, c task.ResumeCommand) error {
	return s.Resume(ctx, c)
}
func (s *DatabaseTaskStore) CompleteTask(ctx context.Context, c task.CompleteCommand) (task.Task, error) {
	return s.Complete(ctx, c)
}
func (s *DatabaseTaskStore) CancelTask(ctx context.Context, c task.CancelCommand) (task.Task, error) {
	return s.Cancel(ctx, c)
}
func (s *DatabaseTaskStore) TimeoutTask(ctx context.Context, c task.TimeoutCommand) error {
	return s.Timeout(ctx, c)
}
func (s *DatabaseTaskStore) FailTask(ctx context.Context, c task.FailCommand) error {
	_, e := s.Fail(ctx, c)
	return e
}
func (s *DatabaseTaskStore) RetryTask(ctx context.Context, c task.RetryCommand) (task.Task, error) {
	if s == nil || s.db == nil {
		return task.Task{}, task.ErrInvalid
	}
	taskID, err := normalizeDatabaseTaskUUID(c.TaskID)
	if err != nil {
		return task.Task{}, err
	}
	key, err := normalizeDatabaseTaskUUID(c.Mutation.IdempotencyKey)
	if err != nil || c.Mutation.ExpectedRevision == 0 || c.At.IsZero() || c.At.Location() != time.UTC {
		return task.Task{}, task.ErrInvalid
	}
	c.TaskID = taskID
	c.Mutation.IdempotencyKey = key
	c.Mutation.RequestDigest = taskRetryRequestDigest(c)
	if c.Validate() != nil {
		return task.Task{}, task.ErrInvalid
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, e
	}
	defer tx.Rollback()
	owner, e := taskOwnerTx(ctx, tx, c.TaskID)
	if e != nil {
		return task.Task{}, e
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return task.Task{}, task.ErrInvalid
	}
	if executionStage, checkErr := databaseTaskIsExecutionStageTx(ctx, tx, c.TaskID); checkErr != nil {
		return task.Task{}, checkErr
	} else if executionStage {
		return task.Task{}, task.ErrConflict
	}
	if e = lockTaskReplayTx(ctx, tx, owner, "retry", c.Mutation.IdempotencyKey); e != nil {
		return task.Task{}, e
	}
	if replay, found, replayErr := readTaskReplayTx(ctx, tx, owner, "retry", c.Mutation.IdempotencyKey, c.Mutation.RequestDigest); found || replayErr != nil {
		return replay, replayErr
	}
	original, e := getDatabaseTaskTx(ctx, tx, owner, c.TaskID, true)
	if e != nil {
		return task.Task{}, e
	}
	if original.DeletedAt != nil {
		return task.Task{}, task.ErrNotFound
	}
	next, e := task.RetryTask(original, task.RetryRequest{
		TaskID:           c.TaskID,
		IdempotencyKey:   c.Mutation.IdempotencyKey,
		RequestDigest:    c.Mutation.RequestDigest,
		ExpectedRevision: c.Mutation.ExpectedRevision,
		At:               c.At.UTC(),
	})
	if e != nil {
		return task.Task{}, e
	}
	raw, e := json.Marshal(next.Spec)
	if e != nil {
		return task.Task{}, e
	}
	result, e := tx.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,lease_epoch,revision,progress_sequence,available_at,retry_of_task_id,created_at,updated_at) VALUES($1,$2,$3,'queued',0,0,1,1,$4,$5,$6,$6)`, next.ID, owner, raw, next.AvailableAt.UTC(), original.ID, c.At.UTC())
	if e != nil {
		return task.Task{}, e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return task.Task{}, task.ErrConflict
	}
	result, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) VALUES($1,$2,1,'created','queued',jsonb_build_object('retry_of_task_id',$3::uuid),$4)`, owner, next.ID, original.ID, c.At.UTC())
	if e != nil {
		return task.Task{}, e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return task.Task{}, task.ErrConflict
	}
	out, e := getDatabaseTaskTx(ctx, tx, owner, next.ID, false)
	if e != nil {
		return task.Task{}, e
	}
	if e = saveTaskReplayTx(ctx, tx, owner, "retry", c.Mutation.IdempotencyKey, c.Mutation.RequestDigest, out, c.At.UTC()); e != nil {
		return task.Task{}, e
	}
	if e = tx.Commit(); e != nil {
		return task.Task{}, e
	}
	return out, nil
}
func (s *DatabaseTaskStore) WatchProgress(ctx context.Context, id string, after uint64) (<-chan task.Progress, error) {
	ch := make(chan task.Progress)
	go func() {
		defer close(ch)
		for {
			v, _, e := s.ListProgress(ctx, id, after, 200)
			if e != nil {
				return
			}
			for _, p := range v {
				select {
				case ch <- p:
					after = p.Sequence
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return ch, nil
}

// ClaimNextDue is the worker queue primitive. It serializes the concurrency
// slot row, selects one FIFO due item with SKIP LOCKED, and fences its lease.
func (s *DatabaseTaskStore) ClaimNextDue(ctx context.Context, holder string, at time.Time, ttl time.Duration, max int) (task.Task, task.Lease, error) {
	if strings.TrimSpace(holder) == "" || ttl <= 0 || max <= 0 || at.IsZero() {
		return task.Task{}, task.Lease{}, task.ErrInvalid
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return task.Task{}, task.Lease{}, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_runtime_concurrency(singleton,running_count,max_concurrent,revision,updated_at) VALUES(true,0,$1,1,$2) ON CONFLICT(singleton) DO UPDATE SET max_concurrent=EXCLUDED.max_concurrent`, max, at.UTC()); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	var running int
	if e = tx.QueryRowContext(ctx, `SELECT running_count FROM agent_task_runtime_concurrency WHERE singleton=true FOR UPDATE`).Scan(&running); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if running >= max {
		return task.Task{}, task.Lease{}, task.ErrConflict
	}
	var id string
	var rev, epoch int64
	e = tx.QueryRowContext(ctx, `SELECT task_id::text,revision,lease_epoch FROM agent_tasks WHERE status='queued' AND available_at<=$1 AND deleted_at IS NULL AND COALESCE(spec_json->>'kind','') <> 'execution_stage' ORDER BY available_at,created_at,task_id FOR UPDATE SKIP LOCKED LIMIT 1`, at.UTC()).Scan(&id, &rev, &epoch)
	if errors.Is(e, sql.ErrNoRows) {
		return task.Task{}, task.Lease{}, task.ErrNotFound
	}
	if e != nil {
		return task.Task{}, task.Lease{}, e
	}
	until := at.UTC().Add(ttl)
	if _, e = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='running',attempt=GREATEST(attempt,1),lease_epoch=lease_epoch+1,lease_holder=$2,lease_expires_at=$3,execution_started_at=COALESCE(execution_started_at,$4),revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$4 WHERE task_id=$1`, id, holder, until, at.UTC()); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=running_count+1,revision=revision+1,updated_at=$1 WHERE singleton=true`, at.UTC()); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'claimed','running',jsonb_build_object('holder',$2::text),$3 FROM agent_tasks WHERE task_id=$1`, id, holder, at.UTC()); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	if e = tx.Commit(); e != nil {
		return task.Task{}, task.Lease{}, e
	}
	v, e := s.GetTask(ctx, id)
	if e != nil || v.Lease == nil {
		return task.Task{}, task.Lease{}, e
	}
	return v, *v.Lease, nil
}

// ReclaimExpired returns one abandoned running task to the durable queue.
// The running slot is repaired in the same transaction. ClaimNextDue then
// assigns the successor lease epoch before any executor can observe it;
// attempt remains the stable logical execution identity across restarts.
func (s *DatabaseTaskStore) ReclaimExpired(ctx context.Context, holder string, at time.Time, ttl time.Duration, max int) error {
	if strings.TrimSpace(holder) == "" || ttl <= 0 || max <= 0 || at.IsZero() {
		return task.ErrInvalid
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var id string
	var epoch int64
	var previousHolder string
	e = tx.QueryRowContext(ctx, `SELECT task_id::text,lease_epoch,lease_holder FROM agent_tasks WHERE status='running' AND lease_expires_at<=$1 AND deleted_at IS NULL AND COALESCE(spec_json->>'kind','') <> 'execution_stage' ORDER BY lease_expires_at,task_id FOR UPDATE SKIP LOCKED LIMIT 1`, at.UTC()).Scan(&id, &epoch, &previousHolder)
	if errors.Is(e, sql.ErrNoRows) {
		return task.ErrNotFound
	}
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_runtime_concurrency(singleton,running_count,max_concurrent,revision,updated_at) VALUES(true,0,$1,1,$2) ON CONFLICT(singleton) DO UPDATE SET max_concurrent=EXCLUDED.max_concurrent`, max, at.UTC()); e != nil {
		return e
	}
	res, e := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='queued',available_at=$2,lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$2 WHERE task_id=$1 AND status='running' AND lease_epoch=$3 AND lease_expires_at<=$2`, id, at.UTC(), epoch)
	if e != nil {
		return e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return task.ErrLeaseConflict
	}
	if _, e = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true`, at.UTC()); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) SELECT owner_id,task_id,progress_sequence,'reclaimed','queued',jsonb_build_object('previous_holder',$2::text,'next_holder',$3::text,'previous_lease_epoch',$4::bigint),$5 FROM agent_tasks WHERE task_id=$1`, id, previousHolder, holder, epoch, at.UTC()); e != nil {
		return e
	}
	return tx.Commit()
}

var _ task.Store = (*DatabaseTaskStore)(nil)
var _ task.TaskQueueRepository = (*DatabaseTaskStore)(nil)
