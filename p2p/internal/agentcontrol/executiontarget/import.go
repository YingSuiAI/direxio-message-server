// Package executiontarget owns server-authoritative execution target
// bootstrap. It imports only an already-running Linux EC2 instance that is
// reachable through AWS SSM; callers cannot supply account, region,
// capabilities, credential binding, target identity, revisions, or digests.
package executiontarget

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
)

var (
	ErrNotReady          = errors.New("execution target: not ready")
	ErrInvalid           = errors.New("execution target: invalid request")
	ErrCredential        = errors.New("execution target: credential unavailable")
	ErrTargetUnavailable = errors.New("execution target: target unavailable")
)

type ImportRequest struct {
	CredentialID       string
	CredentialRevision uint64
	InstanceID         string
	IdempotencyKey     string
}

type ImportResult struct {
	Target        coreexecution.ExecutionTarget
	ObservationID string
	Observation   coreexecution.TargetObservation
}

type TargetStore interface {
	CreateTarget(context.Context, storage.TargetCreateRequest) (coreexecution.ExecutionTarget, error)
	GetTarget(context.Context, string, string, uint64) (coreexecution.ExecutionTarget, error)
	CreateTargetObservation(context.Context, storage.TargetObservationCreateRequest) (storage.TargetObservationRecord, error)
	GetTargetObservationByIdempotency(context.Context, string, string) (storage.TargetObservationRecord, bool, error)
}

type CredentialStore interface {
	GetCredentialRevision(context.Context, string, string, int64) (coreaws.Credentials, error)
}

type Transport interface {
	Inspect(context.Context, coreaws.InspectRequest) (coreaws.TargetObservation, error)
}

type Config struct {
	Targets      TargetStore
	Credentials  CredentialStore
	Transport    Transport
	Reservations ReservationCatalog
	Now          func() time.Time
}

type Service struct {
	targets      TargetStore
	credentials  CredentialStore
	transport    Transport
	reservations ReservationCatalog
	now          func() time.Time
}

func New(cfg Config) *Service {
	clock := cfg.Now
	if clock == nil {
		clock = time.Now
	}
	return &Service{targets: cfg.Targets, credentials: cfg.Credentials, transport: cfg.Transport, reservations: cfg.Reservations, now: clock}
}

func (s *Service) Ready() bool {
	return s != nil && s.targets != nil && s.credentials != nil && s.transport != nil && s.now != nil
}

func (s *Service) Import(ctx context.Context, owner string, req ImportRequest) (ImportResult, error) {
	owner = strings.TrimSpace(owner)
	req.CredentialID = strings.TrimSpace(req.CredentialID)
	req.InstanceID = strings.TrimSpace(req.InstanceID)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if !s.Ready() {
		return ImportResult{}, ErrNotReady
	}
	if owner == "" || !coreexecution.ValidateUUID(req.CredentialID) || req.CredentialRevision == 0 || req.CredentialRevision > math.MaxInt64 ||
		!coreaws.ValidEC2InstanceID(req.InstanceID) || !coreexecution.ValidateUUID(req.IdempotencyKey) {
		return ImportResult{}, ErrInvalid
	}
	// Replay precedes credential and provider lookup. The immutable target and
	// observation contain every non-secret request binding, so a completed
	// import remains replayable even if the live credential is later removed.
	if replay, found, replayErr := s.targets.GetTargetObservationByIdempotency(ctx, owner, req.IdempotencyKey); replayErr != nil {
		return ImportResult{}, replayErr
	} else if found {
		storedTarget, targetErr := s.targets.GetTarget(ctx, owner, replay.Observation.TargetID, replay.Observation.TargetRevision)
		if targetErr != nil || storedTarget.Provider != "aws" || storedTarget.Kind != "aws_ec2_instance" ||
			storedTarget.InfrastructureProfileID != coreaws.InfrastructureProfileGeneralLinuxSSMV1 ||
			storedTarget.Network.Mode != coreexecution.NetworkPolicyModeObservedHTTPSEgress ||
			storedTarget.ID != deterministicTargetID(owner, storedTarget.AccountID, storedTarget.Region, req.InstanceID) ||
			len(storedTarget.CredentialRefs) != 1 || storedTarget.CredentialRefs[0].Ref != req.CredentialID ||
			storedTarget.CredentialRefs[0].Revision != req.CredentialRevision ||
			!hasCapability(storedTarget.Capabilities, coreaws.TargetInstanceCapabilityPrefix+req.InstanceID) ||
			replay.Status != "observed" || replay.Observation.State != "ready" || replay.Observation.Partial || replay.Observation.Stale ||
			replay.Observation.Facts["instance_id"] != req.InstanceID || replay.Observation.Facts["account_id"] != storedTarget.AccountID ||
			replay.Observation.Facts["region"] != storedTarget.Region || replay.Observation.Facts["ssm_status"] != "Online" ||
			replay.Observation.Facts[coreexecution.ObservationFactHTTPSEgress] != coreexecution.ObservationFactHTTPSEgressValue ||
			!coreexecution.ValidateDigest(replay.Observation.Facts[coreexecution.ObservationFactSecurityGroupDigest]) {
			return ImportResult{}, coreexecution.ErrConflict
		}
		normalized, normalizeErr := coreaws.NormalizeInfrastructureTarget(storedTarget)
		if normalizeErr != nil || normalized.Digest != storedTarget.Digest {
			return ImportResult{}, coreexecution.ErrConflict
		}
		return ImportResult{Target: normalized, ObservationID: replay.ObservationID, Observation: replay.Observation}, nil
	}

	credential, err := s.credentials.GetCredentialRevision(ctx, owner, req.CredentialID, int64(req.CredentialRevision))
	if err != nil || credential.Validate() != nil || credential.ID != req.CredentialID ||
		credential.Revision != int64(req.CredentialRevision) || credential.VerifiedRevision != int64(req.CredentialRevision) ||
		credential.AccountID == "" || credential.Region == "" || credential.UserARN == "" {
		return ImportResult{}, ErrCredential
	}
	targetID := deterministicTargetID(owner, credential.AccountID, credential.Region, req.InstanceID)
	credentialRef := coreexecution.CredentialRef{Ref: credential.ID, Purpose: "aws", Revision: req.CredentialRevision}
	credentialRef.BindingDigest, err = coreaws.CredentialBindingDigest(owner, credentialRef, credential)
	if err != nil {
		return ImportResult{}, ErrCredential
	}
	target, err := coreaws.NormalizeInfrastructureTarget(coreexecution.ExecutionTarget{
		ID: targetID, Provider: "aws", Kind: "aws_ec2_instance",
		InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1,
		AccountID:               credential.AccountID, Region: credential.Region, Architecture: "x86_64",
		Capabilities: []string{
			"target.aws_ec2_instance",
			"transport.aws_ssm",
			coreaws.TargetInstanceCapabilityPrefix + req.InstanceID,
		},
		CredentialRefs: []coreexecution.CredentialRef{credentialRef},
		Network:        coreexecution.NetworkPolicy{Mode: coreexecution.NetworkPolicyModeObservedHTTPSEgress},
		Revision:       1,
	})
	if err != nil {
		return ImportResult{}, ErrTargetUnavailable
	}

	observed, inspectErr := s.transport.Inspect(ctx, coreaws.InspectRequest{
		OwnerID: owner, Target: target, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest,
		ProfileID: target.InfrastructureProfileID, AccountID: target.AccountID, Region: target.Region,
		Partition: awsPartition(target.Region), Credential: credential, CredentialID: credential.ID,
		CredentialRevision: req.CredentialRevision, InstanceID: req.InstanceID,
	})
	if inspectErr != nil || observed.TargetID != target.ID || observed.TargetRevision != target.Revision ||
		observed.State != "ready" || observed.Partial || observed.Stale ||
		observed.Facts["instance_id"] != req.InstanceID || observed.Facts["account_id"] != credential.AccountID ||
		observed.Facts["region"] != credential.Region || observed.Facts["operating_system"] != "linux" ||
		observed.Facts["architecture"] != target.Architecture || observed.Facts["ssm_status"] != "Online" ||
		observed.Facts[coreexecution.ObservationFactHTTPSEgress] != coreexecution.ObservationFactHTTPSEgressValue ||
		!coreexecution.ValidateDigest(observed.Facts[coreexecution.ObservationFactSecurityGroupDigest]) {
		return ImportResult{}, ErrTargetUnavailable
	}
	observed.ObservedAt = s.now().UTC().Truncate(time.Microsecond)
	observed.Digest = ""
	observed, err = observed.Normalize()
	if err != nil {
		return ImportResult{}, ErrTargetUnavailable
	}

	// Target creation uses a physical-identity key, while the action's key is
	// reserved for its initial observation. A crash between these two durable
	// writes is safely resumed: CreateTarget replays revision 1 and the second
	// write completes the action without any remote mutation.
	created, err := s.targets.CreateTarget(ctx, storage.TargetCreateRequest{
		OwnerID: owner, Target: target, ExpectedRevision: 0,
		IdempotencyID: deterministicTargetCreateKey(owner, target.ID),
	})
	if err != nil || created.Digest != target.Digest || created.Revision != 1 {
		if err != nil {
			return ImportResult{}, err
		}
		return ImportResult{}, coreexecution.ErrConflict
	}
	observationID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("dirextalk.execution.v2.target-observation\x00"+owner+"\x00"+req.IdempotencyKey)).String()
	record, err := s.targets.CreateTargetObservation(ctx, storage.TargetObservationCreateRequest{
		OwnerID: owner, ObservationID: observationID, Observation: observed, IdempotencyID: req.IdempotencyKey,
	})
	if err != nil {
		return ImportResult{}, err
	}
	if record.Status != "observed" || record.Observation.TargetID != created.ID || record.Observation.TargetRevision != created.Revision ||
		record.Observation.State != "ready" || record.Observation.Partial || record.Observation.Stale {
		return ImportResult{}, coreexecution.ErrConflict
	}
	return ImportResult{Target: created, ObservationID: record.ObservationID, Observation: record.Observation}, nil
}

func deterministicTargetID(owner, accountID, region, instanceID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("dirextalk.execution.v2.aws-ec2-ssm-target\x00"+owner+"\x00"+accountID+"\x00"+region+"\x00"+instanceID)).String()
}

func deterministicTargetCreateKey(owner, targetID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("dirextalk.execution.v2.target-create\x00"+owner+"\x00"+targetID)).String()
}

func hasCapability(capabilities []string, expected string) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

func awsPartition(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.Contains(region, "-gov-"):
		return "aws-us-gov"
	default:
		return "aws"
	}
}
