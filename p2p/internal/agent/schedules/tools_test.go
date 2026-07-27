package schedules

import (
	"context"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestChatScheduleMutationRequiresLaterOwnerPhrase(t *testing.T) {
	store := storage.NewMemoryStore()
	_, err := store.CreateSchedule(context.Background(), storage.Schedule{ScheduleID: "s1", OwnerID: "@owner:test", Name: "daily", Prompt: "hello", TriggerKind: "interval", TriggerValue: "1h", Timezone: "UTC", Status: "enabled", Revision: 1, CreatedAt: time.Now().UTC()}, "")
	if err != nil {
		t.Fatal(err)
	}
	m := New(Config{Store: store, Confirmations: store, OwnerID: func() string { return "@owner:test" }})
	proposal, err := m.invokeChatTool(nativeagent.WithRequestContext(context.Background(), "@owner:test", "conv-1", ""), "agent.schedules.delete", map[string]any{"schedule_id": "s1", "expected_revision": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	p := proposal.(map[string]any)
	t.Logf("proposal=%#v", p)
	if p["confirmation_required"] != true {
		t.Fatalf("expected proposal: %#v", p)
	}
	if _, ok, _ := store.GetSchedule(context.Background(), "@owner:test", "s1"); !ok {
		t.Fatal("proposal mutated schedule")
	}
	confirmationID := p["confirmation_id"].(string)
	phrase := p["approval_phrase"].(string)
	wrong, err := m.invokeChatTool(nativeagent.WithRequestContext(context.Background(), "@owner:test", "conv-1", "确认执行 WRONG"), "agent.schedules.confirm", map[string]any{"confirmation_id": confirmationID})
	if err != nil || wrong.(map[string]any)["confirmation_required"] != true {
		t.Fatalf("wrong phrase executed: %#v %v", wrong, err)
	}
	wrongScope, err := m.invokeChatTool(nativeagent.WithRequestContext(context.Background(), "@other:test", "conv-1", phrase), "agent.schedules.confirm", map[string]any{"confirmation_id": confirmationID})
	if err != nil || wrongScope.(map[string]any)["confirmation_required"] != true {
		t.Fatalf("wrong owner executed: %#v %v", wrongScope, err)
	}
	confirmed, err := m.invokeChatTool(nativeagent.WithRequestContext(context.Background(), "@owner:test", "conv-1", phrase), "agent.schedules.confirm", map[string]any{"confirmation_id": confirmationID})
	t.Logf("confirmed=%#v", confirmed)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.GetSchedule(context.Background(), "@owner:test", "s1"); ok {
		t.Fatal("confirmation did not delete schedule")
	}
}
