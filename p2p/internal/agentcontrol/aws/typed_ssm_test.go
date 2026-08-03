package aws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type typedFake struct {
	stsCalls, ec2Calls, instanceTypeCalls, volumeCalls, securityGroupCalls, ssmInfoCalls, sendCalls, getCalls, cancelCalls int
	stsErr, sendErr, getErr, cancelErr                                                                                     error
	status                                                                                                                 ssmtypes.CommandInvocationStatus
	stdout, stderr                                                                                                         string
	lastSend                                                                                                               *ssm.SendCommandInput
	receipt                                                                                                                *DispatchReceipt
	denyHTTPSEgress                                                                                                        bool
	capacityUnavailable                                                                                                    bool
}

type artifactFake struct{ data []byte }

func (a artifactFake) ResolveArtifact(context.Context, string, execution.ArtifactRef) ([]byte, error) {
	return append([]byte(nil), a.data...), nil
}

type secretLeaseRuntimeFake struct {
	lease   SecretParameterLease
	auth    SecretAccessAuthorization
	err     error
	resolve int
	revoke  int
}

func (f *secretLeaseRuntimeFake) ResolveAuthorizedSecretParameterLease(_ context.Context, _ AuthorizedSecretRequest) (SecretParameterLease, SecretAccessAuthorization, error) {
	f.resolve++
	return f.lease, f.auth, f.err
}
func (f *secretLeaseRuntimeFake) RevokeAuthorizedSecretParameter(_ context.Context, lease SecretParameterLease) error {
	if f.err != nil || lease.ParameterName != f.lease.ParameterName {
		return ErrSecretParameterUncertain
	}
	f.revoke++
	return nil
}

func (f *typedFake) NewSTS(CredentialHandle) (STSClient, error)               { return f, nil }
func (f *typedFake) NewEC2(CredentialHandle) (EC2Client, error)               { return f, nil }
func (f *typedFake) NewSSM(CredentialHandle) (SSMClient, error)               { return f, nil }
func (f *typedFake) NewSSMCommand(CredentialHandle) (SSMCommandClient, error) { return f, nil }
func (f *typedFake) ResolveDispatchReceipt(context.Context, string, execution.Digest) (DispatchReceipt, error) {
	if f.receipt == nil {
		return DispatchReceipt{}, errors.New("missing receipt")
	}
	return *f.receipt, nil
}
func (f *typedFake) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	f.stsCalls++
	if f.stsErr != nil {
		return nil, f.stsErr
	}
	return &sts.GetCallerIdentityOutput{Account: awsapi.String("123456789012"), Arn: awsapi.String("arn:aws:iam::123456789012:role/test"), UserId: awsapi.String("AID")}, nil
}
func (f *typedFake) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.ec2Calls++
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{InstanceId: awsapi.String("i-0123456789abcdef0"), InstanceType: ec2types.InstanceTypeT3Small, Architecture: ec2types.ArchitectureValuesX8664, Platform: "", State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}, Placement: &ec2types.Placement{AvailabilityZone: awsapi.String("us-east-1a")}, VpcId: awsapi.String("vpc-0123456789abcdef0"), RootDeviceName: awsapi.String("/dev/xvda"), BlockDeviceMappings: []ec2types.InstanceBlockDeviceMapping{{DeviceName: awsapi.String("/dev/xvda"), Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: awsapi.String("vol-0123456789abcdef0")}}}, SecurityGroups: []ec2types.GroupIdentifier{{GroupId: awsapi.String("sg-0123456789abcdef0")}}}}}}}, nil
}
func (f *typedFake) DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	f.instanceTypeCalls++
	if f.capacityUnavailable {
		return &ec2.DescribeInstanceTypesOutput{}, nil
	}
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: []ec2types.InstanceTypeInfo{{InstanceType: ec2types.InstanceTypeT3Small, VCpuInfo: &ec2types.VCpuInfo{DefaultVCpus: awsapi.Int32(2)}, MemoryInfo: &ec2types.MemoryInfo{SizeInMiB: awsapi.Int64(2048)}}}}, nil
}
func (f *typedFake) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	f.volumeCalls++
	return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{{VolumeId: awsapi.String("vol-0123456789abcdef0"), Size: awsapi.Int32(20)}}}, nil
}
func (f *typedFake) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.securityGroupCalls++
	if f.denyHTTPSEgress {
		return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{{GroupId: awsapi.String("sg-0123456789abcdef0")}}}, nil
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{{OwnerId: awsapi.String("123456789012"), VpcId: awsapi.String("vpc-0123456789abcdef0"), GroupId: awsapi.String("sg-0123456789abcdef0"), IpPermissionsEgress: []ec2types.IpPermission{{IpProtocol: awsapi.String("tcp"), FromPort: awsapi.Int32(443), ToPort: awsapi.Int32(443), IpRanges: []ec2types.IpRange{{CidrIp: awsapi.String("0.0.0.0/0")}}}}}}}, nil
}
func (f *typedFake) DescribeInstanceInformation(context.Context, *ssm.DescribeInstanceInformationInput, ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error) {
	f.ssmInfoCalls++
	return &ssm.DescribeInstanceInformationOutput{InstanceInformationList: []ssmtypes.InstanceInformation{{InstanceId: awsapi.String("i-0123456789abcdef0"), PingStatus: ssmtypes.PingStatusOnline, PlatformType: ssmtypes.PlatformTypeLinux, PlatformName: awsapi.String("Amazon Linux"), PlatformVersion: awsapi.String("2023")}}}, nil
}
func (f *typedFake) SendCommand(_ context.Context, input *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.sendCalls++
	f.lastSend = input
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: awsapi.String("cmd-123")}}, nil
}
func (f *typedFake) GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &ssm.GetCommandInvocationOutput{CommandId: awsapi.String("cmd-123"), InstanceId: awsapi.String("i-0123456789abcdef0"), Status: f.status, ResponseCode: 0, StandardOutputContent: awsapi.String(f.stdout), StandardErrorContent: awsapi.String(f.stderr)}, nil
}
func (f *typedFake) CancelCommand(context.Context, *ssm.CancelCommandInput, ...func(*ssm.Options)) (*ssm.CancelCommandOutput, error) {
	f.cancelCalls++
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return &ssm.CancelCommandOutput{}, nil
}

func typedFixture(t *testing.T) (*typedFake, *SSMTransport, InspectRequest, FrozenRequest) {
	t.Helper()
	f := &typedFake{status: ssmtypes.CommandInvocationStatusSuccess, stdout: "ok"}
	credID := "11111111-1111-4111-8111-111111111111"
	cred := RehydrateCredentials(credID, "test", "us-east-1", "123456789012", "arn:aws:iam::123456789012:role/test", []byte("AK"), []byte("SECRET"), nil, 1, 1, timeNow(), timeNow())
	target, err := NormalizeInfrastructureTarget(execution.ExecutionTarget{ID: "22222222-2222-4222-8222-222222222222", Provider: "aws", Kind: "aws_ec2_instance", InfrastructureProfileID: InfrastructureProfileGeneralLinuxSSMV1, AccountID: "123456789012", Region: "us-east-1", Architecture: "x86_64", Capabilities: []string{"target.aws_ec2_instance", "transport.aws_ssm"}, Network: execution.NetworkPolicy{Mode: execution.NetworkPolicyModeObservedHTTPSEgress}, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	ireq := InspectRequest{OwnerID: "@owner:example.org", Target: target, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest, ProfileID: target.InfrastructureProfileID, AccountID: target.AccountID, Region: target.Region, Credential: cred, CredentialID: cred.ID, CredentialRevision: 1, InstanceID: "i-0123456789abcdef0"}
	sha := execution.Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	data := []byte("echo hello")
	sum := sha256.Sum256(data)
	artifactDigest := execution.Digest(hex.EncodeToString(sum[:]))
	artifact := execution.ArtifactRef{ID: "77777777-7777-4777-8777-777777777777", Digest: artifactDigest, Immutable: true, Size: int64(len(data))}
	post := &execution.Postcondition{Type: "exit_code", Value: "0"}
	obsRef := &execution.TargetObservationRef{ObservationID: "88888888-8888-4888-8888-888888888888", TargetID: target.ID, TargetRevision: target.Revision, ObservationDigest: execution.Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")}
	step := execution.ExecutionStep{StepKey: "run", Kind: execution.StepScriptRun, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest, ObservationRef: obsRef, TimeoutSeconds: 30, IdempotencyMarker: "idem", OutputPolicy: execution.OutputCapture, Postcondition: post, ScriptRun: &execution.ScriptRunStep{Artifact: artifact, Interpreter: "/bin/sh", Argv: []string{"-e"}, CWD: "/tmp", Root: true, AllowedExitCodes: []int{0}, TimeoutSeconds: 30, OutputLimit: 1024, Redaction: execution.RedactionPolicy{Patterns: []string{"secret"}, Replace: "[REDACTED]"}, Postcondition: post, IdempotencyMarker: "idem"}}
	stepDigest, err := execution.CanonicalDigest(step)
	if err != nil {
		t.Fatal(err)
	}
	step.Digest = stepDigest
	securityGroupDigest, ok := exactManagementSecurityGroupDigest(ec2types.SecurityGroup{OwnerId: awsapi.String("123456789012"), VpcId: awsapi.String("vpc-0123456789abcdef0"), GroupId: awsapi.String("sg-0123456789abcdef0"), IpPermissionsEgress: []ec2types.IpPermission{{IpProtocol: awsapi.String("tcp"), FromPort: awsapi.Int32(443), ToPort: awsapi.Int32(443), IpRanges: []ec2types.IpRange{{CidrIp: awsapi.String("0.0.0.0/0")}}}}}, target.AccountID, "vpc-0123456789abcdef0", "sg-0123456789abcdef0")
	if !ok {
		t.Fatal("exact management security group digest unavailable")
	}
	obs := execution.TargetObservation{TargetID: target.ID, TargetRevision: target.Revision, ObservedAt: timeNow(), State: "ready", Facts: map[string]string{"instance_id": ireqInstance(ireq), "instance_type": "t3.small", execution.ObservationFactAvailabilityZone: "us-east-1a", "account_id": target.AccountID, "region": target.Region, "partition": "aws", "operating_system": "linux", "architecture": target.Architecture, "ssm_status": "Online", "platform_name": "Amazon Linux", "platform_version": "2023", execution.ObservationFactVCPUCount: "2", execution.ObservationFactMemoryMiB: "2048", execution.ObservationFactRootVolumeGiB: "20", execution.ObservationFactHTTPSEgress: execution.ObservationFactHTTPSEgressValue, execution.ObservationFactSecurityGroupDigest: string(securityGroupDigest)}}
	obs, err = obs.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	step.ObservationRef.ObservationDigest = obs.Digest
	step.Digest = ""
	stepDigest, err = execution.CanonicalDigest(step)
	if err != nil {
		t.Fatal(err)
	}
	step.Digest = stepDigest
	freq := FrozenRequest{OwnerID: "@owner:example.org", PlanID: "33333333-3333-4333-8333-333333333333", PlanRevision: 1, PlanDigest: sha, RunID: "44444444-4444-4444-8444-444444444444", RunRevision: 1, RunDigest: sha, StageID: "55555555-5555-4555-8555-555555555555", StageRevision: 1, StageDigest: sha, StepKey: "run", StepRevision: 1, StepDigest: stepDigest, AttemptID: "66666666-6666-4666-8666-666666666666", Attempt: 1, Fence: "fence", FenceDigest: sha, Target: target, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest, InstanceID: ireqInstance(ireq), Credential: cred, CredentialID: cred.ID, CredentialRevision: 1, Observation: obs, Script: FrozenScript{Step: step, Artifact: artifact, OutputPolicy: execution.OutputCapture, OutputLimit: 1024, Redaction: step.ScriptRun.Redaction, SecretRefs: nil}}
	freq.FenceDigest, err = computedFenceDigest(freq)
	if err != nil {
		t.Fatal(err)
	}
	freq.RequestDigest, err = frozenRequestDigest(freq, freq.Script, freq.FenceDigest)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewSSMTransport(f, WithImmutableArtifactResolver(artifactFake{data: data}), WithDispatchReceiptResolver(f))
	if err != nil {
		t.Fatal(err)
	}
	return f, transport, ireq, freq
}
func ireqInstance(r InspectRequest) string { return r.InstanceID }
func timeNow() time.Time                   { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }

func secretContainerFixture(t *testing.T) (*typedFake, *SSMTransport, FrozenRequest, *secretLeaseRuntimeFake, string) {
	t.Helper()
	f, _, _, frozen := typedFixture(t)
	data := []byte("#!/bin/sh\nset -eu\ntest \"$#\" -eq 1\n")
	sum := sha256.Sum256(data)
	artifact := execution.ArtifactRef{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Digest: execution.Digest(hex.EncodeToString(sum[:])), MediaType: "application/x-sh", Size: int64(len(data)), Immutable: true}
	secretRef := execution.CredentialRef{Ref: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Purpose: ExecutionSecretPurposeAIProviderAPIKey, Revision: 2, BindingDigest: execution.Digest(strings.Repeat("b", 64))}
	grant, err := (execution.NetworkGrant{Scheme: "https", Host: execution.PublicHTTPSWildcardHost, Port: 443, Scope: "external"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	post := &execution.Postcondition{Type: "exit_code", Value: "0"}
	step := execution.ExecutionStep{StepKey: "apply-container", Kind: execution.StepContainerApply, TargetID: frozen.TargetID, TargetRevision: frozen.TargetRevision, TargetDigest: frozen.TargetDigest, ObservationRef: frozen.Script.Step.ObservationRef, TimeoutSeconds: 120, IdempotencyMarker: "apply-container", NetworkGrants: []execution.NetworkGrant{grant}, SecretRefs: []execution.CredentialRef{secretRef}, OutputPolicy: execution.OutputDiscard, Postcondition: post, ContainerApply: &execution.ContainerApplyStep{Image: "registry.example/service@sha256:" + strings.Repeat("c", 64), Name: "dirextalk-service", HostAddress: "127.0.0.1", HostPort: 8080, ContainerPort: 8080, RestartPolicy: "unless-stopped"}, Executor: &execution.ExecutorSpec{Artifact: artifact, Interpreter: "/bin/sh", CWD: "/", Root: true, AllowedExitCodes: []int{0}, OutputLimit: 4096, Postcondition: post}}
	snapshot, err := execution.StepSnapshotFromStep(step, execution.StepSetForward)
	if err != nil {
		t.Fatal(err)
	}
	frozen.StepKey = snapshot.Step.StepKey
	frozen.StepDigest = snapshot.Step.Digest
	frozen.Script, err = FrozenScriptForStep(snapshot.Step)
	if err != nil {
		t.Fatal(err)
	}
	frozen.FenceDigest, err = computedFenceDigest(frozen)
	if err != nil {
		t.Fatal(err)
	}
	frozen.RequestDigest, err = frozenRequestDigest(frozen, frozen.Script, frozen.FenceDigest)
	if err != nil {
		t.Fatal(err)
	}
	authDigest, err := execution.CanonicalDigest([]execution.CredentialRef{secretRef})
	if err != nil {
		t.Fatal(err)
	}
	provisionStage := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	provisionAttempt := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	parameterName, err := SecretParameterName(frozen.TargetID, provisionAttempt, secretRef)
	if err != nil {
		t.Fatal(err)
	}
	auth := SecretAccessAuthorization{RunID: frozen.RunID, StageID: provisionStage, ConfirmationID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", Gate: execution.GateSecretAccess, StageStatus: execution.StageSucceeded, SecretGrantDigest: authDigest}
	runtime := &secretLeaseRuntimeFake{auth: auth, lease: SecretParameterLease{SchemaVersion: secretParameterSchema, OwnerID: frozen.OwnerID, RunID: frozen.RunID, ProvisionStageID: provisionStage, ProvisionAttemptID: provisionAttempt, Authorization: auth, TargetID: frozen.TargetID, TargetRevision: frozen.TargetRevision, TargetDigest: frozen.TargetDigest, SecretRef: secretRef, ParameterName: parameterName, ContainerMountPath: "/run/secrets/dirextalk/" + secretRef.Purpose, FenceDigest: execution.Digest(strings.Repeat("d", 64)), RequestDigest: execution.Digest(strings.Repeat("e", 64)), ProviderVersion: 1}}
	transport, err := NewSSMTransport(f, WithImmutableArtifactResolver(artifactFake{data: data}), WithDispatchReceiptResolver(f), WithSecretParameterRuntime(runtime, runtime))
	if err != nil {
		t.Fatal(err)
	}
	return f, transport, frozen, runtime, parameterName
}

func TestTypedSSMInspectPinsAndDispatchesFixedCommand(t *testing.T) {
	f, tr, ir, fr := typedFixture(t)
	obs, err := tr.Inspect(context.Background(), ir)
	if err != nil || obs.State != "ready" || obs.Facts[execution.ObservationFactVCPUCount] != "2" || obs.Facts[execution.ObservationFactMemoryMiB] != "2048" || obs.Facts[execution.ObservationFactRootVolumeGiB] != "20" || f.stsCalls != 1 || f.ec2Calls != 1 || f.instanceTypeCalls != 1 || f.volumeCalls != 1 || f.ssmInfoCalls != 1 {
		t.Fatalf("obs=%#v err=%v calls=%d/%d/%d/%d/%d", obs, err, f.stsCalls, f.ec2Calls, f.instanceTypeCalls, f.volumeCalls, f.ssmInfoCalls)
	}
	dispatched, err := tr.Dispatch(context.Background(), fr)
	if err != nil || dispatched.CommandID != "cmd-123" || dispatched.DocumentName != SSMDocumentName || f.sendCalls != 1 {
		t.Fatalf("dispatch=%#v err=%v calls=%d", dispatched, err, f.sendCalls)
	}
	fr.RequestDigest = dispatched.RequestDigest
	f.receipt = &DispatchReceipt{Frozen: fr, RequestDigest: dispatched.RequestDigest, FenceDigest: fr.FenceDigest, CommandID: dispatched.CommandID, InstanceID: fr.InstanceID, Status: DispatchAccepted}
}

func TestTypedSSMContainerSecretDispatchUsesOnlyAuthorizedParameterHandleAndRevokes(t *testing.T) {
	f, transport, frozen, runtime, parameterName := secretContainerFixture(t)
	if !transport.CanExecuteStep(frozen.Script.Step) {
		t.Fatal("secret-bearing container was not executable with the authorized lease runtime")
	}
	dispatched, err := transport.Dispatch(context.Background(), frozen)
	if err != nil || dispatched.CommandID != "cmd-123" || f.sendCalls != 1 || runtime.resolve != 1 {
		t.Fatalf("dispatch=%+v err=%v calls=%d resolve=%d", dispatched, err, f.sendCalls, runtime.resolve)
	}
	command := strings.Join(f.lastSend.Parameters["commands"], "\n")
	if !strings.Contains(command, parameterName) || strings.Contains(command, "provider-api-key-value") || strings.Contains(command, "Authorization: Bearer") {
		t.Fatalf("command did not carry only the secret handle: %s", command)
	}
	f.receipt = &DispatchReceipt{Frozen: frozen, RequestDigest: frozen.RequestDigest, FenceDigest: frozen.FenceDigest, CommandID: dispatched.CommandID, InstanceID: frozen.InstanceID, Status: DispatchAccepted}
	result, err := transport.Poll(context.Background(), PollRequest{OwnerID: frozen.OwnerID, Frozen: frozen, CommandID: dispatched.CommandID, FenceDigest: frozen.FenceDigest, Known: true})
	if err != nil || result.Status != PollSucceeded || runtime.resolve != 2 || runtime.revoke != 1 {
		t.Fatalf("poll=%+v err=%v resolve=%d revoke=%d", result, err, runtime.resolve, runtime.revoke)
	}
}

func TestTypedSSMContainerSecretFailsClosedWithoutIndependentSucceededGate(t *testing.T) {
	f, transport, frozen, runtime, _ := secretContainerFixture(t)
	runtime.auth.Gate = execution.GateExternalAuth
	if _, err := transport.Dispatch(context.Background(), frozen); !errors.Is(err, ErrTypedInvalid) || f.sendCalls != 0 {
		t.Fatalf("wrong gate err=%v send=%d", err, f.sendCalls)
	}
	_, noRuntime, _, plain := typedFixture(t)
	plain.Script = frozen.Script
	plain.StepKey = frozen.StepKey
	plain.StepDigest = frozen.StepDigest
	plain.FenceDigest, _ = computedFenceDigest(plain)
	plain.RequestDigest, _ = frozenRequestDigest(plain, plain.Script, plain.FenceDigest)
	if noRuntime.CanExecuteStep(plain.Script.Step) {
		t.Fatal("secret-bearing container was executable without lease/revocation runtime")
	}
}

func TestTypedSSMInspectRequiresObservedPublicHTTPSEgressForEnvelope(t *testing.T) {
	f, tr, ir, _ := typedFixture(t)
	f.denyHTTPSEgress = true
	obs, err := tr.Inspect(context.Background(), ir)
	if !errors.Is(err, ErrTypedUnavailable) || obs.State != "unavailable" || len(obs.Warnings) != 1 || obs.Warnings[0] != "https_egress_not_observed" || f.securityGroupCalls != 1 || f.ssmInfoCalls != 0 {
		t.Fatalf("obs=%+v err=%v calls=%d/%d", obs, err, f.securityGroupCalls, f.ssmInfoCalls)
	}
}

func TestTypedSSMInspectAndDispatchFailClosedOnCapacityProofDrift(t *testing.T) {
	f, tr, ir, frozen := typedFixture(t)
	f.capacityUnavailable = true
	observation, err := tr.Inspect(context.Background(), ir)
	if !errors.Is(err, ErrTypedUnavailable) || observation.State != "unavailable" || len(observation.Warnings) != 1 || observation.Warnings[0] != "instance_capacity_unavailable" || f.securityGroupCalls != 0 || f.ssmInfoCalls != 0 {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}

	f.capacityUnavailable = false
	live, err := tr.Inspect(context.Background(), ir)
	if err != nil || !liveObservationMatchesFrozen(live, frozen.Observation, frozen.Target) {
		t.Fatalf("exact live observation rejected: observation=%+v err=%v", live, err)
	}
	frozen.Observation.Facts[execution.ObservationFactMemoryMiB] = "4096"
	if liveObservationMatchesFrozen(live, frozen.Observation, frozen.Target) {
		t.Fatal("capacity drift was accepted before dispatch")
	}
}

func TestTypedSSMValidationAndUncertaintyNeverResends(t *testing.T) {
	f, tr, ir, fr := typedFixture(t)
	ir.AccountID = "999999999999"
	if _, err := tr.Inspect(context.Background(), ir); !errors.Is(err, ErrTypedInvalid) || f.stsCalls != 0 || f.ec2Calls != 0 || f.ssmInfoCalls != 0 {
		t.Fatalf("mismatch err=%v calls=%d/%d/%d", err, f.stsCalls, f.ec2Calls, f.ssmInfoCalls)
	}
	f.sendErr = errors.New("context lost")
	result, err := tr.Dispatch(context.Background(), fr)
	if !errors.Is(err, ErrTypedUncertain) || result.Status != DispatchUncertain || f.sendCalls != 1 {
		t.Fatalf("loss result=%#v err=%v calls=%d", result, err, f.sendCalls)
	}
	result, err = tr.Dispatch(context.Background(), fr)
	if !errors.Is(err, ErrTypedReplay) || result.Status != "" || f.sendCalls != 1 {
		t.Fatalf("explicit second call result=%#v err=%v calls=%d", result, err, f.sendCalls)
	}
	_ = result
}

func TestTypedSSMPollRequiresDeclaredExitAndSupportedPostcondition(t *testing.T) {
	f, tr, _, fr := typedFixture(t)
	// The persisted request is authoritative; an impossible declared postcondition
	// is terminally failed rather than treated as generic SSM success.
	fr.Script.Step.Postcondition = &execution.Postcondition{Type: "file_exists", Value: "/tmp/result"}
	fr.Script.Step.ScriptRun.Postcondition = fr.Script.Step.Postcondition
	fr.Script.Step.Digest = ""
	fr.Script.Step.Digest, _ = execution.CanonicalDigest(fr.Script.Step)
	fr.StepDigest = fr.Script.Step.Digest
	fr.FenceDigest, _ = CanonicalFenceDigest(fr)
	fr.RequestDigest, _ = frozenRequestDigest(fr, fr.Script, fr.FenceDigest)
	f.receipt = &DispatchReceipt{Frozen: fr, RequestDigest: fr.RequestDigest, FenceDigest: fr.FenceDigest, CommandID: "cmd-123", InstanceID: fr.InstanceID, Status: DispatchAccepted}
	result, err := tr.Poll(context.Background(), PollRequest{OwnerID: fr.OwnerID, Frozen: fr, FenceDigest: fr.FenceDigest, Known: true})
	if err != nil || result.Status != PollFailed {
		t.Fatalf("unsupported postcondition result=%#v err=%v", result, err)
	}
}

func TestTypedSSMPollTreatsInvocationVisibilityLagAsPending(t *testing.T) {
	f, tr, _, fr := typedFixture(t)
	f.receipt = &DispatchReceipt{Frozen: fr, RequestDigest: fr.RequestDigest, FenceDigest: fr.FenceDigest, CommandID: "cmd-123", InstanceID: fr.InstanceID, Status: DispatchAccepted}
	f.getErr = &ssmtypes.InvocationDoesNotExist{}

	result, err := tr.Poll(context.Background(), PollRequest{OwnerID: fr.OwnerID, Frozen: fr, CommandID: "cmd-123", FenceDigest: fr.FenceDigest, Known: true})
	if err != nil || result.Status != PollPending || result.CommandID != "cmd-123" || f.getCalls != 1 {
		t.Fatalf("visibility lag result=%#v err=%v calls=%d", result, err, f.getCalls)
	}

	f.getErr = nil
	result, err = tr.Poll(context.Background(), PollRequest{OwnerID: fr.OwnerID, Frozen: fr, CommandID: "cmd-123", FenceDigest: fr.FenceDigest, Known: true})
	if err != nil || result.Status != PollSucceeded || f.getCalls != 2 {
		t.Fatalf("visible invocation result=%#v err=%v calls=%d", result, err, f.getCalls)
	}
}

func TestTypedSSMPollRedactsAndCapsAndUnknownReconcileDoesNotCall(t *testing.T) {
	f, tr, _, fr := typedFixture(t)
	f.stdout = "PASSWORD=secret " + strings.Repeat("x", MaxSSMOutputBytes+100)
	f.stderr = strings.Repeat("e", MaxSSMOutputBytes)
	f.receipt = &DispatchReceipt{Frozen: fr, RequestDigest: fr.RequestDigest, FenceDigest: fr.FenceDigest, CommandID: "cmd-123", InstanceID: fr.InstanceID, Status: DispatchAccepted}
	poll, err := tr.Poll(context.Background(), PollRequest{OwnerID: fr.OwnerID, Frozen: fr, CommandID: "cmd-123", Known: true})
	if err != nil || poll.Status != PollSucceeded || len(poll.Stdout)+len(poll.Stderr) > 1024 || strings.Contains(poll.Stdout, "secret") {
		t.Fatalf("poll=%#v err=%v", poll, err)
	}
	before := f.getCalls
	_, err = tr.Reconcile(context.Background(), PollRequest{OwnerID: fr.OwnerID, Frozen: fr, FenceDigest: execution.Digest("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"), Known: false})
	if !errors.Is(err, ErrTypedUncertain) || f.getCalls != before {
		t.Fatalf("unknown reconcile err=%v calls=%d/%d", err, f.getCalls, before)
	}
	mutated := fr
	mutated.InstanceID = "i-aaaaaaaaaaaaaaaaa"
	_, err = tr.Poll(context.Background(), PollRequest{OwnerID: fr.OwnerID, Frozen: mutated, FenceDigest: fr.FenceDigest})
	if err == nil || f.getCalls != before {
		t.Fatalf("receipt substitution err=%v calls=%d/%d", err, f.getCalls, before)
	}
}

func TestTypedSSMRejectsSnapshotSubstitutionAndInstanceReplay(t *testing.T) {
	f, tr, _, fr := typedFixture(t)
	fr.Script.Step.ScriptRun.Env = map[string]string{"MODE": "unexpected"}
	if _, err := tr.Dispatch(context.Background(), fr); !errors.Is(err, ErrTypedInvalid) || f.sendCalls != 0 {
		t.Fatalf("snapshot substitution err=%v calls=%d", err, f.sendCalls)
	}
	_, _, _, fr = typedFixture(t)
	fr.InstanceID = "i-aaaaaaaaaaaaaaaaa"
	if _, err := tr.Dispatch(context.Background(), fr); !errors.Is(err, ErrTypedInvalid) || f.sendCalls != 0 {
		t.Fatalf("instance substitution err=%v calls=%d", err, f.sendCalls)
	}
	fr.InstanceID = "i-0123456789abcdef0"
	fr.RequestDigest = execution.Digest("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	result, err := tr.Dispatch(context.Background(), fr)
	if !errors.Is(err, ErrTypedInvalid) || result.Status != "" || f.sendCalls != 0 {
		t.Fatalf("digest spoof result=%#v err=%v calls=%d", result, err, f.sendCalls)
	}
	fr.RequestDigest, _ = frozenRequestDigest(fr, fr.Script, fr.FenceDigest)
	result, err = tr.Dispatch(context.Background(), fr)
	if err != nil || result.CommandID == "" || f.sendCalls != 1 {
		t.Fatalf("valid dispatch result=%#v err=%v calls=%d", result, err, f.sendCalls)
	}
	if f.lastSend == nil || awsapi.ToString(f.lastSend.DocumentName) != SSMDocumentName || len(f.lastSend.InstanceIds) != 1 || f.lastSend.InstanceIds[0] != fr.InstanceID {
		t.Fatalf("send input=%#v", f.lastSend)
	}
	if _, err := tr.Dispatch(context.Background(), fr); !errors.Is(err, ErrTypedReplay) || f.sendCalls != 1 {
		t.Fatalf("replay err=%v calls=%d", err, f.sendCalls)
	}
}

func TestTypedSSMCredentialAndCancelStates(t *testing.T) {
	f, tr, _, fr := typedFixture(t)
	fr.Credential.VerifiedRevision = 0
	if _, err := tr.Dispatch(context.Background(), fr); !errors.Is(err, ErrTypedInvalid) || f.sendCalls != 0 {
		t.Fatalf("unverified err=%v calls=%d", err, f.sendCalls)
	}
	f, tr, _, fr = typedFixture(t)
	f.status = ssmtypes.CommandInvocationStatusCancelling
	f.receipt = &DispatchReceipt{Frozen: fr, RequestDigest: fr.RequestDigest, FenceDigest: fr.FenceDigest, CommandID: "cmd-123", InstanceID: fr.InstanceID, Status: DispatchAccepted}
	if _, err := tr.Cancel(context.Background(), PollRequest{OwnerID: fr.OwnerID, Frozen: fr, CommandID: "cmd-123", Known: true}); err != nil {
		t.Fatal(err)
	}
	poll, err := tr.Poll(context.Background(), PollRequest{OwnerID: fr.OwnerID, Frozen: fr, CommandID: "cmd-123", Known: true})
	if err != nil || poll.Status != PollCancellationRequested {
		t.Fatalf("cancel poll=%#v err=%v", poll, err)
	}
}

func TestTypedSSMCommandConfigHasZeroRetries(t *testing.T) {
	cred := RehydrateCredentials("11111111-1111-4111-8111-111111111111", "test", "us-east-1", "123456789012", "arn:aws:iam::123456789012:role/test", []byte("AK"), []byte("SECRET"), nil, 1, 1, timeNow(), timeNow())
	cfg, err := StaticAWSCommandConfig(cred.handle())
	if err != nil || cfg.RetryMaxAttempts != 1 {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
}

func TestTypedSSMArtifactSizeMismatchFailsBeforeSend(t *testing.T) {
	f, tr, _, fr := typedFixture(t)
	tr.artifacts = artifactFake{data: []byte("different bytes")}
	if _, err := tr.Dispatch(context.Background(), fr); !errors.Is(err, ErrTypedArtifact) || f.sendCalls != 0 {
		t.Fatalf("artifact mismatch err=%v calls=%d", err, f.sendCalls)
	}
}

func TestTypedSSMCommandPrefixFailsClosedAndZeroSizeIsAllowed(t *testing.T) {
	f, tr, _, fr := typedFixture(t)
	result, err := tr.Dispatch(context.Background(), fr)
	if err != nil {
		t.Fatal(err)
	}
	if f.lastSend == nil || !strings.Contains(f.lastSend.Parameters["commands"][0], " && umask 077 && ") || strings.Contains(f.lastSend.Parameters["commands"][0], " && umask 077;") {
		t.Fatalf("unsafe command prefix: %q", f.lastSend.Parameters["commands"][0])
	}
	_ = result
	data := []byte("size-zero")
	sum := sha256.Sum256(data)
	if !artifactDigestMatches(execution.ArtifactRef{Digest: execution.Digest(hex.EncodeToString(sum[:])), Immutable: true}, data) {
		t.Fatal("size zero artifact should be accepted")
	}
}

func TestRenderSSMCommandPassesFrozenArgvAfterScript(t *testing.T) {
	step := execution.ExecutionStep{
		Kind: execution.StepScriptRun,
		ScriptRun: &execution.ScriptRunStep{
			Interpreter: "/bin/bash",
			Argv:        []string{"first", "two words"},
			CWD:         "/tmp",
			Root:        true,
		},
	}
	command, err := renderSSMCommand(FrozenScript{Step: step}, []byte("printf '%s\\n' \"$@\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "'/bin/bash' \"$file\" 'first' 'two words'; rc=$?") {
		t.Fatalf("script argv were not ordered after the immutable script: %q", command)
	}
}

func TestRenderSSMCommandDropsNonRootExecutorToEC2User(t *testing.T) {
	step := execution.ExecutionStep{
		Kind: execution.StepScriptRun,
		ScriptRun: &execution.ScriptRunStep{
			Interpreter: "/bin/sh",
			CWD:         "/",
			Root:        false,
		},
	}
	command, err := renderSSMCommand(FrozenScript{Step: step}, []byte("exit 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "id -u ec2-user") || !strings.Contains(command, "runuser -u ec2-user -- '/bin/sh' \"$file\"") {
		t.Fatalf("non-root executor was not privilege-dropped: %q", command)
	}
}

func TestTypedSSMDispatchesExactSealedExecutorArtifact(t *testing.T) {
	f, tr, _, frozen := typedFixture(t)
	artifactBytes := []byte("printf '%s\\n' sealed-executor-artifact\n")
	sum := sha256.Sum256(artifactBytes)
	artifact := execution.ArtifactRef{
		ID: frozen.Script.Artifact.ID, Digest: execution.Digest(hex.EncodeToString(sum[:])),
		MediaType: "application/x-sh", Size: int64(len(artifactBytes)), Immutable: true,
	}
	postcondition := &execution.Postcondition{Type: "exit_code", Value: "0"}
	grant, err := (execution.NetworkGrant{Scheme: "https", Host: "registry.example", Port: 443, PathPrefix: "/service", Scope: "external"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	step := execution.ExecutionStep{
		StepKey: "apply-container", Kind: execution.StepContainerApply,
		TargetID: frozen.TargetID, TargetRevision: frozen.TargetRevision, TargetDigest: frozen.TargetDigest,
		ObservationRef: frozen.Script.Step.ObservationRef, TimeoutSeconds: 30, IdempotencyMarker: "apply-container",
		NetworkGrants: []execution.NetworkGrant{grant}, OutputPolicy: execution.OutputDiscard, Postcondition: postcondition,
		ContainerApply: &execution.ContainerApplyStep{
			Image: "registry.example/service@sha256:" + strings.Repeat("a", 64), Name: "service",
			HostAddress: "127.0.0.1", HostPort: 8080, ContainerPort: 8080, RestartPolicy: "unless-stopped",
		},
		Executor: &execution.ExecutorSpec{
			Artifact: artifact, Interpreter: "/bin/sh", CWD: "/", Root: true,
			AllowedExitCodes: []int{0}, OutputLimit: 1024, Postcondition: postcondition,
		},
	}
	snapshot, err := execution.StepSnapshotFromStep(step, execution.StepSetForward)
	if err != nil {
		t.Fatal(err)
	}
	frozen.Script, err = FrozenScriptForStep(snapshot.Step)
	if err != nil {
		t.Fatal(err)
	}
	frozen.StepKey, frozen.StepDigest = snapshot.Step.StepKey, snapshot.Step.Digest
	frozen.FenceDigest, err = computedFenceDigest(frozen)
	if err != nil {
		t.Fatal(err)
	}
	frozen.RequestDigest, err = frozenRequestDigest(frozen, frozen.Script, frozen.FenceDigest)
	if err != nil {
		t.Fatal(err)
	}
	tr.artifacts = artifactFake{data: artifactBytes}

	durable := SnapshotFromFrozen(frozen)
	raw, err := json.Marshal(durable)
	if err != nil {
		t.Fatal(err)
	}
	var recovered FrozenRequestSnapshot
	if err := json.Unmarshal(raw, &recovered); err != nil || recovered.Script.Step.Executor == nil || recovered.Script.Artifact != artifact || recovered.RequestDigest != frozen.RequestDigest {
		t.Fatalf("typed executor did not survive durable snapshot: %+v, err=%v", recovered, err)
	}

	result, err := tr.Dispatch(context.Background(), frozen)
	if err != nil || result.Status != DispatchAccepted || f.sendCalls != 1 {
		t.Fatalf("typed executor dispatch result=%+v err=%v calls=%d", result, err, f.sendCalls)
	}
	command := f.lastSend.Parameters["commands"][0]
	if !strings.Contains(command, base64.StdEncoding.EncodeToString(artifactBytes)) || strings.Contains(command, "docker pull") || strings.Contains(command, snapshot.Step.ContainerApply.Image) {
		t.Fatalf("dispatch regenerated command instead of using exact artifact: %q", command)
	}
}
