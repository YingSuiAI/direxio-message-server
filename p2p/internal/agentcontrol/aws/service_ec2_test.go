package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCreateEC2ProvisionIsAtomicOwnerBoundAndReplayStable(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	s := NewService(repo, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), nil)
	cred, err := saveVerifiedCredential(t, s, repo, CredentialInput{Name: "deploy", Region: "us-east-1", AccessKeyID: "AKIA", SecretAccessKey: "secret", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	req := EC2ProvisionRequest{OwnerID: "@alice:example", CredentialID: cred.ID, CredentialRevision: cred.Revision, Region: "us-east-1", StackName: "geolibre", DisplayName: "GeoLibre", InstanceType: "t3.medium", VolumeGiB: 20, AcknowledgePublicExposure: true}
	key := uuid.NewString()
	one, err := s.CreateEC2Provision(ctx, req, key)
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.CreateEC2Provision(ctx, req, key)
	if err != nil {
		t.Fatal(err)
	}
	if one.Plan.ID != two.Plan.ID || one.Provision.ID != two.Provision.ID || one.Provision.Revision != 1 {
		t.Fatalf("replay changed snapshot: %#v %#v", one, two)
	}
	if _, err := s.GetProvisionForOwner(ctx, one.Provision.ID, "@mallory:example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign owner read = %v", err)
	}
	page, err := s.ListProvisions(ctx, "@alice:example", "", 10, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("owner list = %#v %v", page, err)
	}
	createKey := uuid.NewString()
	if _, err := s.RequestEC2Create(ctx, one.Provision.ID, 1, createKey, "@alice:example"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestChange(ctx, RequestChangeInput{PlanID: one.Plan.ID, ProvisionID: one.Provision.ID, ExpectedProvisionRevision: 99, IdempotencyKey: createKey}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected mismatched revision idempotency conflict, got %v", err)
	}
}

func TestMemoryProvisionEventsAllocateStableSequenceAndCursor(t *testing.T) {
	repo := NewMemoryRepository()
	provisionID := uuid.NewString()
	repo.mu.Lock()
	repo.provisions[provisionID] = Provision{ID: provisionID, OwnerDigest: OwnerBindingDigest("@alice:example")}
	m := &MemoryChangeCoordinator{repo: repo}
	m.appendEventLocked(ChangeEvent{ProvisionID: provisionID, Kind: "one", Revision: 2})
	m.appendEventLocked(ChangeEvent{ProvisionID: provisionID, Kind: "two", Revision: 3})
	repo.mu.Unlock()
	first, next, err := repo.ListProvisionEvents(context.Background(), provisionID, "@alice:example", 0, 1)
	if err != nil || len(first) != 1 || first[0].Sequence != 1 || first[0].EventID == "" || next != 1 {
		t.Fatalf("first page = %#v next=%d err=%v", first, next, err)
	}
	second, next, err := repo.ListProvisionEvents(context.Background(), provisionID, "@alice:example", next, 1)
	if err != nil || len(second) != 1 || second[0].Sequence != 2 || second[0].EventID == first[0].EventID || next != 2 {
		t.Fatalf("second page = %#v next=%d err=%v", second, next, err)
	}
}
