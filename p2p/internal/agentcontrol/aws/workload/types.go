// Package coreworkload owns immutable workload plans and their confirmed,
// durable apply/destroy operations. Providers are deliberately typed and are
// never passed arbitrary shell or SDK arguments.
package coreworkload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

type TargetKind string

const (
	TargetCoreRunner TargetKind = "CORE_RUNNER"
	TargetAWSEC2SSM  TargetKind = "AWS_EC2_SSM"
	TargetAWSECS     TargetKind = "AWS_ECS"
)

type OperationKind string

const (
	OperationApply   OperationKind = "apply"
	OperationDestroy OperationKind = "destroy"
)

type OperationStatus string

const (
	OperationWaitingUser OperationStatus = "waiting_user"
	OperationRunning     OperationStatus = "running"
	OperationSucceeded   OperationStatus = "succeeded"
	OperationFailed      OperationStatus = "failed"
	OperationUncertain   OperationStatus = "uncertain"
	OperationRejected    OperationStatus = "rejected"
	OperationExpired     OperationStatus = "expired"
	OperationCanceled    OperationStatus = "canceled"
)

type ResourceLimits struct {
	CPU       int64 `json:"cpu,omitempty"`
	MemoryMB  int64 `json:"memory_mb,omitempty"`
	Processes int64 `json:"processes,omitempty"`
	DiskMB    int64 `json:"disk_mb,omitempty"`
	TimeoutS  int64 `json:"timeout_seconds,omitempty"`
	OutputMB  int64 `json:"output_mb,omitempty"`
}

type TargetSettings struct {
	Identity             TargetIdentity    `json:"identity,omitempty" yaml:"identity,omitempty" mapstructure:"identity"`
	Region               string            `json:"region,omitempty" yaml:"region,omitempty" mapstructure:"region"`
	AccountID            string            `json:"account_id,omitempty" yaml:"account_id,omitempty" mapstructure:"account_id"`
	Cluster              string            `json:"cluster,omitempty" yaml:"cluster,omitempty" mapstructure:"cluster"`
	Service              string            `json:"service,omitempty" yaml:"service,omitempty" mapstructure:"service"`
	InstanceID           string            `json:"instance_id,omitempty" yaml:"instance_id,omitempty" mapstructure:"instance_id"`
	Ports                []int32           `json:"ports,omitempty"`
	PortDetails          []Port            `json:"port_details,omitempty"`
	NetworkGrantDetails  []NetworkGrant    `json:"network_grant_details,omitempty"`
	Labels               map[string]string `json:"labels,omitempty"`
	EC2DocumentVersion   string            `json:"ec2_document_version,omitempty" yaml:"ec2_document_version,omitempty" mapstructure:"ec2_document_version"`
	EC2SystemdService    string            `json:"ec2_systemd_service,omitempty" yaml:"ec2_systemd_service,omitempty" mapstructure:"ec2_systemd_service"`
	EC2CleanupProfile    string            `json:"ec2_cleanup_profile,omitempty" yaml:"ec2_cleanup_profile,omitempty" mapstructure:"ec2_cleanup_profile"`
	RequiredInstanceTags map[string]string `json:"required_instance_tags,omitempty" yaml:"required_instance_tags,omitempty" mapstructure:"required_instance_tags"`
	ECSClusterARN        string            `json:"ecs_cluster_arn,omitempty" yaml:"ecs_cluster_arn,omitempty" mapstructure:"ecs_cluster_arn"`
	ECSServiceName       string            `json:"ecs_service_name,omitempty" yaml:"ecs_service_name,omitempty" mapstructure:"ecs_service_name"`
	ECSTaskFamily        string            `json:"ecs_task_family,omitempty" yaml:"ecs_task_family,omitempty" mapstructure:"ecs_task_family"`
	ECSPlatformVersion   string            `json:"ecs_platform_version,omitempty" yaml:"ecs_platform_version,omitempty" mapstructure:"ecs_platform_version"`
	ECSSubnetIDs         []string          `json:"ecs_subnet_ids,omitempty" yaml:"ecs_subnet_ids,omitempty" mapstructure:"ecs_subnet_ids"`
	ECSSecurityGroupIDs  []string          `json:"ecs_security_group_ids,omitempty" yaml:"ecs_security_group_ids,omitempty" mapstructure:"ecs_security_group_ids"`
	ECSAssignPublicIP    bool              `json:"ecs_assign_public_ip,omitempty" yaml:"ecs_assign_public_ip,omitempty" mapstructure:"ecs_assign_public_ip"`
	ECSTargetGroupARN    string            `json:"ecs_target_group_arn,omitempty" yaml:"ecs_target_group_arn,omitempty" mapstructure:"ecs_target_group_arn"`
	ECSTargetGroupPort   uint32            `json:"ecs_target_group_port,omitempty" yaml:"ecs_target_group_port,omitempty" mapstructure:"ecs_target_group_port"`
	ECSTaskRoleARN       string            `json:"ecs_task_role_arn,omitempty" yaml:"ecs_task_role_arn,omitempty" mapstructure:"ecs_task_role_arn"`
	ECSExecutionRoleARN  string            `json:"ecs_execution_role_arn,omitempty" yaml:"ecs_execution_role_arn,omitempty" mapstructure:"ecs_execution_role_arn"`
	ECSDesiredCount      int64             `json:"ecs_desired_count,omitempty" yaml:"ecs_desired_count,omitempty" mapstructure:"ecs_desired_count"`
	ECSImageURI          string            `json:"ecs_image_uri,omitempty" yaml:"ecs_image_uri,omitempty" mapstructure:"ecs_image_uri"`
}

const (
	EC2CleanupProfileGeoLibreStaticV1 = "geolibre-static-v1"
	GeoLibreStaticV1Release           = "geolibre-2856ef8-v1"
	GeoLibreStaticV1Service           = "dirextalk-geolibre.service"
	GeoLibreStaticV1ImageDigest       = "bd18a93768087e5619e75e2e8282ce347aed9179987ee8a7f471df862b72d64d"
	GeoLibreStaticV1ImageURI          = "ghcr.io/opengeos/geolibre@sha256:" + GeoLibreStaticV1ImageDigest
	GeoLibreStaticV1Source            = "https://github.com/opengeos/GeoLibre.git#2856ef8c0b227ad18ecf43d4623cf00013c1740e"
	GeoLibreStaticV1ManifestDigest    = "849a9977a72efe5b4e70d28517c1edf038d7ca91c0fc4f53183a3ee3b1095a86"
	GeoLibreStaticV1CommandDigest     = "f0263e3ae0f0ad857da924ade38f9bd24e6643a0ed37829b4bcaf2152d7bd582"
)

// TargetIdentity is the provider-safe, exact identity used for readback
// fencing. It contains no arbitrary provider payload or credentials.
type TargetIdentity struct {
	Kind                   TargetKind `json:"kind" yaml:"kind" mapstructure:"kind"`
	CoreRunnerID           string     `json:"core_runner_id,omitempty"`
	CoreRunnerService      string     `json:"core_runner_service,omitempty"`
	ImageDigest            string     `json:"image_digest,omitempty"`
	AccountID              string     `json:"account_id,omitempty" yaml:"account_id,omitempty" mapstructure:"account_id"`
	Region                 string     `json:"region,omitempty" yaml:"region,omitempty" mapstructure:"region"`
	InstanceID             string     `json:"instance_id,omitempty" yaml:"instance_id,omitempty" mapstructure:"instance_id"`
	Cluster                string     `json:"cluster,omitempty" yaml:"cluster,omitempty" mapstructure:"cluster"`
	Service                string     `json:"service,omitempty" yaml:"service,omitempty" mapstructure:"service"`
	TaskDefinitionRevision string     `json:"task_definition_revision,omitempty" yaml:"task_definition_revision,omitempty" mapstructure:"task_definition_revision"`
	DesiredCount           int64      `json:"desired_count,omitempty"`
	Endpoint               string     `json:"endpoint,omitempty"`
}

func (i TargetIdentity) Validate(kind TargetKind) error {
	if i.Kind != "" && i.Kind != kind {
		return ErrInvalid
	}
	switch kind {
	case TargetCoreRunner:
		// CORE_RUNNER is owned by the native runner control plane and is not an
		// AWS workload target. Reject it here so it cannot reach a provider.
		return ErrInvalid
	case TargetAWSEC2SSM:
		if i.AccountID == "" || i.Region == "" || i.InstanceID == "" {
			return ErrInvalid
		}
	case TargetAWSECS:
		if i.AccountID == "" || i.Region == "" || i.Cluster == "" || i.Service == "" || i.TaskDefinitionRevision == "" {
			return ErrInvalid
		}
		revision, err := strconv.ParseUint(i.TaskDefinitionRevision, 10, 64)
		if err != nil || revision == 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type Port struct {
	Port uint32 `json:"port"`
}
type NetworkGrant struct {
	ReferenceID string `json:"reference_id"`
	Kind        string `json:"kind"`
}
type SecretGrantRef struct {
	ReferenceID string                         `json:"reference_id"`
	Purpose     coreconfirmation.SecretPurpose `json:"purpose"`
	// Revision pins an AWS credential grant to the immutable encrypted
	// credential revision selected when the plan was created. It is not a
	// mutable credential "current" pointer.
	Revision      int64                   `json:"secret_revision,omitempty"`
	BindingDigest coreconfirmation.Digest `json:"binding_digest"`
}
type ActualSnapshot struct {
	WorkloadID        string         `json:"workload_id"`
	Revision          uint64         `json:"revision"`
	State             string         `json:"state"`
	Identity          TargetIdentity `json:"identity"`
	AppliedPlanID     string         `json:"applied_plan_id,omitempty"`
	AppliedPlanDigest string         `json:"applied_plan_digest,omitempty"`
	ReadbackDigest    string         `json:"readback_digest,omitempty"`
	ProviderVersion   string         `json:"provider_version,omitempty"`
	ObservedAt        time.Time      `json:"observed_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}
type ProviderReadback = ActualSnapshot
type ProviderFailure struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

// Plan is immutable after creation. Revision and Digest are retained in every
// operation and task payload so a provider can reject drift before mutation.
type Plan struct {
	ID              string           `json:"plan_id"`
	Revision        uint64           `json:"revision"`
	Digest          string           `json:"digest"`
	Summary         string           `json:"summary"`
	Artifact        string           `json:"artifact,omitempty"`
	Source          string           `json:"source,omitempty"`
	CommandSteps    []string         `json:"command_steps,omitempty"`
	ImageDigest     string           `json:"image_digest,omitempty"`
	ImageURI        string           `json:"image_uri,omitempty"`
	TargetKind      TargetKind       `json:"target_kind"`
	Target          TargetSettings   `json:"target"`
	NetworkGrants   []string         `json:"network_grants,omitempty"`
	SecretGrants    []string         `json:"secret_grants,omitempty"`
	SecretGrantRefs []SecretGrantRef `json:"secret_grant_refs,omitempty"`
	ResourceLimits  ResourceLimits   `json:"resource_limits,omitempty"`
	ExpiresAt       time.Time        `json:"expires_at"`
	CreatedAt       time.Time        `json:"created_at"`
}

type Workload struct {
	ID         string         `json:"workload_id"`
	Revision   uint64         `json:"revision"`
	PlanID     string         `json:"plan_id"`
	PlanDigest string         `json:"plan_digest"`
	TargetKind TargetKind     `json:"target_kind"`
	Identity   TargetIdentity `json:"identity,omitempty"`
	State      string         `json:"state"`
	UpdatedAt  time.Time      `json:"updated_at"`
	Actual     ActualSnapshot `json:"actual"`
}

type Operation struct {
	ID                    string          `json:"operation_id"`
	WorkloadID            string          `json:"workload_id"`
	PlanID                string          `json:"plan_id"`
	Kind                  OperationKind   `json:"kind"`
	PlanRevision          uint64          `json:"plan_revision"`
	PlanDigest            string          `json:"plan_digest"`
	TargetKind            TargetKind      `json:"target_kind"`
	TaskID                string          `json:"task_id"`
	ConfirmationID        string          `json:"confirmation_id"`
	Status                OperationStatus `json:"status"`
	Revision              uint64          `json:"revision"`
	FailureCode           string          `json:"failure_code,omitempty"`
	FailureSummary        string          `json:"failure_summary,omitempty"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
	DispatchState         string          `json:"dispatch_state,omitempty"`
	DispatchAttempt       uint32          `json:"dispatch_attempt,omitempty"`
	DispatchEpoch         uint64          `json:"dispatch_epoch,omitempty"`
	DispatchClaim         string          `json:"dispatch_claim,omitempty"`
	DispatchLeaseUntil    time.Time       `json:"dispatch_lease_until,omitempty"`
	CompletionFingerprint string          `json:"completion_fingerprint,omitempty"`
}

// TaskFence is the exact generic WorkerPool lease presented to a workload
// handler. Workload execution must never mint or replace this lease.
type TaskFence struct {
	TaskID     string
	Holder     string
	Attempt    uint32
	LeaseEpoch uint64
	Revision   uint64
	ExpiresAt  time.Time
}

func (f TaskFence) Valid(now time.Time) bool {
	return ValidUUID(f.TaskID) && f.Holder != "" && f.Attempt > 0 && f.LeaseEpoch > 0 && f.Revision > 0 && !f.ExpiresAt.IsZero() && f.ExpiresAt.After(now.UTC())
}

type Event struct {
	OperationID string          `json:"operation_id"`
	Sequence    uint64          `json:"sequence"`
	Kind        string          `json:"kind"`
	Status      OperationStatus `json:"status"`
	Message     string          `json:"message,omitempty"`
	Readback    json.RawMessage `json:"readback,omitempty"`
	At          time.Time       `json:"at"`
}

type RequestResult struct {
	Operation    Operation
	Task         coretask.Task
	Confirmation coreconfirmation.Confirmation
}

type Readback struct {
	TargetKind      TargetKind      `json:"target_kind"`
	WorkloadID      string          `json:"workload_id"`
	State           string          `json:"state"`
	Identity        TargetIdentity  `json:"identity"`
	ProviderVersion string          `json:"provider_version,omitempty"`
	Details         json.RawMessage `json:"details,omitempty"`
	Digest          string          `json:"digest"`
	At              time.Time       `json:"at"`
}

var (
	ErrInvalid          = errors.New("coreworkload: invalid")
	ErrConflict         = errors.New("coreworkload: conflict")
	ErrNotFound         = errors.New("coreworkload: not found")
	ErrRevisionConflict = errors.New("coreworkload: revision conflict")
	ErrStale            = errors.New("coreworkload: stale plan")
	ErrProvider         = errors.New("coreworkload: provider unavailable")
)

func ValidUUID(v string) bool {
	id, err := uuid.Parse(strings.TrimSpace(v))
	return err == nil && id != uuid.Nil && id.String() == strings.TrimSpace(v)
}
func ValidDigest(v string) bool {
	if len(v) != 64 || strings.ToLower(v) != v {
		return false
	}
	_, e := hex.DecodeString(v)
	return e == nil
}
func validTarget(v TargetKind) bool {
	return v == TargetAWSEC2SSM || v == TargetAWSECS
}

func canonicalDigest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func planInputDigest(p Plan) string {
	image := p.ImageDigest
	if p.ImageURI != "" {
		image += "\x00" + p.ImageURI
	}
	return canonicalDigest(struct {
		Summary, Artifact, Source string
		Commands                  []string
		Image                     string
		Target                    TargetKind
		Settings                  TargetSettings
		Network, Secrets          []string
		SecretRefs                []SecretGrantRef
		Limits                    ResourceLimits
		ExpiresAt                 time.Time
	}{p.Summary, p.Artifact, p.Source, p.CommandSteps, image, p.TargetKind, p.Target, p.NetworkGrants, p.SecretGrants, p.SecretGrantRefs, p.ResourceLimits, p.ExpiresAt.UTC()})
}
func PlanInputDigest(p Plan) string { return planInputDigest(p) }

// CommandStepsDigest binds an ordered, already-normalized command program.
// Fixed server-owned workload profiles use it to reject any command mutation.
func CommandStepsDigest(commands []string) string {
	return canonicalDigest(append([]string(nil), commands...))
}

// CanonicalAWSSecretARN accepts only reference-only Secrets Manager and SSM
// Parameter Store ARNs. It intentionally does not resolve or read values.
func CanonicalAWSSecretARN(v string) (string, bool) {
	if v == "" || v != strings.TrimSpace(v) || strings.ContainsAny(v, "\r\n\x00 \t") {
		return "", false
	}
	parts := strings.SplitN(v, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || (parts[1] != "aws" && parts[1] != "aws-us-gov" && parts[1] != "aws-cn") || (parts[2] != "secretsmanager" && parts[2] != "ssm") || parts[3] == "" || len(parts[4]) != 12 || strings.ContainsAny(parts[3]+parts[4], "\r\n\x00 \t") || strings.Contains(parts[5], "//") {
		return "", false
	}
	for _, r := range parts[4] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	if parts[2] == "secretsmanager" && !strings.HasPrefix(parts[5], "secret:") || parts[2] == "ssm" && !strings.HasPrefix(parts[5], "parameter/") {
		return "", false
	}
	return v, true
}

// SecretGrantBindingDigest is the stable confirmation binding for an ARN
// reference. Legacy UUID references retain their historical digest behavior.
func SecretGrantBindingDigest(reference string, purpose coreconfirmation.SecretPurpose) string {
	arn, ok := CanonicalAWSSecretARN(reference)
	if !ok {
		return ""
	}
	return canonicalDigest(struct {
		Version string
		ARN     string
		Purpose coreconfirmation.SecretPurpose
	}{"aws-secret-binding-v1", arn, purpose})
}

func RedactText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "provider operation failed"
}

// SafeFailure converts provider-controlled text into a small stable allowlist
// suitable for task, event, and operation persistence.
func SafeFailure(code, summary string) (string, string) {
	switch code {
	case "", "provider_uncertain", "provider_error", "canceled", "user_canceled":
	default:
		code = "provider_error"
	}
	if code == "" {
		return "", ""
	}
	switch code {
	case "canceled", "user_canceled":
		return code, "operation canceled"
	case "provider_uncertain":
		return code, "provider outcome requires reconciliation"
	default:
		return code, "provider operation failed"
	}
}
func SanitizeReadback(r Readback) Readback {
	out := Readback{TargetKind: r.TargetKind, WorkloadID: r.WorkloadID, State: r.State, Identity: r.Identity, ProviderVersion: r.ProviderVersion, At: r.At}
	out.Digest = ReadbackDigest(out)
	return out
}

func (p Plan) Normalize() (Plan, error) {
	p.ID = strings.TrimSpace(p.ID)
	p.Summary = strings.TrimSpace(p.Summary)
	p.Artifact = strings.TrimSpace(p.Artifact)
	p.Source = strings.TrimSpace(p.Source)
	p.ImageDigest = strings.TrimSpace(p.ImageDigest)
	p.ImageURI = strings.TrimSpace(p.ImageURI)
	if p.ImageURI == "" && strings.Contains(p.ImageDigest, "@sha256:") {
		p.ImageURI = p.ImageDigest
	}
	if p.ID != "" && !ValidUUID(p.ID) || p.Summary == "" || len(p.Summary) > 4096 || !validTarget(p.TargetKind) || p.Revision == 0 || p.ExpiresAt.IsZero() || p.ExpiresAt.Location() != time.UTC {
		return Plan{}, ErrInvalid
	}
	if p.TargetKind == TargetCoreRunner && len(p.CommandSteps) == 0 && p.ImageDigest == "" {
		return Plan{}, ErrInvalid
	}
	if p.TargetKind == TargetAWSECS {
		// Legacy read-only plans may contain only ImageDigest.  Typed ECS
		// mutation plans opt into the strict Fargate contract via ImageURI or
		// any ECS target field; providers reject legacy mutation attempts.
		typed := p.ImageURI != "" || p.Target.ECSClusterARN != "" || p.Target.ECSServiceName != ""
		if typed && (p.ImageURI == "" || !validPinnedImageURI(p.ImageURI)) {
			return Plan{}, ErrInvalid
		}
		if typed && !validFargateCPUAndMemory(p.ResourceLimits.CPU, p.ResourceLimits.MemoryMB) {
			return Plan{}, ErrInvalid
		}
	}
	if p.TargetKind != TargetAWSECS && p.ImageDigest != "" && !ValidDigest(p.ImageDigest) {
		return Plan{}, ErrInvalid
	}
	if p.ExpiresAt.Before(time.Now().UTC()) { /* expiry is checked by service; retain historical plans */
	}
	p.NetworkGrants = normalizeStrings(p.NetworkGrants)
	p.SecretGrants = normalizeStrings(p.SecretGrants)
	p.CommandSteps = append([]string(nil), p.CommandSteps...)
	for _, s := range append(append([]string{}, p.CommandSteps...), append(p.NetworkGrants, p.SecretGrants...)...) {
		if strings.TrimSpace(s) == "" || len(s) > 4096 || strings.ContainsAny(s, "\r\n\x00") {
			return Plan{}, ErrInvalid
		}
	}
	for _, id := range p.SecretGrants {
		if !ValidUUID(id) {
			return Plan{}, ErrInvalid
		}
	}
	for _, ref := range p.SecretGrantRefs {
		arnRef := false
		if _, ok := CanonicalAWSSecretARN(ref.ReferenceID); ok {
			arnRef = true
		}
		if (!ValidUUID(ref.ReferenceID) && !arnRef) || !ref.BindingDigest.Valid() || !validSecretPurpose(ref.Purpose) {
			return Plan{}, ErrInvalid
		}
		if ref.Purpose == coreconfirmation.SecretPurposeAWSCredential && (!ValidUUID(ref.ReferenceID) || ref.Revision < 1) {
			return Plan{}, ErrInvalid
		}
		if ref.Purpose != coreconfirmation.SecretPurposeAWSCredential && ref.Revision != 0 {
			return Plan{}, ErrInvalid
		}
		if arnRef && string(ref.BindingDigest) != SecretGrantBindingDigest(ref.ReferenceID, ref.Purpose) {
			return Plan{}, ErrInvalid
		}
	}
	if p.TargetKind == TargetAWSEC2SSM || p.TargetKind == TargetAWSECS {
		awsCredentials := 0
		for _, ref := range p.SecretGrantRefs {
			if ref.Purpose == coreconfirmation.SecretPurposeAWSCredential {
				awsCredentials++
			}
		}
		if awsCredentials != 1 {
			return Plan{}, ErrInvalid
		}
	}
	if p.Target.EC2CleanupProfile == EC2CleanupProfileGeoLibreStaticV1 && !validGeoLibreStaticV1Plan(p) {
		return Plan{}, ErrInvalid
	}
	for _, port := range p.Target.Ports {
		if port < 1 || port > 65535 {
			return Plan{}, ErrInvalid
		}
	}
	if p.ResourceLimits.CPU < 0 || p.ResourceLimits.MemoryMB < 0 || p.ResourceLimits.Processes < 0 || p.ResourceLimits.DiskMB < 0 || p.ResourceLimits.TimeoutS < 0 || p.ResourceLimits.OutputMB < 0 {
		return Plan{}, ErrInvalid
	}
	if err := p.Target.Identity.Validate(p.TargetKind); err != nil {
		return Plan{}, err
	}
	if err := validateTargetSettings(p.TargetKind, p.Target); err != nil {
		return Plan{}, err
	}
	if p.Digest == "" {
		image := p.ImageDigest
		if p.ImageURI != "" {
			image += "\x00" + p.ImageURI
		}
		p.Digest = canonicalDigest(struct {
			Summary, Artifact, Source string
			Commands                  []string
			Image                     string
			Target                    TargetKind
			Settings                  TargetSettings
			Network, Secrets          []string
			SecretRefs                []SecretGrantRef
			Limits                    ResourceLimits
			ExpiresAt                 time.Time
		}{p.Summary, p.Artifact, p.Source, p.CommandSteps, image, p.TargetKind, p.Target, p.NetworkGrants, p.SecretGrants, p.SecretGrantRefs, p.ResourceLimits, p.ExpiresAt.UTC()})
	}
	if !ValidDigest(p.Digest) {
		return Plan{}, ErrInvalid
	}
	return p, nil
}

func validGeoLibreStaticV1Plan(p Plan) bool {
	manifestDigest := p.Target.Labels["dirextalk:manifest-digest"]
	commandDigest := p.Target.Labels["dirextalk:command-digest"]
	ownerBinding := strings.TrimPrefix(p.Target.RequiredInstanceTags["owner"], "sha256:")
	planID := p.Target.RequiredInstanceTags["dirextalk:plan-id"]
	provisionID := p.Target.Labels["dirextalk:provision-id"]
	if len(p.Target.NetworkGrantDetails) != 1 || len(p.NetworkGrants) != 1 {
		return false
	}
	networkGrant := p.Target.NetworkGrantDetails[0]
	return p.TargetKind == TargetAWSEC2SSM &&
		p.Target.EC2SystemdService == GeoLibreStaticV1Service &&
		p.ImageURI == GeoLibreStaticV1ImageURI &&
		p.ImageDigest == GeoLibreStaticV1ImageDigest &&
		p.Source == GeoLibreStaticV1Source &&
		manifestDigest == GeoLibreStaticV1ManifestDigest &&
		p.Artifact == "geolibre-manifest:"+GeoLibreStaticV1ManifestDigest &&
		commandDigest == GeoLibreStaticV1CommandDigest &&
		CommandStepsDigest(p.CommandSteps) == GeoLibreStaticV1CommandDigest &&
		p.Target.Labels["dirextalk:release"] == GeoLibreStaticV1Release &&
		p.Target.Labels["dirextalk:exposure"] == "public-unauthenticated-http" &&
		p.Target.Labels["dirextalk:sidecar"] == "disabled" &&
		p.Target.RequiredInstanceTags["managed"] == "true" &&
		p.Target.RequiredInstanceTags["service"] == "geolibre" &&
		strings.HasPrefix(p.Target.RequiredInstanceTags["owner"], "sha256:") &&
		ValidDigest(ownerBinding) &&
		ValidUUID(planID) &&
		ValidUUID(provisionID) &&
		networkGrant.Kind == "aws_security_group" &&
		validEC2ResourceID(networkGrant.ReferenceID, "sg-") &&
		p.NetworkGrants[0] == "security-group:"+networkGrant.ReferenceID &&
		p.Summary == GeoLibreStaticV1Summary(provisionID)
}

func validEC2ResourceID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(value, prefix)
	if len(suffix) != 8 && len(suffix) != 17 {
		return false
	}
	for _, char := range suffix {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validPinnedImageURI(v string) bool {
	v = strings.TrimSpace(v)
	idx := strings.LastIndex(v, "@sha256:")
	return idx > 0 && idx+len("@sha256:")+64 == len(v) && ValidDigest(v[idx+len("@sha256:"):]) && !strings.ContainsAny(v[:idx], "\r\n\x00 ")
}

func validFargateCPUAndMemory(cpu, memory int64) bool {
	if cpu <= 0 || memory <= 0 {
		return false
	}
	allowed := map[int64]map[int64]bool{
		256:   {512: true, 1024: true, 1536: true, 2048: true},
		512:   {1024: true, 2048: true, 3072: true, 4096: true},
		1024:  {2048: true, 3072: true, 4096: true, 5120: true, 6144: true, 7168: true, 8192: true},
		2048:  {4096: true, 5120: true, 6144: true, 7168: true, 8192: true, 9216: true, 10240: true, 11264: true, 12288: true, 13312: true, 14336: true, 15360: true, 16384: true},
		4096:  {8192: true, 9216: true, 10240: true, 11264: true, 12288: true, 13312: true, 14336: true, 15360: true, 16384: true, 17408: true, 18432: true, 19456: true, 20480: true, 21504: true, 22528: true, 23552: true, 24576: true, 25600: true, 26624: true, 27648: true, 28672: true, 29696: true, 30720: true},
		8192:  {16384: true, 17408: true, 18432: true, 19456: true, 20480: true, 21504: true, 22528: true, 23552: true, 24576: true, 25600: true, 26624: true, 27648: true, 28672: true, 29696: true, 30720: true, 32768: true, 34816: true, 36864: true, 38912: true, 40960: true, 43008: true, 45056: true, 47104: true, 49152: true, 51200: true, 53248: true, 55296: true, 57344: true, 59392: true, 61440: true},
		16384: {32768: true, 34816: true, 36864: true, 38912: true, 40960: true, 43008: true, 45056: true, 47104: true, 49152: true, 51200: true, 53248: true, 55296: true, 57344: true, 59392: true, 61440: true, 65536: true, 69632: true, 73728: true, 77824: true, 81920: true, 86016: true, 90112: true, 94208: true, 98304: true, 102400: true, 106496: true, 110592: true, 114688: true, 118784: true, 122880: true},
		32768: {65536: true, 69632: true, 73728: true, 77824: true, 81920: true, 86016: true, 90112: true, 94208: true, 98304: true, 102400: true, 106496: true, 110592: true, 114688: true, 118784: true, 122880: true},
	}
	return allowed[cpu][memory]
}

func validateTargetSettings(kind TargetKind, t TargetSettings) error {
	switch kind {
	case TargetAWSEC2SSM:
		if !validDocumentVersion(t.EC2DocumentVersion) || !validSystemdService(t.EC2SystemdService) || len(t.RequiredInstanceTags) == 0 {
			return ErrInvalid
		}
		if t.EC2CleanupProfile != "" && t.EC2CleanupProfile != EC2CleanupProfileGeoLibreStaticV1 {
			return ErrInvalid
		}
		for k, v := range t.RequiredInstanceTags {
			if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" || len(k) > 128 || len(v) > 256 || strings.ContainsAny(k+v, "\r\n\x00") {
				return ErrInvalid
			}
		}
	case TargetAWSECS:
		if !validARN(t.ECSClusterARN) || strings.TrimSpace(t.ECSServiceName) == "" || strings.TrimSpace(t.ECSTaskFamily) == "" || !validPlatformVersion(t.ECSPlatformVersion) || len(t.ECSSubnetIDs) == 0 || len(t.ECSSecurityGroupIDs) == 0 || t.ECSDesiredCount < 1 {
			return ErrInvalid
		}
		if t.ECSTargetGroupARN != "" && (t.ECSTargetGroupPort < 1 || t.ECSTargetGroupPort > 65535 || !validARN(t.ECSTargetGroupARN)) {
			return ErrInvalid
		}
		if t.ECSTaskRoleARN != "" && !validARN(t.ECSTaskRoleARN) || t.ECSExecutionRoleARN != "" && !validARN(t.ECSExecutionRoleARN) {
			return ErrInvalid
		}
		for _, id := range append(append([]string{}, t.ECSSubnetIDs...), t.ECSSecurityGroupIDs...) {
			if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "\r\n\x00 ") {
				return ErrInvalid
			}
		}
	}
	return nil
}

// ValidateCanonicalTarget rejects plans whose identity and provider settings
// disagree. Provider clients must call this before constructing SDK clients.
func (t TargetSettings) ValidateCanonicalTarget(kind TargetKind) error {
	if err := t.Identity.Validate(kind); err != nil {
		return err
	}
	if kind == TargetAWSEC2SSM {
		if t.Region != "" && t.Region != t.Identity.Region || t.AccountID != "" && t.AccountID != t.Identity.AccountID || t.InstanceID != "" && t.InstanceID != t.Identity.InstanceID {
			return ErrInvalid
		}
		if t.Region == "" || t.AccountID == "" || t.InstanceID == "" {
			return ErrInvalid
		}
	}
	if kind == TargetAWSECS {
		if t.Region != "" && t.Region != t.Identity.Region || t.AccountID != "" && t.AccountID != t.Identity.AccountID || t.ECSClusterARN == "" || t.ECSServiceName == "" || t.ECSClusterARN != t.Identity.Cluster || t.ECSServiceName != t.Identity.Service {
			return ErrInvalid
		}
		if t.Region == "" || t.AccountID == "" {
			return ErrInvalid
		}
	}
	return nil
}

// ValidateProviderTarget validates the complete typed target contract before
// a provider is constructed or probed.  Readiness configuration uses this
// boundary so a partial target remains disabled instead of being advertised.
func (t TargetSettings) ValidateProviderTarget(kind TargetKind) error {
	if err := t.ValidateCanonicalTarget(kind); err != nil {
		return err
	}
	return validateTargetSettings(kind, t)
}

func validPlatformVersion(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 || v == "LATEST" {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func validDocumentVersion(v string) bool {
	if v == "" || strings.ContainsAny(v, "$\r\n\x00 ") {
		return false
	}
	_, err := strconv.ParseUint(v, 10, 64)
	return err == nil
}

func validSystemdService(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < len("a.service") || len(v) > 255 || !strings.HasSuffix(v, ".service") || strings.ContainsAny(v, "/\\\r\n\x00 ") {
		return false
	}
	for _, r := range v[:len(v)-len(".service")] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func validARN(v string) bool {
	return strings.HasPrefix(strings.TrimSpace(v), "arn:aws:") && !strings.ContainsAny(v, "\r\n\x00 ") && len(v) <= 2048
}
func validSecretPurpose(v coreconfirmation.SecretPurpose) bool {
	switch v {
	case coreconfirmation.SecretPurposeModelAPIKey, coreconfirmation.SecretPurposeMCPCredential, coreconfirmation.SecretPurposeSkillSecret, coreconfirmation.SecretPurposeAWSCredential, coreconfirmation.SecretPurposeOtherExtensionSecret:
		return true
	default:
		return false
	}
}
func normalizeStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
func (p Plan) Validate() error { _, e := p.Normalize(); return e }

func (o Operation) Validate() error {
	if !ValidUUID(o.ID) || !ValidUUID(o.WorkloadID) || !ValidUUID(o.PlanID) || !ValidUUID(o.TaskID) || !ValidUUID(o.ConfirmationID) || !validTarget(o.TargetKind) || !ValidDigest(o.PlanDigest) || o.PlanRevision == 0 || o.Revision == 0 || o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	if o.Kind != OperationApply && o.Kind != OperationDestroy {
		return ErrInvalid
	}
	return nil
}

func bindingForOperation(p Plan, workloadID string, kind OperationKind) coreconfirmation.Binding {
	param := canonicalDigest(struct {
		Kind     OperationKind
		Plan     Plan
		Workload string
	}{kind, p, workloadID})
	network := canonicalDigest(struct {
		Grants []string
		Limits ResourceLimits
	}{p.NetworkGrants, p.ResourceLimits})
	secret := canonicalDigest(struct {
		Legacy []string
		Typed  []SecretGrantRef
	}{p.SecretGrants, p.SecretGrantRefs})
	grants := make([]coreconfirmation.SecretGrant, 0, len(p.SecretGrants)+len(p.SecretGrantRefs))
	for _, ref := range p.SecretGrantRefs {
		if ValidUUID(ref.ReferenceID) && ref.BindingDigest.Valid() {
			grants = append(grants, coreconfirmation.SecretGrant{ReferenceID: ref.ReferenceID, Purpose: ref.Purpose, Revision: ref.Revision, BindingDigest: ref.BindingDigest})
		}
	}
	for _, id := range p.SecretGrants {
		if ValidUUID(id) {
			grants = append(grants, coreconfirmation.SecretGrant{ReferenceID: id, Purpose: coreconfirmation.SecretPurposeOtherExtensionSecret, BindingDigest: coreconfirmation.Digest(secret)})
		}
	}
	return coreconfirmation.Binding{OperationDomain: "workload:" + string(kind), TargetID: workloadID, TargetRevision: int64(p.Revision), SourceVersion: "core-v1", ContentDigest: coreconfirmation.Digest(p.Digest), ParameterDigest: coreconfirmation.Digest(param), NetworkDigest: coreconfirmation.Digest(network), SecretGrantDigest: coreconfirmation.Digest(secret), NetworkGrants: append([]string(nil), p.NetworkGrants...), SecretGrants: grants}
}

// BindingForPlan returns the exact immutable confirmation snapshot used by
// both memory and PostgreSQL request paths.
func BindingForPlan(p Plan, workloadID string) coreconfirmation.Binding {
	return bindingForOperation(p, workloadID, OperationApply)
}

func BindingForOperation(p Plan, workloadID string, kind OperationKind) coreconfirmation.Binding {
	return bindingForOperation(p, workloadID, kind)
}
