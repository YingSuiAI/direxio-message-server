package ssm

import (
	"context"
	"errors"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	workaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/aws"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/uuid"
)

type readinessFactory struct {
	clients Clients
	err     error
}

func (f readinessFactory) New(workaws.CredentialHandle) (Clients, error) { return f.clients, f.err }

type readinessResolver struct{}

func (readinessResolver) ResolveCredential(context.Context, string, int64) (workaws.CredentialHandle, error) {
	return workaws.CredentialHandle{}, errors.New("unused")
}

type readinessSTS struct {
	account string
	arn     string
	err     error
}

func (f readinessSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account), Arn: aws.String(f.arn)}, nil
}

type readinessEC2 struct {
	instance  string
	tags      []ec2types.Tag
	publicIP  string
	groups    []string
	platform  string
	nextToken string
	err       error
}

func (f readinessEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	platform := f.platform
	if platform == "" {
		platform = "Linux/UNIX"
	}
	groups := make([]ec2types.GroupIdentifier, 0, len(f.groups))
	for _, id := range f.groups {
		groups = append(groups, ec2types.GroupIdentifier{GroupId: aws.String(id)})
	}
	return &ec2.DescribeInstancesOutput{NextToken: aws.String(f.nextToken), Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{InstanceId: aws.String(f.instance), PublicIpAddress: aws.String(f.publicIP), SecurityGroups: groups, PlatformDetails: aws.String(platform), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}, Tags: f.tags}}}}}, nil
}

type readinessSSM struct {
	instance  string
	nextToken string
	err       error
}

func (f readinessSSM) DescribeInstanceInformation(context.Context, *ssm.DescribeInstanceInformationInput, ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &ssm.DescribeInstanceInformationOutput{NextToken: aws.String(f.nextToken), InstanceInformationList: []ssmtypes.InstanceInformation{{InstanceId: aws.String(f.instance), PingStatus: ssmtypes.PingStatusOnline}}}, nil
}
func (readinessSSM) SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	return nil, errors.New("unused")
}
func (readinessSSM) GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	return nil, errors.New("unused")
}

func readinessCredential() workaws.CredentialHandle {
	return workaws.CredentialHandle{ReferenceID: uuid.NewString(), Revision: 1, Region: "ap-northeast-3", AccountID: "123456789012", PrincipalARN: "arn:aws:iam::123456789012:role/agent", AccessKeyID: "access", SecretAccessKey: "secret"}
}
func readinessTarget() coreworkload.TargetSettings {
	return coreworkload.TargetSettings{Region: "ap-northeast-3", AccountID: "123456789012", InstanceID: "i-0123456789abcdef0", Identity: coreworkload.TargetIdentity{Kind: coreworkload.TargetAWSEC2SSM, Region: "ap-northeast-3", AccountID: "123456789012", InstanceID: "i-0123456789abcdef0", Endpoint: "http://192.0.2.10"}, NetworkGrantDetails: []coreworkload.NetworkGrant{{ReferenceID: "sg-0123456789abcdef0", Kind: "aws_security_group"}}, EC2DocumentVersion: "1", EC2SystemdService: "dirextalk-agent.service", RequiredInstanceTags: map[string]string{"managed": "true"}}
}
func readinessClients(h workaws.CredentialHandle, target coreworkload.TargetSettings) Clients {
	return Clients{STS: readinessSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: readinessEC2{instance: target.InstanceID, publicIP: "192.0.2.10", groups: []string{"sg-0123456789abcdef0"}, tags: []ec2types.Tag{{Key: aws.String("managed"), Value: aws.String("true")}}, platform: "Linux/UNIX"}, SSM: readinessSSM{instance: target.InstanceID}}
}

func TestProbeRequiresExplicitFreshTargetProof(t *testing.T) {
	h := readinessCredential()
	target := readinessTarget()
	cases := []struct {
		name    string
		target  coreworkload.TargetSettings
		h       workaws.CredentialHandle
		factory readinessFactory
		want    error
	}{
		{name: "ready", target: target, h: h, factory: readinessFactory{clients: readinessClients(h, target)}},
		{name: "partial-target", target: func() coreworkload.TargetSettings { v := target; v.EC2SystemdService = ""; return v }(), h: h, factory: readinessFactory{clients: readinessClients(h, target)}, want: workaws.ErrPrecondition},
		{name: "stale-account", target: target, h: func() workaws.CredentialHandle { v := h; v.AccountID = "999999999999"; return v }(), factory: readinessFactory{clients: readinessClients(h, target)}, want: workaws.ErrPrecondition},
		{name: "provider-error", target: target, h: h, factory: readinessFactory{err: errors.New("offline")}, want: workaws.ErrProvider},
		{name: "wrong-instance", target: target, h: h, factory: readinessFactory{clients: Clients{STS: readinessSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: readinessEC2{instance: "i-other", publicIP: "192.0.2.10", groups: []string{"sg-0123456789abcdef0"}, tags: []ec2types.Tag{{Key: aws.String("managed"), Value: aws.String("true")}}}, SSM: readinessSSM{instance: target.InstanceID}}}, want: workaws.ErrPrecondition},
		{name: "windows", target: target, h: h, factory: readinessFactory{clients: Clients{STS: readinessSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: readinessEC2{instance: target.InstanceID, publicIP: "192.0.2.10", groups: []string{"sg-0123456789abcdef0"}, platform: "Windows", tags: []ec2types.Tag{{Key: aws.String("managed"), Value: aws.String("true")}}}, SSM: readinessSSM{instance: target.InstanceID}}}, want: workaws.ErrPrecondition},
		{name: "ec2-pagination", target: target, h: h, factory: readinessFactory{clients: Clients{STS: readinessSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: readinessEC2{instance: target.InstanceID, publicIP: "192.0.2.10", groups: []string{"sg-0123456789abcdef0"}, nextToken: "next", tags: []ec2types.Tag{{Key: aws.String("managed"), Value: aws.String("true")}}}, SSM: readinessSSM{instance: target.InstanceID}}}, want: workaws.ErrPrecondition},
		{name: "ssm-pagination", target: target, h: h, factory: readinessFactory{clients: Clients{STS: readinessSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: readinessEC2{instance: target.InstanceID, publicIP: "192.0.2.10", groups: []string{"sg-0123456789abcdef0"}, tags: []ec2types.Tag{{Key: aws.String("managed"), Value: aws.String("true")}}}, SSM: readinessSSM{instance: target.InstanceID, nextToken: "next"}}}, want: workaws.ErrPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, e := NewProvider(tc.factory, readinessResolver{}, nil)
			if e != nil {
				t.Fatal(e)
			}
			e = p.Probe(context.Background(), tc.target, tc.h)
			if tc.want == nil {
				if e != nil {
					t.Fatalf("probe failed: %v", e)
				}
				return
			}
			if !errors.Is(e, tc.want) {
				t.Fatalf("error=%v want=%v", e, tc.want)
			}
		})
	}
}
