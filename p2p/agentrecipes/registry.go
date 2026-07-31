// Package agentrecipes exposes trusted, declarative deployment recipes.
package agentrecipes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

var idPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var identPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var placeholderPattern = regexp.MustCompile(`^\$\{[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*\}$`)
var embeddedPlaceholderPattern = regexp.MustCompile(`\$\{([a-z][a-z0-9]*(?:[._-][a-z0-9]+)*)\}`)
var endpointPattern = regexp.MustCompile(`^https://[a-z0-9.-]+(?:/[a-zA-Z0-9._${}-]+)*$`)
var sha256PlaceholderPattern = regexp.MustCompile(`^\$\{[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*\.sha256\}$`)
var allowedEndpointHosts = map[string]struct{}{"registry.example": {}, "source.example": {}}
var grantPattern = regexp.MustCompile(`^egress:https://[a-z0-9.-]+:[0-9]{1,5}/[A-Za-z0-9._${}/-]+$`)
var targetInstanceIdentityCapabilityPattern = regexp.MustCompile(`^target\.instance\.i-(?:[0-9a-f]{8}|[0-9a-f]{17})$`)

const (
	// PublicHTTPSEgressGrant is intentionally broad: the initial AWS profile
	// enforces destination-independent public TCP/443 egress. OCI identities
	// are pinned by digest, not by a hostname-aware network control.
	PublicHTTPSEgressGrant = "public_https_egress"
	// SourceOCIRegistryGrant remains a parser-level alias for external recipe
	// packages, but production manifests must use PublicHTTPSEgressGrant.
	SourceOCIRegistryGrant = "source_oci_registry"
)

const maxRecipeBytes = 1 << 20
const maxRecipeDepth = 32
const maxRecipeNodes = 4096

const (
	maxRecipeStages  = 64
	maxRecipeSteps   = 256
	maxRecipeTimeout = uint64(24 * 60 * 60)
	maxRecipeOutput  = uint64(16 * 1024 * 1024)
)

var initialStepKinds = map[string]struct{}{
	"target.inspect": {}, "compute.provision": {}, "compute.destroy": {}, "source.fetch": {}, "artifact.upload": {}, "package.ensure": {}, "file.put": {}, "container.apply": {}, "systemd.apply": {}, "script.run": {}, "http.probe": {}, "tcp.probe": {}, "artifact.collect": {}, "cleanup": {}, "secret.provision": {}, "auth.external": {},
}
var capabilities = map[string]struct{}{"transport.aws_ssm": {}, "target.aws_ec2_instance": {}, "target.aws_compute_reservation": {}, "runtime.container": {}, "runtime.systemd": {}, "probe.http": {}, "probe.tcp": {}, "artifact.fetch": {}, "artifact.collect": {}, "secret.reference": {}, "compute.catalog": {}, "compute.provision": {}}
var risks = map[string]struct{}{"R0": {}, "R1": {}, "R2": {}, "R3": {}, "R4": {}}
var gates = map[string]struct{}{"none": {}, "resource_purchase": {}, "secret_access": {}, "external_auth": {}, "remote_execution": {}, "remote_privileged_execution": {}, "public_network_exposure": {}, "dns_change": {}, "tls_certificate_issue": {}, "data_migration": {}, "production_cutover": {}, "repository_write": {}, "service_destroy": {}, "rollback": {}}

type RecipeManifest struct {
	ID                         string            `json:"id"`
	Version                    string            `json:"version"`
	SchemaVersion              string            `json:"schema_version"`
	ContentDigest              string            `json:"content_digest"`
	MinimumCoreVersion         string            `json:"minimum_core_version"`
	InputSchema                json.RawMessage   `json:"input_schema,omitempty"`
	OutputSchema               json.RawMessage   `json:"output_schema,omitempty"`
	InputSchemaRef             string            `json:"input_schema_ref,omitempty"`
	OutputSchemaRef            string            `json:"output_schema_ref,omitempty"`
	IntentTags                 []string          `json:"intent_tags"`
	AllowedStepKinds           []string          `json:"allowed_step_kinds"`
	RequiredTargetCapabilities []string          `json:"required_target_capabilities"`
	NetworkAccess              NetworkAccess     `json:"network_access"`
	SecretPurposes             []string          `json:"secret_purposes"`
	Metadata                   map[string]string `json:"metadata,omitempty"`
	Stages                     []RecipeStage     `json:"stages"`
}
type NetworkAccess struct {
	Allowed   bool     `json:"allowed"`
	Purpose   string   `json:"purpose,omitempty"`
	Endpoints []string `json:"endpoints,omitempty"`
}
type RecipeStage struct {
	StageKey       string       `json:"stage_key"`
	Title          string       `json:"title"`
	Kind           string       `json:"kind"`
	Risk           string       `json:"risk"`
	Gate           string       `json:"gate"`
	Effects        []string     `json:"effects,omitempty"`
	DependsOn      []string     `json:"depends_on,omitempty"`
	Steps          []RecipeStep `json:"steps"`
	RollbackSteps  []RecipeStep `json:"rollback_steps,omitempty"`
	Probes         []string     `json:"probes,omitempty"`
	TimeoutSeconds uint64       `json:"timeout_seconds"`
}
type RecipeStep struct {
	StepKey           string                 `json:"step_key"`
	Kind              string                 `json:"kind"`
	Template          map[string]interface{} `json:"template,omitempty"`
	ArtifactRef       map[string]interface{} `json:"artifact_ref,omitempty"`
	Interpreter       string                 `json:"interpreter,omitempty"`
	Argv              []string               `json:"argv,omitempty"`
	CWD               string                 `json:"cwd,omitempty"`
	EnvRefs           []string               `json:"env_refs,omitempty"`
	SecretRefs        []string               `json:"secret_refs,omitempty"`
	Root              bool                   `json:"root,omitempty"`
	NetworkGrants     []string               `json:"network_grants,omitempty"`
	AllowedExitCodes  []int                  `json:"allowed_exit_codes,omitempty"`
	TimeoutSeconds    uint64                 `json:"timeout_seconds"`
	OutputLimit       uint64                 `json:"output_limit,omitempty"`
	Redaction         map[string]interface{} `json:"redaction,omitempty"`
	Postcondition     map[string]interface{} `json:"postcondition,omitempty"`
	OutputPolicy      string                 `json:"output_policy,omitempty"`
	IdempotencyMarker string                 `json:"idempotency_marker"`
}
type Pin struct{ ID, Version, ContentDigest string }
type SelectionQuery struct {
	Intent             string
	TargetCapabilities []string
	Limit              int
}
type Registry struct{ manifests []RecipeManifest }

func Builtin() (*Registry, error) {
	var names []string
	if err := fs.WalkDir(manifestFS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(name, ".json") {
			names = append(names, name)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(names)
	raw := make([][]byte, 0, len(names))
	for _, name := range names {
		b, e := manifestFS.ReadFile(name)
		if e != nil {
			return nil, e
		}
		raw = append(raw, b)
	}
	return NewRegistry(raw...)
}
func NewBuiltinRegistry() (*Registry, error) { return Builtin() }
func NewRegistry(contents ...[]byte) (*Registry, error) {
	out := make([]RecipeManifest, 0, len(contents))
	seen := map[string]struct{}{}
	for _, b := range contents {
		m, e := Parse(b)
		if e != nil {
			return nil, e
		}
		k := m.ID + "@" + m.Version
		if _, ok := seen[k]; ok {
			return nil, fmt.Errorf("duplicate recipe %s", k)
		}
		seen[k] = struct{}{}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Version < out[j].Version
	})
	return &Registry{manifests: out}, nil
}
func (r *Registry) Manifests() []RecipeManifest {
	if r == nil {
		return nil
	}
	out := make([]RecipeManifest, len(r.manifests))
	for i, m := range r.manifests {
		out[i] = clone(m)
	}
	return out
}
func (r *Registry) Resolve(pin Pin) (RecipeManifest, error) {
	if r == nil {
		return RecipeManifest{}, errors.New("nil recipe registry")
	}
	for _, m := range r.manifests {
		if m.ID == pin.ID && m.Version == pin.Version {
			if m.ContentDigest != pin.ContentDigest {
				return RecipeManifest{}, fmt.Errorf("digest mismatch")
			}
			return clone(m), nil
		}
	}
	return RecipeManifest{}, fmt.Errorf("recipe pin not found: %s@%s", pin.ID, pin.Version)
}
func (r *Registry) ResolveExact(id, version, d string) (RecipeManifest, error) {
	return r.Resolve(Pin{id, version, d})
}
func (r *Registry) Select(q SelectionQuery) ([]RecipeManifest, error) {
	if r == nil {
		return nil, errors.New("nil recipe registry")
	}
	if strings.TrimSpace(q.Intent) == "" {
		return nil, errors.New("intent is required")
	}
	instanceIdentities := 0
	for _, c := range q.TargetCapabilities {
		if _, ok := capabilities[c]; ok {
			continue
		}
		if targetInstanceIdentityCapabilityPattern.MatchString(c) {
			instanceIdentities++
			continue
		}
		return nil, fmt.Errorf("unknown target capability %q", c)
	}
	if instanceIdentities > 1 {
		return nil, fmt.Errorf("multiple target instance identities")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1
	}
	if limit > 3 {
		limit = 3
	}
	want := map[string]struct{}{}
	for _, c := range q.TargetCapabilities {
		// The instance identity is server-owned target snapshot metadata. It is
		// deliberately not part of recipe capability satisfaction; manifests
		// cannot request or branch on a concrete EC2 instance identifier.
		if _, known := capabilities[c]; known {
			want[c] = struct{}{}
		}
	}
	out := make([]RecipeManifest, 0, limit)
	for _, m := range r.manifests {
		if contains(m.IntentTags, "fixture") && q.Intent != "fixture" {
			continue
		}
		if !contains(m.IntentTags, q.Intent) || !subset(m.RequiredTargetCapabilities, want) {
			continue
		}
		out = append(out, clone(m))
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func Parse(content []byte) (RecipeManifest, error) {
	if len(content) > maxRecipeBytes {
		return RecipeManifest{}, errors.New("recipe manifest exceeds byte cap")
	}
	var raw map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(content))
	if e := dec.Decode(&raw); e != nil {
		return RecipeManifest{}, e
	}
	var trail interface{}
	if e := dec.Decode(&trail); e == nil {
		return RecipeManifest{}, errors.New("trailing JSON")
	} else if !errors.Is(e, io.EOF) {
		return RecipeManifest{}, e
	}
	if e := rejectUnsafe(raw, ""); e != nil {
		return RecipeManifest{}, e
	}
	if _, e := canonicalValue(content); e != nil {
		return RecipeManifest{}, fmt.Errorf("recipe canonical JSON: %w", e)
	}
	var m RecipeManifest
	strict := json.NewDecoder(bytes.NewReader(content))
	strict.UseNumber()
	strict.DisallowUnknownFields()
	if e := strict.Decode(&m); e != nil {
		return RecipeManifest{}, e
	}
	if e := Validate(m); e != nil {
		return RecipeManifest{}, e
	}
	if ContentDigest(content) != m.ContentDigest {
		return RecipeManifest{}, errors.New("content_digest mismatch")
	}
	return m, nil
}
func Validate(m RecipeManifest) error {
	if !idPattern.MatchString(m.ID) || !semverPattern.MatchString(m.Version) || m.SchemaVersion != "recipe/v1" || !semverPattern.MatchString(m.MinimumCoreVersion) || !digestPattern.MatchString(m.ContentDigest) {
		return errors.New("invalid recipe identity/version/digest")
	}
	if (len(m.InputSchema) == 0) == (m.InputSchemaRef == "") || (len(m.OutputSchema) == 0) == (m.OutputSchemaRef == "") {
		return errors.New("recipe requires one input/output schema form")
	}
	if err := schema(m.InputSchema); err != nil {
		return err
	}
	if err := schema(m.OutputSchema); err != nil {
		return err
	}
	if !validSchemaRef(m.InputSchemaRef) || !validSchemaRef(m.OutputSchemaRef) {
		return errors.New("invalid local schema ref")
	}
	if !sortedUnique(m.IntentTags) || !sortedUnique(m.RequiredTargetCapabilities) || !sortedUnique(m.SecretPurposes) {
		return errors.New("recipe metadata must be sorted and unique")
	}
	if !sortedUnique(m.AllowedStepKinds) || len(m.AllowedStepKinds) == 0 {
		return errors.New("recipe allowed_step_kinds must be sorted and non-empty")
	}
	for _, kind := range m.AllowedStepKinds {
		if _, ok := initialStepKinds[kind]; !ok {
			return fmt.Errorf("unknown allowed step kind %q", kind)
		}
	}
	if !m.NetworkAccess.Allowed && (m.NetworkAccess.Purpose != "" || len(m.NetworkAccess.Endpoints) > 0) {
		return errors.New("disabled network metadata")
	}
	if m.NetworkAccess.Allowed {
		if !identPattern.MatchString(m.NetworkAccess.Purpose) || len(m.NetworkAccess.Endpoints) == 0 {
			return errors.New("allowed network metadata requires purpose and endpoints")
		}
		if len(m.NetworkAccess.Endpoints) > 16 || !sortedUnique(m.NetworkAccess.Endpoints) {
			return errors.New("network endpoints must be sorted and unique")
		}
		for _, endpoint := range m.NetworkAccess.Endpoints {
			if !validEndpoint(endpoint) {
				return fmt.Errorf("endpoint is not a bounded template: %q", endpoint)
			}
		}
	}
	for _, c := range m.RequiredTargetCapabilities {
		if _, ok := capabilities[c]; !ok {
			return fmt.Errorf("unknown target capability %q", c)
		}
	}
	for _, tag := range m.IntentTags {
		if !identPattern.MatchString(tag) {
			return fmt.Errorf("invalid intent tag %q", tag)
		}
	}
	for key, value := range m.Metadata {
		if !identPattern.MatchString(key) || !validString(value, 4096) {
			return fmt.Errorf("invalid recipe metadata %q", key)
		}
	}
	for _, p := range m.SecretPurposes {
		if !identPattern.MatchString(p) {
			return fmt.Errorf("invalid secret purpose %q", p)
		}
	}
	keys := map[string]struct{}{}
	allStepKeys := map[string]struct{}{}
	declaredPaths := schemaPaths(m.InputSchema)
	for path := range schemaPaths(m.OutputSchema) {
		declaredPaths[path] = struct{}{}
	}
	for _, endpoint := range m.NetworkAccess.Endpoints {
		if err := validateTemplatePaths(endpoint, declaredPaths); err != nil {
			return err
		}
	}
	if len(m.Stages) == 0 || len(m.Stages) > maxRecipeStages {
		return errors.New("invalid stage count")
	}
	for _, s := range m.Stages {
		if s.StageKey == "" || !identPattern.MatchString(s.StageKey) || !validString(s.Title, 256) {
			return errors.New("invalid stage key")
		}
		if _, ok := keys[s.StageKey]; ok {
			return errors.New("duplicate stage key")
		}
		keys[s.StageKey] = struct{}{}
		if _, ok := risks[s.Risk]; !ok {
			return errors.New("invalid stage risk")
		}
		if _, ok := gates[s.Gate]; !ok {
			return errors.New("invalid stage gate")
		}
		if !riskGateAllowed(s.Risk, s.Gate) {
			return fmt.Errorf("risk %s does not allow gate %s", s.Risk, s.Gate)
		}
		if !sortedUnique(s.Effects) {
			return errors.New("stage effects must be sorted and unique")
		}
		isRollback := s.Kind == "cleanup" || strings.Contains(strings.ToLower(s.StageKey), "rollback") || len(s.RollbackSteps) > 0
		if isRollback && (s.Risk != "R4" || s.Gate != "rollback" || len(s.Steps) != 0 || len(s.RollbackSteps) == 0) {
			return errors.New("rollback declarations require inert R4 rollback steps")
		}
		if isRollback && len(s.DependsOn) != 1 {
			return errors.New("rollback declarations require one forward dependency")
		}
		if _, ok := initialStepKinds[s.Kind]; !ok {
			return fmt.Errorf("unknown stage kind %q", s.Kind)
		}
		if s.TimeoutSeconds == 0 || s.TimeoutSeconds > maxRecipeTimeout || (len(s.Steps) == 0 && len(s.RollbackSteps) == 0) {
			return errors.New("stage requires timeout and steps")
		}
		for _, probe := range s.Probes {
			if probe != "http.probe" && probe != "tcp.probe" {
				return fmt.Errorf("unknown probe %q", probe)
			}
		}
		if len(unique(s.DependsOn)) != len(s.DependsOn) {
			return errors.New("duplicate stage dependency")
		}
		for _, d := range s.DependsOn {
			if d == s.StageKey {
				return errors.New("stage self dependency")
			}
		}
		if len(s.Steps) > 0 {
			for _, step := range s.Steps {
				if step.Kind != s.Kind {
					return errors.New("stage kind must match normal step kinds")
				}
			}
		} else if len(s.RollbackSteps) > 0 {
			for _, step := range s.RollbackSteps {
				if step.Kind != s.Kind {
					return errors.New("stage kind must match rollback step kinds")
				}
			}
		}
		if err := steps(s.Steps, m.AllowedStepKinds, allStepKeys, s.Gate, m.NetworkAccess.Endpoints, m.SecretPurposes, declaredPaths); err != nil {
			return err
		}
		if err := steps(s.RollbackSteps, m.AllowedStepKinds, allStepKeys, s.Gate, m.NetworkAccess.Endpoints, m.SecretPurposes, declaredPaths); err != nil {
			return err
		}
		gate := deriveStepGate(append(append([]RecipeStep{}, s.Steps...), s.RollbackSteps...))
		if !equalStringSets(s.Effects, deriveStepEffects(s.Steps)) {
			return errors.New("stage effects do not match forward steps")
		}
		if gate == "" {
			gate = expectedGate(s.Kind)
		}
		if gate != "" && gate != s.Gate {
			return fmt.Errorf("stage gate %s does not satisfy step requirements %s", s.Gate, gate)
		}
		if len(allStepKeys) > maxRecipeSteps {
			return errors.New("too many aggregate recipe steps")
		}
	}
	for _, s := range m.Stages {
		for _, d := range s.DependsOn {
			if _, ok := keys[d]; !ok {
				return errors.New("unknown stage dependency")
			}
		}
	}
	if hasCycle(m.Stages) {
		return errors.New("stage dependency cycle")
	}
	return nil
}
func steps(in []RecipeStep, allowed []string, seen map[string]struct{}, gate string, endpoints []string, secretPurposes []string, declaredPaths map[string]struct{}) error {
	if len(in) > maxRecipeSteps {
		return errors.New("too many recipe steps")
	}
	for _, s := range in {
		if s.StepKey == "" || !identPattern.MatchString(s.StepKey) {
			return errors.New("invalid step key")
		}
		if _, ok := seen[s.StepKey]; ok {
			return errors.New("duplicate step key")
		}
		seen[s.StepKey] = struct{}{}
		if _, ok := initialStepKinds[s.Kind]; !ok {
			return fmt.Errorf("unknown step kind %q", s.Kind)
		}
		if !contains(allowed, s.Kind) {
			return fmt.Errorf("step kind %q is not in allowed_step_kinds", s.Kind)
		}
		if s.IdempotencyMarker == "" || s.TimeoutSeconds == 0 || s.TimeoutSeconds > maxRecipeTimeout {
			return errors.New("step requires timeout/idempotency marker")
		}
		if !identPattern.MatchString(s.IdempotencyMarker) {
			return errors.New("invalid idempotency marker")
		}
		if s.Template == nil && s.ArtifactRef == nil {
			return fmt.Errorf("step %s requires declarative template or artifact_ref", s.StepKey)
		}
		if !sortedUnique(s.EnvRefs) || !sortedUnique(s.SecretRefs) || !sortedUnique(s.NetworkGrants) {
			return errors.New("step reference lists must be sorted and unique")
		}
		for _, ref := range append(append([]string{}, s.EnvRefs...), s.SecretRefs...) {
			if !identPattern.MatchString(ref) && !placeholderPattern.MatchString(ref) {
				return fmt.Errorf("invalid step reference %q", ref)
			}
		}
		for _, grant := range s.NetworkGrants {
			if !contains(endpoints, grant) {
				return fmt.Errorf("network grant %q is not declared", grant)
			}
		}
		for _, secret := range s.SecretRefs {
			if !contains(secretPurposes, secret) {
				return fmt.Errorf("secret reference %q is not declared", secret)
			}
		}
		for _, code := range s.AllowedExitCodes {
			if code < 0 || code > 255 {
				return errors.New("exit code outside 0..255")
			}
		}
		if len(s.Argv) > 64 || len(s.EnvRefs) > 64 || len(s.SecretRefs) > 64 || len(s.NetworkGrants) > 64 || !sortedUniqueInts(s.AllowedExitCodes) {
			return errors.New("step list limits or exit ordering invalid")
		}
		if s.Root && gate != "remote_privileged_execution" {
			return errors.New("root script requires remote_privileged_execution")
		}
		if err := validateStepFields(s); err != nil {
			return fmt.Errorf("step %s (%s): %w", s.StepKey, s.Kind, err)
		}
		if err := validateTemplatePaths(s.Template, declaredPaths); err != nil {
			return err
		}
		if err := validateTemplatePaths(s.ArtifactRef, declaredPaths); err != nil {
			return err
		}
		for _, field := range []interface{}{s.Argv, s.CWD, s.EnvRefs, s.SecretRefs, s.NetworkGrants, s.Redaction, s.Postcondition} {
			if err := validateTemplatePaths(field, declaredPaths); err != nil {
				return err
			}
		}
		if err := rejectUnsafe(s.Template, "template."); err != nil {
			return err
		}
		if err := rejectUnsafe(s.ArtifactRef, "artifact_ref."); err != nil {
			return err
		}
		if err := rejectUnsafe(s.Postcondition, "postcondition."); err != nil {
			return err
		}
	}
	return nil
}

func validateStepFields(s RecipeStep) error {
	if err := validateTemplate(s.Template, "template."); err != nil {
		return err
	}
	if s.Kind == "script.run" {
		if len(s.Template) != 0 || len(s.ArtifactRef) == 0 || !placeholderPair(s.ArtifactRef, "artifact_id", placeholderPattern, "digest", sha256PlaceholderPattern) {
			return errors.New("script.run requires artifact_id/digest placeholders")
		}
		if s.Interpreter != "/bin/sh" && s.Interpreter != "/bin/bash" && s.Interpreter != "/usr/bin/sh" && s.Interpreter != "/usr/bin/bash" {
			return errors.New("script.run interpreter is not allowlisted")
		}
		if len(s.Argv) == 0 || !validString(s.CWD, 256) || s.OutputLimit == 0 || len(s.AllowedExitCodes) == 0 || s.Redaction == nil || s.Postcondition == nil {
			return errors.New("script.run requires argv/cwd/output/exit/redaction/postcondition")
		}
		if s.OutputPolicy != "discard" && s.OutputPolicy != "capture" && s.OutputPolicy != "artifact" {
			return errors.New("script.run output_policy is not allowlisted")
		}
		if err := validateRedaction(s.Redaction); err != nil {
			return err
		}
		if err := validatePostcondition(s.Postcondition); err != nil {
			return err
		}
		for _, arg := range s.Argv {
			if !validString(arg, 256) {
				return errors.New("invalid script argv")
			}
		}
		if len(s.ArtifactRef) > 2 || s.OutputLimit > maxRecipeOutput {
			return errors.New("script.run artifact_ref has unknown fields")
		}
		return nil
	}
	if s.Kind == "secret.provision" {
		if len(s.ArtifactRef) > 0 || s.Interpreter != "" || len(s.Argv) > 0 || s.CWD != "" || len(s.EnvRefs) > 0 || len(s.SecretRefs) != 1 || s.Root || len(s.AllowedExitCodes) > 0 || s.OutputLimit > 0 || s.Redaction != nil || s.Postcondition != nil || len(s.NetworkGrants) > 0 || (s.OutputPolicy != "" && s.OutputPolicy != "discard") || len(s.Template) != 1 || s.Template["delivery"] != "target_secure_parameter" {
			return errors.New("secret.provision requires one secret ref and target_secure_parameter delivery")
		}
		return nil
	}
	if s.Kind == "auth.external" {
		if len(s.ArtifactRef) > 0 || s.Interpreter != "" || len(s.Argv) > 0 || s.CWD != "" || len(s.EnvRefs) > 0 || len(s.SecretRefs) > 0 || s.Root || len(s.AllowedExitCodes) > 0 || s.OutputLimit > 0 || s.Redaction != nil || s.Postcondition != nil || len(s.NetworkGrants) > 0 || (s.OutputPolicy != "" && s.OutputPolicy != "discard") || exactTemplate(s.Template, []string{"provider", "status"}) != nil {
			return errors.New("auth.external requires provider/status placeholders without secrets")
		}
		return nil
	}
	if len(s.ArtifactRef) > 0 || s.Interpreter != "" || len(s.Argv) > 0 || s.CWD != "" || len(s.EnvRefs) > 0 || len(s.SecretRefs) > 0 || s.Root || len(s.AllowedExitCodes) > 0 || s.OutputLimit > 0 || s.Redaction != nil {
		return errors.New("step has fields not allowed for its kind")
	}
	if s.Postcondition != nil && s.Kind != "container.apply" && s.Kind != "systemd.apply" && s.Kind != "cleanup" {
		return errors.New("postcondition is not allowed for this kind")
	}
	switch s.Kind {
	case "target.inspect":
		return exactTemplate(s.Template, nil)
	case "compute.provision":
		return exactTemplate(s.Template, []string{"ami_parameter", "architecture", "infrastructure_profile_id", "instance_type", "management_transport", "public_inbound", "public_ip", "region", "volume_gib"})
	case "compute.destroy":
		return exactTemplate(s.Template, []string{"resource_ref"})
	case "source.fetch":
		return exactTemplate(s.Template, []string{"source_commit"})
	case "artifact.upload":
		return exactTemplate(s.Template, []string{"artifact_ref", "destination_ref"})
	case "container.apply":
		return exactTemplate(s.Template, []string{"container_port", "host_address", "host_port", "image_digest", "name", "restart_policy"})
	case "http.probe", "tcp.probe":
		return exactTemplate(s.Template, []string{"probe_ref"})
	case "package.ensure":
		return exactTemplate(s.Template, []string{"package_ref"})
	case "systemd.apply":
		return exactTemplate(s.Template, []string{"artifact_ref", "unit_ref"})
	case "file.put":
		return exactTemplate(s.Template, []string{"artifact_ref", "path_ref"})
	case "artifact.collect":
		return exactTemplate(s.Template, []string{"artifact_ref", "paths_ref"})
	case "cleanup":
		return exactTemplate(s.Template, []string{"resource_ref"})
	}
	return nil
}
func validateRedaction(redaction map[string]interface{}) error {
	if len(redaction) == 0 {
		return errors.New("redaction policy cannot be empty")
	}
	patterns, ok := redaction["patterns"].([]interface{})
	replace, replaceOK := redaction["replace"].(string)
	if !ok || len(patterns) == 0 || len(patterns) > 32 || !replaceOK || !validString(replace, 128) {
		return errors.New("redaction patterns required")
	}
	for _, p := range patterns {
		if s, ok := p.(string); !ok || !validString(s, 256) {
			return errors.New("invalid redaction pattern")
		}
	}
	for k := range redaction {
		if k != "patterns" && k != "replace" {
			return fmt.Errorf("unknown redaction field %s", k)
		}
	}
	return nil
}
func validatePostcondition(post map[string]interface{}) error {
	if len(post) != 2 {
		return errors.New("postcondition requires type/value")
	}
	typ, ok := post["type"].(string)
	if !ok || typ != "exit_code" && typ != "file_exists" && typ != "service_active" && typ != "http_status" {
		return errors.New("invalid postcondition type")
	}
	value, ok := post["value"].(string)
	if !ok || !placeholderPattern.MatchString(value) {
		return errors.New("postcondition value must be placeholder")
	}
	return nil
}

func placeholderPair(values map[string]interface{}, key1 string, re1 *regexp.Regexp, key2 string, re2 *regexp.Regexp) bool {
	if len(values) != 2 {
		return false
	}
	a, okA := values[key1].(string)
	b, okB := values[key2].(string)
	return okA && okB && re1.MatchString(a) && re2.MatchString(b)
}
func exactTemplate(values map[string]interface{}, keys []string) error {
	if values == nil {
		return errors.New("template required")
	}
	if len(values) != len(keys) {
		if len(keys) == 0 && len(values) == 0 {
			return nil
		}
		return errors.New("template fields do not match step kind")
	}
	for _, k := range keys {
		v, ok := values[k].(string)
		if !ok || !placeholderPattern.MatchString(v) {
			return fmt.Errorf("template field %s requires placeholder", k)
		}
	}
	return nil
}
func validateTemplate(values map[string]interface{}, path string) error {
	if values == nil {
		return nil
	}
	for k, v := range values {
		if !identPattern.MatchString(k) {
			return fmt.Errorf("invalid template key %s", path+k)
		}
		if err := validateTemplateValue(v, path+k+"."); err != nil {
			return err
		}
	}
	return nil
}
func validateTemplateValue(value interface{}, path string) error {
	switch v := value.(type) {
	case string:
		if !placeholderPattern.MatchString(v) {
			return fmt.Errorf("template value %s must be a placeholder", path)
		}
	case map[string]interface{}:
		return validateTemplate(v, path)
	case []interface{}:
		for i, x := range v {
			if err := validateTemplateValue(x, fmt.Sprintf("%s%d.", path, i)); err != nil {
				return err
			}
		}
	case float64, bool, nil:
	default:
		return fmt.Errorf("invalid template value %s", path)
	}
	return nil
}
func schemaPaths(raw json.RawMessage) map[string]struct{} {
	out := map[string]struct{}{}
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return out
	}
	var walk func(map[string]interface{}, string, map[string]bool)
	walk = func(node map[string]interface{}, prefix string, seen map[string]bool) {
		if ref, ok := node["$ref"].(string); ok {
			if seen[ref] {
				return
			}
			if target, ok := resolveSchemaRef(root, ref); ok {
				seen[ref] = true
				walk(target, prefix, seen)
				delete(seen, ref)
			}
		}
		props, _ := node["properties"].(map[string]interface{})
		for key, value := range props {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			out[path] = struct{}{}
			if child, ok := value.(map[string]interface{}); ok {
				walk(child, path, seen)
			}
		}
	}
	walk(root, "", map[string]bool{})
	return out
}
func validateTemplatePaths(value interface{}, paths map[string]struct{}) error {
	switch v := value.(type) {
	case map[string]interface{}:
		for _, child := range v {
			if err := validateTemplatePaths(child, paths); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range v {
			if err := validateTemplatePaths(child, paths); err != nil {
				return err
			}
		}
	case string:
		for _, match := range embeddedPlaceholderPattern.FindAllStringSubmatch(v, -1) {
			if _, ok := paths[match[1]]; !ok {
				return fmt.Errorf("template placeholder %s is not declared by schema", match[0])
			}
		}
	}
	return nil
}

func riskGateAllowed(risk, gate string) bool {
	switch risk {
	case "R0", "R1":
		return gate == "none"
	case "R2":
		return gate == "resource_purchase" || gate == "secret_access" || gate == "external_auth" || gate == "remote_execution" || gate == "remote_privileged_execution" || gate == "repository_write"
	case "R3":
		return gate == "public_network_exposure" || gate == "dns_change" || gate == "tls_certificate_issue" || gate == "data_migration" || gate == "production_cutover"
	case "R4":
		return gate == "service_destroy" || gate == "rollback"
	default:
		return false
	}
}

func validString(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validSchemaRef(ref string) bool {
	if ref == "" {
		return true
	}
	if len(ref) > 256 || !strings.HasPrefix(ref, "#/") || strings.Contains(ref, "\\") || strings.Contains(ref, "..") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	if len(parts) != 2 || (parts[0] != "$defs" && parts[0] != "definitions") || !identPattern.MatchString(parts[1]) {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || r == '#' {
			return false
		}
	}
	return true
}

func validEndpoint(endpoint string) bool {
	if endpoint == SourceOCIRegistryGrant || endpoint == PublicHTTPSEgressGrant {
		return true
	}
	if strings.HasPrefix(endpoint, "target_local:") && len(endpoint) > len("target_local:") {
		return true
	}
	if len(endpoint) > 256 || strings.ContainsAny(endpoint, "*?#@") || !grantPattern.MatchString(endpoint) || !strings.Contains(endpoint, "${") {
		return false
	}
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "egress:"), "https://")
	if slash := strings.IndexByte(host, '/'); slash >= 0 {
		host = host[:slash]
	}
	if colon := strings.LastIndexByte(host, ':'); colon >= 0 {
		host = host[:colon]
	}
	if _, ok := allowedEndpointHosts[host]; !ok {
		return false
	}
	for i := 0; i < len(endpoint); {
		start := strings.Index(endpoint[i:], "${")
		if start < 0 {
			break
		}
		start += i
		end := strings.IndexByte(endpoint[start:], '}')
		if end < 0 || !placeholderPattern.MatchString(endpoint[start:start+end+1]) {
			return false
		}
		i = start + end + 1
	}
	return true
}
func hasCycle(stages []RecipeStage) bool {
	state := map[string]int{}
	by := map[string]RecipeStage{}
	for _, s := range stages {
		by[s.StageKey] = s
	}
	var visit func(string) bool
	visit = func(k string) bool {
		if state[k] == 1 {
			return true
		}
		if state[k] == 2 {
			return false
		}
		state[k] = 1
		for _, d := range by[k].DependsOn {
			if visit(d) {
				return true
			}
		}
		state[k] = 2
		return false
	}
	for _, s := range stages {
		if visit(s.StageKey) {
			return true
		}
	}
	return false
}

func expectedGate(kind string) string {
	switch kind {
	case "target.inspect", "http.probe", "tcp.probe":
		return "none"
	case "package.ensure":
		return "remote_privileged_execution"
	case "compute.provision":
		return "resource_purchase"
	case "compute.destroy":
		return "service_destroy"
	case "container.apply":
		return "remote_privileged_execution"
	case "systemd.apply", "script.run":
		return "remote_execution"
	case "source.fetch":
		return "remote_execution"
	case "cleanup":
		return "rollback"
	case "secret.provision":
		return "secret_access"
	case "auth.external":
		return "external_auth"
	default:
		return ""
	}
}
func deriveStepGate(steps []RecipeStep) string {
	root, secret := false, false
	for _, s := range steps {
		if s.Root {
			root = true
		}
		if len(s.SecretRefs) > 0 {
			secret = true
		}
	}
	if root {
		return "remote_privileged_execution"
	}
	if secret {
		return "secret_access"
	}
	return ""
}
func deriveStepEffects(steps []RecipeStep) []string {
	seen := map[string]bool{}
	for _, step := range steps {
		if step.Root {
			seen["remote_privileged_execution"] = true
		}
		if len(step.SecretRefs) > 0 {
			seen["secret_access"] = true
		}
		if step.Kind == "auth.external" {
			seen["external_auth"] = true
		}
	}
	out := make([]string, 0, len(seen))
	for effect := range seen {
		out = append(out, effect)
	}
	sort.Strings(out)
	return out
}
func equalStringSets(a, b []string) bool {
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
func ContentDigest(content []byte) string {
	value, err := canonicalValue(content)
	if err != nil {
		return ""
	}
	raw, ok := value.(map[string]interface{})
	if !ok {
		return ""
	}
	delete(raw, "content_digest")
	b, e := json.Marshal(raw)
	if e != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func canonicalValue(content []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	v, err := readCanonicalValue(dec)
	if err != nil {
		return nil, err
	}
	if err := checkJSONLimits(v, 0, new(int)); err != nil {
		return nil, err
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing JSON")
		}
		return nil, err
	}
	return v, nil
}
func checkJSONLimits(value interface{}, depth int, nodes *int) error {
	if depth > maxRecipeDepth {
		return errors.New("JSON depth cap exceeded")
	}
	*nodes = *nodes + 1
	if *nodes > maxRecipeNodes {
		return errors.New("JSON node cap exceeded")
	}
	switch v := value.(type) {
	case map[string]interface{}:
		for _, child := range v {
			if err := checkJSONLimits(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range v {
			if err := checkJSONLimits(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}
func readCanonicalValue(dec *json.Decoder) (interface{}, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			m := map[string]interface{}{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key := kt.(string)
				if _, exists := m[key]; exists {
					return nil, fmt.Errorf("duplicate key %q", key)
				}
				value, err := readCanonicalValue(dec)
				if err != nil {
					return nil, err
				}
				m[key] = value
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return m, nil
		case '[':
			a := []interface{}{}
			for dec.More() {
				value, err := readCanonicalValue(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return a, nil
		}
	}
	return tok, nil
}
func clone(m RecipeManifest) RecipeManifest {
	b, _ := json.Marshal(m)
	var c RecipeManifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&c)
	for i := range c.Stages {
		for j := range c.Stages[i].Steps {
			if m.Stages[i].Steps[j].Template != nil && c.Stages[i].Steps[j].Template == nil {
				c.Stages[i].Steps[j].Template = map[string]interface{}{}
			}
		}
		for j := range c.Stages[i].RollbackSteps {
			if m.Stages[i].RollbackSteps[j].Template != nil && c.Stages[i].RollbackSteps[j].Template == nil {
				c.Stages[i].RollbackSteps[j].Template = map[string]interface{}{}
			}
		}
	}
	return c
}
func schema(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]interface{}
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return errors.New("schema must be object")
	}
	return validateSchemaDocument(obj)
}

var supportedSchemaKeywords = map[string]struct{}{
	"$ref": {}, "$defs": {}, "definitions": {}, "type": {}, "properties": {}, "items": {},
	"required": {}, "additionalProperties": {}, "enum": {}, "const": {},
	"minLength": {}, "maxLength": {}, "pattern": {}, "minimum": {}, "maximum": {},
}

func validateSchemaDocument(root map[string]interface{}) error {
	return validateSchemaNode(root, root, 0, map[string]bool{})
}

func validateSchemaNode(node map[string]interface{}, root map[string]interface{}, depth int, stack map[string]bool) error {
	if depth > 16 {
		return errors.New("schema depth cap exceeded")
	}
	for key := range node {
		if _, ok := supportedSchemaKeywords[key]; !ok {
			return fmt.Errorf("unsupported schema keyword %q", key)
		}
	}
	if ref, ok := node["$ref"].(string); ok {
		if !validSchemaRef(ref) {
			return errors.New("invalid schema ref")
		}
		target, ok := resolveSchemaRef(root, ref)
		if !ok {
			return errors.New("unresolved schema ref")
		}
		if stack[ref] {
			return errors.New("cyclic schema ref")
		}
		stack[ref] = true
		if err := validateSchemaNode(target, root, depth+1, stack); err != nil {
			return err
		}
		delete(stack, ref)
	}
	if typ, ok := node["type"].(string); ok {
		switch typ {
		case "object", "array", "string", "number", "integer", "boolean", "null":
		default:
			return errors.New("unsupported schema type")
		}
	} else if _, exists := node["type"]; exists {
		return errors.New("schema type must be string")
	}
	for _, defsKey := range []string{"$defs", "definitions"} {
		if defs, ok := node[defsKey].(map[string]interface{}); ok {
			if err := validateSchemaDefinitions(defs, root, depth, stack); err != nil {
				return err
			}
		} else if _, exists := node[defsKey]; exists {
			return errors.New("schema definitions must be object")
		}
	}
	if props, ok := node["properties"].(map[string]interface{}); ok {
		if len(props) > 128 {
			return errors.New("too many schema properties")
		}
		for key, v := range props {
			if !identPattern.MatchString(key) {
				return errors.New("invalid schema property")
			}
			child, ok := v.(map[string]interface{})
			if !ok {
				return errors.New("schema property must be object")
			}
			if err := validateSchemaNode(child, root, depth+1, stack); err != nil {
				return err
			}
		}
	} else if _, exists := node["properties"]; exists {
		return errors.New("schema properties must be object")
	}
	if items, ok := node["items"].(map[string]interface{}); ok {
		if err := validateSchemaNode(items, root, depth+1, stack); err != nil {
			return err
		}
	} else if _, exists := node["items"]; exists {
		return errors.New("schema items must be object")
	}
	if req, ok := node["required"].([]interface{}); ok {
		props, _ := node["properties"].(map[string]interface{})
		seen := map[string]bool{}
		for _, v := range req {
			s, ok := v.(string)
			if !ok || !identPattern.MatchString(s) || seen[s] {
				return errors.New("invalid schema required")
			}
			seen[s] = true
			if props != nil {
				if _, exists := props[s]; !exists {
					return fmt.Errorf("required schema property %q is not declared", s)
				}
			}
		}
	} else if _, exists := node["required"]; exists {
		return errors.New("schema required must be array")
	}
	if additional, ok := node["additionalProperties"]; ok {
		switch value := additional.(type) {
		case bool:
		case map[string]interface{}:
			if err := validateSchemaNode(value, root, depth+1, stack); err != nil {
				return err
			}
		default:
			return errors.New("schema additionalProperties must be boolean or object")
		}
	}
	if enum, ok := node["enum"].([]interface{}); ok {
		if len(enum) == 0 {
			return errors.New("schema enum must not be empty")
		}
	} else if _, exists := node["enum"]; exists {
		return errors.New("schema enum must be array")
	}
	for _, key := range []string{"minLength", "maxLength", "minimum", "maximum"} {
		if value, ok := node[key]; ok {
			if _, ok := value.(float64); !ok {
				return fmt.Errorf("schema %s must be number", key)
			}
		}
	}
	for _, key := range []string{"pattern"} {
		if value, ok := node[key]; ok {
			pattern, ok := value.(string)
			if !ok || len(pattern) > 256 {
				return errors.New("schema pattern must be string")
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return errors.New("invalid schema pattern")
			}
		}
	}
	return nil
}

func validateSchemaDefinitions(defs map[string]interface{}, root map[string]interface{}, depth int, stack map[string]bool) error {
	if len(defs) > 128 {
		return errors.New("too many schema definitions")
	}
	for key, value := range defs {
		if !identPattern.MatchString(key) {
			return errors.New("invalid schema definition name")
		}
		child, ok := value.(map[string]interface{})
		if !ok {
			return errors.New("schema definition must be object")
		}
		if err := validateSchemaNode(child, root, depth+1, stack); err != nil {
			return err
		}
	}
	return nil
}

func resolveSchemaRef(root map[string]interface{}, ref string) (map[string]interface{}, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	current := interface{}(root)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		obj, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	result, ok := current.(map[string]interface{})
	return result, ok
}
func sortedUnique(v []string) bool { return sort.StringsAreSorted(v) && len(unique(v)) == len(v) }
func sortedUniqueInts(v []int) bool {
	for i := 1; i < len(v); i++ {
		if v[i] <= v[i-1] {
			return false
		}
	}
	return true
}
func unique(v []string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, x := range v {
		m[x] = struct{}{}
	}
	return m
}
func contains(v []string, x string) bool {
	for _, a := range v {
		if a == x {
			return true
		}
	}
	return false
}
func subset(req []string, w map[string]struct{}) bool {
	for _, x := range req {
		if _, ok := w[x]; !ok {
			return false
		}
	}
	return true
}

var unsafeKeys = map[string]struct{}{"inline_shell": {}, "shell": {}, "command": {}, "commands": {}, "secret": {}, "secret_value": {}, "script": {}}

func rejectUnsafe(v interface{}, path string) error { return rejectUnsafeAt(v, path, false) }
func rejectUnsafeAt(v interface{}, path string, network bool) error {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, c := range x {
			if _, bad := unsafeKeys[strings.ToLower(k)]; bad {
				return fmt.Errorf("unsafe recipe field %s", path+k)
			}
			childNetwork := network || strings.EqualFold(k, "network_access") || strings.EqualFold(k, "network_grants")
			if s, ok := c.(string); ok && strings.Contains(s, "://") && !childNetwork {
				return fmt.Errorf("URL outside network_access metadata: %s", path+k)
			}
			if e := rejectUnsafeAt(c, path+k+".", childNetwork); e != nil {
				return e
			}
		}
	case []interface{}:
		for i, c := range x {
			if s, ok := c.(string); ok && strings.Contains(s, "://") && !network {
				return fmt.Errorf("URL outside network_access metadata: %s", path)
			}
			if e := rejectUnsafeAt(c, fmt.Sprintf("%s%d.", path, i), network); e != nil {
				return e
			}
		}
	case string:
		if strings.Contains(x, "://") && !network {
			return fmt.Errorf("URL outside network_access metadata: %s", path)
		}
	}
	return nil
}
