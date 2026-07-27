package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestServerModelProfileActionsRedactAPIKeyAndPreserveOmission(t *testing.T) {
	store := storage.NewMemoryStore()
	module := New(Config{ModelProfiles: store, OwnerID: func() string { return "owner" }})
	syncAction := module.Handlers()["agent.model_profiles.sync"]
	result, actionErr := syncAction(context.Background(), map[string]any{
		"idempotency_key": "batch-1",
		"entries":         []any{map[string]any{"client_profile_id": "client-1", "provider": "deepseek", "model": "deepseek-chat", "base_url": "https://api.deepseek.com/v1", "api_key": "secret-key"}},
	})
	if actionErr != nil {
		t.Fatalf("sync: %#v", actionErr)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "secret-key") || !strings.Contains(string(encoded), "api_key_configured") {
		t.Fatalf("redacted sync response = %s", encoded)
	}

	profiles := result.(map[string]any)["profiles"].([]map[string]any)
	profileID := profiles[0]["profile_id"].(string)
	_, actionErr = syncAction(context.Background(), map[string]any{"idempotency_key": "batch-2", "entries": []any{map[string]any{"client_profile_id": "client-1", "provider": "deepseek", "model": "deepseek-reasoner"}}})
	if actionErr != nil {
		t.Fatalf("sync omission: %#v", actionErr)
	}
	profile, ok, err := store.GetModelProfile(context.Background(), "owner", profileID)
	if err != nil || !ok || profile.APIKey != "secret-key" {
		t.Fatalf("preserved profile = %#v, %v, %v", profile, ok, err)
	}
}

func TestServerModelProfileActionsRejectLooseNumericDTOsAndPreserveZero(t *testing.T) {
	store := storage.NewMemoryStore()
	module := New(Config{ModelProfiles: store, OwnerID: func() string { return "owner" }})
	syncAction := module.Handlers()["agent.model_profiles.sync"]
	base := map[string]any{"idempotency_key": "numeric-1", "entries": []any{map[string]any{"client_profile_id": "client-1", "provider": "deepseek", "model": "deepseek-chat", "api_key": "secret"}}}
	for name, value := range map[string]any{"fractional-int": 1.5, "string-int": "3", "nan-float": math.NaN(), "infinite-float": math.Inf(1)} {
		params := cloneMap(base)
		entry := params["entries"].([]any)[0].(map[string]any)
		if name == "fractional-int" || name == "string-int" {
			entry["max_output_tokens"] = value
		} else {
			entry["temperature"] = value
		}
		params["idempotency_key"] = "numeric-" + name
		if _, actionErr := syncAction(context.Background(), params); actionErr == nil {
			t.Fatalf("%s unexpectedly accepted", name)
		}
	}
	zero := cloneMap(base)
	zero["idempotency_key"] = "numeric-zero"
	zero["entries"] = []any{map[string]any{"client_profile_id": "client-zero", "provider": "deepseek", "model": "deepseek-chat", "api_key": "secret", "max_output_tokens": float64(0), "temperature": float64(0)}}
	if _, actionErr := syncAction(context.Background(), zero); actionErr != nil {
		t.Fatalf("explicit zero rejected: %#v", actionErr)
	}
	listed, err := store.ListModelProfiles(context.Background(), "owner", 0, "")
	if err != nil || len(listed.Profiles) != 1 {
		t.Fatalf("zero profile list = %#v, err=%v", listed, err)
	}
	profile := listed.Profiles[0]
	if profile.ClientProfileID != "client-zero" || profile.MaxOutputTokens != 0 || profile.Temperature == nil || *profile.Temperature != 0 {
		t.Fatalf("explicit zero profile = %#v", profile)
	}
}

func TestServerModelProfileActionsRejectNonStringDTOsWithoutMutation(t *testing.T) {
	store := storage.NewMemoryStore()
	module := New(Config{ModelProfiles: store, OwnerID: func() string { return "owner" }})
	syncAction := module.Handlers()["agent.model_profiles.sync"]
	_, actionErr := syncAction(context.Background(), map[string]any{
		"idempotency_key": "strict-seed",
		"entries": []any{map[string]any{
			"client_profile_id": "client-1",
			"provider":          "deepseek",
			"display_name":      "Seed",
			"base_url":          "https://api.deepseek.com/v1",
			"model":             "deepseek-chat",
			"system_prompt":     "be concise",
			"reasoning_effort":  "medium",
			"api_key":           "seed-secret",
		}},
	})
	if actionErr != nil {
		t.Fatalf("seed sync: %#v", actionErr)
	}
	// Resolve the generated id without depending on its format.
	seeded, listErr := store.ListModelProfiles(context.Background(), "owner", 0, "")
	if listErr != nil || len(seeded.Profiles) != 1 {
		t.Fatalf("seed list: %#v, err=%v", seeded, listErr)
	}
	before := seeded.Profiles[0]
	if before.ClientProfileID != "client-1" {
		t.Fatalf("seed profile = %#v", before)
	}

	fields := map[string]string{
		"idempotency_key":           "strict-idempotency",
		"default_client_profile_id": "strict-default",
		"client_profile_id":         "strict-client",
		"provider":                  "strict-provider",
		"display_name":              "strict-display",
		"base_url":                  "strict-base",
		"model":                     "strict-model",
		"system_prompt":             "strict-prompt",
		"reasoning_effort":          "strict-reasoning",
		"api_key":                   "strict-key",
	}
	for field, idempotency := range fields {
		params := map[string]any{
			"idempotency_key": idempotency,
			"entries": []any{map[string]any{
				"client_profile_id": "client-1",
				"provider":          "deepseek",
				field:               123,
			}},
		}
		if field == "idempotency_key" {
			params["idempotency_key"] = 123
		}
		if field == "default_client_profile_id" {
			params["default_client_profile_id"] = 123
		}
		if _, actionErr := syncAction(context.Background(), params); actionErr == nil {
			t.Fatalf("%s unexpectedly accepted non-string", field)
		}
	}
	listAction := module.Handlers()["agent.model_profiles.list"]
	if _, actionErr := listAction(context.Background(), map[string]any{"page_token": 123}); actionErr == nil {
		t.Fatal("page_token unexpectedly accepted non-string")
	}
	listed, err := store.ListModelProfiles(context.Background(), "owner", 0, "")
	if err != nil || len(listed.Profiles) != 1 {
		t.Fatalf("profile mutated after rejected DTOs: %#v, err=%v", listed, err)
	}
	after := listed.Profiles[0]
	if after.ProfileID != before.ProfileID || after.Revision != before.Revision || after.APIKey != before.APIKey || after.Model != before.Model {
		t.Fatalf("profile changed after rejected DTOs: before=%#v after=%#v", before, after)
	}
}

type pinnedTurnRunner struct {
	mu         sync.Mutex
	executions int
	params     []map[string]any
}

func (r *pinnedTurnRunner) Apply(context.Context, string) error { return nil }
func (r *pinnedTurnRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}
func (r *pinnedTurnRunner) Stream(_ context.Context, _ string, params map[string]any, emit func(nativeagent.Event) error) error {
	r.mu.Lock()
	r.executions++
	r.params = append(r.params, cloneMap(params))
	r.mu.Unlock()
	return emit(nativeagent.Event{Event: "done", Data: map[string]any{"ok": true}})
}

func TestDurableTurnPinsServerModelProfileAcrossRotationDeleteAndReactivation(t *testing.T) {
	store := storage.NewMemoryStore()
	created, err := store.SyncModelProfiles(context.Background(), "owner", "turn-profile-create", "", []storage.ModelProfileSyncEntry{{ClientProfileID: "client-1", Provider: "deepseek", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1", APIKey: agentStringPtr("turn-secret")}})
	if err != nil || len(created.Profiles) != 1 {
		t.Fatalf("profile create: %#v, %v", created, err)
	}
	runner := &pinnedTurnRunner{}
	module := New(Config{Runner: runner, Turns: store, ModelProfiles: store, OwnerID: func() string { return "owner" }})
	params := map[string]any{"turn_id": "turn-pinned", "conversation_id": "conversation-pinned", "prompt": "hello", "model_profile_id": created.Profiles[0].ProfileID}
	if err := module.DurableStream(context.Background(), "owner", "agent.chat.stream", params, func(agentturns.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("durable stream: %v", err)
	}
	current, _, _ := store.GetModelProfile(context.Background(), "owner", created.Profiles[0].ProfileID)
	if _, err := store.SyncModelProfiles(context.Background(), "owner", "turn-profile-rotate", "", []storage.ModelProfileSyncEntry{{ClientProfileID: "client-1", ExpectedRevision: agentInt64Ptr(current.Revision), Provider: "openai", Model: "gpt-4o", BaseURL: "https://api.openai.com/v1"}}); err != nil {
		t.Fatalf("profile rotate: %v", err)
	}
	current, _, _ = store.GetModelProfile(context.Background(), "owner", current.ProfileID)
	if err := store.DeleteModelProfile(context.Background(), "owner", "turn-profile-delete", current.ProfileID, agentInt64Ptr(current.Revision)); err != nil {
		t.Fatalf("profile delete: %v", err)
	}
	if _, err := store.SyncModelProfiles(context.Background(), "owner", "turn-profile-reactivate", "", []storage.ModelProfileSyncEntry{{ClientProfileID: "client-1", ExpectedRevision: agentInt64Ptr(current.Revision + 1), Provider: "anthropic", Model: "claude-3", BaseURL: "https://api.anthropic.com/v1"}}); err != nil {
		t.Fatalf("profile reactivate: %v", err)
	}
	if err := module.DurableStream(context.Background(), "owner", "agent.chat.stream", params, func(agentturns.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("durable replay: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.executions != 1 || len(runner.params) != 1 {
		t.Fatalf("runner executions=%d params=%d", runner.executions, len(runner.params))
	}
	pinned, ok := runner.params[0]["model_profile"].(map[string]any)
	if !ok || pinned["model"] != "deepseek-chat" || pinned["provider"] != "deepseek" || pinned["api_key"] != "turn-secret" {
		t.Fatalf("runner pinned profile = %#v", runner.params[0])
	}
	turn, ok, err := store.GetAgentTurn(context.Background(), "owner", "turn-pinned")
	if err != nil || !ok || turn.ModelProfileRevision != created.Profiles[0].Revision || turn.CredentialVersion != created.Profiles[0].CredentialVersion {
		t.Fatalf("persisted pinned turn = %#v, ok=%v err=%v", turn, ok, err)
	}
}

func TestDurableImageTurnValidatesBeforeReserveAndSupportsDigestOnlyReattach(t *testing.T) {
	store := storage.NewMemoryStore()
	created, err := store.SyncModelProfiles(context.Background(), "owner", "image-profile-create", "", []storage.ModelProfileSyncEntry{{ClientProfileID: "image-client", Provider: "openai", Model: "gpt-4o", BaseURL: "https://api.openai.com/v1", APIKey: agentStringPtr("secret"), ModelKind: storage.ModelKindConversation, InputModalities: []string{"text", "image"}}})
	if err != nil || len(created.Profiles) != 1 {
		t.Fatalf("profile create: %#v %v", created, err)
	}
	runner := &pinnedTurnRunner{}
	module := New(Config{Runner: runner, Turns: store, ModelProfiles: store, OwnerID: func() string { return "owner" }})
	invalid := map[string]any{"turn_id": "image-invalid", "conversation_id": "image-conversation", "prompt": "look", "model_profile_id": created.Profiles[0].ProfileID, "attachments": []any{map[string]any{"mime_type": "image/gif", "data_base64": base64.StdEncoding.EncodeToString([]byte("x"))}}}
	if err := module.DurableStream(context.Background(), "owner", "agent.chat.stream", invalid, func(agentturns.StreamEvent) error { return nil }); err == nil {
		t.Fatal("invalid image was accepted")
	}
	if _, ok, _ := store.GetAgentTurn(context.Background(), "owner", "image-invalid"); ok {
		t.Fatal("invalid image reserved a durable turn")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("image"))
	params := map[string]any{"turn_id": "image-valid", "conversation_id": "image-conversation", "prompt": "look", "model_profile_id": created.Profiles[0].ProfileID, "attachments": []any{map[string]any{"type": "image", "mime_type": "image/png", "data_base64": encoded}}}
	if err := module.DurableStream(context.Background(), "owner", "agent.chat.stream", params, func(agentturns.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("valid image turn: %v", err)
	}
	if err := module.DurableStream(context.Background(), "owner", "agent.chat.stream", map[string]any{"turn_id": "image-valid", "after_seq": int64(1)}, func(agentturns.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("digest-only reattach: %v", err)
	}
	changed := cloneMap(params)
	changed["attachments"] = []any{map[string]any{"type": "image", "mime_type": "image/png", "data_base64": base64.StdEncoding.EncodeToString([]byte("changed"))}}
	if err := module.DurableStream(context.Background(), "owner", "agent.chat.stream", changed, func(agentturns.StreamEvent) error { return nil }); !errors.Is(err, agentturns.ErrTurnIDReused) {
		t.Fatalf("changed image error = %v, want turn reuse", err)
	}
	other, err := store.SyncModelProfiles(context.Background(), "owner", "image-profile-other", "", []storage.ModelProfileSyncEntry{{ClientProfileID: "image-client-other", Provider: "openai", Model: "gpt-4o-mini", BaseURL: "https://api.openai.com/v1", APIKey: agentStringPtr("secret"), ModelKind: storage.ModelKindConversation, InputModalities: []string{"text", "image"}}})
	if err != nil || len(other.Profiles) == 0 {
		t.Fatalf("other profile create: %#v %v", other, err)
	}
	profileChanged := cloneMap(params)
	profileChanged["model_profile_id"] = other.Profiles[len(other.Profiles)-1].ProfileID
	if err := module.DurableStream(context.Background(), "owner", "agent.chat.stream", profileChanged, func(agentturns.StreamEvent) error { return nil }); !errors.Is(err, agentturns.ErrTurnIDReused) {
		t.Fatalf("changed profile error = %v, want turn reuse", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.executions != 1 {
		t.Fatalf("reattach reran runtime: %d", runner.executions)
	}
}

func agentStringPtr(value string) *string { return &value }
func agentInt64Ptr(value int64) *int64    { return &value }
