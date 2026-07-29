package ecs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	workaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/aws"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type STSClient interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}
type EC2Client interface {
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
}
type ELBClient interface {
	DescribeTargetGroups(context.Context, *elasticloadbalancingv2.DescribeTargetGroupsInput, ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error)
}
type ECSClient interface {
	DescribeClusters(context.Context, *ecs.DescribeClustersInput, ...func(*ecs.Options)) (*ecs.DescribeClustersOutput, error)
	RegisterTaskDefinition(context.Context, *ecs.RegisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.RegisterTaskDefinitionOutput, error)
	DescribeTaskDefinition(context.Context, *ecs.DescribeTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
	CreateService(context.Context, *ecs.CreateServiceInput, ...func(*ecs.Options)) (*ecs.CreateServiceOutput, error)
	UpdateService(context.Context, *ecs.UpdateServiceInput, ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error)
	DescribeServices(context.Context, *ecs.DescribeServicesInput, ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	DeleteService(context.Context, *ecs.DeleteServiceInput, ...func(*ecs.Options)) (*ecs.DeleteServiceOutput, error)
	DeregisterTaskDefinition(context.Context, *ecs.DeregisterTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DeregisterTaskDefinitionOutput, error)
	ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
}
type Clients struct {
	STS STSClient
	EC2 EC2Client
	ELB ELBClient
	ECS ECSClient
}
type Factory interface {
	New(workaws.CredentialHandle) (Clients, error)
}
type StaticFactory struct{}

func (StaticFactory) New(h workaws.CredentialHandle) (Clients, error) {
	if err := h.Validate(); err != nil {
		return Clients{}, err
	}
	cfg := aws.Config{Region: h.Region, Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(h.AccessKeyID, h.SecretAccessKey, h.SessionToken))}
	return Clients{STS: sts.NewFromConfig(cfg), EC2: ec2.NewFromConfig(cfg), ELB: elasticloadbalancingv2.NewFromConfig(cfg), ECS: ecs.NewFromConfig(cfg)}, nil
}

type Provider struct {
	factory       Factory
	creds         workaws.CredentialResolver
	secrets       workaws.SecretResolver
	timeout, poll time.Duration
	instanceID    string
}
type Option func(*Provider) error

func WithTimeout(v time.Duration) Option {
	return func(p *Provider) error {
		if v <= 0 {
			return workaws.ErrInvalid
		}
		p.timeout = v
		return nil
	}
}
func WithPollInterval(v time.Duration) Option {
	return func(p *Provider) error {
		if v <= 0 {
			return workaws.ErrInvalid
		}
		p.poll = v
		return nil
	}
}
func WithAgentInstanceID(v string) Option {
	return func(p *Provider) error {
		if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n\x00 ") {
			return workaws.ErrInvalid
		}
		p.instanceID = v
		return nil
	}
}
func NewProvider(f Factory, c workaws.CredentialResolver, s workaws.SecretResolver, opts ...Option) (*Provider, error) {
	if f == nil || c == nil {
		return nil, workaws.ErrInvalid
	}
	p := &Provider{factory: f, creds: c, secrets: s, timeout: 5 * time.Minute, poll: 500 * time.Millisecond, instanceID: "agent"}
	for _, o := range opts {
		if o == nil || o(p) != nil {
			return nil, workaws.ErrInvalid
		}
	}
	return p, nil
}

// Probe proves that one explicitly configured ECS target is usable. It only
// performs typed identity and prerequisite readbacks; it never mutates a
// cluster, service, task definition, or load balancer.
func (p *Provider) Probe(ctx context.Context, target coreworkload.TargetSettings, h workaws.CredentialHandle) error {
	if p == nil || p.factory == nil || h.Validate() != nil || target.ValidateProviderTarget(coreworkload.TargetAWSECS) != nil || h.Region != target.Region || h.AccountID != target.AccountID {
		return workaws.ErrPrecondition
	}
	clients, err := p.factory.New(h)
	if err != nil || clients.STS == nil || clients.EC2 == nil || clients.ECS == nil {
		return workaws.ErrProvider
	}
	if target.ECSTargetGroupARN != "" && clients.ELB == nil {
		return workaws.ErrProvider
	}
	plan := coreworkload.Plan{TargetKind: coreworkload.TargetAWSECS, Target: target}
	return p.verify(ctx, h, clients, plan)
}

func (p *Provider) Apply(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (coreworkload.Readback, error) {
	h, cl, e := p.prepare(ctx, plan, op)
	if e != nil {
		return coreworkload.Readback{}, e
	}
	if e = p.verify(ctx, h, cl, plan); e != nil {
		return coreworkload.Readback{}, e
	}
	td, e := p.register(ctx, cl.ECS, h, plan)
	if e != nil {
		return coreworkload.Readback{}, e
	}
	rev := strconv.FormatInt(int64(td.TaskDefinition.Revision), 10)
	if rev == "" {
		return coreworkload.Readback{}, workaws.ErrProvider
	}
	if e = p.readTaskDefinition(ctx, cl.ECS, plan.Target.ECSTaskFamily+":"+rev, plan); e != nil {
		return coreworkload.Readback{}, e
	}
	if e = p.service(ctx, cl.ECS, plan, rev, op); e != nil {
		return coreworkload.Readback{}, e
	}
	if e = p.pollService(ctx, cl.ECS, plan); e != nil {
		return coreworkload.Readback{}, e
	}
	return p.Read(ctx, plan, op)
}
func (p *Provider) Destroy(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (coreworkload.Readback, error) {
	h, cl, e := p.prepare(ctx, plan, op)
	if e != nil {
		return coreworkload.Readback{}, e
	}
	if e = p.verify(ctx, h, cl, plan); e != nil {
		return coreworkload.Readback{}, e
	}
	out, e := cl.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(plan.Target.ECSClusterARN), Services: []string{plan.Target.ECSServiceName}, Include: []ecstypes.ServiceField{ecstypes.ServiceFieldTags}})
	if e != nil || out == nil || len(out.Services) != 1 {
		return coreworkload.Readback{}, workaws.ErrPrecondition
	}
	svc := out.Services[0]
	if !owned(svc.Tags, plan, op.WorkloadID, p.instanceID) {
		return coreworkload.Readback{}, workaws.ErrPrecondition
	}
	if _, e = cl.ECS.UpdateService(ctx, &ecs.UpdateServiceInput{Cluster: aws.String(plan.Target.ECSClusterARN), Service: aws.String(plan.Target.ECSServiceName), DesiredCount: aws.Int32(0)}); e != nil {
		return coreworkload.Readback{}, workaws.ErrUncertain
	}
	if _, e = cl.ECS.DeleteService(ctx, &ecs.DeleteServiceInput{Cluster: aws.String(plan.Target.ECSClusterARN), Service: aws.String(plan.Target.ECSServiceName), Force: aws.Bool(true)}); e != nil {
		return coreworkload.Readback{}, workaws.ErrUncertain
	}
	if rev := plan.Target.Identity.TaskDefinitionRevision; rev != "" {
		if _, e = cl.ECS.DeregisterTaskDefinition(ctx, &ecs.DeregisterTaskDefinitionInput{TaskDefinition: aws.String(plan.Target.ECSTaskFamily + ":" + rev)}); e != nil {
			return coreworkload.Readback{}, workaws.ErrUncertain
		}
	}
	return p.Read(ctx, plan, op)
}
func (p *Provider) Read(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (coreworkload.Readback, error) {
	h, cl, e := p.prepare(ctx, plan, op)
	if e != nil {
		return coreworkload.Readback{}, e
	}
	if e = p.verify(ctx, h, cl, plan); e != nil {
		return coreworkload.Readback{}, e
	}
	out, e := cl.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(plan.Target.ECSClusterARN), Services: []string{plan.Target.ECSServiceName}, Include: []ecstypes.ServiceField{ecstypes.ServiceFieldTags}})
	if e != nil {
		return coreworkload.Readback{}, workaws.ErrProvider
	}
	if out == nil || len(out.Services) == 0 {
		return coreworkload.Readback{TargetKind: plan.TargetKind, WorkloadID: op.WorkloadID, State: "destroyed", Identity: plan.Target.Identity, ProviderVersion: "aws-ecs-fargate-v1", At: time.Now().UTC()}, nil
	}
	if len(out.Services) != 1 || !owned(out.Services[0].Tags, plan, op.WorkloadID, p.instanceID) {
		return coreworkload.Readback{}, workaws.ErrPrecondition
	}
	svc := out.Services[0]
	if svc.TaskDefinition == nil || svc.NetworkConfiguration == nil || svc.PlatformVersion == nil || aws.ToString(svc.PlatformVersion) != plan.Target.ECSPlatformVersion || svc.DesiredCount != int32(plan.Target.ECSDesiredCount) || svc.RunningCount > svc.DesiredCount {
		return coreworkload.Readback{}, workaws.ErrPrecondition
	}
	td, e := cl.ECS.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: svc.TaskDefinition})
	if e != nil || td == nil || td.TaskDefinition == nil || len(td.TaskDefinition.ContainerDefinitions) != 1 {
		return coreworkload.Readback{}, workaws.ErrPrecondition
	}
	want := plan.ImageURI
	if want == "" {
		want = plan.Target.ECSImageURI
	}
	if aws.ToString(td.TaskDefinition.ContainerDefinitions[0].Image) != want {
		return coreworkload.Readback{}, workaws.ErrPrecondition
	}
	list, e := cl.ECS.ListTasks(ctx, &ecs.ListTasksInput{Cluster: aws.String(plan.Target.ECSClusterARN), ServiceName: aws.String(plan.Target.ECSServiceName), DesiredStatus: ecstypes.DesiredStatusRunning})
	if e != nil || list == nil {
		return coreworkload.Readback{}, workaws.ErrProvider
	}
	if int64(len(list.TaskArns)) != plan.Target.ECSDesiredCount {
		return coreworkload.Readback{}, workaws.ErrPrecondition
	}
	if len(list.TaskArns) > 0 {
		tasks, e := cl.ECS.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: aws.String(plan.Target.ECSClusterARN), Tasks: list.TaskArns})
		if e != nil || tasks == nil || len(tasks.Tasks) != len(list.TaskArns) {
			return coreworkload.Readback{}, workaws.ErrPrecondition
		}
	}
	rev := strconv.FormatInt(int64(td.TaskDefinition.Revision), 10)
	return coreworkload.Readback{TargetKind: plan.TargetKind, WorkloadID: op.WorkloadID, State: "ready", Identity: coreworkload.TargetIdentity{Kind: plan.TargetKind, AccountID: plan.Target.AccountID, Region: plan.Target.Region, Cluster: plan.Target.ECSClusterARN, Service: plan.Target.ECSServiceName, TaskDefinitionRevision: rev, DesiredCount: plan.Target.ECSDesiredCount, ImageDigest: want}, ProviderVersion: "aws-ecs-fargate-v1", At: time.Now().UTC()}, nil
}

func (p *Provider) prepare(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (workaws.CredentialHandle, Clients, error) {
	if plan.TargetKind != coreworkload.TargetAWSECS || op.TargetKind != plan.TargetKind || plan.Digest != op.PlanDigest || plan.Revision != op.PlanRevision {
		return workaws.CredentialHandle{}, Clients{}, workaws.ErrInvalid
	}
	if err := plan.Target.ValidateCanonicalTarget(plan.TargetKind); err != nil {
		return workaws.CredentialHandle{}, Clients{}, err
	}
	ref, credentialRevision, e := workaws.CredentialReference(plan)
	if e != nil {
		return workaws.CredentialHandle{}, Clients{}, e
	}
	h, e := p.creds.ResolveCredential(ctx, ref, credentialRevision)
	if e != nil || h.Validate() != nil {
		return h, Clients{}, workaws.ErrPrecondition
	}
	if h.ReferenceID != ref || h.Revision != credentialRevision || h.Region != plan.Target.Region || h.AccountID != plan.Target.AccountID || h.Region != plan.Target.Identity.Region || h.AccountID != plan.Target.Identity.AccountID {
		return h, Clients{}, workaws.ErrPrecondition
	}
	if e = resolveSecrets(ctx, plan, p.secrets); e != nil {
		return h, Clients{}, e
	}
	cl, e := p.factory.New(h)
	if e != nil || cl.STS == nil || cl.EC2 == nil || cl.ECS == nil {
		return h, Clients{}, workaws.ErrProvider
	}
	return h, cl, nil
}
func resolveSecrets(ctx context.Context, p coreworkload.Plan, r workaws.SecretResolver) error {
	return workaws.ResolveApplicationRefs(ctx, p, r)
}
func (p *Provider) verify(ctx context.Context, h workaws.CredentialHandle, cl Clients, plan coreworkload.Plan) error {
	identity, e := cl.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if e != nil || identity == nil || aws.ToString(identity.Account) != plan.Target.AccountID || aws.ToString(identity.Arn) != h.PrincipalARN {
		return workaws.ErrPrecondition
	}
	if plan.Target.ECSClusterARN == "" || !strings.HasPrefix(plan.Target.ECSClusterARN, "arn:aws:ecs:") {
		return workaws.ErrPrecondition
	}
	clusters, e := cl.ECS.DescribeClusters(ctx, &ecs.DescribeClustersInput{Clusters: []string{plan.Target.ECSClusterARN}})
	if e != nil || clusters == nil || len(clusters.Failures) != 0 || len(clusters.Clusters) != 1 || aws.ToString(clusters.Clusters[0].ClusterArn) != plan.Target.ECSClusterARN || clusters.Clusters[0].Status == nil || aws.ToString(clusters.Clusters[0].Status) != "ACTIVE" {
		return workaws.ErrPrecondition
	}
	subs, e := cl.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: plan.Target.ECSSubnetIDs})
	if e != nil || subs == nil || aws.ToString(subs.NextToken) != "" || !sameIDs(plan.Target.ECSSubnetIDs, func() []string {
		ids := make([]string, 0, len(subs.Subnets))
		for _, subnet := range subs.Subnets {
			if subnet.State != ec2types.SubnetStateAvailable {
				return nil
			}
			ids = append(ids, aws.ToString(subnet.SubnetId))
		}
		return ids
	}()) {
		return workaws.ErrPrecondition
	}
	sg, e := cl.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: plan.Target.ECSSecurityGroupIDs})
	if e != nil || sg == nil || aws.ToString(sg.NextToken) != "" || !sameIDs(plan.Target.ECSSecurityGroupIDs, func() []string {
		ids := make([]string, 0, len(sg.SecurityGroups))
		for _, group := range sg.SecurityGroups {
			ids = append(ids, aws.ToString(group.GroupId))
		}
		return ids
	}()) {
		return workaws.ErrPrecondition
	}
	if plan.Target.ECSTargetGroupARN != "" && cl.ELB == nil {
		return workaws.ErrProvider
	}
	if plan.Target.ECSTargetGroupARN != "" && cl.ELB != nil {
		tg, e := cl.ELB.DescribeTargetGroups(ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{TargetGroupArns: []string{plan.Target.ECSTargetGroupARN}})
		if e != nil || tg == nil || aws.ToString(tg.NextMarker) != "" || len(tg.TargetGroups) != 1 || aws.ToString(tg.TargetGroups[0].TargetGroupArn) != plan.Target.ECSTargetGroupARN || tg.TargetGroups[0].TargetType != elbv2types.TargetTypeEnumIp || (tg.TargetGroups[0].Protocol != elbv2types.ProtocolEnumHttp && tg.TargetGroups[0].Protocol != elbv2types.ProtocolEnumHttps && tg.TargetGroups[0].Protocol != elbv2types.ProtocolEnumTcp && tg.TargetGroups[0].Protocol != elbv2types.ProtocolEnumTls) || (plan.Target.ECSTargetGroupPort > 0 && (tg.TargetGroups[0].Port == nil || uint32(aws.ToInt32(tg.TargetGroups[0].Port)) != plan.Target.ECSTargetGroupPort || !portConfigured(plan.Target, plan.Target.ECSTargetGroupPort))) {
			return workaws.ErrPrecondition
		}
	}
	taskDefinition := plan.Target.ECSTaskFamily
	if revision := strings.TrimSpace(plan.Target.Identity.TaskDefinitionRevision); revision != "" {
		taskDefinition += ":" + revision
	}
	td, e := cl.ECS.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(taskDefinition)})
	if e != nil || td == nil || td.TaskDefinition == nil || aws.ToString(td.TaskDefinition.Family) != plan.Target.ECSTaskFamily || td.TaskDefinition.Status != ecstypes.TaskDefinitionStatusActive {
		return workaws.ErrPrecondition
	}
	if revision := strings.TrimSpace(plan.Target.Identity.TaskDefinitionRevision); revision != "" && strconv.FormatInt(int64(td.TaskDefinition.Revision), 10) != revision {
		return workaws.ErrPrecondition
	}
	if plan.Target.ECSTargetGroupARN != "" && (len(td.TaskDefinition.ContainerDefinitions) != 1 || aws.ToString(td.TaskDefinition.ContainerDefinitions[0].Name) != "workload" || !taskDefinitionHasPort(td.TaskDefinition.ContainerDefinitions[0], plan.Target.ECSTargetGroupPort)) {
		return workaws.ErrPrecondition
	}
	return nil
}

func portConfigured(target coreworkload.TargetSettings, port uint32) bool {
	for _, p := range target.PortDetails {
		if p.Port == port {
			return true
		}
	}
	for _, p := range target.Ports {
		if p == int32(port) {
			return true
		}
	}
	return false
}

func taskDefinitionHasPort(container ecstypes.ContainerDefinition, port uint32) bool {
	if port == 0 {
		return false
	}
	for _, mapping := range container.PortMappings {
		if mapping.ContainerPort != nil && mapping.HostPort != nil && mapping.Protocol == ecstypes.TransportProtocolTcp && uint32(aws.ToInt32(mapping.ContainerPort)) == port && uint32(aws.ToInt32(mapping.HostPort)) == port {
			return true
		}
	}
	return false
}

func sameIDs(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	seen := make(map[string]struct{}, len(want))
	for _, id := range want {
		if id == "" {
			return false
		}
		seen[id] = struct{}{}
	}
	if len(seen) != len(want) {
		return false
	}
	for _, id := range got {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	gotSeen := make(map[string]struct{}, len(got))
	for _, id := range got {
		gotSeen[id] = struct{}{}
	}
	return len(gotSeen) == len(got) && len(gotSeen) == len(seen)
}
func (p *Provider) register(ctx context.Context, cl ECSClient, h workaws.CredentialHandle, plan coreworkload.Plan) (*ecs.RegisterTaskDefinitionOutput, error) {
	cpu := strconv.FormatInt(plan.ResourceLimits.CPU, 10)
	mem := strconv.FormatInt(plan.ResourceLimits.MemoryMB, 10)
	img := plan.ImageURI
	if img == "" {
		img = plan.Target.ECSImageURI
	}
	if !strings.Contains(img, "@sha256:") {
		return nil, workaws.ErrInvalid
	}
	secrets, err := p.resolveContainerSecrets(ctx, plan)
	if err != nil {
		return nil, err
	}
	ports := make([]ecstypes.PortMapping, 0, len(plan.Target.PortDetails))
	for _, port := range plan.Target.PortDetails {
		ports = append(ports, ecstypes.PortMapping{ContainerPort: aws.Int32(int32(port.Port)), HostPort: aws.Int32(int32(port.Port)), Protocol: ecstypes.TransportProtocolTcp})
	}
	if len(ports) == 0 {
		for _, port := range plan.Target.Ports {
			ports = append(ports, ecstypes.PortMapping{ContainerPort: aws.Int32(port), HostPort: aws.Int32(port), Protocol: ecstypes.TransportProtocolTcp})
		}
	}
	in := &ecs.RegisterTaskDefinitionInput{Family: aws.String(plan.Target.ECSTaskFamily), Cpu: aws.String(cpu), Memory: aws.String(mem), NetworkMode: ecstypes.NetworkModeAwsvpc, RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate}, ExecutionRoleArn: aws.String(plan.Target.ECSExecutionRoleARN), TaskRoleArn: aws.String(plan.Target.ECSTaskRoleARN), ContainerDefinitions: []ecstypes.ContainerDefinition{{Name: aws.String("workload"), Image: aws.String(img), Essential: aws.Bool(true), PortMappings: ports, Secrets: secrets}}}
	out, e := cl.RegisterTaskDefinition(ctx, in)
	if e != nil || out == nil || out.TaskDefinition == nil {
		return nil, workaws.ErrUncertain
	}
	return out, nil
}

func (p *Provider) resolveContainerSecrets(ctx context.Context, plan coreworkload.Plan) ([]ecstypes.Secret, error) {
	secrets := make([]ecstypes.Secret, 0)
	for _, grant := range plan.SecretGrantRefs {
		if string(grant.Purpose) == "AWS_CREDENTIAL" || string(grant.Purpose) == "aws_credential" {
			continue
		}
		if p.secrets == nil {
			return nil, workaws.ErrPrecondition
		}
		arn, err := p.secrets.ResolveSecretReference(ctx, grant.ReferenceID)
		if err != nil || !(strings.HasPrefix(arn, "arn:aws:secretsmanager:") || strings.HasPrefix(arn, "arn:aws:ssm:")) {
			return nil, workaws.ErrPrecondition
		}
		secrets = append(secrets, ecstypes.Secret{Name: aws.String(grant.ReferenceID), ValueFrom: aws.String(arn)})
	}
	return secrets, nil
}
func (p *Provider) readTaskDefinition(ctx context.Context, cl ECSClient, ref string, plan coreworkload.Plan) error {
	out, e := cl.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(ref)})
	if e != nil || out == nil || out.TaskDefinition == nil || len(out.TaskDefinition.ContainerDefinitions) != 1 {
		return workaws.ErrPrecondition
	}
	img := aws.ToString(out.TaskDefinition.ContainerDefinitions[0].Image)
	want := plan.ImageURI
	if want == "" {
		want = plan.Target.ECSImageURI
	}
	if img != want {
		return workaws.ErrPrecondition
	}
	return nil
}
func (p *Provider) service(ctx context.Context, cl ECSClient, plan coreworkload.Plan, rev string, op coreworkload.Operation) error {
	count := int32(plan.Target.ECSDesiredCount)
	if count < 1 {
		count = 1
	}
	net := &ecstypes.NetworkConfiguration{AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{Subnets: plan.Target.ECSSubnetIDs, SecurityGroups: plan.Target.ECSSecurityGroupIDs, AssignPublicIp: map[bool]ecstypes.AssignPublicIp{true: ecstypes.AssignPublicIpEnabled, false: ecstypes.AssignPublicIpDisabled}[plan.Target.ECSAssignPublicIP]}}
	lb := []ecstypes.LoadBalancer{}
	if plan.Target.ECSTargetGroupARN != "" {
		lb = []ecstypes.LoadBalancer{{TargetGroupArn: aws.String(plan.Target.ECSTargetGroupARN), ContainerName: aws.String("workload"), ContainerPort: aws.Int32(int32(plan.Target.ECSTargetGroupPort))}}
	}
	tags := []ecstypes.Tag{{Key: aws.String("dirextalk-agent-instance"), Value: aws.String(p.instanceID)}, {Key: aws.String("dirextalk-agent-workload"), Value: aws.String(op.WorkloadID)}, {Key: aws.String("dirextalk-agent-plan"), Value: aws.String(plan.Digest)}}
	out, e := cl.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(plan.Target.ECSClusterARN), Services: []string{plan.Target.ECSServiceName}, Include: []ecstypes.ServiceField{ecstypes.ServiceFieldTags}})
	if e != nil {
		return e
	}
	if out != nil && len(out.Services) > 0 {
		if !owned(out.Services[0].Tags, plan, op.WorkloadID, p.instanceID) {
			return workaws.ErrPrecondition
		}
		_, e = cl.UpdateService(ctx, &ecs.UpdateServiceInput{Cluster: aws.String(plan.Target.ECSClusterARN), Service: aws.String(plan.Target.ECSServiceName), TaskDefinition: aws.String(plan.Target.ECSTaskFamily + ":" + rev), DesiredCount: aws.Int32(count), NetworkConfiguration: net, PlatformVersion: aws.String(plan.Target.ECSPlatformVersion), LoadBalancers: lb})
		if e != nil {
			return workaws.ErrUncertain
		}
		return nil
	}
	_, e = cl.CreateService(ctx, &ecs.CreateServiceInput{Cluster: aws.String(plan.Target.ECSClusterARN), ServiceName: aws.String(plan.Target.ECSServiceName), TaskDefinition: aws.String(plan.Target.ECSTaskFamily + ":" + rev), DesiredCount: aws.Int32(count), LaunchType: ecstypes.LaunchTypeFargate, PlatformVersion: aws.String(plan.Target.ECSPlatformVersion), NetworkConfiguration: net, LoadBalancers: lb, ClientToken: aws.String(op.ID), Tags: tags})
	if e != nil {
		return workaws.ErrUncertain
	}
	return nil
}
func (p *Provider) pollService(ctx context.Context, cl ECSClient, plan coreworkload.Plan) error {
	deadline := time.Now().Add(p.timeout)
	for {
		if time.Now().After(deadline) {
			return workaws.ErrUncertain
		}
		out, e := cl.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(plan.Target.ECSClusterARN), Services: []string{plan.Target.ECSServiceName}})
		if e == nil && out != nil && len(out.Services) == 1 && out.Services[0].RunningCount == int32(plan.Target.ECSDesiredCount) && out.Services[0].PendingCount == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return workaws.ErrUncertain
		case <-time.After(p.poll):
		}
	}
}

func owned(tags []ecstypes.Tag, plan coreworkload.Plan, workloadID, instanceID string) bool {
	hasWorkload, hasPlan, hasInstance := false, false, instanceID == ""
	for _, t := range tags {
		switch aws.ToString(t.Key) {
		case "dirextalk-agent-plan":
			hasPlan = aws.ToString(t.Value) == plan.Digest
		case "dirextalk-agent-workload":
			hasWorkload = workloadID == "" || aws.ToString(t.Value) == workloadID
		case "dirextalk-agent-instance":
			hasInstance = instanceID == "" || aws.ToString(t.Value) == instanceID
		}
	}
	return hasWorkload && hasPlan && hasInstance
}
func (p *Provider) readback(ctx context.Context, cl ECSClient, plan coreworkload.Plan, op coreworkload.Operation, rev, state string) (coreworkload.Readback, error) {
	return coreworkload.Readback{TargetKind: plan.TargetKind, WorkloadID: op.WorkloadID, State: state, Identity: coreworkload.TargetIdentity{Kind: plan.TargetKind, AccountID: plan.Target.AccountID, Region: plan.Target.Region, Cluster: plan.Target.ECSClusterARN, Service: plan.Target.ECSServiceName, TaskDefinitionRevision: rev, DesiredCount: plan.Target.ECSDesiredCount, ImageDigest: plan.ImageURI}, ProviderVersion: "aws-ecs-fargate-v1", At: time.Now().UTC()}, nil
}

var _ coreworkload.Provider = (*Provider)(nil)
var _ = fmt.Sprintf
var _ = ec2types.Subnet{}
var _ = elbv2types.TargetGroup{}
