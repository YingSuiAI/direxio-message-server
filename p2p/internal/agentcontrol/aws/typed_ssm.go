package aws

// This file is the pure, typed AWS SSM transport core.  It intentionally does
// not know about storage, plans, routes, or workload/provider actions.  The
// coordinator supplies frozen, digest-bound values and receives only bounded,
// redacted evidence.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	SSMDocumentName                = "AWS-RunShellScript"
	SSMDocumentVersion             = "1"
	SSMPluginName                  = "aws:runShellScript"
	TargetInstanceCapabilityPrefix = "target.instance."
	MaxSSMOutputBytes              = 24 * 1024
	MaxSSMCommandBytes             = 64 * 1024
)

var (
	ErrTypedInvalid      = errors.New("aws ssm: invalid typed request")
	ErrTypedUnavailable  = errors.New("aws ssm: target unavailable")
	ErrTypedNotFound     = errors.New("aws ssm: command not found")
	ErrTypedUncertain    = errors.New("aws ssm: operation uncertain")
	ErrTypedProvider     = errors.New("aws ssm: provider operation failed")
	ErrTypedTerminal     = errors.New("aws ssm: invalid terminal response")
	ErrTypedReplay       = errors.New("aws ssm: dispatch fence already used")
	ErrTypedArtifact     = errors.New("aws ssm: immutable artifact unavailable")
	ec2InstanceIDPattern = regexp.MustCompile(`^i-[0-9a-f]{8,32}$`)
)

// ValidEC2InstanceID is shared by the public target-import boundary and the
// typed transport. Keeping one validator prevents the catalog from accepting
// an identity that the execution transport would later reject.
func ValidEC2InstanceID(value string) bool {
	return ec2InstanceIDPattern.MatchString(strings.TrimSpace(value))
}

type InspectRequest struct {
	OwnerID            string
	Target             execution.ExecutionTarget
	TargetID           string
	TargetRevision     uint64
	TargetDigest       execution.Digest
	ProfileID          string
	AccountID          string
	Region             string
	Partition          string
	Credential         Credentials
	CredentialID       string
	CredentialRevision uint64
	InstanceID         string
	TargetFacts        map[string]string
}

type InspectionRequest = InspectRequest

// ImmutableArtifactResolver is the only source of script bytes.  The
// resolver must return bytes for the exact immutable artifact digest; callers
// cannot place command text in a durable dispatch request.
type ImmutableArtifactResolver interface {
	ResolveArtifact(context.Context, string, execution.ArtifactRef) ([]byte, error)
}

type SecretValueResolver interface {
	ResolveSecretValues(context.Context, string, []execution.CredentialRef) ([]string, error)
}

// FrozenScript is the normalized execution.v2 script step. Secret values are
// intentionally absent; SecretRefs are resolved only for transient redaction.
type FrozenScript struct {
	Step         execution.ExecutionStep
	Artifact     execution.ArtifactRef
	OutputPolicy string
	OutputLimit  uint64
	Redaction    execution.RedactionPolicy
	SecretRefs   []execution.CredentialRef
}

type FrozenRequest struct {
	OwnerID            string
	PlanID             string
	PlanRevision       uint64
	PlanDigest         execution.Digest
	RunID              string
	RunRevision        uint64
	RunDigest          execution.Digest
	StageID            string
	StageRevision      uint64
	StageDigest        execution.Digest
	StepKey            string
	StepRevision       uint64
	StepDigest         execution.Digest
	AttemptID          string
	Attempt            uint64
	Fence              string
	FenceDigest        execution.Digest
	RequestDigest      execution.Digest
	Target             execution.ExecutionTarget
	TargetID           string
	TargetRevision     uint64
	TargetDigest       execution.Digest
	InstanceID         string
	Credential         Credentials
	CredentialID       string
	CredentialRevision uint64
	Observation        execution.TargetObservation
	Script             FrozenScript
}

type FrozenDispatchRequest = FrozenRequest

type DispatchRequest = FrozenRequest

type TargetObservation = execution.TargetObservation

type DispatchStatus string

const (
	DispatchAccepted              DispatchStatus = "accepted"
	DispatchCancellationRequested DispatchStatus = "cancellation_requested"
	DispatchUncertain             DispatchStatus = "uncertain"
)

type DispatchResult struct {
	Status            DispatchStatus
	CommandID         string
	ProviderOperation string
	RequestDigest     execution.Digest
	TargetID          string
	InstanceID        string
	DocumentName      string
	DocumentVersion   string
}

type CommandEvidence = DispatchResult

type PollRequest struct {
	OwnerID     string
	Frozen      FrozenRequest
	CommandID   string
	Known       bool
	FenceDigest execution.Digest
}

type DispatchReceipt struct {
	Frozen        FrozenRequest
	RequestDigest execution.Digest
	FenceDigest   execution.Digest
	CommandID     string
	InstanceID    string
	Status        DispatchStatus
}

type DispatchReceiptResolver interface {
	ResolveDispatchReceipt(context.Context, string, execution.Digest) (DispatchReceipt, error)
}

// CredentialBindingDigest is the server-owned, secret-free binding formula
// for a credential reference. It intentionally excludes encrypted envelopes
// and every plaintext credential value.
func CredentialBindingDigest(owner string, ref execution.CredentialRef, c Credentials) (execution.Digest, error) {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(ref.Ref) == "" || ref.Revision == 0 || c.ID != ref.Ref || c.Revision != int64(ref.Revision) || c.VerifiedRevision != int64(ref.Revision) || c.AccountID == "" || c.Region == "" || c.UserARN == "" {
		return "", ErrTypedInvalid
	}
	return execution.CanonicalDigest(struct {
		Owner, CredentialID, Purpose, AccountID, Region, UserARN string
		CredentialRevision, VerifiedRevision                     uint64
	}{owner, ref.Ref, ref.Purpose, c.AccountID, c.Region, c.UserARN, uint64(c.Revision), uint64(c.VerifiedRevision)})
}

// FrozenRequestSnapshot is the durable redacted request projection. It has no
// command bytes, envelope bytes, or secret material; Credentials contributes
// only verified identity metadata.
type FrozenRequestSnapshot struct {
	OwnerID             string                      `json:"owner_id"`
	PlanID              string                      `json:"plan_id"`
	PlanRevision        uint64                      `json:"plan_revision"`
	PlanDigest          execution.Digest            `json:"plan_digest"`
	RunID               string                      `json:"run_id"`
	RunRevision         uint64                      `json:"run_revision"`
	RunDigest           execution.Digest            `json:"run_digest"`
	StageID             string                      `json:"stage_id"`
	StageRevision       uint64                      `json:"stage_revision"`
	StageDigest         execution.Digest            `json:"stage_digest"`
	StepKey             string                      `json:"step_key"`
	StepRevision        uint64                      `json:"step_revision"`
	StepDigest          execution.Digest            `json:"step_digest"`
	AttemptID           string                      `json:"attempt_id"`
	Attempt             uint64                      `json:"attempt"`
	Fence               string                      `json:"fence"`
	FenceDigest         execution.Digest            `json:"fence_digest"`
	RequestDigest       execution.Digest            `json:"request_digest"`
	Target              execution.ExecutionTarget   `json:"target"`
	TargetID            string                      `json:"target_id"`
	TargetRevision      uint64                      `json:"target_revision"`
	TargetDigest        execution.Digest            `json:"target_digest"`
	InstanceID          string                      `json:"instance_id"`
	CredentialID        string                      `json:"credential_id"`
	CredentialRevision  uint64                      `json:"credential_revision"`
	CredentialAccountID string                      `json:"credential_account_id"`
	CredentialRegion    string                      `json:"credential_region"`
	CredentialUserARN   string                      `json:"credential_user_arn"`
	Observation         execution.TargetObservation `json:"observation"`
	Script              FrozenScript                `json:"script"`
}

func resolveArtifact(v any, ctx context.Context, owner string, ref execution.ArtifactRef) ([]byte, error) {
	switch r := v.(type) {
	case ImmutableArtifactResolver:
		return r.ResolveArtifact(ctx, owner, ref)
	default:
		return nil, ErrTypedArtifact
	}
}

func resolveSecretValues(v any, ctx context.Context, owner string, refs []execution.CredentialRef) ([]string, error) {
	switch r := v.(type) {
	case SecretValueResolver:
		return r.ResolveSecretValues(ctx, owner, refs)
	default:
		return nil, ErrTypedInvalid
	}
}

// ReadArtifactBytes is a bounded helper for owner-scoped filesystem adapters.
func ReadArtifactBytes(ctx context.Context, r io.Reader, size int64) ([]byte, error) {
	if size < 0 || size > 1<<30 {
		return nil, ErrTypedArtifact
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != size {
		return nil, ErrTypedArtifact
	}
	return b, nil
}

func SnapshotFromFrozen(req FrozenRequest) FrozenRequestSnapshot {
	return FrozenRequestSnapshot{OwnerID: req.OwnerID, PlanID: req.PlanID, PlanRevision: req.PlanRevision, PlanDigest: req.PlanDigest, RunID: req.RunID, RunRevision: req.RunRevision, RunDigest: req.RunDigest, StageID: req.StageID, StageRevision: req.StageRevision, StageDigest: req.StageDigest, StepKey: req.StepKey, StepRevision: req.StepRevision, StepDigest: req.StepDigest, AttemptID: req.AttemptID, Attempt: req.Attempt, Fence: req.Fence, FenceDigest: req.FenceDigest, RequestDigest: req.RequestDigest, Target: req.Target, TargetID: req.TargetID, TargetRevision: req.TargetRevision, TargetDigest: req.TargetDigest, InstanceID: req.InstanceID, CredentialID: req.CredentialID, CredentialRevision: uint64(req.CredentialRevision), CredentialAccountID: req.Credential.AccountID, CredentialRegion: req.Credential.Region, CredentialUserARN: req.Credential.UserARN, Observation: req.Observation, Script: req.Script}
}

type PollStatus string

const (
	PollPending               PollStatus = "pending"
	PollRunning               PollStatus = "running"
	PollSucceeded             PollStatus = "succeeded"
	PollFailed                PollStatus = "failed"
	PollCanceled              PollStatus = "canceled"
	PollUncertain             PollStatus = "uncertain"
	PollCancellationRequested PollStatus = "cancellation_requested"
)

type CommandResult struct {
	Status            PollStatus
	CommandID         string
	InstanceID        string
	ExitCode          *int32
	Stdout            string
	Stderr            string
	StdoutTruncated   bool
	StderrTruncated   bool
	OutputDigest      execution.Digest
	ProviderOperation string
}

type ReconcileResult struct{ CommandResult }
type CancelResult struct {
	Status            PollStatus
	CommandID         string
	ProviderOperation string
}

type SSMTransport struct {
	factory       TypedClientFactory
	artifacts     any
	secrets       any
	secretLeases  SecretParameterLeaseResolver
	secretRevoker SecretParameterLeaseRevoker
	receipts      DispatchReceiptResolver
	timeout       time.Duration
	mu            sync.Mutex
	// fences close replay within one transport lifetime. Durable coordinator
	// intent/receipt persistence remains the authority across restart.
	fences map[string]DispatchStatus
}

type SSMTransportOption func(*SSMTransport) error

func WithSSMTimeout(timeout time.Duration) SSMTransportOption {
	return func(t *SSMTransport) error {
		if timeout <= 0 || timeout > 5*time.Minute {
			return ErrTypedInvalid
		}
		t.timeout = timeout
		return nil
	}
}

func WithImmutableArtifactResolver(resolver any) SSMTransportOption {
	return func(t *SSMTransport) error {
		if resolver == nil {
			return ErrTypedInvalid
		}
		if _, ok := resolver.(ImmutableArtifactResolver); !ok {
			return ErrTypedInvalid
		}
		t.artifacts = resolver
		return nil
	}
}

func WithSecretValueResolver(resolver any) SSMTransportOption {
	return func(t *SSMTransport) error {
		if resolver == nil {
			return ErrTypedInvalid
		}
		if _, ok := resolver.(SecretValueResolver); !ok {
			return ErrTypedInvalid
		}
		t.secrets = resolver
		return nil
	}
}

func WithSecretParameterRuntime(resolver SecretParameterLeaseResolver, revoker SecretParameterLeaseRevoker) SSMTransportOption {
	return func(t *SSMTransport) error {
		if resolver == nil || revoker == nil {
			return ErrTypedInvalid
		}
		t.secretLeases = resolver
		t.secretRevoker = revoker
		return nil
	}
}

func WithDispatchReceiptResolver(resolver DispatchReceiptResolver) SSMTransportOption {
	return func(t *SSMTransport) error {
		if resolver == nil {
			return ErrTypedInvalid
		}
		t.receipts = resolver
		return nil
	}
}

func NewSSMTransport(factory TypedClientFactory, options ...SSMTransportOption) (*SSMTransport, error) {
	if factory == nil {
		return nil, ErrTypedInvalid
	}
	t := &SSMTransport{factory: factory, timeout: 30 * time.Second, fences: make(map[string]DispatchStatus)}
	for _, option := range options {
		if option == nil || option(t) != nil {
			return nil, ErrTypedInvalid
		}
	}
	if t.artifacts == nil || t.receipts == nil {
		return nil, ErrTypedInvalid
	}
	return t, nil
}

func (t *SSMTransport) context(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, t.timeout)
}

// CanExecuteStep is consulted by the runner before a durable dispatch intent
// exists. Secret-bearing container steps fail pre-dispatch unless both the
// independently-authorized lease resolver and exact revoker are wired.
func (t *SSMTransport) CanExecuteStep(step execution.ExecutionStep) bool {
	if t == nil || !IsExecutableSSMStep(step) {
		return false
	}
	if len(step.SecretRefs) == 0 {
		return true
	}
	if step.Kind == execution.StepContainerApply {
		return t.secretLeases != nil && t.secretRevoker != nil
	}
	return step.Kind == execution.StepScriptRun && t.secrets != nil
}

func (t *SSMTransport) Inspect(ctx context.Context, req InspectRequest) (TargetObservation, error) {
	if t == nil || t.factory == nil {
		return TargetObservation{}, ErrTypedInvalid
	}
	normalized, instanceID, handle, partition, err := validateInspect(req)
	if err != nil {
		return TargetObservation{}, err
	}
	stsClient, err := t.factory.NewSTS(handle)
	if err != nil {
		return TargetObservation{}, ErrTypedProvider
	}
	ec2c, err := t.factory.NewEC2(handle)
	if err != nil {
		return TargetObservation{}, ErrTypedProvider
	}
	ssmc, err := t.factory.NewSSM(handle)
	if err != nil {
		return TargetObservation{}, ErrTypedProvider
	}
	callCtx, cancel := t.context(ctx)
	defer cancel()
	identity, err := stsClient.GetCallerIdentity(callCtx, &sts.GetCallerIdentityInput{})
	if err != nil || identity == nil || awsapi.ToString(identity.Account) != normalized.AccountID {
		return TargetObservation{}, ErrTypedProvider
	}
	parsedARN, err := arn.Parse(awsapi.ToString(identity.Arn))
	if err != nil || (parsedARN.Service != "iam" && parsedARN.Service != "sts") || parsedARN.AccountID != normalized.AccountID || parsedARN.Partition != partition || (handle.UserARN != "" && handle.UserARN != parsedARN.String()) {
		return TargetObservation{}, ErrTypedProvider
	}
	ec2out, err := ec2c.DescribeInstances(callCtx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return TargetObservation{}, ErrTypedProvider
	}
	instances := make([]ec2types.Instance, 0, 1)
	for _, reservation := range ec2out.Reservations {
		instances = append(instances, reservation.Instances...)
	}
	if len(instances) != 1 || awsapi.ToString(instances[0].InstanceId) != instanceID || instances[0].State == nil || instances[0].State.Name != ec2types.InstanceStateNameRunning || instances[0].Platform == ec2types.PlatformValuesWindows {
		return unavailableObservation(normalized, "ec2_not_running_linux")
	}
	if !instanceArchitectureMatches(normalized.Architecture, instances[0].Architecture) {
		return unavailableObservation(normalized, "ec2_architecture_mismatch")
	}
	availabilityZone := ""
	if instances[0].Placement != nil {
		availabilityZone = strings.TrimSpace(awsapi.ToString(instances[0].Placement.AvailabilityZone))
	}
	if !execution.ValidateAvailabilityZone(normalized.Region, availabilityZone) {
		return unavailableObservation(normalized, "ec2_availability_zone_mismatch")
	}
	instanceType := instances[0].InstanceType
	if instanceType == "" {
		return unavailableObservation(normalized, "instance_capacity_unavailable")
	}
	typeOutput, err := ec2c.DescribeInstanceTypes(callCtx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []ec2types.InstanceType{instanceType}})
	if err != nil {
		return TargetObservation{}, ErrTypedProvider
	}
	if typeOutput == nil || len(typeOutput.InstanceTypes) != 1 || typeOutput.InstanceTypes[0].InstanceType != instanceType || typeOutput.InstanceTypes[0].VCpuInfo == nil || typeOutput.InstanceTypes[0].VCpuInfo.DefaultVCpus == nil || *typeOutput.InstanceTypes[0].VCpuInfo.DefaultVCpus <= 0 || typeOutput.InstanceTypes[0].MemoryInfo == nil || typeOutput.InstanceTypes[0].MemoryInfo.SizeInMiB == nil || *typeOutput.InstanceTypes[0].MemoryInfo.SizeInMiB <= 0 {
		return unavailableObservation(normalized, "instance_capacity_unavailable")
	}
	rootVolumeID := exactRootVolumeID(instances[0])
	if rootVolumeID == "" {
		return unavailableObservation(normalized, "root_volume_unavailable")
	}
	volumeOutput, err := ec2c.DescribeVolumes(callCtx, &ec2.DescribeVolumesInput{VolumeIds: []string{rootVolumeID}})
	if err != nil {
		return TargetObservation{}, ErrTypedProvider
	}
	if volumeOutput == nil || len(volumeOutput.Volumes) != 1 || awsapi.ToString(volumeOutput.Volumes[0].VolumeId) != rootVolumeID || volumeOutput.Volumes[0].Size == nil || *volumeOutput.Volumes[0].Size <= 0 {
		return unavailableObservation(normalized, "root_volume_unavailable")
	}
	vcpuCount := *typeOutput.InstanceTypes[0].VCpuInfo.DefaultVCpus
	memoryMiB := *typeOutput.InstanceTypes[0].MemoryInfo.SizeInMiB
	rootVolumeGiB := *volumeOutput.Volumes[0].Size
	securityGroupIDs := make([]string, 0, len(instances[0].SecurityGroups))
	seenSecurityGroups := make(map[string]struct{}, len(instances[0].SecurityGroups))
	for _, group := range instances[0].SecurityGroups {
		id := strings.TrimSpace(awsapi.ToString(group.GroupId))
		if id == "" {
			continue
		}
		if _, duplicate := seenSecurityGroups[id]; duplicate {
			continue
		}
		seenSecurityGroups[id] = struct{}{}
		securityGroupIDs = append(securityGroupIDs, id)
	}
	sort.Strings(securityGroupIDs)
	if len(securityGroupIDs) == 0 {
		return unavailableObservation(normalized, "security_groups_unavailable")
	}
	securityGroups, err := ec2c.DescribeSecurityGroups(callCtx, &ec2.DescribeSecurityGroupsInput{GroupIds: securityGroupIDs})
	if err != nil || securityGroups == nil || !exactSecurityGroupSet(securityGroupIDs, securityGroups.SecurityGroups) {
		return TargetObservation{}, ErrTypedProvider
	}
	httpsEgress := hasPublicHTTPSEgress(securityGroups.SecurityGroups)
	var securityGroupDigest execution.Digest
	if normalized.Network.Mode == execution.NetworkPolicyModeObservedHTTPSEgress {
		if len(securityGroups.SecurityGroups) != 1 {
			return unavailableObservation(normalized, "https_egress_not_observed")
		}
		securityGroupDigest, httpsEgress = exactManagementSecurityGroupDigest(
			securityGroups.SecurityGroups[0], normalized.AccountID,
			awsapi.ToString(instances[0].VpcId), securityGroupIDs[0],
		)
		if !httpsEgress {
			return unavailableObservation(normalized, "https_egress_not_observed")
		}
	}
	ssmout, err := ssmc.DescribeInstanceInformation(callCtx, &ssm.DescribeInstanceInformationInput{Filters: []ssmtypes.InstanceInformationStringFilter{{Key: awsapi.String("InstanceIds"), Values: []string{instanceID}}}})
	if err != nil {
		return TargetObservation{}, ErrTypedProvider
	}
	if len(ssmout.InstanceInformationList) != 1 || awsapi.ToString(ssmout.InstanceInformationList[0].InstanceId) != instanceID || ssmout.InstanceInformationList[0].PingStatus != ssmtypes.PingStatusOnline || ssmout.InstanceInformationList[0].PlatformType == ssmtypes.PlatformTypeWindows {
		return unavailableObservation(normalized, "ssm_not_online_linux")
	}
	info := ssmout.InstanceInformationList[0]
	now := time.Now().UTC()
	obs := execution.TargetObservation{TargetID: normalized.ID, TargetRevision: normalized.Revision, ObservedAt: now, State: "ready", Facts: map[string]string{
		"instance_id":   instanceID,
		"instance_type": string(instanceType),
		execution.ObservationFactAvailabilityZone: availabilityZone,
		"account_id":                           normalized.AccountID,
		"region":                               normalized.Region,
		"partition":                            partition,
		"operating_system":                     "linux",
		"architecture":                         normalized.Architecture,
		"ssm_status":                           "Online",
		"platform_name":                        awsapi.ToString(info.PlatformName),
		"platform_version":                     awsapi.ToString(info.PlatformVersion),
		execution.ObservationFactVCPUCount:     strconv.FormatInt(int64(vcpuCount), 10),
		execution.ObservationFactMemoryMiB:     strconv.FormatInt(memoryMiB, 10),
		execution.ObservationFactRootVolumeGiB: strconv.FormatInt(int64(rootVolumeGiB), 10),
	}}
	if httpsEgress {
		if !securityGroupDigest.Valid() {
			var digestErr error
			securityGroupDigest, digestErr = execution.CanonicalDigest(securityGroupIDs)
			if digestErr != nil {
				return TargetObservation{}, ErrTypedProvider
			}
		}
		obs.Facts[execution.ObservationFactHTTPSEgress] = execution.ObservationFactHTTPSEgressValue
		obs.Facts[execution.ObservationFactSecurityGroupDigest] = string(securityGroupDigest)
	}
	return obs.Normalize()
}

func instanceArchitectureMatches(want string, got ec2types.ArchitectureValues) bool {
	switch want {
	case "x86_64":
		return got == ec2types.ArchitectureValuesX8664
	case "arm64":
		return got == ec2types.ArchitectureValuesArm64
	default:
		return false
	}
}

func exactRootVolumeID(instance ec2types.Instance) string {
	rootDevice := strings.TrimSpace(awsapi.ToString(instance.RootDeviceName))
	if rootDevice == "" {
		return ""
	}
	var found string
	for _, mapping := range instance.BlockDeviceMappings {
		if strings.TrimSpace(awsapi.ToString(mapping.DeviceName)) != rootDevice || mapping.Ebs == nil {
			continue
		}
		volumeID := strings.TrimSpace(awsapi.ToString(mapping.Ebs.VolumeId))
		if volumeID == "" || found != "" {
			return ""
		}
		found = volumeID
	}
	return found
}

func exactSecurityGroupSet(expected []string, groups []ec2types.SecurityGroup) bool {
	if len(expected) != len(groups) {
		return false
	}
	actual := make([]string, 0, len(groups))
	for _, group := range groups {
		id := strings.TrimSpace(awsapi.ToString(group.GroupId))
		if id == "" {
			return false
		}
		actual = append(actual, id)
	}
	sort.Strings(actual)
	for i := range expected {
		if expected[i] != actual[i] || i > 0 && actual[i] == actual[i-1] {
			return false
		}
	}
	return true
}

func hasPublicHTTPSEgress(groups []ec2types.SecurityGroup) bool {
	for _, group := range groups {
		for _, permission := range group.IpPermissionsEgress {
			protocol := strings.ToLower(strings.TrimSpace(awsapi.ToString(permission.IpProtocol)))
			covers443 := protocol == "-1" || protocol == "tcp" && permission.FromPort != nil && permission.ToPort != nil && *permission.FromPort <= 443 && *permission.ToPort >= 443
			if !covers443 {
				continue
			}
			for _, ipRange := range permission.IpRanges {
				if strings.TrimSpace(awsapi.ToString(ipRange.CidrIp)) == "0.0.0.0/0" {
					return true
				}
			}
			for _, ipRange := range permission.Ipv6Ranges {
				if strings.TrimSpace(awsapi.ToString(ipRange.CidrIpv6)) == "::/0" {
					return true
				}
			}
		}
	}
	return false
}

func (t *SSMTransport) InspectTarget(ctx context.Context, req InspectRequest) (TargetObservation, error) {
	return t.Inspect(ctx, req)
}

func unavailableObservation(target execution.ExecutionTarget, warning string) (TargetObservation, error) {
	obs := execution.TargetObservation{TargetID: target.ID, TargetRevision: target.Revision, ObservedAt: time.Now().UTC(), State: "unavailable", Partial: true, Warnings: []string{warning}}
	n, err := obs.Normalize()
	if err != nil {
		return TargetObservation{}, err
	}
	return n, ErrTypedUnavailable
}

func (t *SSMTransport) Dispatch(ctx context.Context, req FrozenRequest) (DispatchResult, error) {
	if t == nil || t.factory == nil {
		return DispatchResult{}, ErrTypedInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	instanceID, handle, err := validateFrozenBinding(req)
	if err != nil {
		return DispatchResult{}, err
	}
	if t.artifacts == nil {
		return DispatchResult{}, ErrTypedArtifact
	}
	step, err := normalizeFrozenScript(req)
	if err != nil {
		return DispatchResult{}, err
	}
	if !t.CanExecuteStep(step.Step) {
		return DispatchResult{}, ErrTypedInvalid
	}
	if step.OutputPolicy == execution.OutputCapture && len(step.SecretRefs) > 0 {
		if t.secrets == nil {
			return DispatchResult{}, ErrTypedInvalid
		}
		if _, secretErr := resolveSecretValues(t.secrets, ctx, req.OwnerID, step.SecretRefs); secretErr != nil {
			return DispatchResult{}, ErrTypedInvalid
		}
	}
	if err := validateObservation(req); err != nil {
		return DispatchResult{}, err
	}
	artifactBytes, err := resolveArtifact(t.artifacts, ctx, req.OwnerID, step.Artifact)
	if err != nil || !artifactDigestMatches(step.Artifact, artifactBytes) {
		return DispatchResult{}, ErrTypedArtifact
	}
	secretParameters := []string(nil)
	if step.Step.Kind == execution.StepContainerApply && len(step.SecretRefs) > 0 {
		lease, _, leaseErr := t.resolveSecretParameterLease(ctx, req, step.SecretRefs)
		if leaseErr != nil {
			return DispatchResult{}, ErrTypedInvalid
		}
		secretParameters = []string{lease.ParameterName}
	}
	command, err := renderSSMCommandWithSecretParameters(step, artifactBytes, secretParameters)
	if err != nil {
		return DispatchResult{}, err
	}
	fenceDigest, err := computedFenceDigest(req)
	if err != nil || req.FenceDigest != fenceDigest {
		return DispatchResult{}, ErrTypedInvalid
	}
	requestDigest, err := frozenRequestDigest(req, step, fenceDigest)
	if err != nil {
		return DispatchResult{}, ErrTypedInvalid
	}
	if req.RequestDigest != requestDigest {
		return DispatchResult{}, ErrTypedInvalid
	}
	if !t.claimFence(string(req.FenceDigest)) {
		return DispatchResult{}, ErrTypedReplay
	}
	// Revalidate the exact pinned instance immediately before mutation. A
	// durable observation is a binding, not a substitute for this read.
	partition := req.Observation.Facts["partition"]
	if partition == "" {
		partition = "aws"
	}
	observed, inspectErr := t.Inspect(ctx, inspectRequestFromFrozen(req, partition, instanceID))
	if inspectErr != nil || !liveObservationMatchesFrozen(observed, req.Observation, req.Target) {
		t.releaseFence(string(req.FenceDigest))
		if inspectErr != nil {
			return DispatchResult{}, inspectErr
		}
		return DispatchResult{}, ErrTypedUnavailable
	}
	client, err := t.factory.NewSSMCommand(handle)
	if err != nil {
		t.releaseFence(string(req.FenceDigest))
		return DispatchResult{}, ErrTypedProvider
	}
	callCtx, cancel := t.context(ctx)
	defer cancel()
	out, err := client.SendCommand(callCtx, &ssm.SendCommandInput{DocumentName: awsapi.String(SSMDocumentName), DocumentVersion: awsapi.String(SSMDocumentVersion), InstanceIds: []string{instanceID}, Parameters: map[string][]string{"commands": {command}}, TimeoutSeconds: awsapi.Int32(int32(step.Step.TimeoutSeconds)), MaxConcurrency: awsapi.String("1"), MaxErrors: awsapi.String("0")})
	if err != nil || out == nil || out.Command == nil || awsapi.ToString(out.Command.CommandId) == "" {
		// SendCommand may have reached AWS. Never retry or turn this into a
		// normal provider failure: the only safe result is uncertainty.
		t.markFence(string(req.FenceDigest), DispatchUncertain)
		return DispatchResult{Status: DispatchUncertain, RequestDigest: requestDigest, TargetID: req.TargetID, InstanceID: instanceID, DocumentName: SSMDocumentName, DocumentVersion: SSMDocumentVersion}, ErrTypedUncertain
	}
	id := awsapi.ToString(out.Command.CommandId)
	t.markFence(string(req.FenceDigest), DispatchAccepted)
	return DispatchResult{Status: DispatchAccepted, CommandID: id, ProviderOperation: "ssm.send_command", RequestDigest: requestDigest, TargetID: req.TargetID, InstanceID: instanceID, DocumentName: SSMDocumentName, DocumentVersion: SSMDocumentVersion}, nil
}

func liveObservationMatchesFrozen(live, frozen execution.TargetObservation, target execution.ExecutionTarget) bool {
	if live.TargetID != frozen.TargetID || live.TargetRevision != frozen.TargetRevision || live.State != "ready" || live.Partial || live.Stale {
		return false
	}
	for _, key := range []string{"instance_id", "instance_type", execution.ObservationFactAvailabilityZone, "account_id", "region", "partition", "operating_system", "architecture", "ssm_status", "platform_name", "platform_version", execution.ObservationFactVCPUCount, execution.ObservationFactMemoryMiB, execution.ObservationFactRootVolumeGiB} {
		if expected := frozen.Facts[key]; expected != "" && live.Facts[key] != expected {
			return false
		}
	}
	if target.Network.Mode == execution.NetworkPolicyModeObservedHTTPSEgress {
		return live.Facts[execution.ObservationFactHTTPSEgress] == execution.ObservationFactHTTPSEgressValue &&
			live.Facts[execution.ObservationFactSecurityGroupDigest] == frozen.Facts[execution.ObservationFactSecurityGroupDigest]
	}
	return true
}

func (t *SSMTransport) resolveSecretParameterLease(ctx context.Context, frozen FrozenRequest, refs []execution.CredentialRef) (SecretParameterLease, SecretAccessAuthorization, error) {
	var zero SecretParameterLease
	if t == nil || t.secretLeases == nil || len(refs) != 1 {
		return zero, SecretAccessAuthorization{}, ErrTypedInvalid
	}
	req := AuthorizedSecretRequest{Mode: "consume", OwnerID: frozen.OwnerID, PlanID: frozen.PlanID, PlanRevision: frozen.PlanRevision, PlanDigest: frozen.PlanDigest, RunID: frozen.RunID, RunRevision: frozen.RunRevision, RunDigest: frozen.RunDigest, StageID: frozen.StageID, StageRevision: frozen.StageRevision, StageDigest: frozen.StageDigest, AttemptID: frozen.AttemptID, TargetID: frozen.TargetID, TargetRevision: frozen.TargetRevision, TargetDigest: frozen.TargetDigest, SecretRefs: append([]execution.CredentialRef(nil), refs...)}
	lease, authorization, err := t.secretLeases.ResolveAuthorizedSecretParameterLease(ctx, req)
	if err != nil || validateConsumedSecretParameterLease(req, lease, authorization) != nil || !validTargetSecretParameterName(lease.ParameterName, frozen.TargetID) {
		return zero, SecretAccessAuthorization{}, ErrTypedInvalid
	}
	return lease, authorization, nil
}

func (t *SSMTransport) DispatchCommand(ctx context.Context, req FrozenRequest) (DispatchResult, error) {
	return t.Dispatch(ctx, req)
}

func (t *SSMTransport) claimFence(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if key == "" {
		return false
	}
	if _, exists := t.fences[key]; exists {
		return false
	}
	t.fences[key] = "in_flight"
	return true
}
func (t *SSMTransport) releaseFence(key string) { t.mu.Lock(); delete(t.fences, key); t.mu.Unlock() }
func (t *SSMTransport) markFence(key string, status DispatchStatus) {
	t.mu.Lock()
	t.fences[key] = status
	t.mu.Unlock()
}

func inspectRequestFromFrozen(req FrozenRequest, partition, instanceID string) InspectRequest {
	return InspectRequest{OwnerID: req.OwnerID, Target: req.Target, TargetID: req.TargetID, TargetRevision: req.TargetRevision, TargetDigest: req.TargetDigest, ProfileID: req.Target.InfrastructureProfileID, AccountID: req.Target.AccountID, Region: req.Target.Region, Partition: partition, Credential: req.Credential, CredentialID: req.CredentialID, CredentialRevision: req.CredentialRevision, InstanceID: instanceID}
}

func validateObservation(req FrozenRequest) error {
	obs, err := req.Observation.Normalize()
	if err != nil || obs.TargetID != req.TargetID || obs.TargetRevision != req.TargetRevision {
		return ErrTypedInvalid
	}
	if obs.Facts["instance_id"] != req.InstanceID || obs.Facts["account_id"] != req.Target.AccountID || obs.Facts["region"] != req.Target.Region || !execution.ValidateAvailabilityZone(req.Target.Region, obs.Facts[execution.ObservationFactAvailabilityZone]) || obs.Facts["operating_system"] != "linux" || obs.Facts["architecture"] != req.Target.Architecture || obs.Facts["ssm_status"] != "Online" || !validPositiveDecimal(obs.Facts[execution.ObservationFactVCPUCount]) || !validPositiveDecimal(obs.Facts[execution.ObservationFactMemoryMiB]) || !validPositiveDecimal(obs.Facts[execution.ObservationFactRootVolumeGiB]) {
		return ErrTypedInvalid
	}
	if req.Target.Network.Mode == execution.NetworkPolicyModeObservedHTTPSEgress && (obs.Facts[execution.ObservationFactHTTPSEgress] != execution.ObservationFactHTTPSEgressValue || !execution.ValidateDigest(obs.Facts[execution.ObservationFactSecurityGroupDigest])) {
		return ErrTypedInvalid
	}
	return nil
}

func validPositiveDecimal(value string) bool {
	n, err := strconv.ParseUint(value, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == value
}

func normalizeFrozenScript(req FrozenRequest) (FrozenScript, error) {
	s := req.Script
	if s.Step.Digest == "" || !s.Step.Digest.Valid() || req.StepDigest != s.Step.Digest {
		return FrozenScript{}, ErrTypedInvalid
	}
	snapshot, err := execution.StepSnapshotFromStep(s.Step, execution.StepSetForward)
	if err != nil {
		return FrozenScript{}, ErrTypedInvalid
	}
	step := snapshot.Step
	provided := step.Digest
	if err != nil || provided == "" {
		return FrozenScript{}, ErrTypedInvalid
	}
	if req.StepDigest != step.Digest || req.StepKey != step.StepKey {
		return FrozenScript{}, ErrTypedInvalid
	}
	executor, err := executorSpecForStep(step)
	if err != nil {
		return FrozenScript{}, ErrTypedInvalid
	}
	if s.Artifact.ID == "" {
		s.Artifact = executor.Artifact
	}
	if s.OutputPolicy == "" {
		s.OutputPolicy = step.OutputPolicy
	}
	if s.OutputLimit == 0 {
		s.OutputLimit = executor.OutputLimit
	}
	if len(s.Redaction.Patterns) == 0 && s.Redaction.Replace == "" {
		s.Redaction = executor.Redaction
	}
	if s.SecretRefs == nil {
		s.SecretRefs = append([]execution.CredentialRef(nil), step.SecretRefs...)
	}
	if s.OutputPolicy == "" && step.OutputPolicy == "" {
		s.OutputPolicy = execution.OutputDiscard
	}
	if s.OutputPolicy == execution.OutputArtifact {
		return FrozenScript{}, ErrTypedInvalid
	}
	if executor.Artifact != s.Artifact || !s.Artifact.Immutable || s.Artifact.Validate() != nil || !allowedSSMInterpreter(executor.Interpreter) || len(executor.Argv) > 64 || executor.CWD == "" || strings.ContainsAny(executor.CWD, "\r\n\x00") || executor.OutputLimit == 0 || executor.OutputLimit > 16<<20 || !validSSMOutputPolicy(s.OutputPolicy) || step.OutputPolicy != "" && step.OutputPolicy != s.OutputPolicy || executor.OutputLimit != s.OutputLimit || !equalRedaction(executor.Redaction, s.Redaction) || !equalCredentialRefs(step.SecretRefs, s.SecretRefs) || step.TargetID != req.TargetID || step.TargetRevision != req.TargetRevision || step.TargetDigest != req.TargetDigest || !equalPostconditionLocal(step.Postcondition, executor.Postcondition) {
		return FrozenScript{}, ErrTypedInvalid
	}
	if step.Kind == execution.StepScriptRun {
		if !equalCredentialRefs(step.ScriptRun.SecretRefs, s.SecretRefs) || step.TimeoutSeconds != step.ScriptRun.TimeoutSeconds || step.IdempotencyMarker != step.ScriptRun.IdempotencyMarker {
			return FrozenScript{}, ErrTypedInvalid
		}
	} else if step.Kind == execution.StepContainerApply {
		if !equalCredentialRefs(step.SecretRefs, s.SecretRefs) || len(s.SecretRefs) > 1 {
			return FrozenScript{}, ErrTypedInvalid
		}
		for _, ref := range s.SecretRefs {
			if !validExecutionSecretRef(ref) {
				return FrozenScript{}, ErrTypedInvalid
			}
		}
	} else if len(s.SecretRefs) != 0 {
		return FrozenScript{}, ErrTypedInvalid
	}
	if strings.ContainsAny(step.IdempotencyMarker, "\r\n\x00") || step.IdempotencyMarker == "" {
		return FrozenScript{}, ErrTypedInvalid
	}
	for _, pattern := range executor.Redaction.Patterns {
		if _, err := regexp.Compile(pattern); err != nil {
			return FrozenScript{}, ErrTypedInvalid
		}
	}
	for _, arg := range executor.Argv {
		if strings.ContainsAny(arg, "\r\n\x00") {
			return FrozenScript{}, ErrTypedInvalid
		}
	}
	for _, code := range executor.AllowedExitCodes {
		if code < 0 || code > 255 {
			return FrozenScript{}, ErrTypedInvalid
		}
	}
	for k, v := range executor.Env {
		if !validEnvNameLocal(k) || sensitiveEnvName(k) || strings.ContainsAny(v, "\r\n\x00") || len(v) > 4096 {
			return FrozenScript{}, ErrTypedInvalid
		}
	}
	s.Step = step
	return s, nil
}

// IsExecutableSSMStep reports whether a normalized execution.v2 step carries
// a complete, immutable SSM executor binding. It never generates an artifact.
func IsExecutableSSMStep(step execution.ExecutionStep) bool {
	_, err := executorSpecForStep(step)
	return err == nil
}

// FrozenScriptForStep creates the redacted frozen projection used by durable
// dispatch intent storage. Command bytes remain in the artifact store.
func FrozenScriptForStep(step execution.ExecutionStep) (FrozenScript, error) {
	executor, err := executorSpecForStep(step)
	if err != nil {
		return FrozenScript{}, err
	}
	return FrozenScript{Step: step, Artifact: executor.Artifact, OutputPolicy: step.OutputPolicy, OutputLimit: executor.OutputLimit, Redaction: executor.Redaction, SecretRefs: append([]execution.CredentialRef(nil), step.SecretRefs...)}, nil
}

func executorSpecForStep(step execution.ExecutionStep) (execution.ExecutorSpec, error) {
	if step.Kind == execution.StepScriptRun {
		if step.ScriptRun == nil || step.Executor != nil {
			return execution.ExecutorSpec{}, ErrTypedInvalid
		}
		sr := step.ScriptRun
		return execution.ExecutorSpec{Artifact: sr.Artifact, Interpreter: sr.Interpreter, Argv: append([]string(nil), sr.Argv...), CWD: sr.CWD, Env: cloneStringMap(sr.Env), Root: sr.Root, AllowedExitCodes: append([]int(nil), sr.AllowedExitCodes...), OutputLimit: sr.OutputLimit, Redaction: sr.Redaction, Postcondition: sr.Postcondition}, nil
	}
	switch step.Kind {
	case execution.StepPackageEnsure, execution.StepContainerApply, execution.StepHTTPProbe, execution.StepCleanup:
	default:
		return execution.ExecutorSpec{}, ErrTypedInvalid
	}
	if step.Executor == nil || step.ScriptRun != nil || step.ObservationRef == nil || step.Postcondition == nil || step.OutputPolicy != execution.OutputDiscard || step.Kind != execution.StepContainerApply && len(step.SecretRefs) != 0 || step.Kind == execution.StepContainerApply && len(step.SecretRefs) > 1 {
		return execution.ExecutorSpec{}, ErrTypedInvalid
	}
	if step.Kind == execution.StepContainerApply {
		for _, ref := range step.SecretRefs {
			if !validExecutionSecretRef(ref) {
				return execution.ExecutorSpec{}, ErrTypedInvalid
			}
		}
	}
	if step.Kind == execution.StepHTTPProbe && step.Executor.Root || step.Kind != execution.StepHTTPProbe && !step.Executor.Root {
		return execution.ExecutorSpec{}, ErrTypedInvalid
	}
	return *step.Executor, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func artifactDigestMatches(ref execution.ArtifactRef, data []byte) bool {
	sum := sha256.Sum256(data)
	return (ref.Size == 0 || int64(len(data)) == ref.Size) && execution.Digest(hex.EncodeToString(sum[:])) == ref.Digest
}

func renderSSMCommand(script FrozenScript, data []byte) (string, error) {
	return renderSSMCommandWithSecretParameters(script, data, nil)
}

func renderSSMCommandWithSecretParameters(script FrozenScript, data []byte, secretParameters []string) (string, error) {
	executor, err := executorSpecForStep(script.Step)
	if err != nil {
		return "", ErrTypedInvalid
	}
	if len(secretParameters) != len(script.SecretRefs) || len(secretParameters) > 0 && script.Step.Kind != execution.StepContainerApply {
		return "", ErrTypedInvalid
	}
	for _, name := range secretParameters {
		if !validTargetSecretParameterName(name, script.Step.TargetID) {
			return "", ErrTypedInvalid
		}
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	dirMode := "700"
	fileMode := "700"
	nonRootPreflight := ""
	launch := ""
	if !executor.Root {
		dirMode = "755"
		fileMode = "755"
		nonRootPreflight = "command -v runuser >/dev/null 2>&1 && id -u ec2-user >/dev/null 2>&1 || exit 125; "
		launch = " runuser -u ec2-user --"
	}
	parts := []string{"cd -- ", shellQuote(executor.CWD), " && umask 077 && dir=$(mktemp -d /tmp/dirextalk.XXXXXX) || exit 125; chmod ", dirMode, " \"$dir\" && trap 'rm -rf -- \"$dir\"' EXIT && file=$(mktemp \"$dir/script.XXXXXX\") || exit 125; test -f \"$file\" && test ! -L \"$file\" || exit 125; chmod ", fileMode, " \"$file\" && printf '%s' ", shellQuote(encoded), " | base64 --decode > \"$file\" || exit 125; ", nonRootPreflight, "env"}
	keys := make([]string, 0, len(executor.Env))
	for k := range executor.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, " ", k+"="+shellQuote(executor.Env[k]))
	}
	parts = append(parts, launch, " ", shellQuote(executor.Interpreter), " \"$file\"")
	for _, arg := range executor.Argv {
		parts = append(parts, " ", shellQuote(arg))
	}
	for _, parameter := range secretParameters {
		parts = append(parts, " ", shellQuote(parameter))
	}
	parts = append(parts, "; rc=$?; exit $rc")
	command := strings.Join(parts, "")
	if len(command) > MaxSSMCommandBytes {
		return "", ErrTypedInvalid
	}
	return command, nil
}

func validTargetSecretParameterName(name, targetID string) bool {
	if !execution.ValidateUUID(targetID) || len(name) == 0 || len(name) > 1011 || strings.ContainsAny(name, "\r\n\x00") {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "dirextalk" || parts[2] != "execution-v2" || parts[3] != targetID || !execution.ValidateUUID(parts[4]) || len(parts[5]) != 32 {
		return false
	}
	for _, r := range parts[5] {
		if r < '0' || r > '9' && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func allowedSSMInterpreter(s string) bool {
	return s == "/bin/sh" || s == "/bin/bash" || s == "/usr/bin/sh" || s == "/usr/bin/bash"
}
func validEnvNameLocal(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') || i == 0 && r >= '0' && r <= '9' {
			return false
		}
	}
	return true
}
func sensitiveEnvName(s string) bool {
	u := strings.ToUpper(s)
	return strings.Contains(u, "SECRET") || strings.Contains(u, "PASSWORD") || strings.Contains(u, "TOKEN") || strings.Contains(u, "PRIVATE_KEY")
}
func validSSMOutputPolicy(s string) bool {
	return s == execution.OutputDiscard || s == execution.OutputCapture || s == execution.OutputArtifact
}
func equalRedaction(a, b execution.RedactionPolicy) bool {
	if a.Replace != b.Replace || len(a.Patterns) != len(b.Patterns) {
		return false
	}
	aa, bb := append([]string(nil), a.Patterns...), append([]string(nil), b.Patterns...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
func equalCredentialRefs(a, b []execution.CredentialRef) bool {
	if len(a) != len(b) {
		return false
	}
	aa, bb := append([]execution.CredentialRef(nil), a...), append([]execution.CredentialRef(nil), b...)
	sort.Slice(aa, func(i, j int) bool { return aa[i].Ref < aa[j].Ref })
	sort.Slice(bb, func(i, j int) bool { return bb[i].Ref < bb[j].Ref })
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
func equalPostconditionLocal(a, b *execution.Postcondition) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func computedFenceDigest(req FrozenRequest) (execution.Digest, error) {
	return execution.CanonicalDigest(struct {
		OwnerID, PlanID, RunID, StageID, StepKey, AttemptID, Fence      string
		PlanRevision, RunRevision, StageRevision, StepRevision, Attempt uint64
		PlanDigest, RunDigest, StageDigest, StepDigest, TargetDigest    execution.Digest
		TargetID                                                        string
		TargetRevision                                                  uint64
	}{req.OwnerID, req.PlanID, req.RunID, req.StageID, req.StepKey, req.AttemptID, req.Fence, req.PlanRevision, req.RunRevision, req.StageRevision, req.StepRevision, req.Attempt, req.PlanDigest, req.RunDigest, req.StageDigest, req.StepDigest, req.TargetDigest, req.TargetID, req.TargetRevision})
}

// CanonicalFenceDigest and CanonicalRequestDigest are the only exported
// helpers for adapters that persist a frozen execution fence.
func CanonicalFenceDigest(req FrozenRequest) (execution.Digest, error) {
	return computedFenceDigest(req)
}
func CanonicalRequestDigest(req FrozenRequest) (execution.Digest, error) {
	script, err := normalizeFrozenScript(req)
	if err != nil {
		return "", err
	}
	fence, err := computedFenceDigest(req)
	if err != nil {
		return "", err
	}
	return frozenRequestDigest(req, script, fence)
}

func frozenRequestDigest(req FrozenRequest, script FrozenScript, fenceDigest execution.Digest) (execution.Digest, error) {
	executor, err := executorSpecForStep(script.Step)
	if err != nil {
		return "", ErrTypedInvalid
	}
	return execution.CanonicalDigest(struct {
		OwnerID            string
		PlanID             string
		PlanRevision       uint64
		PlanDigest         execution.Digest
		RunID              string
		RunRevision        uint64
		RunDigest          execution.Digest
		StageID            string
		StageRevision      uint64
		StageDigest        execution.Digest
		StepKey            string
		StepRevision       uint64
		StepDigest         execution.Digest
		AttemptID          string
		Attempt            uint64
		Fence              string
		FenceDigest        execution.Digest
		TargetID           string
		TargetRevision     uint64
		TargetDigest       execution.Digest
		InstanceID         string
		ObservationDigest  execution.Digest
		ArtifactDigest     execution.Digest
		ArtifactID         string
		ArtifactMediaType  string
		ArtifactSize       int64
		Interpreter        string
		Argv               []string
		CWD                string
		Env                map[string]string
		OutputPolicy       string
		OutputLimit        uint64
		Redaction          execution.RedactionPolicy
		SecretRefs         []execution.CredentialRef
		TimeoutSeconds     uint64
		IdempotencyMarker  string
		CredentialID       string
		CredentialRevision uint64
		AccountID          string
		UserARN            string
	}{req.OwnerID, req.PlanID, req.PlanRevision, req.PlanDigest, req.RunID, req.RunRevision, req.RunDigest, req.StageID, req.StageRevision, req.StageDigest, req.StepKey, req.StepRevision, req.StepDigest, req.AttemptID, req.Attempt, req.Fence, fenceDigest, req.TargetID, req.TargetRevision, req.TargetDigest, req.InstanceID, req.Observation.Digest, script.Artifact.Digest, script.Artifact.ID, script.Artifact.MediaType, script.Artifact.Size, executor.Interpreter, executor.Argv, executor.CWD, executor.Env, script.OutputPolicy, script.OutputLimit, script.Redaction, script.SecretRefs, script.Step.TimeoutSeconds, script.Step.IdempotencyMarker, req.CredentialID, req.CredentialRevision, req.Target.AccountID, req.Credential.UserARN})
}

func (t *SSMTransport) Poll(ctx context.Context, req PollRequest) (CommandResult, error) {
	if t == nil || t.factory == nil {
		return CommandResult{}, ErrTypedInvalid
	}
	receipt, err := t.resolveReceipt(ctx, req)
	if err != nil || (receipt.Status != DispatchAccepted && receipt.Status != DispatchCancellationRequested && receipt.Status != DispatchUncertain) || !validCommandID(receipt.CommandID) {
		return CommandResult{Status: PollUncertain}, ErrTypedUncertain
	}
	frozen := receipt.Frozen
	instanceID, handle, err := validateFrozenBinding(frozen)
	if err != nil {
		return CommandResult{Status: PollUncertain}, ErrTypedInvalid
	}
	script, scriptErr := normalizeFrozenScript(frozen)
	if scriptErr != nil {
		return CommandResult{Status: PollUncertain}, ErrTypedInvalid
	}
	client, err := t.factory.NewSSM(handle)
	if err != nil {
		return CommandResult{}, ErrTypedProvider
	}
	callCtx, cancel := t.context(ctx)
	defer cancel()
	out, err := client.GetCommandInvocation(callCtx, &ssm.GetCommandInvocationInput{CommandId: awsapi.String(receipt.CommandID), InstanceId: awsapi.String(instanceID), PluginName: awsapi.String(SSMPluginName)})
	if err != nil {
		// Run Command is eventually consistent. AWS can acknowledge the exact
		// SendCommand request before the invocation becomes visible to this
		// read-only API. Keep polling the already-persisted Command ID; never
		// turn this visibility lag into a second dispatch.
		var notVisible *ssmtypes.InvocationDoesNotExist
		if errors.As(err, &notVisible) {
			return CommandResult{Status: PollPending, CommandID: receipt.CommandID, InstanceID: instanceID}, nil
		}
		return CommandResult{Status: PollUncertain, CommandID: receipt.CommandID, InstanceID: instanceID}, ErrTypedUncertain
	}
	if out == nil {
		return CommandResult{Status: PollUncertain, CommandID: receipt.CommandID, InstanceID: instanceID}, ErrTypedUncertain
	}
	status := mapInvocationStatus(out.Status)
	result := CommandResult{Status: status, CommandID: receipt.CommandID, InstanceID: instanceID, ProviderOperation: "ssm.get_command_invocation"}
	if status == PollSucceeded || status == PollFailed || status == PollCanceled {
		result.ExitCode = awsapi.Int32(out.ResponseCode)
		limit := int(script.OutputLimit)
		if limit <= 0 || limit > MaxSSMOutputBytes {
			limit = MaxSSMOutputBytes
		}
		secretValues := []string(nil)
		// Container secret delivery never opens plaintext in the command
		// transport. Its generated executor discards provider output and reads
		// only the persisted Parameter Store handle on the target. The legacy
		// script-run redaction path remains separate and resolves values only
		// when captured output actually needs redaction.
		if script.OutputPolicy == execution.OutputCapture && len(script.SecretRefs) > 0 {
			if t.secrets == nil {
				return CommandResult{Status: PollUncertain, CommandID: receipt.CommandID, InstanceID: instanceID}, ErrTypedUncertain
			}
			var secretErr error
			secretValues, secretErr = resolveSecretValues(t.secrets, callCtx, frozen.OwnerID, script.SecretRefs)
			if secretErr != nil {
				return CommandResult{Status: PollUncertain, CommandID: receipt.CommandID, InstanceID: instanceID}, ErrTypedUncertain
			}
		}
		if script.OutputPolicy == execution.OutputCapture {
			result.Stdout, result.StdoutTruncated = redactBoundedWithPolicy(awsapi.ToString(out.StandardOutputContent), limit, script.Redaction, secretValues)
			result.Stderr, result.StderrTruncated = redactBoundedWithPolicy(awsapi.ToString(out.StandardErrorContent), limit, script.Redaction, secretValues)
			result.Stdout, result.Stderr, result.StdoutTruncated, result.StderrTruncated = capAggregateOutput(result.Stdout, result.Stderr, result.StdoutTruncated, result.StderrTruncated, limit)
			result.OutputDigest = digestOutput(result.Stdout, result.Stderr)
		} else {
			// discard and artifact policies never persist inline provider output.
			result.OutputDigest = digestOutput("", "")
		}
		if status == PollSucceeded && !succeededScriptOutcome(script, int(out.ResponseCode)) {
			// A successful SSM invocation is not sufficient proof of the plan's
			// declared command result. Unsupported postconditions fail closed.
			result.Status = PollFailed
		}
		if script.Step.Kind == execution.StepContainerApply && len(script.SecretRefs) > 0 {
			lease, _, leaseErr := t.resolveSecretParameterLease(callCtx, frozen, script.SecretRefs)
			if leaseErr != nil || t.secretRevoker == nil || t.secretRevoker.RevokeAuthorizedSecretParameter(callCtx, lease) != nil {
				return CommandResult{Status: PollUncertain, CommandID: receipt.CommandID, InstanceID: instanceID}, ErrTypedUncertain
			}
		}
	}
	return result, nil
}

func succeededScriptOutcome(script FrozenScript, exitCode int) bool {
	executor, err := executorSpecForStep(script.Step)
	if err != nil {
		return false
	}
	allowed := false
	for _, code := range executor.AllowedExitCodes {
		if code == exitCode {
			allowed = true
			break
		}
	}
	if !allowed || script.Step.Postcondition == nil {
		return false
	}
	post := script.Step.Postcondition
	if post.Type != "exit_code" {
		return false
	}
	expected, err := strconv.Atoi(post.Value)
	return err == nil && expected >= 0 && expected <= 255 && expected == exitCode
}

func (t *SSMTransport) PollCommand(ctx context.Context, req PollRequest) (CommandResult, error) {
	return t.Poll(ctx, req)
}

func (t *SSMTransport) Reconcile(ctx context.Context, req PollRequest) (ReconcileResult, error) {
	if t == nil || t.factory == nil {
		return ReconcileResult{}, ErrTypedInvalid
	}
	r, err := t.Poll(ctx, req)
	return ReconcileResult{CommandResult: r}, err
}

func (t *SSMTransport) ReconcileCommand(ctx context.Context, req PollRequest) (ReconcileResult, error) {
	return t.Reconcile(ctx, req)
}

func (t *SSMTransport) Cancel(ctx context.Context, req PollRequest) (CancelResult, error) {
	if t == nil || t.factory == nil {
		return CancelResult{}, ErrTypedInvalid
	}
	receipt, err := t.resolveReceipt(ctx, req)
	if err != nil || (receipt.Status != DispatchAccepted && receipt.Status != DispatchCancellationRequested) || !validCommandID(receipt.CommandID) {
		return CancelResult{Status: PollUncertain}, ErrTypedUncertain
	}
	instanceID, handle, err := validateFrozenBinding(receipt.Frozen)
	if err != nil {
		return CancelResult{Status: PollUncertain}, ErrTypedInvalid
	}
	client, err := t.factory.NewSSM(handle)
	if err != nil {
		return CancelResult{}, ErrTypedProvider
	}
	callCtx, cancel := t.context(ctx)
	defer cancel()
	_, err = client.CancelCommand(callCtx, &ssm.CancelCommandInput{CommandId: awsapi.String(receipt.CommandID), InstanceIds: []string{instanceID}})
	if err != nil {
		return CancelResult{Status: PollUncertain, CommandID: receipt.CommandID}, ErrTypedUncertain
	}
	return CancelResult{Status: PollCancellationRequested, CommandID: receipt.CommandID, ProviderOperation: "ssm.cancel_command"}, nil
}

func (t *SSMTransport) resolveReceipt(ctx context.Context, req PollRequest) (DispatchReceipt, error) {
	if t.receipts == nil {
		return DispatchReceipt{}, ErrTypedInvalid
	}
	if !validOwner(req.OwnerID) {
		return DispatchReceipt{}, ErrTypedInvalid
	}
	key := req.FenceDigest
	if key == "" {
		key = req.Frozen.FenceDigest
	}
	if !key.Valid() {
		return DispatchReceipt{}, ErrTypedInvalid
	}
	receipt, err := t.receipts.ResolveDispatchReceipt(ctx, req.OwnerID, key)
	if err != nil || receipt.FenceDigest != key || receipt.RequestDigest == "" || receipt.Frozen.RequestDigest != receipt.RequestDigest || receipt.Frozen.FenceDigest != receipt.FenceDigest || receipt.Frozen.InstanceID != receipt.InstanceID {
		return DispatchReceipt{}, ErrTypedUncertain
	}
	receiptScript, scriptErr := normalizeFrozenScript(receipt.Frozen)
	receiptFence, fenceErr := computedFenceDigest(receipt.Frozen)
	receiptRequest, requestErr := frozenRequestDigest(receipt.Frozen, receiptScript, receiptFence)
	if scriptErr != nil || fenceErr != nil || requestErr != nil || receiptFence != receipt.FenceDigest || receiptRequest != receipt.RequestDigest {
		return DispatchReceipt{}, ErrTypedUncertain
	}
	if req.Frozen.TargetID != "" && req.Frozen.RequestDigest == "" {
		return DispatchReceipt{}, ErrTypedInvalid
	}
	if req.Frozen.RequestDigest != "" {
		script, scriptErr := normalizeFrozenScript(req.Frozen)
		fence, fenceErr := computedFenceDigest(req.Frozen)
		request, requestErr := frozenRequestDigest(req.Frozen, script, fence)
		if scriptErr != nil || fenceErr != nil || requestErr != nil || fence != receipt.FenceDigest || request != receipt.RequestDigest {
			return DispatchReceipt{}, ErrTypedInvalid
		}
	}
	return receipt, nil
}

func (t *SSMTransport) CancelCommand(ctx context.Context, req PollRequest) (CancelResult, error) {
	return t.Cancel(ctx, req)
}

func validateInspect(req InspectRequest) (execution.ExecutionTarget, string, CredentialHandle, string, error) {
	if !validOwner(req.OwnerID) || req.TargetID == "" || req.TargetRevision == 0 || req.TargetDigest == "" || req.ProfileID == "" || req.AccountID == "" || req.Region == "" || req.CredentialID == "" || req.CredentialRevision == 0 {
		return execution.ExecutionTarget{}, "", CredentialHandle{}, "", ErrTypedInvalid
	}
	target, err := NormalizeInfrastructureTarget(req.Target)
	if err != nil || target.ID != req.TargetID || target.Revision != req.TargetRevision || target.Digest != req.TargetDigest || target.InfrastructureProfileID != req.ProfileID || target.AccountID != req.AccountID || target.Region != req.Region {
		return execution.ExecutionTarget{}, "", CredentialHandle{}, "", ErrTypedInvalid
	}
	if err := validateCredentialBinding(req.Credential, req.CredentialID, req.CredentialRevision, req.AccountID, req.Region); err != nil {
		return execution.ExecutionTarget{}, "", CredentialHandle{}, "", ErrTypedInvalid
	}
	instanceID := req.InstanceID
	if instanceID == "" {
		instanceID = req.TargetFacts["instance_id"]
	}
	if !ec2InstanceIDPattern.MatchString(instanceID) {
		return execution.ExecutionTarget{}, "", CredentialHandle{}, "", ErrTypedInvalid
	}
	partition := req.Partition
	if partition == "" {
		partition = "aws"
	}
	if !validPartition(partition) {
		return execution.ExecutionTarget{}, "", CredentialHandle{}, "", ErrTypedInvalid
	}
	return target, instanceID, req.Credential.handle(), partition, nil
}

func validateFrozenBinding(req FrozenRequest) (string, CredentialHandle, error) {
	if !validOwner(req.OwnerID) || !execution.ValidateUUID(req.PlanID) || req.PlanRevision == 0 || !req.PlanDigest.Valid() || !execution.ValidateUUID(req.RunID) || req.RunRevision == 0 || !req.RunDigest.Valid() || !execution.ValidateUUID(req.StageID) || req.StageRevision == 0 || !req.StageDigest.Valid() || req.StepKey == "" || req.StepRevision == 0 || !req.StepDigest.Valid() || !execution.ValidateUUID(req.AttemptID) || req.Attempt == 0 || req.Fence == "" || !req.FenceDigest.Valid() {
		return "", CredentialHandle{}, ErrTypedInvalid
	}
	target, err := NormalizeInfrastructureTarget(req.Target)
	if err != nil || target.ID != req.TargetID || target.Revision != req.TargetRevision || target.Digest != req.TargetDigest {
		return "", CredentialHandle{}, ErrTypedInvalid
	}
	if err := validateCredentialBinding(req.Credential, req.CredentialID, req.CredentialRevision, target.AccountID, target.Region); err != nil {
		return "", CredentialHandle{}, ErrTypedInvalid
	}
	if len(target.CredentialRefs) > 0 {
		matched := false
		for _, ref := range target.CredentialRefs {
			if ref.Ref != req.CredentialID || ref.Revision != req.CredentialRevision {
				continue
			}
			bound, e := CredentialBindingDigest(req.OwnerID, ref, req.Credential)
			if e != nil || ref.BindingDigest != bound {
				return "", CredentialHandle{}, ErrTypedInvalid
			}
			matched = true
		}
		if !matched {
			return "", CredentialHandle{}, ErrTypedInvalid
		}
	}
	if !ec2InstanceIDPattern.MatchString(req.InstanceID) {
		return "", CredentialHandle{}, ErrTypedInvalid
	}
	return req.InstanceID, req.Credential.handle(), nil
}

func validateCredentialBinding(c Credentials, id string, revision uint64, account, region string) error {
	if c.Validate() != nil || c.ID != id || c.Revision < 1 || c.VerifiedRevision != c.Revision || uint64(c.Revision) != revision || c.Region != region || c.AccountID != account || !accountIDValid(c.AccountID) {
		return ErrTypedInvalid
	}
	parsed, err := arn.Parse(c.UserARN)
	if err != nil || (parsed.Service != "iam" && parsed.Service != "sts") || parsed.AccountID != account || parsed.Resource == "" || parsed.String() != c.UserARN {
		return ErrTypedInvalid
	}
	return nil
}

func validCommandID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && !strings.ContainsAny(id, "\r\n\x00")
}
func validOwner(owner string) bool {
	owner = strings.TrimSpace(owner)
	return len(owner) > 0 && len(owner) <= 255 && strings.HasPrefix(owner, "@") && strings.Contains(owner, ":") && !strings.ContainsAny(owner, "\r\n\x00")
}
func validPartition(p string) bool { return p == "aws" || p == "aws-us-gov" || p == "aws-cn" }
func mapInvocationStatus(s ssmtypes.CommandInvocationStatus) PollStatus {
	switch s {
	case ssmtypes.CommandInvocationStatusPending:
		return PollPending
	case ssmtypes.CommandInvocationStatusInProgress:
		return PollRunning
	case ssmtypes.CommandInvocationStatusSuccess:
		return PollSucceeded
	case ssmtypes.CommandInvocationStatusCancelled:
		return PollCanceled
	case ssmtypes.CommandInvocationStatusCancelling:
		return PollCancellationRequested
	case ssmtypes.CommandInvocationStatusDelayed:
		return PollPending
	case ssmtypes.CommandInvocationStatusFailed, ssmtypes.CommandInvocationStatusTimedOut:
		return PollFailed
	default:
		return PollUncertain
	}
}
func redactBounded(s string, max int) (string, bool) {
	return redactBoundedWithPolicy(s, max, execution.RedactionPolicy{}, nil)
}

func capAggregateOutput(stdout, stderr string, stdoutTruncated, stderrTruncated bool, max int) (string, string, bool, bool) {
	if max < 1 {
		return "", "", true, true
	}
	if len(stdout)+len(stderr) <= max {
		return stdout, stderr, stdoutTruncated, stderrTruncated
	}
	if len(stderr) > max-len(stdout) {
		remaining := max - len(stdout)
		if remaining < 0 {
			remaining = 0
		}
		stderr = stderr[:remaining]
		stderrTruncated = true
	}
	if len(stdout)+len(stderr) > max {
		stdout = stdout[:max]
		stderr = ""
		stdoutTruncated = true
		stderrTruncated = true
	}
	return stdout, stderr, stdoutTruncated, stderrTruncated
}

func redactBoundedWithPolicy(s string, max int, policy execution.RedactionPolicy, secretValues []string) (string, bool) {
	const replacement = "[REDACTED]"
	for _, value := range secretValues {
		if value != "" {
			s = strings.ReplaceAll(s, value, replacement)
		}
	}
	for _, pattern := range policy.Patterns {
		if pattern == "" {
			continue
		}
		if re, err := regexp.Compile(pattern); err == nil {
			s = re.ReplaceAllString(s, policy.Replace)
		}
	}
	for _, token := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "SECRET_ACCESS_KEY", "PASSWORD", "TOKEN", "AUTHORIZATION"} {
		s = regexp.MustCompile(`(?im)(`+token+`\s*[=:]\s*)[^\r\n]+`).ReplaceAllString(s, `$1`+replacement)
	}
	b := []byte(s)
	truncated := len(b) > max
	if truncated {
		b = b[:max]
	}
	return string(b), truncated
}
func digestOutput(stdout, stderr string) execution.Digest {
	sum := sha256.Sum256([]byte(stdout + "\x00" + stderr))
	return execution.Digest(hex.EncodeToString(sum[:]))
}
