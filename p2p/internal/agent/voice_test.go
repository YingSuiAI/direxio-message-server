package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestVoiceSessionPinsRoleProfilesAndOwnerFence(t *testing.T) {
	store := storage.NewMemoryStore()
	result, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner-a", "voice-seed", storage.ModelProfileDefaults{ConversationClientProfileID: "chat", SpeechClientProfileID: "speech"}, []storage.ModelProfileSyncEntry{
		{ClientProfileID: "chat", Provider: "openai", Model: "gpt-4o", ModelKind: storage.ModelKindConversation, APIKey: stringPtr("chat-secret")},
		{ClientProfileID: "speech", Provider: "volc_voice", ModelKind: storage.ModelKindSpeech, ProviderConfig: map[string]any{"app_id": "123456781234567812345678"}, ProviderSecrets: map[string]string{"rtc_app_key": "rtc-secret", "access_key_id": "ak", "secret_access_key": "sk"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v := newVoiceCoordinator(store, func() string { return "owner-a" })
	generation := uint64(1)
	v.generation = func() uint64 { return generation }
	v.active = func(string) bool { return true }
	v.cfg.WebhookSecret, v.cfg.CustomLLMURL = "callback-secret", "https://voice.example.test/custom"
	response, apiErr := v.create(context.Background(), "owner-a", map[string]any{"conversation_id": "conv-1"})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	projection := response.(map[string]any)
	if projection["conversation_profile_id"] != result.Profiles[0].ProfileID || projection["speech_profile_id"] == "" {
		t.Fatalf("unexpected profile pins: %#v", projection)
	}
	if _, ok := projection["api_key"]; ok {
		t.Fatal("voice response leaked api_key")
	}
	if _, apiErr = v.end(context.Background(), "owner-b", map[string]any{"session_id": projection["session_id"]}); apiErr == nil || apiErr.Status != 403 {
		t.Fatalf("owner fence = %#v", apiErr)
	}
	generation = 2
	if _, apiErr = v.end(context.Background(), "owner-a", map[string]any{"session_id": projection["session_id"]}); apiErr == nil || apiErr.Status != 401 {
		t.Fatalf("generation fence = %#v", apiErr)
	}
	var secrets map[string]string
	speech, _, _ := store.GetModelProfile(context.Background(), "owner-a", projection["speech_profile_id"].(string))
	if err := json.Unmarshal([]byte(speech.APIKey), &secrets); err != nil || secrets["rtc_app_key"] != "rtc-secret" {
		t.Fatal("speech secret bundle unavailable")
	}
}

func stringPtr(value string) *string { return &value }
