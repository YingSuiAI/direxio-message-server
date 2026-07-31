package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"mime"
	"net"
	"net/url"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Masterminds/semver/v3"
	"github.com/google/uuid"
)

var (
	ErrInvalid        = errors.New("execution: invalid")
	ErrExpired        = errors.New("execution: plan expired")
	ErrCycle          = errors.New("execution: dependency cycle")
	ErrDigestMismatch = errors.New("execution: supplied digest mismatch")
)

const (
	maxTargets   = 64
	maxStages    = 128
	maxSteps     = 256
	maxArtifacts = 512
	maxOutputs   = 128
	maxRefs      = 128
	maxTimeout   = 24 * time.Hour
)

func (d Digest) Valid() bool {
	if len(d) != sha256.Size*2 || strings.ToLower(string(d)) != string(d) {
		return false
	}
	_, e := hex.DecodeString(string(d))
	return e == nil
}
func ValidateUUID(s string) bool {
	u, e := uuid.Parse(strings.TrimSpace(s))
	return e == nil && u != uuid.Nil && u.String() == strings.ToLower(strings.TrimSpace(s))
}
func ValidateDigest(s string) bool         { return Digest(s).Valid() }
func CanonicalBytes(v any) ([]byte, error) { return json.Marshal(v) }
func CanonicalJSON(v any) ([]byte, error)  { return CanonicalBytes(v) }
func CanonicalDigest(v any) (Digest, error) {
	b, e := CanonicalBytes(v)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(b)
	return Digest(hex.EncodeToString(sum[:])), nil
}
func ComputeDigest(v any) (Digest, error) { return CanonicalDigest(v) }
func DigestOf(v any) (Digest, error)      { return CanonicalDigest(v) }

func (p ExecutionPlan) Normalize() (ExecutionPlan, error) {
	p = clonePlan(p)
	p.SchemaVersion = strings.TrimSpace(p.SchemaVersion)
	if p.SchemaVersion == "" {
		p.SchemaVersion = SchemaVersion
	}
	p.ID = strings.TrimSpace(p.ID)
	p.OwnerID = strings.TrimSpace(p.OwnerID)
	p.ProjectID = strings.TrimSpace(p.ProjectID)
	p.AnalysisID = strings.TrimSpace(p.AnalysisID)
	p.DeploymentID = strings.TrimSpace(p.DeploymentID)
	if p.AIConfiguration != nil {
		configuration := *p.AIConfiguration
		if err := normalizeAIConfiguration(&configuration); err != nil {
			return ExecutionPlan{}, err
		}
		p.AIConfiguration = &configuration
	}
	if p.SchemaVersion != SchemaVersion || !ValidateUUID(p.ID) || p.Revision == 0 || !validOwnerID(p.OwnerID) || !ValidateUUID(p.ProjectID) || !ValidateUUID(p.AnalysisID) || (p.DeploymentID != "" && !ValidateUUID(p.DeploymentID)) || !validPurpose(p.Purpose) || !validPlanStatus(p.Status) || p.ExpiresAt.IsZero() || p.ExpiresAt.Location() != time.UTC || (!p.CreatedAt.IsZero() && p.CreatedAt.Location() != time.UTC) || len(p.Stages) == 0 || len(p.Targets) == 0 || len(p.Targets) > maxTargets || len(p.Stages) > maxStages || len(p.Artifacts) > maxArtifacts || len(p.Outputs) > maxOutputs || len(p.Skills) > maxRefs || len(p.Recipes) > maxRefs {
		return ExecutionPlan{}, ErrInvalid
	}
	if p.ExpiresAt.Before(p.CreatedAt) && !p.CreatedAt.IsZero() {
		return ExecutionPlan{}, ErrInvalid
	}
	if e := p.Placement.Validate(); e != nil {
		return ExecutionPlan{}, e
	}
	for i := range p.Targets {
		n, err := p.Targets[i].Normalize()
		if err != nil {
			return ExecutionPlan{}, err
		}
		p.Targets[i] = n
	}
	// A stage embeds a target snapshot by ID/revision; its digest is derived
	// from the normalized target in this plan, never trusted as caller input.
	for i := range p.Stages {
		for _, target := range p.Targets {
			if p.Stages[i].TargetID == target.ID && p.Stages[i].TargetRevision == target.Revision {
				p.Stages[i].TargetDigest = target.Digest
				for j := range p.Stages[i].Steps {
					if p.Stages[i].Steps[j].TargetID == target.ID && p.Stages[i].Steps[j].TargetRevision == target.Revision {
						p.Stages[i].Steps[j].TargetDigest = target.Digest
					}
				}
				for j := range p.Stages[i].RollbackSteps {
					if p.Stages[i].RollbackSteps[j].TargetID == target.ID && p.Stages[i].RollbackSteps[j].TargetRevision == target.Revision {
						p.Stages[i].RollbackSteps[j].TargetDigest = target.Digest
					}
				}
			}
		}
	}
	for i := range p.Stages {
		if e := normalizeStage(&p.Stages[i]); e != nil {
			return ExecutionPlan{}, e
		}
	}
	for i := range p.Outputs {
		if e := validateOutput(p.Outputs[i]); e != nil {
			return ExecutionPlan{}, e
		}
	}
	for i := range p.Artifacts {
		if e := validateArtifact(&p.Artifacts[i]); e != nil {
			return ExecutionPlan{}, e
		}
	}
	if duplicateArtifacts(p.Artifacts) || duplicateOutputs(p.Outputs) {
		return ExecutionPlan{}, ErrInvalid
	}
	sort.Slice(p.Targets, func(i, j int) bool { return p.Targets[i].ID < p.Targets[j].ID })
	sort.Slice(p.Artifacts, func(i, j int) bool { return p.Artifacts[i].ID < p.Artifacts[j].ID })
	sort.Slice(p.Outputs, func(i, j int) bool { return p.Outputs[i].Key < p.Outputs[j].Key })
	sort.Slice(p.Skills, func(i, j int) bool {
		return refKey(p.Skills[i].ID, p.Skills[i].Version, p.Skills[i].Digest) < refKey(p.Skills[j].ID, p.Skills[j].Version, p.Skills[j].Digest)
	})
	sort.Slice(p.Recipes, func(i, j int) bool {
		return refKey(p.Recipes[i].ID, p.Recipes[i].Version, p.Recipes[i].Digest) < refKey(p.Recipes[j].ID, p.Recipes[j].Version, p.Recipes[j].Digest)
	})
	if e := validateRefs(p.Skills, p.Recipes); e != nil {
		return ExecutionPlan{}, e
	}
	if e := validatePlanGraph(&p); e != nil {
		return ExecutionPlan{}, e
	}
	if e := validateSecretProvisionDependencies(p); e != nil {
		return ExecutionPlan{}, e
	}
	if e := validateAIPlan(p); e != nil {
		return ExecutionPlan{}, e
	}
	if e := validateRequiredOutputProducers(p); e != nil {
		return ExecutionPlan{}, e
	}
	provided := p.Digest
	p.Digest = ""
	snapshot := p
	snapshot.Status = ""
	snapshot.Digest = ""
	d, e := CanonicalDigest(snapshot)
	if e != nil {
		return ExecutionPlan{}, e
	}
	if provided != "" && provided != d {
		return ExecutionPlan{}, ErrDigestMismatch
	}
	p.Digest = d
	return p, nil
}

func clonePlan(p ExecutionPlan) ExecutionPlan {
	if p.AIConfiguration != nil {
		configuration := *p.AIConfiguration
		p.AIConfiguration = &configuration
	}
	p.Stages = append([]ExecutionStage(nil), p.Stages...)
	p.Targets = append([]ExecutionTarget(nil), p.Targets...)
	p.Artifacts = append([]ArtifactRef(nil), p.Artifacts...)
	p.Outputs = append([]OutputDeclaration(nil), p.Outputs...)
	p.Skills = append([]SkillRef(nil), p.Skills...)
	p.Recipes = append([]RecipeRef(nil), p.Recipes...)
	for i := range p.Stages {
		p.Stages[i].DependsOn = append([]string(nil), p.Stages[i].DependsOn...)
		p.Stages[i].Probes = append([]string(nil), p.Stages[i].Probes...)
		p.Stages[i].Steps = cloneSteps(p.Stages[i].Steps)
		p.Stages[i].RollbackSteps = cloneSteps(p.Stages[i].RollbackSteps)
	}
	for i := range p.Targets {
		p.Targets[i].Capabilities = append([]string(nil), p.Targets[i].Capabilities...)
		p.Targets[i].CredentialRefs = append([]CredentialRef(nil), p.Targets[i].CredentialRefs...)
		p.Targets[i].Network.Allow = append([]NetworkGrant(nil), p.Targets[i].Network.Allow...)
		p.Targets[i].Network.Deny = append([]NetworkGrant(nil), p.Targets[i].Network.Deny...)
		if p.Targets[i].ComputeReservation != nil {
			reservation := *p.Targets[i].ComputeReservation
			p.Targets[i].ComputeReservation = &reservation
		}
	}
	return p
}

func cloneSteps(in []ExecutionStep) []ExecutionStep {
	out := append([]ExecutionStep(nil), in...)
	for i := range out {
		out[i] = cloneStep(out[i])
	}
	return out
}
func cloneStep(in ExecutionStep) ExecutionStep {
	out := in
	out.DependsOn = append([]string(nil), in.DependsOn...)
	out.ArtifactRefs = append([]ArtifactRef(nil), in.ArtifactRefs...)
	out.NetworkGrants = append([]NetworkGrant(nil), in.NetworkGrants...)
	out.SecretRefs = append([]CredentialRef(nil), in.SecretRefs...)
	out.Permissions = append([]PermissionGrant(nil), in.Permissions...)
	if in.ObservationRef != nil {
		v := *in.ObservationRef
		out.ObservationRef = &v
	}
	if in.Postcondition != nil {
		v := *in.Postcondition
		out.Postcondition = &v
	}
	if in.Executor != nil {
		v := *in.Executor
		v.Argv = append([]string(nil), in.Executor.Argv...)
		v.Env = mapsClone(in.Executor.Env)
		v.AllowedExitCodes = append([]int(nil), in.Executor.AllowedExitCodes...)
		v.Redaction.Patterns = append([]string(nil), in.Executor.Redaction.Patterns...)
		if in.Executor.Postcondition != nil {
			post := *in.Executor.Postcondition
			v.Postcondition = &post
		}
		out.Executor = &v
	}
	if in.TargetInspect != nil {
		v := *in.TargetInspect
		out.TargetInspect = &v
	}
	if in.ComputeProvision != nil {
		v := *in.ComputeProvision
		out.ComputeProvision = &v
	}
	if in.ComputeDestroy != nil {
		v := *in.ComputeDestroy
		out.ComputeDestroy = &v
	}
	if in.SourceFetch != nil {
		v := *in.SourceFetch
		out.SourceFetch = &v
	}
	if in.ArtifactUpload != nil {
		v := *in.ArtifactUpload
		out.ArtifactUpload = &v
	}
	if in.PackageEnsure != nil {
		v := *in.PackageEnsure
		out.PackageEnsure = &v
	}
	if in.FilePut != nil {
		v := *in.FilePut
		out.FilePut = &v
	}
	if in.ContainerApply != nil {
		v := *in.ContainerApply
		out.ContainerApply = &v
	}
	if in.SystemdApply != nil {
		v := *in.SystemdApply
		out.SystemdApply = &v
	}
	if in.HTTPProbe != nil {
		v := *in.HTTPProbe
		v.ExpectedStatus = append([]int(nil), v.ExpectedStatus...)
		out.HTTPProbe = &v
	}
	if in.TCPProbe != nil {
		v := *in.TCPProbe
		out.TCPProbe = &v
	}
	if in.ArtifactCollect != nil {
		v := *in.ArtifactCollect
		v.Paths = append([]string(nil), v.Paths...)
		out.ArtifactCollect = &v
	}
	if in.Cleanup != nil {
		v := *in.Cleanup
		out.Cleanup = &v
	}
	if in.SecretProvision != nil {
		v := *in.SecretProvision
		out.SecretProvision = &v
	}
	if in.ExternalAuth != nil {
		v := *in.ExternalAuth
		out.ExternalAuth = &v
	}
	if in.ScriptRun != nil {
		v := *in.ScriptRun
		v.Argv = append([]string(nil), v.Argv...)
		v.NetworkGrants = append([]NetworkGrant(nil), v.NetworkGrants...)
		v.AllowedExitCodes = append([]int(nil), v.AllowedExitCodes...)
		v.Env = mapsClone(v.Env)
		v.SecretRefs = append([]CredentialRef(nil), v.SecretRefs...)
		v.Redaction.Patterns = append([]string(nil), v.Redaction.Patterns...)
		if v.Postcondition != nil {
			pc := *v.Postcondition
			v.Postcondition = &pc
		}
		out.ScriptRun = &v
	}
	return out
}
func mapsClone(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Risk is frozen on each mutating stage; plan-level risk is derived from its
// stages for callers that need a coarse summary.
func (p ExecutionPlan) Risk() Risk {
	max := RiskR0
	for _, s := range p.Stages {
		if s.Risk > max {
			max = s.Risk
		}
	}
	return max
}
func (p ExecutionPlan) Validate() error                { _, e := p.Normalize(); return e }
func (p ExecutionPlan) ValidateAt(now time.Time) error { _, e := p.NormalizeAt(now); return e }
func (p ExecutionPlan) NormalizeAt(now time.Time) (ExecutionPlan, error) {
	p, e := p.Normalize()
	if e != nil {
		return ExecutionPlan{}, e
	}
	if !now.IsZero() && !p.ExpiresAt.After(now) {
		return ExecutionPlan{}, ErrExpired
	}
	return p, nil
}
func NewExecutionPlan(p ExecutionPlan) (ExecutionPlan, error) { return p.NormalizeAt(time.Now().UTC()) }
func (p ExecutionPlan) Seal() (ExecutionPlan, error)          { return p.Normalize() }
func (p ExecutionPlan) CanonicalDigest() (Digest, error) {
	n, e := p.Normalize()
	if e != nil {
		return "", e
	}
	return n.Digest, nil
}

func normalizeStage(s *ExecutionStage) error {
	providedDigest := s.Digest
	s.Digest = ""
	s.StageKey = strings.TrimSpace(s.StageKey)
	s.TargetID = strings.TrimSpace(s.TargetID)
	s.Kind = strings.TrimSpace(s.Kind)
	s.Title = strings.TrimSpace(s.Title)
	if hasDuplicateStrings(s.DependsOn) {
		return ErrInvalid
	}
	s.DependsOn = normalizeSet(s.DependsOn)
	if hasDuplicateGates(s.Effects) {
		return ErrInvalid
	}
	s.Effects = normalizeGates(s.Effects)
	if !validKey(s.StageKey) || s.Revision == 0 || !safeToken(s.Kind) || (s.Title != "" && !safeText(s.Title, 512)) || !ValidateUUID(s.TargetID) || s.TargetRevision == 0 || !s.TargetDigest.Valid() || len(s.Steps) == 0 || len(s.Steps) > maxSteps || len(s.RollbackSteps) > maxSteps || s.TimeoutSeconds == 0 || s.TimeoutSeconds > uint64(maxTimeout/time.Second) {
		return ErrInvalid
	}
	if !validRisk(s.Risk) || !validGate(s.Gate) || !validRiskGate(s.Risk, s.Gate) {
		return ErrInvalid
	}
	for i := range s.Steps {
		if e := normalizeStep(&s.Steps[i]); e != nil {
			return e
		}
		if s.Steps[i].TimeoutSeconds > s.TimeoutSeconds || (s.Steps[i].ScriptRun != nil && s.Steps[i].ScriptRun.TimeoutSeconds > s.TimeoutSeconds) {
			return ErrInvalid
		}
	}
	if len(s.RollbackSteps) == 0 && s.RollbackPolicy != nil {
		return ErrInvalid
	}
	if len(s.RollbackSteps) > 0 {
		if e := normalizeRollbackPolicy(s.RollbackPolicy); e != nil {
			return e
		}
	}
	for i := range s.RollbackSteps {
		if e := normalizeStep(&s.RollbackSteps[i]); e != nil {
			return e
		}
		if s.RollbackSteps[i].TimeoutSeconds > s.TimeoutSeconds || (s.RollbackSteps[i].ScriptRun != nil && s.RollbackSteps[i].ScriptRun.TimeoutSeconds > s.TimeoutSeconds) {
			return ErrInvalid
		}
		// Rollback is a separately confirmed R4 operation.  Do not permit a
		// normal mutating action to be smuggled into that declaration.
		if s.RollbackSteps[i].Kind != StepCleanup {
			return ErrInvalid
		}
	}
	if e := validateStagePolicy(*s); e != nil {
		return e
	}
	d, err := CanonicalDigest(*s)
	if err != nil {
		return err
	}
	if providedDigest != "" && providedDigest != d {
		return ErrDigestMismatch
	}
	s.Digest = d
	return nil
}
func validateStagePolicy(s ExecutionStage) error {
	for _, st := range s.Steps {
		switch st.Kind {
		case StepTargetInspect, StepHTTPProbe, StepTCPProbe:
			if s.Risk > RiskR1 || s.Gate != GateNone {
				return ErrInvalid
			}
		case StepComputeProvision:
			if s.Risk != RiskR2 || s.Gate != GateResourcePurchase {
				return ErrInvalid
			}
		case StepComputeDestroy:
			if s.Risk != RiskR4 || s.Gate != GateServiceDestroy {
				return ErrInvalid
			}
		case StepSecretProvision:
			if s.Risk != RiskR2 || s.Gate != GateSecretAccess || !containsGate(s.Effects, GateSecretAccess) {
				return ErrInvalid
			}
		case StepExternalAuth:
			if s.Risk != RiskR2 || s.Gate != GateExternalAuth || !containsGate(s.Effects, GateExternalAuth) {
				return ErrInvalid
			}
		default:
			if s.Risk == RiskR0 || s.Risk == RiskR1 {
				return ErrInvalid
			}
		}
	}
	needRoot, needSecret := false, false
	for _, st := range s.Steps {
		needRoot = needRoot || st.ScriptRun != nil && st.ScriptRun.Root || st.Executor != nil && st.Executor.Root
		needSecret = needSecret || len(st.SecretRefs) > 0 || st.ScriptRun != nil && len(st.ScriptRun.SecretRefs) > 0
	}
	if needRoot && (s.Risk != RiskR2 || s.Gate != GateRemotePrivilegedExecution || !containsGate(s.Effects, GateRemotePrivilegedExecution)) {
		return ErrInvalid
	}
	if needSecret && (s.Risk != RiskR2 || !containsGate(s.Effects, GateSecretAccess)) {
		return ErrInvalid
	}
	return nil
}

func normalizeRollbackPolicy(p *RollbackPolicy) error {
	if p == nil || p.Risk != RiskR4 || p.Gate != GateRollback {
		return ErrInvalid
	}
	provided := p.Digest
	p.Digest = ""
	d, err := CanonicalDigest(*p)
	if err != nil {
		return err
	}
	if provided != "" && provided != d {
		return ErrDigestMismatch
	}
	p.Digest = d
	return nil
}

func normalizeStep(s *ExecutionStep) error {
	providedDigest := s.Digest
	s.Digest = ""
	s.StepKey = strings.TrimSpace(s.StepKey)
	s.Kind = StepKind(strings.TrimSpace(string(s.Kind)))
	s.IdempotencyMarker = strings.TrimSpace(s.IdempotencyMarker)
	s.OutputPolicy = strings.TrimSpace(s.OutputPolicy)
	if !validKey(s.StepKey) || !validKind(s.Kind) || s.TimeoutSeconds == 0 || s.TimeoutSeconds > uint64(maxTimeout/time.Second) || !safeToken(s.IdempotencyMarker) || (s.OutputPolicy != "" && !validOutputPolicy(s.OutputPolicy)) || (s.Postcondition != nil && !validPostcondition(*s.Postcondition)) {
		return ErrInvalid
	}
	if hasDuplicateStrings(s.DependsOn) || duplicateNetworkGrants(s.NetworkGrants) {
		return ErrInvalid
	}
	s.DependsOn = normalizeSet(s.DependsOn)
	hasTarget := s.TargetID != "" || s.TargetRevision != 0 || s.TargetDigest != ""
	if hasTarget && (s.TargetID == "" || s.TargetRevision == 0 || !s.TargetDigest.Valid()) {
		return ErrInvalid
	}
	if s.TargetID != "" && !ValidateUUID(s.TargetID) {
		return ErrInvalid
	}
	if s.TargetRevision > 0 && !s.TargetDigest.Valid() {
		return ErrInvalid
	}
	if s.ObservationRef != nil {
		if err := normalizeTargetObservationRef(s.ObservationRef); err != nil {
			return err
		}
		if s.ObservationRef.TargetID != s.TargetID || s.ObservationRef.TargetRevision != s.TargetRevision {
			return ErrInvalid
		}
	}
	grants, err := normalizeNetworkGrants(s.NetworkGrants)
	if err != nil {
		return err
	}
	s.NetworkGrants = grants
	sort.Slice(s.SecretRefs, func(i, j int) bool { return s.SecretRefs[i].Ref < s.SecretRefs[j].Ref })
	sort.Slice(s.Permissions, func(i, j int) bool { return s.Permissions[i].Name < s.Permissions[j].Name })
	if duplicateCredentials(s.SecretRefs) || duplicatePermissions(s.Permissions) {
		return ErrInvalid
	}
	branches := []any{s.TargetInspect, s.ComputeProvision, s.ComputeDestroy, s.SourceFetch, s.ArtifactUpload, s.PackageEnsure, s.FilePut, s.ContainerApply, s.SystemdApply, s.ScriptRun, s.HTTPProbe, s.TCPProbe, s.ArtifactCollect, s.Cleanup, s.SecretProvision, s.ExternalAuth}
	count := 0
	for _, b := range branches {
		if present(b) {
			count++
		}
	}
	if count != 1 {
		return ErrInvalid
	}
	if e := validateStepPayload(s); e != nil {
		return e
	}
	for _, a := range s.ArtifactRefs {
		if e := validateArtifact(&a); e != nil {
			return e
		}
	}
	if s.Executor != nil {
		if e := validateExecutor(s.Executor); e != nil {
			return e
		}
		if s.Executor.Postcondition == nil || !equalPostcondition(s.Executor.Postcondition, s.Postcondition) {
			return ErrInvalid
		}
	}
	if duplicateArtifacts(s.ArtifactRefs) {
		return ErrInvalid
	}
	d, err := CanonicalDigest(*s)
	if err != nil {
		return err
	}
	if providedDigest != "" && providedDigest != d {
		return ErrDigestMismatch
	}
	s.Digest = d
	return nil
}

func validateStepPayload(s *ExecutionStep) error {
	switch s.Kind {
	case StepTargetInspect:
		if s.TargetInspect == nil || s.TargetInspect.ObservationID == "" || !ValidateUUID(s.TargetInspect.ObservationID) {
			return ErrInvalid
		}
	case StepComputeProvision:
		if s.ComputeProvision == nil || !validComputeProvision(*s.ComputeProvision) {
			return ErrInvalid
		}
	case StepComputeDestroy:
		if s.ComputeDestroy == nil || !ValidateUUID(s.ComputeDestroy.ResourceID) {
			return ErrInvalid
		}
	case StepSourceFetch:
		if s.SourceFetch == nil || validateSource(s.SourceFetch.Source) != nil || validateArtifact(&s.SourceFetch.Artifact) != nil || !validGrantList(s.NetworkGrants) {
			return ErrInvalid
		}
	case StepArtifactUpload:
		if s.ArtifactUpload == nil || validateArtifact(&s.ArtifactUpload.Artifact) != nil || !safePath(s.ArtifactUpload.Destination, false) {
			return ErrInvalid
		}
	case StepPackageEnsure:
		if s.PackageEnsure == nil || !safeToken(s.PackageEnsure.Name) || (s.PackageEnsure.Version != "" && !safeToken(s.PackageEnsure.Version)) || !safeToken(s.PackageEnsure.Manager) || !safeToken(s.PackageEnsure.PlatformProfile) {
			return ErrInvalid
		}
	case StepFilePut:
		if s.FilePut == nil || !safePath(s.FilePut.Path, true) || s.FilePut.Mode > 0777 || validateArtifact(&s.FilePut.Artifact) != nil {
			return ErrInvalid
		}
	case StepContainerApply:
		if s.ContainerApply == nil || !validPinnedImage(s.ContainerApply.Image) || !safeToken(s.ContainerApply.Name) || s.ContainerApply.HostAddress != "127.0.0.1" || s.ContainerApply.HostPort < 1 || s.ContainerApply.HostPort > 65535 || s.ContainerApply.ContainerPort < 1 || s.ContainerApply.ContainerPort > 65535 || s.ContainerApply.RestartPolicy != "unless-stopped" || !validGrantList(s.NetworkGrants) {
			return ErrInvalid
		}
	case StepSystemdApply:
		if s.SystemdApply == nil || !safeUnit(s.SystemdApply.Unit) || (s.SystemdApply.Artifact.ID != "" && validateArtifact(&s.SystemdApply.Artifact) != nil) {
			return ErrInvalid
		}
	case StepScriptRun:
		if s.ScriptRun == nil || s.ObservationRef == nil || !validOutputPolicy(s.OutputPolicy) || s.Postcondition == nil || validateScript(s.ScriptRun) != nil || len(s.NetworkGrants) > 0 && !validGrantList(s.NetworkGrants) || (s.OutputPolicy == OutputCapture || s.OutputPolicy == OutputArtifact) && (len(s.ScriptRun.Redaction.Patterns) == 0 || !safeText(s.ScriptRun.Redaction.Replace, 256)) || s.ScriptRun.IdempotencyMarker != s.IdempotencyMarker || s.ScriptRun.TimeoutSeconds != s.TimeoutSeconds || !equalNetworkGrants(s.ScriptRun.NetworkGrants, s.NetworkGrants) || !equalCredentials(s.ScriptRun.SecretRefs, s.SecretRefs) || !equalPostcondition(s.ScriptRun.Postcondition, s.Postcondition) {
			return ErrInvalid
		}
	case StepHTTPProbe:
		if s.HTTPProbe == nil || (s.HTTPProbe.Mode != "target_local" && s.HTTPProbe.Mode != "external") || !validProbeURL(s.HTTPProbe.URL, s.HTTPProbe.Mode == "target_local") || s.HTTPProbe.Mode == "external" && !validGrantList(s.NetworkGrants) || len(s.HTTPProbe.ExpectedStatus) > 0 && !validStatusCodes(s.HTTPProbe.ExpectedStatus) {
			return ErrInvalid
		}
	case StepTCPProbe:
		if s.TCPProbe == nil || (s.TCPProbe.Mode != "target_local" && s.TCPProbe.Mode != "external") || s.TCPProbe.Mode == "external" && !validGrantList(s.NetworkGrants) || s.TCPProbe.Port < 1 || s.TCPProbe.Port > 65535 || !validTCPAddress(s.TCPProbe.Address, s.TCPProbe.Mode == "target_local") {
			return ErrInvalid
		}
	case StepArtifactCollect:
		if s.ArtifactCollect == nil || !safeRefID(s.ArtifactCollect.OutputKey) || len(s.ArtifactCollect.Paths) == 0 {
			return ErrInvalid
		}
		for _, p := range s.ArtifactCollect.Paths {
			if !safePath(p, true) {
				return ErrInvalid
			}
		}
	case StepCleanup:
		if s.Cleanup == nil || !safeToken(s.Cleanup.Resource) {
			return ErrInvalid
		}
	case StepSecretProvision:
		if s.SecretProvision == nil || s.SecretProvision.Delivery != "target_secure_parameter" || len(s.SecretRefs) != 1 || validateCredential(s.SecretRefs[0]) != nil || len(s.NetworkGrants) != 0 || len(s.ArtifactRefs) != 0 || len(s.Permissions) != 0 || s.Executor != nil || s.Postcondition != nil || (s.OutputPolicy != "" && s.OutputPolicy != OutputDiscard) {
			return ErrInvalid
		}
	case StepExternalAuth:
		if s.ExternalAuth == nil || !validAIProvider(s.ExternalAuth.Provider) || s.ExternalAuth.Status != AIExternalAuthPending || len(s.SecretRefs) != 0 || len(s.NetworkGrants) != 0 || len(s.ArtifactRefs) != 0 || len(s.Permissions) != 0 || s.Executor != nil || s.Postcondition != nil || (s.OutputPolicy != "" && s.OutputPolicy != OutputDiscard) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (r TargetObservationRef) Normalize() (TargetObservationRef, error) {
	r.ObservationID = strings.TrimSpace(r.ObservationID)
	r.TargetID = strings.TrimSpace(r.TargetID)
	if !ValidateUUID(r.ObservationID) || !ValidateUUID(r.TargetID) || r.TargetRevision == 0 || !r.ObservationDigest.Valid() {
		return TargetObservationRef{}, ErrInvalid
	}
	return r, nil
}

func (r TargetObservationRef) Validate() error { _, err := r.Normalize(); return err }

func normalizeTargetObservationRef(r *TargetObservationRef) error {
	if r == nil {
		return ErrInvalid
	}
	n, err := r.Normalize()
	if err != nil {
		return err
	}
	*r = n
	return nil
}
func safeToken(s string) bool {
	return s != "" && len(s) <= 512 && !strings.ContainsAny(s, "\r\n\x00;|&$`")
}
func allowedInterpreter(s string) bool {
	switch s {
	case "/bin/sh", "/bin/bash", "/usr/bin/sh", "/usr/bin/bash":
		return true
	}
	return false
}
func validEnvName(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i, r := range s {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) || i == 0 && r >= '0' && r <= '9' {
			return false
		}
	}
	return true
}
func validPostcondition(p Postcondition) bool {
	switch p.Type {
	case "exit_code", "file_exists", "service_active", "http_status":
		return p.Value != "" && safeText(p.Value, 512)
	}
	return false
}
func validOutputPolicy(s string) bool {
	return s == OutputDiscard || s == OutputCapture || s == OutputArtifact
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func equalNetworkGrants(a, b []NetworkGrant) bool {
	aa, errA := normalizeNetworkGrants(a)
	bb, errB := normalizeNetworkGrants(b)
	if errA != nil || errB != nil || len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
func equalCredentials(a, b []CredentialRef) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]CredentialRef(nil), a...)
	bb := append([]CredentialRef(nil), b...)
	sort.Slice(aa, func(i, j int) bool { return aa[i].Ref < aa[j].Ref })
	sort.Slice(bb, func(i, j int) bool { return bb[i].Ref < bb[j].Ref })
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
func equalPostcondition(a, b *Postcondition) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func validPinnedImage(s string) bool {
	const p = "@sha256:"
	i := strings.LastIndex(s, p)
	if i < 1 || len(s[i+len(p):]) != 64 {
		return false
	}
	return Digest(s[i+len(p):]).Valid()
}
func safeText(s string, max int) bool {
	return s != "" && len(s) <= max && !strings.ContainsAny(s, "\r\n\x00")
}
func safePath(s string, absolute bool) bool {
	if s == "" || len(s) > 4096 || strings.ContainsAny(s, "\r\n\x00") || strings.Contains(s, "..") || path.Clean(s) != s {
		return false
	}
	return !absolute || strings.HasPrefix(s, "/")
}
func safeUnit(s string) bool { return safeToken(s) && !strings.ContainsAny(s, "/ \\") }
func validProbeURL(s string, local bool) bool {
	u, e := url.Parse(s)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || strings.ContainsAny(s, "\r\n\x00") {
		return false
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return false
	}
	if !local {
		if ip := net.ParseIP(h); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			return false
		}
	}
	return true
}
func validStatusCodes(v []int) bool {
	seen := map[int]bool{}
	for _, n := range v {
		if n < 100 || n > 599 || seen[n] {
			return false
		}
		seen[n] = true
	}
	return true
}
func validGrantList(v []NetworkGrant) bool {
	if len(v) == 0 {
		return false
	}
	for _, g := range v {
		if _, err := g.Normalize(); err != nil {
			return false
		}
	}
	return true
}

func (g NetworkGrant) Normalize() (NetworkGrant, error) {
	g.Scheme, g.Host, g.PathPrefix, g.Scope = strings.ToLower(strings.TrimSpace(g.Scheme)), strings.ToLower(strings.TrimSpace(g.Host)), strings.TrimSpace(g.PathPrefix), strings.TrimSpace(g.Scope)
	wildcardPublicHTTPS := g.Host == PublicHTTPSWildcardHost && g.Scheme == "https" && g.Port == 443 && g.Scope == "external" && g.PathPrefix == ""
	if (g.Scheme != "http" && g.Scheme != "https" && g.Scheme != "tcp") || (!wildcardPublicHTTPS && !validNetworkHost(g.Host)) || g.Port == 0 || (g.Scope != "external" && g.Scope != "target_local") || (g.PathPrefix != "" && !safePath(g.PathPrefix, true)) {
		return NetworkGrant{}, ErrInvalid
	}
	if g.Host == PublicHTTPSWildcardHost && !wildcardPublicHTTPS {
		return NetworkGrant{}, ErrInvalid
	}
	if g.Scheme == "tcp" && g.PathPrefix != "" {
		return NetworkGrant{}, ErrInvalid
	}
	if ip := net.ParseIP(g.Host); g.Scope == "external" && ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return NetworkGrant{}, ErrInvalid
	}
	provided := g.Digest
	g.Digest = ""
	d, err := CanonicalDigest(g)
	if err != nil {
		return NetworkGrant{}, err
	}
	if provided != "" && provided != d {
		return NetworkGrant{}, ErrDigestMismatch
	}
	g.Digest = d
	return g, nil
}
func validNetworkHost(host string) bool {
	if host == "" || len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}
func normalizeNetworkGrants(in []NetworkGrant) ([]NetworkGrant, error) {
	out := append([]NetworkGrant(nil), in...)
	for i := range out {
		n, err := out[i].Normalize()
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	if duplicateNetworkGrants(out) {
		return nil, ErrInvalid
	}
	return out, nil
}
func duplicateNetworkGrants(v []NetworkGrant) bool {
	seen := map[Digest]bool{}
	for _, g := range v {
		if seen[g.Digest] {
			return true
		}
		seen[g.Digest] = true
	}
	return false
}
func validTCPAddress(s string, local bool) bool {
	if s == "" || len(s) > 255 || strings.ContainsAny(s, "\r\n\x00 /\\") {
		return false
	}
	if ip := net.ParseIP(s); ip != nil {
		if !local && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			return false
		}
		return true
	}
	return strings.Contains(s, ".")
}

func present(v any) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	return !(rv.Kind() == reflect.Ptr && rv.IsNil())
}

func validateScript(s *ScriptRunStep) error {
	if e := validateArtifact(&s.Artifact); e != nil {
		return e
	}
	// AWS-RunShellScript executes as root.  There is no verified privilege-drop
	// primitive in the first transport, so a plan must state that truthfully.
	// Accepting Root=false here would make the approval preview materially lie.
	if !s.Root || !s.Artifact.Immutable || !allowedInterpreter(s.Interpreter) || len(s.Argv) == 0 || len(s.Argv) > 64 || !safePath(s.CWD, true) || s.TimeoutSeconds == 0 || s.TimeoutSeconds > uint64(maxTimeout/time.Second) || s.OutputLimit == 0 || s.OutputLimit > 16<<20 || len(s.AllowedExitCodes) == 0 || s.IdempotencyMarker == "" {
		return ErrInvalid
	}
	if strings.ContainsAny(s.Interpreter, "\r\n\x00") || strings.ContainsAny(s.CWD, "\r\n\x00") {
		return ErrInvalid
	}
	if len(s.Env) > 64 {
		return ErrInvalid
	}
	for k, v := range s.Env {
		if !validEnvName(k) || sensitiveEnvName(k) || sensitiveEnvValue(v) || len(v) > 4096 || strings.ContainsAny(v, "\r\n\x00") {
			return ErrInvalid
		}
	}
	for _, r := range s.SecretRefs {
		if e := validateCredential(r); e != nil {
			return e
		}
	}
	if duplicateCredentials(s.SecretRefs) {
		return ErrInvalid
	}
	if duplicateNetworkGrants(s.NetworkGrants) || hasDuplicateStrings(s.Redaction.Patterns) {
		return ErrInvalid
	}
	seenExitCodes := make(map[int]struct{}, len(s.AllowedExitCodes))
	for _, code := range s.AllowedExitCodes {
		if code < 0 || code > 255 {
			return ErrInvalid
		}
		if _, ok := seenExitCodes[code]; ok {
			return ErrInvalid
		}
		seenExitCodes[code] = struct{}{}
	}
	sort.Ints(s.AllowedExitCodes)
	if s.Postcondition != nil && !validPostcondition(*s.Postcondition) {
		return ErrInvalid
	}
	if len(s.Redaction.Patterns) > 0 {
		if s.Redaction.Replace == "" || len(s.Redaction.Patterns) > 64 {
			return ErrInvalid
		}
		for _, p := range s.Redaction.Patterns {
			if !safeText(p, 256) {
				return ErrInvalid
			}
		}
	}
	grants, err := normalizeNetworkGrants(s.NetworkGrants)
	if err != nil {
		return err
	}
	s.NetworkGrants = grants
	s.Redaction.Patterns = normalizeSet(s.Redaction.Patterns)
	return nil
}

func validateExecutor(s *ExecutorSpec) error {
	if s == nil || validateArtifact(&s.Artifact) != nil || !s.Artifact.Immutable || !allowedInterpreter(s.Interpreter) || !safePath(s.CWD, true) || len(s.Argv) > 64 || len(s.Env) > 64 || len(s.AllowedExitCodes) == 0 || s.OutputLimit == 0 || s.OutputLimit > 16<<20 || s.Postcondition == nil || !validPostcondition(*s.Postcondition) {
		return ErrInvalid
	}
	for _, arg := range s.Argv {
		if strings.ContainsAny(arg, "\r\n\x00") || len(arg) > 4096 {
			return ErrInvalid
		}
	}
	for k, v := range s.Env {
		if !validEnvName(k) || sensitiveEnvName(k) || sensitiveEnvValue(v) || len(v) > 4096 || strings.ContainsAny(v, "\r\n\x00") {
			return ErrInvalid
		}
	}
	if hasDuplicateStrings(s.Redaction.Patterns) {
		return ErrInvalid
	}
	for _, pattern := range s.Redaction.Patterns {
		if !safeText(pattern, 256) {
			return ErrInvalid
		}
	}
	seen := map[int]bool{}
	for _, code := range s.AllowedExitCodes {
		if code < 0 || code > 255 || seen[code] {
			return ErrInvalid
		}
		seen[code] = true
	}
	sort.Ints(s.AllowedExitCodes)
	s.Redaction.Patterns = normalizeSet(s.Redaction.Patterns)
	return nil
}

// Environment is a durable plan field, never a secret transport.  Keep this
// check intentionally conservative; callers must use CredentialRef instead.
func sensitiveEnvName(key string) bool {
	u := strings.ToUpper(strings.TrimSpace(key))
	return strings.Contains(u, "SECRET") || strings.Contains(u, "PASSWORD") || strings.Contains(u, "TOKEN") || strings.Contains(u, "PRIVATE_KEY") || strings.Contains(u, "AUTH") || strings.Contains(u, "COOKIE") || strings.Contains(u, "AWS_ACCESS_KEY") || strings.Contains(u, "AWS_SECRET") || strings.Contains(u, "AWS_SESSION")
}

func sensitiveEnvValue(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return false
	}
	lower := strings.ToLower(v)
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "basic ") || strings.Contains(lower, "authorization=") || strings.Contains(lower, "cookie=") || strings.Contains(lower, "aws_access_key") || strings.Contains(lower, "aws_secret_access_key") || strings.Contains(lower, "x-amz-security-token") {
		return true
	}
	if strings.HasPrefix(v, "AKIA") || strings.HasPrefix(v, "ASIA") {
		return len(v) >= 20
	}
	if len(v) < 24 || strings.IndexFunc(v, unicode.IsSpace) >= 0 || ValidateDigest(v) || ValidateUUID(v) {
		return false
	}
	counts := map[rune]int{}
	for _, r := range v {
		counts[r]++
	}
	length := float64(len([]rune(v)))
	entropy := 0.0
	for _, count := range counts {
		p := float64(count) / length
		entropy -= p * math.Log2(p)
	}
	return entropy >= 3.75
}

func validatePlanGraph(p *ExecutionPlan) error {
	targets := map[string]ExecutionTarget{}
	for _, t := range p.Targets {
		if e := t.Validate(); e != nil {
			return e
		}
		if _, ok := targets[t.ID]; ok {
			return ErrInvalid
		}
		if !ValidateUUID(t.ID) || t.Revision == 0 || !t.Digest.Valid() {
			return ErrInvalid
		}
		targets[t.ID] = t
	}
	stages := map[string]ExecutionStage{}
	for _, s := range p.Stages {
		if _, ok := stages[s.StageKey]; ok {
			return ErrInvalid
		}
		if _, ok := targets[s.TargetID]; !ok {
			return ErrInvalid
		}
		if t := targets[s.TargetID]; t.Revision != s.TargetRevision || t.Digest != s.TargetDigest {
			return ErrInvalid
		}
		stages[s.StageKey] = s
		if e := validateStepList(s.Steps, s, p); e != nil {
			return e
		}
		if e := validateStepList(s.RollbackSteps, s, p); e != nil {
			return e
		}
	}
	for _, s := range p.Stages {
		for _, d := range s.DependsOn {
			if _, ok := stages[d]; !ok {
				return ErrInvalid
			}
		}
	}
	if hasStageCycle(p.Stages) {
		return ErrCycle
	}
	return nil
}

func normalizeAIConfiguration(c *AIConfiguration) error {
	if c == nil {
		return ErrInvalid
	}
	c.Mode = AIAuthMode(strings.TrimSpace(string(c.Mode)))
	c.Provider = strings.TrimSpace(c.Provider)
	c.SecretRef = strings.TrimSpace(c.SecretRef)
	c.SecretPurpose = strings.TrimSpace(c.SecretPurpose)
	c.Status = strings.TrimSpace(c.Status)
	if !validAIProvider(c.Provider) {
		return ErrInvalid
	}
	switch c.Mode {
	case AIAuthModeAPIKey:
		if c.SecretPurpose != AISecretPurposeProviderAPIKey || validateCredential(c.CredentialRef()) != nil || c.Status != "" {
			return ErrInvalid
		}
	case AIAuthModeAuthGate:
		if c.Status != AIExternalAuthPending || c.SecretRef != "" || c.SecretRevision != 0 || c.SecretPurpose != "" || c.SecretBindingDigest != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (c AIConfiguration) Normalize() (AIConfiguration, error) {
	if err := normalizeAIConfiguration(&c); err != nil {
		return AIConfiguration{}, err
	}
	return c, nil
}

func (c AIConfiguration) Validate() error {
	_, err := c.Normalize()
	return err
}

func validAIProvider(provider string) bool {
	if provider == "" || len(provider) > 64 || strings.ToLower(provider) != provider {
		return false
	}
	for i, r := range provider {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || (i > 0 && (r == '.' || r == '_' || r == '-'))) {
			return false
		}
	}
	return true
}

func validateAIPlan(p ExecutionPlan) error {
	var authStage string
	authCount, aiProvisionCount := 0, 0
	for _, stage := range p.Stages {
		for _, step := range stage.Steps {
			switch step.Kind {
			case StepSecretProvision:
				if len(step.SecretRefs) == 1 && step.SecretRefs[0].Purpose == AISecretPurposeProviderAPIKey {
					aiProvisionCount++
				}
			case StepExternalAuth:
				authCount++
				authStage = stage.StageKey
			}
		}
	}
	if p.AIConfiguration == nil {
		if authCount != 0 || aiProvisionCount != 0 {
			return ErrInvalid
		}
		return nil
	}
	c := *p.AIConfiguration
	remoteDependent := false
	switch c.Mode {
	case AIAuthModeAPIKey:
		if authCount != 0 || aiProvisionCount != 1 {
			return ErrInvalid
		}
		want := c.CredentialRef()
		provisionStage := ""
		provisionCount := 0
		for _, stage := range p.Stages {
			for _, step := range stage.Steps {
				if step.Kind == StepSecretProvision && len(step.SecretRefs) == 1 && step.SecretRefs[0] == want {
					provisionCount++
					provisionStage = stage.StageKey
				}
			}
		}
		if provisionCount != 1 {
			return ErrInvalid
		}
		for _, stage := range p.Stages {
			if stage.StageKey == provisionStage {
				continue
			}
			for _, step := range stage.Steps {
				if credentialInStep(step, want) {
					if !containsString(stage.DependsOn, provisionStage) || (stage.Gate != GateRemoteExecution && stage.Gate != GateRemotePrivilegedExecution) {
						return ErrInvalid
					}
					remoteDependent = true
				}
			}
		}
	case AIAuthModeAuthGate:
		if authCount != 1 || aiProvisionCount != 0 {
			return ErrInvalid
		}
		for _, stage := range p.Stages {
			if stage.StageKey == authStage {
				for _, step := range stage.Steps {
					if step.Kind == StepExternalAuth && (step.ExternalAuth.Provider != c.Provider || step.ExternalAuth.Status != c.Status) {
						return ErrInvalid
					}
				}
				continue
			}
			if containsString(stage.DependsOn, authStage) && (stage.Gate == GateRemoteExecution || stage.Gate == GateRemotePrivilegedExecution) {
				remoteDependent = true
			}
		}
	default:
		return ErrInvalid
	}
	if !remoteDependent {
		return ErrInvalid
	}
	return nil
}

// validateSecretProvisionDependencies makes secret authorization a distinct
// stage boundary. A remote mutation may reference only an exact, immutable
// credential revision that was provisioned by one directly-dependent
// secret_access stage for the same target. This prevents a remote execution
// confirmation from implicitly authorizing secret access as a side effect.
func validateSecretProvisionDependencies(p ExecutionPlan) error {
	type provision struct {
		stageKey       string
		targetID       string
		targetRevision uint64
		targetDigest   Digest
		used           bool
	}
	byCredential := make(map[CredentialRef][]*provision)
	for stageIndex := range p.Stages {
		stage := &p.Stages[stageIndex]
		for _, step := range stage.RollbackSteps {
			if len(step.SecretRefs) != 0 {
				return ErrInvalid
			}
		}
		for _, step := range stage.Steps {
			if step.Kind != StepSecretProvision {
				continue
			}
			if len(step.SecretRefs) != 1 {
				return ErrInvalid
			}
			ref := step.SecretRefs[0]
			entry := &provision{stageKey: stage.StageKey, targetID: stage.TargetID, targetRevision: stage.TargetRevision, targetDigest: stage.TargetDigest}
			byCredential[ref] = append(byCredential[ref], entry)
		}
	}
	for _, entries := range byCredential {
		if len(entries) != 1 {
			return ErrInvalid
		}
	}
	for _, stage := range p.Stages {
		for _, step := range stage.Steps {
			if step.Kind == StepSecretProvision {
				continue
			}
			for _, ref := range step.SecretRefs {
				entries := byCredential[ref]
				if len(entries) != 1 {
					return ErrInvalid
				}
				entry := entries[0]
				if entry.stageKey == stage.StageKey || !containsString(stage.DependsOn, entry.stageKey) ||
					entry.targetID != stage.TargetID || entry.targetRevision != stage.TargetRevision || entry.targetDigest != stage.TargetDigest {
					return ErrInvalid
				}
				entry.used = true
			}
		}
	}
	for _, entries := range byCredential {
		if !entries[0].used {
			return ErrInvalid
		}
	}
	return nil
}

func credentialInStep(step ExecutionStep, want CredentialRef) bool {
	for _, ref := range step.SecretRefs {
		if ref == want {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func validateStepList(steps []ExecutionStep, stage ExecutionStage, p *ExecutionPlan) error {
	seen := map[string]bool{}
	for _, st := range steps {
		if seen[st.StepKey] {
			return ErrInvalid
		}
		seen[st.StepKey] = true
		for _, dep := range st.DependsOn {
			if dep == st.StepKey || !containsStepKey(steps, dep) {
				return ErrInvalid
			}
		}
		if st.TargetID != "" && (st.TargetID != stage.TargetID || st.TargetRevision != stage.TargetRevision || st.TargetDigest != stage.TargetDigest) {
			return ErrInvalid
		}
		if e := validateTargetNetworkClosure(st.NetworkGrants, targetsNetwork(stage.TargetID, p)); e != nil {
			return e
		}
		if e := validateTypedOutboundClosure(st); e != nil {
			return e
		}
		for _, a := range append(append([]ArtifactRef(nil), st.ArtifactRefs...), stepPayloadArtifacts(st)...) {
			if !artifactInPlan(a, p) {
				return ErrInvalid
			}
		}
		if st.ArtifactCollect != nil && !outputInPlan(st.ArtifactCollect.OutputKey, p) {
			return ErrInvalid
		}
	}
	if hasStepCycle(steps) {
		return ErrCycle
	}
	return nil
}

func validateTypedOutboundClosure(step ExecutionStep) error {
	var scheme, host, pathname string
	var port uint16
	var scope string
	switch step.Kind {
	case StepSourceFetch:
		if step.SourceFetch == nil {
			return ErrInvalid
		}
		return validateOutboundURL(step.SourceFetch.Source.Location, "external", step.NetworkGrants)
	case StepHTTPProbe:
		if step.HTTPProbe == nil {
			return ErrInvalid
		}
		return validateOutboundURL(step.HTTPProbe.URL, step.HTTPProbe.Mode, step.NetworkGrants)
	case StepTCPProbe:
		if step.TCPProbe == nil {
			return ErrInvalid
		}
		scheme, host, port, scope = "tcp", step.TCPProbe.Address, uint16(step.TCPProbe.Port), step.TCPProbe.Mode
	case StepContainerApply:
		if step.ContainerApply == nil {
			return ErrInvalid
		}
		image := strings.Split(step.ContainerApply.Image, "@sha256:")[0]
		return validateOutboundURL("https://"+image, "external", step.NetworkGrants)
	default:
		return nil
	}
	matched := 0
	for _, grant := range step.NetworkGrants {
		if grant.Scheme == scheme && grant.Host == strings.ToLower(host) && grant.Port == port && grant.Scope == scope && grant.PathPrefix == pathname {
			matched++
		}
	}
	if matched != 1 || len(step.NetworkGrants) != 1 {
		return ErrInvalid
	}
	return nil
}

func validateOutboundURL(raw, mode string, grants []NetworkGrant) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ErrInvalid
	}
	port := uint16(443)
	if u.Scheme == "http" {
		port = 80
	}
	if u.Port() != "" {
		parsed, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil {
			return ErrInvalid
		}
		port = uint16(parsed)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	scope := "external"
	if mode == "target_local" {
		scope = "target_local"
	}
	matched := 0
	for _, grant := range grants {
		exact := grant.Scheme == u.Scheme && grant.Host == strings.ToLower(u.Hostname()) && grant.Port == port && grant.Scope == scope && grant.PathPrefix == path
		publicHTTPS := grant.Scheme == "https" && grant.Host == PublicHTTPSWildcardHost && grant.Port == 443 && grant.Scope == "external" && grant.PathPrefix == "" && u.Scheme == "https" && port == 443 && scope == "external"
		if exact || publicHTTPS {
			matched++
		}
	}
	if matched != 1 || len(grants) != 1 {
		return ErrInvalid
	}
	return nil
}

func targetsNetwork(targetID string, p *ExecutionPlan) NetworkPolicy {
	for _, t := range p.Targets {
		if t.ID == targetID {
			return t.Network
		}
	}
	return NetworkPolicy{}
}

// validateTargetNetworkClosure makes the step's digest-bound grants a subset
// of the target policy; a target denial always wins. The observed AWS HTTPS
// mode is intentionally broad and therefore accepts only an equally honest
// public-HTTPS wildcard grant, never a hostname/path-specific claim.
func validateTargetNetworkClosure(grants []NetworkGrant, policy NetworkPolicy) error {
	for _, grant := range grants {
		if containsNetworkGrant(policy.Deny, grant) {
			return ErrInvalid
		}
		if containsNetworkGrant(policy.Allow, grant) {
			continue
		}
		if policy.Mode == NetworkPolicyModeObservedHTTPSEgress && (grant.Scope == "target_local" || grant.Scope == "external" && grant.Scheme == "https" && grant.Host == PublicHTTPSWildcardHost && grant.Port == 443 && grant.PathPrefix == "") {
			continue
		}
		return ErrInvalid
	}
	return nil
}

func containsNetworkGrant(grants []NetworkGrant, want NetworkGrant) bool {
	for _, grant := range grants {
		if grant == want {
			return true
		}
	}
	return false
}

func validateRequiredOutputProducers(p ExecutionPlan) error {
	producers := map[string]int{}
	for _, stage := range p.Stages {
		for _, step := range stage.Steps { // rollback declarations are not forward outputs.
			if step.ArtifactCollect != nil {
				producers[step.ArtifactCollect.OutputKey]++
			}
		}
	}
	for _, output := range p.Outputs {
		if producers[output.Key] != 1 {
			return ErrInvalid
		}
	}
	return nil
}
func stepPayloadArtifacts(s ExecutionStep) []ArtifactRef {
	out := []ArtifactRef{}
	if s.Executor != nil {
		out = append(out, s.Executor.Artifact)
	}
	switch s.Kind {
	case StepSourceFetch:
		if s.SourceFetch != nil {
			out = append(out, s.SourceFetch.Artifact)
		}
	case StepArtifactUpload:
		if s.ArtifactUpload != nil {
			out = append(out, s.ArtifactUpload.Artifact)
		}
	case StepFilePut:
		if s.FilePut != nil {
			out = append(out, s.FilePut.Artifact)
		}
	case StepSystemdApply:
		if s.SystemdApply != nil && s.SystemdApply.Artifact.ID != "" {
			out = append(out, s.SystemdApply.Artifact)
		}
	case StepScriptRun:
		if s.ScriptRun != nil {
			out = append(out, s.ScriptRun.Artifact)
		}
	case StepArtifactCollect:
	}
	return out
}
func outputInPlan(k string, p *ExecutionPlan) bool {
	for _, o := range p.Outputs {
		if o.Key == k {
			return true
		}
	}
	return false
}
func containsStepKey(steps []ExecutionStep, key string) bool {
	for _, s := range steps {
		if s.StepKey == key {
			return true
		}
	}
	return false
}

func artifactInPlan(a ArtifactRef, p *ExecutionPlan) bool {
	for _, o := range p.Artifacts {
		if o == a {
			return true
		}
	}
	return false
}

func hasStageCycle(v []ExecutionStage) bool {
	state := map[string]uint8{}
	by := map[string]ExecutionStage{}
	for _, s := range v {
		by[s.StageKey] = s
	}
	var f func(string) bool
	f = func(k string) bool {
		if state[k] == 1 {
			return true
		}
		if state[k] == 2 {
			return false
		}
		state[k] = 1
		for _, d := range by[k].DependsOn {
			if f(d) {
				return true
			}
		}
		state[k] = 2
		return false
	}
	for _, s := range v {
		if f(s.StageKey) {
			return true
		}
	}
	return false
}
func hasStepCycle(v []ExecutionStep) bool {
	state := map[string]uint8{}
	by := map[string]ExecutionStep{}
	for _, s := range v {
		by[s.StepKey] = s
	}
	var f func(string) bool
	f = func(k string) bool {
		if state[k] == 1 {
			return true
		}
		if state[k] == 2 {
			return false
		}
		state[k] = 1
		for _, d := range by[k].DependsOn {
			if f(d) {
				return true
			}
		}
		state[k] = 2
		return false
	}
	for _, s := range v {
		if f(s.StepKey) {
			return true
		}
	}
	return false
}

func (b ConfirmationBindingSnapshot) Normalize() (ConfirmationBindingSnapshot, error) {
	if e := validateBinding(&b); e != nil {
		return ConfirmationBindingSnapshot{}, e
	}
	provided := b.Digest
	b.Digest = ""
	d, e := CanonicalDigest(b)
	if e != nil {
		return ConfirmationBindingSnapshot{}, e
	}
	if provided != "" && provided != d {
		return ConfirmationBindingSnapshot{}, ErrDigestMismatch
	}
	b.Digest = d
	return b, nil
}
func (b ConfirmationBindingSnapshot) Validate() error { _, e := b.Normalize(); return e }
func (b ConfirmationBindingSnapshot) ValidateAt(now time.Time) error {
	if e := validateBinding(&b); e != nil {
		return e
	}
	if !now.IsZero() && !b.ExpiresAt.After(now) {
		return ErrExpired
	}
	return nil
}
func (b ConfirmationBindingSnapshot) CanonicalDigest() (Digest, error) {
	n, e := b.Normalize()
	if e != nil {
		return "", e
	}
	return n.Digest, nil
}
func validateBinding(b *ConfirmationBindingSnapshot) error {
	if !validOwnerID(b.OwnerID) || !ValidateUUID(b.PlanID) || b.PlanRevision == 0 || !b.PlanDigest.Valid() || (b.DeploymentID != "" && !ValidateUUID(b.DeploymentID)) || !ValidateUUID(b.RunID) || b.RunRevision == 0 || !ValidateUUID(b.StageID) || b.StageRevision == 0 || !safeToken(b.StageIdempotencyKey) || !b.StageDigest.Valid() || !ValidateUUID(b.TargetID) || b.TargetRevision == 0 || !b.TargetDigest.Valid() || b.ExpiresAt.Location() != time.UTC {
		return ErrInvalid
	}
	for _, d := range []Digest{b.ExecutionDigest, b.ArtifactSetDigest, b.NetworkDigest, b.SecretGrantDigest, b.PolicyDigest, b.CostQuoteDigest, b.RollbackDigest, b.PreviewDigest} {
		if !d.Valid() {
			return ErrInvalid
		}
	}
	if !validRisk(b.Risk) || !validGate(b.Gate) || b.Risk == RiskR0 || b.Risk == RiskR1 || !validRiskGate(b.Risk, b.Gate) {
		return ErrInvalid
	}
	return nil
}

func validateArtifact(a *ArtifactRef) error {
	if !ValidateUUID(a.ID) || !a.Digest.Valid() || a.Size < 0 || (a.URI != "" && !safeText(a.URI, 2048)) || (a.MediaType != "" && !safeToken(a.MediaType)) {
		return ErrInvalid
	}
	return nil
}
func validateSource(s SourceRef) error {
	if !s.Immutable {
		return ErrInvalid
	}
	switch s.Kind {
	case "git_https":
		u, err := url.Parse(s.Location)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
			u.Fragment != "" || (!hexPin(s.Commit, 40) && !hexPin(s.Commit, 64)) ||
			s.ArtifactID != "" || s.ArtifactDigest != "" {
			return ErrInvalid
		}
	case "uploaded_artifact":
		if s.Location != "" || s.Commit != "" || !ValidateUUID(s.ArtifactID) ||
			!s.ArtifactDigest.Valid() {
			return ErrInvalid
		}
	case "oci_image":
		if !safeText(s.Location, 2048) || !validPinnedImage(s.Location) ||
			s.Commit != "" || s.ArtifactID != "" || s.ArtifactDigest != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
func hexPin(s string, n int) bool {
	if len(s) != n || strings.ToLower(s) != s {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}
func (a ProjectAnalysis) Validate() error {
	if !ValidateUUID(a.AnalysisID) || !ValidateUUID(a.ProjectID) || validateSource(a.Source) != nil || a.Revision == 0 || (a.Digest != "" && !a.Digest.Valid()) || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() || a.CreatedAt.Location() != time.UTC || a.UpdatedAt.Location() != time.UTC || a.UpdatedAt.Before(a.CreatedAt) {
		return ErrInvalid
	}
	if e := validateResource(a.Build); e != nil {
		return e
	}
	if e := validateResource(a.Runtime); e != nil {
		return e
	}
	if len(a.Ports) > 128 {
		return ErrInvalid
	}
	for _, p := range a.Ports {
		if p < 1 || p > 65535 {
			return ErrInvalid
		}
	}
	if hasDuplicateInts(a.Ports) {
		return ErrInvalid
	}
	for _, v := range [][]string{a.DetectedStacks, a.Dependencies, a.EnvironmentNames, a.SecretPurposes, a.SecretRefs, a.Volumes, a.Migrations, a.Probes, a.Assumptions, a.BlockingUncertainties} {
		if len(v) > 128 {
			return ErrInvalid
		}
		for _, s := range v {
			if len(s) > 512 || strings.ContainsAny(s, "\r\n\x00") {
				return ErrInvalid
			}
		}
	}
	if a.Source.ArtifactDigest != "" && !a.Source.ArtifactDigest.Valid() {
		return ErrInvalid
	}
	for _, r := range a.SecretRefs {
		if r == "" || !ValidateUUID(r) {
			return ErrInvalid
		}
	}
	return nil
}

func (a ProjectAnalysis) Normalize() (ProjectAnalysis, error) {
	for _, v := range [][]string{a.DetectedStacks, a.Dependencies, a.EnvironmentNames, a.SecretPurposes, a.SecretRefs, a.Volumes, a.Migrations, a.Probes, a.Assumptions, a.BlockingUncertainties} {
		if hasDuplicateStrings(v) {
			return ProjectAnalysis{}, ErrInvalid
		}
	}
	a.DetectedStacks = normalizeSet(a.DetectedStacks)
	a.Dependencies = normalizeSet(a.Dependencies)
	a.EnvironmentNames = normalizeSet(a.EnvironmentNames)
	a.SecretPurposes = normalizeSet(a.SecretPurposes)
	a.SecretRefs = normalizeSet(a.SecretRefs)
	a.Volumes = normalizeSet(a.Volumes)
	a.Migrations = normalizeSet(a.Migrations)
	a.Probes = normalizeSet(a.Probes)
	a.Assumptions = normalizeSet(a.Assumptions)
	a.BlockingUncertainties = normalizeSet(a.BlockingUncertainties)
	sort.Ints(a.Ports)
	provided := a.Digest
	a.Digest = ""
	if e := a.Validate(); e != nil {
		return ProjectAnalysis{}, e
	}
	d, e := CanonicalDigest(a)
	if e != nil {
		return ProjectAnalysis{}, e
	}
	if provided != "" && provided != d {
		return ProjectAnalysis{}, ErrDigestMismatch
	}
	a.Digest = d
	return a, nil
}
func validateResource(r ResourceRequirement) error {
	for _, s := range []string{r.CPU, r.Memory, r.Disk, r.GPU} {
		if s != "" && !safeToken(s) {
			return ErrInvalid
		}
	}
	if r.Architecture != "" && r.Architecture != "x86_64" && r.Architecture != "arm64" {
		return ErrInvalid
	}
	return nil
}

func validComputeReservation(region string, r ComputeReservation) bool {
	return safeRefID(r.InfrastructureProfileID) && r.AMIParameter == AWSAL2023X8664AMIParameter &&
		safeToken(r.InstanceType) && r.VolumeGiB >= 8 && r.VolumeGiB <= 16384 &&
		ValidateAvailabilityZone(region, r.AvailabilityZone) &&
		r.Architecture == "x86_64" && r.ManagementTransport == "aws_ssm" &&
		r.PublicIP && !r.PublicInbound && r.CostQuote.Validate() == nil
}

func validComputeProvision(s ComputeProvisionStep) bool {
	return safeRefID(s.InfrastructureProfileID) && s.AMIParameter == AWSAL2023X8664AMIParameter &&
		safeToken(s.InstanceType) && s.VolumeGiB >= 8 && s.VolumeGiB <= 16384 &&
		safeToken(s.Region) && ValidateAvailabilityZone(s.Region, s.AvailabilityZone) && s.Architecture == "x86_64" && s.ManagementTransport == "aws_ssm" &&
		s.PublicIP && !s.PublicInbound
}

// ValidateAvailabilityZone accepts only standard AWS availability-zone names
// belonging to the supplied region. Local/Wavelength zones and arbitrary
// caller-controlled strings are intentionally rejected by the execution
// contract.
func ValidateAvailabilityZone(region, availabilityZone string) bool {
	region = strings.TrimSpace(region)
	availabilityZone = strings.TrimSpace(availabilityZone)
	if region == "" || availabilityZone == "" || len(availabilityZone) != len(region)+1 || !strings.HasPrefix(availabilityZone, region) {
		return false
	}
	suffix := availabilityZone[len(availabilityZone)-1]
	return suffix >= 'a' && suffix <= 'z'
}
func hasDuplicateInts(v []int) bool {
	seen := map[int]bool{}
	for _, n := range v {
		if seen[n] {
			return true
		}
		seen[n] = true
	}
	return false
}
func (a ProjectAnalysis) CanonicalDigest() (Digest, error) {
	n, e := a.Normalize()
	if e != nil {
		return "", e
	}
	return n.Digest, nil
}
func (t ExecutionTarget) Validate() error {
	if !ValidateUUID(t.ID) || !safeToken(t.Provider) || !safeToken(t.Kind) || (t.InfrastructureProfileID != "" && !safeRefID(t.InfrastructureProfileID)) || t.Revision == 0 || (t.Digest != "" && !t.Digest.Valid()) || (t.AccountID != "" && !safeText(t.AccountID, 256)) || (t.Region != "" && !safeToken(t.Region)) || (t.Architecture != "" && !safeToken(t.Architecture)) {
		return ErrInvalid
	}
	if (t.Provider == "aws" || t.Provider == "aws_ssm") && (t.Kind == TargetKindAWSEC2Instance || t.Kind == TargetKindAWSComputeReservation) && (t.AccountID == "" || t.Region == "") {
		return ErrInvalid
	}
	if t.Kind == TargetKindAWSComputeReservation {
		if t.Provider != "aws" || t.ComputeReservation == nil || !validComputeReservation(t.Region, *t.ComputeReservation) || t.InfrastructureProfileID != t.ComputeReservation.InfrastructureProfileID || t.Architecture != t.ComputeReservation.Architecture || t.Network.Mode != "restricted" || len(t.Network.Allow) != 0 || len(t.Network.Deny) != 0 {
			return ErrInvalid
		}
	} else if t.ComputeReservation != nil {
		return ErrInvalid
	}
	for _, c := range t.CredentialRefs {
		if e := validateCredential(c); e != nil {
			return e
		}
	}
	if duplicateCredentials(t.CredentialRefs) {
		return ErrInvalid
	}
	if e := validateNetworkPolicy(t.Network); e != nil {
		return e
	}
	if hasDuplicateStrings(t.Capabilities) {
		return ErrInvalid
	}
	for _, c := range t.Capabilities {
		if !safeToken(c) {
			return ErrInvalid
		}
	}
	return nil
}
func (t ExecutionTarget) Normalize() (ExecutionTarget, error) {
	t.Capabilities = normalizeSet(t.Capabilities)
	allow, err := normalizeNetworkGrants(t.Network.Allow)
	if err != nil {
		return ExecutionTarget{}, err
	}
	t.Network.Allow = allow
	deny, err := normalizeNetworkGrants(t.Network.Deny)
	if err != nil {
		return ExecutionTarget{}, err
	}
	t.Network.Deny = deny
	provided := t.Digest
	t.Digest = ""
	if e := t.Validate(); e != nil {
		return ExecutionTarget{}, e
	}
	d, e := CanonicalDigest(t)
	if e != nil {
		return ExecutionTarget{}, e
	}
	if provided != "" && provided != d {
		return ExecutionTarget{}, ErrDigestMismatch
	}
	t.Digest = d
	return t, nil
}
func (c CostQuote) Validate() error {
	if c.Amount == "" || !safeText(c.Amount, 64) || c.Currency == "" || !safeToken(c.Currency) || c.ExpiresAt.IsZero() || c.ExpiresAt.Location() != time.UTC {
		return ErrInvalid
	}
	return nil
}
func (p PlacementRecommendation) Validate() error {
	if p.Kind != "local_control_plane" && p.Kind != "existing_target" && p.Kind != "new_ephemeral_target" && p.Kind != "new_persistent_target" && p.Kind != "managed_service" {
		return ErrInvalid
	}
	for _, o := range []PlacementOption{p.Minimum, p.Recommended, p.HighPerformance} {
		if e := o.CostQuote.Validate(); e != nil {
			return e
		}
		if !safeToken(o.Region) || !safeToken(o.Spec) || !safeToken(o.Disk) || !safeToken(o.Network) {
			return ErrInvalid
		}
	}
	return nil
}
func (p PlacementRecommendation) Normalize() (PlacementRecommendation, error) {
	p.Kind = strings.TrimSpace(p.Kind)
	if e := p.Validate(); e != nil {
		return PlacementRecommendation{}, e
	}
	return p, nil
}
func validateNetworkPolicy(n NetworkPolicy) error {
	if n.Mode != "" && n.Mode != "none" && n.Mode != "target_local" && n.Mode != "external" && n.Mode != "restricted" && n.Mode != NetworkPolicyModeObservedHTTPSEgress {
		return ErrInvalid
	}
	if duplicateNetworkGrants(n.Allow) || duplicateNetworkGrants(n.Deny) {
		return ErrInvalid
	}
	for _, v := range append(append([]NetworkGrant{}, n.Allow...), n.Deny...) {
		if _, err := v.Normalize(); err != nil {
			return err
		}
	}
	return nil
}
func validateCredential(c CredentialRef) error {
	if !ValidateUUID(c.Ref) || !safeToken(c.Purpose) || c.Revision == 0 || !c.BindingDigest.Valid() {
		return ErrInvalid
	}
	return nil
}
func duplicateCredentials(v []CredentialRef) bool {
	seen := map[string]bool{}
	for _, c := range v {
		k := c.Ref + "\x00" + fmtUint(c.Revision)
		if seen[k] {
			return true
		}
		seen[k] = true
	}
	return false
}
func duplicatePermissions(v []PermissionGrant) bool {
	seen := map[string]bool{}
	for _, p := range v {
		if p.Name == "" || !safeToken(p.Name) || p.Revision == 0 || !p.BindingDigest.Valid() {
			return true
		}
		if seen[p.Name] {
			return true
		}
		seen[p.Name] = true
	}
	return false
}
func fmtUint(v uint64) string { return strconv.FormatUint(v, 10) }
func validateRefs(sk []SkillRef, rk []RecipeRef) error {
	seenS := map[string]bool{}
	seenR := map[string]bool{}
	for _, r := range sk {
		if !safeRefID(r.ID) || !canonicalSemver(r.Version) || !r.Digest.Valid() {
			return ErrInvalid
		}
		k := r.ID
		if seenS[k] {
			return ErrInvalid
		}
		seenS[k] = true
	}
	for _, r := range rk {
		if !safeRefID(r.ID) || !canonicalSemver(r.Version) || !r.Digest.Valid() {
			return ErrInvalid
		}
		k := r.ID
		if seenR[k] {
			return ErrInvalid
		}
		seenR[k] = true
	}
	return nil
}
func safeRefID(s string) bool                    { return safeText(s, 128) && !strings.ContainsAny(s, "/ ") }
func refKey(id, version string, d Digest) string { return id + "\x00" + version + "\x00" + string(d) }
func canonicalSemver(s string) bool              { v, e := semver.NewVersion(s); return e == nil && v.String() == s }
func duplicateArtifacts(v []ArtifactRef) bool {
	seen := map[string]bool{}
	for _, a := range v {
		k := a.ID + "\x00" + string(a.Digest)
		if seen[k] {
			return true
		}
		seen[k] = true
	}
	return false
}
func duplicateOutputs(v []OutputDeclaration) bool {
	seen := map[string]bool{}
	for _, o := range v {
		if seen[o.Key] {
			return true
		}
		seen[o.Key] = true
	}
	return false
}
func validateOutput(o OutputDeclaration) error {
	mediaType, _, err := mime.ParseMediaType(o.MediaType)
	if !safeRefID(o.Key) || err != nil || mediaType != o.MediaType || !strings.Contains(mediaType, "/") || o.MaxSize == 0 || o.MaxSize > 16<<20 {
		return ErrInvalid
	}
	return nil
}
func validPurpose(p PlanPurpose) bool { return p == PurposeJob || p == PurposeService }
func validPlanStatus(s PlanStatus) bool {
	return s == PlanDraft || s == PlanReady || s == PlanExpired || s == PlanSuperseded
}
func validRisk(r Risk) bool {
	switch r {
	case RiskR0, RiskR1, RiskR2, RiskR3, RiskR4:
		return true
	}
	return false
}
func validGate(g Gate) bool {
	switch g {
	case GateNone, GateResourcePurchase, GateSecretAccess, GateExternalAuth, GateRemoteExecution, GateRemotePrivilegedExecution, GatePublicNetworkExposure, GateDNSChange, GateTLSCertificateIssue, GateDataMigration, GateProductionCutover, GateRepositoryWrite, GateServiceDestroy, GateRollback:
		return true
	}
	return false
}
func validRiskGate(r Risk, g Gate) bool {
	if !validRisk(r) || !validGate(g) {
		return false
	}
	switch r {
	case RiskR0, RiskR1:
		return g == GateNone
	case RiskR2:
		return g == GateResourcePurchase || g == GateSecretAccess || g == GateExternalAuth || g == GateRemoteExecution || g == GateRemotePrivilegedExecution || g == GateRepositoryWrite
	case RiskR3:
		return g == GatePublicNetworkExposure || g == GateDNSChange || g == GateTLSCertificateIssue || g == GateDataMigration || g == GateProductionCutover
	case RiskR4:
		return g == GateServiceDestroy || g == GateRollback
	}
	return false
}
func validRiskGateForStage(r Risk, g Gate) bool { return validRiskGate(r, g) }
func validKind(k StepKind) bool {
	switch k {
	case StepTargetInspect, StepComputeProvision, StepComputeDestroy, StepSourceFetch, StepArtifactUpload, StepPackageEnsure, StepFilePut, StepContainerApply, StepSystemdApply, StepScriptRun, StepHTTPProbe, StepTCPProbe, StepArtifactCollect, StepCleanup, StepSecretProvision, StepExternalAuth:
		return true
	}
	return false
}
func validKey(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.') {
			return false
		}
		if i == 0 && (r == '-' || r == '.') {
			return false
		}
	}
	return true
}
func normalizeSet(v []string) []string {
	r := make([]string, 0, len(v))
	for _, s := range v {
		r = append(r, strings.TrimSpace(s))
	}
	sort.Strings(r)
	return r
}
func normalizeGates(v []Gate) []Gate {
	r := append([]Gate(nil), v...)
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	return r
}
func hasDuplicateGates(v []Gate) bool {
	seen := map[Gate]bool{}
	for _, gate := range v {
		if !validGate(gate) || seen[gate] {
			return true
		}
		seen[gate] = true
	}
	return false
}
func containsGate(v []Gate, want Gate) bool {
	for _, gate := range v {
		if gate == want {
			return true
		}
	}
	return false
}
func hasDuplicateStrings(v []string) bool {
	seen := map[string]bool{}
	for _, s := range v {
		if seen[s] {
			return true
		}
		seen[s] = true
	}
	return false
}
func validOwnerID(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) == 0 || len(s) > 255 || strings.ContainsAny(s, "\r\n\x00") {
		return false
	}
	return strings.HasPrefix(s, "@") && strings.Contains(s, ":")
}

// Status validators are intentionally separate; a stage status must never be
// accepted where a run status is expected.
func ValidatePlanStatus(s PlanStatus) bool { return validPlanStatus(s) }
func ValidateRunStatus(s RunStatus) bool {
	switch s {
	case RunPending, RunWaitingUser, RunQueued, RunRunning, RunSucceeded, RunFailed, RunUncertain, RunCanceled, RunRejected, RunExpired:
		return true
	}
	return false
}
func isTerminalRun(s RunStatus) bool {
	switch s {
	case RunSucceeded, RunFailed, RunUncertain, RunCanceled, RunRejected, RunExpired:
		return true
	}
	return false
}
func requiresStartedRun(s RunStatus) bool {
	return s == RunRunning || s == RunSucceeded || s == RunFailed || s == RunUncertain
}
func isTerminalStage(s StageStatus) bool {
	switch s {
	case StageSucceeded, StageFailed, StageUncertain, StageSkipped, StageCanceled, StageRejected, StageExpired:
		return true
	}
	return false
}
func validRunOperation(o RunOperation) bool {
	switch o {
	case RunOperationExecute, RunOperationDeploy, RunOperationUpgrade, RunOperationRepair, RunOperationDestroy, RunOperationRollback:
		return true
	}
	return false
}
func validTriggerKind(k TriggerKind) bool {
	return k == "" || k == TriggerManual || k == TriggerSchedule || k == TriggerRetry || k == TriggerRollback
}
func ValidateStageStatus(s StageStatus) bool {
	switch s {
	case StageBlocked, StageWaitingUser, StageQueued, StageRunning, StageSucceeded, StageFailed, StageUncertain, StageSkipped, StageCanceled, StageRejected, StageExpired:
		return true
	}
	return false
}
func (r ExecutionRun) Validate() error {
	if !ValidateUUID(r.RunID) || !validOwnerID(r.OwnerID) || !ValidateUUID(r.PlanID) || !ValidateUUID(r.ProjectID) || !validPurpose(r.Purpose) || r.PlanRevision == 0 || r.Revision == 0 || !r.PlanDigest.Valid() || !r.RunDigest.Valid() || !ValidateRunStatus(r.Status) || !validRunOperation(r.Operation) || !validTriggerKind(r.TriggerKind) || (r.RollbackOfRunID != "" && !ValidateUUID(r.RollbackOfRunID)) || (r.DeploymentID != "" && !ValidateUUID(r.DeploymentID)) || (r.CurrentStageID != "" && !ValidateUUID(r.CurrentStageID)) {
		return ErrInvalid
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.CreatedAt.Location() != time.UTC || r.UpdatedAt.Location() != time.UTC {
		return ErrInvalid
	}
	if r.Status == RunRunning {
		if r.StartedAt.IsZero() || r.StartedAt.Location() != time.UTC || !r.FinishedAt.IsZero() {
			return ErrInvalid
		}
	} else if requiresStartedRun(r.Status) {
		if r.StartedAt.IsZero() || r.StartedAt.Location() != time.UTC || r.FinishedAt.IsZero() || r.FinishedAt.Location() != time.UTC {
			return ErrInvalid
		}
	} else if isTerminalRun(r.Status) {
		if (!r.StartedAt.IsZero() && (r.FinishedAt.IsZero() || r.FinishedAt.Location() != time.UTC)) || (r.StartedAt.IsZero() && !r.FinishedAt.IsZero()) {
			return ErrInvalid
		}
	} else if !r.FinishedAt.IsZero() || !r.StartedAt.IsZero() {
		return ErrInvalid
	}
	if !orderedTimes(r.CreatedAt, r.StartedAt, r.FinishedAt, r.UpdatedAt) {
		return ErrInvalid
	}
	return nil
}
func (s RunStage) Validate() error {
	if !ValidateUUID(s.StageID) || !ValidateUUID(s.RunID) || !validOwnerID(s.OwnerID) || !ValidateUUID(s.PlanID) || !validKey(s.StageKey) || s.PlanRevision == 0 || s.StageRevision == 0 || !safeToken(s.StageIdempotencyKey) || !s.StageDigest.Valid() || !ValidateStageStatus(s.Status) || !ValidateUUID(s.TargetID) || s.TargetRevision == 0 || !s.TargetDigest.Valid() || s.Ordinal == 0 || (s.TaskID != "" && !ValidateUUID(s.TaskID)) || (s.ConfirmationID != "" && !ValidateUUID(s.ConfirmationID)) {
		return ErrInvalid
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.CreatedAt.Location() != time.UTC || s.UpdatedAt.Location() != time.UTC {
		return ErrInvalid
	}
	if s.Status == StageRunning {
		if s.StartedAt.IsZero() || s.StartedAt.Location() != time.UTC || !s.FinishedAt.IsZero() {
			return ErrInvalid
		}
	} else if stageRequiresStarted(s.Status) {
		if s.StartedAt.IsZero() || s.FinishedAt.IsZero() || s.StartedAt.Location() != time.UTC || s.FinishedAt.Location() != time.UTC {
			return ErrInvalid
		}
	} else if isTerminalStage(s.Status) {
		if (!s.StartedAt.IsZero() && (s.FinishedAt.IsZero() || s.FinishedAt.Location() != time.UTC)) || (s.StartedAt.IsZero() && !s.FinishedAt.IsZero()) {
			return ErrInvalid
		}
	} else if !s.FinishedAt.IsZero() || !s.StartedAt.IsZero() {
		return ErrInvalid
	}
	if !orderedTimes(s.CreatedAt, s.StartedAt, s.FinishedAt, s.UpdatedAt) {
		return ErrInvalid
	}
	return nil
}
func (a StepAttempt) Validate() error {
	if !ValidateUUID(a.AttemptID) || !ValidateUUID(a.RunID) || !ValidateUUID(a.StageID) || !validOwnerID(a.OwnerID) || !ValidateUUID(a.PlanID) || a.PlanRevision == 0 || !a.PlanDigest.Valid() || a.StageRevision == 0 || !a.StageDigest.Valid() || a.StepRevision == 0 || !a.StepDigest.Valid() || a.Revision == 0 || !validAttemptStatus(a.Status) || !validKey(a.StepKey) || a.Attempt == 0 || (a.ReceiptID != "" && !ValidateUUID(a.ReceiptID)) {
		return ErrInvalid
	}
	if a.Uncertain != (a.Status == AttemptUncertain) {
		return ErrInvalid
	}
	if a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() || a.CreatedAt.Location() != time.UTC || a.UpdatedAt.Location() != time.UTC {
		return ErrInvalid
	}
	if a.Status == AttemptRunning {
		if a.StartedAt.IsZero() || a.StartedAt.Location() != time.UTC || !a.FinishedAt.IsZero() {
			return ErrInvalid
		}
	} else if attemptRequiresStarted(a.Status) {
		if a.StartedAt.IsZero() || a.FinishedAt.IsZero() || a.StartedAt.Location() != time.UTC || a.FinishedAt.Location() != time.UTC {
			return ErrInvalid
		}
	} else if isTerminalAttempt(a.Status) {
		if (!a.StartedAt.IsZero() && (a.FinishedAt.IsZero() || a.FinishedAt.Location() != time.UTC)) || (a.StartedAt.IsZero() && !a.FinishedAt.IsZero()) {
			return ErrInvalid
		}
	} else if !a.FinishedAt.IsZero() || !a.StartedAt.IsZero() {
		return ErrInvalid
	}
	if !orderedTimes(a.CreatedAt, a.StartedAt, a.FinishedAt, a.UpdatedAt) {
		return ErrInvalid
	}
	return nil
}
func stageRequiresStarted(s StageStatus) bool {
	return s == StageSucceeded || s == StageFailed || s == StageUncertain
}
func attemptRequiresStarted(s AttemptStatus) bool {
	return s == AttemptSucceeded || s == AttemptFailed || s == AttemptUncertain
}
func orderedTimes(created, started, finished, updated time.Time) bool {
	if updated.Before(created) {
		return false
	}
	if !started.IsZero() && (started.Before(created) || updated.Before(started)) {
		return false
	}
	if !finished.IsZero() && (finished.Before(created) || !started.IsZero() && finished.Before(started) || updated.Before(finished)) {
		return false
	}
	return true
}
func validAttemptStatus(s AttemptStatus) bool {
	switch s {
	case AttemptPending, AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptUncertain, AttemptCanceled:
		return true
	}
	return false
}
func isTerminalAttempt(s AttemptStatus) bool {
	return s == AttemptSucceeded || s == AttemptFailed || s == AttemptUncertain || s == AttemptCanceled
}
func (r Receipt) Validate() error {
	if !ValidateUUID(r.ReceiptID) || !ValidateUUID(r.RunID) || !ValidateUUID(r.AttemptID) || !validOwnerID(r.OwnerID) || r.Revision == 0 || !validReceiptStatus(r.Status) || !safeToken(r.IdempotencyKey) || r.At.IsZero() || r.At.Location() != time.UTC || (r.ProviderOperation == "" && r.SSMCommandID == "") || strings.ContainsAny(r.RedactedDetails, "\r\n\x00") {
		return ErrInvalid
	}
	if r.OutputDigest != "" && !r.OutputDigest.Valid() {
		return ErrInvalid
	}
	if r.ResponseDigest != "" && !r.ResponseDigest.Valid() {
		return ErrInvalid
	}
	return nil
}
func validReceiptStatus(s ReceiptStatus) bool {
	switch s {
	case ReceiptAccepted, ReceiptSucceeded, ReceiptFailed, ReceiptUncertain, ReceiptCanceled:
		return true
	}
	return false
}
func (e Event) Validate() error {
	if !ValidateUUID(e.EventID) || !ValidateUUID(e.RunID) || !validOwnerID(e.OwnerID) || e.Revision == 0 || (e.StageID != "" && !ValidateUUID(e.StageID)) || (e.AttemptID != "" && !ValidateUUID(e.AttemptID)) || (e.Status != "" && !ValidateStageStatus(e.Status)) || e.Sequence == 0 || !safeToken(e.Type) || (e.Key != "" && !safeToken(e.Key)) || e.At.IsZero() || e.At.Location() != time.UTC {
		return ErrInvalid
	}
	if e.PayloadDigest != "" && !e.PayloadDigest.Valid() || e.Digest != "" && !e.Digest.Valid() {
		return ErrInvalid
	}
	return nil
}
func (b ServiceBinding) Validate() error {
	if !ValidateUUID(b.BindingID) || !validOwnerID(b.OwnerID) || !ValidateUUID(b.DeploymentID) || !ValidateUUID(b.ProjectID) || !ValidateUUID(b.RunID) || !ValidateUUID(b.TargetID) || b.TargetRevision == 0 || !b.TargetDigest.Valid() || (b.ReleaseID != "" && !safeText(b.ReleaseID, 128)) || b.Protocol == "" || b.Endpoint == "" || b.Revision == 0 || (b.Digest != "" && !b.Digest.Valid()) || (b.Protocol != "http" && b.Protocol != "https" && b.Protocol != "grpc" && b.Protocol != "grpcs" && b.Protocol != "ssm") {
		return ErrInvalid
	}
	u, e := url.Parse(b.Endpoint)
	if e != nil || u.Host == "" || u.User != nil || strings.ToLower(u.Scheme) != b.Protocol {
		return ErrInvalid
	}
	// An SSM binding is an inventory/management endpoint, not an invokable
	// service API.  Keep it incapable of carrying auth grants or operation
	// schemas; any SSM mutation must still be expressed as a new reviewed plan.
	if b.Protocol == "ssm" && (u.Path != "" || u.RawQuery != "" || u.Fragment != "" || len(b.AuthRefs) != 0 || len(b.OperationSchemas) != 0) {
		return ErrInvalid
	}
	if duplicateCredentials(b.AuthRefs) {
		return ErrInvalid
	}
	for _, id := range b.ArtifactIDs {
		if !ValidateUUID(id) {
			return ErrInvalid
		}
	}
	if b.HealthArtifact.ID != "" && validateArtifact(&b.HealthArtifact) != nil {
		return ErrInvalid
	}
	if b.UsageArtifact.ID != "" && validateArtifact(&b.UsageArtifact) != nil {
		return ErrInvalid
	}
	for _, c := range b.AuthRefs {
		if e := validateCredential(c); e != nil {
			return e
		}
	}
	seenSchema := map[string]bool{}
	for _, s := range b.OperationSchemas {
		if !safeToken(s.Name) || !safeToken(s.Version) || !s.Digest.Valid() {
			return ErrInvalid
		}
		if seenSchema[s.Name+"\x00"+s.Version] {
			return ErrInvalid
		}
		seenSchema[s.Name+"\x00"+s.Version] = true
	}
	return nil
}
func (b ServiceBinding) Normalize() (ServiceBinding, error) {
	if hasDuplicateStrings(b.ArtifactIDs) {
		return ServiceBinding{}, ErrInvalid
	}
	b.ArtifactIDs = normalizeSet(b.ArtifactIDs)
	provided := b.Digest
	b.Digest = ""
	if e := b.Validate(); e != nil {
		return ServiceBinding{}, e
	}
	d, e := CanonicalDigest(b)
	if e != nil {
		return ServiceBinding{}, e
	}
	if provided != "" && provided != d {
		return ServiceBinding{}, ErrDigestMismatch
	}
	b.Digest = d
	return b, nil
}
func (o TargetObservation) Validate() error {
	if !ValidateUUID(o.TargetID) || o.TargetRevision == 0 || o.ObservedAt.IsZero() || o.ObservedAt.Location() != time.UTC || !safeToken(o.State) || (o.Digest != "" && !o.Digest.Valid()) {
		return ErrInvalid
	}
	for k, v := range o.Facts {
		if !safeText(k, 128) || !safeText(v, 2048) {
			return ErrInvalid
		}
	}
	for _, w := range o.Warnings {
		if !safeText(w, 512) {
			return ErrInvalid
		}
	}
	return nil
}
func (o TargetObservation) Normalize() (TargetObservation, error) {
	o.Warnings = normalizeSet(o.Warnings)
	provided := o.Digest
	o.Digest = ""
	if e := o.Validate(); e != nil {
		return TargetObservation{}, e
	}
	d, e := CanonicalDigest(o)
	if e != nil {
		return TargetObservation{}, e
	}
	if provided != "" && provided != d {
		return TargetObservation{}, ErrDigestMismatch
	}
	o.Digest = d
	return o, nil
}
func (a ArtifactRef) Validate() error   { return validateArtifact(&a) }
func (s ScriptRunStep) validate() error { return validateScript(&s) }
