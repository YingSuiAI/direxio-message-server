package p2p

import (
	"context"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type nativeModelProfileResolver struct {
	store storage.ModelProfileStore
	owner func() string
}

func (r nativeModelProfileResolver) ResolveModelProfile(ctx context.Context, profileID string) (nativeagent.ServerModelProfile, error) {
	profile, err := r.store.ResolveModelProfile(ctx, r.owner(), profileID)
	if err != nil {
		return nativeagent.ServerModelProfile{}, err
	}
	return nativeagent.ServerModelProfile{
		ProfileID: profile.ProfileID, ClientProfileID: profile.ClientProfileID,
		DisplayName: profile.DisplayName, Provider: profile.Provider,
		BaseURL: profile.BaseURL, Model: profile.Model, SystemPrompt: profile.SystemPrompt,
		APIKey: profile.APIKey, APIKeyConfigured: profile.APIKeyConfigured,
		Temperature: profile.Temperature, TopP: profile.TopP,
		MaxOutputTokens: profile.MaxOutputTokens, ContextWindow: profile.ContextWindow,
		ReasoningEffort: profile.ReasoningEffort,
		ModelKind:       profile.ModelKind, InputModalities: append([]string(nil), profile.InputModalities...),
		Revision: profile.Revision, CredentialVersion: profile.CredentialVersion,
	}, nil
}
