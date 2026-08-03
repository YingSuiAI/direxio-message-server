package schedules

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func runNowReplayFixture(t *testing.T, lease time.Duration) (*storage.MemoryStore, *Module, storage.Schedule, storage.ScheduleRun) {
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
	return store, New(Config{Store: store, OwnerID: func() string { return "owner" }}), sch, run
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
	tools := New(Config{}).Tools()
	if len(tools) != 4 {
		t.Fatalf("Native Agent schedule tools = %d, want 4 read tools", len(tools))
	}
	for _, tool := range tools {
		if tool.Write {
			t.Fatalf("Native Agent schedule tool %q is writable", tool.Name)
		}
	}
	for _, x := range []string{"agent.messages.send", "agent.runtime.run"} {
		for _, a := range nativeagent.EmbeddedAllowedTools() {
			if x == a {
				t.Fatalf("mutation allowlisted")
			}
		}
	}
}

func TestScheduleListUsesPagePaginationContract(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	for _, id := range []string{"schedule-1", "schedule-2", "schedule-3"} {
		if _, err := store.CreateSchedule(ctx, storage.Schedule{OwnerID: "owner", ScheduleID: id}, ""); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	list := New(Config{Store: store, OwnerID: func() string { return "owner" }}).Handlers()["agent.schedules.list"]

	for _, params := range []map[string]any{{"limit": 1}, {"cursor": "schedule-1"}} {
		if _, actionErr := list(ctx, params); actionErr == nil {
			t.Fatalf("legacy pagination params unexpectedly accepted: %#v", params)
		}
	}

	first, actionErr := list(ctx, map[string]any{"page_size": 2})
	if actionErr != nil {
		t.Fatalf("first page: %#v", actionErr)
	}
	firstPage := first.(map[string]any)
	if got := len(firstPage["schedules"].([]storage.Schedule)); got != 2 {
		t.Fatalf("first page length = %d, want 2", got)
	}
	next, ok := firstPage["next_cursor"].(string)
	if !ok || next == "" {
		t.Fatalf("first page cursor = %#v", firstPage["next_cursor"])
	}

	second, actionErr := list(ctx, map[string]any{"page_size": 2, "page_token": next})
	if actionErr != nil {
		t.Fatalf("second page: %#v", actionErr)
	}
	secondPage := second.(map[string]any)
	if got := len(secondPage["schedules"].([]storage.Schedule)); got != 1 {
		t.Fatalf("second page length = %d, want 1", got)
	}
	if got := secondPage["next_cursor"].(string); got != "" {
		t.Fatalf("second page cursor = %q, want empty", got)
	}
}

func TestScheduleListRejectsInvalidPageParameters(t *testing.T) {
	list := New(Config{Store: storage.NewMemoryStore(), OwnerID: func() string { return "owner" }}).Handlers()["agent.schedules.list"]
	for name, params := range map[string]map[string]any{
		"unknown":       {"page_size": 1, "limit": 1},
		"string size":   {"page_size": "1"},
		"fraction size": {"page_size": 1.5},
		"zero size":     {"page_size": 0},
		"negative size": {"page_size": -1},
		"large size":    {"page_size": 101},
		"token type":    {"page_token": 1},
	} {
		if _, actionErr := list(context.Background(), params); actionErr == nil {
			t.Fatalf("%s unexpectedly accepted: %#v", name, params)
		}
	}
}

func TestScheduleListAcceptsUseNumberIntegralPageSize(t *testing.T) {
	list := New(Config{Store: storage.NewMemoryStore(), OwnerID: func() string { return "owner" }}).Handlers()["agent.schedules.list"]
	if _, e := list(context.Background(), map[string]any{"page_size": json.Number("2")}); e != nil {
		t.Fatal(e)
	}
	for _, v := range []any{json.Number("1.5"), json.Number("NaN"), json.Number("-1"), "2"} {
		if _, e := list(context.Background(), map[string]any{"page_size": v}); e == nil {
			t.Fatalf("accepted %#v", v)
		}
	}
}

func TestExpectedRevisionUseNumberStrict(t *testing.T) {
	if n, e := expectedRevision(map[string]any{"expected_revision": json.Number("2")}); e != nil || n != 2 {
		t.Fatalf("n=%d e=%v", n, e)
	}
	for _, v := range []any{json.Number("1.5"), json.Number("0"), json.Number("-1"), "2"} {
		if _, e := expectedRevision(map[string]any{"expected_revision": v}); e == nil {
			t.Fatalf("accepted %#v", v)
		}
	}
}

func TestActiveSameKeyReplayReturnsInProgressWithoutRunnerCall(t *testing.T) {
	store, module, sch, run := runNowReplayFixture(t, time.Minute)
	out, terminal, handled := module.reconcileRunNowReplay(context.Background(), sch.ScheduleID, run.RunID)
	if !handled || terminal {
		t.Fatalf("handled=%v terminal=%v out=%#v", handled, terminal, out)
	}
	got, ok, err := store.GetScheduleRun(context.Background(), "owner", sch.ScheduleID, run.RunID)
	if err != nil || !ok || got.Status != "running" {
		t.Fatalf("run=%#v ok=%v err=%v", got, ok, err)
	}
}

func TestExpiredSameKeyReplayFailsWithoutExtraRunnerCall(t *testing.T) {
	store, module, sch, run := runNowReplayFixture(t, time.Millisecond)
	time.Sleep(3 * time.Millisecond)
	_, terminal, handled := module.reconcileRunNowReplay(context.Background(), sch.ScheduleID, run.RunID)
	if !handled || !terminal {
		t.Fatalf("handled=%v terminal=%v", handled, terminal)
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
	store, module, sch, run := runNowReplayFixture(t, time.Minute)
	if err := store.FinishScheduleRun(context.Background(), "owner", run.RunID, sch.LeaseOwner, sch.LeaseEpoch, "done", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_, terminal, handled := module.reconcileRunNowReplay(context.Background(), sch.ScheduleID, run.RunID)
	if !handled || !terminal {
		t.Fatalf("handled=%v terminal=%v", handled, terminal)
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
