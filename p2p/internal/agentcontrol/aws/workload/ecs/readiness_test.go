package ecs

import (
	"context"
	"errors"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	workaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/aws"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/uuid"
)

type probeFactory struct {
	clients Clients
	err     error
}

func (f probeFactory) New(workaws.CredentialHandle) (Clients, error) { return f.clients, f.err }

type probeResolver struct{}

func (probeResolver) ResolveCredential(context.Context, string, int64) (workaws.CredentialHandle, error) {
	return workaws.CredentialHandle{}, errors.New("unused")
}

type probeSTS struct{ account, arn string }

func (f probeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account), Arn: aws.String(f.arn)}, nil
}

type probeEC2 struct {
	err            error
	subnetID       string
	groupID        string
	unusable       bool
	nextToken      string
	groupNextToken string
}

func (f probeEC2) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	id := f.subnetID
	if id == "" {
		id = "subnet-0123456789abcdef0"
	}
	state := ec2types.SubnetStateAvailable
	if f.unusable {
		state = ec2types.SubnetStatePending
	}
	return &ec2.DescribeSubnetsOutput{NextToken: aws.String(f.nextToken), Subnets: []ec2types.Subnet{{SubnetId: aws.String(id), State: state}}}, nil
}
func (f probeEC2) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	id := f.groupID
	if id == "" {
		id = "sg-0123456789abcdef0"
	}
	nextToken := f.groupNextToken
	if nextToken == "" {
		nextToken = f.nextToken
	}
	return &ec2.DescribeSecurityGroupsOutput{NextToken: aws.String(nextToken), SecurityGroups: []ec2types.SecurityGroup{{GroupId: aws.String(id)}}}, nil
}

type probeECS struct {
	clusterErr, taskErr error
	clusterARN          string
	taskFamily          string
	taskRevision        int32
	containerPorts      []int32
	containerProtocol   ecstypes.TransportProtocol
}

type probeELB struct {
	arn        string
	port       int32
	nextMarker string
	targetType elbtypes.TargetTypeEnum
	protocol   elbtypes.ProtocolEnum
}

func (f probeELB) DescribeTargetGroups(context.Context, *elasticloadbalancingv2.DescribeTargetGroupsInput, ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error) {
	targetType := f.targetType
	if targetType == "" {
		targetType = elbtypes.TargetTypeEnumIp
	}
	protocol := f.protocol
	if protocol == "" {
		protocol = elbtypes.ProtocolEnumTcp
	}
	return &elasticloadbalancingv2.DescribeTargetGroupsOutput{NextMarker: aws.String(f.nextMarker), TargetGroups: []elbtypes.TargetGroup{{TargetGroupArn: aws.String(f.arn), Port: aws.Int32(f.port), TargetType: targetType, Protocol: protocol}}}, nil
}

func (f probeECS) DescribeClusters(context.Context, *ecs.DescribeClustersInput, ...func(*ecs.Options)) (*ecs.DescribeClustersOutput, error) {
	if f.clusterErr != nil {
		return nil, f.clusterErr
	}
	return &ecs.DescribeClustersOutput{Clusters: []ecstypes.Cluster{{ClusterArn: aws.String(f.clusterARN), Status: aws.String("ACTIVE")}}}, nil
}
func (f probeECS) DescribeTaskDefinition(context.Context, *ecs.DescribeTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	if f.taskErr != nil {
		return nil, f.taskErr
	}
	revision := f.taskRevision
	if revision == 0 {
		revision = 1
	}
	definition := &ecstypes.TaskDefinition{Family: aws.String(f.taskFamily), Revision: revision, Status: ecstypes.TaskDefinitionStatusActive}
	if len(f.containerPorts) > 0 {
		mappings := make([]ecstypes.PortMapping, 0, len(f.containerPorts))
		protocol := f.containerProtocol
		if protocol == "" {
			protocol = ecstypes.TransportProtocolTcp
		}
		for _, port := range f.containerPorts {
			mappings = append(mappings, ecstypes.PortMapping{ContainerPort: aws.Int32(port), HostPort: aws.Int32(port), Protocol: protocol})
		}
		definition.ContainerDefinitions = []ecstypes.ContainerDefinition{{Name: aws.String("workload"), PortMappings: mappings}}
	}
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: definition}, nil
}
func (probeECS) RegisterTaskDefinition(context.Context, *ecs.RegisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error) {
	return nil, errors.New("unused")
}
func (probeECS) CreateService(context.Context, *ecs.CreateServiceInput, ...func(*ecs.Options)) (*ecs.CreateServiceOutput, error) {
	return nil, errors.New("unused")
}
func (probeECS) UpdateService(context.Context, *ecs.UpdateServiceInput, ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error) {
	return nil, errors.New("unused")
}
func (probeECS) DescribeServices(context.Context, *ecs.DescribeServicesInput, ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	return nil, errors.New("unused")
}
func (probeECS) DeleteService(context.Context, *ecs.DeleteServiceInput, ...func(*ecs.Options)) (*ecs.DeleteServiceOutput, error) {
	return nil, errors.New("unused")
}
func (probeECS) DeregisterTaskDefinition(context.Context, *ecs.DeregisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error) {
	return nil, errors.New("unused")
}
func (probeECS) ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	return nil, errors.New("unused")
}
func (probeECS) DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	return nil, errors.New("unused")
}

func probeCredential() workaws.CredentialHandle {
	return workaws.CredentialHandle{ReferenceID: uuid.NewString(), Revision: 1, Region: "ap-northeast-3", AccountID: "123456789012", PrincipalARN: "arn:aws:iam::123456789012:role/agent", AccessKeyID: "access", SecretAccessKey: "secret"}
}
func probeTarget() coreworkload.TargetSettings {
	return coreworkload.TargetSettings{Region: "ap-northeast-3", AccountID: "123456789012", Identity: coreworkload.TargetIdentity{Kind: coreworkload.TargetAWSECS, Region: "ap-northeast-3", AccountID: "123456789012", Cluster: "arn:aws:ecs:ap-northeast-3:123456789012:cluster/agent", Service: "agent", TaskDefinitionRevision: "1"}, ECSClusterARN: "arn:aws:ecs:ap-northeast-3:123456789012:cluster/agent", ECSServiceName: "agent", ECSTaskFamily: "agent", ECSPlatformVersion: "1.4.0", ECSSubnetIDs: []string{"subnet-0123456789abcdef0"}, ECSSecurityGroupIDs: []string{"sg-0123456789abcdef0"}, ECSDesiredCount: 1}
}

func TestProbeRequiresExactClusterAndTaskPrerequisites(t *testing.T) {
	h := probeCredential()
	target := probeTarget()
	clients := Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily}}
	targetGroup := func() coreworkload.TargetSettings {
		v := target
		v.ECSTargetGroupARN = "arn:aws:elasticloadbalancing:ap-northeast-3:123456789012:targetgroup/agent/right"
		v.ECSTargetGroupPort = 8080
		v.Ports = []int32{8080}
		return v
	}
	cases := []struct {
		name    string
		target  coreworkload.TargetSettings
		factory probeFactory
		want    error
	}{
		{name: "ready", target: target, factory: probeFactory{clients: clients}},
		{name: "partial-target", target: func() coreworkload.TargetSettings { v := target; v.ECSSubnetIDs = nil; return v }(), factory: probeFactory{clients: clients}, want: workaws.ErrPrecondition},
		{name: "stale-cluster", target: target, factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: "arn:aws:ecs:ap-northeast-3:123456789012:cluster/other", taskFamily: target.ECSTaskFamily}}}, want: workaws.ErrPrecondition},
		{name: "stale-task-family", target: target, factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: "other"}}}, want: workaws.ErrPrecondition},
		{name: "stale-task-revision", target: func() coreworkload.TargetSettings { v := target; v.Identity.TaskDefinitionRevision = "2"; return v }(), factory: probeFactory{clients: clients}, want: workaws.ErrPrecondition},
		{name: "stale-network", target: target, factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{subnetID: "subnet-other", groupID: "sg-other"}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily}}}, want: workaws.ErrPrecondition},
		{name: "duplicate-network", target: func() coreworkload.TargetSettings {
			v := target
			v.ECSSubnetIDs = append(v.ECSSubnetIDs, v.ECSSubnetIDs[0])
			return v
		}(), factory: probeFactory{clients: clients}, want: workaws.ErrPrecondition},
		{name: "unusable-subnet", target: target, factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{unusable: true}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily}}}, want: workaws.ErrPrecondition},
		{name: "stale-target-group", target: func() coreworkload.TargetSettings {
			v := target
			v.ECSTargetGroupARN = "arn:aws:elasticloadbalancing:ap-northeast-3:123456789012:targetgroup/agent/other"
			v.ECSTargetGroupPort = 8080
			return v
		}(), factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily}, ELB: probeELB{arn: "arn:aws:elasticloadbalancing:ap-northeast-3:123456789012:targetgroup/agent/right", port: 8080}}}, want: workaws.ErrPrecondition},
		{name: "pagination", target: target, factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{nextToken: "next"}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily}}}, want: workaws.ErrPrecondition},
		{name: "security-group-pagination", target: target, factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{groupNextToken: "next"}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily}}}, want: workaws.ErrPrecondition},
		{name: "target-group-ready", target: targetGroup(), factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily, containerPorts: []int32{8080}}, ELB: probeELB{arn: "arn:aws:elasticloadbalancing:ap-northeast-3:123456789012:targetgroup/agent/right", port: 8080}}}},
		{name: "target-group-type", target: targetGroup(), factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily, containerPorts: []int32{8080}}, ELB: probeELB{arn: "arn:aws:elasticloadbalancing:ap-northeast-3:123456789012:targetgroup/agent/right", port: 8080, targetType: elbtypes.TargetTypeEnumInstance}}}, want: workaws.ErrPrecondition},
		{name: "target-group-protocol", target: targetGroup(), factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily, containerPorts: []int32{8080}}, ELB: probeELB{arn: "arn:aws:elasticloadbalancing:ap-northeast-3:123456789012:targetgroup/agent/right", port: 8080, protocol: elbtypes.ProtocolEnumUdp}}}, want: workaws.ErrPrecondition},
		{name: "target-group-port", target: targetGroup(), factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily, containerPorts: []int32{9090}}, ELB: probeELB{arn: "arn:aws:elasticloadbalancing:ap-northeast-3:123456789012:targetgroup/agent/right", port: 8080}}}, want: workaws.ErrPrecondition},
		{name: "target-group-udp-binding", target: targetGroup(), factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily, containerPorts: []int32{8080}, containerProtocol: ecstypes.TransportProtocolUdp}, ELB: probeELB{arn: "arn:aws:elasticloadbalancing:ap-northeast-3:123456789012:targetgroup/agent/right", port: 8080}}}, want: workaws.ErrPrecondition},
		{name: "target-group-pagination", target: targetGroup(), factory: probeFactory{clients: Clients{STS: probeSTS{account: h.AccountID, arn: h.PrincipalARN}, EC2: probeEC2{}, ECS: probeECS{clusterARN: target.ECSClusterARN, taskFamily: target.ECSTaskFamily, containerPorts: []int32{8080}}, ELB: probeELB{arn: "arn:aws:elasticloadbalancing:ap-northeast-3:123456789012:targetgroup/agent/right", port: 8080, nextMarker: "next"}}}, want: workaws.ErrPrecondition},
		{name: "provider-error", target: target, factory: probeFactory{err: errors.New("offline")}, want: workaws.ErrProvider},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewProvider(tc.factory, probeResolver{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = p.Probe(context.Background(), tc.target, h)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("probe failed: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
}
