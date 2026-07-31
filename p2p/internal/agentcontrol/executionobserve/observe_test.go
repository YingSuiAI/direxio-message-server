package executionobserve

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

const (
	testOwner       = "@observe-owner:example.org"
	testTargetID    = "11111111-1111-4111-8111-111111111111"
	testCredential  = "22222222-2222-4222-8222-222222222222"
	testIdempotency = "33333333-3333-4333-8333-333333333333"
)

type fakeTargetStore struct {
	mu          sync.Mutex
	target      coreexecution.ExecutionTarget
	records     map[string]storage.TargetObservationRecord
	createCalls int
}

func (s *fakeTargetStore) GetTarget(_ context.Context, owner, id string, revision uint64) (coreexecution.ExecutionTarget, error) {
	if owner != testOwner || id != s.target.ID || revision != s.target.Revision {
		return coreexecution.ExecutionTarget{}, coreexecution.ErrNotFound
	}
	return s.target, nil
}
func (s *fakeTargetStore) GetTargetObservationByIdempotency(_ context.Context, _ string, key string) (storage.TargetObservationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	return record, ok, nil
}
func (s *fakeTargetStore) CreateTargetObservation(_ context.Context, in storage.TargetObservationCreateRequest) (storage.TargetObservationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	if previous, ok := s.records[in.IdempotencyID]; ok {
		return previous, nil
	}
	record := storage.TargetObservationRecord{OwnerID: in.OwnerID, ObservationID: in.ObservationID, Revision: 1, Status: "observed", Observation: in.Observation}
	s.records[in.IdempotencyID] = record
	return record, nil
}

type fakeCredentialStore struct{ credential coreaws.Credentials }

func (s fakeCredentialStore) GetCredentialRevision(_ context.Context, owner, id string, revision int64) (coreaws.Credentials, error) {
	if owner != testOwner || id != s.credential.ID || revision != s.credential.Revision {
		return coreaws.Credentials{}, coreaws.ErrNotFound
	}
	return s.credential, nil
}

type fakeTransport struct {
	mu    sync.Mutex
	calls int
	seen  coreaws.InspectRequest
	obs   coreaws.TargetObservation
	err   error
}

func (t *fakeTransport) Inspect(_ context.Context, request coreaws.InspectRequest) (coreaws.TargetObservation, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	t.seen = request
	return t.obs, t.err
}

func testService(t *testing.T, transport *fakeTransport) (*Service, *fakeTargetStore, agentembedded.ExecutionV2ObserveRequest) {
	t.Helper()
	now := time.Date(2035, 1, 1, 0, 0, 0, 123456000, time.UTC)
	credential := coreaws.RehydrateCredentials(testCredential, "observe", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/observe", []byte("AKIAABCDEFGHIJKLMNOP"), []byte("secret-value"), nil, 1, 1, now, now)
	ref := coreexecution.CredentialRef{Ref: credential.ID, Purpose: "aws", Revision: 1}
	var err error
	ref.BindingDigest, err = coreaws.CredentialBindingDigest(testOwner, ref, credential)
	if err != nil {
		t.Fatal(err)
	}
	target, err := coreaws.NormalizeInfrastructureTarget(coreexecution.ExecutionTarget{
		ID: testTargetID, Provider: "aws", Kind: "aws_ec2_instance", InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1,
		AccountID: credential.AccountID, Region: credential.Region, Architecture: "x86_64",
		Capabilities: []string{"target.aws_ec2_instance", "transport.aws_ssm", InstanceCapabilityPrefix + "i-0123456789abcdef0"}, CredentialRefs: []coreexecution.CredentialRef{ref}, Revision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeTargetStore{target: target, records: make(map[string]storage.TargetObservationRecord)}
	service := New(Config{Targets: store, Credentials: fakeCredentialStore{credential: credential}, Transport: transport, Now: func() time.Time { return now }})
	return service, store, agentembedded.ExecutionV2ObserveRequest{TargetID: target.ID, TargetRevision: target.Revision, IdempotencyKey: testIdempotency}
}

func TestObserveResolvesAuthoritativeTargetAndReplaysWithoutInspect(t *testing.T) {
	transport := &fakeTransport{obs: coreaws.TargetObservation{TargetID: testTargetID, TargetRevision: 1, State: "ready", Facts: map[string]string{"instance_id": "i-0123456789abcdef0", "ssm_status": "Online"}}}
	service, store, request := testService(t, transport)
	first, err := service.Observe(context.Background(), testOwner, request)
	if err != nil || first.Digest == "" || first.ObservedAt.IsZero() {
		t.Fatalf("first observation=%+v err=%v", first, err)
	}
	if transport.calls != 1 || store.createCalls != 1 {
		t.Fatalf("transport/create calls=%d/%d", transport.calls, store.createCalls)
	}
	if transport.seen.TargetDigest != store.target.Digest || transport.seen.InstanceID != "i-0123456789abcdef0" || transport.seen.CredentialRevision != 1 {
		t.Fatalf("inspect request was not server bound: %+v", transport.seen)
	}
	replay, err := service.Observe(context.Background(), testOwner, request)
	if err != nil || replay.Digest != first.Digest || replay.ObservedAt != first.ObservedAt {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if transport.calls != 1 || store.createCalls != 1 {
		t.Fatalf("replay repeated provider/storage call: %d/%d", transport.calls, store.createCalls)
	}
}

func TestObservePersistsKnownUnavailableStateButDoesNotClaimSuccess(t *testing.T) {
	transport := &fakeTransport{obs: coreaws.TargetObservation{TargetID: testTargetID, TargetRevision: 1, State: "unavailable", Partial: true, Warnings: []string{"ssm_not_online_linux"}}, err: coreaws.ErrTypedUnavailable}
	service, store, request := testService(t, transport)
	observation, err := service.Observe(context.Background(), testOwner, request)
	if !errors.Is(err, coreaws.ErrTypedUnavailable) || observation.State != "unavailable" || !observation.Partial {
		t.Fatalf("unavailable observation=%+v err=%v", observation, err)
	}
	if store.createCalls != 1 {
		t.Fatalf("known provider state was not persisted")
	}
}

func TestObserveDoesNotPersistUnknownProviderFailure(t *testing.T) {
	transport := &fakeTransport{obs: coreaws.TargetObservation{TargetID: testTargetID, TargetRevision: 1, State: "ready"}, err: coreaws.ErrTypedProvider}
	service, store, request := testService(t, transport)
	if _, err := service.Observe(context.Background(), testOwner, request); !errors.Is(err, ErrTarget) {
		t.Fatalf("provider failure err=%v", err)
	}
	if store.createCalls != 0 {
		t.Fatalf("unknown provider failure was persisted")
	}
}

func TestObserveRejectsTargetWithoutServerInstanceBinding(t *testing.T) {
	transport := &fakeTransport{}
	service, store, request := testService(t, transport)
	store.target.Capabilities = []string{"target.aws_ec2_instance", "transport.aws_ssm"}
	if _, err := service.Observe(context.Background(), testOwner, request); !errors.Is(err, ErrTarget) {
		t.Fatalf("missing instance binding err=%v", err)
	}
	if transport.calls != 0 {
		t.Fatalf("provider called without an exact instance binding")
	}
}
