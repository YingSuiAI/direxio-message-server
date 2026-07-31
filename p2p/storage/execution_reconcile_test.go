package storage

import (
	"testing"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func TestExecutionSecretParameterLeaseDigestMatchesRunnerContract(t *testing.T) {
	lease := coreaws.SecretParameterLease{
		SchemaVersion:    "execution-secret-parameter/v1",
		OwnerID:          "@owner:example.org",
		RunID:            "11111111-1111-4111-8111-111111111111",
		ProvisionStageID: "22222222-2222-4222-8222-222222222222",
		TargetID:         "33333333-3333-4333-8333-333333333333",
		TargetRevision:   2,
		TargetDigest:     coreexecution.Digest("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		SecretRef:        coreexecution.CredentialRef{Ref: "44444444-4444-4444-8444-444444444444", Purpose: coreaws.ExecutionSecretPurposeAIProviderAPIKey, Revision: 3, BindingDigest: coreexecution.Digest("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")},
		ParameterName:    "/dirextalk/execution-v2/33333333-3333-4333-8333-333333333333/55555555-5555-4555-8555-555555555555/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProviderVersion:  7,
		FenceDigest:      coreexecution.Digest("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		RequestDigest:    coreexecution.Digest("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
	}
	want, err := coreexecution.CanonicalDigest(struct {
		SchemaVersion   string
		OwnerID         string
		RunID           string
		StageID         string
		TargetID        string
		TargetRevision  uint64
		TargetDigest    coreexecution.Digest
		SecretRef       coreexecution.CredentialRef
		ParameterName   string
		ProviderVersion int64
		FenceDigest     coreexecution.Digest
		RequestDigest   coreexecution.Digest
	}{lease.SchemaVersion, lease.OwnerID, lease.RunID, lease.ProvisionStageID, lease.TargetID, lease.TargetRevision, lease.TargetDigest, lease.SecretRef, lease.ParameterName, lease.ProviderVersion, lease.FenceDigest, lease.RequestDigest})
	if err != nil {
		t.Fatal(err)
	}
	got, err := executionSecretParameterLeaseDigest(lease)
	if err != nil || got != want || !got.Valid() {
		t.Fatalf("digest=%q want=%q err=%v", got, want, err)
	}
}

func TestNewExecutionReconcilerWithSecretProvisionRequiresReadbackBoundary(t *testing.T) {
	if got := NewExecutionReconcilerWithSecretProvision(nil, nil, nil, nil); got != nil {
		t.Fatal("nil dependencies must not construct reconciler")
	}
}
