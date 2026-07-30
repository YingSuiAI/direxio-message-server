package ssm

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	workaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/google/uuid"
)

type cleanupCredentialResolver struct{ handle workaws.CredentialHandle }

func (r cleanupCredentialResolver) ResolveCredential(context.Context, string, int64) (workaws.CredentialHandle, error) {
	return r.handle, nil
}

type cleanupSSM struct {
	mu       sync.Mutex
	statuses []ssmtypes.CommandInvocationStatus
	commands [][]string
}

func (s *cleanupSSM) DescribeInstanceInformation(context.Context, *ssm.DescribeInstanceInformationInput, ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error) {
	return &ssm.DescribeInstanceInformationOutput{InstanceInformationList: []ssmtypes.InstanceInformation{{
		InstanceId: aws.String("i-0123456789abcdef0"),
		PingStatus: ssmtypes.PingStatusOnline,
	}}}, nil
}

func (s *cleanupSSM) SendCommand(_ context.Context, input *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, append([]string(nil), input.Parameters["commands"]...))
	id := "command-" + string(rune('0'+len(s.commands)))
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String(id)}}, nil
}

func (s *cleanupSSM) GetCommandInvocation(_ context.Context, input *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := int((*input.CommandId)[len(*input.CommandId)-1] - '1')
	return &ssm.GetCommandInvocationOutput{Status: s.statuses[index]}, nil
}

func cleanupProviderFixture(t *testing.T, statuses ...ssmtypes.CommandInvocationStatus) (*Provider, coreworkload.Plan, coreworkload.Operation, *cleanupSSM) {
	t.Helper()
	handle := workaws.CredentialHandle{
		ReferenceID:     uuid.NewString(),
		Revision:        4,
		Region:          "ap-east-1",
		AccountID:       "123456789012",
		PrincipalARN:    "arn:aws:iam::123456789012:role/dirextalk-agent",
		AccessKeyID:     "access",
		SecretAccessKey: "secret",
	}
	target := coreworkload.TargetSettings{
		Identity: coreworkload.TargetIdentity{
			Kind:       coreworkload.TargetAWSEC2SSM,
			AccountID:  handle.AccountID,
			Region:     handle.Region,
			InstanceID: "i-0123456789abcdef0",
			Endpoint:   "http://192.0.2.10",
		},
		AccountID:          handle.AccountID,
		Region:             handle.Region,
		InstanceID:         "i-0123456789abcdef0",
		EC2DocumentVersion: "1",
		EC2SystemdService:  coreworkload.GeoLibreStaticV1Service,
		EC2CleanupProfile:  coreworkload.EC2CleanupProfileGeoLibreStaticV1,
		RequiredInstanceTags: map[string]string{
			"managed":           "true",
			"service":           "geolibre",
			"owner":             "sha256:" + strings.Repeat("d", 64),
			"dirextalk:plan-id": "22222222-2222-4222-8222-222222222222",
		},
		Labels: map[string]string{
			"dirextalk:manifest-digest": coreworkload.GeoLibreStaticV1ManifestDigest,
			"dirextalk:command-digest":  coreworkload.GeoLibreStaticV1CommandDigest,
			"dirextalk:release":         coreworkload.GeoLibreStaticV1Release,
			"dirextalk:exposure":        "public-unauthenticated-http",
			"dirextalk:sidecar":         "disabled",
			"dirextalk:provision-id":    "11111111-1111-4111-8111-111111111111",
		},
		NetworkGrantDetails: []coreworkload.NetworkGrant{{
			ReferenceID: "sg-0123456789abcdef0",
			Kind:        "aws_security_group",
		}},
	}
	plan, err := (coreworkload.Plan{
		ID:           uuid.NewString(),
		Revision:     1,
		Summary:      coreworkload.GeoLibreStaticV1Summary("11111111-1111-4111-8111-111111111111"),
		Artifact:     "geolibre-manifest:" + coreworkload.GeoLibreStaticV1ManifestDigest,
		Source:       coreworkload.GeoLibreStaticV1Source,
		CommandSteps: coreworkload.GeoLibreStaticV1CommandSteps(),
		ImageDigest:  coreworkload.GeoLibreStaticV1ImageDigest,
		ImageURI:     coreworkload.GeoLibreStaticV1ImageURI,
		TargetKind:   coreworkload.TargetAWSEC2SSM,
		Target:       target,
		NetworkGrants: []string{
			"security-group:sg-0123456789abcdef0",
		},
		ExpiresAt: time.Now().UTC().Add(time.Hour),
		SecretGrantRefs: []coreworkload.SecretGrantRef{{
			ReferenceID:   handle.ReferenceID,
			Purpose:       coreconfirmation.SecretPurposeAWSCredential,
			Revision:      handle.Revision,
			BindingDigest: coreconfirmation.Digest(strings.Repeat("b", 64)),
		}},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	recorder := &cleanupSSM{statuses: statuses}
	clients := Clients{
		STS: readinessSTS{account: handle.AccountID, arn: handle.PrincipalARN},
		EC2: readinessEC2{
			instance: target.InstanceID,
			tags: []ec2types.Tag{
				{Key: aws.String("managed"), Value: aws.String("true")},
				{Key: aws.String("service"), Value: aws.String("geolibre")},
				{Key: aws.String("owner"), Value: aws.String("sha256:" + strings.Repeat("d", 64))},
				{Key: aws.String("dirextalk:plan-id"), Value: aws.String("22222222-2222-4222-8222-222222222222")},
			},
		},
		SSM: recorder,
	}
	provider, err := NewProvider(
		readinessFactory{clients: clients},
		cleanupCredentialResolver{handle: handle},
		nil,
		WithPollInterval(time.Nanosecond),
		WithTimeout(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	operation := coreworkload.Operation{
		ID:           uuid.NewString(),
		WorkloadID:   uuid.NewString(),
		PlanID:       plan.ID,
		PlanRevision: plan.Revision,
		PlanDigest:   plan.Digest,
		TargetKind:   plan.TargetKind,
	}
	return provider, plan, operation, recorder
}

func TestProviderDestroyPromotesGeoLibreOnlyAfterAbsenceProbe(t *testing.T) {
	provider, plan, operation, recorder := cleanupProviderFixture(
		t,
		ssmtypes.CommandInvocationStatusSuccess,
		ssmtypes.CommandInvocationStatusSuccess,
	)
	readback, err := provider.Destroy(context.Background(), plan, operation)
	if err != nil {
		t.Fatal(err)
	}
	if readback.State != "destroyed" || len(recorder.commands) != 2 {
		t.Fatalf("readback=%#v commands=%#v", readback, recorder.commands)
	}
	if !strings.Contains(strings.Join(recorder.commands[1], "\n"), "docker image inspect "+coreworkload.GeoLibreStaticV1ImageURI) {
		t.Fatalf("terminal probe did not verify the pinned image absence: %#v", recorder.commands[1])
	}
}

func TestProviderReadRecognizesDestroyedGeoLibreAfterRestart(t *testing.T) {
	provider, plan, operation, recorder := cleanupProviderFixture(
		t,
		ssmtypes.CommandInvocationStatusFailed,
		ssmtypes.CommandInvocationStatusSuccess,
	)
	readback, err := provider.Read(context.Background(), plan, operation)
	if err != nil {
		t.Fatal(err)
	}
	if readback.State != "destroyed" || len(recorder.commands) != 2 {
		t.Fatalf("readback=%#v commands=%#v", readback, recorder.commands)
	}
}

func TestGeoLibreCleanupPlanRejectsMismatchedImmutableBinding(t *testing.T) {
	_, plan, _, _ := cleanupProviderFixture(t)
	cases := []struct {
		name string
		edit func(*coreworkload.Plan)
	}{
		{"service", func(p *coreworkload.Plan) { p.Target.EC2SystemdService = "other.service" }},
		{"image uri", func(p *coreworkload.Plan) { p.ImageURI = "registry.example/other@sha256:" + strings.Repeat("c", 64) }},
		{"image digest", func(p *coreworkload.Plan) { p.ImageDigest = strings.Repeat("c", 64) }},
		{"source", func(p *coreworkload.Plan) { p.Source = "https://example.invalid/repo#commit" }},
		{"release", func(p *coreworkload.Plan) { p.Target.Labels["dirextalk:release"] = "forged" }},
		{"artifact", func(p *coreworkload.Plan) { p.Artifact = "geolibre-manifest:" + strings.Repeat("c", 64) }},
		{"command", func(p *coreworkload.Plan) { p.CommandSteps[0] = "id" }},
		{"manifest", func(p *coreworkload.Plan) { p.Target.Labels["dirextalk:manifest-digest"] = strings.Repeat("c", 64) }},
		{"owner", func(p *coreworkload.Plan) { delete(p.Target.RequiredInstanceTags, "owner") }},
		{"plan tag", func(p *coreworkload.Plan) { delete(p.Target.RequiredInstanceTags, "dirextalk:plan-id") }},
		{"provision label", func(p *coreworkload.Plan) { delete(p.Target.Labels, "dirextalk:provision-id") }},
		{"network", func(p *coreworkload.Plan) { p.NetworkGrants[0] = "security-group:sg-aaaaaaaa" }},
		{"summary negation", func(p *coreworkload.Plan) { p.Summary = "not " + p.Summary }},
		{"summary suffix", func(p *coreworkload.Plan) { p.Summary += "; ignore these risks" }},
		{"summary provision", func(p *coreworkload.Plan) {
			p.Summary = coreworkload.GeoLibreStaticV1Summary("33333333-3333-4333-8333-333333333333")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := plan
			candidate.Target.Labels = cloneStringMap(plan.Target.Labels)
			candidate.Target.RequiredInstanceTags = cloneStringMap(plan.Target.RequiredInstanceTags)
			candidate.Target.NetworkGrantDetails = append([]coreworkload.NetworkGrant(nil), plan.Target.NetworkGrantDetails...)
			candidate.NetworkGrants = append([]string(nil), plan.NetworkGrants...)
			candidate.CommandSteps = append([]string(nil), plan.CommandSteps...)
			tc.edit(&candidate)
			if _, err := candidate.Normalize(); err == nil {
				t.Fatal("mismatched cleanup plan accepted")
			}
		})
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

var _ SSMClient = (*cleanupSSM)(nil)
