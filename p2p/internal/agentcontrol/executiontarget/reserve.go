package executiontarget

// This file owns revision one of the two-plan EC2 bootstrap model. Reserve is
// deliberately a local catalog mutation: it verifies a live server-side AWS
// credential and catalog offer, then persists an immutable purchase target.
// It never calls CloudFormation or EC2 mutation APIs.

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strings"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
)

var reservationInstanceType = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}\.[a-z0-9][a-z0-9-]{0,31}$`)

type ReserveRequest struct {
	CredentialID       string
	CredentialRevision uint64
	InstanceType       string
	VolumeGiB          uint32
	IdempotencyKey     string
}

// ReservationOffer contains only provider facts resolved by a trusted
// server-side catalog. Account, region, profile, network and credential
// binding are never accepted from the ProductCore request.
type ReservationOffer struct {
	InfrastructureProfileID string
	AMIParameter            string
	InstanceType            string
	AvailabilityZone        string
	VolumeGiB               uint32
	Architecture            string
	ManagementTransport     string
	PublicIP                bool
	PublicInbound           bool
	CostQuote               coreexecution.CostQuote
}

type ReservationCatalog interface {
	ResolveReservation(context.Context, coreaws.Credentials, string, uint32) (ReservationOffer, error)
}

func (s *Service) ReserveReady() bool {
	return s != nil && s.targets != nil && s.credentials != nil && s.reservations != nil && s.now != nil
}

func (s *Service) Reserve(ctx context.Context, owner string, req ReserveRequest) (coreexecution.ExecutionTarget, error) {
	owner = strings.TrimSpace(owner)
	req.CredentialID = strings.TrimSpace(req.CredentialID)
	req.InstanceType = strings.TrimSpace(req.InstanceType)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if !s.ReserveReady() {
		return coreexecution.ExecutionTarget{}, ErrNotReady
	}
	if owner == "" || !coreexecution.ValidateUUID(req.CredentialID) || req.CredentialRevision == 0 || req.CredentialRevision > math.MaxInt64 ||
		!reservationInstanceType.MatchString(req.InstanceType) || req.VolumeGiB < 8 || req.VolumeGiB > 16384 || !coreexecution.ValidateUUID(req.IdempotencyKey) {
		return coreexecution.ExecutionTarget{}, ErrInvalid
	}
	targetID := deterministicReservationTargetID(owner, req.IdempotencyKey)
	// The deterministic ID makes a successful reserve replayable even after a
	// credential is rotated or a catalog quote expires. All caller-selected
	// values are present in the immutable target and are checked below.
	if replay, replayErr := s.targets.GetTarget(ctx, owner, targetID, 1); replayErr == nil {
		if !validReservationReplay(replay, req) {
			return coreexecution.ExecutionTarget{}, coreexecution.ErrConflict
		}
		return replay, nil
	} else if !errors.Is(replayErr, coreexecution.ErrNotFound) {
		return coreexecution.ExecutionTarget{}, replayErr
	}

	credential, err := s.credentials.GetCredentialRevision(ctx, owner, req.CredentialID, int64(req.CredentialRevision))
	if err != nil || credential.Validate() != nil || credential.ID != req.CredentialID || credential.Revision != int64(req.CredentialRevision) ||
		credential.VerifiedRevision != int64(req.CredentialRevision) || credential.AccountID == "" || credential.Region == "" || credential.UserARN == "" {
		return coreexecution.ExecutionTarget{}, ErrCredential
	}
	offer, err := s.reservations.ResolveReservation(ctx, credential, req.InstanceType, req.VolumeGiB)
	if err != nil || offer.InfrastructureProfileID != coreaws.InfrastructureProfileGeneralLinuxSSMV1 ||
		offer.AMIParameter != coreexecution.AWSAL2023X8664AMIParameter || offer.InstanceType != req.InstanceType || offer.VolumeGiB != req.VolumeGiB ||
		offer.Architecture != "x86_64" || offer.ManagementTransport != "aws_ssm" || !offer.PublicIP || offer.PublicInbound ||
		!coreexecution.ValidateAvailabilityZone(credential.Region, offer.AvailabilityZone) ||
		offer.CostQuote.Validate() != nil || !offer.CostQuote.ExpiresAt.After(s.now().UTC()) {
		return coreexecution.ExecutionTarget{}, ErrTargetUnavailable
	}
	credentialRef := coreexecution.CredentialRef{Ref: credential.ID, Purpose: "aws", Revision: req.CredentialRevision}
	credentialRef.BindingDigest, err = coreaws.CredentialBindingDigest(owner, credentialRef, credential)
	if err != nil {
		return coreexecution.ExecutionTarget{}, ErrCredential
	}
	reservation := &coreexecution.ComputeReservation{
		InfrastructureProfileID: offer.InfrastructureProfileID, AMIParameter: offer.AMIParameter,
		InstanceType: offer.InstanceType, VolumeGiB: offer.VolumeGiB, Architecture: offer.Architecture,
		AvailabilityZone:    offer.AvailabilityZone,
		ManagementTransport: offer.ManagementTransport, PublicIP: true, PublicInbound: false, CostQuote: offer.CostQuote,
	}
	target, err := (coreexecution.ExecutionTarget{
		ID: targetID, Provider: "aws", Kind: coreexecution.TargetKindAWSComputeReservation,
		InfrastructureProfileID: offer.InfrastructureProfileID, AccountID: credential.AccountID,
		Region: credential.Region, Architecture: offer.Architecture,
		Capabilities:   []string{"compute.catalog", "compute.provision", "target.aws_compute_reservation"},
		CredentialRefs: []coreexecution.CredentialRef{credentialRef}, Network: coreexecution.NetworkPolicy{Mode: "restricted"},
		ComputeReservation: reservation, Revision: 1,
	}).Normalize()
	if err != nil {
		return coreexecution.ExecutionTarget{}, ErrTargetUnavailable
	}
	created, err := s.targets.CreateTarget(ctx, storage.TargetCreateRequest{OwnerID: owner, Target: target, ExpectedRevision: 0, IdempotencyID: req.IdempotencyKey})
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	if created.ID != target.ID || created.Revision != 1 || created.Digest != target.Digest || !validReservationReplay(created, req) {
		return coreexecution.ExecutionTarget{}, coreexecution.ErrConflict
	}
	return created, nil
}

func deterministicReservationTargetID(owner, idempotencyKey string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("dirextalk.execution.v2.aws-compute-reservation\x00"+owner+"\x00"+idempotencyKey)).String()
}

func validReservationReplay(target coreexecution.ExecutionTarget, req ReserveRequest) bool {
	normalized, err := target.Normalize()
	return err == nil && normalized.Digest == target.Digest && target.Revision == 1 && target.Provider == "aws" &&
		target.Kind == coreexecution.TargetKindAWSComputeReservation && target.InfrastructureProfileID == coreaws.InfrastructureProfileGeneralLinuxSSMV1 &&
		target.Network.Mode == "restricted" && len(target.CredentialRefs) == 1 && target.CredentialRefs[0].Ref == req.CredentialID &&
		target.CredentialRefs[0].Revision == req.CredentialRevision && target.ComputeReservation != nil &&
		target.ComputeReservation.InstanceType == req.InstanceType && target.ComputeReservation.VolumeGiB == req.VolumeGiB
}
