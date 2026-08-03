package executionplanning

import (
	"context"
	"errors"
	"strings"
	"testing"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

type aiCredentialResolverStub struct {
	want     coreexecution.CredentialRef
	provider string
	err      error
	seen     int
}

func (s *aiCredentialResolverStub) ResolveCredential(_ context.Context, _ string, ref coreexecution.CredentialRef) error {
	s.seen++
	if ref != s.want {
		return coreexecution.ErrConflict
	}
	return s.err
}

func (s *aiCredentialResolverStub) ResolveExecutionSecretProvider(_ context.Context, _ string, ref coreexecution.CredentialRef) (string, error) {
	if ref != s.want {
		return "", coreexecution.ErrConflict
	}
	return s.provider, s.err
}

func TestResolveAIConfigurationPinsActiveCredentialOrExplicitExternalGate(t *testing.T) {
	ref := coreexecution.CredentialRef{Ref: "11111111-1111-4111-8111-111111111111", Purpose: coreexecution.AISecretPurposeProviderAPIKey, Revision: 3, BindingDigest: coreexecution.Digest(strings.Repeat("a", 64))}
	resolver := &aiCredentialResolverStub{want: ref, provider: "openai"}
	service := New(Config{Credentials: resolver})
	api := &coreexecution.AIConfiguration{Mode: coreexecution.AIAuthModeAPIKey, Provider: "openai", SecretRef: ref.Ref, SecretRevision: ref.Revision, SecretPurpose: ref.Purpose, SecretBindingDigest: ref.BindingDigest}
	resolved, err := service.resolveAIConfiguration(context.Background(), "@owner:example.test", api)
	if err != nil || resolved.CredentialRef() != ref || resolver.seen != 1 {
		t.Fatalf("resolved=%+v seen=%d err=%v", resolved, resolver.seen, err)
	}
	facts := BindingFacts{}
	if err := bindAISecret(&facts, resolved); err != nil || facts.SecretRefs[coreexecution.AISecretPurposeProviderAPIKey] != ref {
		t.Fatalf("bound facts=%+v err=%v", facts, err)
	}
	auth := &coreexecution.AIConfiguration{Mode: coreexecution.AIAuthModeAuthGate, Provider: "anthropic", Status: coreexecution.AIExternalAuthPending}
	if _, err := service.resolveAIConfiguration(context.Background(), "@owner:example.test", auth); err != nil || resolver.seen != 1 {
		t.Fatalf("external auth consulted secret resolver: seen=%d err=%v", resolver.seen, err)
	}
	resolver.provider = "openrouter"
	if _, err := service.resolveAIConfiguration(context.Background(), "@owner:example.test", api); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("provider metadata drift=%v", err)
	}

	service.cfg.Credentials = nil
	if _, err := service.resolveAIConfiguration(context.Background(), "@owner:example.test", api); !errors.Is(err, ErrNotReady) {
		t.Fatalf("api key without credential resolver=%v", err)
	}
}

func TestBindAISecretRejectsResolverDrift(t *testing.T) {
	configuration := &coreexecution.AIConfiguration{Mode: coreexecution.AIAuthModeAPIKey, Provider: "openai", SecretRef: "11111111-1111-4111-8111-111111111111", SecretRevision: 1, SecretPurpose: coreexecution.AISecretPurposeProviderAPIKey, SecretBindingDigest: coreexecution.Digest(strings.Repeat("a", 64))}
	facts := BindingFacts{SecretRefs: map[string]coreexecution.CredentialRef{coreexecution.AISecretPurposeProviderAPIKey: {Ref: configuration.SecretRef, Purpose: configuration.SecretPurpose, Revision: 2, BindingDigest: configuration.SecretBindingDigest}}}
	if err := bindAISecret(&facts, configuration); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("secret resolver drift=%v", err)
	}
}
