package executiontarget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

const (
	importOwner      = "@target-import:example.org"
	importCredential = "11111111-1111-4111-8111-111111111111"
	importInstance   = "i-0123456789abcdef0"
	importKey        = "22222222-2222-4222-8222-222222222222"
)

type importTargetStore struct {
	mu            sync.Mutex
	target        coreexecution.ExecutionTarget
	observations  map[string]storage.TargetObservationRecord
	createTargets int
	createObs     int
}

func (s *importTargetStore) CreateTarget(_ context.Context, in storage.TargetCreateRequest) (coreexecution.ExecutionTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createTargets++
	if s.target.ID != "" {
		if s.target.Digest != in.Target.Digest {
			return coreexecution.ExecutionTarget{}, coreexecution.ErrConflict
		}
		return s.target, nil
	}
	s.target = in.Target
	return s.target, nil
}

func (s *importTargetStore) GetTarget(_ context.Context, owner, id string, revision uint64) (coreexecution.ExecutionTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner != importOwner || s.target.ID != id || s.target.Revision != revision {
		return coreexecution.ExecutionTarget{}, coreexecution.ErrNotFound
	}
	return s.target, nil
}

func (s *importTargetStore) CreateTargetObservation(_ context.Context, in storage.TargetObservationCreateRequest) (storage.TargetObservationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createObs++
	if replay, ok := s.observations[in.IdempotencyID]; ok {
		return replay, nil
	}
	record := storage.TargetObservationRecord{
		OwnerID: in.OwnerID, ObservationID: in.ObservationID, Revision: 1,
		Status: "observed", Observation: in.Observation,
	}
	s.observations[in.IdempotencyID] = record
	return record, nil
}

func (s *importTargetStore) GetTargetObservationByIdempotency(_ context.Context, _ string, key string) (storage.TargetObservationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.observations[key]
	return record, ok, nil
}

type importCredentialStore struct {
	credential coreaws.Credentials
	calls      int
}

func (s *importCredentialStore) GetCredentialRevision(_ context.Context, owner, id string, revision int64) (coreaws.Credentials, error) {
	s.calls++
	if owner != importOwner || id != s.credential.ID || revision != s.credential.Revision {
		return coreaws.Credentials{}, coreaws.ErrNotFound
	}
	return s.credential, nil
}

type importTransport struct {
	calls int
	seen  coreaws.InspectRequest
	err   error
}

func (t *importTransport) Inspect(_ context.Context, in coreaws.InspectRequest) (coreaws.TargetObservation, error) {
	t.calls++
	t.seen = in
	if t.err != nil {
		return coreaws.TargetObservation{}, t.err
	}
	return coreexecution.TargetObservation{
		TargetID: in.TargetID, TargetRevision: in.TargetRevision, State: "ready",
		Facts: map[string]string{
			"instance_id": in.InstanceID, "account_id": in.AccountID, "region": in.Region,
			"partition": in.Partition, "operating_system": "linux",
			"architecture": in.Target.Architecture, "ssm_status": "Online",
			coreexecution.ObservationFactHTTPSEgress:         coreexecution.ObservationFactHTTPSEgressValue,
			coreexecution.ObservationFactSecurityGroupDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}, nil
}

func importFixture(t *testing.T) (*Service, *importTargetStore, *importCredentialStore, *importTransport, ImportRequest) {
	t.Helper()
	now := time.Date(2036, 1, 2, 3, 4, 5, 0, time.UTC)
	credential := coreaws.RehydrateCredentials(
		importCredential, "import", "us-east-1", "123456789012",
		"arn:aws:iam::123456789012:role/import", []byte("AKIAABCDEFGHIJKLMNOP"),
		[]byte("secret-value"), nil, 4, 4, now, now,
	)
	targets := &importTargetStore{observations: map[string]storage.TargetObservationRecord{}}
	credentials := &importCredentialStore{credential: credential}
	transport := &importTransport{}
	service := New(Config{Targets: targets, Credentials: credentials, Transport: transport, Now: func() time.Time { return now }})
	return service, targets, credentials, transport, ImportRequest{
		CredentialID: importCredential, CredentialRevision: 4,
		InstanceID: importInstance, IdempotencyKey: importKey,
	}
}

func TestImportVerifiesAWSBindingAndReplaysWithoutProviderRead(t *testing.T) {
	service, store, credentials, transport, request := importFixture(t)
	first, err := service.Import(context.Background(), importOwner, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Target.Provider != "aws" || first.Target.Kind != "aws_ec2_instance" || first.Target.Revision != 1 ||
		first.Target.AccountID != "123456789012" || first.Target.Region != "us-east-1" || !first.Target.Digest.Valid() ||
		first.Target.Network.Mode != coreexecution.NetworkPolicyModeObservedHTTPSEgress ||
		first.Observation.State != "ready" || !first.Observation.Digest.Valid() || !coreexecution.ValidateUUID(first.ObservationID) {
		t.Fatalf("import result=%+v", first)
	}
	if transport.calls != 1 || store.createTargets != 1 || store.createObs != 1 {
		t.Fatalf("calls inspect/target/observation=%d/%d/%d", transport.calls, store.createTargets, store.createObs)
	}
	if transport.seen.CredentialRevision != 4 || transport.seen.TargetDigest != first.Target.Digest ||
		transport.seen.InstanceID != importInstance || transport.seen.AccountID != "123456789012" {
		t.Fatalf("unbound inspect request=%+v", transport.seen)
	}
	replay, err := service.Import(context.Background(), importOwner, request)
	if err != nil || replay.Target.Digest != first.Target.Digest || replay.Observation.Digest != first.Observation.Digest ||
		replay.ObservationID != first.ObservationID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if transport.calls != 1 || store.createTargets != 1 || store.createObs != 1 {
		t.Fatalf("replay repeated provider/write: %d/%d/%d", transport.calls, store.createTargets, store.createObs)
	}
	if credentials.calls != 1 {
		t.Fatalf("replay depended on live credential lookup: %d", credentials.calls)
	}
	request.InstanceID = "i-aaaaaaaaaaaaaaaaa"
	if _, err = service.Import(context.Background(), importOwner, request); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("idempotency drift err=%v", err)
	}
	if transport.calls != 1 || credentials.calls != 1 {
		t.Fatal("idempotency drift reached credential or provider")
	}
}

func TestImportFailsBeforePersistenceWhenSSMIsNotReady(t *testing.T) {
	service, store, _, transport, request := importFixture(t)
	transport.err = coreaws.ErrTypedUnavailable
	if _, err := service.Import(context.Background(), importOwner, request); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("import err=%v", err)
	}
	if store.createTargets != 0 || store.createObs != 0 {
		t.Fatalf("unverified target was persisted: %d/%d", store.createTargets, store.createObs)
	}
}

func TestImportRejectsCredentialRevisionAndCallerAuthorityFieldsByConstruction(t *testing.T) {
	service, store, _, transport, request := importFixture(t)
	request.CredentialRevision++
	if _, err := service.Import(context.Background(), importOwner, request); !errors.Is(err, ErrCredential) {
		t.Fatalf("credential revision err=%v", err)
	}
	if transport.calls != 0 || store.createTargets != 0 {
		t.Fatal("credential drift reached provider or storage")
	}
}
