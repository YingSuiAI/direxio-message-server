package aws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
)

type credentialStore struct{ value coreaws.Credentials }

func (s credentialStore) GetCredentialRevision(_ context.Context, _ string, revision int64) (coreaws.Credentials, error) {
	if revision != s.value.Revision {
		return coreaws.Credentials{}, errors.New("revision unavailable")
	}
	return s.value, nil
}

type revisionCredentialStore struct {
	values map[int64]coreaws.Credentials
	seen   int64
}

func (s *revisionCredentialStore) GetCredentialRevision(_ context.Context, _ string, revision int64) (coreaws.Credentials, error) {
	s.seen = revision
	v, ok := s.values[revision]
	if !ok {
		return coreaws.Credentials{}, errors.New("revision unavailable")
	}
	return v, nil
}

func TestDurableCredentialResolverRequiresVerifiedPinnedRevision(t *testing.T) {
	id := uuid.NewString()
	now := time.Now().UTC()
	base := coreaws.RehydrateCredentials(id, "prod", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/prod", []byte("AKIA"), []byte("secret"), nil, 1, 1, now, now)
	r, err := NewCredentialResolver(credentialStore{value: base})
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.ResolveCredential(context.Background(), id, 1)
	if err != nil || h.SecretAccessKey != "secret" || h.ReferenceID != id {
		t.Fatalf("handle=%+v err=%v", h, err)
	}
	raw, _ := json.Marshal(h)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "AKIA") {
		t.Fatalf("credential secret serialized: %s", raw)
	}
	for _, c := range []coreaws.Credentials{
		coreaws.RehydrateCredentials(id, "prod", "us-east-1", "123456789012", "", []byte("AKIA"), []byte("secret"), nil, 0, 1, now, now),
		coreaws.RehydrateCredentials(id, "prod", "us-east-1", "123456789012", "", []byte("AKIA"), []byte("rotated"), nil, 1, 2, now, now),
	} {
		bad, _ := NewCredentialResolver(credentialStore{value: c})
		if _, err := bad.ResolveCredential(context.Background(), id, 1); !errors.Is(err, ErrPrecondition) {
			t.Fatalf("unverified/rotated credential accepted: %v", err)
		}
	}
}

func TestDurableCredentialResolverDoesNotFollowCredentialUpdate(t *testing.T) {
	id := uuid.NewString()
	now := time.Now().UTC()
	store := &revisionCredentialStore{values: map[int64]coreaws.Credentials{
		1: coreaws.RehydrateCredentials(id, "prod", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/prod", []byte("AKIA-OLD"), []byte("secret-old"), nil, 1, 1, now, now),
		2: coreaws.RehydrateCredentials(id, "prod", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/prod", []byte("AKIA-NEW"), []byte("secret-new"), nil, 2, 2, now, now),
	}}
	r, err := NewCredentialResolver(store)
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.ResolveCredential(context.Background(), id, 1)
	if err != nil || h.Revision != 1 || h.AccessKeyID != "AKIA-OLD" || h.SecretAccessKey != "secret-old" || store.seen != 1 {
		t.Fatalf("pinned revision was not used: handle=%+v seen=%d err=%v", h, store.seen, err)
	}
}

func TestCredentialReferenceAndPlanDigestPinRevision(t *testing.T) {
	credentialID := uuid.NewString()
	plan := coreworkload.Plan{SecretGrantRefs: []coreworkload.SecretGrantRef{{ReferenceID: credentialID, Purpose: coreconfirmation.SecretPurposeAWSCredential, Revision: 1, BindingDigest: coreconfirmation.Digest(strings.Repeat("a", 64))}}}
	ref, revision, err := CredentialReference(plan)
	if err != nil || ref != credentialID || revision != 1 {
		t.Fatalf("pinned reference = %q/%d, err=%v", ref, revision, err)
	}
	one := coreworkload.PlanInputDigest(plan)
	plan.SecretGrantRefs[0].Revision = 2
	two := coreworkload.PlanInputDigest(plan)
	if one == two {
		t.Fatal("credential revision is missing from workload plan digest")
	}
	binding := coreworkload.BindingForPlan(coreworkload.Plan{Revision: 1, Digest: strings.Repeat("b", 64), SecretGrantRefs: []coreworkload.SecretGrantRef{{ReferenceID: credentialID, Purpose: coreconfirmation.SecretPurposeAWSCredential, Revision: 2, BindingDigest: coreconfirmation.Digest(strings.Repeat("c", 64))}}}, uuid.NewString())
	if len(binding.SecretGrants) != 1 || binding.SecretGrants[0].Revision != 2 {
		t.Fatalf("confirmation binding lost credential revision: %+v", binding.SecretGrants)
	}
	plan.SecretGrantRefs[0].Revision = 0
	if _, _, err := CredentialReference(plan); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unversioned credential grant accepted: %v", err)
	}
}

func TestCanonicalSecretResolverAndTargetBinding(t *testing.T) {
	r := CanonicalSecretReference{}
	arn, err := r.ResolveSecretReference(context.Background(), "arn:aws:ssm:us-east-1:123456789012:parameter/app/token")
	if err != nil || arn == "" {
		t.Fatalf("canonical ARN rejected: %q %v", arn, err)
	}
	for _, value := range []string{"plaintext", "arn:aws:ssm:us-east-1:123456789012:wrong/x", "arn:aws:secretsmanager:us-east-1:123456789012:secret:x//bad"} {
		if _, err := r.ResolveSecretReference(context.Background(), value); err == nil {
			t.Fatalf("invalid ARN accepted: %q", value)
		}
	}
	if err := ValidateSecretARNForTarget(arn, "us-east-1", "123456789012"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSecretARNForTarget(arn, "us-west-2", "123456789012"); err == nil {
		t.Fatal("cross-region ARN accepted")
	}
	gov := "arn:aws-us-gov:ssm:us-gov-west-1:123456789012:parameter/app/token"
	if err := ValidateSecretARNForTarget(gov, "us-gov-west-1", "123456789012"); err != nil {
		t.Fatalf("gov partition rejected: %v", err)
	}
	if err := ValidateSecretARNForTarget(arn, "cn-north-1", "123456789012"); err == nil {
		t.Fatal("commercial partition accepted for China region")
	}
	if _, err := r.ResolveSecretReferenceExact(context.Background(), uuid.NewString(), coreconfirmation.SecretPurposeSkillSecret, coreconfirmation.Digest(coreworkload.SecretGrantBindingDigest(arn, coreconfirmation.SecretPurposeSkillSecret))); err == nil {
		t.Fatal("UUID secret reference accepted")
	}
	if _, err := r.ResolveSecretReferenceExact(context.Background(), arn, coreconfirmation.SecretPurposeSkillSecret, "bad"); err == nil {
		t.Fatal("invalid binding accepted")
	}
	if _, err := r.ResolveSecretReferenceExact(context.Background(), arn, coreconfirmation.SecretPurposeSkillSecret, coreconfirmation.Digest(coreworkload.SecretGrantBindingDigest(arn, coreconfirmation.SecretPurposeSkillSecret))); err != nil {
		t.Fatalf("derived binding rejected: %v", err)
	}
}

func TestResolveApplicationRefsRejectsUUIDAndRequiresExactARNBinding(t *testing.T) {
	arn := "arn:aws:ssm:us-east-1:123456789012:parameter/app/token"
	purpose := coreconfirmation.SecretPurposeSkillSecret
	plan := coreworkload.Plan{Target: coreworkload.TargetSettings{Region: "us-east-1", AccountID: "123456789012"}, SecretGrantRefs: []coreworkload.SecretGrantRef{{ReferenceID: arn, Purpose: purpose, BindingDigest: coreconfirmation.Digest(coreworkload.SecretGrantBindingDigest(arn, purpose))}}}
	if err := ResolveApplicationRefs(context.Background(), plan, CanonicalSecretReference{}); err != nil {
		t.Fatalf("exact ARN resolver rejected: %v", err)
	}
	plan.SecretGrantRefs[0].ReferenceID = uuid.NewString()
	if err := ResolveApplicationRefs(context.Background(), plan, CanonicalSecretReference{}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("UUID application reference accepted: %v", err)
	}
}
