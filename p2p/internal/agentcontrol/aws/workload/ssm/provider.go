package ssm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	workaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/aws"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type STSClient interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}
type EC2Client interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}
type SSMClient interface {
	DescribeInstanceInformation(context.Context, *ssm.DescribeInstanceInformationInput, ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error)
	SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}
type Clients struct {
	STS STSClient
	EC2 EC2Client
	SSM SSMClient
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
	return Clients{STS: sts.NewFromConfig(cfg), EC2: ec2.NewFromConfig(cfg), SSM: ssm.NewFromConfig(cfg)}, nil
}

type Provider struct {
	factory       Factory
	creds         workaws.CredentialResolver
	secrets       workaws.SecretResolver
	timeout, poll time.Duration
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
func NewProvider(factory Factory, creds workaws.CredentialResolver, secrets workaws.SecretResolver, opts ...Option) (*Provider, error) {
	if factory == nil || creds == nil {
		return nil, workaws.ErrInvalid
	}
	p := &Provider{factory: factory, creds: creds, secrets: secrets, timeout: 2 * time.Minute, poll: 250 * time.Millisecond}
	for _, o := range opts {
		if o == nil || o(p) != nil {
			return nil, workaws.ErrInvalid
		}
	}
	return p, nil
}

// Probe proves that one explicitly configured SSM target is usable. It is a
// startup/readiness check only: no command is submitted and no secret value is
// returned. Callers must provide the exact durable credential handle and target
// binding; there is no default-target or account-wide scan.
func (p *Provider) Probe(ctx context.Context, target coreworkload.TargetSettings, h workaws.CredentialHandle) error {
	if p == nil || p.factory == nil || h.Validate() != nil || target.ValidateProviderTarget(coreworkload.TargetAWSEC2SSM) != nil || h.Region != target.Region || h.AccountID != target.AccountID {
		return workaws.ErrPrecondition
	}
	clients, err := p.factory.New(h)
	if err != nil || clients.STS == nil || clients.EC2 == nil || clients.SSM == nil {
		return workaws.ErrProvider
	}
	plan := coreworkload.Plan{TargetKind: coreworkload.TargetAWSEC2SSM, Target: target}
	return p.verify(ctx, h, clients, plan)
}

func (p *Provider) Apply(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (coreworkload.Readback, error) {
	h, clients, err := p.prepare(ctx, plan, op)
	if err != nil {
		return coreworkload.Readback{}, err
	}
	if err = p.verify(ctx, h, clients, plan); err != nil {
		return coreworkload.Readback{}, err
	}
	if len(plan.CommandSteps) == 0 {
		return coreworkload.Readback{}, workaws.ErrInvalid
	}
	if _, err = p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, plan.CommandSteps, "apply"); err != nil {
		return coreworkload.Readback{}, err
	}
	if _, err = p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, []string{activeProbe(plan.Target.EC2SystemdService)}, "ready"); err != nil {
		return coreworkload.Readback{}, err
	}
	return p.readback(ctx, clients, plan, op, "ready")
}
func (p *Provider) Destroy(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (coreworkload.Readback, error) {
	h, clients, err := p.prepare(ctx, plan, op)
	if err != nil {
		return coreworkload.Readback{}, err
	}
	if err = p.verify(ctx, h, clients, plan); err != nil {
		return coreworkload.Readback{}, err
	}
	if _, err = p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, []string{"systemctl stop " + plan.Target.EC2SystemdService, "systemctl disable " + plan.Target.EC2SystemdService}, "destroy"); err != nil {
		return coreworkload.Readback{}, err
	}
	if _, err = p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, []string{inactiveProbe(plan.Target.EC2SystemdService)}, "destroy-ready"); err != nil {
		return coreworkload.Readback{}, err
	}
	return p.readback(ctx, clients, plan, op, "destroyed")
}
func (p *Provider) Read(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (coreworkload.Readback, error) {
	h, clients, err := p.prepare(ctx, plan, op)
	if err != nil {
		return coreworkload.Readback{}, err
	}
	if err = p.verify(ctx, h, clients, plan); err != nil {
		return coreworkload.Readback{}, err
	}
	state := "ready"
	if _, err = p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, []string{activeProbe(plan.Target.EC2SystemdService)}, "read"); err != nil {
		state = "destroyed"
		if _, e := p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, []string{inactiveProbe(plan.Target.EC2SystemdService)}, "read"); e != nil {
			return coreworkload.Readback{}, err
		}
	}
	return p.readback(ctx, clients, plan, op, state)
}
func (p *Provider) prepare(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (workaws.CredentialHandle, Clients, error) {
	if plan.TargetKind != coreworkload.TargetAWSEC2SSM || op.TargetKind != plan.TargetKind || plan.Digest != op.PlanDigest || plan.Revision != op.PlanRevision {
		return workaws.CredentialHandle{}, Clients{}, workaws.ErrInvalid
	}
	if err := plan.Target.ValidateCanonicalTarget(plan.TargetKind); err != nil {
		return workaws.CredentialHandle{}, Clients{}, err
	}
	ref, credentialRevision, err := workaws.CredentialReference(plan)
	if err != nil {
		return workaws.CredentialHandle{}, Clients{}, err
	}
	h, err := p.creds.ResolveCredential(ctx, ref, credentialRevision)
	if err != nil {
		return h, Clients{}, workaws.ErrPrecondition
	}
	if err = h.Validate(); err != nil {
		return h, Clients{}, err
	}
	if h.ReferenceID != ref || h.Revision != credentialRevision || h.Region != plan.Target.Region || h.AccountID != plan.Target.AccountID || h.Region != plan.Target.Identity.Region || h.AccountID != plan.Target.Identity.AccountID {
		return h, Clients{}, workaws.ErrPrecondition
	}
	if err = workawsResolve(ctx, plan, p.secrets); err != nil {
		return h, Clients{}, err
	}
	cl, err := p.factory.New(h)
	if err != nil {
		return h, Clients{}, workaws.ErrProvider
	}
	if cl.STS == nil || cl.EC2 == nil || cl.SSM == nil {
		return h, Clients{}, workaws.ErrProvider
	}
	return h, cl, nil
}
func workawsResolve(ctx context.Context, p coreworkload.Plan, r workaws.SecretResolver) error {
	return workaws.ResolveApplicationRefs(ctx, p, r)
}

func (p *Provider) verify(ctx context.Context, h workaws.CredentialHandle, cl Clients, plan coreworkload.Plan) error {
	identity, e := cl.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if e != nil || identity == nil || aws.ToString(identity.Account) != plan.Target.AccountID || aws.ToString(identity.Arn) != h.PrincipalARN {
		return workaws.ErrPrecondition
	}
	out, e := cl.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{plan.Target.InstanceID}})
	if e != nil || out == nil || aws.ToString(out.NextToken) != "" || len(out.Reservations) != 1 || len(out.Reservations[0].Instances) != 1 {
		return workaws.ErrPrecondition
	}
	in := out.Reservations[0].Instances[0]
	if in.InstanceId == nil || aws.ToString(in.InstanceId) != plan.Target.InstanceID || in.State == nil || in.State.Name != ec2types.InstanceStateNameRunning || aws.ToString(in.PlatformDetails) != "Linux/UNIX" {
		return workaws.ErrPrecondition
	}
	tags := map[string]string{}
	for _, t := range in.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	for k, v := range plan.Target.RequiredInstanceTags {
		if tags[k] != v {
			return workaws.ErrPrecondition
		}
	}
	si, e := cl.SSM.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("InstanceIds"), Values: []string{plan.Target.InstanceID}}}})
	if e != nil || si == nil || aws.ToString(si.NextToken) != "" || len(si.InstanceInformationList) != 1 || aws.ToString(si.InstanceInformationList[0].InstanceId) != plan.Target.InstanceID || si.InstanceInformationList[0].PingStatus != ssmtypes.PingStatusOnline {
		return workaws.ErrPrecondition
	}
	return nil
}
func (p *Provider) command(ctx context.Context, cl SSMClient, instance string, plan coreworkload.Plan, commands []string, kind string) (string, error) {
	ver, err := strconv.ParseUint(plan.Target.EC2DocumentVersion, 10, 64)
	if err != nil || ver == 0 {
		return "", workaws.ErrInvalid
	}
	_ = ver
	comment := DeterministicComment(plan.Digest, kind, instance)
	out, err := cl.SendCommand(ctx, &ssm.SendCommandInput{DocumentName: aws.String("AWS-RunShellScript"), DocumentVersion: aws.String(plan.Target.EC2DocumentVersion), Comment: aws.String(comment), InstanceIds: []string{instance}, Parameters: map[string][]string{"commands": commands}})
	if err != nil || out == nil || out.Command == nil || out.Command.CommandId == nil {
		return "", workaws.ErrUncertain
	}
	id := aws.ToString(out.Command.CommandId)
	deadline := time.Now().Add(p.timeout)
	for {
		if time.Now().After(deadline) {
			return id, workaws.ErrUncertain
		}
		inv, e := cl.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{CommandId: aws.String(id), InstanceId: aws.String(instance)})
		if e == nil && inv != nil {
			switch inv.Status {
			case ssmtypes.CommandInvocationStatusSuccess:
				return id, nil
			case ssmtypes.CommandInvocationStatusFailed, ssmtypes.CommandInvocationStatusCancelled, ssmtypes.CommandInvocationStatusTimedOut:
				return id, workaws.ErrProvider
			}
		}
		select {
		case <-ctx.Done():
			return id, workaws.ErrUncertain
		case <-time.After(p.poll):
		}
	}
}
func activeProbe(s string) string   { return fmt.Sprintf("systemctl is-active --quiet %s", s) }
func inactiveProbe(s string) string { return fmt.Sprintf("systemctl is-inactive --quiet %s", s) }

// DeterministicComment is safe to expose in acceptance tests and read-only
// reconciliation tooling; it contains no command or credential material.
func DeterministicComment(planDigest, kind, instance string) string {
	h := sha256.Sum256([]byte(planDigest + ":" + kind + ":" + instance))
	return "dirextalk-core-v1:" + hex.EncodeToString(h[:])
}
func (p *Provider) readback(ctx context.Context, cl Clients, plan coreworkload.Plan, op coreworkload.Operation, state string) (coreworkload.Readback, error) {
	return coreworkload.Readback{TargetKind: plan.TargetKind, WorkloadID: op.WorkloadID, State: state, Identity: plan.Target.Identity, ProviderVersion: "aws-ssm-v1", At: time.Now().UTC()}, nil
}

var _ coreworkload.Provider = (*Provider)(nil)
