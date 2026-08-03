package nativeagent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type testModelProfileResolver struct{}

func (testModelProfileResolver) ResolveModelProfile(context.Context, string) (ServerModelProfile, error) {
	return ServerModelProfile{Provider: "deepseek", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1", APIKey: "server-secret", SystemPrompt: "server prompt"}, nil
}

func (testModelProfileResolver) ResolveDefaultModelProfile(context.Context, string) (ServerModelProfile, error) {
	return ServerModelProfile{Provider: "anthropic", Model: "claude", BaseURL: "https://api.anthropic.com/v1", APIKey: "default-secret", ModelKind: "conversation", InputModalities: []string{"text", "image"}}, nil
}

type pinnedTestModelProfileResolver struct {
	err                 error
	requestedRevision   int64
	requestedCredential int64
	wrongMetadata       bool
}

func (p *pinnedTestModelProfileResolver) ResolveModelProfile(context.Context, string) (ServerModelProfile, error) {
	return ServerModelProfile{Provider: "deepseek", Model: "current", BaseURL: "https://api.deepseek.com/v1", APIKey: "current-secret"}, nil
}

func (p *pinnedTestModelProfileResolver) ResolveModelProfilePinned(_ context.Context, _ string, revision, credential int64) (ServerModelProfile, error) {
	p.requestedRevision, p.requestedCredential = revision, credential
	if p.err != nil {
		return ServerModelProfile{}, p.err
	}
	if p.wrongMetadata {
		revision++
	}
	return ServerModelProfile{Provider: "deepseek", Model: "pinned", BaseURL: "https://api.deepseek.com/v1", APIKey: "pinned-secret", Revision: revision, CredentialVersion: credential}, nil
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
	_, err = runtime.resolveModelProfileForRequest(context.Background(), map[string]any{"model_profile_id": "client-local", "model_profile": map[string]any{"provider": "openai", "model": "gpt", "base_url": "https://example.invalid/v1", "api_key": "legacy-key"}})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") || strings.Contains(err.Error(), "legacy-key") {
		t.Fatalf("conflicting inline/server profile error = %v", err)
	}
}

func TestInlineModelProfileCannotBypassPinnedServerSelection(t *testing.T) {
	runtime := New(Config{ModelProfiles: testModelProfileResolver{}})
	for _, params := range []map[string]any{
		{"model_profile_revision": int64(3), "credential_version": int64(2), "model_profile": map[string]any{"provider": "openai", "api_key": "inline-secret"}},
		{"model_profile_id": "profile-1", "model_profile": map[string]any{"provider": "openai", "api_key": "inline-secret"}},
	} {
		_, err := runtime.resolveModelProfileForRequest(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "cannot be combined") || strings.Contains(err.Error(), "inline-secret") {
			t.Fatalf("inline bypass accepted/leaked: err=%v", err)
		}
	}
}

func TestInlineModelProfileWithServerSelectionFailsWhenStoreUnavailable(t *testing.T) {
	runtime := New(Config{})
	_, err := runtime.resolveModelProfileForRequest(context.Background(), map[string]any{
		"model_profile_id": "profile-1",
		"model_profile":    map[string]any{"provider": "openai", "api_key": "inline-secret"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") || strings.Contains(err.Error(), "inline-secret") {
		t.Fatalf("unavailable-store conflict = %v", err)
	}
}

func TestProfilePinRequiresServerModelProfileID(t *testing.T) {
	runtime := New(Config{ModelProfiles: testModelProfileResolver{}})
	_, err := runtime.resolveModelProfileForRequest(context.Background(), map[string]any{"model_profile_revision": int64(3), "credential_version": int64(2)})
	if err == nil || !strings.Contains(err.Error(), "server model profile ID is required") {
		t.Fatalf("dangling profile pin accepted: %v", err)
	}
}

func TestPinnedServerProfileUsesExactRevisionAndCredential(t *testing.T) {
	resolver := &pinnedTestModelProfileResolver{}
	runtime := New(Config{ModelProfiles: resolver})
	profile, err := runtime.resolveModelProfileForRequest(context.Background(), map[string]any{
		"model_profile_id": "profile-1", "model_profile_revision": int64(7), "credential_version": int64(3),
	})
	if err != nil || profile.Model != "pinned" || resolver.requestedRevision != 7 || resolver.requestedCredential != 3 {
		t.Fatalf("pinned profile = %#v, resolver=%#v, err=%v", profile, resolver, err)
	}
}

func TestPinnedServerProfileStaleRevisionOrCredentialFailsWithoutSecret(t *testing.T) {
	for _, stale := range []string{"stale profile revision", "stale credential version"} {
		resolver := &pinnedTestModelProfileResolver{err: errors.New(stale + ": pinned-secret")}
		runtime := New(Config{ModelProfiles: resolver})
		_, err := runtime.resolveModelProfileForRequest(context.Background(), map[string]any{
			"model_profile_id": "profile-1", "model_profile_revision": int64(7), "credential_version": int64(3),
		})
		if err == nil || !strings.Contains(err.Error(), "pinned server model profile unavailable") || strings.Contains(err.Error(), stale) || strings.Contains(err.Error(), "pinned-secret") {
			t.Fatalf("stale pinned profile error=%v", err)
		}
	}
}

func TestPinnedServerProfileFailsClosedWhenResolverDoesNotSupportPins(t *testing.T) {
	runtime := New(Config{ModelProfiles: testModelProfileResolver{}})
	_, err := runtime.resolveModelProfileForRequest(context.Background(), map[string]any{
		"model_profile_id": "profile-1", "model_profile_revision": int64(7), "credential_version": int64(3),
	})
	if err == nil || !strings.Contains(err.Error(), "pinned server model profiles are unavailable") || strings.Contains(err.Error(), "server-secret") {
		t.Fatalf("unsupported pinned resolver error=%v", err)
	}
}

func TestPinnedServerProfileRejectsMismatchedReturnedMetadata(t *testing.T) {
	resolver := &pinnedTestModelProfileResolver{wrongMetadata: true}
	runtime := New(Config{ModelProfiles: resolver})
	_, err := runtime.resolveModelProfileForRequest(context.Background(), map[string]any{
		"model_profile_id": "profile-1", "model_profile_revision": int64(7), "credential_version": int64(3),
	})
	if err == nil || !strings.Contains(err.Error(), "pinned server model profile unavailable") || strings.Contains(err.Error(), "pinned-secret") {
		t.Fatalf("mismatched pinned metadata accepted/leaked: %v", err)
	}
}

func TestDefaultConversationProfileResolvesWithoutRequestSelection(t *testing.T) {
	runtime := New(Config{ModelProfiles: testModelProfileResolver{}})
	profile, err := runtime.resolveModelProfileForRequest(context.Background(), map[string]any{})
	if err != nil || profile.Provider != "anthropic" || profile.Model != "claude" || profile.APIKey != "default-secret" {
		t.Fatalf("default profile = %#v, err=%v", profile, err)
	}
}
