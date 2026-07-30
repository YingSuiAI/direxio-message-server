package storage

import (
	"context"
	"testing"
)

func TestMemoryModelProfileRolesAndDefaults(t *testing.T) {
	store := NewMemoryStore()
	result, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner", "roles-1", ModelProfileDefaults{
		ConversationClientProfileID: "chat",
		EmbeddingClientProfileID:    "embed",
		SpeechClientProfileID:       "voice",
	}, []ModelProfileSyncEntry{
		{ClientProfileID: "chat", Provider: "openai", Model: "gpt", InputModalities: []string{"text", "image"}},
		{ClientProfileID: "embed", Provider: "openai", Model: "embed", ModelKind: ModelKindEmbedding},
		{ClientProfileID: "voice", Provider: "volc_voice", ModelKind: ModelKindSpeech, ProviderConfig: map[string]any{"app_id": "app"}, ProviderSecrets: map[string]string{"rtc_app_key": "rtc", "access_key_id": "access", "secret_access_key": "secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Defaults.ConversationClientProfileID != "chat" || result.Defaults.EmbeddingClientProfileID != "embed" || result.Defaults.SpeechClientProfileID != "voice" {
		t.Fatalf("defaults = %#v", result.Defaults)
	}
	if len(result.Profiles) != 3 {
		t.Fatalf("profiles = %#v", result.Profiles)
	}
	for _, profile := range result.Profiles {
		if profile.ModelKind == ModelKindSpeech {
			if profile.APIKey == "" || !profile.APIKeyConfigured || !profile.ProviderSecretStatus["rtc_app_key"] {
				t.Fatalf("speech credential state = %#v", profile)
			}
		}
	}
	voice := result.Profiles[2]
	resolvedVoice, err := store.ResolveModelProfileVersion(context.Background(), "owner", voice.ProfileID, voice.CredentialVersion)
	if err != nil || !resolvedVoice.APIKeyConfigured || resolvedVoice.APIKey == "" {
		t.Fatalf("speech credential persistence = %v", err)
	}
	if _, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner", "roles-invalid", ModelProfileDefaults{ConversationClientProfileID: "embed"}, nil); err != ErrModelProfileInvalid {
		t.Fatalf("kind mismatch error = %v", err)
	}
	profilesByClient := make(map[string]ModelProfile)
	for _, profile := range result.Profiles {
		profilesByClient[profile.ClientProfileID] = profile
	}
	if err := store.DeleteModelProfile(context.Background(), "owner", "delete-embed", profilesByClient["embed"].ProfileID, int64Ptr(profilesByClient["embed"].Revision)); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListModelProfiles(context.Background(), "owner", 0, "")
	if err != nil || listed.Defaults.ConversationClientProfileID != "chat" || listed.Defaults.EmbeddingClientProfileID != "" || listed.Defaults.SpeechClientProfileID != "voice" {
		t.Fatalf("embedding default deletion = %#v, %v", listed.Defaults, err)
	}
	if err := store.DeleteModelProfile(context.Background(), "owner", "delete-voice", profilesByClient["voice"].ProfileID, int64Ptr(profilesByClient["voice"].Revision)); err != nil {
		t.Fatal(err)
	}
	listed, err = store.ListModelProfiles(context.Background(), "owner", 0, "")
	if err != nil || listed.Defaults.ConversationClientProfileID != "chat" || listed.Defaults.SpeechClientProfileID != "" {
		t.Fatalf("speech default deletion = %#v, %v", listed.Defaults, err)
	}
	key := "generic-secret"
	if _, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner", "speech-to-chat", ModelProfileDefaults{}, []ModelProfileSyncEntry{{ClientProfileID: "voice", Provider: "openai", Model: "gpt"}}); err != ErrModelProfileInvalid {
		t.Fatalf("speech to generic without key = %v", err)
	}
	if _, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner", "speech-to-chat-key", ModelProfileDefaults{}, []ModelProfileSyncEntry{{ClientProfileID: "voice", Provider: "openai", Model: "gpt", APIKey: &key}}); err != nil {
		t.Fatalf("speech to generic with key = %v", err)
	}
	if _, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner", "unknown-secret", ModelProfileDefaults{}, []ModelProfileSyncEntry{{ClientProfileID: "voice-2", Provider: "volc_voice", ModelKind: ModelKindSpeech, ProviderSecrets: map[string]string{"rtc_app_key": "rtc", "access_key_id": "access", "secret_access_key": "secret", "unexpected": "reject"}}}); err != ErrModelProfileInvalid {
		t.Fatalf("unknown speech secret = %v", err)
	}
	if _, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner", "generic-to-speech-create", ModelProfileDefaults{}, []ModelProfileSyncEntry{{ClientProfileID: "generic-2", Provider: "openai", Model: "embed", ModelKind: ModelKindEmbedding}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner", "generic-to-speech", ModelProfileDefaults{}, []ModelProfileSyncEntry{{ClientProfileID: "generic-2", Provider: "volc_voice", ModelKind: ModelKindSpeech}}); err != ErrModelProfileInvalid {
		t.Fatalf("generic to speech without bundle = %v", err)
	}
}
