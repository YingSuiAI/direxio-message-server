// Package executionplanning owns the trusted, server-side execution.v2
// analysis and plan compiler boundary. It never executes a provider action.
package executionplanning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
)

var ErrNotReady = errors.New("execution planning: not ready")
var ErrUncertain = errors.New("execution planning: blocking uncertainty")

const executionDeploymentNamespaceName = "https://dirextalk.io/namespaces/execution-deployment/v2"

var executionDeploymentNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte(executionDeploymentNamespaceName))

type AnalysisStore interface {
	CreateAnalysis(context.Context, storage.AnalysisCreateRequest) (coreexecution.ProjectAnalysis, error)
}
type PlanStore interface {
	CreatePlan(context.Context, storage.ExecutionPlanCreate) (coreexecution.ExecutionPlan, error)
	GetCurrentPlan(context.Context, string, string) (coreexecution.ExecutionPlan, error)
}
type SourceResolver interface {
	ResolveSource(context.Context, string, string, SourceInput) (SourceFacts, error)
}
type TargetResolver interface {
	ResolveTarget(context.Context, string, string, uint64) (coreexecution.ExecutionTarget, error)
}
type ArtifactResolver interface {
	ResolveArtifact(context.Context, string, string, coreexecution.Digest) error
}

// ExecutionSecretResolver resolves owner-scoped execution-secret metadata.
// It is intentionally separate from AWS credential/catalog resolution: AI
// provider keys are execution secrets and must never be looked up as AWS
// credentials.
type ExecutionSecretResolver interface {
	ResolveCredential(context.Context, string, coreexecution.CredentialRef) error
}

// ExecutionSecretProviderResolver binds the plan's provider choice to the
// exact encrypted-secret metadata revision. A resolver that can prove only a
// credential reference is insufficient for AI plans because the same opaque
// key shape must not be relabeled as a different provider.
type ExecutionSecretProviderResolver interface {
	ExecutionSecretResolver
	ResolveExecutionSecretProvider(context.Context, string, coreexecution.CredentialRef) (string, error)
}

// CredentialResolver is retained as a source-compatible alias for callers
// while the service composition migrates to ExecutionSecrets.
type CredentialResolver = ExecutionSecretResolver
type PlanRevisionWriter interface {
	RevisePlan(context.Context, string, coreexecution.ExecutionPlan, uint64, string) (coreexecution.ExecutionPlan, error)
}

type SourceInput struct {
	Kind               string               `json:"kind"`
	Location           string               `json:"location"`
	Commit             string               `json:"commit"`
	ArtifactID         string               `json:"artifact_id,omitempty"`
	ArtifactDigest     coreexecution.Digest `json:"artifact_digest,omitempty"`
	CredentialRef      string               `json:"credential_ref,omitempty"`
	CredentialRevision uint64               `json:"credential_revision,omitempty"`
	Immutable          bool                 `json:"immutable"`
}
type SourceFacts struct {
	Analysis              coreexecution.ProjectAnalysis
	BlockingUncertainties []string
}
type BindingFacts struct {
	ObservationRef coreexecution.TargetObservationRef
	StepBindings   map[string]coreexecution.ExecutionStep
	SecretRefs     map[string]coreexecution.CredentialRef
	Placement      coreexecution.PlacementRecommendation
	Artifacts      []coreexecution.ArtifactRef
}
type StepBindingResolver interface {
	ResolveBindings(context.Context, string, agentembedded.ExecutionV2PlanCreateRequest, agentrecipes.RecipeManifest, coreexecution.ExecutionTarget) (BindingFacts, error)
}

type Config struct {
	AnalysisStore    AnalysisStore
	PlanStore        PlanStore
	RevisionWriter   PlanRevisionWriter
	Sources          SourceResolver
	Targets          TargetResolver
	Artifacts        ArtifactResolver
	Credentials      CredentialResolver
	ExecutionSecrets ExecutionSecretResolver
	Bindings         StepBindingResolver
	Executors        ExecutorSealer
	Recipes          *agentrecipes.Registry
	Now              func() time.Time
}

type Service struct{ cfg Config }

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg}
}
func (s *Service) Ready() bool {
	return s != nil && s.cfg.AnalysisStore != nil && s.cfg.PlanStore != nil && s.cfg.Sources != nil && s.cfg.Targets != nil && s.cfg.Recipes != nil
}
func (s *Service) PlanReady() bool {
	return s.Ready() && s.cfg.RevisionWriter != nil && s.cfg.Bindings != nil && s.cfg.Executors != nil
}

func (s *Service) Analyze(ctx context.Context, owner string, req agentembedded.ExecutionV2AnalyzeRequest) (coreexecution.ProjectAnalysis, error) {
	params := map[string]any{
		"project_id": req.ProjectID, "analysis_id": uuid.NewSHA1(uuid.Nil, []byte(owner+"\x00"+req.IdempotencyKey)).String(),
		"idempotency_key": req.IdempotencyKey,
		"source": map[string]any{
			"kind": req.Source.Kind, "location": req.Source.Location, "commit": req.Source.Commit,
			"artifact_id": req.Source.ArtifactID, "artifact_digest": req.Source.ArtifactDigest,
			"credential_ref": req.Source.CredentialRef, "credential_revision": req.Source.CredentialRevision,
			"immutable": req.Source.Immutable,
		},
	}
	v, err := s.analyzeAny(ctx, owner, params)
	if err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	return v.(coreexecution.ProjectAnalysis), nil
}
func (s *Service) analyzeAny(ctx context.Context, owner string, params map[string]any) (any, error) {
	if s == nil || s.cfg.AnalysisStore == nil || s.cfg.Sources == nil {
		return nil, ErrNotReady
	}
	projectID, analysisID, idem, err := ids(params, "project_id", "analysis_id", "idempotency_key")
	if err != nil {
		return nil, err
	}
	source, err := sourceInput(params["source"])
	if err != nil {
		return nil, err
	}
	facts, err := s.cfg.Sources.ResolveSource(ctx, owner, projectID, source)
	if err != nil {
		return nil, err
	}
	a := facts.Analysis
	a.AnalysisID, a.ProjectID, a.Digest, a.Revision = analysisID, projectID, "", 1
	a.BlockingUncertainties = mergeStrings(a.BlockingUncertainties, facts.BlockingUncertainties)
	if a.Source.Kind != source.Kind || a.Source.Location != source.Location ||
		a.Source.Commit != source.Commit || a.Source.ArtifactID != source.ArtifactID ||
		!a.Source.Immutable {
		return nil, ErrUncertain
	}
	return s.cfg.AnalysisStore.CreateAnalysis(ctx, storage.AnalysisCreateRequest{OwnerID: owner, Analysis: a, IdempotencyID: idem})
}

func (s *Service) Compile(ctx context.Context, owner string, req agentembedded.ExecutionV2PlanCreateRequest) (coreexecution.ExecutionPlan, error) {
	if s == nil || !s.Ready() || s.cfg.Bindings == nil || s.cfg.Executors == nil {
		return coreexecution.ExecutionPlan{}, ErrNotReady
	}
	target, err := s.cfg.Targets.ResolveTarget(ctx, owner, req.TargetID, req.TargetRevision)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	aiConfiguration, err := s.resolveAIConfiguration(ctx, owner, req.AIConfiguration)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	req.AIConfiguration = aiConfiguration
	recipe, err := s.resolveRecipeRequest(req, target)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	b, err := s.cfg.Bindings.ResolveBindings(ctx, owner, req, recipe, target)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if err := bindAISecret(&b, aiConfiguration); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	params := map[string]any{"project_id": req.ProjectID, "analysis_id": req.AnalysisID, "idempotency_key": req.IdempotencyKey, "target_id": req.TargetID, "target_revision": req.TargetRevision, "purpose": string(req.Purpose), "recipe_id": recipe.ID, "recipe_version": recipe.Version, "recipe_digest": recipe.ContentDigest, "observation_ref": b.ObservationRef, "step_bindings": b.StepBindings, "secret_refs": b.SecretRefs, "artifacts": b.Artifacts, "placement": b.Placement, "ai_configuration": aiConfiguration}
	v, err := s.compileAny(ctx, owner, params)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	return v.(coreexecution.ExecutionPlan), nil
}
func (s *Service) compileAny(ctx context.Context, owner string, params map[string]any) (any, error) {
	if s == nil || !s.Ready() {
		return nil, ErrNotReady
	}
	analysisID, _, idem, err := ids(params, "analysis_id", "", "idempotency_key")
	if err != nil {
		return nil, err
	}
	var aid = analysisID
	analysis, err := analysisStoreAnalysis(ctx, s.cfg.AnalysisStore, owner, aid)
	if err != nil {
		return nil, err
	}
	if len(analysis.BlockingUncertainties) > 0 {
		return nil, fmt.Errorf("%w: analysis has unresolved blockers", ErrUncertain)
	}
	targetID, err := requiredString(params, "target_id")
	if err != nil {
		return nil, err
	}
	targetRev, err := requiredUint(params, "target_revision")
	if err != nil {
		return nil, err
	}
	target, err := s.cfg.Targets.ResolveTarget(ctx, owner, targetID, targetRev)
	if err != nil {
		return nil, err
	}
	if target.ID != targetID || target.Revision != targetRev || !target.Digest.Valid() {
		return nil, ErrUncertain
	}
	purpose := coreexecution.PlanPurpose(stringOr(params, "purpose"))
	if purpose == "" {
		purpose = coreexecution.PurposeService
	}
	if purpose != coreexecution.PurposeJob && purpose != coreexecution.PurposeService {
		return nil, errors.New("invalid purpose")
	}
	recipe, err := s.resolveRecipe(params, target)
	if err != nil {
		return nil, err
	}
	bindings, err := stepBindings(params["step_bindings"])
	if err != nil {
		return nil, err
	}
	if s.cfg.Artifacts != nil {
		for _, a := range targetArtifacts(bindings) {
			if err := s.cfg.Artifacts.ResolveArtifact(ctx, owner, a.ID, a.Digest); err != nil {
				return nil, err
			}
		}
	}
	if s.cfg.Credentials != nil {
		for _, c := range target.CredentialRefs {
			if err := s.cfg.Credentials.ResolveCredential(ctx, owner, c); err != nil {
				return nil, err
			}
		}
	}
	secrets := map[string]coreexecution.CredentialRef{}
	if raw, ok := params["secret_refs"].(map[string]coreexecution.CredentialRef); ok {
		secrets = raw
	}
	stages, err := agentrecipes.Compile(recipe, agentrecipes.CompileContext{
		TargetID:        target.ID,
		TargetRevision:  target.Revision,
		TargetDigest:    target.Digest,
		ObservationRef:  observationRef(params["observation_ref"]),
		StepBindings:    bindings,
		SecretRefs:      secrets,
		AIConfiguration: aiConfigurationFromParams(params["ai_configuration"]),
	})
	if err != nil {
		return nil, err
	}
	if stages, err = agentrecipes.AddAIAuthorizationStages(stages, aiConfigurationFromParams(params["ai_configuration"])); err != nil {
		return nil, err
	}
	planID := deterministicPlanID(owner, idem)
	sealed, err := s.cfg.Executors.SealExecutors(ctx, ExecutorSealRequest{OwnerID: owner, ProjectID: analysis.ProjectID, PlanID: planID, PlanRevision: 1, Observation: observationRef(params["observation_ref"]), Stages: stages})
	if err != nil {
		return nil, err
	}
	stages = sealed.Stages
	params["artifacts"] = mergeArtifacts(artifactsFromParams(params["artifacts"]), sealed.Artifacts)
	params["plan_id"] = planID
	params["plan_revision"] = uint64(1)
	plan, err := buildPlan(owner, analysis, target, stages, purpose, params, idem, s.cfg.Now())
	if err != nil {
		return nil, err
	}
	return s.cfg.PlanStore.CreatePlan(ctx, storage.ExecutionPlanCreate{OwnerID: owner, Analysis: analysis, Plan: plan, IdempotencyID: idem})
}

func (s *Service) Revise(ctx context.Context, owner string, req agentembedded.ExecutionV2PlanReviseRequest) (coreexecution.ExecutionPlan, error) {
	if s == nil || !s.PlanReady() {
		return coreexecution.ExecutionPlan{}, ErrNotReady
	}
	current, err := s.cfg.PlanStore.GetCurrentPlan(ctx, owner, req.PlanID)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if current.Revision != req.ExpectedRevision {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
	}
	analysis, err := analysisStoreAnalysis(ctx, s.cfg.AnalysisStore, owner, current.AnalysisID)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if len(analysis.BlockingUncertainties) > 0 {
		return coreexecution.ExecutionPlan{}, fmt.Errorf("%w: analysis has unresolved blockers", ErrUncertain)
	}
	target, err := s.cfg.Targets.ResolveTarget(ctx, owner, req.TargetID, req.TargetRevision)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	aiConfiguration, err := s.resolveAIConfiguration(ctx, owner, req.AIConfiguration)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	recipe, err := s.resolveRecipeRequest(agentembedded.ExecutionV2PlanCreateRequest{RecipeID: req.RecipeID, Intent: req.Intent, AIConfiguration: aiConfiguration}, target)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	b, err := s.cfg.Bindings.ResolveBindings(ctx, owner, agentembedded.ExecutionV2PlanCreateRequest{ProjectID: current.ProjectID, AnalysisID: current.AnalysisID, Intent: req.Intent, RecipeID: recipe.ID, TargetID: target.ID, TargetRevision: target.Revision, Purpose: req.Purpose, AIConfiguration: aiConfiguration, IdempotencyKey: req.IdempotencyKey}, recipe, target)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if err := bindAISecret(&b, aiConfiguration); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	stages, err := agentrecipes.Compile(recipe, agentrecipes.CompileContext{TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest, ObservationRef: b.ObservationRef, StepBindings: b.StepBindings, SecretRefs: b.SecretRefs, AIConfiguration: aiConfiguration})
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if stages, err = agentrecipes.AddAIAuthorizationStages(stages, aiConfiguration); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	sealed, err := s.cfg.Executors.SealExecutors(ctx, ExecutorSealRequest{OwnerID: owner, ProjectID: analysis.ProjectID, PlanID: current.ID, PlanRevision: current.Revision + 1, Observation: b.ObservationRef, Stages: stages})
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	plan, err := buildPlan(owner, analysis, target, sealed.Stages, req.Purpose, map[string]any{"recipe_id": recipe.ID, "recipe_version": recipe.Version, "recipe_digest": recipe.ContentDigest, "artifacts": mergeArtifacts(b.Artifacts, sealed.Artifacts), "placement": b.Placement, "plan_id": current.ID, "plan_revision": current.Revision + 1, "ai_configuration": aiConfiguration}, req.IdempotencyKey, s.cfg.Now())
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	return s.cfg.RevisionWriter.RevisePlan(ctx, owner, plan, req.ExpectedRevision, req.IdempotencyKey)
}
func analysisStoreAnalysis(ctx context.Context, st AnalysisStore, owner, id string) (coreexecution.ProjectAnalysis, error) {
	if x, ok := st.(interface {
		GetAnalysis(context.Context, string, string) (coreexecution.ProjectAnalysis, error)
	}); ok {
		return x.GetAnalysis(ctx, owner, id)
	}
	return coreexecution.ProjectAnalysis{}, ErrNotReady
}

func (s *Service) resolveRecipe(params map[string]any, target coreexecution.ExecutionTarget) (agentrecipes.RecipeManifest, error) {
	id, ok := params["recipe_id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return agentrecipes.RecipeManifest{}, errors.New("recipe_id is required")
	}
	version, ok := params["recipe_version"].(string)
	if !ok || version == "" {
		return agentrecipes.RecipeManifest{}, errors.New("recipe_version is required")
	}
	digest, ok := params["recipe_digest"].(string)
	if !ok || digest == "" {
		return agentrecipes.RecipeManifest{}, errors.New("recipe_digest is required")
	}
	return s.cfg.Recipes.ResolveExact(id, version, digest)
}
func (s *Service) resolveRecipeRequest(req agentembedded.ExecutionV2PlanCreateRequest, target coreexecution.ExecutionTarget) (agentrecipes.RecipeManifest, error) {
	if s.cfg.Recipes == nil || strings.TrimSpace(req.RecipeID) == "" {
		return agentrecipes.RecipeManifest{}, ErrNotReady
	}
	got, err := s.cfg.Recipes.Select(agentrecipes.SelectionQuery{Intent: req.Intent, TargetCapabilities: target.Capabilities, Limit: 3})
	if err != nil {
		return agentrecipes.RecipeManifest{}, err
	}
	for _, r := range got {
		if r.ID == req.RecipeID {
			return r, nil
		}
	}
	return agentrecipes.RecipeManifest{}, fmt.Errorf("recipe %q is not available for target", req.RecipeID)
}

func buildPlan(owner string, analysis coreexecution.ProjectAnalysis, target coreexecution.ExecutionTarget, stages []coreexecution.ExecutionStage, purpose coreexecution.PlanPurpose, params map[string]any, idem string, now time.Time) (coreexecution.ExecutionPlan, error) {
	id := deterministicPlanID(owner, idem)
	if supplied, ok := params["plan_id"].(string); ok && coreexecution.ValidateUUID(supplied) {
		id = supplied
	}
	revision := uint64(1)
	if supplied, ok := params["plan_revision"].(uint64); ok && supplied > 0 {
		revision = supplied
	}
	expires := now.Add(24 * time.Hour)
	p := coreexecution.ExecutionPlan{SchemaVersion: coreexecution.SchemaVersion, ID: id, Revision: revision, OwnerID: owner, ProjectID: analysis.ProjectID, AnalysisID: analysis.AnalysisID, Purpose: purpose, Stages: stages, Targets: []coreexecution.ExecutionTarget{target}, CreatedAt: now.UTC(), ExpiresAt: expires.UTC(), Status: coreexecution.PlanReady, Recipes: []coreexecution.RecipeRef{{ID: stringOr(params, "recipe_id"), Version: stringOr(params, "recipe_version"), Digest: coreexecution.Digest(stringOr(params, "recipe_digest"))}}}
	if configuration, ok := params["ai_configuration"].(*coreexecution.AIConfiguration); ok && configuration != nil {
		copy := *configuration
		p.AIConfiguration = &copy
	}
	if purpose == coreexecution.PurposeService {
		p.DeploymentID = deterministicDeploymentID(id)
	}
	if raw, ok := params["placement"]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &p.Placement)
		for _, option := range []coreexecution.PlacementOption{p.Placement.Minimum, p.Placement.Recommended, p.Placement.HighPerformance} {
			if !option.CostQuote.ExpiresAt.IsZero() && option.CostQuote.ExpiresAt.Before(p.ExpiresAt) {
				p.ExpiresAt = option.CostQuote.ExpiresAt.UTC()
			}
		}
	}
	if raw, ok := params["artifacts"].([]coreexecution.ArtifactRef); ok {
		p.Artifacts = append([]coreexecution.ArtifactRef(nil), raw...)
	}
	if _, err := p.NormalizeAt(now); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	p.Digest = ""
	return p.NormalizeAt(now)
}

func (s *Service) resolveAIConfiguration(ctx context.Context, owner string, configuration *coreexecution.AIConfiguration) (*coreexecution.AIConfiguration, error) {
	if configuration == nil {
		return nil, nil
	}
	normalized, err := configuration.Normalize()
	if err != nil {
		return nil, err
	}
	if normalized.Mode == coreexecution.AIAuthModeAPIKey {
		resolver := s.cfg.ExecutionSecrets
		if resolver == nil {
			// Compatibility for existing composition only; this field must be
			// wired to the execution-secret store, never an AWS credential store.
			resolver = s.cfg.Credentials
		}
		if resolver == nil {
			return nil, ErrNotReady
		}
		if err := resolver.ResolveCredential(ctx, owner, normalized.CredentialRef()); err != nil {
			return nil, err
		}
		providerResolver, ok := resolver.(ExecutionSecretProviderResolver)
		if !ok {
			return nil, ErrNotReady
		}
		provider, err := providerResolver.ResolveExecutionSecretProvider(ctx, owner, normalized.CredentialRef())
		if err != nil {
			return nil, err
		}
		if provider != normalized.Provider {
			return nil, coreexecution.ErrConflict
		}
	}
	return &normalized, nil
}

func bindAISecret(bindings *BindingFacts, configuration *coreexecution.AIConfiguration) error {
	if configuration == nil || configuration.Mode != coreexecution.AIAuthModeAPIKey {
		return nil
	}
	if bindings.SecretRefs == nil {
		bindings.SecretRefs = map[string]coreexecution.CredentialRef{}
	}
	want := configuration.CredentialRef()
	if current, exists := bindings.SecretRefs[coreexecution.AISecretPurposeProviderAPIKey]; exists && current != want {
		return coreexecution.ErrConflict
	}
	bindings.SecretRefs[coreexecution.AISecretPurposeProviderAPIKey] = want
	return nil
}

func aiConfigurationFromParams(raw any) *coreexecution.AIConfiguration {
	configuration, ok := raw.(*coreexecution.AIConfiguration)
	if !ok || configuration == nil {
		return nil
	}
	copy := *configuration
	return &copy
}

func deterministicPlanID(owner, idempotencyKey string) string {
	return uuid.NewSHA1(uuid.Nil, []byte(owner+"\x00"+idempotencyKey)).String()
}

func deterministicDeploymentID(planID string) string {
	if !coreexecution.ValidateUUID(strings.TrimSpace(planID)) {
		return ""
	}
	return uuid.NewSHA1(executionDeploymentNamespace, []byte(strings.TrimSpace(planID))).String()
}

func artifactsFromParams(raw any) []coreexecution.ArtifactRef {
	if artifacts, ok := raw.([]coreexecution.ArtifactRef); ok {
		return append([]coreexecution.ArtifactRef(nil), artifacts...)
	}
	return nil
}

func mergeArtifacts(groups ...[]coreexecution.ArtifactRef) []coreexecution.ArtifactRef {
	byID := map[string]coreexecution.ArtifactRef{}
	for _, group := range groups {
		for _, artifact := range group {
			if prior, exists := byID[artifact.ID]; exists && prior != artifact {
				// Preserve both values so plan normalization rejects the conflicting
				// immutable identity instead of silently choosing one.
				return append(append([]coreexecution.ArtifactRef(nil), group...), prior)
			}
			byID[artifact.ID] = artifact
		}
	}
	out := make([]coreexecution.ArtifactRef, 0, len(byID))
	for _, artifact := range byID {
		out = append(out, artifact)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func mergeStrings(groups ...[]string) []string {
	set := map[string]bool{}
	for _, group := range groups {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value != "" {
				set[value] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func ids(p map[string]any, first, second, idem string) (string, string, string, error) {
	a, e := requiredString(p, first)
	if e != nil {
		return "", "", "", e
	}
	b := ""
	if second != "" {
		b, e = requiredString(p, second)
		if e != nil {
			return "", "", "", e
		}
	}
	c, e := requiredString(p, idem)
	if e != nil {
		return "", "", "", e
	}
	if !coreexecution.ValidateUUID(a) || (b != "" && !coreexecution.ValidateUUID(b)) || !coreexecution.ValidateUUID(c) {
		return "", "", "", errors.New("invalid UUID")
	}
	return a, b, c, nil
}
func requiredString(p map[string]any, k string) (string, error) {
	v, ok := p[k].(string)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		return "", fmt.Errorf("%s is required", k)
	}
	return v, nil
}
func requiredUint(p map[string]any, k string) (uint64, error) {
	v, ok := p[k].(float64)
	if ok && v > 0 {
		return uint64(v), nil
	}
	if n, ok := p[k].(uint64); ok && n > 0 {
		return n, nil
	}
	return 0, fmt.Errorf("%s is required", k)
}
func stringOr(p map[string]any, k string) string { v, _ := p[k].(string); return strings.TrimSpace(v) }
func sourceInput(v any) (SourceInput, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return SourceInput{}, e
	}
	var x SourceInput
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	e = d.Decode(&x)
	if e != nil {
		return x, e
	}
	return x, nil
}
func stepBindings(v any) (map[string]coreexecution.ExecutionStep, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var x map[string]coreexecution.ExecutionStep
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(&x); e != nil || len(x) == 0 {
		return nil, errors.New("step_bindings are required")
	}
	return x, nil
}
func targetArtifacts(m map[string]coreexecution.ExecutionStep) []coreexecution.ArtifactRef {
	var out []coreexecution.ArtifactRef
	for _, s := range m {
		out = append(out, s.ArtifactRefs...)
	}
	return out
}
func observationRef(v any) coreexecution.TargetObservationRef {
	ref, _ := v.(coreexecution.TargetObservationRef)
	return ref
}
func digest(v any) coreexecution.Digest {
	h := sha256.Sum256([]byte(fmt.Sprint(v)))
	return coreexecution.Digest(hex.EncodeToString(h[:]))
}
