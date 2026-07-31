package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type provisionIntentStoreFake struct {
	record     EC2ProvisionIntentRecord
	completed  EC2ProvisionCompletion
	readback   CloudFormationProvisionReadback
	reserves   int
	operations int
	readbacks  int
	uncertain  int
	failed     int
	completes  int
}

func (s *provisionIntentStoreFake) RecordEC2ProvisionReadback(_ context.Context, owner string, fence execution.Digest, readback CloudFormationProvisionReadback) (EC2ProvisionIntentRecord, error) {
	if owner != s.record.Intent.OwnerID || fence != s.record.Intent.FenceDigest || readback.Status == "" {
		return EC2ProvisionIntentRecord{}, execution.ErrConflict
	}
	s.readbacks++
	s.readback = readback
	if readback.Status == "CREATE_IN_PROGRESS" || readback.PendingReason != "" {
		s.record.Status = "pending"
	}
	s.record.Revision++
	return s.record, nil
}

func (s *provisionIntentStoreFake) ReserveEC2ProvisionIntent(_ context.Context, in EC2ProvisionIntent) (EC2ProvisionIntentRecord, bool, error) {
	s.reserves++
	if s.record.Intent.FenceDigest != "" {
		if s.record.Intent.FenceDigest != in.FenceDigest || s.record.Intent.RequestDigest != in.RequestDigest {
			return EC2ProvisionIntentRecord{}, false, execution.ErrConflict
		}
		return s.record, false, nil
	}
	s.record = EC2ProvisionIntentRecord{Intent: in, Status: "intent", Revision: 1}
	return s.record, true, nil
}
func (s *provisionIntentStoreFake) GetEC2ProvisionIntent(_ context.Context, owner string, fence execution.Digest) (EC2ProvisionIntentRecord, error) {
	if owner != s.record.Intent.OwnerID || fence != s.record.Intent.FenceDigest {
		return EC2ProvisionIntentRecord{}, execution.ErrNotFound
	}
	return s.record, nil
}
func (s *provisionIntentStoreFake) MarkEC2ProvisionFailed(_ context.Context, owner string, fence execution.Digest, _ CloudFormationProvisionReadback) error {
	if owner != s.record.Intent.OwnerID || fence != s.record.Intent.FenceDigest {
		return execution.ErrConflict
	}
	s.failed++
	s.record.Status = "failed"
	s.record.Revision++
	return nil
}
func (s *provisionIntentStoreFake) RecordEC2ProviderOperation(_ context.Context, owner string, fence execution.Digest, operationID string) (EC2ProvisionIntentRecord, error) {
	s.operations++
	if owner != s.record.Intent.OwnerID || fence != s.record.Intent.FenceDigest || operationID == "" {
		return EC2ProvisionIntentRecord{}, execution.ErrConflict
	}
	s.record.ProviderOperationID, s.record.Status, s.record.Revision = operationID, "accepted", s.record.Revision+1
	return s.record, nil
}
func (s *provisionIntentStoreFake) MarkEC2ProvisionUncertain(_ context.Context, owner string, fence execution.Digest) error {
	if owner != s.record.Intent.OwnerID || fence != s.record.Intent.FenceDigest {
		return execution.ErrConflict
	}
	s.uncertain++
	s.record.Status = "uncertain"
	s.record.Revision++
	return nil
}
func (s *provisionIntentStoreFake) CompleteEC2Provision(_ context.Context, in EC2ProvisionCompletion) error {
	if in.Intent.Intent.FenceDigest != s.record.Intent.FenceDigest || in.Target.ID != s.record.Intent.Request.Target.ID || in.Target.Revision != 2 || in.Observation.TargetRevision != 2 {
		return execution.ErrConflict
	}
	s.completes++
	s.completed = in
	return nil
}

type cloudFormationProvisionFake struct {
	createCalls, readCalls int
	createErr              error
	readErr                error
	readback               CloudFormationProvisionReadback
}

type provisionReadbackSDKFake struct {
	req      CloudFormationCreateRequest
	ssmInfo  []ssmtypes.InstanceInformation
	ssmErr   error
	ssmCalls int
}

func (f *provisionReadbackSDKFake) NewCloudFormationProvision(Credentials) (CloudFormationProvisionClient, error) {
	return f, nil
}
func (f *provisionReadbackSDKFake) NewEC2ProvisionReadback(Credentials) (EC2Client, error) {
	return f, nil
}
func (f *provisionReadbackSDKFake) NewSSMProvisionReadback(Credentials) (SSMClient, error) {
	return f, nil
}
func (f *provisionReadbackSDKFake) CreateStack(context.Context, *cloudformation.CreateStackInput, ...func(*cloudformation.Options)) (*cloudformation.CreateStackOutput, error) {
	return nil, errors.New("unexpected create")
}
func (f *provisionReadbackSDKFake) DescribeStacks(context.Context, *cloudformation.DescribeStacksInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeStacksOutput, error) {
	stackID := "arn:aws:cloudformation:us-east-1:123456789012:stack/" + f.req.StackName + "/id"
	return &cloudformation.DescribeStacksOutput{Stacks: []cftypes.Stack{{
		StackName: awsapi.String(f.req.StackName), StackId: awsapi.String(stackID), StackStatus: cftypes.StackStatusCreateComplete,
		Tags: []cftypes.Tag{
			{Key: awsapi.String("dirextalk:managed-by"), Value: awsapi.String("execution-v2")},
			{Key: awsapi.String("dirextalk:request-digest"), Value: awsapi.String(string(f.req.RequestDigest))},
			{Key: awsapi.String("dirextalk:reservation-id"), Value: awsapi.String(f.req.ReservationTargetID)},
			{Key: awsapi.String("dirextalk:reservation-digest"), Value: awsapi.String(string(f.req.ReservationDigest))},
			{Key: awsapi.String("dirextalk:template-version"), Value: awsapi.String(cloudFormationProvisionTemplateVersion)},
		},
		Outputs: []cftypes.Output{
			{OutputKey: awsapi.String("AmiId"), OutputValue: awsapi.String("ami-0123456789abcdef0")},
			{OutputKey: awsapi.String("InstanceId"), OutputValue: awsapi.String("i-0123456789abcdef0")},
			{OutputKey: awsapi.String("SecurityGroupId"), OutputValue: awsapi.String("sg-0123456789abcdef0")},
			{OutputKey: awsapi.String("SubnetId"), OutputValue: awsapi.String("subnet-0123456789abcdef0")},
			{OutputKey: awsapi.String("VpcId"), OutputValue: awsapi.String("vpc-0123456789abcdef0")},
		},
	}}}, nil
}
func (f *provisionReadbackSDKFake) GetTemplate(context.Context, *cloudformation.GetTemplateInput, ...func(*cloudformation.Options)) (*cloudformation.GetTemplateOutput, error) {
	template, _, err := buildEC2ProvisionTemplate(f.req)
	if err != nil {
		return nil, err
	}
	return &cloudformation.GetTemplateOutput{TemplateBody: awsapi.String(string(template))}, nil
}
func (f *provisionReadbackSDKFake) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
		InstanceId: awsapi.String("i-0123456789abcdef0"), ImageId: awsapi.String("ami-0123456789abcdef0"), InstanceType: ec2types.InstanceTypeT3Small,
		Architecture: ec2types.ArchitectureValuesX8664, Placement: &ec2types.Placement{AvailabilityZone: awsapi.String("us-east-1a")}, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}, VpcId: awsapi.String("vpc-0123456789abcdef0"), SubnetId: awsapi.String("subnet-0123456789abcdef0"), PublicIpAddress: awsapi.String("203.0.113.10"),
		SecurityGroups: []ec2types.GroupIdentifier{{GroupId: awsapi.String("sg-0123456789abcdef0")}}, IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: awsapi.String("arn:aws:iam::123456789012:instance-profile/dirextalk")},
		RootDeviceName: awsapi.String("/dev/xvda"), BlockDeviceMappings: []ec2types.InstanceBlockDeviceMapping{{DeviceName: awsapi.String("/dev/xvda"), Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: awsapi.String("vol-0123456789abcdef0")}}},
	}}}}}, nil
}
func (f *provisionReadbackSDKFake) DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: []ec2types.InstanceTypeInfo{{InstanceType: ec2types.InstanceTypeT3Small, VCpuInfo: &ec2types.VCpuInfo{DefaultVCpus: awsapi.Int32(2)}, MemoryInfo: &ec2types.MemoryInfo{SizeInMiB: awsapi.Int64(2048)}}}}, nil
}
func (f *provisionReadbackSDKFake) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{{OwnerId: awsapi.String("123456789012"), VpcId: awsapi.String("vpc-0123456789abcdef0"), GroupId: awsapi.String("sg-0123456789abcdef0"), IpPermissionsEgress: []ec2types.IpPermission{{IpProtocol: awsapi.String("tcp"), FromPort: awsapi.Int32(443), ToPort: awsapi.Int32(443), IpRanges: []ec2types.IpRange{{CidrIp: awsapi.String("0.0.0.0/0")}}}}}}}, nil
}
func (f *provisionReadbackSDKFake) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{{VolumeId: awsapi.String("vol-0123456789abcdef0"), Size: awsapi.Int32(20), Encrypted: awsapi.Bool(true), VolumeType: ec2types.VolumeTypeGp3}}}, nil
}
func (f *provisionReadbackSDKFake) DescribeInstanceInformation(context.Context, *ssm.DescribeInstanceInformationInput, ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error) {
	f.ssmCalls++
	if f.ssmErr != nil {
		return nil, f.ssmErr
	}
	return &ssm.DescribeInstanceInformationOutput{InstanceInformationList: append([]ssmtypes.InstanceInformation(nil), f.ssmInfo...)}, nil
}
func (f *provisionReadbackSDKFake) SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	return nil, errors.New("unexpected send")
}
func (f *provisionReadbackSDKFake) GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	return nil, errors.New("unexpected get command")
}
func (f *provisionReadbackSDKFake) CancelCommand(context.Context, *ssm.CancelCommandInput, ...func(*ssm.Options)) (*ssm.CancelCommandOutput, error) {
	return nil, errors.New("unexpected cancel")
}

func (f *cloudFormationProvisionFake) Create(_ context.Context, in CloudFormationCreateRequest) (string, error) {
	f.createCalls++
	if f.createErr != nil {
		return "", f.createErr
	}
	return "arn:aws:cloudformation:us-east-1:123456789012:stack/" + in.StackName + "/id", nil
}
func (f *cloudFormationProvisionFake) Readback(_ context.Context, in CloudFormationCreateRequest, _ string) (CloudFormationProvisionReadback, error) {
	f.readCalls++
	if f.readErr != nil {
		return CloudFormationProvisionReadback{}, f.readErr
	}
	out := f.readback
	out.StackName = in.StackName
	return out, nil
}

func provisionFixture(t *testing.T) (EC2ProvisionRequest, Credentials, *provisionIntentStoreFake, *cloudFormationProvisionFake, *EC2ProvisionExecutor) {
	t.Helper()
	now := time.Date(2036, 1, 2, 3, 4, 5, 0, time.UTC)
	credentialID := "11111111-1111-4111-8111-111111111111"
	credential := RehydrateCredentials(credentialID, "provision", "us-east-1", "123456789012", "arn:aws:iam::123456789012:role/provision", []byte("AKIAABCDEFGHIJKLMNOP"), []byte("secret"), nil, 4, 4, now, now)
	ref := execution.CredentialRef{Ref: credentialID, Purpose: "aws", Revision: 4}
	var err error
	ref.BindingDigest, err = CredentialBindingDigest("@owner:example.org", ref, credential)
	if err != nil {
		t.Fatal(err)
	}
	reservation := &execution.ComputeReservation{InfrastructureProfileID: InfrastructureProfileGeneralLinuxSSMV1, AMIParameter: execution.AWSAL2023X8664AMIParameter, InstanceType: "t3.small", AvailabilityZone: "us-east-1a", VolumeGiB: 20, Architecture: "x86_64", ManagementTransport: "aws_ssm", PublicIP: true, CostQuote: execution.CostQuote{Amount: "0.02", Currency: "USD", ExpiresAt: now.Add(time.Hour)}}
	target, err := (execution.ExecutionTarget{ID: "22222222-2222-4222-8222-222222222222", Provider: "aws", Kind: execution.TargetKindAWSComputeReservation, InfrastructureProfileID: InfrastructureProfileGeneralLinuxSSMV1, AccountID: credential.AccountID, Region: credential.Region, Architecture: "x86_64", Capabilities: []string{"compute.catalog", "compute.provision", "target.aws_compute_reservation"}, CredentialRefs: []execution.CredentialRef{ref}, Network: execution.NetworkPolicy{Mode: "restricted"}, ComputeReservation: reservation, Revision: 1}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	step := execution.ExecutionStep{StepKey: "provision-compute", Kind: execution.StepComputeProvision, TargetID: target.ID, TargetRevision: 1, TargetDigest: target.Digest, TimeoutSeconds: 1800, IdempotencyMarker: "provision-compute", ComputeProvision: &execution.ComputeProvisionStep{InfrastructureProfileID: reservation.InfrastructureProfileID, AMIParameter: reservation.AMIParameter, InstanceType: reservation.InstanceType, AvailabilityZone: reservation.AvailabilityZone, VolumeGiB: reservation.VolumeGiB, Region: target.Region, Architecture: "x86_64", ManagementTransport: "aws_ssm", PublicIP: true}}
	stepSnapshot, err := execution.StepSnapshotFromStep(step, execution.StepSetForward)
	if err != nil {
		t.Fatal(err)
	}
	req := EC2ProvisionRequest{OwnerID: "@owner:example.org", PlanID: "33333333-3333-4333-8333-333333333333", PlanRevision: 1, PlanDigest: execution.Digest("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), RunID: "44444444-4444-4444-8444-444444444444", RunRevision: 1, RunDigest: execution.Digest("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"), StageID: "55555555-5555-4555-8555-555555555555", StageRevision: 1, StageDigest: execution.Digest("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"), AttemptID: "66666666-6666-4666-8666-666666666666", StepRevision: 1, Target: target, Step: stepSnapshot.Step, PolicyDigest: execution.Digest("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"), CostQuoteDigest: execution.Digest("9999999999999999999999999999999999999999999999999999999999999999"), FenceDigest: execution.Digest("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")}
	store := &provisionIntentStoreFake{}
	provider := &cloudFormationProvisionFake{readback: CloudFormationProvisionReadback{Status: "CREATE_COMPLETE", InstanceID: "i-0123456789abcdef0", InstanceType: "t3.small", AvailabilityZone: "us-east-1a", Architecture: "x86_64", SSMStatus: "Online", OperatingSystem: "linux", PlatformName: "Amazon Linux", PlatformVersion: "2023", VCPUCount: 2, MemoryMiB: 2048, RootVolumeGiB: 20, PublicIP: "203.0.113.10", HTTPSEgress: execution.ObservationFactHTTPSEgressValue, SecurityGroupDigest: execution.Digest("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")}}
	executor, err := NewEC2ProvisionExecutor(store, provider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return req, credential, store, provider, executor
}

func TestEC2ProvisionPersistsIntentAndOperationBeforeAtomicRevisionTwo(t *testing.T) {
	req, credential, store, provider, executor := provisionFixture(t)
	result, err := executor.Execute(context.Background(), req, credential)
	if err != nil {
		t.Fatal(err)
	}
	if store.reserves != 1 || store.operations != 1 || provider.createCalls != 1 || provider.readCalls != 1 || store.completes != 1 {
		t.Fatalf("calls reserve/op/create/read/complete=%d/%d/%d/%d/%d", store.reserves, store.operations, provider.createCalls, provider.readCalls, store.completes)
	}
	if result.Target.ID != req.Target.ID || result.Target.Revision != 2 || result.Target.Kind != execution.TargetKindAWSEC2Instance || result.Target.ComputeReservation != nil || result.Target.Network.Mode != execution.NetworkPolicyModeObservedHTTPSEgress || result.Observation.Facts["ssm_status"] != "Online" {
		t.Fatalf("completion=%+v", result)
	}
}

func TestEC2ProvisionTemplateGrantsOnlyTargetScopedSecretRead(t *testing.T) {
	req, credential, _, _, _ := provisionFixture(t)
	_, providerRequest, _, err := normalizeEC2ProvisionRequest(req, credential)
	if err != nil {
		t.Fatal(err)
	}
	template, _, err := buildEC2ProvisionTemplate(providerRequest)
	if err != nil {
		t.Fatal(err)
	}
	body := string(template)
	wantPath := "parameter/dirextalk/execution-v2/" + req.Target.ID + "/*"
	for _, required := range []string{"ssm:GetParameter", wantPath, "kms:Decrypt", "kms:ViaService", "kms:EncryptionContext:PARAMETER_ARN"} {
		if !strings.Contains(body, required) {
			t.Fatalf("template missing %q: %s", required, body)
		}
	}
	for _, forbidden := range []string{"ssm:GetParameters", "ssm:GetParametersByPath", "ssm:PutParameter", "ssm:DeleteParameter", "provider-api-key-value", "GeoLibre"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("template contains overbroad or project-specific policy %q", forbidden)
		}
	}
}

func TestCloudFormationReadbackTreatsSSMStartupAsStrictPending(t *testing.T) {
	req, credential, _, _, _ := provisionFixture(t)
	_, providerRequest, _, err := normalizeEC2ProvisionRequest(req, credential)
	if err != nil {
		t.Fatal(err)
	}
	sdk := &provisionReadbackSDKFake{req: providerRequest}
	provider, err := NewCloudFormationProvisionProvider(sdk)
	if err != nil {
		t.Fatal(err)
	}
	operationID := "arn:aws:cloudformation:us-east-1:123456789012:stack/" + providerRequest.StackName + "/id"

	readback, err := provider.Readback(context.Background(), providerRequest, operationID)
	if err != nil || readback.Status != "CREATE_COMPLETE" || readback.PendingReason != EC2ProvisionPendingSSMRegistration || readback.InstanceID != "i-0123456789abcdef0" || readback.SSMStatus != "" {
		t.Fatalf("registration readback=%+v err=%v", readback, err)
	}

	sdk.ssmInfo = []ssmtypes.InstanceInformation{{InstanceId: awsapi.String("i-0123456789abcdef0"), PingStatus: ssmtypes.PingStatusInactive}}
	readback, err = provider.Readback(context.Background(), providerRequest, operationID)
	if err != nil || readback.PendingReason != EC2ProvisionPendingSSMOnline || readback.SSMStatus != "Inactive" || readback.InstanceID == "" {
		t.Fatalf("offline readback=%+v err=%v", readback, err)
	}
	sdk.ssmInfo = []ssmtypes.InstanceInformation{{InstanceId: awsapi.String("i-0123456789abcdef0"), PingStatus: ssmtypes.PingStatusOnline, PlatformType: ssmtypes.PlatformTypeLinux, PlatformName: awsapi.String("Amazon Linux"), PlatformVersion: awsapi.String("2023")}}
	readback, err = provider.Readback(context.Background(), providerRequest, operationID)
	if err != nil || readback.PendingReason != "" || readback.SSMStatus != "Online" || readback.OperatingSystem != "linux" {
		t.Fatalf("ready readback=%+v err=%v", readback, err)
	}

	sdk.ssmErr = errors.New("ssm unavailable")
	if _, err = provider.Readback(context.Background(), providerRequest, operationID); !errors.Is(err, ErrEC2ProvisionUncertain) {
		t.Fatalf("SSM API failure err=%v", err)
	}
	sdk.ssmErr = nil
	sdk.ssmInfo = []ssmtypes.InstanceInformation{{InstanceId: awsapi.String("i-aaaaaaaaaaaaaaaaa"), PingStatus: ssmtypes.PingStatusInactive}}
	if _, err = provider.Readback(context.Background(), providerRequest, operationID); !errors.Is(err, ErrEC2ProvisionUncertain) {
		t.Fatalf("structural drift err=%v", err)
	}
}

func TestEC2ProvisionAmbiguousCreateNeverRedispatchesAndUsesReadbackOnly(t *testing.T) {
	req, credential, store, provider, executor := provisionFixture(t)
	provider.createErr = errors.New("response lost")
	provider.readErr = errors.New("not yet visible")
	if _, err := executor.Execute(context.Background(), req, credential); !errors.Is(err, ErrEC2ProvisionUncertain) || provider.createCalls != 1 || provider.readCalls != 1 || store.uncertain != 1 {
		t.Fatalf("first err=%v calls=%d/%d uncertain=%d", err, provider.createCalls, provider.readCalls, store.uncertain)
	}
	provider.createErr = nil
	provider.readErr = nil
	result, err := executor.Execute(context.Background(), req, credential)
	if err != nil || result.Target.Revision != 2 || provider.createCalls != 1 || provider.readCalls != 2 || store.completes != 1 {
		t.Fatalf("reconcile=%+v err=%v calls=%d/%d complete=%d", result, err, provider.createCalls, provider.readCalls, store.completes)
	}
}

func TestEC2ProvisionRetriesOnlyWhitelistedReadbackFailuresWithoutRedispatch(t *testing.T) {
	req, credential, store, provider, executor := provisionFixture(t)
	provider.readErr = provisionReadbackUncertain("describe_stacks")
	if _, err := executor.Execute(context.Background(), req, credential); !errors.Is(err, ErrEC2ProvisionPending) || provider.createCalls != 1 || provider.readCalls != 1 || store.uncertain != 1 {
		t.Fatalf("transient readback err=%v calls=%d/%d uncertain=%d", err, provider.createCalls, provider.readCalls, store.uncertain)
	}
	provider.readErr = nil
	result, err := executor.Execute(context.Background(), req, credential)
	if err != nil || result.Target.Revision != 2 || provider.createCalls != 1 || provider.readCalls != 2 || store.completes != 1 {
		t.Fatalf("readback retry=%+v err=%v calls=%d/%d complete=%d", result, err, provider.createCalls, provider.readCalls, store.completes)
	}

	req, credential, store, provider, executor = provisionFixture(t)
	provider.readErr = provisionReadbackUncertain("stack_identity")
	if _, err = executor.Execute(context.Background(), req, credential); !errors.Is(err, ErrEC2ProvisionUncertain) || errors.Is(err, ErrEC2ProvisionPending) || provider.createCalls != 1 || store.uncertain != 1 {
		t.Fatalf("structural readback err=%v calls=%d uncertain=%d", err, provider.createCalls, store.uncertain)
	}
}

func TestEC2ProvisionCreateCompleteWaitsForSSMByReadbackOnly(t *testing.T) {
	req, credential, store, provider, executor := provisionFixture(t)
	ready := provider.readback
	provider.readback.PendingReason = EC2ProvisionPendingSSMRegistration
	provider.readback.SSMStatus = ""
	provider.readback.OperatingSystem = ""
	provider.readback.PlatformName = ""
	provider.readback.PlatformVersion = ""

	if _, err := executor.Execute(context.Background(), req, credential); !errors.Is(err, ErrEC2ProvisionPending) {
		t.Fatalf("initial pending err=%v", err)
	}
	if provider.createCalls != 1 || provider.readCalls != 1 || store.readbacks != 1 || store.record.Status != "pending" || store.readback.InstanceID != provider.readback.InstanceID || store.readback.PendingReason != EC2ProvisionPendingSSMRegistration || store.uncertain != 0 || store.completes != 0 {
		t.Fatalf("initial calls create/read/persist/uncertain/complete=%d/%d/%d/%d/%d status=%s", provider.createCalls, provider.readCalls, store.readbacks, store.uncertain, store.completes, store.record.Status)
	}
	if _, err := executor.Reconcile(context.Background(), req, credential); !errors.Is(err, ErrEC2ProvisionPending) || provider.createCalls != 1 || provider.readCalls != 2 || store.uncertain != 0 {
		t.Fatalf("pending reconcile err=%v create/read=%d/%d uncertain=%d", err, provider.createCalls, provider.readCalls, store.uncertain)
	}

	provider.readback = ready
	completion, err := executor.Execute(context.Background(), req, credential)
	if err != nil || completion.Target.Revision != 2 || provider.createCalls != 1 || provider.readCalls != 3 || store.completes != 1 || store.uncertain != 0 {
		t.Fatalf("ready completion=%+v err=%v calls=%d/%d complete=%d uncertain=%d", completion, err, provider.createCalls, provider.readCalls, store.completes, store.uncertain)
	}
}

func TestEC2ProvisionRejectsStructurallyInvalidSSMPendingReadback(t *testing.T) {
	req, credential, store, provider, executor := provisionFixture(t)
	provider.readback.PendingReason = EC2ProvisionPendingSSMOnline
	provider.readback.SSMStatus = "Bogus"
	provider.readback.OperatingSystem = ""
	provider.readback.PlatformName = ""
	provider.readback.PlatformVersion = ""
	if _, err := executor.Execute(context.Background(), req, credential); !errors.Is(err, ErrEC2ProvisionUncertain) || provider.createCalls != 1 || provider.readCalls != 1 || store.uncertain != 1 || store.completes != 0 {
		t.Fatalf("err=%v calls=%d/%d uncertain=%d complete=%d", err, provider.createCalls, provider.readCalls, store.uncertain, store.completes)
	}
}

func TestEC2ProvisionReconcileReadsExactPersistedIntentWithoutCreate(t *testing.T) {
	req, credential, store, provider, executor := provisionFixture(t)
	provider.createErr = errors.New("response lost")
	provider.readErr = errors.New("stack readback unavailable")
	if _, err := executor.Execute(context.Background(), req, credential); !errors.Is(err, ErrEC2ProvisionUncertain) {
		t.Fatalf("initial ambiguous create: %v", err)
	}
	if provider.createCalls != 1 || store.record.Status != "uncertain" {
		t.Fatalf("initial create calls=%d intent=%s", provider.createCalls, store.record.Status)
	}

	provider.readErr = nil
	result, err := executor.Reconcile(context.Background(), req, credential)
	if err != nil {
		t.Fatal(err)
	}
	if result.Target.Revision != 2 || provider.createCalls != 1 || provider.readCalls != 2 || store.completes != 1 {
		t.Fatalf("readback-only reconcile=%+v create/read/complete=%d/%d/%d", result, provider.createCalls, provider.readCalls, store.completes)
	}
}
