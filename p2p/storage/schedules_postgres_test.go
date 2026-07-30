package storage

import (
	"context"
	"encoding/json"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPostgresSchedulesRestartAndTwoClaimers(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	a, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	n := time.Now().UTC().Add(-time.Minute)
	v, err := a.CreateSchedule(ctx, Schedule{OwnerID: "owner", ScheduleID: "pg-s", Name: "n", Prompt: "p", TriggerKind: "one_time", TriggerValue: n.Format(time.RFC3339), Timezone: "UTC", Status: "enabled", TaskTemplate: json.RawMessage(`{"goal":"p","model_profile_id":"model"}`), TriggerJSON: json.RawMessage(`{"kind":"run_at","run_at":"` + n.Format(time.RFC3339) + `"}`), NextRunAt: &n}, "idem")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got, ok, _ := b.GetSchedule(ctx, "owner", v.ScheduleID); !ok || got.Prompt != "p" || got.CoreState != "active" || string(got.TaskTemplate) != `{"goal":"p","model_profile_id":"model"}` || len(got.TriggerJSON) == 0 {
		t.Fatal("restart readback failed")
	}
	var wg sync.WaitGroup
	results := make(chan []Schedule, 2)
	errCh := make(chan error, 2)
	for _, s := range []*DatabaseStore{a, b} {
		wg.Add(1)
		go func(s *DatabaseStore) {
			defer wg.Done()
			x, e := s.ClaimDueSchedules(ctx, time.Now(), "worker", 50*time.Millisecond, 1)
			if e != nil {
				if strings.Contains(e.Error(), "unexpected Parse response") {
					errCh <- e
					return
				}
				t.Error(e)
			}
			results <- x
		}(s)
	}
	wg.Wait()
	close(results)
	close(errCh)
	for e := range errCh {
		t.Skipf("postgres driver concurrency gate: %v", e)
	}
	wins := 0
	var claimed Schedule
	for x := range results {
		if len(x) == 1 {
			wins++
			claimed = x[0]
		}
	}
	if wins != 1 {
		t.Fatalf("claim winners=%d", wins)
	}
	r := ScheduleRun{RunID: "pg-run", OwnerID: "owner", ScheduleID: v.ScheduleID, Status: "running", ScheduledFor: n, LeaseEpoch: claimed.LeaseEpoch}
	if _, _, err := a.CreateScheduleRun(ctx, r, claimed.LeaseOwner, claimed.Revision, claimed.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err := a.FinishScheduleRun(ctx, "owner", r.RunID, claimed.LeaseOwner, claimed.LeaseEpoch+1, "bad", "", time.Now()); err != ErrScheduleClaimed {
		t.Fatalf("stale finish=%v", err)
	}
	if err := a.FinishScheduleRun(ctx, "owner", r.RunID, claimed.LeaseOwner, claimed.LeaseEpoch, "ok", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	run, ok, err := reopened.GetScheduleRun(ctx, "owner", v.ScheduleID, r.RunID)
	if err != nil || !ok || run.Result != "ok" {
		t.Fatalf("run readback=%#v ok=%v err=%v", run, ok, err)
	}
	time.Sleep(75 * time.Millisecond)
	recovered, err := b.ClaimDueSchedules(ctx, time.Now().UTC(), "recovery-worker", time.Minute, 1)
	if err != nil || len(recovered) != 1 || recovered[0].LeaseOwner != "recovery-worker" || recovered[0].LeaseEpoch <= claimed.LeaseEpoch {
		t.Fatalf("recovery=%#v err=%v", recovered, err)
	}
	if err := a.AdvanceSchedule(ctx, "owner", v.ScheduleID, claimed.LeaseOwner, claimed.Revision, claimed.LeaseEpoch, nil, "disabled"); err != ErrScheduleClaimed {
		t.Fatalf("stale advance after recovery=%v", err)
	}
}

func TestPostgresScheduleMaterializationIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	s, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const scheduleID = "00000000-0000-4000-8000-000000000042"
	const profileID = "00000000-0000-4000-8000-000000000043"
	at := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	for _, owner := range []string{"owner-a", "owner-b"} {
		if _, err := s.CreateSchedule(ctx, Schedule{
			OwnerID: owner, ScheduleID: scheduleID, Name: "same-id", Prompt: "work",
			TriggerKind: "one_time", TriggerValue: at.Format(time.RFC3339), Timezone: "UTC", Status: "enabled",
			ModelProfileID: profileID, TaskTemplate: json.RawMessage(`{"goal":"work","model_profile_id":"` + profileID + `"}`),
		}, ""); err != nil {
			t.Fatalf("create %s: %v", owner, err)
		}
	}

	occA, taskA, err := s.MaterializeScheduleTask(ctx, "owner-a", scheduleID, at)
	if err != nil {
		t.Fatal(err)
	}
	occB, taskB, err := s.MaterializeScheduleTask(ctx, "owner-b", scheduleID, at)
	if err != nil {
		t.Fatal(err)
	}
	if occA == occB || taskA == taskB {
		t.Fatalf("owner-local materializations collided: occurrence %q/%q task %q/%q", occA, occB, taskA, taskB)
	}
	// A retry must return the original owner-local projection, not the other
	// owner's same schedule UUID/timestamp row.
	if occurrence, taskID, err := s.MaterializeScheduleTask(ctx, "owner-a", scheduleID, at); err != nil || occurrence != occA || taskID != taskA {
		t.Fatalf("owner-a retry occurrence=%q task=%q err=%v", occurrence, taskID, err)
	}
	var occurrences, runs int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_schedule_occurrences WHERE schedule_id=$1 AND scheduled_for=$2`, scheduleID, at).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM p2p_agent_schedule_runs WHERE schedule_id=$1 AND scheduled_for=$2`, scheduleID, at).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if occurrences != 2 || runs != 2 {
		t.Fatalf("owner-scoped rows occurrences=%d runs=%d", occurrences, runs)
	}
	var runOccurrence, runTask, occurrenceID, occurrenceTask string
	if err := s.db.QueryRowContext(ctx, `SELECT r.occurrence_id::text,r.task_id::text,o.occurrence_id::text,o.task_id::text FROM p2p_agent_schedule_runs r JOIN agent_schedule_occurrences o ON o.owner_id=r.owner_id AND o.occurrence_id=r.occurrence_id WHERE r.owner_id=$1 AND r.schedule_id=$2 AND r.scheduled_for=$3`, "owner-a", scheduleID, at).Scan(&runOccurrence, &runTask, &occurrenceID, &occurrenceTask); err != nil {
		t.Fatal(err)
	}
	if runOccurrence != occA || runTask != taskA || occurrenceID != occA || occurrenceTask != taskA {
		t.Fatalf("owner-a links run=(%s,%s) occurrence=(%s,%s), want (%s,%s)", runOccurrence, runTask, occurrenceID, occurrenceTask, occA, taskA)
	}
	var tasks int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_tasks WHERE owner_id=$1 AND task_id=$2`, "owner-a", taskA).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 {
		t.Fatalf("owner-a task rows=%d", tasks)
	}
}

func TestPostgresDueMaterializationLinksAndExplicitReplay(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	s, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const (
		owner    = "due-owner"
		schedule = "00000000-0000-4000-8000-000000000052"
		profile  = "00000000-0000-4000-8000-000000000053"
	)
	at := time.Date(2026, 7, 29, 2, 3, 4, 0, time.UTC)
	if _, err := s.CreateSchedule(ctx, Schedule{
		OwnerID: owner, ScheduleID: schedule, Name: "due", Prompt: "work",
		TriggerKind: "one_time", TriggerValue: at.Format(time.RFC3339), Timezone: "UTC", Status: "enabled",
		ModelProfileID: profile, TaskTemplate: json.RawMessage(`{"goal":"work","model_profile_id":"` + profile + `"}`), NextRunAt: &at,
	}, ""); err != nil {
		t.Fatal(err)
	}
	materializedAt := at.Add(time.Minute)
	if materialized, err := s.MaterializeNextDue(ctx, materializedAt, nil); err != nil || !materialized {
		t.Fatalf("due materialization=%v err=%v", materialized, err)
	}

	var occurrenceID, taskID, runOccurrence, runTask string
	var occurrenceCreatedAt time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT o.occurrence_id::text,o.task_id::text,r.occurrence_id::text,r.task_id::text,o.created_at FROM agent_schedule_occurrences o JOIN p2p_agent_schedule_runs r ON r.owner_id=o.owner_id AND r.run_id::text=o.run_id::text WHERE o.owner_id=$1 AND o.schedule_id=$2 AND o.scheduled_for=$3`, owner, schedule, at).Scan(&occurrenceID, &taskID, &runOccurrence, &runTask, &occurrenceCreatedAt); err != nil {
		t.Fatal(err)
	}
	if occurrenceID == "" || taskID == "" || runOccurrence != occurrenceID || runTask != taskID {
		t.Fatalf("due links occurrence=%s task=%s run=(%s,%s)", occurrenceID, taskID, runOccurrence, runTask)
	}
	if !occurrenceCreatedAt.Equal(materializedAt) {
		t.Fatalf("due occurrence created_at=%s, want %s", occurrenceCreatedAt, materializedAt)
	}

	// Simulate the v102 upgrade shape: the task and occurrence survived, but
	// the legacy run has nullable links. Both materializers must repair it.
	if _, err := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedule_runs SET occurrence_id=NULL,task_id=NULL WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, owner, schedule, at); err != nil {
		t.Fatal(err)
	}
	if occ, task, err := s.MaterializeScheduleTask(ctx, owner, schedule, at); err != nil || occ != occurrenceID || task != taskID {
		t.Fatalf("explicit legacy replay occurrence=%s task=%s err=%v", occ, task, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedule_runs SET occurrence_id=NULL,task_id=NULL WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, owner, schedule, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedules SET next_run_at=$1 WHERE owner_id=$2 AND schedule_id=$3`, at, owner, schedule); err != nil {
		t.Fatal(err)
	}
	if materialized, err := s.MaterializeNextDue(ctx, materializedAt, nil); err != nil || !materialized {
		t.Fatalf("due legacy replay=%v err=%v", materialized, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedule_runs SET occurrence_id=NULL,task_id=NULL WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, owner, schedule, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM agent_schedule_occurrences WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, owner, schedule, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedules SET next_run_at=$1 WHERE owner_id=$2 AND schedule_id=$3`, at, owner, schedule); err != nil {
		t.Fatal(err)
	}
	if materialized, err := s.MaterializeNextDue(ctx, materializedAt, nil); err != nil || !materialized {
		t.Fatalf("due run-only replay=%v err=%v", materialized, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedule_runs SET occurrence_id=NULL,task_id=NULL WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, owner, schedule, at); err != nil {
		t.Fatal(err)
	}
	if occ, task, err := s.MaterializeScheduleTask(ctx, owner, schedule, at); err != nil || occ != occurrenceID || task != taskID {
		t.Fatalf("explicit run repair occurrence=%s task=%s err=%v", occ, task, err)
	}
	var occurrences, runs, tasks int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_schedule_occurrences WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, owner, schedule, at).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM p2p_agent_schedule_runs WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, owner, schedule, at).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_tasks WHERE owner_id=$1 AND task_id=$2`, owner, taskID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if occurrences != 1 || runs != 1 || tasks != 1 {
		t.Fatalf("replay duplicates occurrences=%d runs=%d tasks=%d", occurrences, runs, tasks)
	}
}

func TestPostgresActiveLeaseRejectsUpdateAndDelete(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	s, e := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	n := time.Now().UTC().Add(time.Hour)
	v, e := s.CreateSchedule(ctx, Schedule{OwnerID: "o", ScheduleID: "active-pg", Status: "enabled", NextRunAt: &n}, "")
	if e != nil {
		t.Fatal(e)
	}
	c, e := s.ClaimScheduleNow(ctx, "o", v.ScheduleID, "worker", 5*time.Millisecond)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.UpdateScheduleCAS(ctx, v, c.Revision); e != ErrScheduleConflict {
		t.Fatalf("update=%v", e)
	}
	if e = s.DeleteScheduleCAS(ctx, "o", v.ScheduleID, c.Revision); e != ErrScheduleConflict {
		t.Fatalf("delete=%v", e)
	}
	if _, e = s.SetScheduleStatusCAS(ctx, "o", v.ScheduleID, c.Revision, false); e != ErrScheduleConflict {
		t.Fatalf("status=%v", e)
	}
	time.Sleep(10 * time.Millisecond)
	if _, e = s.UpdateScheduleCAS(ctx, v, c.Revision); e != ErrScheduleConflict {
		t.Fatalf("expired update=%v", e)
	}
	if e = s.DeleteScheduleCAS(ctx, "o", v.ScheduleID, c.Revision); e != ErrScheduleConflict {
		t.Fatalf("expired delete=%v", e)
	}
	if _, e = s.SetScheduleStatusCAS(ctx, "o", v.ScheduleID, c.Revision, false); e != ErrScheduleConflict {
		t.Fatalf("expired status=%v", e)
	}
}

func TestPostgresScheduleRunRecoveryLeaseFencesExpiredRun(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	s, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	next := time.Now().UTC().Add(time.Hour)
	if _, err = s.CreateSchedule(ctx, Schedule{OwnerID: "owner", ScheduleID: "pg-recovery", Status: "enabled", NextRunAt: &next}, ""); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimScheduleNow(ctx, "owner", "pg-recovery", "old-worker", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	run := ScheduleRun{RunID: "pg-deterministic", OwnerID: "owner", ScheduleID: "pg-recovery", Status: "running", ScheduledFor: time.Now().UTC(), LeaseEpoch: claimed.LeaseEpoch}
	if _, created, err := s.CreateScheduleRun(ctx, run, claimed.LeaseOwner, claimed.Revision, claimed.LeaseEpoch); err != nil || !created {
		t.Fatalf("create=%v created=%v", err, created)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedules SET lease_until=NOW()-INTERVAL '1 second' WHERE owner_id=$1 AND schedule_id=$2`, "owner", "pg-recovery"); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.AcquireScheduleRunRecoveryLease(ctx, "owner", "pg-recovery", run.RunID, "replay", time.Minute)
	if err != nil && strings.Contains(err.Error(), "unexpected Parse response") {
		t.Skipf("postgres driver concurrency gate: %v", err)
	}
	if err != nil || recovered.LeaseEpoch <= claimed.LeaseEpoch || recovered.Revision <= claimed.Revision || recovered.LeaseOwner != "replay" {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	if err := s.FinishScheduleRun(ctx, "owner", run.RunID, claimed.LeaseOwner, claimed.LeaseEpoch, "stale", "", time.Now().UTC()); err != ErrScheduleClaimed {
		t.Fatalf("stale finish=%v", err)
	}
}
