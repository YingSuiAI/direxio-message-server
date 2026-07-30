package nativeagent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestNativeAgentReferencesExtractRoomsAndPostsInToolOrder(t *testing.T) {
	produced := []*schema.Message{
		schema.ToolMessage(`{"result":{"contacts":[{"display_name":"Ada","room_id":"!direct:example.com"},{"display_name":"Ada duplicate","room_id":"!direct:example.com"}]}}`, "call_contacts", schema.WithToolName("dirextalk_contacts_search")),
		schema.ToolMessage(`{"result":{"rooms":[{"type":"group","name":"Team","room_id":"!group:example.com"},{"type":"channel","name":"News","room_id":"!channel:example.com"}]}}`, "call_rooms", schema.WithToolName("dirextalk_rooms_search")),
		schema.ToolMessage(`{"result":{"room_id":"!group:example.com","name":"Team","messages":[{"msg":"matching message"}]}}`, "call_messages", schema.WithToolName("dirextalk_messages_list")),
		schema.ToolMessage(`{"result":{"room_id":"!channel:example.com","channel_id":"news","name":"News","posts":[{"post_id":"post_1","msg":"first post"},{"post_id":"post_2","msg":"second post"}]}}`, "call_posts", schema.WithToolName("dirextalk_channel_posts_list")),
		schema.ToolMessage(`{"result":{"room_id":"!migrated-channel:example.com","channel_id":"news","name":"News duplicate","posts":[{"post_id":"post_1","msg":"duplicate post"}]}}`, "call_duplicate_posts", schema.WithToolName("dirextalk_channel_posts_list")),
	}

	got := nativeAgentReferences(produced)
	if len(got) != 5 {
		t.Fatalf("references len = %d, want 5: %#v", len(got), got)
	}
	want := []map[string]any{
		{"kind": "room", "room_id": "!direct:example.com", "room_type": "direct", "title": "Ada"},
		{"kind": "room", "room_id": "!group:example.com", "room_type": "group", "title": "Team"},
		{"kind": "room", "room_id": "!channel:example.com", "room_type": "channel", "title": "News"},
		{"kind": "channel_post", "room_id": "!channel:example.com", "channel_id": "news", "post_id": "post_1", "title": "News", "preview": "first post"},
		{"kind": "channel_post", "room_id": "!channel:example.com", "channel_id": "news", "post_id": "post_2", "title": "News", "preview": "second post"},
	}
	for index := range want {
		for key, value := range want[index] {
			if got[index][key] != value {
				t.Fatalf("reference %d %s = %#v, want %#v; full=%#v", index, key, got[index][key], value, got)
			}
		}
	}
}

func TestNativeAgentReferencesUseMessagePreviewAndIgnoreInvalidResults(t *testing.T) {
	produced := []*schema.Message{
		schema.ToolMessage(`{"result":{"room_id":"!room:example.com","name":"Room","messages":[{"msg":" matched text "}]}}`, "call_messages", schema.WithToolName("dirextalk_messages_list")),
		schema.ToolMessage(`{"error":"denied"}`, "call_error", schema.WithToolName("dirextalk_rooms_search")),
		schema.ToolMessage(`not-json`, "call_bad", schema.WithToolName("dirextalk_contacts_list")),
		schema.ToolMessage(`{"result":{"room_id":"","name":"Missing"}}`, "call_missing", schema.WithToolName("dirextalk_messages_list")),
		schema.ToolMessage(`{"result":{"rooms":[{"type":"unknown","name":"Unknown","room_id":"!unknown:example.com"}]}}`, "call_unknown", schema.WithToolName("dirextalk_rooms_search")),
		schema.ToolMessage(`{"result":{"room_id":"!channel:example.com","posts":[{"post_id":"post_without_channel","msg":"body"}]}}`, "call_incomplete_post", schema.WithToolName("dirextalk_channel_posts_list")),
		schema.ToolMessage(`{"result":{"room_id":"!ignored:example.com"}}`, "call_other", schema.WithToolName("runtime__shell")),
	}

	got := nativeAgentReferences(produced)
	if len(got) != 1 {
		t.Fatalf("references = %#v, want one valid room reference", got)
	}
	if got[0]["room_id"] != "!room:example.com" || got[0]["preview"] != "matched text" {
		t.Fatalf("unexpected message reference: %#v", got[0])
	}
}

func TestNativeAgentReferencesExtractOnlyPendingWorkloadProposals(t *testing.T) {
	valid := `{"result":{"operation":{"operation_id":"11111111-1111-4111-8111-111111111111","workload_id":"22222222-2222-4222-8222-222222222222","plan_id":"33333333-3333-4333-8333-333333333333","task_id":"44444444-4444-4444-8444-444444444444","confirmation_id":"55555555-5555-4555-8555-555555555555","kind":"apply","revision":2,"plan_revision":1,"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","target_kind":"aws-ecs","summary":"Deploy API"},"confirmation":{"confirmation_id":"55555555-5555-4555-8555-555555555555","task_id":"44444444-4444-4444-8444-444444444444","state":"pending","revision":1,"expires_at":"2030-01-01T00:00:00Z","binding":{"operation_domain":"workload:apply","target_id":"22222222-2222-4222-8222-222222222222","target_revision":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}`
	produced := []*schema.Message{
		schema.ToolMessage(valid, "call_apply", schema.WithToolName("native_agent_workloads_apply")),
		// Model-authored text and an unrelated tool with the same payload shape
		// must never become actionable confirmation references.
		schema.ToolMessage(valid, "call_other", schema.WithToolName("runtime__shell")),
		schema.ToolMessage(strings.Replace(valid, `"state":"pending"`, `"state":"confirmed"`, 1), "call_confirmed", schema.WithToolName("native_agent_workloads_apply")),
	}
	got := nativeAgentReferences(produced)
	if len(got) != 1 {
		t.Fatalf("references = %#v, want one pending confirmation", got)
	}
	want := map[string]any{
		"kind": "pending_confirmation", "confirmation_id": "55555555-5555-4555-8555-555555555555",
		"operation_id": "11111111-1111-4111-8111-111111111111", "workload_id": "22222222-2222-4222-8222-222222222222",
		"action": "apply", "revision": int64(1), "target_kind": "aws-ecs", "summary": "Deploy API",
	}
	for key, value := range want {
		if got[0][key] != value {
			t.Fatalf("reference %s = %#v, want %#v; full=%#v", key, got[0][key], value, got)
		}
	}
}

func TestNativeAgentReferencesRejectMalformedPendingConfirmation(t *testing.T) {
	base := `{"result":{"operation":{"operation_id":"11111111-1111-4111-8111-111111111111","workload_id":"22222222-2222-4222-8222-222222222222","plan_id":"33333333-3333-4333-8333-333333333333","task_id":"44444444-4444-4444-8444-444444444444","confirmation_id":"55555555-5555-4555-8555-555555555555","kind":"apply","revision":2,"plan_revision":1,"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"confirmation":{"confirmation_id":"55555555-5555-4555-8555-555555555555","task_id":"44444444-4444-4444-8444-444444444444","state":"pending","revision":1,"expires_at":"2030-01-01T00:00:00Z","binding":{"operation_domain":"workload:apply","target_id":"22222222-2222-4222-8222-222222222222","target_revision":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}`
	for _, malformed := range []string{
		strings.Replace(base, `"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"plan_digest":"bad"`, 1),
		strings.Replace(base, `"operation_id":"11111111-1111-4111-8111-111111111111"`, `"operation_id":"not-uuid"`, 1),
		strings.Replace(base, `"expires_at":"2030-01-01T00:00:00Z"`, `"expires_at":"2020-01-01T00:00:00Z"`, 1),
		strings.Replace(base, `"task_id":"44444444-4444-4444-8444-444444444444"`, `"task_id":"66666666-6666-4666-8666-666666666666"`, 1),
		strings.Replace(base, `"operation_domain":"workload:apply"`, `"operation_domain":"workload:destroy"`, 1),
		strings.Replace(base, `"target_revision":1`, `"target_revision":2`, 1),
		strings.Replace(base, `"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"content_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`, 1),
	} {
		got := nativeAgentReferences([]*schema.Message{schema.ToolMessage(malformed, "call", schema.WithToolName("native_agent_workloads_apply"))})
		if len(got) != 0 {
			t.Fatalf("malformed pending reference accepted: %#v", got)
		}
	}
}
