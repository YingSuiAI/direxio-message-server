// Package executionobserve owns the read-only execution.v2 target observation
// boundary. It resolves all target and credential facts from server storage
// before calling the typed AWS SSM transport; callers cannot supply a target
// snapshot, instance id, or credential material.
package executionobserve

import (
	"context"
	"errors"
	"strings"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
)

var (
	ErrNotReady   = errors.New("execution observe: not ready")
	ErrInvalid    = errors.New("execution observe: invalid request")
	ErrTarget     = errors.New("execution observe: target unavailable")
	ErrCredential = errors.New("execution observe: credential unavailable")
)

// InstanceCapabilityPrefix is the server-owned target capability convention
// used to bind an EC2 target revision to one exact instance. The instance id
// is intentionally not accepted from the ProductCore request.
const InstanceCapabilityPrefix = coreaws.TargetInstanceCapabilityPrefix

// TargetStore is deliberately narrower than the full execution catalog. It
// prevents this read-only port from acquiring mutation authority over targets.
type TargetStore interface {
	GetTarget(context.Context, string, string, uint64) (coreexecution.ExecutionTarget, error)
	CreateTargetObservation(context.Context, storage.TargetObservationCreateRequest) (storage.TargetObservationRecord, error)
}

// ObservationReplayStore is implemented by the PostgreSQL catalog. It is
// optional for small test doubles, but production storage implements it so a
// repeated idempotency key can return the immutable observation without making
// another provider call.
type ObservationReplayStore interface {
	GetTargetObservationByIdempotency(context.Context, string, string) (storage.TargetObservationRecord, bool, error)
}

type CredentialStore interface {
	GetCredentialRevision(context.Context, string, string, int64) (coreaws.Credentials, error)
}

// CredentialStoreFunc adapts an owner-aware repository factory without
// widening the observe service's dependency surface.
type CredentialStoreFunc func(context.Context, string, string, int64) (coreaws.Credentials, error)

func (f CredentialStoreFunc) GetCredentialRevision(ctx context.Context, owner, id string, revision int64) (coreaws.Credentials, error) {
	if f == nil {
		return coreaws.Credentials{}, ErrNotReady
	}
	return f(ctx, owner, id, revision)
}

type Transport interface {
	Inspect(context.Context, coreaws.InspectRequest) (coreaws.TargetObservation, error)
}

type Config struct {
	Targets     TargetStore
	Credentials CredentialStore
	Transport   Transport
	Now         func() time.Time
}

type Service struct {
	targets     TargetStore
	credentials CredentialStore
	transport   Transport
	now         func() time.Time
}

func New(cfg Config) *Service {
	clock := cfg.Now
	if clock == nil {
		clock = time.Now
	}
	return &Service{targets: cfg.Targets, credentials: cfg.Credentials, transport: cfg.Transport, now: clock}
}

func (s *Service) Ready() bool {
	return s != nil && s.targets != nil && s.credentials != nil && s.transport != nil
}

// Observe resolves target and credential revisions from the owner-scoped
// catalog, performs a read-only SSM/EC2 inspection, then persists a server
// generated immutable observation. No request field is treated as a target
// digest or provider fact.
func (s *Service) Observe(ctx context.Context, owner string, req agentembedded.ExecutionV2ObserveRequest) (coreexecution.TargetObservation, error) {
	if !s.Ready() {
		return coreexecution.TargetObservation{}, ErrNotReady
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || !coreexecution.ValidateUUID(req.TargetID) || req.TargetRevision == 0 || !coreexecution.ValidateUUID(req.IdempotencyKey) {
		return coreexecution.TargetObservation{}, ErrInvalid
	}
	target, err := s.targets.GetTarget(ctx, owner, req.TargetID, req.TargetRevision)
	if err != nil {
		return coreexecution.TargetObservation{}, ErrTarget
	}
	if target.ID != req.TargetID || target.Revision != req.TargetRevision || !target.Digest.Valid() || target.Provider != "aws" || target.Kind != "aws_ec2_instance" || target.InfrastructureProfileID == "" {
		return coreexecution.TargetObservation{}, ErrTarget
	}
	instanceID, ok := targetInstanceID(target)
	if !ok {
		return coreexecution.TargetObservation{}, ErrTarget
	}
	// The replay lookup happens after resolving the authoritative target so a
	// reused key can never cross an owner or target revision boundary.
	if replayStore, ok := s.targets.(ObservationReplayStore); ok {
		if replay, found, replayErr := replayStore.GetTargetObservationByIdempotency(ctx, owner, req.IdempotencyKey); replayErr != nil {
			return coreexecution.TargetObservation{}, replayErr
		} else if found {
			if replay.OwnerID != owner || replay.Observation.TargetID != target.ID || replay.Observation.TargetRevision != target.Revision {
				return coreexecution.TargetObservation{}, coreexecution.ErrConflict
			}
			return replay.Observation, nil
		}
	}
	if len(target.CredentialRefs) != 1 {
		return coreexecution.TargetObservation{}, ErrCredential
	}
	ref := target.CredentialRefs[0]
	if ref.Ref == "" || ref.Revision == 0 || !ref.BindingDigest.Valid() {
		return coreexecution.TargetObservation{}, ErrCredential
	}
	credential, err := s.credentials.GetCredentialRevision(ctx, owner, ref.Ref, int64(ref.Revision))
	if err != nil || credential.ID != ref.Ref || credential.Revision != int64(ref.Revision) || credential.VerifiedRevision != int64(ref.Revision) {
		return coreexecution.TargetObservation{}, ErrCredential
	}
	bound, err := coreaws.CredentialBindingDigest(owner, ref, credential)
	if err != nil || bound != ref.BindingDigest {
		return coreexecution.TargetObservation{}, ErrCredential
	}
	partition := awsPartition(credential.Region)
	observed, inspectErr := s.transport.Inspect(ctx, coreaws.InspectRequest{
		OwnerID: owner, Target: target, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest,
		ProfileID: target.InfrastructureProfileID, AccountID: target.AccountID, Region: target.Region, Partition: partition,
		Credential: credential, CredentialID: credential.ID, CredentialRevision: ref.Revision, InstanceID: instanceID,
	})
	if inspectErr != nil && !errors.Is(inspectErr, coreaws.ErrTypedUnavailable) {
		// Provider failures without a known target state are not observations;
		// never persist a partial or caller-shaped value as if it were readback.
		return coreexecution.TargetObservation{}, ErrTarget
	}
	if observed.TargetID != "" && (observed.TargetID != target.ID || observed.TargetRevision != target.Revision) {
		return coreexecution.TargetObservation{}, ErrTarget
	}
	if observed.TargetID == "" {
		// A provider must return a complete typed observation. In particular, do
		// not manufacture CPU/memory/disk metrics or a synthetic unavailable row.
		return coreexecution.TargetObservation{}, ErrTarget
	}
	observed.TargetID = target.ID
	observed.TargetRevision = target.Revision
	observed.ObservedAt = s.now().UTC().Truncate(time.Microsecond)
	observed.Digest = ""
	observed, err = observed.Normalize()
	if err != nil {
		return coreexecution.TargetObservation{}, ErrTarget
	}
	observationID := uuid.NewSHA1(uuid.Nil, []byte("execution-v2-observation\x00"+owner+"\x00"+req.IdempotencyKey)).String()
	record, persistErr := s.targets.CreateTargetObservation(ctx, storage.TargetObservationCreateRequest{
		OwnerID: owner, ObservationID: observationID, Observation: observed, IdempotencyID: req.IdempotencyKey,
	})
	if persistErr != nil {
		return coreexecution.TargetObservation{}, persistErr
	}
	if inspectErr != nil {
		// Known provider states (for example SSM offline) are persisted for
		// readback, but remain an error to the caller; unknown provider failures
		// never reach this point because they return no typed observation.
		return record.Observation, inspectErr
	}
	return record.Observation, nil
}

func targetInstanceID(target coreexecution.ExecutionTarget) (string, bool) {
	for _, capability := range target.Capabilities {
		if !strings.HasPrefix(capability, InstanceCapabilityPrefix) {
			continue
		}
		instanceID := strings.TrimPrefix(capability, InstanceCapabilityPrefix)
		if coreaws.ValidEC2InstanceID(instanceID) {
			return instanceID, true
		}
	}
	return "", false
}

func awsPartition(region string) string {
	region = strings.TrimSpace(region)
	if strings.HasPrefix(region, "cn-") {
		return "aws-cn"
	}
	if strings.Contains(region, "-gov-") {
		return "aws-us-gov"
	}
	return "aws"
}
