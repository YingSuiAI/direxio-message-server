package aws

// This file defines the durable, typed boundary for the first V2 EC2
// provisioning stage. The SDK provider and runtime may enter only through
// this immutable intent/readback contract.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

var (
	ErrEC2ProvisionInvalid   = errors.New("aws ec2 provision: invalid request")
	ErrEC2ProvisionUncertain = errors.New("aws ec2 provision: operation uncertain")
	ErrEC2ProvisionPending   = errors.New("aws ec2 provision: operation pending")
	ErrEC2ProvisionFailed    = errors.New("aws ec2 provision: operation failed")
)

const (
	EC2ProvisionPendingSSMRegistration = "ssm_registration_pending"
	EC2ProvisionPendingSSMOnline       = "ssm_online_pending"
)

type EC2ProvisionRequest struct {
	OwnerID         string                    `json:"owner_id"`
	PlanID          string                    `json:"plan_id"`
	PlanRevision    uint64                    `json:"plan_revision"`
	PlanDigest      execution.Digest          `json:"plan_digest"`
	RunID           string                    `json:"run_id"`
	RunRevision     uint64                    `json:"run_revision"`
	RunDigest       execution.Digest          `json:"run_digest"`
	StageID         string                    `json:"stage_id"`
	StageRevision   uint64                    `json:"stage_revision"`
	StageDigest     execution.Digest          `json:"stage_digest"`
	AttemptID       string                    `json:"attempt_id"`
	StepRevision    uint64                    `json:"step_revision"`
	Target          execution.ExecutionTarget `json:"target"`
	Step            execution.ExecutionStep   `json:"step"`
	PolicyDigest    execution.Digest          `json:"policy_digest"`
	CostQuoteDigest execution.Digest          `json:"cost_quote_digest"`
	FenceDigest     execution.Digest          `json:"fence_digest"`
	RequestDigest   execution.Digest          `json:"request_digest"`
}

type CloudFormationCreateRequest struct {
	OwnerID             string                         `json:"owner_id"`
	StackName           string                         `json:"stack_name"`
	AccountID           string                         `json:"account_id"`
	Region              string                         `json:"region"`
	Credential          CredentialHandle               `json:"-"`
	CredentialID        string                         `json:"credential_id"`
	CredentialRevision  uint64                         `json:"credential_revision"`
	ReservationTargetID string                         `json:"reservation_target_id"`
	ReservationDigest   execution.Digest               `json:"reservation_digest"`
	Provision           execution.ComputeProvisionStep `json:"provision"`
	RequestDigest       execution.Digest               `json:"request_digest"`
}

type EC2ProvisionIntent struct {
	OwnerID              string              `json:"owner_id"`
	FenceDigest          execution.Digest    `json:"fence_digest"`
	RequestDigest        execution.Digest    `json:"request_digest"`
	ProviderOperationKey string              `json:"provider_operation_key"`
	Request              EC2ProvisionRequest `json:"request"`
}

type EC2ProvisionIntentRecord struct {
	Intent              EC2ProvisionIntent `json:"intent"`
	Status              string             `json:"status"`
	ProviderOperationID string             `json:"provider_operation_id,omitempty"`
	Revision            uint64             `json:"revision"`
}

type CloudFormationProvisionReadback struct {
	StackName           string           `json:"stack_name"`
	StackID             string           `json:"stack_id,omitempty"`
	Status              string           `json:"status"`
	PendingReason       string           `json:"pending_reason,omitempty"`
	InstanceID          string           `json:"instance_id,omitempty"`
	InstanceType        string           `json:"instance_type,omitempty"`
	AvailabilityZone    string           `json:"availability_zone,omitempty"`
	Architecture        string           `json:"architecture,omitempty"`
	SSMStatus           string           `json:"ssm_status,omitempty"`
	OperatingSystem     string           `json:"operating_system,omitempty"`
	PlatformName        string           `json:"platform_name,omitempty"`
	PlatformVersion     string           `json:"platform_version,omitempty"`
	VCPUCount           int32            `json:"vcpu_count,omitempty"`
	MemoryMiB           int64            `json:"memory_mib,omitempty"`
	RootVolumeGiB       int32            `json:"root_volume_gib,omitempty"`
	PublicIP            string           `json:"public_ip,omitempty"`
	PublicInbound       bool             `json:"public_inbound"`
	HTTPSEgress         string           `json:"https_egress,omitempty"`
	SecurityGroupDigest execution.Digest `json:"security_group_digest,omitempty"`
}

type EC2ProvisionCompletion struct {
	Intent      EC2ProvisionIntentRecord    `json:"intent"`
	Target      execution.ExecutionTarget   `json:"target"`
	Observation execution.TargetObservation `json:"observation"`
}

// EC2ProvisionIntentStore requires the provider intent before mutation and
// makes target revision 2 + its initial observation one atomic completion.
type EC2ProvisionIntentStore interface {
	ReserveEC2ProvisionIntent(context.Context, EC2ProvisionIntent) (EC2ProvisionIntentRecord, bool, error)
	GetEC2ProvisionIntent(context.Context, string, execution.Digest) (EC2ProvisionIntentRecord, error)
	RecordEC2ProviderOperation(context.Context, string, execution.Digest, string) (EC2ProvisionIntentRecord, error)
	RecordEC2ProvisionReadback(context.Context, string, execution.Digest, CloudFormationProvisionReadback) (EC2ProvisionIntentRecord, error)
	MarkEC2ProvisionUncertain(context.Context, string, execution.Digest) error
	MarkEC2ProvisionFailed(context.Context, string, execution.Digest, CloudFormationProvisionReadback) error
	CompleteEC2Provision(context.Context, EC2ProvisionCompletion) error
}

type CloudFormationProvisionProvider interface {
	Create(context.Context, CloudFormationCreateRequest) (string, error)
	Readback(context.Context, CloudFormationCreateRequest, string) (CloudFormationProvisionReadback, error)
}

type EC2ProvisionExecutor struct {
	store    EC2ProvisionIntentStore
	provider CloudFormationProvisionProvider
	now      func() time.Time
}

func NewEC2ProvisionExecutor(store EC2ProvisionIntentStore, provider CloudFormationProvisionProvider, now func() time.Time) (*EC2ProvisionExecutor, error) {
	if store == nil || provider == nil {
		return nil, ErrEC2ProvisionInvalid
	}
	if now == nil {
		now = time.Now
	}
	return &EC2ProvisionExecutor{store: store, provider: provider, now: now}, nil
}

func (e *EC2ProvisionExecutor) Execute(ctx context.Context, req EC2ProvisionRequest, credential Credentials) (EC2ProvisionCompletion, error) {
	var empty EC2ProvisionCompletion
	normalized, providerRequest, intent, err := normalizeEC2ProvisionRequest(req, credential)
	if err != nil {
		return empty, err
	}
	record, created, err := e.store.ReserveEC2ProvisionIntent(ctx, intent)
	if err != nil || record.Intent.FenceDigest != intent.FenceDigest || record.Intent.RequestDigest != intent.RequestDigest {
		if err != nil {
			return empty, fmt.Errorf("%w: reserve_intent", ErrEC2ProvisionUncertain)
		}
		return empty, fmt.Errorf("%w: reserve_intent_mismatch", ErrEC2ProvisionInvalid)
	}
	operationID := record.ProviderOperationID
	if created {
		operationID, err = e.provider.Create(ctx, providerRequest)
		if err == nil && strings.TrimSpace(operationID) != "" {
			record, err = e.store.RecordEC2ProviderOperation(ctx, intent.OwnerID, intent.FenceDigest, operationID)
			if err != nil {
				// The provider may have accepted CREATE. The durable intent and
				// deterministic stack key force every later attempt into readback.
				_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
				return empty, fmt.Errorf("%w: record_provider_operation", ErrEC2ProvisionUncertain)
			}
		} else {
			// A lost CreateStack response is not proof of failure. Resolve the
			// deterministic stack name immediately, and never call Create again.
			operationID = ""
		}
	}
	readback, err := e.provider.Readback(ctx, providerRequest, operationID)
	if err != nil {
		_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		// Once CreateStack has a durable deterministic intent, transient SDK
		// read failures are safe to poll again: this path never calls CreateStack
		// for an existing intent. Identity/template/topology mismatches are not
		// retryable and still fence the run uncertain immediately.
		if ctx.Err() == nil && retryableProvisionReadback(err) {
			return empty, ErrEC2ProvisionPending
		}
		return empty, fmt.Errorf("%w: provider_readback_%s", ErrEC2ProvisionUncertain, provisionReadbackCode(err))
	}
	if record.ProviderOperationID == "" && strings.TrimSpace(readback.StackID) != "" {
		record, err = e.store.RecordEC2ProviderOperation(ctx, intent.OwnerID, intent.FenceDigest, readback.StackID)
		if err != nil {
			_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
			return empty, fmt.Errorf("%w: record_recovered_provider_operation", ErrEC2ProvisionUncertain)
		}
	}
	record, err = e.store.RecordEC2ProvisionReadback(ctx, intent.OwnerID, intent.FenceDigest, readback)
	if err != nil {
		_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		return empty, fmt.Errorf("%w: record_provider_readback_%s", ErrEC2ProvisionUncertain, provisionStoreErrorCode(err))
	}
	if readback.Status != "CREATE_COMPLETE" {
		if readback.Status == "CREATE_IN_PROGRESS" || readback.Status == "REVIEW_IN_PROGRESS" {
			return empty, ErrEC2ProvisionPending
		}
		if terminalCloudFormationCreateFailure(readback.Status) {
			_ = e.store.MarkEC2ProvisionFailed(ctx, intent.OwnerID, intent.FenceDigest, readback)
			return empty, ErrEC2ProvisionFailed
		}
		_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		return empty, fmt.Errorf("%w: unsupported_provider_status", ErrEC2ProvisionUncertain)
	}
	if readback.PendingReason != "" {
		if !validPendingProvisionReadback(normalized, record, readback) {
			_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
			return empty, fmt.Errorf("%w: invalid_pending_readback", ErrEC2ProvisionUncertain)
		}
		return empty, ErrEC2ProvisionPending
	}
	completion, err := provisionCompletion(normalized, record, readback, e.now().UTC())
	if err != nil {
		_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		return empty, fmt.Errorf("%w: invalid_completion", ErrEC2ProvisionUncertain)
	}
	if err := e.store.CompleteEC2Provision(ctx, completion); err != nil {
		return empty, fmt.Errorf("%w: persist_completion", ErrEC2ProvisionUncertain)
	}
	return completion, nil
}

func provisionStoreErrorCode(err error) string {
	type safeCoder interface{ SafeCode() string }
	var coded safeCoder
	if errors.As(err, &coded) {
		switch coded.SafeCode() {
		case "invalid_input", "marshal", "sensitive_metadata", "digest", "begin", "load_intent", "binding_conflict", "update", "update_count", "update_cas", "reload_intent", "commit":
			return coded.SafeCode()
		}
	}
	switch {
	case errors.Is(err, execution.ErrConflict):
		return "conflict"
	case errors.Is(err, execution.ErrNotFound):
		return "not_found"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline"
	default:
		return "unclassified"
	}
}

// Reconcile performs readback for an exact, already persisted provision
// intent. Unlike Execute it has no path to CreateStack, even when the intent
// is missing or corrupt.
func (e *EC2ProvisionExecutor) Reconcile(ctx context.Context, req EC2ProvisionRequest, credential Credentials) (EC2ProvisionCompletion, error) {
	var empty EC2ProvisionCompletion
	if e == nil || e.store == nil || e.provider == nil {
		return empty, ErrEC2ProvisionInvalid
	}
	normalized, providerRequest, intent, err := normalizeEC2ProvisionRequest(req, credential)
	if err != nil {
		return empty, err
	}
	record, err := e.store.GetEC2ProvisionIntent(ctx, intent.OwnerID, intent.FenceDigest)
	if err != nil || record.Intent.FenceDigest != intent.FenceDigest || record.Intent.RequestDigest != intent.RequestDigest || record.Intent.ProviderOperationKey != intent.ProviderOperationKey {
		return empty, ErrEC2ProvisionUncertain
	}
	readback, err := e.provider.Readback(ctx, providerRequest, record.ProviderOperationID)
	if err != nil {
		_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		return empty, fmt.Errorf("%w: provider_readback_%s", ErrEC2ProvisionUncertain, provisionReadbackCode(err))
	}
	if record.ProviderOperationID == "" && strings.TrimSpace(readback.StackID) != "" {
		record, err = e.store.RecordEC2ProviderOperation(ctx, intent.OwnerID, intent.FenceDigest, readback.StackID)
		if err != nil {
			_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
			return empty, ErrEC2ProvisionUncertain
		}
	}
	record, err = e.store.RecordEC2ProvisionReadback(ctx, intent.OwnerID, intent.FenceDigest, readback)
	if err != nil {
		_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		return empty, ErrEC2ProvisionUncertain
	}
	if readback.Status != "CREATE_COMPLETE" {
		if readback.Status == "CREATE_IN_PROGRESS" || readback.Status == "REVIEW_IN_PROGRESS" {
			return empty, ErrEC2ProvisionPending
		}
		if terminalCloudFormationCreateFailure(readback.Status) {
			_ = e.store.MarkEC2ProvisionFailed(ctx, intent.OwnerID, intent.FenceDigest, readback)
			return empty, ErrEC2ProvisionFailed
		}
		_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		return empty, ErrEC2ProvisionUncertain
	}
	if readback.PendingReason != "" {
		if !validPendingProvisionReadback(normalized, record, readback) {
			_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
			return empty, ErrEC2ProvisionUncertain
		}
		return empty, ErrEC2ProvisionPending
	}
	completion, err := provisionCompletion(normalized, record, readback, e.now().UTC())
	if err != nil {
		_ = e.store.MarkEC2ProvisionUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		return empty, ErrEC2ProvisionUncertain
	}
	if err := e.store.CompleteEC2Provision(ctx, completion); err != nil {
		return empty, err
	}
	return completion, nil
}

func normalizeEC2ProvisionRequest(req EC2ProvisionRequest, credential Credentials) (EC2ProvisionRequest, CloudFormationCreateRequest, EC2ProvisionIntent, error) {
	var emptyProvider CloudFormationCreateRequest
	var emptyIntent EC2ProvisionIntent
	if strings.TrimSpace(req.OwnerID) == "" || !execution.ValidateUUID(req.PlanID) || req.PlanRevision == 0 || !req.PlanDigest.Valid() || !execution.ValidateUUID(req.RunID) || req.RunRevision == 0 || !req.RunDigest.Valid() || !execution.ValidateUUID(req.StageID) || req.StageRevision == 0 || !req.StageDigest.Valid() || !execution.ValidateUUID(req.AttemptID) || req.StepRevision == 0 || !req.PolicyDigest.Valid() || !req.CostQuoteDigest.Valid() || !req.FenceDigest.Valid() {
		return req, emptyProvider, emptyIntent, ErrEC2ProvisionInvalid
	}
	target, err := req.Target.Normalize()
	if err != nil || target.Digest != req.Target.Digest || target.Kind != execution.TargetKindAWSComputeReservation || target.ComputeReservation == nil || target.Revision != 1 || len(target.CredentialRefs) != 1 {
		return req, emptyProvider, emptyIntent, ErrEC2ProvisionInvalid
	}
	stepSnapshot, err := execution.StepSnapshotFromStep(req.Step, execution.StepSetForward)
	if err != nil || stepSnapshot.Step.Kind != execution.StepComputeProvision || stepSnapshot.Step.ComputeProvision == nil || stepSnapshot.Step.TargetID != target.ID || stepSnapshot.Step.TargetRevision != 1 || stepSnapshot.Step.TargetDigest != target.Digest || stepSnapshot.Step.Executor != nil || len(stepSnapshot.Step.NetworkGrants) != 0 || len(stepSnapshot.Step.SecretRefs) != 0 {
		return req, emptyProvider, emptyIntent, ErrEC2ProvisionInvalid
	}
	step := stepSnapshot.Step
	reservation := target.ComputeReservation
	provision := step.ComputeProvision
	if provision.InfrastructureProfileID != reservation.InfrastructureProfileID || provision.AMIParameter != reservation.AMIParameter || provision.InstanceType != reservation.InstanceType || provision.AvailabilityZone != reservation.AvailabilityZone || provision.VolumeGiB != reservation.VolumeGiB || provision.Region != target.Region || provision.Architecture != reservation.Architecture || provision.ManagementTransport != reservation.ManagementTransport || !provision.PublicIP || provision.PublicInbound {
		return req, emptyProvider, emptyIntent, ErrEC2ProvisionInvalid
	}
	ref := target.CredentialRefs[0]
	if credential.Validate() != nil || credential.ID != ref.Ref || credential.Revision != int64(ref.Revision) || credential.VerifiedRevision != int64(ref.Revision) || credential.AccountID != target.AccountID || credential.Region != target.Region {
		return req, emptyProvider, emptyIntent, ErrEC2ProvisionInvalid
	}
	requestDigest, err := ec2ProvisionRequestDigest(req, target, step)
	if err != nil || req.RequestDigest != "" && req.RequestDigest != requestDigest {
		return req, emptyProvider, emptyIntent, ErrEC2ProvisionInvalid
	}
	req.Target, req.Step, req.RequestDigest = target, step, requestDigest
	stackName := EC2ProvisionOperationKey(target.ID)
	providerRequest := CloudFormationCreateRequest{OwnerID: req.OwnerID, StackName: stackName, AccountID: target.AccountID, Region: target.Region, Credential: credential.handle(), CredentialID: ref.Ref, CredentialRevision: ref.Revision, ReservationTargetID: target.ID, ReservationDigest: target.Digest, Provision: *provision, RequestDigest: requestDigest}
	intent := EC2ProvisionIntent{OwnerID: req.OwnerID, FenceDigest: req.FenceDigest, RequestDigest: requestDigest, ProviderOperationKey: stackName, Request: req}
	return req, providerRequest, intent, nil
}

func ec2ProvisionRequestDigest(req EC2ProvisionRequest, target execution.ExecutionTarget, step execution.ExecutionStep) (execution.Digest, error) {
	return execution.CanonicalDigest(struct {
		OwnerID                                  string
		PlanID                                   string
		PlanRevision                             uint64
		PlanDigest                               execution.Digest
		RunID, StageID, AttemptID                string
		RunRevision, StageRevision, StepRevision uint64
		RunDigest, StageDigest                   execution.Digest
		TargetDigest, StepDigest, FenceDigest    execution.Digest
		PolicyDigest, CostQuoteDigest            execution.Digest
	}{req.OwnerID, req.PlanID, req.PlanRevision, req.PlanDigest, req.RunID, req.StageID, req.AttemptID, req.RunRevision, req.StageRevision, req.StepRevision, req.RunDigest, req.StageDigest, target.Digest, step.Digest, req.FenceDigest, req.PolicyDigest, req.CostQuoteDigest})
}

// CanonicalEC2ProvisionRequestDigest is used by the authoritative resolver to
// seal a request before the generic dispatch intent is persisted. It accepts
// no caller-supplied digest and validates the exact target/step snapshots.
func CanonicalEC2ProvisionRequestDigest(req EC2ProvisionRequest) (execution.Digest, error) {
	target, err := req.Target.Normalize()
	if err != nil || target.Digest != req.Target.Digest {
		return "", ErrEC2ProvisionInvalid
	}
	step, err := execution.StepSnapshotFromStep(req.Step, execution.StepSetForward)
	if err != nil || step.Step.Digest != req.Step.Digest {
		return "", ErrEC2ProvisionInvalid
	}
	req.RequestDigest = ""
	return ec2ProvisionRequestDigest(req, target, step.Step)
}

// EC2ProvisionOperationKey is the deterministic provider identity persisted
// before CreateStack. It is safe to use for readback even when the mutation
// response containing the stack ARN is lost.
func EC2ProvisionOperationKey(targetID string) string {
	compact := strings.ReplaceAll(strings.TrimSpace(targetID), "-", "")
	if len(compact) < 24 {
		return ""
	}
	return "dirextalk-v2-" + compact[:24]
}

// ValidateEC2ProvisionIntentSnapshot validates the redacted durable snapshot
// without needing credential secret material.
func ValidateEC2ProvisionIntentSnapshot(intent EC2ProvisionIntent) error {
	req := intent.Request
	if intent.OwnerID != req.OwnerID || intent.FenceDigest != req.FenceDigest || intent.RequestDigest != req.RequestDigest ||
		intent.ProviderOperationKey != EC2ProvisionOperationKey(req.Target.ID) || strings.TrimSpace(req.OwnerID) == "" ||
		!execution.ValidateUUID(req.PlanID) || req.PlanRevision == 0 || !req.PlanDigest.Valid() || !execution.ValidateUUID(req.RunID) || req.RunRevision == 0 || !req.RunDigest.Valid() ||
		!execution.ValidateUUID(req.StageID) || req.StageRevision == 0 || !req.StageDigest.Valid() || !execution.ValidateUUID(req.AttemptID) || req.StepRevision == 0 ||
		!req.PolicyDigest.Valid() || !req.CostQuoteDigest.Valid() || !req.FenceDigest.Valid() || !req.RequestDigest.Valid() {
		return ErrEC2ProvisionInvalid
	}
	target, err := req.Target.Normalize()
	if err != nil || target.Digest != req.Target.Digest || target.Kind != execution.TargetKindAWSComputeReservation || target.ComputeReservation == nil || target.Revision != 1 || len(target.CredentialRefs) != 1 {
		return ErrEC2ProvisionInvalid
	}
	stepSnapshot, err := execution.StepSnapshotFromStep(req.Step, execution.StepSetForward)
	if err != nil || stepSnapshot.Step.Digest != req.Step.Digest || stepSnapshot.Step.Kind != execution.StepComputeProvision || stepSnapshot.Step.ComputeProvision == nil ||
		stepSnapshot.Step.TargetID != target.ID || stepSnapshot.Step.TargetRevision != target.Revision || stepSnapshot.Step.TargetDigest != target.Digest ||
		stepSnapshot.Step.Executor != nil || len(stepSnapshot.Step.NetworkGrants) != 0 || len(stepSnapshot.Step.SecretRefs) != 0 {
		return ErrEC2ProvisionInvalid
	}
	provision, reservation := stepSnapshot.Step.ComputeProvision, target.ComputeReservation
	if provision.InfrastructureProfileID != reservation.InfrastructureProfileID || provision.AMIParameter != reservation.AMIParameter || provision.InstanceType != reservation.InstanceType ||
		provision.AvailabilityZone != reservation.AvailabilityZone || provision.VolumeGiB != reservation.VolumeGiB || provision.Region != target.Region || provision.Architecture != reservation.Architecture ||
		provision.ManagementTransport != reservation.ManagementTransport || !provision.PublicIP || provision.PublicInbound {
		return ErrEC2ProvisionInvalid
	}
	digest, err := ec2ProvisionRequestDigest(req, target, stepSnapshot.Step)
	if err != nil || digest != req.RequestDigest {
		return ErrEC2ProvisionInvalid
	}
	return nil
}

func terminalCloudFormationCreateFailure(status string) bool {
	switch strings.TrimSpace(status) {
	case "CREATE_FAILED", "ROLLBACK_COMPLETE", "ROLLBACK_FAILED", "DELETE_COMPLETE", "DELETE_FAILED":
		return true
	default:
		return false
	}
}

func validPendingProvisionReadback(req EC2ProvisionRequest, intent EC2ProvisionIntentRecord, readback CloudFormationProvisionReadback) bool {
	reservation := req.Target.ComputeReservation
	if reservation == nil || readback.Status != "CREATE_COMPLETE" || readback.StackName != intent.Intent.ProviderOperationKey ||
		!ValidEC2InstanceID(readback.InstanceID) || readback.InstanceType != reservation.InstanceType || readback.AvailabilityZone != reservation.AvailabilityZone || readback.Architecture != "x86_64" ||
		readback.VCPUCount <= 0 || readback.MemoryMiB <= 0 || readback.RootVolumeGiB != int32(reservation.VolumeGiB) || strings.TrimSpace(readback.PublicIP) == "" ||
		readback.PublicInbound || readback.HTTPSEgress != execution.ObservationFactHTTPSEgressValue || !readback.SecurityGroupDigest.Valid() ||
		readback.OperatingSystem != "" || readback.PlatformName != "" || readback.PlatformVersion != "" {
		return false
	}
	switch readback.PendingReason {
	case EC2ProvisionPendingSSMRegistration:
		return readback.SSMStatus == ""
	case EC2ProvisionPendingSSMOnline:
		return readback.SSMStatus == "ConnectionLost" || readback.SSMStatus == "Inactive"
	default:
		return false
	}
}

func provisionCompletion(req EC2ProvisionRequest, intent EC2ProvisionIntentRecord, readback CloudFormationProvisionReadback, observedAt time.Time) (EC2ProvisionCompletion, error) {
	var empty EC2ProvisionCompletion
	reservationSpec := req.Target.ComputeReservation
	if reservationSpec == nil || readback.StackName != intent.Intent.ProviderOperationKey || !ValidEC2InstanceID(readback.InstanceID) ||
		readback.InstanceType != reservationSpec.InstanceType || readback.AvailabilityZone != reservationSpec.AvailabilityZone || readback.Architecture != "x86_64" || readback.OperatingSystem != "linux" ||
		readback.SSMStatus != "Online" || readback.PlatformName != "Amazon Linux" || !strings.HasPrefix(readback.PlatformVersion, "2023") ||
		readback.VCPUCount <= 0 || readback.MemoryMiB <= 0 || readback.RootVolumeGiB != int32(reservationSpec.VolumeGiB) ||
		strings.TrimSpace(readback.PublicIP) == "" || readback.PublicInbound || readback.HTTPSEgress != execution.ObservationFactHTTPSEgressValue || !readback.SecurityGroupDigest.Valid() {
		return empty, ErrEC2ProvisionUncertain
	}
	reservation := req.Target
	instance, err := NormalizeInfrastructureTarget(execution.ExecutionTarget{
		ID: reservation.ID, Provider: "aws", Kind: execution.TargetKindAWSEC2Instance,
		InfrastructureProfileID: reservation.InfrastructureProfileID, AccountID: reservation.AccountID,
		Region: reservation.Region, Architecture: "x86_64", Revision: 2,
		Capabilities:   []string{"target.aws_ec2_instance", "target.instance." + readback.InstanceID, "transport.aws_ssm"},
		CredentialRefs: append([]execution.CredentialRef(nil), reservation.CredentialRefs...),
		Network:        execution.NetworkPolicy{Mode: execution.NetworkPolicyModeObservedHTTPSEgress},
	})
	if err != nil {
		return empty, ErrEC2ProvisionUncertain
	}
	observation, err := (execution.TargetObservation{TargetID: instance.ID, TargetRevision: instance.Revision, ObservedAt: observedAt, State: "ready", Facts: map[string]string{
		"instance_id": readback.InstanceID, "account_id": instance.AccountID, "region": instance.Region, "public_ip": readback.PublicIP,
		"instance_type": readback.InstanceType, execution.ObservationFactAvailabilityZone: readback.AvailabilityZone, "partition": provisionPartition(instance.Region),
		"architecture": "x86_64", "operating_system": "linux", "ssm_status": "Online",
		"platform_name": readback.PlatformName, "platform_version": readback.PlatformVersion,
		execution.ObservationFactVCPUCount:           fmt.Sprintf("%d", readback.VCPUCount),
		execution.ObservationFactMemoryMiB:           fmt.Sprintf("%d", readback.MemoryMiB),
		execution.ObservationFactRootVolumeGiB:       fmt.Sprintf("%d", readback.RootVolumeGiB),
		execution.ObservationFactHTTPSEgress:         execution.ObservationFactHTTPSEgressValue,
		execution.ObservationFactSecurityGroupDigest: string(readback.SecurityGroupDigest),
	}}).Normalize()
	if err != nil {
		return empty, ErrEC2ProvisionUncertain
	}
	return EC2ProvisionCompletion{Intent: intent, Target: instance, Observation: observation}, nil
}

func provisionPartition(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.Contains(region, "-gov-"):
		return "aws-us-gov"
	default:
		return "aws"
	}
}
