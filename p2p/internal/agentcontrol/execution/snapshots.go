package execution

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// Snapshot schemas are immutable wire envelopes.  In particular, plan
// status is intentionally absent: lifecycle is a mutable run/plan concern,
// never part of the executable plan identity.
const (
	PlanSnapshotSchema  = "execution-plan-snapshot/v2"
	StageSnapshotSchema = "execution-stage-snapshot/v2"
	StepSnapshotSchema  = "execution-step-snapshot/v2"
	MaxSnapshotBytes    = 1 << 20
)

type PlanSnapshot struct {
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
	Digest          Digest                  `json:"digest"`
}

// ExecutionPlanSnapshot is the descriptive alias used by adapters.
type ExecutionPlanSnapshot = PlanSnapshot

type StageSnapshot struct {
	SchemaVersion string         `json:"schema_version"`
	Stage         ExecutionStage `json:"stage"`
}

type ExecutionStageSnapshot = StageSnapshot

type StepSet string

const (
	StepSetForward  StepSet = "forward"
	StepSetRollback StepSet = "rollback"
)

type StepSnapshot struct {
	SchemaVersion string        `json:"schema_version"`
	StepSet       StepSet       `json:"step_set"`
	Step          ExecutionStep `json:"step"`
}

type ExecutionStepSnapshot = StepSnapshot

func PlanSnapshotFromPlan(p ExecutionPlan) (PlanSnapshot, error) {
	n, err := p.Normalize()
	if err != nil {
		return PlanSnapshot{}, err
	}
	s := planSnapshotFromNormalized(n)
	// Existing plan digests are status-independent and remain the plan pin.
	s.Digest = n.Digest
	return s, nil
}

func planSnapshotFromNormalized(p ExecutionPlan) PlanSnapshot {
	cloned := clonePlan(p)
	return PlanSnapshot{SchemaVersion: PlanSnapshotSchema, ID: p.ID, Revision: p.Revision, OwnerID: p.OwnerID, ProjectID: p.ProjectID, AnalysisID: p.AnalysisID, Purpose: p.Purpose, DeploymentID: p.DeploymentID, AIConfiguration: cloned.AIConfiguration, Placement: p.Placement, Targets: append([]ExecutionTarget(nil), p.Targets...), Artifacts: append([]ArtifactRef(nil), p.Artifacts...), Skills: append([]SkillRef(nil), p.Skills...), Recipes: append([]RecipeRef(nil), p.Recipes...), Stages: cloned.Stages, Outputs: append([]OutputDeclaration(nil), p.Outputs...), CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt}
}

func (s PlanSnapshot) Normalize() (PlanSnapshot, error) {
	if s.SchemaVersion == "" {
		s.SchemaVersion = PlanSnapshotSchema
	}
	if s.SchemaVersion != PlanSnapshotSchema {
		return PlanSnapshot{}, ErrInvalid
	}
	p := ExecutionPlan{SchemaVersion: SchemaVersion, ID: s.ID, Revision: s.Revision, OwnerID: s.OwnerID, ProjectID: s.ProjectID, AnalysisID: s.AnalysisID, Purpose: s.Purpose, DeploymentID: s.DeploymentID, AIConfiguration: s.AIConfiguration, Placement: s.Placement, Targets: s.Targets, Artifacts: s.Artifacts, Skills: s.Skills, Recipes: s.Recipes, Stages: s.Stages, Outputs: s.Outputs, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt, Status: PlanReady}
	n, err := p.Normalize()
	if err != nil {
		return PlanSnapshot{}, err
	}
	provided := s.Digest
	s = planSnapshotFromNormalized(n)
	// The plan digest is the single authoritative content identity.  A second
	// envelope digest would permit two byte representations of the same plan.
	if provided != "" && provided != n.Digest {
		return PlanSnapshot{}, ErrDigestMismatch
	}
	s.Digest = n.Digest
	return s, nil
}

func (s PlanSnapshot) Validate() error { _, err := s.Normalize(); return err }

func StageSnapshotFromStage(stage ExecutionStage) (StageSnapshot, error) {
	n := cloneStageValue(stage)
	if err := normalizeStage(&n); err != nil {
		return StageSnapshot{}, err
	}
	return StageSnapshot{SchemaVersion: StageSnapshotSchema, Stage: n}, nil
}

func (s StageSnapshot) Normalize() (StageSnapshot, error) {
	if s.SchemaVersion == "" {
		s.SchemaVersion = StageSnapshotSchema
	}
	if s.SchemaVersion != StageSnapshotSchema {
		return StageSnapshot{}, ErrInvalid
	}
	n := cloneStageValue(s.Stage)
	if err := normalizeStage(&n); err != nil {
		return StageSnapshot{}, err
	}
	s.Stage = n
	return s, nil
}
func (s StageSnapshot) Validate() error { _, err := s.Normalize(); return err }

func StepSnapshotFromStep(step ExecutionStep, set StepSet) (StepSnapshot, error) {
	n := cloneStep(step)
	if err := normalizeStep(&n); err != nil {
		return StepSnapshot{}, err
	}
	if set != StepSetForward && set != StepSetRollback {
		return StepSnapshot{}, ErrInvalid
	}
	return StepSnapshot{SchemaVersion: StepSnapshotSchema, StepSet: set, Step: n}, nil
}
func (s StepSnapshot) Normalize() (StepSnapshot, error) {
	if s.SchemaVersion == "" {
		s.SchemaVersion = StepSnapshotSchema
	}
	if s.SchemaVersion != StepSnapshotSchema || (s.StepSet != StepSetForward && s.StepSet != StepSetRollback) {
		return StepSnapshot{}, ErrInvalid
	}
	n := cloneStep(s.Step)
	if err := normalizeStep(&n); err != nil {
		return StepSnapshot{}, err
	}
	s.Step = n
	return s, nil
}
func (s StepSnapshot) Validate() error { _, err := s.Normalize(); return err }

func EncodePlanSnapshot(s PlanSnapshot) ([]byte, error)   { return encodeSnapshot(s) }
func EncodeStageSnapshot(s StageSnapshot) ([]byte, error) { return encodeSnapshot(s) }
func EncodeStepSnapshot(s StepSnapshot) ([]byte, error)   { return encodeSnapshot(s) }

func DecodePlanSnapshot(raw []byte) (PlanSnapshot, error) {
	var s PlanSnapshot
	if err := decodeSnapshot(raw, &s); err != nil {
		return PlanSnapshot{}, err
	}
	n, err := s.Normalize()
	if err != nil || !canonicalSnapshotBytes(raw, n) {
		return PlanSnapshot{}, ErrInvalid
	}
	return n, nil
}
func EncodeExecutionPlanSnapshot(s ExecutionPlanSnapshot) ([]byte, error) {
	return EncodePlanSnapshot(s)
}
func DecodeExecutionPlanSnapshot(raw []byte) (ExecutionPlanSnapshot, error) {
	return DecodePlanSnapshot(raw)
}
func DecodeStageSnapshot(raw []byte) (StageSnapshot, error) {
	var s StageSnapshot
	if err := decodeSnapshot(raw, &s); err != nil {
		return StageSnapshot{}, err
	}
	n, err := s.Normalize()
	if err != nil || !canonicalSnapshotBytes(raw, n) {
		return StageSnapshot{}, ErrInvalid
	}
	return n, nil
}
func EncodeExecutionStageSnapshot(s ExecutionStageSnapshot) ([]byte, error) {
	return EncodeStageSnapshot(s)
}
func DecodeExecutionStageSnapshot(raw []byte) (ExecutionStageSnapshot, error) {
	return DecodeStageSnapshot(raw)
}
func DecodeStepSnapshot(raw []byte) (StepSnapshot, error) {
	var s StepSnapshot
	if err := decodeSnapshot(raw, &s); err != nil {
		return StepSnapshot{}, err
	}
	n, err := s.Normalize()
	if err != nil || !canonicalSnapshotBytes(raw, n) {
		return StepSnapshot{}, ErrInvalid
	}
	return n, nil
}
func EncodeExecutionStepSnapshot(s ExecutionStepSnapshot) ([]byte, error) {
	return EncodeStepSnapshot(s)
}
func DecodeExecutionStepSnapshot(raw []byte) (ExecutionStepSnapshot, error) {
	return DecodeStepSnapshot(raw)
}

func encodeSnapshot(v any) ([]byte, error) {
	var n any
	switch x := v.(type) {
	case PlanSnapshot:
		var err error
		n, err = x.Normalize()
		if err != nil {
			return nil, err
		}
	case StageSnapshot:
		var err error
		n, err = x.Normalize()
		if err != nil {
			return nil, err
		}
	case StepSnapshot:
		var err error
		n, err = x.Normalize()
		if err != nil {
			return nil, err
		}
	default:
		return nil, ErrInvalid
	}
	b, err := json.Marshal(n)
	if err != nil || len(b) > MaxSnapshotBytes {
		return nil, ErrInvalid
	}
	return b, err
}

func decodeSnapshot(raw []byte, out any) error {
	if len(raw) == 0 || len(raw) > MaxSnapshotBytes {
		return ErrInvalid
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return ErrInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return ErrInvalid
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return ErrInvalid
	}
	return nil
}

func canonicalSnapshotBytes(raw []byte, value any) bool {
	canonical, err := json.Marshal(value)
	return err == nil && bytes.Equal(raw, canonical)
}

// rejectDuplicateJSONKeys walks the token stream before decoding into a Go
// struct. encoding/json otherwise accepts the last duplicate member, which
// would make a signed snapshot ambiguous to non-Go consumers.
func rejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := scanJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return ErrInvalid
	}
	return nil
}

func scanJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrInvalid
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrInvalid
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		closing, err := dec.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec); err != nil {
				return err
			}
		}
		closing, err := dec.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func cloneStageValue(s ExecutionStage) ExecutionStage {
	s.DependsOn = append([]string(nil), s.DependsOn...)
	s.Probes = append([]string(nil), s.Probes...)
	s.Steps = cloneSteps(s.Steps)
	s.RollbackSteps = cloneSteps(s.RollbackSteps)
	if s.RollbackPolicy != nil {
		p := *s.RollbackPolicy
		s.RollbackPolicy = &p
	}
	return s
}

// SelectStageExecution materializes exactly one immutable step set.
type StageExecutionSelection struct {
	StepSet StepSet
	Steps   []ExecutionStep
	Risk    Risk
	Gate    Gate
	Skipped bool
}

func SelectStageExecution(op RunOperation, stage ExecutionStage) (StageExecutionSelection, error) {
	if op != RunOperationRollback {
		return StageExecutionSelection{StepSet: StepSetForward, Steps: cloneSteps(stage.Steps), Risk: stage.Risk, Gate: stage.Gate}, nil
	}
	if len(stage.RollbackSteps) == 0 {
		return StageExecutionSelection{StepSet: StepSetRollback, Risk: RiskR4, Gate: GateRollback, Skipped: true}, nil
	}
	return StageExecutionSelection{StepSet: StepSetRollback, Steps: cloneSteps(stage.RollbackSteps), Risk: RiskR4, Gate: GateRollback}, nil
}

// Keep strings.TrimSpace referenced by generated callers that pass a schema
// token through this file; it also documents that decoder normalization does
// not silently accept a different schema.
var _ = strings.TrimSpace
