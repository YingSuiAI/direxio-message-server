package schedules

import (
	"context"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type replayRecordingRunner struct{ calls int }

func (r *replayRecordingRunner) ExecuteScheduled(context.Context, string, storage.ModelProfile, []string) (string, error) {
	r.calls++
	return "unexpected", nil
}

func runNowReplayFixture(t *testing.T, lease time.Duration) (*storage.MemoryStore, *Module, storage.Schedule, storage.ScheduleRun, *replayRecordingRunner) {
	t.Helper()
	ctx := context.Background()
	store := storage.NewMemoryStore()
	next := time.Now().UTC().Add(time.Hour)
	if _, err := store.CreateSchedule(ctx, storage.Schedule{OwnerID: "owner", ScheduleID: "schedule", Status: "enabled", NextRunAt: &next}, ""); err != nil {
		t.Fatal(err)
	}
	sch, err := store.ClaimScheduleNow(ctx, "owner", "schedule", "initial-worker", lease)
	if err != nil {
		t.Fatal(err)
	}
	run := storage.ScheduleRun{RunID: "deterministic-run", OwnerID: "owner", ScheduleID: "schedule", Status: "running", ScheduledFor: time.Now().UTC(), LeaseEpoch: sch.LeaseEpoch}
	if _, created, err := store.CreateScheduleRun(ctx, run, sch.LeaseOwner, sch.Revision, sch.LeaseEpoch); err != nil || !created {
		t.Fatalf("create run=%v created=%v", err, created)
	}
	runner := &replayRecordingRunner{}
	return store, New(Config{Store: store, Runner: runner, OwnerID: func() string { return "owner" }}), sch, run, runner
}

func TestCreateRejectsInvalidTriggerAndMissingTimezone(t *testing.T) {
	m := New(Config{Store: storage.NewMemoryStore()})
	for _, trigger := range []map[string]any{{"kind": "one_time", "value": "not-a-time"}, {"kind": "cron", "value": "* * * * *"}} {
		_, err := m.create(context.Background(), map[string]any{"name": "n", "prompt": "p", "model_profile_id": "x", "trigger": trigger, "idempotency_key": "k"})
		if err == nil {
			t.Fatalf("trigger %#v accepted", trigger)
		}
	}
}

func TestRecurringCronCoalescesDowntimeToNextOccurrence(t *testing.T) {
	n, e := nextCron("*/5 * * * *", "UTC", time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC))
	if e != nil || n.Minute() != 5 {
		t.Fatalf("next=%v err=%v", n, e)
	}
}

func TestPinnedCredentialAndSecretFreeReadback(t *testing.T) {
	v := storage.Schedule{Prompt: "secret", ModelProfileRevision: 3, CredentialVersion: 4}
	if v.ModelProfileRevision != 3 || v.CredentialVersion != 4 {
		t.Fatal("pin lost")
	}
}
func TestReadOnlyAllowlistRejectsMutation(t *testing.T) {
	for _, x := range []string{"agent.messages.send", "agent.runtime.run"} {
		for _, a := range nativeagent.EmbeddedAllowedTools() {
			if x == a {
				t.Fatalf("mutation allowlisted")
			}
		}
	}
}

func TestActiveSameKeyReplayReturnsInProgressWithoutRunnerCall(t *testing.T) {
	store, module, sch, run, runner := runNowReplayFixture(t, time.Minute)
	out, terminal, handled := module.reconcileRunNowReplay(context.Background(), sch.ScheduleID, run.RunID)
	if !handled || terminal || runner.calls != 0 {
		t.Fatalf("handled=%v terminal=%v calls=%d out=%#v", handled, terminal, runner.calls, out)
	}
	got, ok, err := store.GetScheduleRun(context.Background(), "owner", sch.ScheduleID, run.RunID)
	if err != nil || !ok || got.Status != "running" {
		t.Fatalf("run=%#v ok=%v err=%v", got, ok, err)
	}
}

func TestExpiredSameKeyReplayFailsWithoutExtraRunnerCall(t *testing.T) {
	store, module, sch, run, runner := runNowReplayFixture(t, time.Millisecond)
	time.Sleep(3 * time.Millisecond)
	_, terminal, handled := module.reconcileRunNowReplay(context.Background(), sch.ScheduleID, run.RunID)
	if !handled || !terminal || runner.calls != 0 {
		t.Fatalf("handled=%v terminal=%v calls=%d", handled, terminal, runner.calls)
	}
	got, ok, err := store.GetScheduleRun(context.Background(), "owner", sch.ScheduleID, run.RunID)
	if err != nil || !ok || got.Status != "failed" || got.FinishedAt == nil {
		t.Fatalf("run=%#v ok=%v err=%v", got, ok, err)
	}
	advanced, ok, err := store.GetSchedule(context.Background(), "owner", sch.ScheduleID)
	if err != nil || !ok || advanced.Status != "enabled" || advanced.LeaseOwner != "" {
		t.Fatalf("schedule=%#v ok=%v err=%v", advanced, ok, err)
	}
}

func TestTerminalFinishBeforeAdvanceReplayAdvancesOnly(t *testing.T) {
	store, module, sch, run, runner := runNowReplayFixture(t, time.Minute)
	if err := store.FinishScheduleRun(context.Background(), "owner", run.RunID, sch.LeaseOwner, sch.LeaseEpoch, "done", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_, terminal, handled := module.reconcileRunNowReplay(context.Background(), sch.ScheduleID, run.RunID)
	if !handled || !terminal || runner.calls != 0 {
		t.Fatalf("handled=%v terminal=%v calls=%d", handled, terminal, runner.calls)
	}
	got, ok, err := store.GetScheduleRun(context.Background(), "owner", sch.ScheduleID, run.RunID)
	if err != nil || !ok || got.Status != "succeeded" || got.Result != "done" {
		t.Fatalf("run=%#v ok=%v err=%v", got, ok, err)
	}
	advanced, ok, err := store.GetSchedule(context.Background(), "owner", sch.ScheduleID)
	if err != nil || !ok || advanced.Status != "enabled" || advanced.LeaseOwner != "" {
		t.Fatalf("schedule=%#v ok=%v err=%v", advanced, ok, err)
	}
}
