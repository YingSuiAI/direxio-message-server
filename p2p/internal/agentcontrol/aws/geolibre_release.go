package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"regexp"
	"strings"
	"time"

	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

const (
	GeoLibreReleaseVersion = coreworkload.GeoLibreStaticV1Release
	GeoLibreRepositoryURL  = "https://github.com/opengeos/GeoLibre.git"
	GeoLibreCommitSHA      = "2856ef8c0b227ad18ecf43d4623cf00013c1740e"
	GeoLibreSystemdService = coreworkload.GeoLibreStaticV1Service
	GeoLibreImageDigest    = coreworkload.GeoLibreStaticV1ImageDigest
	GeoLibreImageURI       = coreworkload.GeoLibreStaticV1ImageURI
)

// GeoLibreReleaseManifest is compiled into the Message Server. Requests can
// select this release, but cannot replace its repository, commit, build inputs,
// base images, commands, service name, port, or health check.
type GeoLibreReleaseManifest struct {
	Version        string
	RepositoryURL  string
	CommitSHA      string
	ImageURI       string
	ImageDigest    string
	SystemdService string
	ContainerPort  uint32
	HealthPath     string
	CommandSteps   []string
}

// GeoLibreInstallTarget contains only authenticated, read-back provision
// identity. Callers must construct it from the owner-scoped provision record;
// no field is accepted from model-authored tool arguments.
type GeoLibreInstallTarget struct {
	ProvisionID        string
	ProvisionPlanID    string
	CredentialID       string
	CredentialRevision int64
	AccountID          string
	Region             string
	InstanceID         string
	PublicIP           string
	SecurityGroupID    string
	OwnerBindingDigest string
}

// CurrentGeoLibreReleaseManifest returns an isolated manifest copy.
func CurrentGeoLibreReleaseManifest() GeoLibreReleaseManifest {
	manifest := GeoLibreReleaseManifest{
		Version:        GeoLibreReleaseVersion,
		RepositoryURL:  GeoLibreRepositoryURL,
		CommitSHA:      GeoLibreCommitSHA,
		ImageURI:       GeoLibreImageURI,
		ImageDigest:    GeoLibreImageDigest,
		SystemdService: GeoLibreSystemdService,
		ContainerPort:  80,
		HealthPath:     "/healthz",
	}
	manifest.CommandSteps = coreworkload.GeoLibreStaticV1CommandSteps()
	return manifest
}

// Digest binds the audited source inputs, base-image manifests, hardening
// patch, install commands, service, port, and health endpoint.
func (m GeoLibreReleaseManifest) Digest() string {
	copy := m
	copy.CommandSteps = append([]string(nil), m.CommandSteps...)
	return canonicalDigest(copy)
}

// BuildGeoLibreSSMPlan creates the only accepted SSM plan for this release.
// The PostgreSQL workload store replaces the placeholder credential binding
// with the encrypted owner/revision binding before it hashes and persists the
// immutable plan.
func BuildGeoLibreSSMPlan(target GeoLibreInstallTarget, idempotencyKey string, expiresAt time.Time) (coreworkload.PlanInput, error) {
	if !validGeoLibreInstallTarget(target) || !validUUID(strings.TrimSpace(idempotencyKey)) ||
		expiresAt.IsZero() || expiresAt.Location() != time.UTC || !expiresAt.After(time.Now().UTC()) {
		return coreworkload.PlanInput{}, ErrInvalid
	}
	manifest := CurrentGeoLibreReleaseManifest()
	manifestDigest := manifest.Digest()
	if manifestDigest != coreworkload.GeoLibreStaticV1ManifestDigest ||
		coreworkload.CommandStepsDigest(manifest.CommandSteps) != coreworkload.GeoLibreStaticV1CommandDigest {
		return coreworkload.PlanInput{}, ErrInvalid
	}
	identity := coreworkload.TargetIdentity{
		Kind:       coreworkload.TargetAWSEC2SSM,
		AccountID:  target.AccountID,
		Region:     target.Region,
		InstanceID: target.InstanceID,
		Endpoint:   "http://" + target.PublicIP,
	}
	settings := coreworkload.TargetSettings{
		Identity:           identity,
		AccountID:          target.AccountID,
		Region:             target.Region,
		InstanceID:         target.InstanceID,
		Ports:              []int32{80},
		PortDetails:        []coreworkload.Port{{Port: 80}},
		EC2DocumentVersion: "1",
		EC2SystemdService:  manifest.SystemdService,
		EC2CleanupProfile:  coreworkload.EC2CleanupProfileGeoLibreStaticV1,
		RequiredInstanceTags: map[string]string{
			"owner":             target.OwnerBindingDigest,
			"managed":           "true",
			"service":           EC2ServiceProfile,
			"dirextalk:plan-id": target.ProvisionPlanID,
		},
		Labels: map[string]string{
			"dirextalk:provision-id":    target.ProvisionID,
			"dirextalk:release":         manifest.Version,
			"dirextalk:manifest-digest": manifestDigest,
			"dirextalk:command-digest":  coreworkload.GeoLibreStaticV1CommandDigest,
			"dirextalk:exposure":        "public-unauthenticated-http",
			"dirextalk:sidecar":         "disabled",
		},
		NetworkGrantDetails: []coreworkload.NetworkGrant{{
			ReferenceID: target.SecurityGroupID,
			Kind:        "aws_security_group",
		}},
	}
	return coreworkload.PlanInput{
		Summary:       coreworkload.GeoLibreStaticV1Summary(target.ProvisionID),
		Artifact:      "geolibre-manifest:" + manifestDigest,
		Source:        manifest.RepositoryURL + "#" + manifest.CommitSHA,
		CommandSteps:  append([]string(nil), manifest.CommandSteps...),
		ImageDigest:   manifest.ImageDigest,
		ImageURI:      manifest.ImageURI,
		TargetKind:    coreworkload.TargetAWSEC2SSM,
		Target:        settings,
		NetworkGrants: []string{"security-group:" + target.SecurityGroupID},
		SecretGrantRefs: []coreworkload.SecretGrantRef{{
			ReferenceID:   target.CredentialID,
			Purpose:       coreconfirmation.SecretPurposeAWSCredential,
			Revision:      target.CredentialRevision,
			BindingDigest: coreconfirmation.Digest(strings.Repeat("0", 64)),
		}},
		ResourceLimits: coreworkload.ResourceLimits{TimeoutS: 3600, OutputMB: 16, DiskMB: 20480},
		ExpiresAt:      expiresAt,
		IdempotencyKey: strings.TrimSpace(idempotencyKey),
	}, nil
}

func validGeoLibreInstallTarget(target GeoLibreInstallTarget) bool {
	if !validUUID(strings.TrimSpace(target.ProvisionID)) ||
		!validUUID(strings.TrimSpace(target.ProvisionPlanID)) ||
		!validUUID(strings.TrimSpace(target.CredentialID)) ||
		target.CredentialRevision < 1 ||
		!accountIDValid(target.AccountID) ||
		!validRegion(target.Region) ||
		!regexp.MustCompile(`^i-[0-9a-f]{8,17}$`).MatchString(target.InstanceID) ||
		!regexp.MustCompile(`^sg-[0-9a-f]{8,17}$`).MatchString(target.SecurityGroupID) ||
		!validGeoLibrePublicIPv4(target.PublicIP) ||
		!validOwnerBindingDigest(target.OwnerBindingDigest) {
		return false
	}
	return true
}

func validGeoLibrePublicIPv4(value string) bool {
	if strings.TrimSpace(value) != value {
		return false
	}
	ip := net.ParseIP(value)
	v4 := ip.To4()
	if ip == nil || v4 == nil || v4.String() != value || !ip.IsGlobalUnicast() ||
		ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return true
}

func validOwnerBindingDigest(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}
