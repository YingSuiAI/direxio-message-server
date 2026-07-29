package aws_test

import (
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	workaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/aws"
	"testing"
	"time"
)

func TestCredentialHandleRejectsAmbientOrIncompleteCredentials(t *testing.T) {
	if err := (workaws.CredentialHandle{ReferenceID: "cred", Region: "us-east-1", AccountID: "123"}).Validate(); err == nil {
		t.Fatal("expected static credential precondition")
	}
}

func TestECSImageDigestAndSystemdValidation(t *testing.T) {
	p := coreworkload.Plan{Summary: "x", TargetKind: coreworkload.TargetAWSECS, ImageURI: "registry.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: nowUTC(), ResourceLimits: coreworkload.ResourceLimits{CPU: 256, MemoryMB: 512}, Target: coreworkload.TargetSettings{Identity: coreworkload.TargetIdentity{Kind: coreworkload.TargetAWSECS, AccountID: "123456789012", Region: "us-east-1", Cluster: "arn:aws:ecs:us-east-1:123456789012:cluster/c", Service: "svc", TaskDefinitionRevision: "1"}, ECSClusterARN: "arn:aws:ecs:us-east-1:123456789012:cluster/c", ECSServiceName: "svc", ECSTaskFamily: "family", ECSPlatformVersion: "1.4.0", ECSSubnetIDs: []string{"subnet-a"}, ECSSecurityGroupIDs: []string{"sg-a"}, ECSDesiredCount: 1}}
	_ = p.Validate() // typed provider tests separately exercise AWS read-back seams
	p.ImageURI = "registry.example/app:latest"
	if err := p.Validate(); err == nil {
		t.Fatal("unpinned image accepted")
	}
	ssm := p
	ssm.TargetKind = coreworkload.TargetAWSEC2SSM
	ssm.Target = coreworkload.TargetSettings{Identity: coreworkload.TargetIdentity{Kind: coreworkload.TargetAWSEC2SSM, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-123"}, EC2DocumentVersion: "$LATEST", EC2SystemdService: "ssh.service", RequiredInstanceTags: map[string]string{"Owner": "x"}}
	ssm.ImageURI = ""
	if err := ssm.Validate(); err == nil {
		t.Fatal("non-numeric SSM document accepted")
	}
}

func nowUTC() (t time.Time) { return time.Now().UTC().Add(time.Hour) }
