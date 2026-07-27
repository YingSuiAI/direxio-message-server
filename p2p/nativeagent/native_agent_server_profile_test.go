package nativeagent

import (
	"context"
	"testing"
)

type testModelProfileResolver struct{}

func (testModelProfileResolver) ResolveModelProfile(context.Context, string) (ServerModelProfile, error) {
	return ServerModelProfile{Provider: "deepseek", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1", APIKey: "server-secret", SystemPrompt: "server prompt"}, nil
}

func TestServerModelProfileIDResolvesWithoutRequestKey(t *testing.T) {
	runtime := New(Config{ModelProfiles: testModelProfileResolver{}})
	profile, err := runtime.resolveModelProfileForRequest(context.Background(), map[string]any{"model_profile_id": "profile-1"})
	if err != nil {
		t.Fatal(err)
	}
	if profile.APIKey != "server-secret" || profile.Provider != "deepseek" || profile.SystemPrompt != "server prompt" {
		t.Fatalf("profile = %#v", profile)
	}
	legacy, err := runtime.resolveModelProfileForRequest(context.Background(), map[string]any{"model_profile_id": "client-local", "model_profile": map[string]any{"provider": "openai", "model": "gpt", "base_url": "https://example.invalid/v1", "api_key": "legacy-key"}})
	if err != nil || legacy.APIKey != "legacy-key" || legacy.Provider != "openai" {
		t.Fatalf("legacy inline profile = %#v, err=%v", legacy, err)
	}
}
