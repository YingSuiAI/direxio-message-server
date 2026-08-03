package aws

import (
	"errors"
	"sort"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

const (
	InfrastructureProfileGeneralLinuxSSMV1 = "general-linux-ssm-v1"
	InfrastructureProfileContainerHostV1   = "container-host-v1"

	infrastructureProvider = "aws"
	infrastructureKind     = "aws_ec2_instance"
	infrastructureOS       = "linux"
	infrastructureArch     = "x86_64"
)

var (
	// ErrUnknownInfrastructureProfile is returned without echoing the supplied
	// identifier. Profile identifiers are user-controlled input and must not
	// become an information or logging side channel.
	ErrUnknownInfrastructureProfile = errors.New("aws: unknown infrastructure profile")
	// ErrInvalidInfrastructureTarget is the stable, redacted validation error
	// for a target that does not satisfy its selected profile.
	ErrInvalidInfrastructureTarget = errors.New("aws: invalid infrastructure target")
)

// InfrastructureProfile is immutable catalog metadata for a generic AWS
// target. It intentionally contains no project, workload, instance-type,
// image, ingress, or other provider-operation details.
type InfrastructureProfile struct {
	ID                   string   `json:"id"`
	Provider             string   `json:"provider"`
	Kind                 string   `json:"kind"`
	OperatingSystem      string   `json:"operating_system"`
	Architecture         string   `json:"architecture"`
	RequiredCapabilities []string `json:"required_capabilities"`
}

var infrastructureProfiles = map[string]InfrastructureProfile{
	InfrastructureProfileGeneralLinuxSSMV1: {
		ID:                   InfrastructureProfileGeneralLinuxSSMV1,
		Provider:             infrastructureProvider,
		Kind:                 infrastructureKind,
		OperatingSystem:      infrastructureOS,
		Architecture:         infrastructureArch,
		RequiredCapabilities: []string{"target.aws_ec2_instance", "transport.aws_ssm"},
	},
	InfrastructureProfileContainerHostV1: {
		ID:                   InfrastructureProfileContainerHostV1,
		Provider:             infrastructureProvider,
		Kind:                 infrastructureKind,
		OperatingSystem:      infrastructureOS,
		Architecture:         infrastructureArch,
		RequiredCapabilities: []string{"runtime.container", "target.aws_ec2_instance", "transport.aws_ssm"},
	},
}

// LookupInfrastructureProfile returns a detached copy of one immutable
// catalog entry. Mutating its capability slice cannot mutate the catalog.
func LookupInfrastructureProfile(id string) (InfrastructureProfile, error) {
	p, ok := infrastructureProfiles[id]
	if !ok {
		return InfrastructureProfile{}, ErrUnknownInfrastructureProfile
	}
	return cloneInfrastructureProfile(p), nil
}

// InfrastructureProfiles returns all catalog entries in stable identifier
// order. Every result is detached from the immutable catalog.
func InfrastructureProfiles() []InfrastructureProfile {
	ids := make([]string, 0, len(infrastructureProfiles))
	for id := range infrastructureProfiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]InfrastructureProfile, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneInfrastructureProfile(infrastructureProfiles[id]))
	}
	return out
}

func cloneInfrastructureProfile(p InfrastructureProfile) InfrastructureProfile {
	p.RequiredCapabilities = append([]string(nil), p.RequiredCapabilities...)
	return p
}

// ValidateInfrastructureTarget validates and canonicalizes a target against
// its one explicit infrastructure profile. It intentionally returns only
// stable sentinels: account, region, and caller-provided identifiers are not
// copied into errors or logs.
func ValidateInfrastructureTarget(target execution.ExecutionTarget) error {
	_, err := NormalizeInfrastructureTarget(target)
	return err
}

// NormalizeInfrastructureTarget performs profile validation and then uses the
// execution boundary's canonical target normalization. Since
// InfrastructureProfileID is an existing ExecutionTarget field, the returned
// digest changes whenever the selected profile changes.
func NormalizeInfrastructureTarget(target execution.ExecutionTarget) (execution.ExecutionTarget, error) {
	if target.InfrastructureProfileID == "" {
		return execution.ExecutionTarget{}, ErrInvalidInfrastructureTarget
	}
	profile, err := LookupInfrastructureProfile(target.InfrastructureProfileID)
	if err != nil {
		return execution.ExecutionTarget{}, ErrUnknownInfrastructureProfile
	}
	if target.Provider != profile.Provider || target.Kind != profile.Kind || target.AccountID == "" || !accountIDValid(target.AccountID) || target.Region == "" || !validRegion(target.Region) || target.Architecture != profile.Architecture || !hasRequiredCapabilities(target.Capabilities, profile.RequiredCapabilities) {
		return execution.ExecutionTarget{}, ErrInvalidInfrastructureTarget
	}
	normalized, err := target.Normalize()
	if err != nil {
		if errors.Is(err, execution.ErrDigestMismatch) {
			return execution.ExecutionTarget{}, execution.ErrDigestMismatch
		}
		return execution.ExecutionTarget{}, ErrInvalidInfrastructureTarget
	}
	return normalized, nil
}

func hasRequiredCapabilities(actual, required []string) bool {
	seen := make(map[string]struct{}, len(actual))
	for _, capability := range actual {
		seen[capability] = struct{}{}
	}
	for _, capability := range required {
		if _, ok := seen[capability]; !ok {
			return false
		}
	}
	return true
}

// Validate keeps profile metadata self-contained and deterministic. It is not
// used to accept caller-defined profiles; the catalog remains fixed in code.
func (p InfrastructureProfile) Validate() error {
	if p.ID != InfrastructureProfileGeneralLinuxSSMV1 && p.ID != InfrastructureProfileContainerHostV1 {
		return ErrUnknownInfrastructureProfile
	}
	if p.Provider != infrastructureProvider || p.Kind != infrastructureKind || p.OperatingSystem != infrastructureOS || p.Architecture != infrastructureArch || len(p.RequiredCapabilities) == 0 {
		return ErrInvalidInfrastructureTarget
	}
	previous := ""
	for _, capability := range p.RequiredCapabilities {
		if capability == "" || capability <= previous {
			return ErrInvalidInfrastructureTarget
		}
		previous = capability
	}
	return nil
}

func (p InfrastructureProfile) String() string {
	return strings.TrimSpace(p.ID)
}
