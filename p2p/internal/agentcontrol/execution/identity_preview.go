package execution

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ExecutionNamespace is deliberately package-owned.  UUIDs derived from it
// are stable across process restarts and cannot collide with unrelated UUIDv5
// domains.
var ExecutionNamespace = uuid.MustParse("2c7f7d5e-3db4-4f34-9a4e-0cc3a4b5321b")

func lengthDelimited(parts ...string) []byte {
	b := make([]byte, 0, 32)
	for _, part := range parts {
		b = append(b, fmt.Sprintf("%d:", len(part))...)
		b = append(b, part...)
	}
	return b
}
func deterministicUUID(kind string, parts ...string) string {
	args := append([]string{kind}, parts...)
	return uuid.NewSHA1(ExecutionNamespace, lengthDelimited(args...)).String()
}

func DeterministicRunID(ownerID, idempotencyKey string) string {
	return deterministicUUID("run", ownerID, idempotencyKey)
}
func DeterministicStageID(runID, stageKey string) string {
	return deterministicUUID("stage", runID, stageKey)
}
func DeterministicTaskID(stageID string) string { return deterministicUUID("task", stageID) }
func DeterministicConfirmationID(stageID string) string {
	return deterministicUUID("confirmation", stageID)
}
func DeterministicStageIdempotencyKey(stageID string, operation RunOperation) string {
	return deterministicUUID("stage-idempotency", stageID, string(operation))
}

type ConfirmationPreview struct {
	SchemaVersion       string         `json:"schema_version"`
	OwnerID             string         `json:"owner_id"`
	PlanID              string         `json:"plan_id"`
	PlanRevision        uint64         `json:"plan_revision"`
	PlanDigest          Digest         `json:"plan_digest"`
	DeploymentID        string         `json:"deployment_id,omitempty"`
	RunID               string         `json:"run_id"`
	RunRevision         uint64         `json:"run_revision"`
	StageID             string         `json:"stage_id"`
	StageKey            string         `json:"stage_key"`
	StageRevision       uint64         `json:"stage_revision"`
	StageDigest         Digest         `json:"stage_digest"`
	StageIdempotencyKey string         `json:"stage_idempotency_key"`
	TargetID            string         `json:"target_id"`
	TargetRevision      uint64         `json:"target_revision"`
	TargetDigest        Digest         `json:"target_digest"`
	Title               string         `json:"title,omitempty"`
	Kind                string         `json:"kind"`
	Risk                Risk           `json:"risk"`
	Gate                Gate           `json:"gate"`
	StepSet             StepSet        `json:"step_set"`
	StepKeys            []string       `json:"step_keys"`
	StepKinds           []StepKind     `json:"step_kinds"`
	ExecutionDigest     Digest         `json:"execution_digest"`
	ArtifactSetDigest   Digest         `json:"artifact_set_digest"`
	NetworkGrants       []NetworkGrant `json:"network_grants,omitempty"`
	NetworkDigest       Digest         `json:"network_digest"`
	SecretGrantDigest   Digest         `json:"secret_grant_digest"`
	PolicyDigest        Digest         `json:"policy_digest"`
	CostQuote           CostQuote      `json:"cost_quote"`
	CostQuoteDigest     Digest         `json:"cost_quote_digest"`
	RollbackDigest      Digest         `json:"rollback_digest"`
	ExpiresAt           time.Time      `json:"expires_at"`
	Digest              Digest         `json:"digest"`
}

const ConfirmationPreviewSchema = "execution-confirmation-preview/v2"

func BuildConfirmationPreview(plan ExecutionPlan, run ExecutionRun, stage ExecutionStage) (ConfirmationPreview, error) {
	p, err := plan.Normalize()
	if err != nil {
		return ConfirmationPreview{}, err
	}
	if err := run.Validate(); err != nil ||
		run.OwnerID != p.OwnerID ||
		run.PlanID != p.ID ||
		run.ProjectID != p.ProjectID ||
		run.Purpose != p.Purpose ||
		run.DeploymentID != p.DeploymentID ||
		run.PlanRevision != p.Revision ||
		run.PlanDigest != p.Digest {
		return ConfirmationPreview{}, ErrInvalid
	}
	selectionOp := run.Operation
	provided := cloneStageValue(stage)
	if err := normalizeStage(&provided); err != nil {
		return ConfirmationPreview{}, err
	}
	matched := false
	for _, candidate := range p.Stages {
		if candidate.StageKey == stage.StageKey {
			if candidate.Revision != provided.Revision ||
				candidate.Digest != provided.Digest ||
				candidate.TargetID != provided.TargetID ||
				candidate.TargetRevision != provided.TargetRevision ||
				candidate.TargetDigest != provided.TargetDigest {
				return ConfirmationPreview{}, ErrInvalid
			}
			stage = candidate
			matched = true
			break
		}
	}
	if !matched {
		return ConfirmationPreview{}, ErrInvalid
	}
	selection, err := SelectStageExecution(selectionOp, stage)
	if err != nil || selection.Skipped {
		return ConfirmationPreview{}, ErrInvalid
	}
	var target ExecutionTarget
	for _, t := range p.Targets {
		if t.ID == stage.TargetID && t.Revision == stage.TargetRevision {
			target = t
			break
		}
	}
	if target.ID == "" {
		return ConfirmationPreview{}, ErrInvalid
	}
	keys := make([]string, len(selection.Steps))
	kinds := make([]StepKind, len(selection.Steps))
	for i, step := range selection.Steps {
		keys[i], kinds[i] = step.StepKey, step.Kind
	}
	// Digests in this preview are pins copied from the immutable plan/stage;
	// only derived aggregate digests are canonicalized from selected content.
	executionDigest, err := CanonicalDigest(selection.Steps)
	if err != nil {
		return ConfirmationPreview{}, err
	}
	artifactDigest, err := CanonicalDigest(p.Artifacts)
	if err != nil {
		return ConfirmationPreview{}, err
	}
	networkGrants, err := networkGrantsForSteps(selection.Steps)
	if err != nil {
		return ConfirmationPreview{}, err
	}
	networkDigest, err := CanonicalDigest(struct {
		TargetPolicy NetworkPolicy  `json:"target_policy"`
		StepGrants   []NetworkGrant `json:"step_grants"`
	}{TargetPolicy: target.Network, StepGrants: networkGrants})
	if err != nil {
		return ConfirmationPreview{}, err
	}
	secretDigest, err := CanonicalDigest(secretRefsForSteps(selection.Steps))
	if err != nil {
		return ConfirmationPreview{}, err
	}
	policyDigest, err := CanonicalDigest(struct {
		Risk Risk
		Gate Gate
	}{selection.Risk, selection.Gate})
	if err != nil {
		return ConfirmationPreview{}, err
	}
	costDigest, err := CanonicalDigest(p.Placement.Recommended.CostQuote)
	if err != nil {
		return ConfirmationPreview{}, err
	}
	rollbackDigest, err := CanonicalDigest(stage.RollbackSteps)
	if err != nil {
		return ConfirmationPreview{}, err
	}
	v := ConfirmationPreview{SchemaVersion: ConfirmationPreviewSchema, OwnerID: p.OwnerID, PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, DeploymentID: p.DeploymentID, RunID: run.RunID, RunRevision: run.Revision, StageID: DeterministicStageID(run.RunID, stage.StageKey), StageKey: stage.StageKey, StageRevision: stage.Revision, StageDigest: stage.Digest, StageIdempotencyKey: DeterministicStageIdempotencyKey(DeterministicStageID(run.RunID, stage.StageKey), selectionOp), TargetID: stage.TargetID, TargetRevision: stage.TargetRevision, TargetDigest: stage.TargetDigest, Title: stage.Title, Kind: stage.Kind, Risk: selection.Risk, Gate: selection.Gate, StepSet: selection.StepSet, StepKeys: keys, StepKinds: kinds, ExecutionDigest: executionDigest, ArtifactSetDigest: artifactDigest, NetworkGrants: networkGrants, NetworkDigest: networkDigest, SecretGrantDigest: secretDigest, PolicyDigest: policyDigest, CostQuote: p.Placement.Recommended.CostQuote, CostQuoteDigest: costDigest, RollbackDigest: rollbackDigest, ExpiresAt: p.ExpiresAt}
	v.Digest, err = CanonicalDigest(struct {
		ConfirmationPreview `json:"preview"`
	}{v})
	if err != nil {
		return ConfirmationPreview{}, err
	}
	return v, nil
}

func networkGrantsForSteps(steps []ExecutionStep) ([]NetworkGrant, error) {
	byDigest := make(map[Digest]NetworkGrant)
	for _, step := range steps {
		grants := append([]NetworkGrant(nil), step.NetworkGrants...)
		if step.ScriptRun != nil {
			grants = append(grants, step.ScriptRun.NetworkGrants...)
		}
		for _, grant := range grants {
			normalized, err := grant.Normalize()
			if err != nil {
				return nil, err
			}
			byDigest[normalized.Digest] = normalized
		}
	}
	out := make([]NetworkGrant, 0, len(byDigest))
	for _, grant := range byDigest {
		out = append(out, grant)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}

func secretRefsForSteps(steps []ExecutionStep) []CredentialRef {
	var refs []CredentialRef
	for _, s := range steps {
		refs = append(refs, s.SecretRefs...)
		if s.ScriptRun != nil {
			refs = append(refs, s.ScriptRun.SecretRefs...)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Ref == refs[j].Ref {
			return refs[i].Revision < refs[j].Revision
		}
		return refs[i].Ref < refs[j].Ref
	})
	return refs
}

// BuildConfirmationBinding accepts a preview and, optionally, an execution
// operation.  It copies every existing digest pin verbatim; digest strings
// are never fed back through a digest function.
func BuildConfirmationBinding(args ...any) (ConfirmationBindingSnapshot, error) {
	var p ConfirmationPreview
	var op RunOperation
	for _, arg := range args {
		switch v := arg.(type) {
		case ConfirmationPreview:
			p = v
		case RunOperation:
			op = v
		case string:
			op = RunOperation(v)
		}
	}
	if p.SchemaVersion == "" {
		p.SchemaVersion = ConfirmationPreviewSchema
	}
	if p.SchemaVersion != ConfirmationPreviewSchema || p.StepSet == "" || !p.PlanDigest.Valid() || !p.StageDigest.Valid() || !p.TargetDigest.Valid() || !p.ExecutionDigest.Valid() || !p.ArtifactSetDigest.Valid() || !p.NetworkDigest.Valid() || !p.SecretGrantDigest.Valid() || !p.PolicyDigest.Valid() || !p.CostQuoteDigest.Valid() || !p.RollbackDigest.Valid() || !p.Digest.Valid() {
		return ConfirmationBindingSnapshot{}, ErrInvalid
	}
	if op == "" {
		if p.StepSet == StepSetRollback {
			op = RunOperationRollback
		} else {
			op = RunOperationExecute
		}
	}
	b := ConfirmationBindingSnapshot{OwnerID: p.OwnerID, PlanID: p.PlanID, PlanRevision: p.PlanRevision, PlanDigest: p.PlanDigest, DeploymentID: p.DeploymentID, RunID: p.RunID, RunRevision: p.RunRevision, StageID: p.StageID, StageRevision: p.StageRevision, StageDigest: p.StageDigest, StageIdempotencyKey: p.StageIdempotencyKey, TargetID: p.TargetID, TargetRevision: p.TargetRevision, TargetDigest: p.TargetDigest, ExecutionDigest: p.ExecutionDigest, ArtifactSetDigest: p.ArtifactSetDigest, NetworkDigest: p.NetworkDigest, SecretGrantDigest: p.SecretGrantDigest, PolicyDigest: p.PolicyDigest, CostQuoteDigest: p.CostQuoteDigest, RollbackDigest: p.RollbackDigest, PreviewDigest: p.Digest, Risk: p.Risk, Gate: p.Gate, ExpiresAt: p.ExpiresAt}
	if op == RunOperationRollback && (p.Risk != RiskR4 || p.Gate != GateRollback) {
		return ConfirmationBindingSnapshot{}, ErrInvalid
	}
	return b.Normalize()
}

// Keep this import useful for callers serializing previews in generic paths.
var _ = json.Valid
