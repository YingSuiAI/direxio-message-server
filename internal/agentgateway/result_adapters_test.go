package agentgateway

import (
	"reflect"
	"sort"
	"testing"
)

func TestPublicResultAdaptersPreserveLegacyEnvelopes(t *testing.T) {
	tests := []struct {
		name   string
		action string
		input  map[string]any
		check  func(*testing.T, map[string]any)
	}{
		{"conversation create", "agent.chat.conversations.create", map[string]any{"ID": "c1", "Title": "Chat"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["conversation"]; !ok || got["replayed"] != false {
				t.Fatalf("conversation create = %#v", got)
			}
		}},
		{"conversation get", "agent.chat.conversations.get", map[string]any{"ID": "c1", "Messages": []any{map[string]any{"role": "user"}}, "NextCursor": "next"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["conversation"]; !ok || got["next_cursor"] != "next" || len(got["messages"].([]any)) != 1 {
				t.Fatalf("conversation get = %#v", got)
			}
		}},
		{"conversation list", "agent.chat.conversations.list", map[string]any{"Conversations": []any{}, "next_page_token": "p"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["conversations"]; !ok || got["next_cursor"] != "p" {
				t.Fatalf("conversation list = %#v", got)
			}
		}},
		{"conversation delete", "agent.chat.conversations.delete", map[string]any{"deleted": true}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"conversation", "replayed"}) {
				t.Fatalf("conversation delete envelope keys=%v value=%#v", keys, got)
			}
		}},
		{"model get", "agent.model_profiles.get", map[string]any{"id": "p1", "provider": "openai_compatible"}, func(t *testing.T, got map[string]any) {
			profile, ok := got["profile"].(map[string]any)
			if !ok || profile["profile_id"] != "p1" {
				t.Fatalf("model get = %#v", got)
			}
			if _, leaked := profile["id"]; leaked {
				t.Fatalf("core id leaked into model profile: %#v", profile)
			}
		}},
		{"model list", "agent.model_profiles.list", map[string]any{"Profiles": []any{}, "NextCursor": "p"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["profiles"]; !ok || got["next_page_token"] != "p" {
				t.Fatalf("model list = %#v", got)
			}
		}},
		{"source list", "agent.knowledge.sources.list", map[string]any{"Sources": []any{map[string]any{"ID": "s1", "SizeBytes": float64(4)}}, "NextPageToken": "p"}, func(t *testing.T, got map[string]any) {
			items := got["sources"].([]any)
			if items[0].(map[string]any)["source_id"] != "s1" || got["next_page_token"] != "p" {
				t.Fatalf("source list = %#v", got)
			}
		}},
		{"memory list", "agent.knowledge.memories.list", map[string]any{"Sources": []any{map[string]any{"ID": "m1", "Content": "hello"}}, "NextPageToken": "p"}, func(t *testing.T, got map[string]any) {
			items := got["items"].([]any)
			if items[0].(map[string]any)["memory_id"] != "m1" || items[0].(map[string]any)["content"] != "hello" || got["next_page_token"] != "p" {
				t.Fatalf("memory list = %#v", got)
			}
		}},
		{"knowledge search", "agent.knowledge.search", map[string]any{"Matches": []any{map[string]any{"SourceID": "s1", "Score": float64(.9)}}, "NextPageToken": "p"}, func(t *testing.T, got map[string]any) {
			items := got["items"].([]any)
			if items[0].(map[string]any)["source_id"] != "s1" || got["next_cursor"] != "p" {
				t.Fatalf("knowledge search = %#v", got)
			}
		}},
		{"knowledge config", "agent.knowledge.config.get", map[string]any{"EmbeddingProfileID": "embed", "Dimension": float64(3), "Revision": float64(2)}, func(t *testing.T, got map[string]any) {
			if got["embedding_profile_id"] != "embed" || got["dimension"] != float64(3) || got["revision"] != float64(2) {
				t.Fatalf("knowledge config = %#v", got)
			}
		}},
		{"knowledge upload start exact projection", "agent.knowledge.upload.start", map[string]any{"upload_id": "u1", "source_id": "s1", "status": "open", "size": float64(8), "received_size": float64(2), "max_chunk_bytes": float64(4), "progress": float64(.25), "replayed": true, "revision": float64(2)}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"max_chunk_bytes", "progress", "received_size", "replayed", "size", "source_id", "status", "upload_id"}) {
				t.Fatalf("upload start keys=%v value=%#v", keys, got)
			}
		}},
		{"knowledge status exact projection", "agent.knowledge.status", map[string]any{"Supported": true, "Count": float64(3), "ReadyCount": float64(2), "FailedCount": float64(1), "EmbeddingIndexed": float64(1), "EmbeddingStale": float64(2), "EmbeddingProfileID": "embed", "EmbeddingProfileRevision": float64(4), "EmbeddingModel": "text-embedding-3-small", "extra": "drop"}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"count", "embedding_indexed", "embedding_model", "embedding_profile_id", "embedding_profile_revision", "embedding_stale", "supported"}) {
				t.Fatalf("knowledge status keys=%v value=%#v", keys, got)
			}
			if got["embedding_indexed"] != float64(1) || got["embedding_stale"] != float64(2) {
				t.Fatalf("knowledge status counters were inferred/changed: %#v", got)
			}
		}},
		{"memory create exact projection", "agent.knowledge.memory.create", map[string]any{"memory_id": "m1", "title": "title", "content": "hello", "tags": []any{"a"}, "revision": float64(2), "created_at": "now", "updated_at": "later", "replayed": true, "embedding_indexed": false, "embedding_profile_id": "embed", "embedding_profile_revision": float64(4), "embedding_model": "model", "extra": "drop"}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"content", "created_at", "embedding_indexed", "embedding_model", "embedding_profile_id", "embedding_profile_revision", "memory_id", "replayed", "tags", "title"}) {
				t.Fatalf("memory create keys=%v value=%#v", keys, got)
			}
		}},
		{"memory get exact projection", "agent.knowledge.memories.get", map[string]any{"memory_id": "m1", "title": "title", "content": "hello", "tags": []any{}, "revision": float64(2), "created_at": "now", "updated_at": "later", "replayed": true, "embedding_indexed": false}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"content", "created_at", "memory_id", "revision", "tags", "title", "updated_at"}) {
				t.Fatalf("memory get keys=%v value=%#v", keys, got)
			}
		}},
		{"task get", "agent.core.tasks.get", map[string]any{"ID": "t1", "State": "queued"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["task"]; !ok {
				t.Fatalf("task get = %#v", got)
			}
		}},
		{"schedule list", "agent.schedules.list", map[string]any{"schedules": []any{}, "next_page_token": "p"}, func(t *testing.T, got map[string]any) {
			if got["next_cursor"] != "p" {
				t.Fatalf("schedule list = %#v", got)
			}
		}},
		{"extension list", "agent.core.skills.list", map[string]any{"Installations": []any{map[string]any{"ID": "i1", "State": "installed"}}, "NextPageToken": "p"}, func(t *testing.T, got map[string]any) {
			items := got["installations"].([]any)
			if items[0].(map[string]any)["id"] != "i1" || got["next_page_token"] != "p" {
				t.Fatalf("extension list = %#v", got)
			}
		}},
		{"chat", "agent.chat", map[string]any{"message": map[string]any{"Content": "hello"}}, func(t *testing.T, got map[string]any) {
			if got["text"] != "hello" {
				t.Fatalf("chat = %#v", got)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.check(t, adaptActionResult(test.action, test.input))
		})
	}
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestLegacyInputAliasesAreCanonicalizedBeforeAgentDigest(t *testing.T) {
	input := map[string]any{"confirm": "deprovision_account"}
	applyLegacyInputAliases("agent.account.deprovision", input)
	if input["confirmation"] != "deprovision_account" {
		t.Fatalf("account confirmation alias = %#v", input)
	}
	if _, ok := input["confirm"]; ok {
		t.Fatalf("legacy account confirm must not reach closed Agent schema: %#v", input)
	}
	input = map[string]any{"message_limit": float64(20), "message_cursor": "cursor"}
	applyLegacyInputAliases("agent.chat.conversations.get", input)
	if input["limit"] != float64(20) || input["page_token"] != "cursor" {
		t.Fatalf("conversation aliases = %#v", input)
	}
	input = map[string]any{"name": "daily", "prompt": "summarize", "model_profile_id": "profile", "trigger": map[string]any{"cron": "* * * * *"}}
	applyLegacyInputAliases("agent.schedules.create", input)
	if _, ok := input["spec"]; !ok {
		t.Fatalf("schedule spec alias missing: %#v", input)
	}
	if _, ok := input["trigger"]; ok || input["cron"] != "* * * * *" {
		t.Fatalf("schedule trigger projection = %#v", input)
	}
}
