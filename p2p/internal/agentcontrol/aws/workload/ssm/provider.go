package ssm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type STSClient interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}
type EC2Client interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
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
	_, err = p.verify(ctx, h, clients, plan)
	return err
}

func (p *Provider) Apply(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (coreworkload.Readback, error) {
	h, clients, err := p.prepare(ctx, plan, op)
	if err != nil {
		return coreworkload.Readback{}, err
	}
	identity, err := p.verify(ctx, h, clients, plan)
	if err != nil {
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
	return p.readback(ctx, clients, plan, op, identity, "ready")
}
func (p *Provider) Destroy(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (coreworkload.Readback, error) {
	h, clients, err := p.prepare(ctx, plan, op)
	if err != nil {
		return coreworkload.Readback{}, err
	}
	identity, err := p.verify(ctx, h, clients, plan)
	if err != nil {
		return coreworkload.Readback{}, err
	}
	commands, err := destroyCommands(plan)
	if err != nil {
		return coreworkload.Readback{}, err
	}
	if _, err = p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, commands, "destroy"); err != nil {
		return coreworkload.Readback{}, err
	}
	if _, err = p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, []string{destroyedProbe(plan)}, "destroy-ready"); err != nil {
		return coreworkload.Readback{}, err
	}
	return p.readback(ctx, clients, plan, op, identity, "destroyed")
}

func destroyCommands(plan coreworkload.Plan) ([]string, error) {
	commands := []string{
		"systemctl stop " + plan.Target.EC2SystemdService,
		"systemctl disable " + plan.Target.EC2SystemdService,
	}
	switch plan.Target.EC2CleanupProfile {
	case "":
		return commands, nil
	case coreworkload.EC2CleanupProfileGeoLibreStaticV1:
		return []string{
			"set -euo pipefail",
			"if systemctl list-unit-files --type=service --no-legend | awk '{print $1}' | grep -Fxq " + coreworkload.GeoLibreStaticV1Service + "; then systemctl stop " + coreworkload.GeoLibreStaticV1Service + "; systemctl disable " + coreworkload.GeoLibreStaticV1Service + "; fi",
			"docker info >/dev/null",
			"if docker container inspect dirextalk-geolibre >/dev/null 2>&1; then docker rm -f dirextalk-geolibre >/dev/null; fi",
			"if docker image inspect " + coreworkload.GeoLibreStaticV1ImageURI + " >/dev/null 2>&1; then docker image rm " + coreworkload.GeoLibreStaticV1ImageURI + " >/dev/null; fi",
			"rm -f /etc/systemd/system/" + coreworkload.GeoLibreStaticV1Service + " /var/lib/dirextalk-geolibre/nginx.conf.template",
			"systemctl daemon-reload",
		}, nil
	default:
		return nil, workaws.ErrInvalid
	}
}
func (p *Provider) Read(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (coreworkload.Readback, error) {
	h, clients, err := p.prepare(ctx, plan, op)
	if err != nil {
		return coreworkload.Readback{}, err
	}
	identity, err := p.verify(ctx, h, clients, plan)
	if err != nil {
		return coreworkload.Readback{}, err
	}
	state := "ready"
	if _, err = p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, []string{activeProbe(plan.Target.EC2SystemdService)}, "read"); err != nil {
		state = "destroyed"
		if _, e := p.command(ctx, clients.SSM, plan.Target.InstanceID, plan, []string{destroyedProbe(plan)}, "read"); e != nil {
			return coreworkload.Readback{}, err
		}
	}
	return p.readback(ctx, clients, plan, op, identity, state)
}
func (p *Provider) prepare(ctx context.Context, plan coreworkload.Plan, op coreworkload.Operation) (workaws.CredentialHandle, Clients, error) {
	if plan.TargetKind != coreworkload.TargetAWSEC2SSM || op.TargetKind != plan.TargetKind || plan.Digest != op.PlanDigest || plan.Revision != op.PlanRevision {
		return workaws.CredentialHandle{}, Clients{}, workaws.ErrInvalid
	}
	if _, err := plan.Normalize(); err != nil || coreworkload.PlanInputDigest(plan) != plan.Digest {
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

func (p *Provider) verify(ctx context.Context, h workaws.CredentialHandle, cl Clients, plan coreworkload.Plan) (coreworkload.TargetIdentity, error) {
	identity, e := cl.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if e != nil || identity == nil || aws.ToString(identity.Account) != plan.Target.AccountID || aws.ToString(identity.Arn) != h.PrincipalARN {
		return coreworkload.TargetIdentity{}, workaws.ErrPrecondition
	}
	out, e := cl.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{plan.Target.InstanceID}})
	if e != nil || out == nil || aws.ToString(out.NextToken) != "" || len(out.Reservations) != 1 || len(out.Reservations[0].Instances) != 1 {
		return coreworkload.TargetIdentity{}, workaws.ErrPrecondition
	}
	in := out.Reservations[0].Instances[0]
	if in.InstanceId == nil || aws.ToString(in.InstanceId) != plan.Target.InstanceID || in.State == nil || in.State.Name != ec2types.InstanceStateNameRunning || aws.ToString(in.PlatformDetails) != "Linux/UNIX" {
		return coreworkload.TargetIdentity{}, workaws.ErrPrecondition
	}
	tags := map[string]string{}
	for _, t := range in.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	if plan.Target.RequiredInstanceTags["service"] == "geolibre" {
		owner := plan.Target.RequiredInstanceTags["owner"]
		if !coreworkload.OwnerBindingTagValid(owner) || plan.Target.RequiredInstanceTags["dirextalk:owner-binding"] != owner {
			return coreworkload.TargetIdentity{}, workaws.ErrPrecondition
		}
	}
	for k, v := range plan.Target.RequiredInstanceTags {
		if tags[k] != v {
			return coreworkload.TargetIdentity{}, workaws.ErrPrecondition
		}
	}
	// The plan's endpoint is an immutable public identity. Never send a command
	// to an instance whose current public address has drifted from that identity.
	publicIP := strings.TrimSpace(aws.ToString(in.PublicIpAddress))
	if publicIP == "" || plan.Target.Identity.Endpoint != "http://"+publicIP {
		return coreworkload.TargetIdentity{}, workaws.ErrPrecondition
	}
	if !matchesBoundSecurityGroups(plan) || !matchesInstanceSecurityGroups(in.SecurityGroups, plan) {
		return coreworkload.TargetIdentity{}, workaws.ErrPrecondition
	}
	if plan.Target.EC2CleanupProfile == coreworkload.EC2CleanupProfileGeoLibreStaticV1 {
		if plan.Target.Labels["dirextalk:exposure"] != "public-unauthenticated-http" || !securityGroupAllowsPublicHTTP(ctx, cl.EC2, plan) {
			return coreworkload.TargetIdentity{}, workaws.ErrPrecondition
		}
	}
	si, e := cl.SSM.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("InstanceIds"), Values: []string{plan.Target.InstanceID}}}})
	if e != nil || si == nil || aws.ToString(si.NextToken) != "" || len(si.InstanceInformationList) != 1 || aws.ToString(si.InstanceInformationList[0].InstanceId) != plan.Target.InstanceID || si.InstanceInformationList[0].PingStatus != ssmtypes.PingStatusOnline {
		return coreworkload.TargetIdentity{}, workaws.ErrPrecondition
	}
	verified := plan.Target.Identity
	verified.Endpoint = "http://" + publicIP
	return verified, nil
}

// securityGroupAllowsPublicHTTP proves the live SG attached to the fixed
// GeoLibre target still permits the exact public TCP/80 ingress advertised by
// its immutable plan. A missing, paginated, mismatched, or broader-only rule
// is rejected before any SSM command is submitted.
func securityGroupAllowsPublicHTTP(ctx context.Context, client EC2Client, plan coreworkload.Plan) bool {
	if client == nil || len(plan.Target.NetworkGrantDetails) != 1 {
		return false
	}
	groupID := strings.TrimSpace(plan.Target.NetworkGrantDetails[0].ReferenceID)
	if groupID == "" || plan.Target.NetworkGrantDetails[0].Kind != "aws_security_group" {
		return false
	}
	out, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{groupID}})
	if err != nil || out == nil || aws.ToString(out.NextToken) != "" || len(out.SecurityGroups) != 1 {
		return false
	}
	group := out.SecurityGroups[0]
	if strings.TrimSpace(aws.ToString(group.GroupId)) != groupID {
		return false
	}
	// The typed GeoLibre topology owns one and only one ingress rule. Requiring
	// an exact single permission prevents a seemingly valid port-80 rule from
	// hiding additional SSH/443/private/IPv6/source-group exposure.
	if len(group.IpPermissions) != 1 {
		return false
	}
	permission := group.IpPermissions[0]
	if aws.ToString(permission.IpProtocol) != "tcp" || permission.FromPort == nil || permission.ToPort == nil || aws.ToInt32(permission.FromPort) != 80 || aws.ToInt32(permission.ToPort) != 80 || len(permission.IpRanges) != 1 || len(permission.Ipv6Ranges) != 0 || len(permission.PrefixListIds) != 0 || len(permission.UserIdGroupPairs) != 0 {
		return false
	}
	cidr := permission.IpRanges[0]
	return strings.TrimSpace(aws.ToString(cidr.CidrIp)) == "0.0.0.0/0" && strings.TrimSpace(aws.ToString(cidr.Description)) == ""
}

func matchesBoundSecurityGroups(plan coreworkload.Plan) bool {
	expected := make(map[string]struct{}, len(plan.Target.NetworkGrantDetails))
	for _, grant := range plan.Target.NetworkGrantDetails {
		if grant.Kind != "aws_security_group" || strings.TrimSpace(grant.ReferenceID) == "" {
			return false
		}
		if _, duplicate := expected[grant.ReferenceID]; duplicate {
			return false
		}
		expected[grant.ReferenceID] = struct{}{}
	}
	if len(expected) == 0 {
		return false
	}
	if len(plan.NetworkGrants) > 0 {
		if len(plan.NetworkGrants) != len(expected) {
			return false
		}
		seen := make(map[string]struct{}, len(plan.NetworkGrants))
		for _, value := range plan.NetworkGrants {
			const prefix = "security-group:"
			if !strings.HasPrefix(value, prefix) {
				return false
			}
			id := strings.TrimPrefix(value, prefix)
			if _, duplicate := seen[id]; duplicate {
				return false
			}
			if _, ok := expected[id]; !ok {
				return false
			}
			seen[id] = struct{}{}
		}
	}
	return true
}

func matchesInstanceSecurityGroups(groups []ec2types.GroupIdentifier, plan coreworkload.Plan) bool {
	expected := make(map[string]struct{}, len(plan.Target.NetworkGrantDetails))
	for _, grant := range plan.Target.NetworkGrantDetails {
		expected[grant.ReferenceID] = struct{}{}
	}
	if len(groups) != len(expected) {
		return false
	}
	for _, group := range groups {
		id := strings.TrimSpace(aws.ToString(group.GroupId))
		if id == "" {
			return false
		}
		if _, ok := expected[id]; !ok {
			return false
		}
		delete(expected, id)
	}
	return len(expected) == 0
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

func destroyedProbe(plan coreworkload.Plan) string {
	if plan.Target.EC2CleanupProfile != coreworkload.EC2CleanupProfileGeoLibreStaticV1 {
		return inactiveProbe(plan.Target.EC2SystemdService)
	}
	return "set -euo pipefail; docker info >/dev/null; " +
		"test ! -e /etc/systemd/system/" + coreworkload.GeoLibreStaticV1Service + "; " +
		"test ! -e /var/lib/dirextalk-geolibre/nginx.conf.template; " +
		"test -z \"$(docker container ls -a --filter name=^/dirextalk-geolibre$ --format '{{.Names}}')\"; " +
		"! docker image inspect " + coreworkload.GeoLibreStaticV1ImageURI + " >/dev/null 2>&1"
}

// DeterministicComment is safe to expose in acceptance tests and read-only
// reconciliation tooling; it contains no command or credential material.
func DeterministicComment(planDigest, kind, instance string) string {
	h := sha256.Sum256([]byte(planDigest + ":" + kind + ":" + instance))
	return "dirextalk-core-v1:" + hex.EncodeToString(h[:])
}
func (p *Provider) readback(ctx context.Context, cl Clients, plan coreworkload.Plan, op coreworkload.Operation, identity coreworkload.TargetIdentity, state string) (coreworkload.Readback, error) {
	if state == "destroyed" {
		// The instance endpoint is no longer an immutable fact after destroy.
		identity.Endpoint = ""
	}
	return coreworkload.Readback{TargetKind: plan.TargetKind, WorkloadID: op.WorkloadID, State: state, Identity: identity, ProviderVersion: "aws-ssm-v1", At: time.Now().UTC()}, nil
}

var _ coreworkload.Provider = (*Provider)(nil)
