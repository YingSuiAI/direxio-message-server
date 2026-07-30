package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
	"strings"
	"time"
)

func ptrTime(value time.Time) *time.Time { return &value }

// ensureScheduleRunLink repairs the nullable v102 links left by older rows,
// while rejecting a row that belongs to a different deterministic identity.
func ensureScheduleRunLink(ctx context.Context, tx *sql.Tx, owner, scheduleID string, scheduledFor time.Time, status string, startedAt *time.Time, runID, occurrenceID, taskID string) error {
	var existingRun string
	var existingOccurrence, existingTask sql.NullString
	query := `SELECT run_id,occurrence_id::text,task_id::text FROM p2p_agent_schedule_runs WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3 FOR UPDATE`
	err := tx.QueryRowContext(ctx, query, owner, scheduleID, scheduledFor).Scan(&existingRun, &existingOccurrence, &existingTask)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_schedule_runs(run_id,schedule_id,owner_id,status,scheduled_for,started_at,result,error,lease_epoch,occurrence_id,task_id) VALUES($1,$2,$3,$4,$5,$6,'','',0,$8::uuid,$7::uuid) ON CONFLICT(owner_id,schedule_id,scheduled_for) DO NOTHING`, runID, scheduleID, owner, status, scheduledFor, startedAt, taskID, occurrenceID); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, query, owner, scheduleID, scheduledFor).Scan(&existingRun, &existingOccurrence, &existingTask)
	}
	if err != nil {
		return err
	}
	if existingRun != runID || (existingOccurrence.Valid && existingOccurrence.String != occurrenceID) || (existingTask.Valid && existingTask.String != taskID) {
		return task.ErrConflict
	}
	if existingOccurrence.Valid && existingTask.Valid {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE p2p_agent_schedule_runs SET occurrence_id=COALESCE(p2p_agent_schedule_runs.occurrence_id,$4::uuid),task_id=COALESCE(p2p_agent_schedule_runs.task_id,$5::uuid) WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, owner, scheduleID, scheduledFor, occurrenceID, taskID)
	return err
}

// ensureScheduleOccurrenceLink validates an existing occurrence or creates
// the deterministic projection after the linked task/run are in place.
func ensureScheduleOccurrenceLink(ctx context.Context, tx *sql.Tx, owner, scheduleID string, scheduledFor time.Time, occurrenceID, taskID, runID string, createdAt time.Time) error {
	var existingOccurrence, existingTask, existingRun string
	query := `SELECT occurrence_id::text,task_id::text,run_id::text FROM agent_schedule_occurrences WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3 FOR UPDATE`
	err := tx.QueryRowContext(ctx, query, owner, scheduleID, scheduledFor).Scan(&existingOccurrence, &existingTask, &existingRun)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_schedule_occurrences(occurrence_id,schedule_id,owner_id,scheduled_for,task_id,run_id,created_at) VALUES($1::uuid,$2,$3,$4,$5::uuid,$6::uuid,$7) ON CONFLICT(owner_id,schedule_id,scheduled_for) DO NOTHING`, occurrenceID, scheduleID, owner, scheduledFor, taskID, runID, createdAt); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, query, owner, scheduleID, scheduledFor).Scan(&existingOccurrence, &existingTask, &existingRun)
	}
	if err != nil {
		return err
	}
	if existingOccurrence != occurrenceID || existingTask != taskID || existingRun != runID {
		return task.ErrConflict
	}
	return nil
}

// MaterializeScheduleTask atomically links one legacy schedule run to one
// generic task. The deterministic IDs make retries and multiple workers safe.
func (s *DatabaseStore) MaterializeScheduleTask(ctx context.Context, owner, scheduleID string, at time.Time) (string, string, error) {
	if s == nil || s.db == nil || owner == "" || scheduleID == "" || at.IsZero() {
		return "", "", task.ErrInvalid
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return "", "", e
	}
	defer tx.Rollback()
	var tpl []byte
	var rev int64
	var prompt, profile string
	var template []byte
	e = tx.QueryRowContext(ctx, `SELECT prompt,model_profile_id,task_template,revision FROM p2p_agent_schedules WHERE owner_id=$1 AND schedule_id=$2 AND deleted_at IS NULL FOR UPDATE`, owner, scheduleID).Scan(&prompt, &profile, &template, &rev)
	if e != nil {
		return "", "", e
	}
	occ := uuid.NewSHA1(uuid.Nil, []byte(owner+"\x00"+scheduleID+"\x00scheduled\x00"+at.UTC().Format(time.RFC3339Nano))).String()
	tid := uuid.NewSHA1(uuid.Nil, []byte(occ+"\x00task")).String()
	runID := occ
	var existingOccurrence, existingTask, existingRun string
	if e = tx.QueryRowContext(ctx, `SELECT occurrence_id::text,task_id::text,run_id::text FROM agent_schedule_occurrences WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3 FOR UPDATE`, owner, scheduleID, at.UTC()).Scan(&existingOccurrence, &existingTask, &existingRun); e == nil {
		if existingOccurrence != occ || existingTask != tid || existingRun != runID {
			return "", "", task.ErrConflict
		}
		if e = ensureScheduleRunLink(ctx, tx, owner, scheduleID, at.UTC(), "running", ptrTime(at.UTC()), runID, occ, tid); e != nil {
			return "", "", e
		}
		if e = tx.Commit(); e != nil {
			return "", "", e
		}
		return occ, tid, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return "", "", e
	}
	templateSpec := task.TaskTemplate{Kind: task.TaskKindAgent, Goal: prompt, ModelProfileID: profile}
	if len(template) > 2 && string(template) != "{}" && json.Unmarshal(template, &templateSpec) != nil {
		return "", "", task.ErrInvalid
	}
	materialized, e := templateSpec.Materialize(uuid.NewSHA1(uuid.Nil, []byte(occ+"\x00idempotency")).String(), at.UTC())
	if e != nil {
		return "", "", e
	}
	tpl, _ = json.Marshal(materialized)
	_, e = tx.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,$4,$4) ON CONFLICT(task_id) DO NOTHING`, tid, owner, tpl, at.UTC())
	if e != nil {
		return "", "", e
	}
	if e = ensureScheduleRunLink(ctx, tx, owner, scheduleID, at.UTC(), "running", ptrTime(at.UTC()), runID, occ, tid); e != nil {
		return "", "", e
	}
	if e = ensureScheduleOccurrenceLink(ctx, tx, owner, scheduleID, at.UTC(), occ, tid, runID, at.UTC()); e != nil {
		return "", "", e
	}
	_, e = tx.ExecContext(ctx, `UPDATE p2p_agent_schedules SET latest_run_at=$1,revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND schedule_id=$3 AND revision=$4`, at.UTC(), owner, scheduleID, rev)
	if e != nil {
		return "", "", e
	}
	if e = tx.Commit(); e != nil {
		return "", "", e
	}
	return occ, tid, nil
}

// TriggerSchedule is the embedded runtime's idempotent run-now port.
func (s *DatabaseStore) TriggerSchedule(ctx context.Context, owner, scheduleID, key string) (Schedule, string, string, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(scheduleID) == "" {
		return Schedule{}, "", "", task.ErrInvalid
	}
	key = strings.TrimSpace(key)
	if !task.ValidUUID(key) {
		return Schedule{}, "", "", task.ErrInvalid
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Schedule{}, "", "", e
	}
	defer tx.Rollback()
	var v Schedule
	var template []byte
	var deleted *time.Time
	e = tx.QueryRowContext(ctx, `SELECT schedule_id,owner_id,name,prompt,trigger_kind,trigger_value,timezone,skip_if_running,status,revision,model_profile_id,model_profile_revision,credential_version,next_run_at,latest_run_at,lease_owner,lease_until,lease_epoch,task_template,created_at,updated_at,deleted_at FROM p2p_agent_schedules WHERE owner_id=$1 AND schedule_id=$2 FOR UPDATE`, owner, scheduleID).Scan(&v.ScheduleID, &v.OwnerID, &v.Name, &v.Prompt, &v.TriggerKind, &v.TriggerValue, &v.Timezone, &v.SkipIfRunning, &v.Status, &v.Revision, &v.ModelProfileID, &v.ModelProfileRevision, &v.CredentialVersion, &v.NextRunAt, &v.LatestRunAt, &v.LeaseOwner, &v.LeaseUntil, &v.LeaseEpoch, &template, &v.CreatedAt, &v.UpdatedAt, &deleted)
	if errors.Is(e, sql.ErrNoRows) || deleted != nil {
		return Schedule{}, "", "", ErrScheduleNotFound
	}
	if e != nil {
		return Schedule{}, "", "", e
	}
	// Keep the legacy run_now and Core trigger projections on the same
	// deterministic occurrence. Both APIs pass the same owner/schedule/key
	// into this transaction and receive links to this one generic task.
	occ := uuid.NewSHA1(uuid.Nil, []byte(owner+"\x00agent.schedules.run_now\x00"+key)).String()
	tid := uuid.NewSHA1(uuid.Nil, []byte(occ+"\x00task")).String()
	var existingOccurrence, existingTask, existingRun string
	var existingScheduled time.Time
	if e = tx.QueryRowContext(ctx, `SELECT occurrence_id::text,task_id::text,run_id::text,scheduled_for FROM agent_schedule_occurrences WHERE owner_id=$1 AND occurrence_id=$2 FOR UPDATE`, owner, occ).Scan(&existingOccurrence, &existingTask, &existingRun, &existingScheduled); e == nil {
		if existingOccurrence != occ || existingTask != tid || existingRun != occ {
			return Schedule{}, "", "", task.ErrConflict
		}
		if e = ensureScheduleRunLink(ctx, tx, owner, scheduleID, existingScheduled.UTC(), "queued", nil, occ, occ, tid); e != nil {
			return Schedule{}, "", "", e
		}
		if e = tx.Commit(); e != nil {
			return Schedule{}, "", "", e
		}
		return v, occ, existingTask, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return Schedule{}, "", "", e
	}
	at := time.Now().UTC()
	tpl := task.TaskTemplate{Kind: task.TaskKindAgent, Goal: v.Prompt, ModelProfileID: v.ModelProfileID}
	if len(template) > 2 && string(template) != "{}" && json.Unmarshal(template, &tpl) != nil {
		return Schedule{}, "", "", task.ErrInvalid
	}
	materialized, e := tpl.Materialize(key, at)
	if e != nil {
		return Schedule{}, "", "", e
	}
	specRaw, _ := json.Marshal(materialized)
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,$5,$5) ON CONFLICT(task_id) DO NOTHING`, tid, owner, specRaw, at, at); e != nil {
		return Schedule{}, "", "", e
	}
	if e = ensureScheduleRunLink(ctx, tx, owner, scheduleID, at.UTC(), "queued", nil, occ, occ, tid); e != nil {
		return Schedule{}, "", "", e
	}
	if e = ensureScheduleOccurrenceLink(ctx, tx, owner, scheduleID, at.UTC(), occ, tid, occ, at.UTC()); e != nil {
		return Schedule{}, "", "", e
	}
	r, e := tx.ExecContext(ctx, `UPDATE p2p_agent_schedules SET latest_run_at=$1,revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND schedule_id=$3 AND revision=$4`, at, owner, scheduleID, v.Revision)
	if e != nil {
		return Schedule{}, "", "", e
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return Schedule{}, "", "", ErrScheduleConflict
	}
	if e = tx.Commit(); e != nil {
		return Schedule{}, "", "", e
	}
	v.LatestRunAt, v.UpdatedAt = &at, at
	v.Revision++
	return v, occ, tid, nil
}

// MaterializeNextDue atomically links a due legacy schedule to the generic
// task/occurrence projection, then advances the schedule cursor.
func (s *DatabaseStore) MaterializeNextDue(ctx context.Context, at time.Time, calc task.CronCalculator) (bool, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	var owner, id, prompt, profile, kind, val, tz string
	var template []byte
	var next time.Time
	var rev int64
	e = tx.QueryRowContext(ctx, `SELECT owner_id,schedule_id,prompt,model_profile_id,trigger_kind,trigger_value,timezone,task_template,next_run_at,revision FROM p2p_agent_schedules WHERE deleted_at IS NULL AND status IN ('enabled','active') AND next_run_at IS NOT NULL AND next_run_at<=$1 ORDER BY next_run_at,schedule_id FOR UPDATE SKIP LOCKED LIMIT 1`, at.UTC()).Scan(&owner, &id, &prompt, &profile, &kind, &val, &tz, &template, &next, &rev)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	scheduled := next.UTC()
	occ := uuid.NewSHA1(uuid.Nil, []byte(owner+"\x00"+id+"\x00scheduled\x00"+scheduled.Format(time.RFC3339Nano))).String()
	tid := uuid.NewSHA1(uuid.Nil, []byte(occ+"\x00task")).String()
	templateSpec := task.TaskTemplate{Kind: task.TaskKindAgent, Goal: prompt, ModelProfileID: profile}
	if len(template) > 2 && string(template) != "{}" && json.Unmarshal(template, &templateSpec) != nil {
		return false, task.ErrInvalid
	}
	materialized, e := templateSpec.Materialize(uuid.NewSHA1(uuid.Nil, []byte(occ+"\x00idempotency")).String(), scheduled)
	if e != nil {
		return false, e
	}
	tpl, _ := json.Marshal(materialized)
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,$5,$5) ON CONFLICT(task_id) DO NOTHING`, tid, owner, tpl, scheduled, at.UTC()); e != nil {
		return false, e
	}
	if e = ensureScheduleRunLink(ctx, tx, owner, id, scheduled, "queued", nil, occ, occ, tid); e != nil {
		return false, e
	}
	if e = ensureScheduleOccurrenceLink(ctx, tx, owner, id, scheduled, occ, tid, occ, at.UTC()); e != nil {
		return false, e
	}
	var following *time.Time
	if kind != "one_time" {
		v, ce := calc.Next(scheduled, val, tz)
		if ce != nil {
			return false, ce
		}
		following = &v
	}
	if _, e = tx.ExecContext(ctx, `UPDATE p2p_agent_schedules SET next_run_at=$1,latest_run_at=$2,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND schedule_id=$4 AND revision=$5`, following, scheduled, owner, id, rev); e != nil {
		return false, e
	}
	if e = tx.Commit(); e != nil {
		return false, e
	}
	return true, nil
}

var _ interface {
	MaterializeNextDue(context.Context, time.Time, task.CronCalculator) (bool, error)
} = (*DatabaseStore)(nil)
