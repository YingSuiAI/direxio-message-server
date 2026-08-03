package agentembedded

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	action "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

func executionV2Actions() []string {
	return []string{
		"agent.execution.v2.projects.analyze", "agent.execution.v2.analyses.get", "agent.execution.v2.targets.list", "agent.execution.v2.targets.get", "agent.execution.v2.targets.import", "agent.execution.v2.targets.reserve", "agent.execution.v2.targets.observe",
		"agent.execution.v2.plans.create", "agent.execution.v2.plans.revise", "agent.execution.v2.plans.get", "agent.execution.v2.plans.list", "agent.execution.v2.deployments.list", "agent.execution.v2.deployments.get", "agent.execution.v2.deployments.events",
		"agent.execution.v2.runs.create", "agent.execution.v2.runs.get", "agent.execution.v2.runs.list", "agent.execution.v2.runs.cancel", "agent.execution.v2.runs.retry", "agent.execution.v2.runs.reconcile", "agent.execution.v2.runs.events",
		"agent.execution.v2.confirmations.get", "agent.execution.v2.confirmations.list", "agent.execution.v2.confirmations.confirm", "agent.execution.v2.confirmations.reject", "agent.execution.v2.artifacts.get",
		"agent.execution.v2.service_bindings.list", "agent.execution.v2.service_bindings.get", "agent.execution.v2.service_bindings.invoke",
		"agent.execution.v2.secrets.create", "agent.execution.v2.secrets.get", "agent.execution.v2.secrets.list", "agent.execution.v2.secrets.revoke",
	}
}

// ExecutionV2Store is the durable, owner-scoped read/write seam. Database
// adapters satisfy it; tests may provide a narrow in-memory implementation.
type ExecutionV2Store interface {
	CreateAnalysis(context.Context, storage.AnalysisCreateRequest) (coreexecution.ProjectAnalysis, error)
	GetAnalysis(context.Context, string, string) (coreexecution.ProjectAnalysis, error)
	CreatePlan(context.Context, storage.ExecutionPlanCreate) (coreexecution.ExecutionPlan, error)
	GetCurrentPlan(context.Context, string, string) (coreexecution.ExecutionPlan, error)
	GetPlanRevision(context.Context, string, string, uint64) (coreexecution.ExecutionPlan, error)
	ListExecutionPlanRevisions(context.Context, string, string, uint64, int) (storage.ExecutionPlanHistoryPage, error)
	ListExecutionPlans(context.Context, string, string, int) (storage.ExecutionPlanPage, error)
	ListTargets(context.Context, string, string, int) (storage.ExecutionTargetPage, error)
	GetTarget(context.Context, string, string, uint64) (coreexecution.ExecutionTarget, error)
	GetExecutionRun(context.Context, string, string) (storage.ExecutionRunView, error)
	ListExecutionRuns(context.Context, string, string, string, string, int) (storage.ExecutionRunPage, error)
	GetExecutionDeployment(context.Context, string, string) (storage.ExecutionDeploymentRecord, error)
	ListExecutionDeployments(context.Context, string, string, string, int) (storage.ExecutionDeploymentPage, error)
	ListExecutionEvents(context.Context, string, string, uint64, int) ([]storage.ExecutionEventRecord, uint64, error)
	ListDeploymentEvents(context.Context, string, string, uint64, int) ([]storage.DeploymentEventRecord, uint64, error)
	GetV2Confirmation(context.Context, string, string) (storage.ExecutionConfirmationRecord, error)
	ListV2Confirmations(context.Context, string, string, []coreconfirmation.State, int) (storage.ExecutionConfirmationPage, error)
	GetArtifactMetadata(context.Context, string, string) (storage.ExecutionArtifactRecord, error)
	GetServiceBinding(context.Context, string, string) (coreexecution.ServiceBinding, error)
	ListServiceBindings(context.Context, string, string, string, int) ([]coreexecution.ServiceBinding, string, error)
}

type ExecutionV2Coordinator interface {
	CreateRun(context.Context, coreexecution.CreateRunCommand) (coreexecution.RunMaterialization, error)
	ConfirmStage(context.Context, coreexecution.ConfirmStageCommand) (coreconfirmation.Confirmation, error)
}

type executionV2Rejector interface {
	RejectStage(context.Context, coreexecution.RejectStageCommand) (coreconfirmation.Confirmation, error)
}
type executionV2Canceler interface {
	CancelRun(context.Context, coreexecution.CancelRunCommand) (coreexecution.ExecutionRun, error)
}
type executionV2Retrier interface {
	RetryRun(context.Context, coreexecution.RetryRunCommand) (coreexecution.RunMaterialization, error)
}

// ObservePort, ReconcilePort and InvokePort are deliberately typed seams. A
// missing seam returns execution_v2_not_ready rather than a fabricated result.
type ExecutionV2SourceInput struct {
	Kind               string
	Location           string
	Commit             string
	ArtifactID         string
	ArtifactDigest     string
	CredentialRef      string
	CredentialRevision uint64
	Immutable          bool
}

type ExecutionV2AnalyzeRequest struct {
	ProjectID      string
	Source         ExecutionV2SourceInput
	IdempotencyKey string
}

type ExecutionV2PlanCreateRequest struct {
	ProjectID       string
	AnalysisID      string
	Intent          string
	RecipeID        string
	TargetID        string
	TargetRevision  uint64
	Purpose         coreexecution.PlanPurpose
	AIConfiguration *coreexecution.AIConfiguration
	IdempotencyKey  string
}

type ExecutionV2PlanReviseRequest struct {
	PlanID           string
	ExpectedRevision uint64
	Intent           string
	RecipeID         string
	TargetID         string
	TargetRevision   uint64
	Purpose          coreexecution.PlanPurpose
	AIConfiguration  *coreexecution.AIConfiguration
	IdempotencyKey   string
}

type ExecutionV2ObserveRequest struct {
	TargetID       string
	TargetRevision uint64
	IdempotencyKey string
}

type ExecutionV2TargetImportRequest struct {
	CredentialID       string
	CredentialRevision uint64
	InstanceID         string
	IdempotencyKey     string
}

type ExecutionV2TargetImportResult struct {
	Target        coreexecution.ExecutionTarget
	ObservationID string
	Observation   coreexecution.TargetObservation
}

type ExecutionV2TargetReserveRequest struct {
	CredentialID       string
	CredentialRevision uint64
	InstanceType       string
	VolumeGiB          uint32
	IdempotencyKey     string
}

type ExecutionV2ReconcileRequest struct {
	RunID            string
	StageID          string
	ExpectedRevision uint64
	IdempotencyKey   string
}

type ExecutionV2InvokeRequest struct {
	BindingID        string
	Operation        string
	Input            map[string]any
	ExpectedRevision uint64
	IdempotencyKey   string
}

type ExecutionV2ObservePort interface {
	Observe(context.Context, string, ExecutionV2ObserveRequest) (coreexecution.TargetObservation, error)
}
type ExecutionV2TargetImportPort interface {
	ImportTarget(context.Context, string, ExecutionV2TargetImportRequest) (ExecutionV2TargetImportResult, error)
}
type ExecutionV2TargetReservePort interface {
	ReserveTarget(context.Context, string, ExecutionV2TargetReserveRequest) (coreexecution.ExecutionTarget, error)
}
type ExecutionV2ReconcilePort interface {
	Reconcile(context.Context, string, ExecutionV2ReconcileRequest) (coreexecution.ExecutionRun, error)
}
type ExecutionV2InvokePort interface {
	Invoke(context.Context, string, ExecutionV2InvokeRequest) (map[string]any, error)
}
type ExecutionV2AnalyzePort interface {
	Analyze(context.Context, string, ExecutionV2AnalyzeRequest) (coreexecution.ProjectAnalysis, error)
}
type ExecutionV2PlanCompilerPort interface {
	Compile(context.Context, string, ExecutionV2PlanCreateRequest) (coreexecution.ExecutionPlan, error)
	Revise(context.Context, string, ExecutionV2PlanReviseRequest) (coreexecution.ExecutionPlan, error)
}

type ExecutionV2SecretPort interface {
	CreateExecutionSecret(context.Context, storage.ExecutionSecretCreateRequest) (storage.ExecutionSecretMetadata, error)
	GetExecutionSecret(context.Context, string, string, uint64) (storage.ExecutionSecretMetadata, error)
	ListExecutionSecrets(context.Context, string, string, int) (storage.ExecutionSecretPage, error)
	RevokeExecutionSecret(context.Context, storage.ExecutionSecretRevokeRequest) (storage.ExecutionSecretMetadata, error)
}

type ExecutionV2Config struct {
	Store              ExecutionV2Store
	Coordinator        ExecutionV2Coordinator
	Observe            ExecutionV2ObservePort
	TargetImport       ExecutionV2TargetImportPort
	TargetReserve      ExecutionV2TargetReservePort
	Reconcile          ExecutionV2ReconcilePort
	Invoke             ExecutionV2InvokePort
	Analyze            ExecutionV2AnalyzePort
	PlanCompiler       ExecutionV2PlanCompilerPort
	Secrets            ExecutionV2SecretPort
	Ready              func() bool
	PlanReady          func() bool
	ObserveReady       func() bool
	TargetImportReady  func() bool
	TargetReserveReady func() bool
	RunReady           func() bool
	ReconcileReady     func() bool
	BindingsReady      func() bool
	InvokeReady        func() bool
	TransportAWSReady  func() bool
	SecretsReady       func() bool
}

// NewExecutionV2ActionPort returns an owner-scoped ProductCore action port.
func NewExecutionV2ActionPort(cfg ExecutionV2Config) ActionPort {
	return &executionV2Port{cfg: cfg}
}

type executionV2Port struct{ cfg ExecutionV2Config }

func (p *executionV2Port) Handle(ctx context.Context, owner, name string, params map[string]any) (any, *action.Error) {
	isSecretAction := strings.HasPrefix(name, "agent.execution.v2.secrets.")
	baseReady := p != nil && p.cfg.Ready != nil && p.cfg.Ready()
	secretReady := p != nil && p.cfg.Secrets != nil && p.cfg.SecretsReady != nil && p.cfg.SecretsReady()
	if p == nil || (!isSecretAction && !baseReady) || (isSecretAction && !secretReady) {
		return nil, executionV2NotReady()
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, action.CodedError(http.StatusUnauthorized, "owner_required", "owner is required")
	}
	if !containsExecutionV2Action(name) {
		return nil, action.CodedError(http.StatusNotFound, "execution_v2_action_not_found", "unsupported execution.v2 action")
	}
	name = strings.TrimPrefix(name, "agent.execution.v2.")
	if e := rejectExecUnknown(name, params); e != nil {
		return nil, e
	}
	if e := validateExecutionV2Params(name, params); e != nil {
		return nil, e
	}
	if !p.actionReady(name) {
		return nil, executionV2NotReady()
	}
	switch name {
	case "projects.analyze":
		return p.analyze(ctx, owner, params)
	case "analyses.get":
		return p.analysisGet(ctx, owner, params)
	case "targets.list":
		return p.targetsList(ctx, owner, params)
	case "targets.get":
		return p.targetGet(ctx, owner, params)
	case "targets.import":
		return p.targetImport(ctx, owner, params)
	case "targets.reserve":
		return p.targetReserve(ctx, owner, params)
	case "targets.observe":
		return p.observe(ctx, owner, params)
	case "plans.create":
		return p.planCreate(ctx, owner, params)
	case "plans.revise":
		return p.planRevise(ctx, owner, params)
	case "plans.get":
		return p.planGet(ctx, owner, params)
	case "plans.list":
		return p.plansList(ctx, owner, params)
	case "deployments.list":
		return p.deploymentsList(ctx, owner, params)
	case "deployments.get":
		return p.deploymentGet(ctx, owner, params)
	case "deployments.events":
		return p.deploymentEvents(ctx, owner, params)
	case "runs.create":
		return p.runCreate(ctx, owner, params)
	case "runs.get":
		return p.runGet(ctx, owner, params)
	case "runs.list":
		return p.runsList(ctx, owner, params)
	case "runs.cancel":
		return p.runCancel(ctx, owner, params)
	case "runs.retry":
		return p.runRetry(ctx, owner, params)
	case "runs.reconcile":
		return p.reconcile(ctx, owner, params)
	case "runs.events":
		return p.runEvents(ctx, owner, params)
	case "confirmations.get":
		return p.confirmationGet(ctx, owner, params)
	case "confirmations.list":
		return p.confirmationsList(ctx, owner, params)
	case "confirmations.confirm":
		return p.confirm(ctx, owner, params)
	case "confirmations.reject":
		return p.reject(ctx, owner, params)
	case "artifacts.get":
		return p.artifactGet(ctx, owner, params)
	case "service_bindings.list":
		return p.bindingsList(ctx, owner, params)
	case "service_bindings.get":
		return p.bindingGet(ctx, owner, params)
	case "service_bindings.invoke":
		return p.invoke(ctx, owner, params)
	case "secrets.create":
		return p.secretCreate(ctx, owner, params)
	case "secrets.get":
		return p.secretGet(ctx, owner, params)
	case "secrets.list":
		return p.secretsList(ctx, owner, params)
	case "secrets.revoke":
		return p.secretRevoke(ctx, owner, params)
	default:
		return nil, executionV2NotReady()
	}
}

func (p *executionV2Port) actionReady(name string) bool {
	if p == nil {
		return false
	}
	var f func() bool
	switch name {
	case "plans.create", "plans.revise", "plans.get", "plans.list", "projects.analyze", "analyses.get":
		f = p.cfg.PlanReady
	case "targets.list", "targets.get", "targets.observe", "deployments.list", "deployments.get", "deployments.events":
		f = p.cfg.ObserveReady
	case "targets.import":
		f = func() bool {
			return p.cfg.TargetImportReady != nil && p.cfg.TargetImportReady() && p.cfg.TransportAWSReady != nil && p.cfg.TransportAWSReady()
		}
	case "targets.reserve":
		f = p.cfg.TargetReserveReady
	case "runs.reconcile":
		f = func() bool {
			return p.cfg.Reconcile != nil && p.cfg.ReconcileReady != nil && p.cfg.ReconcileReady() && p.cfg.TransportAWSReady != nil && p.cfg.TransportAWSReady()
		}
	case "runs.create", "runs.get", "runs.list", "runs.cancel", "runs.retry", "runs.events", "confirmations.get", "confirmations.list", "confirmations.confirm", "confirmations.reject":
		f = func() bool {
			return p.cfg.RunReady != nil && p.cfg.RunReady() && p.cfg.TransportAWSReady != nil && p.cfg.TransportAWSReady()
		}
	case "artifacts.get", "service_bindings.list", "service_bindings.get":
		f = p.cfg.BindingsReady
	case "service_bindings.invoke":
		f = func() bool {
			return p.cfg.BindingsReady != nil && p.cfg.BindingsReady() &&
				p.cfg.Invoke != nil && p.cfg.InvokeReady != nil && p.cfg.InvokeReady()
		}
	case "secrets.create", "secrets.get", "secrets.list", "secrets.revoke":
		f = func() bool { return p.cfg.Secrets != nil && p.cfg.SecretsReady != nil && p.cfg.SecretsReady() }
	default:
		f = p.cfg.Ready
	}
	// A missing feature-specific readiness hook is fail-closed. The base
	// execution.v2 readiness only advertises the owner-scoped domain.
	return f != nil && f()
}

func containsExecutionV2Action(n string) bool {
	for _, x := range executionV2Actions() {
		if x == n {
			return true
		}
	}
	return false
}
func executionV2NotReady() *action.Error {
	return action.CodedError(http.StatusPreconditionFailed, "execution_v2_not_ready", "execution.v2 capability is not ready")
}
func pStore(p *executionV2Port) (ExecutionV2Store, *action.Error) {
	if p == nil || p.cfg.Store == nil {
		return nil, executionV2NotReady()
	}
	return p.cfg.Store, nil
}

var executionV2AllowedFields = map[string]map[string]bool{
	"projects.analyze": {"project_id": true, "source": true, "idempotency_key": true},
	"analyses.get":     {"analysis_id": true}, "targets.list": {"page_size": true, "page_token": true}, "targets.get": {"target_id": true, "revision": true},
	"targets.import": {"credential_id": true, "credential_revision": true, "instance_id": true, "idempotency_key": true}, "targets.reserve": {"credential_id": true, "credential_revision": true, "instance_type": true, "volume_gib": true, "idempotency_key": true}, "targets.observe": {"target_id": true, "target_revision": true, "idempotency_key": true},
	"plans.create": {"project_id": true, "analysis_id": true, "intent": true, "recipe_id": true, "target_id": true, "target_revision": true, "purpose": true, "ai_configuration": true, "idempotency_key": true}, "plans.revise": {"plan_id": true, "intent": true, "recipe_id": true, "target_id": true, "target_revision": true, "purpose": true, "ai_configuration": true, "idempotency_key": true, "expected_revision": true}, "plans.get": {"plan_id": true, "revision": true}, "plans.list": {"page_size": true, "page_token": true},
	"deployments.list": {"project_id": true, "page_size": true, "page_token": true}, "deployments.get": {"deployment_id": true}, "deployments.events": {"deployment_id": true, "after_sequence": true, "limit": true},
	"runs.create": {"plan_id": true, "plan_revision": true, "operation": true, "trigger_kind": true, "rollback_of_run_id": true, "idempotency_key": true}, "runs.get": {"run_id": true}, "runs.list": {"project_id": true, "deployment_id": true, "page_size": true, "page_token": true},
	"runs.cancel": {"run_id": true, "idempotency_key": true, "expected_revision": true}, "runs.retry": {"run_id": true, "idempotency_key": true, "expected_revision": true}, "runs.reconcile": {"run_id": true, "stage_id": true, "idempotency_key": true, "expected_revision": true}, "runs.events": {"run_id": true, "after_sequence": true, "limit": true},
	"confirmations.get": {"confirmation_id": true}, "confirmations.list": {"page_size": true, "page_token": true, "states": true}, "confirmations.confirm": {"confirmation_id": true, "idempotency_key": true, "expected_revision": true}, "confirmations.reject": {"confirmation_id": true, "idempotency_key": true, "expected_revision": true},
	"artifacts.get": {"artifact_id": true}, "service_bindings.list": {"project_id": true, "page_size": true, "page_token": true}, "service_bindings.get": {"binding_id": true}, "service_bindings.invoke": {"binding_id": true, "operation": true, "idempotency_key": true, "expected_revision": true, "input": true},
	"secrets.create": {"provider": true, "purpose": true, "value": true, "idempotency_key": true}, "secrets.get": {"secret_ref": true, "revision": true}, "secrets.list": {"page_size": true, "page_token": true}, "secrets.revoke": {"secret_ref": true, "expected_revision": true, "idempotency_key": true},
}

func rejectExecUnknown(name string, p map[string]any) *action.Error {
	for k := range p {
		if !executionV2AllowedFields[name][k] {
			return action.CodedError(http.StatusBadRequest, "unknown_field", "unknown field: "+k)
		}
	}
	return nil
}

func reqString(p map[string]any, k string) (string, *action.Error) {
	v, ok := p[k].(string)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		return "", action.BadRequest("missing required field: " + k)
	}
	return v, nil
}
func optString(p map[string]any, k string) string {
	if v, ok := p[k].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
func reqUUID(p map[string]any, k string) (string, *action.Error) {
	v, e := reqString(p, k)
	if e != nil {
		return "", e
	}
	if _, x := uuid.Parse(v); x != nil {
		return "", action.BadRequest("invalid " + k)
	}
	return v, nil
}

func optUUID(p map[string]any, k string) (string, *action.Error) {
	v := optString(p, k)
	if v == "" {
		return "", nil
	}
	if _, err := uuid.Parse(v); err != nil {
		return "", action.BadRequest("invalid " + k)
	}
	return v, nil
}

func parseUintParam(p map[string]any, k string, required bool, min, max uint64) (uint64, *action.Error) {
	raw, present := p[k]
	if !present {
		if required {
			return 0, action.BadRequest("missing required field: " + k)
		}
		return 0, nil
	}
	var n uint64
	valid := true
	switch v := raw.(type) {
	case float64:
		valid = !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v == math.Trunc(v)
		if valid {
			n = uint64(v)
			valid = float64(n) == v
		}
	case float32:
		f := float64(v)
		valid = !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f == math.Trunc(f)
		if valid {
			n = uint64(f)
			valid = float64(n) == f
		}
	case int:
		valid = v >= 0
		if valid {
			n = uint64(v)
		}
	case int64:
		valid = v >= 0
		if valid {
			n = uint64(v)
		}
	case uint:
		n = uint64(v)
	case uint64:
		n = v
	case json.Number:
		i, err := strconv.ParseUint(string(v), 10, 64)
		valid, n = err == nil, i
	default:
		valid = false
	}
	if !valid || n < min || (max > 0 && n > max) {
		return 0, action.BadRequest("invalid " + k)
	}
	return n, nil
}

func uintParam(p map[string]any, k string) uint64 {
	n, _ := parseUintParam(p, k, false, 0, 0)
	return n
}

func pageParam(p map[string]any) (int, string, *action.Error) {
	n, err := parseUintParam(p, "page_size", false, 1, 200)
	if err != nil {
		return 0, "", err
	}
	if n == 0 {
		n = 100
	}
	token := optString(p, "page_token")
	if len(token) > 512 {
		return 0, "", action.BadRequest("invalid page_token")
	}
	return int(n), token, nil
}

func parseEventPage(p map[string]any) (uint64, int, *action.Error) {
	after, err := parseUintParam(p, "after_sequence", false, 0, 0)
	if err != nil {
		return 0, 0, err
	}
	limit, err := parseUintParam(p, "limit", false, 1, 200)
	if err != nil {
		return 0, 0, err
	}
	if limit == 0 {
		limit = 100
	}
	return after, int(limit), nil
}

func parseStringField(p map[string]any, k string, required bool, max int) (string, *action.Error) {
	raw, present := p[k]
	if !present {
		if required {
			return "", action.BadRequest("missing required field: " + k)
		}
		return "", nil
	}
	v, ok := raw.(string)
	v = strings.TrimSpace(v)
	if !ok || (required && v == "") || len(v) > max {
		return "", action.BadRequest("invalid " + k)
	}
	return v, nil
}

func mapOut(v any) any { b, _ := json.Marshal(v); var out any; _ = json.Unmarshal(b, &out); return out }

// Ownership is an authenticated ProductCore envelope fact, not part of the
// digest-bound analysis, target, observation, or stage snapshots. Keep the
// core snapshots portable while making every public projection self-scoping
// for strict clients.
func executionV2OwnedMap(owner string, value any) map[string]any {
	out, _ := mapOut(value).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	out["owner_id"] = strings.TrimSpace(owner)
	return out
}

func executionV2AnalysisMap(owner string, analysis coreexecution.ProjectAnalysis) map[string]any {
	return executionV2OwnedMap(owner, analysis)
}

func executionV2TargetMap(owner string, target coreexecution.ExecutionTarget) map[string]any {
	return executionV2OwnedMap(owner, target)
}

func executionV2ObservationMap(owner string, observation coreexecution.TargetObservation) map[string]any {
	return executionV2OwnedMap(owner, observation)
}

func executionV2StageMap(owner string, stage coreexecution.ExecutionStage) map[string]any {
	return executionV2OwnedMap(owner, stage)
}

func executionV2PlanMap(owner string, plan coreexecution.ExecutionPlan) map[string]any {
	out := executionV2OwnedMap(owner, plan)
	targets := make([]any, 0, len(plan.Targets))
	for _, target := range plan.Targets {
		targets = append(targets, executionV2TargetMap(owner, target))
	}
	stages := make([]any, 0, len(plan.Stages))
	for _, stage := range plan.Stages {
		stages = append(stages, executionV2StageMap(owner, stage))
	}
	out["targets"] = targets
	out["stages"] = stages
	return out
}

func executionV2Time(v time.Time) string {
	if v.IsZero() {
		return ""
	}
	return v.UTC().Format(time.RFC3339Nano)
}

func executionV2PlanRecordMap(v storage.ExecutionPlanRevisionRecord) map[string]any {
	return map[string]any{
		"owner_id": v.OwnerID, "plan_id": v.PlanID, "project_id": v.ProjectID,
		"analysis_id": v.AnalysisID, "revision": v.Revision, "status": v.Status,
		"digest": string(v.Digest), "expires_at": executionV2Time(v.ExpiresAt),
		"created_at": executionV2Time(v.CreatedAt), "changed_stage_keys": append([]string(nil), v.ChangedStageKeys...),
	}
}

func executionV2DeploymentMap(v storage.ExecutionDeploymentRecord) map[string]any {
	return map[string]any{
		"owner_id": v.OwnerID, "deployment_id": v.DeploymentID, "project_id": v.ProjectID,
		"run_id": v.RunID, "current_stage_id": v.CurrentStageID, "release_id": v.ReleaseID,
		"state": v.State, "revision": v.Revision, "created_at": executionV2Time(v.CreatedAt),
		"updated_at": executionV2Time(v.UpdatedAt),
	}
}

func executionV2ArtifactMap(v storage.ExecutionArtifactRecord) map[string]any {
	return map[string]any{
		"owner_id": v.OwnerID, "artifact_id": v.ArtifactID, "project_id": v.ProjectID,
		"plan_id": v.PlanID, "plan_revision": v.PlanRevision, "run_id": v.RunID,
		"attempt_id": v.AttemptID, "content_digest": string(v.ContentDigest),
		"storage_backend": v.StorageBackend, "size_bytes": v.SizeBytes, "media_type": v.MediaType,
		"revision": v.Revision, "status": v.Status, "metadata": mapOut(v.Metadata),
		"created_at": executionV2Time(v.CreatedAt),
	}
}

func executionV2SecretMap(v storage.ExecutionSecretMetadata) map[string]any {
	return map[string]any{
		"secret_ref": v.SecretRef, "revision": v.Revision, "purpose": v.Purpose,
		"provider": v.Provider, "binding_digest": string(v.BindingDigest), "status": v.Status,
		"created_at": executionV2Time(v.CreatedAt), "updated_at": executionV2Time(v.UpdatedAt),
	}
}

func executionV2EventMap(v storage.ExecutionEventRecord) map[string]any {
	return map[string]any{
		"owner_id": v.OwnerID, "run_id": v.RunID, "event_id": v.EventID,
		"revision": uint64(1), "sequence": v.Sequence, "stage_id": v.StageID,
		"attempt_id": v.AttemptID, "status": v.Status, "key": v.EventKey,
		"digest": string(v.EventDigest), "type": v.Kind, "payload_digest": string(v.PayloadDigest),
		"at": executionV2Time(v.CreatedAt),
	}
}

func executionV2DeploymentEventMap(v storage.DeploymentEventRecord) map[string]any {
	return map[string]any{
		"owner_id": v.OwnerID, "deployment_id": v.DeploymentID, "event_id": v.EventID,
		"revision": uint64(1), "sequence": v.Sequence, "status": v.Status,
		"key": v.EventKey, "digest": string(v.EventDigest), "type": v.Kind,
		"at": executionV2Time(v.CreatedAt),
	}
}

func executionV2BindingMap(v coreconfirmation.Binding) map[string]any {
	return map[string]any{
		"digest": v.Digest, "owner_id": v.OwnerID, "operation_domain": v.OperationDomain,
		"plan_id": v.PlanID, "plan_revision": v.PlanRevision, "plan_digest": string(v.PlanDigest),
		"deployment_id": v.DeploymentID, "run_id": v.RunID, "run_revision": v.RunRevision,
		"stage_id": v.StageID, "stage_revision": v.StageRevision, "stage_digest": string(v.StageDigest),
		"stage_idempotency_key": v.StageIdempotencyKey, "target_id": v.TargetID,
		"target_revision": v.TargetRevision, "target_digest": string(v.TargetDigest),
		"execution_digest": string(v.ExecutionDigest), "artifact_set_digest": string(v.ArtifactSetDigest),
		"network_digest": string(v.NetworkDigest), "secret_grant_digest": string(v.SecretGrantDigest),
		"policy_digest": string(v.PolicyDigest), "cost_quote_digest": string(v.CostQuoteDigest),
		"rollback_digest": string(v.RollbackDigest), "preview_digest": string(v.PreviewDigest),
		"risk_level": v.RiskLevel, "gate_type": v.GateType,
		"expires_at": executionV2Time(v.BindingExpiresAt),
	}
}

func executionV2ConfirmationMap(v storage.ExecutionConfirmationRecord) map[string]any {
	c := v.Confirmation
	id := strings.TrimSpace(c.ConfirmationID)
	if id == "" {
		id = strings.TrimSpace(c.ID)
	}
	return map[string]any{
		"confirmation_id": id, "owner_id": c.OwnerID, "task_id": c.TaskID,
		"state": string(c.State), "revision": c.Revision,
		"created_at": executionV2Time(c.CreatedAt), "updated_at": executionV2Time(c.UpdatedAt),
		"expires_at": executionV2Time(c.ExpiresAt), "terminal_code": c.TerminalCode,
		"terminal_reason": c.TerminalReason, "binding": executionV2BindingMap(c.Binding),
		"preview": mapOut(v.Preview),
	}
}

func mapErr(err error) *action.Error {
	if errors.Is(err, coreexecution.ErrNotFound) {
		return action.CodedError(http.StatusNotFound, "execution_v2_not_found", "execution.v2 record was not found")
	}
	if errors.Is(err, coreexecution.ErrConflict) {
		return action.CodedError(http.StatusConflict, "execution_v2_conflict", "execution.v2 request conflicts with current state")
	}
	logrus.WithError(err).Error("execution.v2 action failed")
	return action.CodedError(http.StatusInternalServerError, "execution_v2_internal", "execution.v2 request failed")
}

var (
	executionV2CommitRE       = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)
	executionV2DigestRE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	executionV2NameRE         = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	executionV2InstanceRE     = regexp.MustCompile(`^i-[0-9a-f]{8,32}$`)
	executionV2InstanceTypeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}\.[a-z0-9][a-z0-9-]{0,31}$`)
)

func parseExecutionV2AIConfiguration(raw any) (*coreexecution.AIConfiguration, *action.Error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, action.BadRequest("ai_configuration must be an object")
	}
	mode, err := parseStringField(m, "mode", true, 32)
	if err != nil {
		return nil, err
	}
	provider, err := parseStringField(m, "provider", true, 64)
	if err != nil || !executionV2NameRE.MatchString(provider) {
		return nil, action.BadRequest("invalid ai_configuration provider")
	}
	switch coreexecution.AIAuthMode(mode) {
	case coreexecution.AIAuthModeAPIKey:
		allowed := map[string]bool{"mode": true, "provider": true, "secret_ref": true, "secret_revision": true, "secret_purpose": true, "secret_binding_digest": true}
		for key := range m {
			if !allowed[key] {
				return nil, action.CodedError(http.StatusBadRequest, "unknown_field", "unknown ai_configuration field: "+key)
			}
		}
		secretRef, parseErr := reqUUID(m, "secret_ref")
		if parseErr != nil {
			return nil, parseErr
		}
		revision, parseErr := parseUintParam(m, "secret_revision", true, 1, 0)
		if parseErr != nil {
			return nil, parseErr
		}
		purpose, parseErr := parseStringField(m, "secret_purpose", true, 64)
		if parseErr != nil || purpose != coreexecution.AISecretPurposeProviderAPIKey {
			return nil, action.BadRequest("invalid ai_configuration secret_purpose")
		}
		bindingDigest, parseErr := parseStringField(m, "secret_binding_digest", true, 64)
		if parseErr != nil || !executionV2DigestRE.MatchString(bindingDigest) {
			return nil, action.BadRequest("invalid ai_configuration secret_binding_digest")
		}
		return &coreexecution.AIConfiguration{Mode: coreexecution.AIAuthModeAPIKey, Provider: provider, SecretRef: secretRef, SecretRevision: revision, SecretPurpose: purpose, SecretBindingDigest: coreexecution.Digest(bindingDigest)}, nil
	case coreexecution.AIAuthModeAuthGate:
		allowed := map[string]bool{"mode": true, "provider": true, "status": true}
		for key := range m {
			if !allowed[key] {
				return nil, action.CodedError(http.StatusBadRequest, "unknown_field", "unknown ai_configuration field: "+key)
			}
		}
		status, parseErr := parseStringField(m, "status", true, 64)
		if parseErr != nil || status != coreexecution.AIExternalAuthPending {
			return nil, action.BadRequest("invalid ai_configuration status")
		}
		return &coreexecution.AIConfiguration{Mode: coreexecution.AIAuthModeAuthGate, Provider: provider, Status: status}, nil
	default:
		return nil, action.BadRequest("invalid ai_configuration mode")
	}
}

func optionalExecutionV2AIConfiguration(p map[string]any) (*coreexecution.AIConfiguration, *action.Error) {
	raw, present := p["ai_configuration"]
	if !present {
		return nil, nil
	}
	return parseExecutionV2AIConfiguration(raw)
}

func parseExecutionV2Source(raw any) (ExecutionV2SourceInput, *action.Error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return ExecutionV2SourceInput{}, action.BadRequest("source must be an object")
	}
	allowed := map[string]bool{
		"kind": true, "location": true, "commit": true, "artifact_id": true,
		"credential_ref": true, "credential_revision": true, "immutable": true,
	}
	for key := range m {
		if !allowed[key] {
			return ExecutionV2SourceInput{}, action.CodedError(http.StatusBadRequest, "unknown_field", "unknown source field: "+key)
		}
	}
	kind, err := parseStringField(m, "kind", true, 64)
	if err != nil {
		return ExecutionV2SourceInput{}, err
	}
	location, err := parseStringField(m, "location", false, 2048)
	if err != nil {
		return ExecutionV2SourceInput{}, err
	}
	commit, err := parseStringField(m, "commit", false, 64)
	if err != nil {
		return ExecutionV2SourceInput{}, err
	}
	artifactID, err := optUUID(m, "artifact_id")
	if err != nil {
		return ExecutionV2SourceInput{}, err
	}
	credentialRef, err := optUUID(m, "credential_ref")
	if err != nil {
		return ExecutionV2SourceInput{}, err
	}
	credentialRevision, err := parseUintParam(m, "credential_revision", false, 1, 0)
	if err != nil {
		return ExecutionV2SourceInput{}, err
	}
	immutable, ok := m["immutable"].(bool)
	if !ok || !immutable {
		return ExecutionV2SourceInput{}, action.BadRequest("source must be immutable")
	}
	if (credentialRef == "") != (credentialRevision == 0) {
		return ExecutionV2SourceInput{}, action.BadRequest("credential_ref and credential_revision must be supplied together")
	}
	switch kind {
	case "git_https":
		u, parseErr := url.Parse(location)
		if parseErr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || !executionV2CommitRE.MatchString(commit) {
			return ExecutionV2SourceInput{}, action.BadRequest("git_https source must pin an exact HTTPS repository commit")
		}
		if artifactID != "" {
			return ExecutionV2SourceInput{}, action.BadRequest("git_https source cannot include artifact_id")
		}
	case "oci_image":
		parts := strings.Split(location, "@sha256:")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || !executionV2DigestRE.MatchString(parts[1]) || commit != "" || artifactID != "" || credentialRef != "" {
			return ExecutionV2SourceInput{}, action.BadRequest("oci_image source must be digest-pinned")
		}
	case "uploaded_artifact":
		if artifactID == "" || location != "" || commit != "" || credentialRef != "" {
			return ExecutionV2SourceInput{}, action.BadRequest("uploaded_artifact source must reference one immutable artifact")
		}
	default:
		return ExecutionV2SourceInput{}, action.BadRequest("unsupported source kind")
	}
	return ExecutionV2SourceInput{
		Kind: kind, Location: location, Commit: commit, ArtifactID: artifactID,
		CredentialRef: credentialRef, CredentialRevision: credentialRevision, Immutable: true,
	}, nil
}

func validatePlanSelection(p map[string]any) *action.Error {
	intent, err := parseStringField(p, "intent", true, 128)
	if err != nil || !executionV2NameRE.MatchString(intent) {
		return action.BadRequest("invalid intent")
	}
	recipe, err := parseStringField(p, "recipe_id", true, 128)
	if err != nil || !executionV2NameRE.MatchString(recipe) {
		return action.BadRequest("invalid recipe_id")
	}
	purpose, err := parseStringField(p, "purpose", true, 16)
	if err != nil || (purpose != string(coreexecution.PurposeService) && purpose != string(coreexecution.PurposeJob)) {
		return action.BadRequest("invalid purpose")
	}
	if recipe == coreexecution.RecipeGenericContainerService && (intent != "deploy" || purpose != string(coreexecution.PurposeService)) {
		return action.BadRequest("generic-container-service supports initial service deployment only")
	}
	return nil
}

func validExecutionV2RunOperation(v string) bool {
	switch coreexecution.RunOperation(v) {
	case coreexecution.RunOperationExecute, coreexecution.RunOperationDeploy, coreexecution.RunOperationUpgrade,
		coreexecution.RunOperationRepair, coreexecution.RunOperationDestroy, coreexecution.RunOperationRollback:
		return true
	default:
		return false
	}
}

func validExecutionV2Trigger(v string) bool {
	switch coreexecution.TriggerKind(v) {
	case coreexecution.TriggerManual, coreexecution.TriggerSchedule, coreexecution.TriggerRetry, coreexecution.TriggerRollback:
		return true
	default:
		return false
	}
}

func parseConfirmationStates(p map[string]any) ([]coreconfirmation.State, *action.Error) {
	raw, present := p["states"]
	if !present {
		return nil, nil
	}
	values, ok := raw.([]any)
	if !ok || len(values) > 5 {
		return nil, action.BadRequest("states must be an array")
	}
	seen := map[coreconfirmation.State]struct{}{}
	out := make([]coreconfirmation.State, 0, len(values))
	for _, item := range values {
		value, ok := item.(string)
		state := coreconfirmation.State(strings.TrimSpace(value))
		if !ok {
			return nil, action.BadRequest("invalid confirmation state")
		}
		switch state {
		case coreconfirmation.StatePending, coreconfirmation.StateConfirmed, coreconfirmation.StateConsumed,
			coreconfirmation.StateRejected, coreconfirmation.StateExpired:
		default:
			return nil, action.BadRequest("invalid confirmation state")
		}
		if _, duplicate := seen[state]; duplicate {
			return nil, action.BadRequest("duplicate confirmation state")
		}
		seen[state] = struct{}{}
		out = append(out, state)
	}
	return out, nil
}

func validateInvocationInput(input map[string]any) *action.Error {
	content, err := json.Marshal(input)
	if err != nil || len(content) > 64<<10 {
		return action.BadRequest("invalid input")
	}
	var walk func(any, int) bool
	walk = func(value any, depth int) bool {
		if depth > 16 {
			return false
		}
		switch typed := value.(type) {
		case map[string]any:
			for key, nested := range typed {
				normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"), ".", "_"))
				switch normalized {
				case "authorization", "password", "passwd", "secret", "token", "access_token", "api_key",
					"private_key", "aws_access_key_id", "aws_secret_access_key", "cookie", "set_cookie":
					return false
				}
				if !walk(nested, depth+1) {
					return false
				}
			}
		case []any:
			for _, nested := range typed {
				if !walk(nested, depth+1) {
					return false
				}
			}
		case string:
			return len(typed) <= 16<<10
		case nil, bool, float64, json.Number:
			return true
		default:
			return false
		}
		return true
	}
	if !walk(input, 0) {
		return action.BadRequest("input contains unsupported or sensitive values")
	}
	return nil
}

func validateExecutionV2Params(name string, p map[string]any) *action.Error {
	requireUUID := func(key string) *action.Error {
		_, err := reqUUID(p, key)
		return err
	}
	requireIdempotency := func() *action.Error {
		_, err := reqUUID(p, "idempotency_key")
		return err
	}
	requireRevision := func(key string) *action.Error {
		_, err := parseUintParam(p, key, true, 1, 0)
		return err
	}
	validateOptionalUUID := func(key string) *action.Error {
		_, err := optUUID(p, key)
		return err
	}

	switch name {
	case "projects.analyze":
		if err := requireUUID("project_id"); err != nil {
			return err
		}
		if err := requireIdempotency(); err != nil {
			return err
		}
		_, err := parseExecutionV2Source(p["source"])
		return err
	case "analyses.get":
		return requireUUID("analysis_id")
	case "targets.list", "plans.list":
		_, _, err := pageParam(p)
		return err
	case "targets.get":
		if err := requireUUID("target_id"); err != nil {
			return err
		}
		_, err := parseUintParam(p, "revision", false, 1, 0)
		return err
	case "targets.import":
		if err := requireUUID("credential_id"); err != nil {
			return err
		}
		if err := requireRevision("credential_revision"); err != nil {
			return err
		}
		instanceID, err := parseStringField(p, "instance_id", true, 35)
		if err != nil {
			return err
		}
		if !executionV2InstanceRE.MatchString(instanceID) {
			return action.BadRequest("invalid instance_id")
		}
		return requireIdempotency()
	case "targets.reserve":
		if err := requireUUID("credential_id"); err != nil {
			return err
		}
		if err := requireRevision("credential_revision"); err != nil {
			return err
		}
		instanceType, err := parseStringField(p, "instance_type", true, 65)
		if err != nil || !executionV2InstanceTypeRE.MatchString(instanceType) {
			return action.BadRequest("invalid instance_type")
		}
		if _, err := parseUintParam(p, "volume_gib", true, 8, 16384); err != nil {
			return err
		}
		return requireIdempotency()
	case "targets.observe":
		if err := requireUUID("target_id"); err != nil {
			return err
		}
		if err := requireRevision("target_revision"); err != nil {
			return err
		}
		return requireIdempotency()
	case "plans.create":
		for _, key := range []string{"project_id", "analysis_id", "target_id"} {
			if err := requireUUID(key); err != nil {
				return err
			}
		}
		if err := requireRevision("target_revision"); err != nil {
			return err
		}
		if err := validatePlanSelection(p); err != nil {
			return err
		}
		if _, err := optionalExecutionV2AIConfiguration(p); err != nil {
			return err
		}
		return requireIdempotency()
	case "plans.revise":
		for _, key := range []string{"plan_id", "target_id"} {
			if err := requireUUID(key); err != nil {
				return err
			}
		}
		if err := requireRevision("target_revision"); err != nil {
			return err
		}
		if err := requireRevision("expected_revision"); err != nil {
			return err
		}
		if err := validatePlanSelection(p); err != nil {
			return err
		}
		if _, err := optionalExecutionV2AIConfiguration(p); err != nil {
			return err
		}
		return requireIdempotency()
	case "plans.get":
		if err := requireUUID("plan_id"); err != nil {
			return err
		}
		_, err := parseUintParam(p, "revision", false, 1, 0)
		return err
	case "deployments.list":
		if err := validateOptionalUUID("project_id"); err != nil {
			return err
		}
		_, _, err := pageParam(p)
		return err
	case "deployments.get":
		return requireUUID("deployment_id")
	case "deployments.events":
		if err := requireUUID("deployment_id"); err != nil {
			return err
		}
		_, _, err := parseEventPage(p)
		return err
	case "runs.create":
		if err := requireUUID("plan_id"); err != nil {
			return err
		}
		if err := requireRevision("plan_revision"); err != nil {
			return err
		}
		if err := requireIdempotency(); err != nil {
			return err
		}
		operation, err := parseStringField(p, "operation", true, 32)
		if err != nil || !validExecutionV2RunOperation(operation) {
			return action.BadRequest("invalid operation")
		}
		trigger, err := parseStringField(p, "trigger_kind", false, 32)
		if err != nil || (trigger != "" && !validExecutionV2Trigger(trigger)) {
			return action.BadRequest("invalid trigger_kind")
		}
		rollbackID, err := optUUID(p, "rollback_of_run_id")
		if err != nil {
			return err
		}
		if (operation == string(coreexecution.RunOperationRollback)) != (rollbackID != "") {
			return action.BadRequest("rollback_of_run_id must be supplied only for rollback")
		}
		return nil
	case "runs.get":
		return requireUUID("run_id")
	case "runs.list":
		for _, key := range []string{"project_id", "deployment_id"} {
			if err := validateOptionalUUID(key); err != nil {
				return err
			}
		}
		_, _, err := pageParam(p)
		return err
	case "runs.cancel", "runs.retry":
		if err := requireUUID("run_id"); err != nil {
			return err
		}
		if err := requireRevision("expected_revision"); err != nil {
			return err
		}
		return requireIdempotency()
	case "runs.reconcile":
		for _, key := range []string{"run_id", "stage_id"} {
			if err := requireUUID(key); err != nil {
				return err
			}
		}
		if err := requireRevision("expected_revision"); err != nil {
			return err
		}
		return requireIdempotency()
	case "runs.events":
		if err := requireUUID("run_id"); err != nil {
			return err
		}
		_, _, err := parseEventPage(p)
		return err
	case "confirmations.get":
		return requireUUID("confirmation_id")
	case "confirmations.list":
		if _, _, err := pageParam(p); err != nil {
			return err
		}
		_, err := parseConfirmationStates(p)
		return err
	case "confirmations.confirm", "confirmations.reject":
		if err := requireUUID("confirmation_id"); err != nil {
			return err
		}
		if err := requireRevision("expected_revision"); err != nil {
			return err
		}
		return requireIdempotency()
	case "artifacts.get":
		return requireUUID("artifact_id")
	case "service_bindings.list":
		if err := validateOptionalUUID("project_id"); err != nil {
			return err
		}
		_, _, err := pageParam(p)
		return err
	case "service_bindings.get":
		return requireUUID("binding_id")
	case "service_bindings.invoke":
		if err := requireUUID("binding_id"); err != nil {
			return err
		}
		if err := requireRevision("expected_revision"); err != nil {
			return err
		}
		if err := requireIdempotency(); err != nil {
			return err
		}
		if _, err := parseStringField(p, "operation", true, 128); err != nil {
			return err
		}
		input, ok := p["input"].(map[string]any)
		if !ok {
			return action.BadRequest("input must be an object")
		}
		return validateInvocationInput(input)
	case "secrets.create":
		provider, err := parseStringField(p, "provider", true, 64)
		if err != nil || !executionV2NameRE.MatchString(provider) {
			return action.BadRequest("invalid provider")
		}
		purpose, err := parseStringField(p, "purpose", true, 64)
		if err != nil || purpose != coreexecution.AISecretPurposeProviderAPIKey {
			return action.BadRequest("invalid purpose")
		}
		value, ok := p["value"].(string)
		if !ok || value == "" || len(value) > 16<<10 || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") {
			return action.BadRequest("invalid value")
		}
		return requireIdempotency()
	case "secrets.get":
		if err := requireUUID("secret_ref"); err != nil {
			return err
		}
		_, err := parseUintParam(p, "revision", false, 1, 0)
		return err
	case "secrets.list":
		_, _, err := pageParam(p)
		return err
	case "secrets.revoke":
		if err := requireUUID("secret_ref"); err != nil {
			return err
		}
		if err := requireRevision("expected_revision"); err != nil {
			return err
		}
		return requireIdempotency()
	default:
		return action.BadRequest("unsupported execution.v2 action")
	}
}

func (p *executionV2Port) analyze(ctx context.Context, owner string, in map[string]any) (any, *action.Error) {
	if p.cfg.Analyze == nil {
		return nil, executionV2NotReady()
	}
	source, e := parseExecutionV2Source(in["source"])
	if e != nil {
		return nil, e
	}
	out, err := p.cfg.Analyze.Analyze(ctx, owner, ExecutionV2AnalyzeRequest{
		ProjectID:      optString(in, "project_id"),
		Source:         source,
		IdempotencyKey: optString(in, "idempotency_key"),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"analysis": executionV2AnalysisMap(owner, out)}, nil
}

func (p *executionV2Port) analysisGet(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "analysis_id")
	if e != nil {
		return nil, e
	}
	v, err := s.GetAnalysis(ctx, o, id)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"analysis": executionV2AnalysisMap(o, v)}, nil
}
func (p *executionV2Port) targetsList(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	n, c, e := pageParam(in)
	if e != nil {
		return nil, e
	}
	v, err := s.ListTargets(ctx, o, c, n)
	if err != nil {
		return nil, mapErr(err)
	}
	items := make([]any, 0, len(v.Items))
	for _, target := range v.Items {
		items = append(items, executionV2TargetMap(o, target))
	}
	return map[string]any{"targets": items, "next_page_token": v.NextCursor}, nil
}
func (p *executionV2Port) targetGet(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "target_id")
	if e != nil {
		return nil, e
	}
	v, err := s.GetTarget(ctx, o, id, uintParam(in, "revision"))
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"target": executionV2TargetMap(o, v)}, nil
}
func (p *executionV2Port) targetImport(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	if p.cfg.TargetImport == nil {
		return nil, executionV2NotReady()
	}
	v, err := p.cfg.TargetImport.ImportTarget(ctx, o, ExecutionV2TargetImportRequest{
		CredentialID:       optString(in, "credential_id"),
		CredentialRevision: uintParam(in, "credential_revision"),
		InstanceID:         optString(in, "instance_id"),
		IdempotencyKey:     optString(in, "idempotency_key"),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{
		"target":         executionV2TargetMap(o, v.Target),
		"observation_id": v.ObservationID,
		"observation":    executionV2ObservationMap(o, v.Observation),
	}, nil
}
func (p *executionV2Port) targetReserve(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	if p.cfg.TargetReserve == nil {
		return nil, executionV2NotReady()
	}
	volume, parseErr := parseUintParam(in, "volume_gib", true, 8, 16384)
	if parseErr != nil {
		return nil, parseErr
	}
	v, err := p.cfg.TargetReserve.ReserveTarget(ctx, o, ExecutionV2TargetReserveRequest{
		CredentialID:       optString(in, "credential_id"),
		CredentialRevision: uintParam(in, "credential_revision"),
		InstanceType:       optString(in, "instance_type"),
		VolumeGiB:          uint32(volume),
		IdempotencyKey:     optString(in, "idempotency_key"),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"target": executionV2TargetMap(o, v)}, nil
}
func (p *executionV2Port) planCreate(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	if p.cfg.PlanCompiler == nil {
		return nil, executionV2NotReady()
	}
	aiConfiguration, parseErr := optionalExecutionV2AIConfiguration(in)
	if parseErr != nil {
		return nil, parseErr
	}
	out, err := p.cfg.PlanCompiler.Compile(ctx, o, ExecutionV2PlanCreateRequest{
		ProjectID:       optString(in, "project_id"),
		AnalysisID:      optString(in, "analysis_id"),
		Intent:          optString(in, "intent"),
		RecipeID:        optString(in, "recipe_id"),
		TargetID:        optString(in, "target_id"),
		TargetRevision:  uintParam(in, "target_revision"),
		Purpose:         coreexecution.PlanPurpose(optString(in, "purpose")),
		AIConfiguration: aiConfiguration,
		IdempotencyKey:  optString(in, "idempotency_key"),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"plan": executionV2PlanMap(o, out)}, nil
}

func (p *executionV2Port) planRevise(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	if p.cfg.PlanCompiler == nil {
		return nil, executionV2NotReady()
	}
	aiConfiguration, parseErr := optionalExecutionV2AIConfiguration(in)
	if parseErr != nil {
		return nil, parseErr
	}
	out, err := p.cfg.PlanCompiler.Revise(ctx, o, ExecutionV2PlanReviseRequest{
		PlanID:           optString(in, "plan_id"),
		ExpectedRevision: uintParam(in, "expected_revision"),
		Intent:           optString(in, "intent"),
		RecipeID:         optString(in, "recipe_id"),
		TargetID:         optString(in, "target_id"),
		TargetRevision:   uintParam(in, "target_revision"),
		Purpose:          coreexecution.PlanPurpose(optString(in, "purpose")),
		AIConfiguration:  aiConfiguration,
		IdempotencyKey:   optString(in, "idempotency_key"),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"plan": executionV2PlanMap(o, out)}, nil
}

func (p *executionV2Port) secretCreate(ctx context.Context, owner string, in map[string]any) (any, *action.Error) {
	if p.cfg.Secrets == nil {
		return nil, executionV2NotReady()
	}
	secret, err := p.cfg.Secrets.CreateExecutionSecret(ctx, storage.ExecutionSecretCreateRequest{OwnerID: owner, Provider: optString(in, "provider"), Purpose: optString(in, "purpose"), Value: in["value"].(string), IdempotencyID: optString(in, "idempotency_key")})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"secret": executionV2SecretMap(secret)}, nil
}

func (p *executionV2Port) secretGet(ctx context.Context, owner string, in map[string]any) (any, *action.Error) {
	if p.cfg.Secrets == nil {
		return nil, executionV2NotReady()
	}
	secret, err := p.cfg.Secrets.GetExecutionSecret(ctx, owner, optString(in, "secret_ref"), uintParam(in, "revision"))
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"secret": executionV2SecretMap(secret)}, nil
}

func (p *executionV2Port) secretsList(ctx context.Context, owner string, in map[string]any) (any, *action.Error) {
	if p.cfg.Secrets == nil {
		return nil, executionV2NotReady()
	}
	limit, cursor, parseErr := pageParam(in)
	if parseErr != nil {
		return nil, parseErr
	}
	page, err := p.cfg.Secrets.ListExecutionSecrets(ctx, owner, cursor, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	items := make([]any, 0, len(page.Items))
	for _, secret := range page.Items {
		items = append(items, executionV2SecretMap(secret))
	}
	return map[string]any{"secrets": items, "next_page_token": page.NextCursor}, nil
}

func (p *executionV2Port) secretRevoke(ctx context.Context, owner string, in map[string]any) (any, *action.Error) {
	if p.cfg.Secrets == nil {
		return nil, executionV2NotReady()
	}
	secret, err := p.cfg.Secrets.RevokeExecutionSecret(ctx, storage.ExecutionSecretRevokeRequest{OwnerID: owner, SecretRef: optString(in, "secret_ref"), ExpectedRevision: uintParam(in, "expected_revision"), IdempotencyID: optString(in, "idempotency_key")})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"secret": executionV2SecretMap(secret)}, nil
}
func (p *executionV2Port) planGet(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "plan_id")
	if e != nil {
		return nil, e
	}
	var v coreexecution.ExecutionPlan
	var err error
	if r := uintParam(in, "revision"); r > 0 {
		v, err = s.GetPlanRevision(ctx, o, id, r)
	} else {
		v, err = s.GetCurrentPlan(ctx, o, id)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"plan": executionV2PlanMap(o, v)}, nil
}
func (p *executionV2Port) plansList(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	n, c, e := pageParam(in)
	if e != nil {
		return nil, e
	}
	v, err := s.ListExecutionPlans(ctx, o, c, n)
	if err != nil {
		return nil, mapErr(err)
	}
	items := make([]any, 0, len(v.Items))
	for _, item := range v.Items {
		items = append(items, executionV2PlanRecordMap(item))
	}
	return map[string]any{"plans": items, "next_page_token": v.NextCursor}, nil
}
func (p *executionV2Port) deploymentsList(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	n, c, e := pageParam(in)
	if e != nil {
		return nil, e
	}
	v, err := s.ListExecutionDeployments(ctx, o, optString(in, "project_id"), c, n)
	if err != nil {
		return nil, mapErr(err)
	}
	items := make([]any, 0, len(v.Items))
	for _, item := range v.Items {
		items = append(items, executionV2DeploymentMap(item))
	}
	return map[string]any{"deployments": items, "next_page_token": v.NextCursor}, nil
}
func (p *executionV2Port) deploymentGet(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "deployment_id")
	if e != nil {
		return nil, e
	}
	v, err := s.GetExecutionDeployment(ctx, o, id)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"deployment": executionV2DeploymentMap(v)}, nil
}
func (p *executionV2Port) deploymentEvents(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "deployment_id")
	if e != nil {
		return nil, e
	}
	after, limit, e := parseEventPage(in)
	if e != nil {
		return nil, e
	}
	v, n, err := s.ListDeploymentEvents(ctx, o, id, after, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	items := make([]any, 0, len(v))
	for _, item := range v {
		items = append(items, executionV2DeploymentEventMap(item))
	}
	return map[string]any{"events": items, "next_sequence": n}, nil
}
func (p *executionV2Port) runCreate(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	if p.cfg.Coordinator == nil {
		return nil, executionV2NotReady()
	}
	id, e := reqUUID(in, "plan_id")
	if e != nil {
		return nil, e
	}
	idem, e := reqUUID(in, "idempotency_key")
	if e != nil {
		return nil, e
	}
	v, err := p.cfg.Coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: o, PlanID: id, PlanRevision: uintParam(in, "plan_revision"), IdempotencyKey: idem, Operation: coreexecution.RunOperation(optString(in, "operation")), TriggerKind: coreexecution.TriggerKind(optString(in, "trigger_kind")), RollbackOfRunID: optString(in, "rollback_of_run_id")})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"run": mapOut(v.Run), "stages": mapOut(v.Stages)}, nil
}
func (p *executionV2Port) runGet(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "run_id")
	if e != nil {
		return nil, e
	}
	v, err := s.GetExecutionRun(ctx, o, id)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"run": mapOut(v.Run), "stages": mapOut(v.Stages)}, nil
}
func (p *executionV2Port) runsList(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	n, c, e := pageParam(in)
	if e != nil {
		return nil, e
	}
	v, err := s.ListExecutionRuns(ctx, o, optString(in, "project_id"), optString(in, "deployment_id"), c, n)
	if err != nil {
		return nil, mapErr(err)
	}
	items := make([]any, 0, len(v.Items))
	for _, item := range v.Items {
		items = append(items, mapOut(item.Run))
	}
	return map[string]any{"runs": items, "next_page_token": v.NextCursor}, nil
}
func (p *executionV2Port) runEvents(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "run_id")
	if e != nil {
		return nil, e
	}
	after, limit, e := parseEventPage(in)
	if e != nil {
		return nil, e
	}
	v, n, err := s.ListExecutionEvents(ctx, o, id, after, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	items := make([]any, 0, len(v))
	for _, item := range v {
		items = append(items, executionV2EventMap(item))
	}
	return map[string]any{"events": items, "next_sequence": n}, nil
}
func (p *executionV2Port) runCancel(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	c, ok := p.cfg.Coordinator.(executionV2Canceler)
	if !ok {
		return nil, executionV2NotReady()
	}
	id, e := reqUUID(in, "run_id")
	if e != nil {
		return nil, e
	}
	idem, e := reqUUID(in, "idempotency_key")
	if e != nil {
		return nil, e
	}
	v, err := c.CancelRun(ctx, coreexecution.CancelRunCommand{OwnerID: o, RunID: id, IdempotencyKey: idem, ExpectedRevision: uintParam(in, "expected_revision")})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"run": mapOut(v)}, nil
}
func (p *executionV2Port) runRetry(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	c, ok := p.cfg.Coordinator.(executionV2Retrier)
	if !ok {
		return nil, executionV2NotReady()
	}
	id, e := reqUUID(in, "run_id")
	if e != nil {
		return nil, e
	}
	idem, e := reqUUID(in, "idempotency_key")
	if e != nil {
		return nil, e
	}
	v, err := c.RetryRun(ctx, coreexecution.RetryRunCommand{OwnerID: o, RunID: id, IdempotencyKey: idem, ExpectedRevision: uintParam(in, "expected_revision")})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"run": mapOut(v.Run)}, nil
}
func (p *executionV2Port) observe(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	if p.cfg.Observe == nil {
		return nil, executionV2NotReady()
	}
	v, err := p.cfg.Observe.Observe(ctx, o, ExecutionV2ObserveRequest{
		TargetID:       optString(in, "target_id"),
		TargetRevision: uintParam(in, "target_revision"),
		IdempotencyKey: optString(in, "idempotency_key"),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"observation": executionV2ObservationMap(o, v)}, nil
}
func (p *executionV2Port) reconcile(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	if p.cfg.Reconcile == nil {
		return nil, executionV2NotReady()
	}
	v, err := p.cfg.Reconcile.Reconcile(ctx, o, ExecutionV2ReconcileRequest{
		RunID:            optString(in, "run_id"),
		StageID:          optString(in, "stage_id"),
		ExpectedRevision: uintParam(in, "expected_revision"),
		IdempotencyKey:   optString(in, "idempotency_key"),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"run": mapOut(v)}, nil
}
func (p *executionV2Port) confirmationGet(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "confirmation_id")
	if e != nil {
		return nil, e
	}
	v, err := s.GetV2Confirmation(ctx, o, id)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"confirmation": executionV2ConfirmationMap(v)}, nil
}
func (p *executionV2Port) confirmationsList(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	n, c, e := pageParam(in)
	if e != nil {
		return nil, e
	}
	states, e := parseConfirmationStates(in)
	if e != nil {
		return nil, e
	}
	v, err := s.ListV2Confirmations(ctx, o, c, states, n)
	if err != nil {
		return nil, mapErr(err)
	}
	items := make([]any, 0, len(v.Items))
	for _, item := range v.Items {
		items = append(items, executionV2ConfirmationMap(item))
	}
	return map[string]any{"confirmations": items, "next_page_token": v.NextCursor}, nil
}
func (p *executionV2Port) confirm(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	if p.cfg.Coordinator == nil {
		return nil, executionV2NotReady()
	}
	id, e := reqUUID(in, "confirmation_id")
	if e != nil {
		return nil, e
	}
	idem, e := reqUUID(in, "idempotency_key")
	if e != nil {
		return nil, e
	}
	_, err := p.cfg.Coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{OwnerID: o, ConfirmationID: id, IdempotencyKey: idem, ExpectedRevision: int64(uintParam(in, "expected_revision"))})
	if err != nil {
		return nil, mapErr(err)
	}
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	v, err := s.GetV2Confirmation(ctx, o, id)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"confirmation": executionV2ConfirmationMap(v)}, nil
}
func (p *executionV2Port) reject(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	c, ok := p.cfg.Coordinator.(executionV2Rejector)
	if !ok {
		return nil, executionV2NotReady()
	}
	id, e := reqUUID(in, "confirmation_id")
	if e != nil {
		return nil, e
	}
	idem, e := reqUUID(in, "idempotency_key")
	if e != nil {
		return nil, e
	}
	_, err := c.RejectStage(ctx, coreexecution.RejectStageCommand{OwnerID: o, ConfirmationID: id, IdempotencyKey: idem, ExpectedRevision: int64(uintParam(in, "expected_revision"))})
	if err != nil {
		return nil, mapErr(err)
	}
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	v, err := s.GetV2Confirmation(ctx, o, id)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"confirmation": executionV2ConfirmationMap(v)}, nil
}
func (p *executionV2Port) artifactGet(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "artifact_id")
	if e != nil {
		return nil, e
	}
	v, err := s.GetArtifactMetadata(ctx, o, id)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"artifact": executionV2ArtifactMap(v)}, nil
}
func (p *executionV2Port) bindingsList(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	n, c, e := pageParam(in)
	if e != nil {
		return nil, e
	}
	v, next, err := s.ListServiceBindings(ctx, o, optString(in, "project_id"), c, n)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"bindings": mapOut(v), "next_page_token": next}, nil
}
func (p *executionV2Port) bindingGet(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	s, e := pStore(p)
	if e != nil {
		return nil, e
	}
	id, e := reqUUID(in, "binding_id")
	if e != nil {
		return nil, e
	}
	v, err := s.GetServiceBinding(ctx, o, id)
	if err != nil {
		return nil, mapErr(err)
	}
	return map[string]any{"binding": mapOut(v)}, nil
}
func (p *executionV2Port) invoke(ctx context.Context, o string, in map[string]any) (any, *action.Error) {
	if p.cfg.Invoke == nil {
		return nil, executionV2NotReady()
	}
	input, _ := in["input"].(map[string]any)
	v, err := p.cfg.Invoke.Invoke(ctx, o, ExecutionV2InvokeRequest{
		BindingID:        optString(in, "binding_id"),
		Operation:        optString(in, "operation"),
		Input:            input,
		ExpectedRevision: uintParam(in, "expected_revision"),
		IdempotencyKey:   optString(in, "idempotency_key"),
	})
	if err != nil {
		// Adapter errors can include upstream response bodies or headers. Do not
		// route them through mapErr, which logs the original error.
		return nil, executionV2InvokeFailure()
	}
	safe, err := storage.SafeServiceBindingInvokeOutput(v)
	if err != nil {
		return nil, executionV2InvokeOutputRejected()
	}
	return map[string]any{"result": safe}, nil
}

func executionV2InvokeFailure() *action.Error {
	return action.CodedError(http.StatusBadGateway, "execution_v2_invoke_failed", "service binding invocation failed")
}

func executionV2InvokeOutputRejected() *action.Error {
	return action.CodedError(http.StatusBadGateway, "execution_v2_invoke_output_rejected", "service binding invocation returned unsafe output")
}
