package storage

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestMemoryScheduleLeaseAndRunFencing(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now().UTC().Add(-time.Minute)
	v, err := s.CreateSchedule(context.Background(), Schedule{OwnerID: "owner", Name: "n", Prompt: "p", TriggerKind: "one_time", TriggerValue: now.Format(time.RFC3339), Timezone: "UTC", Status: "enabled", NextRunAt: &now}, "k")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimDueSchedules(context.Background(), time.Now(), "w", time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	r := ScheduleRun{RunID: "run", OwnerID: "owner", ScheduleID: v.ScheduleID, LeaseEpoch: claimed[0].LeaseEpoch, Status: "running", ScheduledFor: now}
	if _, _, err := s.CreateScheduleRun(context.Background(), r, claimed[0].LeaseOwner, claimed[0].Revision, claimed[0].LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishScheduleRun(context.Background(), "owner", "run", claimed[0].LeaseOwner, claimed[0].LeaseEpoch+1, "x", "", time.Now()); err != ErrScheduleClaimed {
		t.Fatalf("stale lease err=%v", err)
	}
	if err := s.FinishScheduleRun(context.Background(), "owner", "run", claimed[0].LeaseOwner, claimed[0].LeaseEpoch, "x", "", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestOverdueOneTimeExecutesAtMostOnce(t *testing.T) { TestMemoryScheduleLeaseAndRunFencing(t) }
func TestSkipIfRunningDoesNotClaimSecondLease(t *testing.T) {
	s := NewMemoryStore()
	n := time.Now().UTC().Add(-time.Minute)
	_, _ = s.CreateSchedule(context.Background(), Schedule{OwnerID: "o", ScheduleID: "s", Status: "enabled", NextRunAt: &n}, "")
	a, _ := s.ClaimDueSchedules(context.Background(), time.Now(), "w", time.Minute, 1)
	b, _ := s.ClaimDueSchedules(context.Background(), time.Now(), "w2", time.Minute, 1)
	if len(a) != 1 || len(b) != 0 {
		t.Fatalf("claims=%d,%d", len(a), len(b))
	}
}
func TestFailedExecutionHasNoAutomaticRetry(t *testing.T) {
	s := NewMemoryStore()
	n := time.Now().UTC().Add(-time.Minute)
	_, _ = s.CreateSchedule(context.Background(), Schedule{OwnerID: "o", ScheduleID: "s", Status: "enabled", NextRunAt: &n}, "")
	a, _ := s.ClaimDueSchedules(context.Background(), time.Now(), "w", time.Minute, 1)
	_ = s.AdvanceSchedule(context.Background(), "o", "s", a[0].LeaseOwner, a[0].Revision, a[0].LeaseEpoch, nil, "disabled")
	b, _ := s.ClaimDueSchedules(context.Background(), time.Now(), "w2", time.Minute, 1)
	if len(b) != 0 {
		t.Fatal("failed run was retried")
	}
}

func TestMemoryScheduleExpiredLeaseRecoveryFencesStaleWorker(t *testing.T) {
	s := NewMemoryStore()
	n := time.Now().UTC().Add(-time.Minute)
	_, err := s.CreateSchedule(context.Background(), Schedule{OwnerID: "o", ScheduleID: "reclaim", Status: "enabled", NextRunAt: &n}, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimDueSchedules(context.Background(), time.Now().UTC(), "worker-a", time.Millisecond, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	r := ScheduleRun{RunID: "old", OwnerID: "o", ScheduleID: "reclaim", Status: "running", ScheduledFor: n, LeaseEpoch: first[0].LeaseEpoch}
	if _, _, err := s.CreateScheduleRun(context.Background(), r, first[0].LeaseOwner, first[0].Revision, first[0].LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	second, err := s.ClaimDueSchedules(context.Background(), time.Now().UTC(), "worker-b", time.Minute, 1)
	if err != nil || len(second) != 1 || second[0].LeaseEpoch <= first[0].LeaseEpoch || second[0].LeaseOwner != "worker-b" {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if err := s.FinishScheduleRun(context.Background(), "o", r.RunID, "worker-a", first[0].LeaseEpoch, "stale", "", time.Now().UTC()); err != ErrScheduleClaimed {
		t.Fatalf("stale finish=%v", err)
	}
	if err := s.AdvanceSchedule(context.Background(), "o", "reclaim", "worker-a", first[0].Revision, first[0].LeaseEpoch, nil, "disabled"); err != ErrScheduleClaimed {
		t.Fatalf("stale advance=%v", err)
	}
}

func TestMemoryScheduleRunNowClaimDoesNotResurrectActiveLease(t *testing.T) {
	s := NewMemoryStore()
	n := time.Now().UTC().Add(time.Hour)
	_, _ = s.CreateSchedule(context.Background(), Schedule{OwnerID: "o", ScheduleID: "now", Status: "enabled", NextRunAt: &n}, "")
	active, err := s.ClaimScheduleNow(context.Background(), "o", "now", "run-now-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimScheduleNow(context.Background(), "o", "now", "run-now-b", time.Minute); err != ErrScheduleConflict {
		t.Fatalf("overlap=%v", err)
	}
	if _, err := s.SetScheduleStatus(context.Background(), "o", "now", "", true); err != ErrScheduleConflict {
		t.Fatalf("enable active=%v", err)
	}
	if err := s.AdvanceSchedule(context.Background(), "o", "now", active.LeaseOwner, active.Revision, active.LeaseEpoch, active.NextRunAt, "enabled"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryScheduleOccurrenceRecoveryNeverExecutesTwice(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	due := time.Now().UTC().Add(-time.Minute)
	_, _ = s.CreateSchedule(ctx, Schedule{OwnerID: "o", ScheduleID: "occ", Status: "enabled", NextRunAt: &due}, "")
	first, _ := s.ClaimDueSchedules(ctx, time.Now().UTC(), "a", time.Millisecond, 1)
	r := ScheduleRun{RunID: "first", OwnerID: "o", ScheduleID: "occ", Status: "running", ScheduledFor: due, LeaseEpoch: first[0].LeaseEpoch}
	if _, created, err := s.CreateScheduleRun(ctx, r, first[0].LeaseOwner, first[0].Revision, first[0].LeaseEpoch); err != nil || !created {
		t.Fatalf("create=%v created=%v", err, created)
	}
	time.Sleep(3 * time.Millisecond)
	second, _ := s.ClaimDueSchedules(ctx, time.Now().UTC(), "b", time.Minute, 1)
	if len(second) != 1 {
		t.Fatalf("second=%#v", second)
	}
	if existing, created, err := s.CreateScheduleRun(ctx, ScheduleRun{RunID: "second", OwnerID: "o", ScheduleID: "occ", Status: "running", ScheduledFor: due, LeaseEpoch: second[0].LeaseEpoch}, second[0].LeaseOwner, second[0].Revision, second[0].LeaseEpoch); err != nil || created || existing.RunID != "first" {
		t.Fatalf("existing=%#v created=%v err=%v", existing, created, err)
	}
	if recovered, err := s.RecoverScheduleRun(ctx, "o", "occ", second[0].LeaseOwner, second[0].LeaseEpoch, due); err != nil || recovered.Status != "failed" {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	if err := s.AdvanceSchedule(ctx, "o", "occ", second[0].LeaseOwner, second[0].Revision, second[0].LeaseEpoch, nil, "disabled"); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.ListScheduleRuns(ctx, "o", "occ", 10, "")
	if len(runs.Runs) != 1 {
		t.Fatalf("runs=%#v", runs)
	}
}

func TestMemoryScheduleRunRecoveryLeaseFencesExpiredDeterministicRun(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	n := time.Now().UTC().Add(time.Hour)
	if _, err := s.CreateSchedule(ctx, Schedule{OwnerID: "o", ScheduleID: "recover", Status: "enabled", NextRunAt: &n}, ""); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimScheduleNow(ctx, "o", "recover", "old-worker", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	run := ScheduleRun{RunID: "deterministic", OwnerID: "o", ScheduleID: "recover", Status: "running", ScheduledFor: time.Now().UTC(), LeaseEpoch: first.LeaseEpoch}
	if _, created, err := s.CreateScheduleRun(ctx, run, first.LeaseOwner, first.Revision, first.LeaseEpoch); err != nil || !created {
		t.Fatalf("create=%v created=%v", err, created)
	}
	time.Sleep(3 * time.Millisecond)
	recovered, err := s.AcquireScheduleRunRecoveryLease(ctx, "o", "recover", run.RunID, "replay", time.Minute)
	if err != nil || recovered.LeaseEpoch <= first.LeaseEpoch || recovered.Revision <= first.Revision || recovered.LeaseOwner != "replay" {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	if err := s.FinishScheduleRun(ctx, "o", run.RunID, first.LeaseOwner, first.LeaseEpoch, "stale", "", time.Now().UTC()); err != ErrScheduleClaimed {
		t.Fatalf("stale finish=%v", err)
	}
	if err := s.AdvanceSchedule(ctx, "o", "recover", first.LeaseOwner, first.Revision, first.LeaseEpoch, nil, "disabled"); err != ErrScheduleClaimed {
		t.Fatalf("stale advance=%v", err)
	}
}

func TestMemoryScheduleRunsListOrdersByOccurrenceAndPaginatesStableTies(t *testing.T) {
	store := NewMemoryStore()
	store.ensureSchedules()
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tie := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	for _, run := range []ScheduleRun{
		{RunID: "run-older", OwnerID: "owner", ScheduleID: "schedule", ScheduledFor: older},
		{RunID: "run-tie-a", OwnerID: "owner", ScheduleID: "schedule", ScheduledFor: tie},
		{RunID: "run-tie-z", OwnerID: "owner", ScheduleID: "schedule", ScheduledFor: tie},
		{RunID: "run-newest", OwnerID: "owner", ScheduleID: "schedule", ScheduledFor: newest},
	} {
		store.scheduleRuns[run.RunID] = run
	}

	first, err := store.ListScheduleRuns(context.Background(), "owner", "schedule", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{first.Runs[0].RunID, first.Runs[1].RunID}; !reflect.DeepEqual(got, []string{"run-newest", "run-tie-z"}) {
		t.Fatalf("first page = %#v, want newest-first with run_id DESC tie-break", got)
	}
	if first.NextCursor == "" {
		t.Fatal("first page missing cursor")
	}

	second, err := store.ListScheduleRuns(context.Background(), "owner", "schedule", 2, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{second.Runs[0].RunID, second.Runs[1].RunID}; !reflect.DeepEqual(got, []string{"run-tie-a", "run-older"}) {
		t.Fatalf("second page = %#v, want remaining runs without gaps or duplicates", got)
	}
	if second.NextCursor != "" {
		t.Fatalf("second page cursor = %q, want empty", second.NextCursor)
	}
}
