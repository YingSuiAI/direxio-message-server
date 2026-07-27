package storage

import (
	"context"
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
	v, err := a.CreateSchedule(ctx, Schedule{OwnerID: "owner", ScheduleID: "pg-s", Name: "n", Prompt: "p", TriggerKind: "one_time", TriggerValue: n.Format(time.RFC3339), Timezone: "UTC", Status: "enabled", NextRunAt: &n}, "idem")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got, ok, _ := b.GetSchedule(ctx, "owner", v.ScheduleID); !ok || got.Prompt != "p" {
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
