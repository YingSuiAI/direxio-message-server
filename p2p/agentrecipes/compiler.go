package agentrecipes

// The recipe compiler is intentionally a very small boundary.  A recipe
// declares which step kinds and constraints are allowed; the caller supplies
// every concrete value in an immutable, typed ExecutionStep binding.  There
// are no provider/project defaults here: accepting a missing binding would
// turn a declarative recipe into an implicit command generator.

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

// CompileContext contains server-owned immutable facts and one typed input
// binding for every executable recipe step (forward and rollback).
type CompileContext struct {
	TargetID       string
	TargetRevision uint64
	TargetDigest   execution.Digest
	ObservationRef execution.TargetObservationRef
	SecretRefs     map[string]execution.CredentialRef
	// AIConfiguration is optional plan metadata. When present, the compiler
	// binds the exact server-resolved provider secret to container steps; the
	// value never contains secret bytes.
	AIConfiguration *execution.AIConfiguration
	StepBindings    map[string]execution.ExecutionStep
}

// Compile materializes only forward stages. Rollback declarations are inert
// metadata attached to their forward dependency.
func Compile(recipe RecipeManifest, ctx CompileContext) ([]execution.ExecutionStage, error) {
	if err := Validate(recipe); err != nil {
		return nil, err
	}
	if !execution.ValidateUUID(ctx.TargetID) || ctx.TargetRevision == 0 || !ctx.TargetDigest.Valid() {
		return nil, fmt.Errorf("invalid compile target")
	}
	if ctx.StepBindings == nil {
		return nil, fmt.Errorf("step bindings are required")
	}

	declared := make(map[string]RecipeStep)
	for _, stage := range recipe.Stages {
		for _, step := range append(append([]RecipeStep(nil), stage.Steps...), stage.RollbackSteps...) {
			if _, exists := declared[step.StepKey]; exists {
				return nil, fmt.Errorf("duplicate recipe step binding key %q", step.StepKey)
			}
			declared[step.StepKey] = step
		}
	}
	for key := range ctx.StepBindings {
		if _, ok := declared[key]; !ok {
			return nil, fmt.Errorf("unused step binding %q", key)
		}
	}
	for key := range declared {
		if _, ok := ctx.StepBindings[key]; !ok {
			return nil, fmt.Errorf("missing step binding %q", key)
		}
	}

	out := make([]execution.ExecutionStage, 0, len(recipe.Stages))
	byKey := map[string]int{}
	for _, declaredStage := range recipe.Stages {
		if len(declaredStage.Steps) == 0 {
			continue // inert rollback declaration
		}
		compiled := make([]execution.ExecutionStep, 0, len(declaredStage.Steps))
		for _, step := range declaredStage.Steps {
			materialized, err := materializeStep(step, ctx)
			if err != nil {
				return nil, err
			}
			compiled = append(compiled, materialized)
		}
		stage := execution.ExecutionStage{
			StageKey: declaredStage.StageKey, Revision: 1, Title: declaredStage.Title,
			Kind: declaredStage.Kind, Risk: execution.Risk(declaredStage.Risk), Gate: execution.Gate(declaredStage.Gate),
			Effects: materializeEffects(declaredStage), DependsOn: append([]string(nil), declaredStage.DependsOn...),
			TargetID: ctx.TargetID, TargetRevision: ctx.TargetRevision, TargetDigest: ctx.TargetDigest,
			Steps: compiled, TimeoutSeconds: declaredStage.TimeoutSeconds, Probes: append([]string(nil), declaredStage.Probes...),
		}
		out = append(out, stage)
		byKey[stage.StageKey] = len(out) - 1
	}
	for _, declaredStage := range recipe.Stages {
		if len(declaredStage.RollbackSteps) == 0 {
			continue
		}
		if len(declaredStage.DependsOn) != 1 {
			return nil, fmt.Errorf("rollback %s requires one forward dependency", declaredStage.StageKey)
		}
		forwardIndex, ok := byKey[declaredStage.DependsOn[0]]
		if !ok {
			return nil, fmt.Errorf("rollback %s has no forward stage", declaredStage.StageKey)
		}
		forward := &out[forwardIndex]
		for _, step := range declaredStage.RollbackSteps {
			materialized, err := materializeStep(step, ctx)
			if err != nil {
				return nil, err
			}
			forward.RollbackSteps = append(forward.RollbackSteps, materialized)
		}
		forward.RollbackPolicy = &execution.RollbackPolicy{Risk: execution.RiskR4, Gate: execution.GateRollback}
	}
	return out, nil
}

func materializeStep(recipeStep RecipeStep, ctx CompileContext) (execution.ExecutionStep, error) {
	binding, ok := ctx.StepBindings[recipeStep.StepKey]
	if !ok {
		return execution.ExecutionStep{}, fmt.Errorf("missing step binding %q", recipeStep.StepKey)
	}
	if binding.StepKey != recipeStep.StepKey || string(binding.Kind) != recipeStep.Kind {
		return execution.ExecutionStep{}, fmt.Errorf("step binding %q kind/key mismatch", recipeStep.StepKey)
	}
	if err := validateBindingEnvelope(binding, recipeStep, ctx); err != nil {
		return execution.ExecutionStep{}, err
	}
	if err := validateSingleBranch(binding, recipeStep.Kind); err != nil {
		return execution.ExecutionStep{}, err
	}

	grants, err := bindingNetworkGrants(recipeStep.NetworkGrants, binding)
	if err != nil {
		return execution.ExecutionStep{}, fmt.Errorf("step %s network grants: %w", recipeStep.StepKey, err)
	}
	secretPurposes := append([]string(nil), recipeStep.SecretRefs...)
	if ctx.AIConfiguration != nil && ctx.AIConfiguration.Mode == execution.AIAuthModeAPIKey && recipeStep.Kind == "container.apply" {
		// AI credentials are a cross-recipe capability, not an inline recipe
		// value. The plan carries only the immutable metadata reference.
		secretPurposes = append(secretPurposes, execution.AISecretPurposeProviderAPIKey)
	}
	secrets, err := boundSecrets(secretPurposes, ctx.SecretRefs)
	if err != nil {
		return execution.ExecutionStep{}, err
	}
	if len(binding.SecretRefs) > 0 && !equalCredentialRefs(binding.SecretRefs, secrets) {
		return execution.ExecutionStep{}, fmt.Errorf("step %s contains secret binding smuggling", recipeStep.StepKey)
	}
	if binding.ScriptRun != nil && len(binding.ScriptRun.SecretRefs) > 0 && !equalCredentialRefs(binding.ScriptRun.SecretRefs, secrets) {
		return execution.ExecutionStep{}, fmt.Errorf("step %s contains script secret binding smuggling", recipeStep.StepKey)
	}
	post, err := bindPostcondition(recipeStep.Postcondition, binding)
	if err != nil {
		return execution.ExecutionStep{}, fmt.Errorf("step %s postcondition: %w", recipeStep.StepKey, err)
	}

	// Copy only the typed payload. Envelope metadata is rewritten below from
	// the recipe and server context, so a caller cannot smuggle a different
	// target, timeout, grant, secret, or idempotency value into the plan.
	s := execution.ExecutionStep{StepKey: recipeStep.StepKey, Kind: execution.StepKind(recipeStep.Kind), TargetID: ctx.TargetID, TargetRevision: ctx.TargetRevision, TargetDigest: ctx.TargetDigest, TimeoutSeconds: recipeStep.TimeoutSeconds, IdempotencyMarker: recipeStep.IdempotencyMarker, NetworkGrants: grants, SecretRefs: secrets, OutputPolicy: recipeStep.OutputPolicy, Postcondition: post}
	s.TargetInspect = cloneTargetInspect(binding.TargetInspect)
	s.ComputeProvision = cloneComputeProvision(binding.ComputeProvision)
	s.ComputeDestroy = cloneComputeDestroy(binding.ComputeDestroy)
	s.SourceFetch = cloneSourceFetch(binding.SourceFetch)
	s.ArtifactUpload = cloneArtifactUpload(binding.ArtifactUpload)
	s.PackageEnsure = clonePackageEnsure(binding.PackageEnsure)
	s.FilePut = cloneFilePut(binding.FilePut)
	s.ContainerApply = cloneContainerApply(binding.ContainerApply)
	s.SystemdApply = cloneSystemdApply(binding.SystemdApply)
	s.HTTPProbe = cloneHTTPProbe(binding.HTTPProbe)
	s.TCPProbe = cloneTCPProbe(binding.TCPProbe)
	s.ArtifactCollect = cloneArtifactCollect(binding.ArtifactCollect)
	s.Cleanup = cloneCleanup(binding.Cleanup)
	s.SecretProvision = cloneSecretProvision(binding.SecretProvision)
	s.ExternalAuth = cloneExternalAuth(binding.ExternalAuth)
	if binding.ScriptRun != nil {
		if binding.ScriptRun.TimeoutSeconds != 0 && binding.ScriptRun.TimeoutSeconds != recipeStep.TimeoutSeconds || binding.ScriptRun.IdempotencyMarker != "" && binding.ScriptRun.IdempotencyMarker != recipeStep.IdempotencyMarker || binding.ScriptRun.OutputLimit != 0 && binding.ScriptRun.OutputLimit != recipeStep.OutputLimit {
			return execution.ExecutionStep{}, fmt.Errorf("step %s contains script constraint smuggling", recipeStep.StepKey)
		}
		s.ScriptRun = cloneScriptRun(binding.ScriptRun)
		s.ScriptRun.NetworkGrants = append([]execution.NetworkGrant(nil), grants...)
		s.ScriptRun.SecretRefs = append([]execution.CredentialRef(nil), secrets...)
		s.ScriptRun.TimeoutSeconds = recipeStep.TimeoutSeconds
		s.ScriptRun.IdempotencyMarker = recipeStep.IdempotencyMarker
		s.ScriptRun.Postcondition = clonePostcondition(post)
		s.ScriptRun.OutputLimit = recipeStep.OutputLimit
		s.ScriptRun.AllowedExitCodes = append([]int(nil), recipeStep.AllowedExitCodes...)
		s.ScriptRun.Redaction = materializeRedaction(recipeStep.Redaction)
		if recipeStep.Interpreter != "" && recipeStep.Interpreter != s.ScriptRun.Interpreter {
			return execution.ExecutionStep{}, fmt.Errorf("step %s interpreter violates recipe constraint", recipeStep.StepKey)
		}
		if recipeStep.Root != s.ScriptRun.Root {
			return execution.ExecutionStep{}, fmt.Errorf("step %s root privilege violates recipe constraint", recipeStep.StepKey)
		}
		observation, e := ctx.ObservationRef.Normalize()
		if e != nil || observation.TargetID != ctx.TargetID || observation.TargetRevision != ctx.TargetRevision {
			return execution.ExecutionStep{}, fmt.Errorf("step %s requires an exact immutable observation", recipeStep.StepKey)
		}
		s.ObservationRef = &observation
	}
	return s, nil
}

func validateBindingEnvelope(binding execution.ExecutionStep, recipeStep RecipeStep, ctx CompileContext) error {
	if binding.TargetID != "" && binding.TargetID != ctx.TargetID || binding.TargetRevision != 0 && binding.TargetRevision != ctx.TargetRevision || binding.TargetDigest != "" && binding.TargetDigest != ctx.TargetDigest {
		return fmt.Errorf("step %s contains target/digest smuggling", recipeStep.StepKey)
	}
	if binding.Digest != "" {
		return fmt.Errorf("step %s contains caller-supplied execution digest", recipeStep.StepKey)
	}
	if binding.ObservationRef != nil {
		observation, err := ctx.ObservationRef.Normalize()
		if err != nil || *binding.ObservationRef != observation {
			return fmt.Errorf("step %s contains observation smuggling", recipeStep.StepKey)
		}
	}
	if binding.TimeoutSeconds != 0 && binding.TimeoutSeconds != recipeStep.TimeoutSeconds || binding.IdempotencyMarker != "" && binding.IdempotencyMarker != recipeStep.IdempotencyMarker || binding.OutputPolicy != "" && binding.OutputPolicy != recipeStep.OutputPolicy {
		return fmt.Errorf("step %s contains envelope constraint smuggling", recipeStep.StepKey)
	}
	if binding.ArtifactRefs != nil || binding.Permissions != nil || binding.DependsOn != nil || binding.OutputKey != "" {
		return fmt.Errorf("step %s contains unsupported envelope fields", recipeStep.StepKey)
	}
	return nil
}

func validateSingleBranch(binding execution.ExecutionStep, kind string) error {
	branches := []struct {
		kind string
		v    any
	}{
		{"target.inspect", binding.TargetInspect}, {"compute.provision", binding.ComputeProvision}, {"compute.destroy", binding.ComputeDestroy}, {"source.fetch", binding.SourceFetch}, {"artifact.upload", binding.ArtifactUpload}, {"package.ensure", binding.PackageEnsure}, {"file.put", binding.FilePut}, {"container.apply", binding.ContainerApply}, {"systemd.apply", binding.SystemdApply}, {"script.run", binding.ScriptRun}, {"http.probe", binding.HTTPProbe}, {"tcp.probe", binding.TCPProbe}, {"artifact.collect", binding.ArtifactCollect}, {"cleanup", binding.Cleanup}, {"secret.provision", binding.SecretProvision}, {"auth.external", binding.ExternalAuth},
	}
	count := 0
	matched := false
	for _, branch := range branches {
		if branch.v != nil && !(reflect.ValueOf(branch.v).Kind() == reflect.Ptr && reflect.ValueOf(branch.v).IsNil()) {
			count++
			matched = matched || branch.kind == kind
		}
	}
	if count != 1 || !matched {
		return fmt.Errorf("step binding for %s must contain exactly one matching typed branch", kind)
	}
	return nil
}

func boundSecrets(purposes []string, refs map[string]execution.CredentialRef) ([]execution.CredentialRef, error) {
	if len(purposes) == 0 {
		return nil, nil
	}
	out := make([]execution.CredentialRef, 0, len(purposes))
	for _, purpose := range purposes {
		ref, ok := refs[purpose]
		if !ok || ref.Purpose != purpose || ref.Ref == "" || ref.Revision == 0 || !ref.BindingDigest.Valid() {
			return nil, fmt.Errorf("missing immutable secret binding for %s", purpose)
		}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func equalCredentialRefs(a, b []execution.CredentialRef) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]execution.CredentialRef(nil), a...)
	bb := append([]execution.CredentialRef(nil), b...)
	sort.Slice(aa, func(i, j int) bool { return aa[i].Ref < aa[j].Ref })
	sort.Slice(bb, func(i, j int) bool { return bb[i].Ref < bb[j].Ref })
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func bindingNetworkGrants(declared []string, binding execution.ExecutionStep) ([]execution.NetworkGrant, error) {
	supplied := append([]execution.NetworkGrant(nil), binding.NetworkGrants...)
	if binding.ScriptRun != nil {
		nested := append([]execution.NetworkGrant(nil), binding.ScriptRun.NetworkGrants...)
		if len(supplied) > 0 && len(nested) > 0 && !equalNetworkGrantRefs(supplied, nested) {
			return nil, fmt.Errorf("top-level and script network grants differ")
		}
		if len(supplied) == 0 {
			supplied = nested
		}
	}
	if len(declared) != len(supplied) {
		return nil, fmt.Errorf("binding grants do not match declared grants")
	}
	for i := range supplied {
		normalized, err := supplied[i].Normalize()
		if err != nil {
			return nil, fmt.Errorf("invalid bound grant: %w", err)
		}
		supplied[i] = normalized
		if !grantMatches(declared[i], normalized) {
			return nil, fmt.Errorf("bound grant %d widens recipe grant", i)
		}
	}
	return supplied, nil
}

func equalNetworkGrantRefs(a, b []execution.NetworkGrant) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]execution.NetworkGrant(nil), a...)
	bb := append([]execution.NetworkGrant(nil), b...)
	for i := range aa {
		na, ea := aa[i].Normalize()
		nb, eb := bb[i].Normalize()
		if ea != nil || eb != nil || na != nb {
			return false
		}
	}
	return true
}

func grantMatches(declared string, grant execution.NetworkGrant) bool {
	if declared == PublicHTTPSEgressGrant {
		return grant.Scope == "external" && grant.Scheme == "https" && grant.Host == execution.PublicHTTPSWildcardHost && grant.Port == 443 && grant.PathPrefix == ""
	}
	if declared == SourceOCIRegistryGrant {
		return grant.Scope == "external" && grant.Scheme == "https" && grant.Port == 443
	}
	if strings.HasPrefix(declared, "target_local:") {
		return grant.Scope == "target_local"
	}
	raw := strings.TrimPrefix(declared, "egress:")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() != grant.Host || u.Port() != fmt.Sprint(grant.Port) || grant.Scope != "external" {
		return false
	}
	return templatePathMatch(u.Path, grant.PathPrefix)
}

func templatePathMatch(pattern, value string) bool {
	if pattern == "" {
		pattern = "/"
	}
	if value == "" {
		value = "/"
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		if strings.HasPrefix(pattern[i:], "${") {
			end := strings.IndexByte(pattern[i+2:], '}')
			if end < 0 {
				return false
			}
			b.WriteString(`[^/]+`)
			i += end + 3
			continue
		}
		b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		i++
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()).MatchString(value)
}

func materializeEffects(stage RecipeStage) []execution.Gate {
	seen := map[execution.Gate]bool{}
	for _, step := range stage.Steps {
		if step.Root {
			seen[execution.GateRemotePrivilegedExecution] = true
		}
		if len(step.SecretRefs) > 0 {
			seen[execution.GateSecretAccess] = true
		}
		if step.Kind == "auth.external" {
			seen[execution.GateExternalAuth] = true
		}
	}
	effects := make([]execution.Gate, 0, len(seen))
	for effect := range seen {
		effects = append(effects, effect)
	}
	sort.Slice(effects, func(i, j int) bool { return effects[i] < effects[j] })
	return effects
}

// AddAIAuthorizationStages adds the provider-neutral authorization boundary
// required by an AI-enabled container plan. It deliberately operates on
// already materialized typed steps: no recipe or caller can provide secret
// bytes, shell, or an alternate delivery mechanism.
func AddAIAuthorizationStages(stages []execution.ExecutionStage, configuration *execution.AIConfiguration) ([]execution.ExecutionStage, error) {
	if configuration == nil {
		return stages, nil
	}
	normalized, err := configuration.Normalize()
	if err != nil {
		return nil, err
	}
	consumer := -1
	for i := range stages {
		for j := range stages[i].Steps {
			if stages[i].Steps[j].Kind == execution.StepContainerApply && stages[i].Steps[j].ContainerApply != nil {
				if consumer >= 0 && consumer != i {
					return nil, fmt.Errorf("AI configuration requires one container consumer stage")
				}
				consumer = i
			}
		}
	}
	if consumer < 0 {
		return nil, fmt.Errorf("AI configuration requires a container consumer stage")
	}
	consumerStage := &stages[consumer]
	if consumerStage.Gate != execution.GateRemoteExecution && consumerStage.Gate != execution.GateRemotePrivilegedExecution {
		return nil, fmt.Errorf("AI container consumer must be a remote execution stage")
	}
	stageKey, stepKey := "authorize-ai", "authorize-ai"
	for _, stage := range stages {
		if stage.StageKey == stageKey {
			return nil, fmt.Errorf("reserved AI authorization stage key already exists")
		}
	}
	var stage execution.ExecutionStage
	var step execution.ExecutionStep
	if normalized.Mode == execution.AIAuthModeAPIKey {
		ref := normalized.CredentialRef()
		stage = execution.ExecutionStage{StageKey: stageKey, Revision: 1, Title: "Authorize provider secret", Kind: string(execution.StepSecretProvision), Risk: execution.RiskR2, Gate: execution.GateSecretAccess, TargetID: consumerStage.TargetID, TargetRevision: consumerStage.TargetRevision, TargetDigest: consumerStage.TargetDigest, TimeoutSeconds: 120}
		stage.Effects = []execution.Gate{execution.GateSecretAccess}
		step = execution.ExecutionStep{StepKey: stepKey, Kind: execution.StepSecretProvision, TargetID: consumerStage.TargetID, TargetRevision: consumerStage.TargetRevision, TargetDigest: consumerStage.TargetDigest, SecretRefs: []execution.CredentialRef{ref}, TimeoutSeconds: 120, IdempotencyMarker: "authorize-ai", OutputPolicy: execution.OutputDiscard, SecretProvision: &execution.SecretProvisionStep{Delivery: "target_secure_parameter"}}
		for i := range consumerStage.Steps {
			if consumerStage.Steps[i].Kind == execution.StepContainerApply {
				if !containsCredentialRef(consumerStage.Steps[i].SecretRefs, ref) {
					consumerStage.Steps[i].SecretRefs = append(consumerStage.Steps[i].SecretRefs, ref)
				}
			}
		}
		consumerStage.Effects = appendUniqueGate(consumerStage.Effects, execution.GateSecretAccess)
	} else {
		stage = execution.ExecutionStage{StageKey: stageKey, Revision: 1, Title: "Complete external provider authorization", Kind: string(execution.StepExternalAuth), Risk: execution.RiskR2, Gate: execution.GateExternalAuth, TargetID: consumerStage.TargetID, TargetRevision: consumerStage.TargetRevision, TargetDigest: consumerStage.TargetDigest, TimeoutSeconds: 120}
		stage.Effects = []execution.Gate{execution.GateExternalAuth}
		step = execution.ExecutionStep{StepKey: stepKey, Kind: execution.StepExternalAuth, TargetID: consumerStage.TargetID, TargetRevision: consumerStage.TargetRevision, TargetDigest: consumerStage.TargetDigest, TimeoutSeconds: 120, IdempotencyMarker: "authorize-ai", OutputPolicy: execution.OutputDiscard, ExternalAuth: &execution.ExternalAuthStep{Provider: normalized.Provider, Status: normalized.Status}}
	}
	stage.Steps = []execution.ExecutionStep{step}
	consumerStage.DependsOn = appendUniqueStageDependency(consumerStage.DependsOn, stageKey)
	// Keep the authorization stage immediately before its consumer for stable
	// plan review and deterministic stage materialization.
	out := make([]execution.ExecutionStage, 0, len(stages)+1)
	for i, candidate := range stages {
		if i == consumer {
			out = append(out, stage)
		}
		out = append(out, candidate)
	}
	return out, nil
}

func containsCredentialRef(refs []execution.CredentialRef, want execution.CredentialRef) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

func appendUniqueStageDependency(deps []string, want string) []string {
	for _, dep := range deps {
		if dep == want {
			return deps
		}
	}
	return append(deps, want)
}

func appendUniqueGate(gates []execution.Gate, want execution.Gate) []execution.Gate {
	for _, gate := range gates {
		if gate == want {
			return gates
		}
	}
	return append(gates, want)
}

// bindPostcondition treats the recipe value as a schema constraint.  Values
// such as a unit name or URL are concrete inputs and must come from the typed
// binding; copying a placeholder here would produce an unusable plan.
func bindPostcondition(recipe map[string]interface{}, binding execution.ExecutionStep) (*execution.Postcondition, error) {
	var requiredType string
	if recipe != nil {
		requiredType, _ = recipe["type"].(string)
	}
	var post *execution.Postcondition
	if binding.Postcondition != nil {
		post = clonePostcondition(binding.Postcondition)
	}
	if binding.ScriptRun != nil && binding.ScriptRun.Postcondition != nil {
		nested := clonePostcondition(binding.ScriptRun.Postcondition)
		if post != nil && *post != *nested {
			return nil, fmt.Errorf("top-level and script postconditions differ")
		}
		post = nested
	}
	if requiredType == "" {
		if post != nil {
			return nil, fmt.Errorf("unexpected postcondition")
		}
		return nil, nil
	}
	if post == nil || post.Type != requiredType || post.Value == "" {
		return nil, fmt.Errorf("typed postcondition is required")
	}
	return post, nil
}

func materializeRedaction(v map[string]interface{}) execution.RedactionPolicy {
	var out execution.RedactionPolicy
	if patterns, ok := v["patterns"].([]interface{}); ok {
		for _, raw := range patterns {
			if pattern, ok := raw.(string); ok {
				out.Patterns = append(out.Patterns, pattern)
			}
		}
	}
	if replace, ok := v["replace"].(string); ok {
		out.Replace = replace
	}
	return out
}

func clonePostcondition(v *execution.Postcondition) *execution.Postcondition {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// Typed payloads are value structs except for slices/maps in ScriptRun.  The
// explicit clones keep the returned plan independent of caller-owned memory.
func cloneTargetInspect(v *execution.TargetInspectStep) *execution.TargetInspectStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneComputeProvision(v *execution.ComputeProvisionStep) *execution.ComputeProvisionStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneComputeDestroy(v *execution.ComputeDestroyStep) *execution.ComputeDestroyStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneSourceFetch(v *execution.SourceFetchStep) *execution.SourceFetchStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneArtifactUpload(v *execution.ArtifactUploadStep) *execution.ArtifactUploadStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func clonePackageEnsure(v *execution.PackageEnsureStep) *execution.PackageEnsureStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneFilePut(v *execution.FilePutStep) *execution.FilePutStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneContainerApply(v *execution.ContainerApplyStep) *execution.ContainerApplyStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneSystemdApply(v *execution.SystemdApplyStep) *execution.SystemdApplyStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneHTTPProbe(v *execution.HTTPProbeStep) *execution.HTTPProbeStep {
	if v == nil {
		return nil
	}
	c := *v
	c.ExpectedStatus = append([]int(nil), v.ExpectedStatus...)
	return &c
}
func cloneTCPProbe(v *execution.TCPProbeStep) *execution.TCPProbeStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneArtifactCollect(v *execution.ArtifactCollectStep) *execution.ArtifactCollectStep {
	if v == nil {
		return nil
	}
	c := *v
	c.Paths = append([]string(nil), v.Paths...)
	return &c
}
func cloneCleanup(v *execution.CleanupStep) *execution.CleanupStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneSecretProvision(v *execution.SecretProvisionStep) *execution.SecretProvisionStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneExternalAuth(v *execution.ExternalAuthStep) *execution.ExternalAuthStep {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func cloneScriptRun(v *execution.ScriptRunStep) *execution.ScriptRunStep {
	if v == nil {
		return nil
	}
	c := *v
	c.Argv = append([]string(nil), v.Argv...)
	c.Env = map[string]string{}
	for k, value := range v.Env {
		c.Env[k] = value
	}
	c.SecretRefs = append([]execution.CredentialRef(nil), v.SecretRefs...)
	c.NetworkGrants = append([]execution.NetworkGrant(nil), v.NetworkGrants...)
	c.AllowedExitCodes = append([]int(nil), v.AllowedExitCodes...)
	c.Redaction.Patterns = append([]string(nil), v.Redaction.Patterns...)
	c.Postcondition = clonePostcondition(v.Postcondition)
	return &c
}
