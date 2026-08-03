package aws

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

const profileTargetID = "11111111-1111-4111-8111-111111111111"

func validProfileTarget() execution.ExecutionTarget {
	return execution.ExecutionTarget{
		ID:                      profileTargetID,
		Provider:                "aws",
		Kind:                    "aws_ec2_instance",
		InfrastructureProfileID: InfrastructureProfileGeneralLinuxSSMV1,
		AccountID:               "123456789012",
		Region:                  "us-east-1",
		Architecture:            "x86_64",
		Capabilities:            []string{"target.aws_ec2_instance", "transport.aws_ssm"},
		Revision:                1,
	}
}

func TestLookupInfrastructureProfileClonesCatalogEntry(t *testing.T) {
	got, err := LookupInfrastructureProfile(InfrastructureProfileGeneralLinuxSSMV1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "aws" || got.Kind != "aws_ec2_instance" || got.OperatingSystem != "linux" || got.Architecture != "x86_64" {
		t.Fatalf("profile metadata = %#v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("profile validation: %v", err)
	}
	got.RequiredCapabilities[0] = "mutated"
	again, err := LookupInfrastructureProfile(InfrastructureProfileGeneralLinuxSSMV1)
	if err != nil {
		t.Fatal(err)
	}
	if again.RequiredCapabilities[0] == "mutated" {
		t.Fatal("lookup returned mutable catalog storage")
	}
	profiles := InfrastructureProfiles()
	if len(profiles) != 2 || profiles[0].ID != InfrastructureProfileContainerHostV1 || profiles[1].ID != InfrastructureProfileGeneralLinuxSSMV1 {
		t.Fatalf("stable catalog order = %#v", profiles)
	}
	profiles[0].RequiredCapabilities[0] = "mutated"
	container, err := LookupInfrastructureProfile(InfrastructureProfileContainerHostV1)
	if err != nil || container.RequiredCapabilities[0] == "mutated" {
		t.Fatalf("catalog list was not detached: %#v %v", container, err)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "geolibre") || strings.Contains(string(raw), "instance_type") || strings.Contains(string(raw), "public_ingress") {
		t.Fatalf("profile leaked project/provider-operation metadata: %s", raw)
	}
}

func TestLookupInfrastructureProfileUnknownIsStableAndRedacted(t *testing.T) {
	secretID := "geolibre-project-secret"
	_, err := LookupInfrastructureProfile(secretID)
	if !errors.Is(err, ErrUnknownInfrastructureProfile) {
		t.Fatalf("unknown profile error = %v", err)
	}
	if strings.Contains(err.Error(), secretID) {
		t.Fatalf("unknown profile error echoed caller input: %v", err)
	}
}

func TestValidateInfrastructureTargetRejectsProfileMismatches(t *testing.T) {
	tests := []struct {
		name string
		edit func(*execution.ExecutionTarget)
	}{
		{"provider", func(v *execution.ExecutionTarget) { v.Provider = "aws_ssm" }},
		{"kind", func(v *execution.ExecutionTarget) { v.Kind = "aws_ecs_service" }},
		{"profile", func(v *execution.ExecutionTarget) { v.InfrastructureProfileID = InfrastructureProfileContainerHostV1 }},
		{"account", func(v *execution.ExecutionTarget) { v.AccountID = "not-an-account" }},
		{"region", func(v *execution.ExecutionTarget) { v.Region = "not-a-region" }},
		{"architecture", func(v *execution.ExecutionTarget) { v.Architecture = "arm64" }},
		{"capability", func(v *execution.ExecutionTarget) { v.Capabilities = []string{"target.aws_ec2_instance"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := validProfileTarget()
			test.edit(&target)
			if err := ValidateInfrastructureTarget(target); !errors.Is(err, ErrInvalidInfrastructureTarget) {
				t.Fatalf("mismatch error = %v", err)
			}
		})
	}
	missing := validProfileTarget()
	missing.InfrastructureProfileID = ""
	if err := ValidateInfrastructureTarget(missing); !errors.Is(err, ErrInvalidInfrastructureTarget) {
		t.Fatalf("missing explicit profile error = %v", err)
	}
	unknown := validProfileTarget()
	unknown.InfrastructureProfileID = "unknown-profile"
	if err := ValidateInfrastructureTarget(unknown); !errors.Is(err, ErrUnknownInfrastructureProfile) {
		t.Fatalf("unknown profile target error = %v", err)
	}
}

func TestNormalizeInfrastructureTargetIsDigestBoundToProfile(t *testing.T) {
	target := validProfileTarget()
	normalized, err := NormalizeInfrastructureTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Digest == "" || !normalized.Digest.Valid() {
		t.Fatalf("target digest = %q", normalized.Digest)
	}
	if err := ValidateInfrastructureTarget(normalized); err != nil {
		t.Fatalf("normalized target was not accepted: %v", err)
	}

	container := target
	container.InfrastructureProfileID = InfrastructureProfileContainerHostV1
	container.Capabilities = append(container.Capabilities, "runtime.container")
	containerNormalized, err := NormalizeInfrastructureTarget(container)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Digest == containerNormalized.Digest {
		t.Fatal("infrastructure profile was omitted from target digest")
	}

	stale := normalized
	stale.InfrastructureProfileID = InfrastructureProfileContainerHostV1
	if err := ValidateInfrastructureTarget(stale); !errors.Is(err, ErrInvalidInfrastructureTarget) && !errors.Is(err, execution.ErrDigestMismatch) {
		t.Fatalf("stale profile digest error = %v", err)
	}
}

func TestContainerHostProfileRequiresContainerCapability(t *testing.T) {
	p, err := LookupInfrastructureProfile(InfrastructureProfileContainerHostV1)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRequiredCapabilities([]string{"transport.aws_ssm", "target.aws_ec2_instance", "runtime.container"}, p.RequiredCapabilities) {
		t.Fatal("container profile capabilities were not required")
	}
	if hasRequiredCapabilities([]string{"transport.aws_ssm", "target.aws_ec2_instance"}, p.RequiredCapabilities) {
		t.Fatal("container profile accepted missing runtime capability")
	}
}
