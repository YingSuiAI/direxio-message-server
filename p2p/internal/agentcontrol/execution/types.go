// Package execution contains the server-owned execution-plan/v2 domain.
// It is deliberately transport and persistence agnostic: secrets are refs,
// execution is typed, and all immutable records are digest-bound.
package execution

import "time"

const SchemaVersion = "execution-plan/v2"

const RecipeGenericContainerService = "generic-container-service"

const (
	// TargetKindAWSComputeReservation is the non-executable first revision in
	// the two-plan EC2 bootstrap model. Only verified provider readback may
	// materialize the same target ID as an aws_ec2_instance revision.
	TargetKindAWSComputeReservation = "aws_compute_reservation"
	TargetKindAWSEC2Instance        = "aws_ec2_instance"

	// NetworkPolicyModeObservedHTTPSEgress describes the network boundary the
	// current AWS profile actually enforces: public TCP/443 egress to any
	// destination. Plans using this mode must bind PublicHTTPSWildcardHost and
	// must not claim that the security group enforces a registry hostname or
	// path. Artifact/source identities remain independently digest-pinned.
	NetworkPolicyModeObservedHTTPSEgress = "observed_https_egress"
	PublicHTTPSWildcardHost              = "*"
	ObservationFactHTTPSEgress           = "https_egress"
	ObservationFactHTTPSEgressValue      = "security_group_public_tcp_443"
	ObservationFactSecurityGroupDigest   = "security_group_set_digest"
	ObservationFactVCPUCount             = "vcpu_count"
	ObservationFactMemoryMiB             = "memory_mib"
	ObservationFactRootVolumeGiB         = "root_volume_gib"
	ObservationFactAvailabilityZone      = "availability_zone"
	AWSAL2023X8664AMIParameter           = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
)

type Digest string

type PlanPurpose string

const (
	PurposeJob     PlanPurpose = "job"
	PurposeService PlanPurpose = "service"
)

type Risk string

const (
	RiskR0 Risk = "R0"
	RiskR1 Risk = "R1"
	RiskR2 Risk = "R2"
	RiskR3 Risk = "R3"
	RiskR4 Risk = "R4"
)

type Gate string

const (
	GateNone                      Gate = "none"
	GateResourcePurchase          Gate = "resource_purchase"
	GateSecretAccess              Gate = "secret_access"
	GateExternalAuth              Gate = "external_auth"
	GateRemoteExecution           Gate = "remote_execution"
	GateRemotePrivilegedExecution Gate = "remote_privileged_execution"
	GatePublicNetworkExposure     Gate = "public_network_exposure"
	GateDNSChange                 Gate = "dns_change"
	GateTLSCertificateIssue       Gate = "tls_certificate_issue"
	GateDataMigration             Gate = "data_migration"
	GateProductionCutover         Gate = "production_cutover"
	GateRepositoryWrite           Gate = "repository_write"
	GateServiceDestroy            Gate = "service_destroy"
	GateRollback                  Gate = "rollback"
)

type PlanStatus string

const (
	PlanDraft      PlanStatus = "draft"
	PlanReady      PlanStatus = "ready"
	PlanExpired    PlanStatus = "expired"
	PlanSuperseded PlanStatus = "superseded"
)

type RunStatus string

type RunOperation string

type TriggerKind string

const (
	TriggerManual   TriggerKind = "manual"
	TriggerSchedule TriggerKind = "schedule"
	TriggerRetry    TriggerKind = "retry"
	TriggerRollback TriggerKind = "rollback"
)

// RunOperationAllowedByPlan keeps the first production container slice
// initial-deploy-only. Its executor deliberately has no destructive replace
// or cleanup rollback path; upgrade/repair/destroy/rollback require a future
// recipe with a versioned or blue-green lifecycle model.
func RunOperationAllowedByPlan(plan ExecutionPlan, operation RunOperation) bool {
	for _, recipe := range plan.Recipes {
		if recipe.ID == RecipeGenericContainerService {
			return operation == RunOperationExecute || operation == RunOperationDeploy
		}
	}
	return true
}

const (
	RunOperationExecute  RunOperation = "execute"
	RunOperationDeploy   RunOperation = "deploy"
	RunOperationUpgrade  RunOperation = "upgrade"
	RunOperationRepair   RunOperation = "repair"
	RunOperationDestroy  RunOperation = "destroy"
	RunOperationRollback RunOperation = "rollback"
)

const (
	RunPending     RunStatus = "pending"
	RunWaitingUser RunStatus = "waiting_user"
	RunQueued      RunStatus = "queued"
	RunRunning     RunStatus = "running"
	RunSucceeded   RunStatus = "succeeded"
	RunFailed      RunStatus = "failed"
	RunUncertain   RunStatus = "uncertain"
	RunCanceled    RunStatus = "canceled"
	RunRejected    RunStatus = "rejected"
	RunExpired     RunStatus = "expired"
)

type StageStatus string

type AttemptStatus string

const (
	AttemptPending   AttemptStatus = "pending"
	AttemptRunning   AttemptStatus = "running"
	AttemptSucceeded AttemptStatus = "succeeded"
	AttemptFailed    AttemptStatus = "failed"
	AttemptUncertain AttemptStatus = "uncertain"
	AttemptCanceled  AttemptStatus = "canceled"
)

// CanTransition limits lifecycle movement to forward, durable states. Callers
// must create a new revision rather than rewriting a terminal record.
func (s PlanStatus) CanTransition(to PlanStatus) bool {
	switch s {
	case PlanDraft:
		return to == PlanReady || to == PlanExpired || to == PlanSuperseded
	case PlanReady:
		return to == PlanExpired || to == PlanSuperseded
	}
	return false
}

func (s RunStatus) CanTransition(to RunStatus) bool {
	switch s {
	case RunPending:
		return to == RunWaitingUser || to == RunQueued || to == RunCanceled || to == RunRejected || to == RunExpired
	case RunWaitingUser:
		return to == RunQueued || to == RunCanceled || to == RunRejected || to == RunExpired
	case RunQueued:
		return to == RunRunning || to == RunCanceled || to == RunExpired
	case RunRunning:
		return to == RunSucceeded || to == RunFailed || to == RunUncertain || to == RunCanceled
	case RunUncertain:
		// Reconciliation is the only caller allowed to use this transition. It
		// resolves the exact persisted provider operation; it never dispatches a
		// replacement mutation.
		return to == RunRunning || to == RunSucceeded || to == RunFailed || to == RunCanceled
	}
	return false
}

func (s StageStatus) CanTransition(to StageStatus) bool {
	switch s {
	case StageBlocked:
		return to == StageWaitingUser || to == StageQueued || to == StageSkipped || to == StageCanceled || to == StageExpired
	case StageWaitingUser:
		return to == StageQueued || to == StageRejected || to == StageCanceled || to == StageExpired
	case StageQueued:
		return to == StageRunning || to == StageCanceled || to == StageExpired
	case StageRunning:
		return to == StageSucceeded || to == StageFailed || to == StageUncertain || to == StageCanceled
	case StageUncertain:
		return to == StageSucceeded || to == StageFailed || to == StageCanceled
	}
	return false
}

type ReceiptStatus string

const (
	ReceiptAccepted  ReceiptStatus = "accepted"
	ReceiptSucceeded ReceiptStatus = "succeeded"
	ReceiptFailed    ReceiptStatus = "failed"
	ReceiptUncertain ReceiptStatus = "uncertain"
	ReceiptCanceled  ReceiptStatus = "canceled"
)

const (
	StageBlocked     StageStatus = "blocked"
	StageWaitingUser StageStatus = "waiting_user"
	StageQueued      StageStatus = "queued"
	StageRunning     StageStatus = "running"
	StageSucceeded   StageStatus = "succeeded"
	StageFailed      StageStatus = "failed"
	StageUncertain   StageStatus = "uncertain"
	StageSkipped     StageStatus = "skipped"
	StageCanceled    StageStatus = "canceled"
	StageRejected    StageStatus = "rejected"
	StageExpired     StageStatus = "expired"
)

type SourceRef struct {
	Kind           string `json:"kind"`
	Location       string `json:"location"`
	Commit         string `json:"commit"`
	ArtifactID     string `json:"artifact_id,omitempty"`
	ArtifactDigest Digest `json:"artifact_digest,omitempty"`
	Immutable      bool   `json:"immutable"`
}

type ResourceRequirement struct {
	CPU          string `json:"cpu,omitempty"`
	Memory       string `json:"memory,omitempty"`
	Disk         string `json:"disk,omitempty"`
	GPU          string `json:"gpu,omitempty"`
	Architecture string `json:"architecture,omitempty"`
}

type ProjectAnalysis struct {
	AnalysisID            string              `json:"analysis_id"`
	ProjectID             string              `json:"project_id"`
	Source                SourceRef           `json:"source"`
	DetectedStacks        []string            `json:"detected_stacks,omitempty"`
	Build                 ResourceRequirement `json:"build,omitempty"`
	Runtime               ResourceRequirement `json:"runtime,omitempty"`
	Dependencies          []string            `json:"dependencies,omitempty"`
	Ports                 []int               `json:"ports,omitempty"`
	EnvironmentNames      []string            `json:"environment_names,omitempty"`
	SecretPurposes        []string            `json:"secret_purposes,omitempty"`
	SecretRefs            []string            `json:"secret_refs,omitempty"`
	Volumes               []string            `json:"volumes,omitempty"`
	Migrations            []string            `json:"migrations,omitempty"`
	Probes                []string            `json:"probes,omitempty"`
	Exposure              string              `json:"exposure,omitempty"`
	Assumptions           []string            `json:"assumptions,omitempty"`
	BlockingUncertainties []string            `json:"blocking_uncertainties,omitempty"`
	Revision              uint64              `json:"revision"`
	Digest                Digest              `json:"digest"`
	CreatedAt             time.Time           `json:"created_at"`
	UpdatedAt             time.Time           `json:"updated_at"`
}

type CostQuote struct {
	Amount    string    `json:"amount,omitempty"`
	Currency  string    `json:"currency,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type PlacementOption struct {
	Region    string    `json:"region,omitempty"`
	Spec      string    `json:"spec,omitempty"`
	Disk      string    `json:"disk,omitempty"`
	Network   string    `json:"network,omitempty"`
	CostQuote CostQuote `json:"cost_quote,omitempty"`
}

type PlacementRecommendation struct {
	Kind            string          `json:"kind"`
	Minimum         PlacementOption `json:"minimum"`
	Recommended     PlacementOption `json:"recommended"`
	HighPerformance PlacementOption `json:"high_performance"`
}

type CredentialRef struct {
	Ref           string `json:"ref"`
	Purpose       string `json:"purpose"`
	Revision      uint64 `json:"revision"`
	BindingDigest Digest `json:"binding_digest"`
}

type AIAuthMode string

const (
	AIAuthModeAPIKey   AIAuthMode = "api_key"
	AIAuthModeAuthGate AIAuthMode = "auth_gate"

	AISecretPurposeProviderAPIKey = "ai_provider_api_key"
	AIExternalAuthPending         = "pending_external_auth"
)

// AIConfiguration is plan metadata only. API key bytes are accepted only by
// the dedicated secret mutation and are never part of a plan snapshot.
type AIConfiguration struct {
	Mode                AIAuthMode `json:"mode"`
	Provider            string     `json:"provider"`
	SecretRef           string     `json:"secret_ref,omitempty"`
	SecretRevision      uint64     `json:"secret_revision,omitempty"`
	SecretPurpose       string     `json:"secret_purpose,omitempty"`
	SecretBindingDigest Digest     `json:"secret_binding_digest,omitempty"`
	Status              string     `json:"status,omitempty"`
}

func (c AIConfiguration) CredentialRef() CredentialRef {
	return CredentialRef{Ref: c.SecretRef, Purpose: c.SecretPurpose, Revision: c.SecretRevision, BindingDigest: c.SecretBindingDigest}
}

type NetworkPolicy struct {
	Mode  string         `json:"mode,omitempty"`
	Allow []NetworkGrant `json:"allow,omitempty"`
	Deny  []NetworkGrant `json:"deny,omitempty"`
}

// ComputeReservation is a server-owned, immutable EC2 purchase selection.
// Account, region and credentials live on ExecutionTarget and are therefore
// included in the same target digest without being repeated here.
type ComputeReservation struct {
	InfrastructureProfileID string    `json:"infrastructure_profile_id"`
	AMIParameter            string    `json:"ami_parameter"`
	InstanceType            string    `json:"instance_type"`
	AvailabilityZone        string    `json:"availability_zone"`
	VolumeGiB               uint32    `json:"volume_gib"`
	Architecture            string    `json:"architecture"`
	ManagementTransport     string    `json:"management_transport"`
	PublicIP                bool      `json:"public_ip"`
	PublicInbound           bool      `json:"public_inbound"`
	CostQuote               CostQuote `json:"cost_quote"`
}

// NetworkGrant is a canonical, digest-bound egress allowance. It deliberately
// names the endpoint rather than accepting an opaque shell-era grant string.
type NetworkGrant struct {
	Scheme     string `json:"scheme"`
	Host       string `json:"host"`
	Port       uint16 `json:"port"`
	PathPrefix string `json:"path_prefix,omitempty"`
	Scope      string `json:"scope"`
	Digest     Digest `json:"digest"`
}

type ExecutionTarget struct {
	ID                      string              `json:"id"`
	Provider                string              `json:"provider"`
	Kind                    string              `json:"kind"`
	InfrastructureProfileID string              `json:"infrastructure_profile_id,omitempty"`
	AccountID               string              `json:"account_id,omitempty"`
	Region                  string              `json:"region,omitempty"`
	Architecture            string              `json:"architecture,omitempty"`
	Capabilities            []string            `json:"capabilities,omitempty"`
	CredentialRefs          []CredentialRef     `json:"credential_refs,omitempty"`
	Network                 NetworkPolicy       `json:"network,omitempty"`
	ComputeReservation      *ComputeReservation `json:"compute_reservation,omitempty"`
	Revision                uint64              `json:"revision"`
	Digest                  Digest              `json:"digest"`
}

type TargetObservation struct {
	TargetID       string            `json:"target_id"`
	TargetRevision uint64            `json:"target_revision"`
	ObservedAt     time.Time         `json:"observed_at"`
	Facts          map[string]string `json:"facts,omitempty"`
	State          string            `json:"state"`
	Partial        bool              `json:"partial"`
	Stale          bool              `json:"stale"`
	Warnings       []string          `json:"warnings,omitempty"`
	Digest         Digest            `json:"digest"`
}

// TargetObservationRef pins an executable step to the immutable target state
// observed before the step was approved.
type TargetObservationRef struct {
	ObservationID     string `json:"observation_id"`
	TargetID          string `json:"target_id"`
	TargetRevision    uint64 `json:"target_revision"`
	ObservationDigest Digest `json:"observation_digest"`
}

type SkillRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  Digest `json:"digest"`
}
type RecipeRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  Digest `json:"digest"`
}

type ArtifactRef struct {
	ID        string `json:"id"`
	Digest    Digest `json:"digest"`
	MediaType string `json:"media_type,omitempty"`
	Size      int64  `json:"size,omitempty"`
	URI       string `json:"uri,omitempty"`
	Immutable bool   `json:"immutable"`
}
type OutputDeclaration struct {
	Key       string `json:"key"`
	MediaType string `json:"media_type"`
	MaxSize   uint64 `json:"max_size"`
	Required  bool   `json:"required"`
}

type ExecutionPlan struct {
	SchemaVersion   string                  `json:"schema_version"`
	ID              string                  `json:"id"`
	Revision        uint64                  `json:"revision"`
	OwnerID         string                  `json:"owner_id"`
	ProjectID       string                  `json:"project_id"`
	AnalysisID      string                  `json:"analysis_id"`
	Purpose         PlanPurpose             `json:"purpose"`
	DeploymentID    string                  `json:"deployment_id,omitempty"`
	AIConfiguration *AIConfiguration        `json:"ai_configuration,omitempty"`
	Placement       PlacementRecommendation `json:"placement"`
	Targets         []ExecutionTarget       `json:"targets,omitempty"`
	Artifacts       []ArtifactRef           `json:"artifacts,omitempty"`
	Skills          []SkillRef              `json:"skills,omitempty"`
	Recipes         []RecipeRef             `json:"recipes,omitempty"`
	Stages          []ExecutionStage        `json:"stages"`
	Outputs         []OutputDeclaration     `json:"outputs,omitempty"`
	CreatedAt       time.Time               `json:"created_at,omitempty"`
	ExpiresAt       time.Time               `json:"expires_at"`
	Status          PlanStatus              `json:"status"`
	Digest          Digest                  `json:"digest"`
}

type ExecutionStage struct {
	StageKey       string          `json:"stage_key"`
	Revision       uint64          `json:"revision"`
	Title          string          `json:"title,omitempty"`
	Kind           string          `json:"kind"`
	Risk           Risk            `json:"risk"`
	Gate           Gate            `json:"gate"`
	Effects        []Gate          `json:"effects,omitempty"`
	DependsOn      []string        `json:"depends_on,omitempty"`
	TargetID       string          `json:"target_id"`
	TargetRevision uint64          `json:"target_revision"`
	TargetDigest   Digest          `json:"target_digest"`
	Steps          []ExecutionStep `json:"steps"`
	RollbackSteps  []ExecutionStep `json:"rollback_steps,omitempty"`
	RollbackPolicy *RollbackPolicy `json:"rollback_policy,omitempty"`
	Probes         []string        `json:"probes,omitempty"`
	TimeoutSeconds uint64          `json:"timeout_seconds"`
	Digest         Digest          `json:"digest"`
}

// RollbackPolicy makes rollback an inert, independently approved declaration;
// forward stage execution must never execute RollbackSteps.
type RollbackPolicy struct {
	Risk   Risk   `json:"risk"`
	Gate   Gate   `json:"gate"`
	Digest Digest `json:"digest"`
}

type StepKind string

const (
	StepTargetInspect    StepKind = "target.inspect"
	StepComputeProvision StepKind = "compute.provision"
	StepComputeDestroy   StepKind = "compute.destroy"
	StepSourceFetch      StepKind = "source.fetch"
	StepArtifactUpload   StepKind = "artifact.upload"
	StepPackageEnsure    StepKind = "package.ensure"
	StepFilePut          StepKind = "file.put"
	StepContainerApply   StepKind = "container.apply"
	StepSystemdApply     StepKind = "systemd.apply"
	StepScriptRun        StepKind = "script.run"
	StepHTTPProbe        StepKind = "http.probe"
	StepTCPProbe         StepKind = "tcp.probe"
	StepArtifactCollect  StepKind = "artifact.collect"
	StepCleanup          StepKind = "cleanup"
	StepSecretProvision  StepKind = "secret.provision"
	StepExternalAuth     StepKind = "auth.external"
)

type PermissionGrant struct {
	Name          string `json:"name"`
	Revision      uint64 `json:"revision,omitempty"`
	BindingDigest Digest `json:"binding_digest,omitempty"`
}

const (
	OutputDiscard  = "discard"
	OutputCapture  = "capture"
	OutputArtifact = "artifact"
)

type RedactionPolicy struct {
	Patterns []string `json:"patterns,omitempty"`
	Replace  string   `json:"replace,omitempty"`
}
type Postcondition struct {
	Type  string `json:"type"`
	Value string `json:"value,omitempty"`
}

type ScriptRunStep struct {
	Artifact          ArtifactRef       `json:"artifact"`
	Interpreter       string            `json:"interpreter"`
	Argv              []string          `json:"argv"`
	CWD               string            `json:"cwd"`
	Env               map[string]string `json:"env,omitempty"`
	SecretRefs        []CredentialRef   `json:"secret_refs,omitempty"`
	Root              bool              `json:"root"`
	NetworkGrants     []NetworkGrant    `json:"network_grants,omitempty"`
	AllowedExitCodes  []int             `json:"allowed_exit_codes"`
	TimeoutSeconds    uint64            `json:"timeout_seconds"`
	OutputLimit       uint64            `json:"output_limit"`
	Redaction         RedactionPolicy   `json:"redaction,omitempty"`
	Postcondition     *Postcondition    `json:"postcondition,omitempty"`
	IdempotencyMarker string            `json:"idempotency_marker"`
}

type TargetInspectStep struct {
	ObservationID string `json:"observation_id,omitempty"`
}
type ComputeProvisionStep struct {
	InfrastructureProfileID string `json:"infrastructure_profile_id"`
	AMIParameter            string `json:"ami_parameter"`
	InstanceType            string `json:"instance_type"`
	AvailabilityZone        string `json:"availability_zone"`
	VolumeGiB               uint32 `json:"volume_gib"`
	Region                  string `json:"region"`
	Architecture            string `json:"architecture"`
	ManagementTransport     string `json:"management_transport"`
	PublicIP                bool   `json:"public_ip"`
	PublicInbound           bool   `json:"public_inbound"`
}
type ComputeDestroyStep struct {
	ResourceID string `json:"resource_id,omitempty"`
}
type SourceFetchStep struct {
	Source   SourceRef   `json:"source"`
	Artifact ArtifactRef `json:"artifact"`
}
type ArtifactUploadStep struct {
	Artifact    ArtifactRef `json:"artifact"`
	Destination string      `json:"destination"`
}
type PackageEnsureStep struct {
	Name            string `json:"name"`
	Version         string `json:"version,omitempty"`
	Manager         string `json:"manager"`
	PlatformProfile string `json:"platform_profile"`
}
type FilePutStep struct {
	Path     string      `json:"path"`
	Artifact ArtifactRef `json:"artifact"`
	Mode     uint32      `json:"mode,omitempty"`
}
type ContainerApplyStep struct {
	Image         string `json:"image"`
	Name          string `json:"name"`
	HostAddress   string `json:"host_address"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	RestartPolicy string `json:"restart_policy"`
}
type SystemdApplyStep struct {
	Unit     string      `json:"unit"`
	Artifact ArtifactRef `json:"artifact,omitempty"`
}
type HTTPProbeStep struct {
	URL            string `json:"url"`
	Mode           string `json:"mode"`
	ExpectedStatus []int  `json:"expected_status,omitempty"`
}
type TCPProbeStep struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	Mode    string `json:"mode"`
}
type ArtifactCollectStep struct {
	Paths     []string `json:"paths"`
	OutputKey string   `json:"output_key"`
}
type CleanupStep struct {
	Resource string `json:"resource,omitempty"`
}

type SecretProvisionStep struct {
	Delivery string `json:"delivery"`
}

type ExternalAuthStep struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
}

// ExecutorSpec is a plan-sealed, provider-neutral description of the exact
// immutable artifact used to execute a typed step. The typed payload above
// remains the product meaning; this projection is deterministic executor
// machinery and is included in the step/stage/plan/confirmation digests.
//
// ExecutorSpec is produced only by the trusted plan sealer. Product callers
// cannot submit command bytes, and transports must resolve Artifact by its
// exact owner-scoped identity and digest instead of regenerating a command.
type ExecutorSpec struct {
	Artifact         ArtifactRef       `json:"artifact"`
	Interpreter      string            `json:"interpreter"`
	Argv             []string          `json:"argv,omitempty"`
	CWD              string            `json:"cwd"`
	Env              map[string]string `json:"env,omitempty"`
	Root             bool              `json:"root"`
	AllowedExitCodes []int             `json:"allowed_exit_codes"`
	OutputLimit      uint64            `json:"output_limit"`
	Redaction        RedactionPolicy   `json:"redaction,omitempty"`
	Postcondition    *Postcondition    `json:"postcondition"`
}

type ExecutionStep struct {
	StepKey           string                `json:"step_key"`
	Kind              StepKind              `json:"kind"`
	DependsOn         []string              `json:"depends_on,omitempty"`
	TargetID          string                `json:"target_id"`
	TargetRevision    uint64                `json:"target_revision"`
	TargetDigest      Digest                `json:"target_digest"`
	ObservationRef    *TargetObservationRef `json:"observation_ref,omitempty"`
	ArtifactRefs      []ArtifactRef         `json:"artifact_refs,omitempty"`
	Permissions       []PermissionGrant     `json:"permissions,omitempty"`
	NetworkGrants     []NetworkGrant        `json:"network_grants,omitempty"`
	SecretRefs        []CredentialRef       `json:"secret_refs,omitempty"`
	TimeoutSeconds    uint64                `json:"timeout_seconds"`
	IdempotencyMarker string                `json:"idempotency_marker"`
	OutputKey         string                `json:"output_key,omitempty"`
	Postcondition     *Postcondition        `json:"postcondition,omitempty"`
	OutputPolicy      string                `json:"output_policy,omitempty"`
	Digest            Digest                `json:"digest"`
	Executor          *ExecutorSpec         `json:"executor,omitempty"`

	TargetInspect    *TargetInspectStep    `json:"target_inspect,omitempty"`
	ComputeProvision *ComputeProvisionStep `json:"compute_provision,omitempty"`
	ComputeDestroy   *ComputeDestroyStep   `json:"compute_destroy,omitempty"`
	SourceFetch      *SourceFetchStep      `json:"source_fetch,omitempty"`
	ArtifactUpload   *ArtifactUploadStep   `json:"artifact_upload,omitempty"`
	PackageEnsure    *PackageEnsureStep    `json:"package_ensure,omitempty"`
	FilePut          *FilePutStep          `json:"file_put,omitempty"`
	ContainerApply   *ContainerApplyStep   `json:"container_apply,omitempty"`
	SystemdApply     *SystemdApplyStep     `json:"systemd_apply,omitempty"`
	ScriptRun        *ScriptRunStep        `json:"script_run,omitempty"`
	HTTPProbe        *HTTPProbeStep        `json:"http_probe,omitempty"`
	TCPProbe         *TCPProbeStep         `json:"tcp_probe,omitempty"`
	ArtifactCollect  *ArtifactCollectStep  `json:"artifact_collect,omitempty"`
	Cleanup          *CleanupStep          `json:"cleanup,omitempty"`
	SecretProvision  *SecretProvisionStep  `json:"secret_provision,omitempty"`
	ExternalAuth     *ExternalAuthStep     `json:"external_auth,omitempty"`
}

type ExecutionRun struct {
	RunID           string       `json:"run_id"`
	OwnerID         string       `json:"owner_id"`
	Operation       RunOperation `json:"operation"`
	TriggerKind     TriggerKind  `json:"trigger_kind,omitempty"`
	RollbackOfRunID string       `json:"rollback_of_run_id,omitempty"`
	DeploymentID    string       `json:"deployment_id,omitempty"`
	PlanID          string       `json:"plan_id"`
	ProjectID       string       `json:"project_id"`
	Purpose         PlanPurpose  `json:"purpose"`
	PlanRevision    uint64       `json:"plan_revision"`
	PlanDigest      Digest       `json:"plan_digest"`
	RunDigest       Digest       `json:"run_digest"`
	Status          RunStatus    `json:"status"`
	CurrentStage    string       `json:"current_stage,omitempty"`
	CurrentStageID  string       `json:"current_stage_id,omitempty"`
	Revision        uint64       `json:"revision"`
	TerminalReason  string       `json:"terminal_reason,omitempty"`
	StartedAt       time.Time    `json:"started_at,omitempty"`
	FinishedAt      time.Time    `json:"finished_at,omitempty"`
	CreatedAt       time.Time    `json:"created_at,omitempty"`
	UpdatedAt       time.Time    `json:"updated_at,omitempty"`
}

// Run is retained as the concise domain name used by persistence adapters.
type Run = ExecutionRun

type RunStage struct {
	StageID      string `json:"stage_id"`
	RunID        string `json:"run_id"`
	OwnerID      string `json:"owner_id"`
	PlanID       string `json:"plan_id"`
	StageKey     string `json:"stage_key"`
	PlanRevision uint64 `json:"plan_revision"`
	// RunRevision is the immutable aggregate revision at which this stage was
	// materialized. It is deliberately distinct from StageRevision (the plan
	// stage revision) and is never promoted with later run revisions.
	RunRevision         uint64      `json:"run_revision"`
	StageRevision       uint64      `json:"stage_revision"`
	StageDigest         Digest      `json:"stage_digest"`
	TargetID            string      `json:"target_id"`
	TargetRevision      uint64      `json:"target_revision"`
	TargetDigest        Digest      `json:"target_digest"`
	TaskID              string      `json:"task_id,omitempty"`
	ConfirmationID      string      `json:"confirmation_id,omitempty"`
	Ordinal             uint64      `json:"ordinal"`
	StageIdempotencyKey string      `json:"stage_idempotency_key"`
	Status              StageStatus `json:"status"`
	StartedAt           time.Time   `json:"started_at,omitempty"`
	FinishedAt          time.Time   `json:"finished_at,omitempty"`
	CreatedAt           time.Time   `json:"created_at,omitempty"`
	UpdatedAt           time.Time   `json:"updated_at,omitempty"`
}

type StepAttempt struct {
	AttemptID     string        `json:"attempt_id"`
	RunID         string        `json:"run_id"`
	StageID       string        `json:"stage_id"`
	PlanID        string        `json:"plan_id"`
	PlanRevision  uint64        `json:"plan_revision"`
	PlanDigest    Digest        `json:"plan_digest"`
	StageRevision uint64        `json:"stage_revision"`
	StageDigest   Digest        `json:"stage_digest"`
	StepRevision  uint64        `json:"step_revision"`
	StepDigest    Digest        `json:"step_digest"`
	StepKey       string        `json:"step_key"`
	Attempt       uint64        `json:"attempt"`
	ReceiptID     string        `json:"receipt_id,omitempty"`
	OwnerID       string        `json:"owner_id"`
	Revision      uint64        `json:"revision"`
	Status        AttemptStatus `json:"status"`
	Uncertain     bool          `json:"uncertain,omitempty"`
	StartedAt     time.Time     `json:"started_at,omitempty"`
	FinishedAt    time.Time     `json:"finished_at,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

type Receipt struct {
	ReceiptID      string        `json:"receipt_id"`
	RunID          string        `json:"run_id"`
	OwnerID        string        `json:"owner_id"`
	Revision       uint64        `json:"revision"`
	AttemptID      string        `json:"attempt_id"`
	Status         ReceiptStatus `json:"status"`
	IdempotencyKey string        `json:"idempotency_key"`
	// RequestDigest and FenceDigest are immutable transport/persistence pins;
	// neither is interchangeable with receipt_id, attempt_id, or the legacy
	// idempotency digest.
	RequestDigest     Digest    `json:"request_digest,omitempty"`
	FenceDigest       Digest    `json:"fence_digest,omitempty"`
	ResponseDigest    Digest    `json:"response_digest,omitempty"`
	ProviderOperation string    `json:"provider_operation,omitempty"`
	SSMCommandID      string    `json:"ssm_command_id,omitempty"`
	ExitCode          *int      `json:"exit_code,omitempty"`
	OutputDigest      Digest    `json:"output_digest,omitempty"`
	ProbeEvidence     []string  `json:"probe_evidence,omitempty"`
	RedactedDetails   string    `json:"redacted_details,omitempty"`
	At                time.Time `json:"at"`
}

type Event struct {
	EventID       string      `json:"event_id"`
	RunID         string      `json:"run_id"`
	OwnerID       string      `json:"owner_id"`
	Revision      uint64      `json:"revision"`
	StageID       string      `json:"stage_id,omitempty"`
	AttemptID     string      `json:"attempt_id,omitempty"`
	Status        StageStatus `json:"status,omitempty"`
	Key           string      `json:"key,omitempty"`
	Digest        Digest      `json:"digest,omitempty"`
	Sequence      uint64      `json:"sequence"`
	Type          string      `json:"type"`
	At            time.Time   `json:"at"`
	PayloadDigest Digest      `json:"payload_digest,omitempty"`
}

type OperationSchema struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  Digest `json:"digest"`
}

type ServiceBinding struct {
	BindingID        string            `json:"binding_id"`
	OwnerID          string            `json:"owner_id"`
	DeploymentID     string            `json:"deployment_id"`
	ProjectID        string            `json:"project_id"`
	RunID            string            `json:"run_id"`
	TargetID         string            `json:"target_id"`
	TargetRevision   uint64            `json:"target_revision"`
	TargetDigest     Digest            `json:"target_digest"`
	ReleaseID        string            `json:"release_id"`
	Protocol         string            `json:"protocol"`
	Endpoint         string            `json:"endpoint"`
	AuthRefs         []CredentialRef   `json:"auth_refs,omitempty"`
	OperationSchemas []OperationSchema `json:"operation_schemas,omitempty"`
	ArtifactIDs      []string          `json:"artifact_ids,omitempty"`
	HealthArtifact   ArtifactRef       `json:"health_artifact,omitempty"`
	UsageArtifact    ArtifactRef       `json:"usage_artifact,omitempty"`
	Revision         uint64            `json:"revision"`
	Digest           Digest            `json:"digest"`
}

type ConfirmationBindingSnapshot struct {
	OwnerID             string    `json:"owner_id"`
	PlanID              string    `json:"plan_id"`
	PlanRevision        uint64    `json:"plan_revision"`
	PlanDigest          Digest    `json:"plan_digest"`
	DeploymentID        string    `json:"deployment_id,omitempty"`
	RunID               string    `json:"run_id"`
	RunRevision         uint64    `json:"run_revision"`
	StageID             string    `json:"stage_id"`
	StageRevision       uint64    `json:"stage_revision"`
	StageDigest         Digest    `json:"stage_digest"`
	StageIdempotencyKey string    `json:"stage_idempotency_key"`
	TargetID            string    `json:"target_id"`
	TargetRevision      uint64    `json:"target_revision"`
	TargetDigest        Digest    `json:"target_digest"`
	ExecutionDigest     Digest    `json:"execution_digest"`
	ArtifactSetDigest   Digest    `json:"artifact_set_digest"`
	NetworkDigest       Digest    `json:"network_digest"`
	SecretGrantDigest   Digest    `json:"secret_grant_digest"`
	PolicyDigest        Digest    `json:"policy_digest"`
	CostQuoteDigest     Digest    `json:"cost_quote_digest"`
	RollbackDigest      Digest    `json:"rollback_digest"`
	PreviewDigest       Digest    `json:"preview_digest"`
	Risk                Risk      `json:"risk"`
	Gate                Gate      `json:"gate"`
	ExpiresAt           time.Time `json:"expires_at"`
	Digest              Digest    `json:"digest"`
}
