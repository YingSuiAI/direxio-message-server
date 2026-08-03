package storage

import (
	"context"
	"strings"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

// ResolveExecutionSecretProvider returns only the provider label from the
// exact active execution-secret revision. It exists so the plan compiler can
// bind an AI provider choice to authoritative encrypted-secret metadata
// without opening the secret value.
func (s *DatabaseExecutionSecretStore) ResolveExecutionSecretProvider(
	ctx context.Context,
	owner string,
	ref coreexecution.CredentialRef,
) (string, error) {
	if err := s.ResolveCredential(ctx, owner, ref); err != nil {
		return "", err
	}
	meta, err := s.GetExecutionSecret(ctx, owner, ref.Ref, ref.Revision)
	if err != nil {
		return "", err
	}
	provider := strings.TrimSpace(meta.Provider)
	if meta.Status != "active" || meta.Revision != ref.Revision || meta.Purpose != ref.Purpose ||
		meta.BindingDigest != ref.BindingDigest || provider == "" || provider != meta.Provider {
		return "", coreexecution.ErrConflict
	}
	return provider, nil
}
