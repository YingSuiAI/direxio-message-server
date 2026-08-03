package aws

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const cloudFormationProvisionTemplateVersion = "dirextalk-execution-v2-ec2-3"

// SDKCloudFormationProvisionProvider owns only the generic host lifecycle. It
// never sees a Recipe/project name or arbitrary shell command.
type SDKCloudFormationProvisionProvider struct {
	factory CloudFormationProvisionClientFactory
	timeout time.Duration
}

func NewCloudFormationProvisionProvider(factory CloudFormationProvisionClientFactory) (*SDKCloudFormationProvisionProvider, error) {
	if factory == nil {
		return nil, ErrEC2ProvisionInvalid
	}
	return &SDKCloudFormationProvisionProvider{factory: factory, timeout: 90 * time.Second}, nil
}

type ec2ProvisionReadbackError struct{ code string }

func (e *ec2ProvisionReadbackError) Error() string { return "aws ec2 provision readback: " + e.code }
func (e *ec2ProvisionReadbackError) Unwrap() error { return ErrEC2ProvisionUncertain }

func provisionReadbackUncertain(code string) error {
	return &ec2ProvisionReadbackError{code: code}
}

func provisionReadbackCode(err error) string {
	var typed *ec2ProvisionReadbackError
	if errors.As(err, &typed) && typed != nil && typed.code != "" {
		return typed.code
	}
	return "unclassified"
}

func retryableProvisionReadback(err error) bool {
	var typed *ec2ProvisionReadbackError
	if !errors.As(err, &typed) || typed == nil {
		return false
	}
	switch typed.code {
	case "cloudformation_client", "describe_stacks", "get_template", "ec2_client", "describe_instances",
		"describe_instance_type", "security_group", "describe_root_volume", "ssm_client", "describe_ssm_instance":
		return true
	default:
		return false
	}
}

func (p *SDKCloudFormationProvisionProvider) Create(ctx context.Context, req CloudFormationCreateRequest) (string, error) {
	if p == nil || p.factory == nil || validateCloudFormationRequest(req) != nil {
		return "", ErrEC2ProvisionInvalid
	}
	template, _, err := buildEC2ProvisionTemplate(req)
	if err != nil {
		return "", ErrEC2ProvisionInvalid
	}
	client, err := p.factory.NewCloudFormationProvision(credentialsFromCreateRequest(req))
	if err != nil || client == nil {
		return "", ErrEC2ProvisionUncertain
	}
	callCtx, cancel := provisionContext(ctx, p.timeout)
	defer cancel()
	out, err := client.CreateStack(callCtx, &cloudformation.CreateStackInput{
		StackName:          awsapi.String(req.StackName),
		TemplateBody:       awsapi.String(string(template)),
		Capabilities:       []cftypes.Capability{cftypes.CapabilityCapabilityIam},
		ClientRequestToken: awsapi.String(string(req.RequestDigest)),
		OnFailure:          cftypes.OnFailureDoNothing,
		TimeoutInMinutes:   awsapi.Int32(30),
		Tags: []cftypes.Tag{
			{Key: awsapi.String("dirextalk:managed-by"), Value: awsapi.String("execution-v2")},
			{Key: awsapi.String("dirextalk:request-digest"), Value: awsapi.String(string(req.RequestDigest))},
			{Key: awsapi.String("dirextalk:reservation-id"), Value: awsapi.String(req.ReservationTargetID)},
			{Key: awsapi.String("dirextalk:reservation-digest"), Value: awsapi.String(string(req.ReservationDigest))},
			{Key: awsapi.String("dirextalk:template-version"), Value: awsapi.String(cloudFormationProvisionTemplateVersion)},
		},
	})
	if err != nil || out == nil || !validStackARN(awsapi.ToString(out.StackId), req.AccountID, req.Region, req.StackName) {
		// CreateStack is a mutation. Every error, including an SDK timeout, is
		// ambiguous and must be resolved by DescribeStacks using StackName.
		return "", ErrEC2ProvisionUncertain
	}
	return awsapi.ToString(out.StackId), nil
}

func (p *SDKCloudFormationProvisionProvider) Readback(ctx context.Context, req CloudFormationCreateRequest, operationID string) (CloudFormationProvisionReadback, error) {
	var zero CloudFormationProvisionReadback
	if p == nil || p.factory == nil || validateCloudFormationRequest(req) != nil || (strings.TrimSpace(operationID) != "" && !validStackARN(operationID, req.AccountID, req.Region, req.StackName)) {
		return zero, ErrEC2ProvisionInvalid
	}
	credential := credentialsFromCreateRequest(req)
	cf, err := p.factory.NewCloudFormationProvision(credential)
	if err != nil || cf == nil {
		return zero, provisionReadbackUncertain("cloudformation_client")
	}
	callCtx, cancel := provisionContext(ctx, p.timeout)
	defer cancel()
	lookup := req.StackName
	if strings.TrimSpace(operationID) != "" {
		lookup = operationID
	}
	stacks, err := cf.DescribeStacks(callCtx, &cloudformation.DescribeStacksInput{StackName: awsapi.String(lookup)})
	if err != nil || stacks == nil || len(stacks.Stacks) != 1 {
		return zero, provisionReadbackUncertain("describe_stacks")
	}
	stack := stacks.Stacks[0]
	stackID := awsapi.ToString(stack.StackId)
	if awsapi.ToString(stack.StackName) != req.StackName || !validStackARN(stackID, req.AccountID, req.Region, req.StackName) ||
		!exactProvisionTags(stack.Tags, req) {
		return zero, provisionReadbackUncertain("stack_identity")
	}
	templateOut, err := cf.GetTemplate(callCtx, &cloudformation.GetTemplateInput{StackName: awsapi.String(stackID), TemplateStage: cftypes.TemplateStageOriginal})
	if err != nil || templateOut == nil || strings.TrimSpace(awsapi.ToString(templateOut.TemplateBody)) == "" {
		return zero, provisionReadbackUncertain("get_template")
	}
	_, expectedTemplateDigest, err := buildEC2ProvisionTemplate(req)
	if err != nil {
		return zero, ErrEC2ProvisionInvalid
	}
	actualTemplateDigest, err := canonicalTemplateDigest([]byte(awsapi.ToString(templateOut.TemplateBody)))
	if err != nil || actualTemplateDigest != expectedTemplateDigest {
		return zero, provisionReadbackUncertain("template_digest")
	}
	readback := CloudFormationProvisionReadback{StackName: req.StackName, StackID: stackID, Status: string(stack.StackStatus)}
	if stack.StackStatus != cftypes.StackStatusCreateComplete {
		return readback, nil
	}
	outputs, ok := exactStackOutputs(stack.Outputs)
	if !ok || !ValidEC2InstanceID(outputs["InstanceId"]) || !validSecurityGroupID(outputs["SecurityGroupId"]) || outputs["AmiId"] == "" || outputs["VpcId"] == "" || outputs["SubnetId"] == "" {
		return zero, provisionReadbackUncertain("stack_outputs")
	}
	ec2c, err := p.factory.NewEC2ProvisionReadback(credential)
	if err != nil || ec2c == nil {
		return zero, provisionReadbackUncertain("ec2_client")
	}
	instances, err := ec2c.DescribeInstances(callCtx, &ec2.DescribeInstancesInput{InstanceIds: []string{outputs["InstanceId"]}})
	if err != nil || instances == nil {
		return zero, provisionReadbackUncertain("describe_instances")
	}
	instance, ok := oneInstance(instances)
	if !ok || awsapi.ToString(instance.InstanceId) != outputs["InstanceId"] || instance.Architecture != ec2types.ArchitectureValuesX8664 ||
		string(instance.InstanceType) != req.Provision.InstanceType || instance.State == nil || instance.State.Name != ec2types.InstanceStateNameRunning ||
		instance.Placement == nil || awsapi.ToString(instance.Placement.AvailabilityZone) != req.Provision.AvailabilityZone ||
		awsapi.ToString(instance.ImageId) != outputs["AmiId"] || awsapi.ToString(instance.VpcId) != outputs["VpcId"] || awsapi.ToString(instance.SubnetId) != outputs["SubnetId"] ||
		strings.TrimSpace(awsapi.ToString(instance.PublicIpAddress)) == "" || len(instance.SecurityGroups) != 1 || awsapi.ToString(instance.SecurityGroups[0].GroupId) != outputs["SecurityGroupId"] ||
		instance.IamInstanceProfile == nil || !validIAMProfileARN(awsapi.ToString(instance.IamInstanceProfile.Arn), req.AccountID) {
		return zero, provisionReadbackUncertain("instance_identity")
	}
	typeOutput, err := ec2c.DescribeInstanceTypes(callCtx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []ec2types.InstanceType{instance.InstanceType}})
	if err != nil || typeOutput == nil || len(typeOutput.InstanceTypes) != 1 || typeOutput.InstanceTypes[0].InstanceType != instance.InstanceType ||
		typeOutput.InstanceTypes[0].VCpuInfo == nil || awsapi.ToInt32(typeOutput.InstanceTypes[0].VCpuInfo.DefaultVCpus) <= 0 ||
		typeOutput.InstanceTypes[0].MemoryInfo == nil || awsapi.ToInt64(typeOutput.InstanceTypes[0].MemoryInfo.SizeInMiB) <= 0 {
		return zero, provisionReadbackUncertain("describe_instance_type")
	}
	securityGroups, err := ec2c.DescribeSecurityGroups(callCtx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{outputs["SecurityGroupId"]}})
	if err != nil || securityGroups == nil || len(securityGroups.SecurityGroups) != 1 {
		return zero, provisionReadbackUncertain("security_group")
	}
	securityDigest, ok := exactManagementSecurityGroupDigest(securityGroups.SecurityGroups[0], req.AccountID, outputs["VpcId"], outputs["SecurityGroupId"])
	if !ok {
		return zero, provisionReadbackUncertain("security_group")
	}
	rootVolumeID := rootVolume(instance)
	if rootVolumeID == "" {
		return zero, provisionReadbackUncertain("root_volume_identity")
	}
	volumes, err := ec2c.DescribeVolumes(callCtx, &ec2.DescribeVolumesInput{VolumeIds: []string{rootVolumeID}})
	if err != nil || volumes == nil || len(volumes.Volumes) != 1 || awsapi.ToString(volumes.Volumes[0].VolumeId) != rootVolumeID ||
		awsapi.ToInt32(volumes.Volumes[0].Size) != int32(req.Provision.VolumeGiB) || !awsapi.ToBool(volumes.Volumes[0].Encrypted) || volumes.Volumes[0].VolumeType != ec2types.VolumeTypeGp3 {
		return zero, provisionReadbackUncertain("describe_root_volume")
	}
	readback.InstanceID = outputs["InstanceId"]
	readback.InstanceType = string(instance.InstanceType)
	readback.AvailabilityZone = awsapi.ToString(instance.Placement.AvailabilityZone)
	readback.Architecture = "x86_64"
	readback.VCPUCount = awsapi.ToInt32(typeOutput.InstanceTypes[0].VCpuInfo.DefaultVCpus)
	readback.MemoryMiB = awsapi.ToInt64(typeOutput.InstanceTypes[0].MemoryInfo.SizeInMiB)
	readback.RootVolumeGiB = awsapi.ToInt32(volumes.Volumes[0].Size)
	readback.PublicIP = awsapi.ToString(instance.PublicIpAddress)
	readback.PublicInbound = false
	readback.HTTPSEgress = execution.ObservationFactHTTPSEgressValue
	readback.SecurityGroupDigest = securityDigest

	ssmc, err := p.factory.NewSSMProvisionReadback(credential)
	if err != nil || ssmc == nil {
		return zero, provisionReadbackUncertain("ssm_client")
	}
	ssmOut, err := ssmc.DescribeInstanceInformation(callCtx, &ssm.DescribeInstanceInformationInput{Filters: []ssmtypes.InstanceInformationStringFilter{{Key: awsapi.String("InstanceIds"), Values: []string{outputs["InstanceId"]}}}})
	if err != nil || ssmOut == nil || len(ssmOut.InstanceInformationList) > 1 {
		return zero, provisionReadbackUncertain("describe_ssm_instance")
	}
	if len(ssmOut.InstanceInformationList) == 0 {
		readback.PendingReason = EC2ProvisionPendingSSMRegistration
		return readback, nil
	}
	info := ssmOut.InstanceInformationList[0]
	if awsapi.ToString(info.InstanceId) != outputs["InstanceId"] {
		return zero, provisionReadbackUncertain("ssm_instance_identity")
	}
	if info.PingStatus != ssmtypes.PingStatusOnline {
		if info.PingStatus != ssmtypes.PingStatusConnectionLost && info.PingStatus != ssmtypes.PingStatusInactive {
			return zero, provisionReadbackUncertain("ssm_ping_status")
		}
		readback.PendingReason = EC2ProvisionPendingSSMOnline
		readback.SSMStatus = string(info.PingStatus)
		return readback, nil
	}
	if info.PlatformType != ssmtypes.PlatformTypeLinux || awsapi.ToString(info.PlatformName) != "Amazon Linux" || !strings.HasPrefix(awsapi.ToString(info.PlatformVersion), "2023") {
		return zero, provisionReadbackUncertain("ssm_platform")
	}
	readback.OperatingSystem = "linux"
	readback.SSMStatus = "Online"
	readback.PlatformName = awsapi.ToString(info.PlatformName)
	readback.PlatformVersion = awsapi.ToString(info.PlatformVersion)
	return readback, nil
}

func provisionContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, timeout)
}

func credentialsFromCreateRequest(req CloudFormationCreateRequest) Credentials {
	return Credentials{ID: req.CredentialID, Region: req.Region, AccountID: req.AccountID, Revision: int64(req.CredentialRevision), VerifiedRevision: int64(req.CredentialRevision), private: req.Credential.credential, UserARN: req.Credential.UserARN}
}

func validateCloudFormationRequest(req CloudFormationCreateRequest) error {
	if strings.TrimSpace(req.OwnerID) == "" || !validStackName(req.StackName) || !accountIDValid(req.AccountID) || !validRegion(req.Region) ||
		!execution.ValidateUUID(req.CredentialID) || req.CredentialRevision == 0 || !execution.ValidateUUID(req.ReservationTargetID) ||
		!req.ReservationDigest.Valid() || !req.RequestDigest.Valid() || req.Credential.credential == nil || req.Credential.AccountID != req.AccountID || req.Credential.Region != req.Region ||
		!executionStepMatchesProfile(req.Provision) {
		return ErrEC2ProvisionInvalid
	}
	return nil
}

func executionStepMatchesProfile(step execution.ComputeProvisionStep) bool {
	return step.InfrastructureProfileID == InfrastructureProfileGeneralLinuxSSMV1 && step.AMIParameter == execution.AWSAL2023X8664AMIParameter &&
		strings.TrimSpace(step.InstanceType) != "" && step.VolumeGiB >= 8 && step.VolumeGiB <= 16384 && validRegion(step.Region) && execution.ValidateAvailabilityZone(step.Region, step.AvailabilityZone) &&
		step.Architecture == "x86_64" && step.ManagementTransport == "aws_ssm" && step.PublicIP && !step.PublicInbound
}

func validStackName(value string) bool {
	if len(value) < 1 || len(value) > 128 || !((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

func buildEC2ProvisionTemplate(req CloudFormationCreateRequest) ([]byte, execution.Digest, error) {
	if !executionStepMatchesProfile(req.Provision) {
		return nil, "", ErrEC2ProvisionInvalid
	}
	parameterARN := map[string]any{"Fn::Sub": "arn:${AWS::Partition}:ssm:${AWS::Region}:${AWS::AccountId}:parameter/dirextalk/execution-v2/" + req.ReservationTargetID + "/*"}
	instanceRole := map[string]any{
		"Type": "AWS::IAM::Role",
		"Properties": map[string]any{
			"AssumeRolePolicyDocument": map[string]any{
				"Version": "2012-10-17",
				"Statement": []any{map[string]any{
					"Effect": "Allow", "Principal": map[string]any{"Service": []any{"ec2.amazonaws.com"}}, "Action": []any{"sts:AssumeRole"},
				}},
			},
			"ManagedPolicyArns": []any{"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"},
			"Policies": []any{map[string]any{
				"PolicyName": "DirextalkTargetSecretRead",
				"PolicyDocument": map[string]any{
					"Version": "2012-10-17",
					"Statement": []any{
						map[string]any{"Sid": "ReadExactTargetParameters", "Effect": "Allow", "Action": []any{"ssm:GetParameter"}, "Resource": parameterARN},
						map[string]any{
							"Sid": "DecryptExactTargetParameters", "Effect": "Allow", "Action": []any{"kms:Decrypt"}, "Resource": "*",
							"Condition": map[string]any{
								"StringEquals": map[string]any{"kms:ViaService": map[string]any{"Fn::Sub": "ssm.${AWS::Region}.${AWS::URLSuffix}"}},
								"StringLike":   map[string]any{"kms:EncryptionContext:PARAMETER_ARN": parameterARN},
							},
						},
					},
				},
			}},
		},
	}
	template := map[string]any{
		"AWSTemplateFormatVersion": "2010-09-09",
		"Description":              "Dirextalk execution.v2 generic SSM-managed Linux target",
		"Parameters": map[string]any{
			"LatestAmi": map[string]any{"Type": "AWS::SSM::Parameter::Value<AWS::EC2::Image::Id>", "Default": req.Provision.AMIParameter},
		},
		"Resources": map[string]any{
			"Vpc":               map[string]any{"Type": "AWS::EC2::VPC", "Properties": map[string]any{"CidrBlock": "10.42.0.0/24", "EnableDnsHostnames": true, "EnableDnsSupport": true}},
			"InternetGateway":   map[string]any{"Type": "AWS::EC2::InternetGateway"},
			"GatewayAttachment": map[string]any{"Type": "AWS::EC2::VPCGatewayAttachment", "Properties": map[string]any{"InternetGatewayId": map[string]any{"Ref": "InternetGateway"}, "VpcId": map[string]any{"Ref": "Vpc"}}},
			"Subnet":            map[string]any{"Type": "AWS::EC2::Subnet", "Properties": map[string]any{"AvailabilityZone": req.Provision.AvailabilityZone, "CidrBlock": "10.42.0.0/25", "MapPublicIpOnLaunch": true, "VpcId": map[string]any{"Ref": "Vpc"}}},
			"RouteTable":        map[string]any{"Type": "AWS::EC2::RouteTable", "Properties": map[string]any{"VpcId": map[string]any{"Ref": "Vpc"}}},
			"DefaultRoute":      map[string]any{"Type": "AWS::EC2::Route", "DependsOn": "GatewayAttachment", "Properties": map[string]any{"DestinationCidrBlock": "0.0.0.0/0", "GatewayId": map[string]any{"Ref": "InternetGateway"}, "RouteTableId": map[string]any{"Ref": "RouteTable"}}},
			"RouteAssociation":  map[string]any{"Type": "AWS::EC2::SubnetRouteTableAssociation", "Properties": map[string]any{"RouteTableId": map[string]any{"Ref": "RouteTable"}, "SubnetId": map[string]any{"Ref": "Subnet"}}},
			"ManagementSecurityGroup": map[string]any{"Type": "AWS::EC2::SecurityGroup", "Properties": map[string]any{
				"GroupDescription": "Dirextalk execution.v2: zero inbound; HTTPS egress only", "VpcId": map[string]any{"Ref": "Vpc"},
				"SecurityGroupIngress": []any{}, "SecurityGroupEgress": []any{map[string]any{"IpProtocol": "tcp", "FromPort": 443, "ToPort": 443, "CidrIp": "0.0.0.0/0"}},
			}},
			"InstanceRole":    instanceRole,
			"InstanceProfile": map[string]any{"Type": "AWS::IAM::InstanceProfile", "Properties": map[string]any{"Roles": []any{map[string]any{"Ref": "InstanceRole"}}}},
			"Instance": map[string]any{"Type": "AWS::EC2::Instance", "DependsOn": "DefaultRoute", "Properties": map[string]any{
				"ImageId": map[string]any{"Ref": "LatestAmi"}, "InstanceType": req.Provision.InstanceType, "IamInstanceProfile": map[string]any{"Ref": "InstanceProfile"},
				"MetadataOptions":     map[string]any{"HttpEndpoint": "enabled", "HttpTokens": "required"},
				"NetworkInterfaces":   []any{map[string]any{"AssociatePublicIpAddress": true, "DeleteOnTermination": true, "DeviceIndex": "0", "GroupSet": []any{map[string]any{"Ref": "ManagementSecurityGroup"}}, "SubnetId": map[string]any{"Ref": "Subnet"}}},
				"BlockDeviceMappings": []any{map[string]any{"DeviceName": "/dev/xvda", "Ebs": map[string]any{"DeleteOnTermination": true, "Encrypted": true, "VolumeSize": req.Provision.VolumeGiB, "VolumeType": "gp3"}}},
				"UserData":            map[string]any{"Fn::Base64": "#!/bin/bash\nset -eu\nsystemctl enable amazon-ssm-agent\nsystemctl start amazon-ssm-agent\n"},
			}},
		},
		"Outputs": map[string]any{
			"InstanceId":      map[string]any{"Value": map[string]any{"Ref": "Instance"}},
			"SecurityGroupId": map[string]any{"Value": map[string]any{"Ref": "ManagementSecurityGroup"}},
			"VpcId":           map[string]any{"Value": map[string]any{"Ref": "Vpc"}},
			"SubnetId":        map[string]any{"Value": map[string]any{"Ref": "Subnet"}},
			"AmiId":           map[string]any{"Value": map[string]any{"Ref": "LatestAmi"}},
		},
	}
	raw, err := json.Marshal(template)
	if err != nil {
		return nil, "", err
	}
	digest, err := canonicalTemplateDigest(raw)
	return raw, digest, err
}

func canonicalTemplateDigest(raw []byte) (execution.Digest, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	return execution.CanonicalDigest(value)
}

func validStackARN(value, account, region, name string) bool {
	parsed, err := arn.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Service != "cloudformation" || parsed.AccountID != account || parsed.Region != region {
		return false
	}
	parts := strings.Split(parsed.Resource, "/")
	return len(parts) == 3 && parts[0] == "stack" && parts[1] == name && strings.TrimSpace(parts[2]) != ""
}

func exactProvisionTags(tags []cftypes.Tag, req CloudFormationCreateRequest) bool {
	want := map[string]string{
		"dirextalk:managed-by": "execution-v2", "dirextalk:request-digest": string(req.RequestDigest),
		"dirextalk:reservation-id": req.ReservationTargetID, "dirextalk:reservation-digest": string(req.ReservationDigest),
		"dirextalk:template-version": cloudFormationProvisionTemplateVersion,
	}
	got := map[string]string{}
	for _, tag := range tags {
		key := awsapi.ToString(tag.Key)
		if strings.HasPrefix(key, "aws:") {
			continue
		}
		if _, exists := got[key]; exists {
			return false
		}
		got[key] = awsapi.ToString(tag.Value)
	}
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

func exactStackOutputs(outputs []cftypes.Output) (map[string]string, bool) {
	want := []string{"AmiId", "InstanceId", "SecurityGroupId", "SubnetId", "VpcId"}
	got := map[string]string{}
	for _, output := range outputs {
		key, value := awsapi.ToString(output.OutputKey), awsapi.ToString(output.OutputValue)
		if key == "" || value == "" {
			return nil, false
		}
		got[key] = value
	}
	if len(got) != len(want) {
		return nil, false
	}
	sort.Strings(want)
	for _, key := range want {
		if got[key] == "" {
			return nil, false
		}
	}
	return got, true
}

func oneInstance(out *ec2.DescribeInstancesOutput) (ec2types.Instance, bool) {
	var instances []ec2types.Instance
	for _, reservation := range out.Reservations {
		instances = append(instances, reservation.Instances...)
	}
	if len(instances) != 1 {
		return ec2types.Instance{}, false
	}
	return instances[0], true
}

func validSecurityGroupID(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sg-") || len(value) < 11 || len(value) > 23 {
		return false
	}
	for _, r := range value[3:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validIAMProfileARN(value, account string) bool {
	parsed, err := arn.Parse(value)
	return err == nil && parsed.Service == "iam" && parsed.AccountID == account && strings.HasPrefix(parsed.Resource, "instance-profile/")
}

func exactManagementSecurityGroup(group ec2types.SecurityGroup, account, vpcID, groupID string) bool {
	if awsapi.ToString(group.OwnerId) != account || awsapi.ToString(group.VpcId) != vpcID || awsapi.ToString(group.GroupId) != groupID || len(group.IpPermissions) != 0 || len(group.IpPermissionsEgress) != 1 {
		return false
	}
	rule := group.IpPermissionsEgress[0]
	return awsapi.ToString(rule.IpProtocol) == "tcp" && awsapi.ToInt32(rule.FromPort) == 443 && awsapi.ToInt32(rule.ToPort) == 443 &&
		len(rule.IpRanges) == 1 && awsapi.ToString(rule.IpRanges[0].CidrIp) == "0.0.0.0/0" && len(rule.Ipv6Ranges) == 0 && len(rule.PrefixListIds) == 0 && len(rule.UserIdGroupPairs) == 0
}

// exactManagementSecurityGroupDigest is the single network-fact formula used
// by both CloudFormation completion and the last-moment SSM dispatch check.
// Keeping the validation and digest together prevents two trustworthy
// readbacks of the same security group from appearing as policy drift.
func exactManagementSecurityGroupDigest(group ec2types.SecurityGroup, account, vpcID, groupID string) (execution.Digest, bool) {
	if !exactManagementSecurityGroup(group, account, vpcID, groupID) {
		return "", false
	}
	digest, err := execution.CanonicalDigest(struct {
		GroupID string `json:"group_id"`
		VpcID   string `json:"vpc_id"`
		Ingress []any  `json:"ingress"`
		Egress  []struct {
			Protocol, CIDR string
			From, To       int32
		} `json:"egress"`
	}{GroupID: groupID, VpcID: vpcID, Ingress: []any{}, Egress: []struct {
		Protocol, CIDR string
		From, To       int32
	}{{Protocol: "tcp", CIDR: "0.0.0.0/0", From: 443, To: 443}}})
	return digest, err == nil && digest.Valid()
}

func rootVolume(instance ec2types.Instance) string {
	root := awsapi.ToString(instance.RootDeviceName)
	for _, mapping := range instance.BlockDeviceMappings {
		if awsapi.ToString(mapping.DeviceName) == root && mapping.Ebs != nil {
			return awsapi.ToString(mapping.Ebs.VolumeId)
		}
	}
	return ""
}

var _ CloudFormationProvisionProvider = (*SDKCloudFormationProvisionProvider)(nil)
