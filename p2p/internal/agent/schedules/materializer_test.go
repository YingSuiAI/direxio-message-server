package schedules

import (
	"context"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type recordingMaterializer struct {
	requests []OccurrenceRequest
	result   storage.ScheduleRun
}

func (m *recordingMaterializer) MaterializeOccurrence(_ context.Context, req OccurrenceRequest) (storage.ScheduleRun, error) {
	m.requests = append(m.requests, req)
	if m.result.RunID == "" {
		m.result = storage.ScheduleRun{RunID: req.RunID, ScheduleID: req.Schedule.ScheduleID, OwnerID: req.Schedule.OwnerID, Status: "running", ScheduledFor: req.ScheduledFor}
	}
	return m.result, nil
}

func TestRunNowMaterializesGenericOccurrence(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	next := time.Now().UTC().Add(time.Hour)
	if _, err := store.CreateSchedule(ctx, storage.Schedule{OwnerID: "owner", ScheduleID: "schedule", Prompt: "do work", ModelProfileID: "00000000-0000-4000-8000-000000000001", Status: "enabled", NextRunAt: &next}, ""); err != nil {
		t.Fatal(err)
	}
	materializer := &recordingMaterializer{}
	m := New(Config{Store: store, Materializer: materializer, OwnerID: func() string { return "owner" }})
	out, err := m.runNowOnce(ctx, map[string]any{"schedule_id": "schedule", "idempotency_key": "00000000-0000-4000-8000-000000000001"})
	if err != nil {
		t.Fatalf("run now: %v", err)
	}
	if len(materializer.requests) != 1 || materializer.requests[0].RunID == "" || materializer.requests[0].TaskID == "" {
		t.Fatalf("materialization request=%+v", materializer.requests)
	}
	if out.(map[string]any)["run"].(storage.ScheduleRun).RunID == "" {
		t.Fatalf("result=%#v", out)
	}
}

func TestRunNowIdentityIsIndependentOfWallClock(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	next := time.Now().UTC().Add(time.Hour)
	if _, err := store.CreateSchedule(ctx, storage.Schedule{OwnerID: "owner", ScheduleID: "schedule", Prompt: "do work", ModelProfileID: "00000000-0000-4000-8000-000000000001", Status: "enabled", NextRunAt: &next}, ""); err != nil {
		t.Fatal(err)
	}
	materializer := &recordingMaterializer{}
	m := New(Config{Store: store, Materializer: materializer, OwnerID: func() string { return "owner" }})
	params := map[string]any{"schedule_id": "schedule", "idempotency_key": "00000000-0000-4000-8000-000000000001"}
	if _, e := m.runNowOnce(ctx, params); e != nil {
		t.Fatal(e)
	}
	time.Sleep(time.Millisecond)
	if _, e := m.runNowOnce(ctx, params); e != nil {
		t.Fatal(e)
	}
	if len(materializer.requests) != 2 || materializer.requests[0].OccurrenceID != materializer.requests[1].OccurrenceID || materializer.requests[0].TaskID != materializer.requests[1].TaskID {
		t.Fatalf("requests=%+v", materializer.requests)
	}
}

func TestScheduledOccurrenceIDsAreOwnerScopedUUIDs(t *testing.T) {
	at := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	o, taskID := ScheduledOccurrenceIDs("owner-a", "schedule", at)
	if len(o) != 36 || len(taskID) != 36 {
		t.Fatalf("occurrence=%q task=%q", o, taskID)
	}
	other, otherTask := ScheduledOccurrenceIDs("owner-b", "schedule", at)
	if o == other || taskID == otherTask {
		t.Fatalf("owner-local identities collided: %q/%q", o, other)
	}
}
