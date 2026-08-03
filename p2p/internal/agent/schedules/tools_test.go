package schedules

import (
	"context"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestNativeAgentScheduleMutationIsNotExposed(t *testing.T) {
	store := storage.NewMemoryStore()
	_, err := store.CreateSchedule(context.Background(), storage.Schedule{ScheduleID: "s1", OwnerID: "@owner:test", Name: "daily", Prompt: "hello", TriggerKind: "interval", TriggerValue: "1h", Timezone: "UTC", Status: "enabled", Revision: 1, CreatedAt: time.Now().UTC()}, "")
	if err != nil {
		t.Fatal(err)
	}
	m := New(Config{Store: store, OwnerID: func() string { return "@owner:test" }})
	if _, err := m.invokeChatTool(context.Background(), "agent.schedules.delete", map[string]any{"schedule_id": "s1", "expected_revision": int64(1)}); err == nil {
		t.Fatal("Native Agent exposed a schedule mutation")
	}
	if _, ok, _ := store.GetSchedule(context.Background(), "@owner:test", "s1"); !ok {
		t.Fatal("read-only rejection mutated schedule")
	}
}
