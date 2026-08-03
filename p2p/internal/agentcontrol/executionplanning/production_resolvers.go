package executionplanning

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

var (
	ErrSourceInvalid   = errors.New("execution planning: invalid source")
	ErrSourceIntegrity = errors.New("execution planning: source integrity mismatch")
)

type databaseTargetReader interface {
	GetTarget(context.Context, string, string, uint64) (coreexecution.ExecutionTarget, error)
}

// DatabaseTargetResolver is the production target lookup boundary. It always
// resolves the exact owner-scoped revision requested by the compiler; revision
// zero (the catalog's convenient "latest" query) is deliberately forbidden.
type DatabaseTargetResolver struct {
	store databaseTargetReader
}

func NewDatabaseTargetResolver(store *storage.DatabaseExecutionStore) *DatabaseTargetResolver {
	if store == nil {
		return nil
	}
	return &DatabaseTargetResolver{store: store}
}

func (r *DatabaseTargetResolver) ResolveTarget(
	ctx context.Context,
	owner string,
	targetID string,
	revision uint64,
) (coreexecution.ExecutionTarget, error) {
	owner = strings.TrimSpace(owner)
	targetID = strings.TrimSpace(targetID)
	if r == nil || r.store == nil || owner == "" || !coreexecution.ValidateUUID(targetID) || revision == 0 {
		return coreexecution.ExecutionTarget{}, coreexecution.ErrInvalid
	}
	target, err := r.store.GetTarget(ctx, owner, targetID, revision)
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	if target.ID != targetID || target.Revision != revision || !target.Digest.Valid() {
		return coreexecution.ExecutionTarget{}, ErrUncertain
	}
	normalized, err := target.Normalize()
	if err != nil || normalized.Digest != target.Digest {
		return coreexecution.ExecutionTarget{}, ErrUncertain
	}
	return normalized, nil
}

// ProductionSourceResolver proves only immutable source identity and facts
// available from trusted server storage. It intentionally does not clone a
// repository, pull an OCI image, extract an archive, or run project code on
// the Message Server. Uploaded archives are scanned in-memory under strict
// limits by StaticSourceArchiveAnalyzer; other source kinds return explicit
// blocking uncertainties instead of invented stack/runtime facts.
type ProductionSourceResolver struct {
	archives sourceArchiveAnalyzer
	oci      sourceOCIAnalyzer
}

func NewProductionSourceResolver(
	metadata *storage.DatabaseExecutionStore,
	content *artifactstore.Store,
) *ProductionSourceResolver {
	if metadata == nil || content == nil {
		return nil
	}
	catalog := artifactstore.NewSourceCatalog(content, metadata)
	return &ProductionSourceResolver{
		archives: NewStaticSourceArchiveAnalyzer(catalog),
		oci:      NewPublicOCIRegistryAnalyzer(),
	}
}

func (r *ProductionSourceResolver) ResolveSource(
	ctx context.Context,
	owner string,
	projectID string,
	in SourceInput,
) (SourceFacts, error) {
	owner = strings.TrimSpace(owner)
	projectID = strings.TrimSpace(projectID)
	if r == nil || owner == "" || !coreexecution.ValidateUUID(projectID) || !in.Immutable {
		return SourceFacts{}, ErrSourceInvalid
	}

	switch strings.TrimSpace(in.Kind) {
	case "uploaded_artifact":
		return r.resolveUploadedArtifact(ctx, owner, projectID, in)
	case "git_https":
		return resolveGitMetadata(in)
	case "oci_image":
		return r.resolveOCIMetadata(ctx, in)
	default:
		return SourceFacts{}, ErrSourceInvalid
	}
}

func (r *ProductionSourceResolver) resolveUploadedArtifact(
	ctx context.Context,
	owner string,
	projectID string,
	in SourceInput,
) (SourceFacts, error) {
	if in.Location != "" || in.Commit != "" || in.CredentialRef != "" || in.CredentialRevision != 0 ||
		in.ArtifactDigest != "" || !coreexecution.ValidateUUID(in.ArtifactID) {
		return SourceFacts{}, ErrSourceInvalid
	}
	if r.archives == nil {
		return SourceFacts{}, ErrSourceIntegrity
	}
	return r.archives.AnalyzeSourceArchive(ctx, owner, projectID, in.ArtifactID)
}

func resolveGitMetadata(in SourceInput) (SourceFacts, error) {
	if in.ArtifactID != "" || in.ArtifactDigest != "" || in.Location == "" ||
		(!isHexPin(in.Commit, 40) && !isHexPin(in.Commit, 64)) ||
		(in.CredentialRef == "") != (in.CredentialRevision == 0) {
		return SourceFacts{}, ErrSourceInvalid
	}
	u, err := url.Parse(in.Location)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" ||
		u.RawQuery != "" || strings.ContainsAny(in.Location, "\r\n\x00") {
		return SourceFacts{}, ErrSourceInvalid
	}
	source := coreexecution.SourceRef{
		Kind: "git_https", Location: in.Location, Commit: in.Commit, Immutable: true,
	}
	if in.CredentialRef != "" {
		if !coreexecution.ValidateUUID(in.CredentialRef) {
			return SourceFacts{}, ErrSourceInvalid
		}
		return sourceUncertainty(
			source,
			"private git inspection is not enabled; a credential-bound remote metadata analyzer is required",
		), nil
	}
	return sourceUncertainty(
		source,
		"git commit existence and project manifests have not been inspected by a trusted remote analyzer",
	), nil
}

func (r *ProductionSourceResolver) resolveOCIMetadata(ctx context.Context, in SourceInput) (SourceFacts, error) {
	if in.Commit != "" || in.ArtifactID != "" || in.ArtifactDigest != "" ||
		in.CredentialRef != "" || in.CredentialRevision != 0 {
		return SourceFacts{}, ErrSourceInvalid
	}
	if _, err := parsePinnedOCIReference(in.Location); err != nil {
		return SourceFacts{}, ErrSourceInvalid
	}
	source := coreexecution.SourceRef{Kind: "oci_image", Location: in.Location, Immutable: true}
	if r == nil || r.oci == nil {
		facts := sourceUncertainty(
			source,
			"OCI manifest/config existence and runtime requirements have not been inspected by a trusted registry analyzer",
		)
		facts.Analysis.DetectedStacks = []string{"oci_image"}
		return facts, nil
	}
	return r.oci.AnalyzePinnedImage(ctx, in.Location)
}

func sourceUncertainty(source coreexecution.SourceRef, uncertainty string) SourceFacts {
	return SourceFacts{
		Analysis: coreexecution.ProjectAnalysis{
			Source:                source,
			BlockingUncertainties: []string{uncertainty},
		},
		BlockingUncertainties: []string{uncertainty},
	}
}

func isHexPin(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type productionBindingReader interface {
	GetAnalysis(context.Context, string, string) (coreexecution.ProjectAnalysis, error)
	GetLatestReadyTargetObservation(context.Context, string, string, uint64) (storage.TargetObservationRecord, error)
}

// ProductionContainerBindingResolver converts only a fully verified,
// blocker-free public OCI analysis into the restricted generic container
// recipe. It reads the newest exact target observation and derives every
// network, port, name and placement value on the server.
type ProductionContainerBindingResolver struct {
	store productionBindingReader
	now   func() time.Time
}

func NewProductionContainerBindingResolver(store *storage.DatabaseExecutionStore, clock func() time.Time) *ProductionContainerBindingResolver {
	if store == nil {
		return nil
	}
	if clock == nil {
		clock = time.Now
	}
	return &ProductionContainerBindingResolver{store: store, now: clock}
}

func (r *ProductionContainerBindingResolver) ResolveBindings(ctx context.Context, owner string, req agentembedded.ExecutionV2PlanCreateRequest, recipe agentrecipes.RecipeManifest, target coreexecution.ExecutionTarget) (BindingFacts, error) {
	var out BindingFacts
	if r == nil || r.store == nil || strings.TrimSpace(owner) == "" || recipe.ID != "generic-container-service" || req.Intent != "deploy" || req.Purpose != coreexecution.PurposeService || !coreexecution.ValidateUUID(req.ProjectID) || !coreexecution.ValidateUUID(req.AnalysisID) || req.TargetID != target.ID || req.TargetRevision != target.Revision || target.Provider != "aws" || target.Kind != "aws_ec2_instance" || (target.InfrastructureProfileID != "general-linux-ssm-v1" && target.InfrastructureProfileID != "container-host-v1") || len(target.CredentialRefs) != 1 || !hasAllCapabilities(target.Capabilities, "target.aws_ec2_instance", "transport.aws_ssm") {
		return out, ErrUncertain
	}
	analysis, err := r.store.GetAnalysis(ctx, owner, req.AnalysisID)
	if err != nil {
		return out, err
	}
	normalized, err := analysis.Normalize()
	if err != nil || normalized.Digest != analysis.Digest || analysis.ProjectID != req.ProjectID || len(analysis.BlockingUncertainties) != 0 || analysis.Source.Kind != "oci_image" || !analysis.Source.Immutable || !containsSortedString(analysis.DetectedStacks, "oci_image") || !validOCIAnalysisRuntime(analysis.Runtime) || len(analysis.SecretPurposes) != 0 || len(analysis.SecretRefs) != 0 || len(analysis.Volumes) != 0 || len(analysis.Migrations) != 0 || analysis.Exposure != "target_local" || len(analysis.Ports) != 1 || len(analysis.Probes) != 1 {
		return out, ErrUncertain
	}
	if _, _, err := pinnedImageRegistry(analysis.Source.Location); err != nil {
		return out, ErrUncertain
	}
	port := analysis.Ports[0]
	probeURL, err := exactLoopbackProbe(analysis.Probes[0], port)
	if err != nil {
		return out, ErrUncertain
	}
	// The current EC2 security group allows public TCP/443 to any destination.
	// Keep the image itself digest-pinned, but disclose and bind the actual
	// network envelope instead of claiming hostname/path enforcement.
	external, err := (coreexecution.NetworkGrant{Scheme: "https", Host: coreexecution.PublicHTTPSWildcardHost, Port: 443, Scope: "external"}).Normalize()
	if err != nil {
		return out, ErrUncertain
	}
	probeParsed, _ := url.Parse(probeURL)
	probePort, _ := strconv.Atoi(probeParsed.Port())
	local, err := (coreexecution.NetworkGrant{Scheme: "http", Host: "127.0.0.1", Port: uint16(probePort), PathPrefix: probeParsed.Path, Scope: "target_local"}).Normalize()
	if err != nil || !targetAllows(target, external) || !targetAllows(target, local) {
		return out, ErrUncertain
	}
	observation, err := r.store.GetLatestReadyTargetObservation(ctx, owner, target.ID, target.Revision)
	now := r.now().UTC()
	if err != nil || observation.OwnerID != owner || observation.Status != "observed" || observation.Observation.TargetID != target.ID || observation.Observation.TargetRevision != target.Revision || observation.Observation.State != "ready" || observation.Observation.Partial || observation.Observation.Stale || observation.Observation.Digest == "" || observation.Observation.ObservedAt.After(now.Add(time.Minute)) || observation.Observation.ObservedAt.Before(now.Add(-15*time.Minute)) || observation.Observation.Facts["ssm_status"] != "Online" || observation.Observation.Facts["operating_system"] != "linux" || observation.Observation.Facts["platform_name"] != "Amazon Linux" || !strings.HasPrefix(observation.Observation.Facts["platform_version"], "2023") || !targetPinsObservedInstance(target, observation.Observation.Facts["instance_id"]) || observation.Observation.Facts["account_id"] != target.AccountID || observation.Observation.Facts["region"] != target.Region || observation.Observation.Facts["architecture"] != target.Architecture || !runtimeRequirementFits(analysis.Runtime, target.Architecture, observation.Observation.Facts) || target.Network.Mode != coreexecution.NetworkPolicyModeObservedHTTPSEgress || observation.Observation.Facts[coreexecution.ObservationFactHTTPSEgress] != coreexecution.ObservationFactHTTPSEgressValue || !coreexecution.Digest(observation.Observation.Facts[coreexecution.ObservationFactSecurityGroupDigest]).Valid() {
		return out, ErrUncertain
	}
	observationRef := coreexecution.TargetObservationRef{ObservationID: observation.ObservationID, TargetID: target.ID, TargetRevision: target.Revision, ObservationDigest: observation.Observation.Digest}
	if _, err := observationRef.Normalize(); err != nil {
		return out, ErrUncertain
	}
	serviceName := "dirextalk-" + strings.ReplaceAll(req.ProjectID, "-", "")[:12]
	var aiSecret *coreexecution.CredentialRef
	if req.AIConfiguration != nil {
		configuration, normalizeErr := req.AIConfiguration.Normalize()
		if normalizeErr != nil {
			return out, ErrUncertain
		}
		if configuration.Mode == coreexecution.AIAuthModeAPIKey {
			ref := configuration.CredentialRef()
			aiSecret = &ref
			out.SecretRefs = map[string]coreexecution.CredentialRef{coreexecution.AISecretPurposeProviderAPIKey: ref}
		}
	}
	out.ObservationRef = observationRef
	applyBinding := coreexecution.ExecutionStep{StepKey: "apply-container", Kind: coreexecution.StepContainerApply, NetworkGrants: []coreexecution.NetworkGrant{external}, ContainerApply: &coreexecution.ContainerApplyStep{Image: analysis.Source.Location, Name: serviceName, HostAddress: "127.0.0.1", HostPort: port, ContainerPort: port, RestartPolicy: "unless-stopped"}}
	if aiSecret != nil {
		applyBinding.SecretRefs = []coreexecution.CredentialRef{*aiSecret}
	}
	out.StepBindings = map[string]coreexecution.ExecutionStep{
		"inspect-target":  {StepKey: "inspect-target", Kind: coreexecution.StepTargetInspect, TargetInspect: &coreexecution.TargetInspectStep{ObservationID: observation.ObservationID}},
		"ensure-package":  {StepKey: "ensure-package", Kind: coreexecution.StepPackageEnsure, NetworkGrants: []coreexecution.NetworkGrant{external}, PackageEnsure: &coreexecution.PackageEnsureStep{Name: "docker", Manager: "dnf", PlatformProfile: "amazon-linux-2023"}},
		"apply-container": applyBinding,
		"probe-http":      {StepKey: "probe-http", Kind: coreexecution.StepHTTPProbe, NetworkGrants: []coreexecution.NetworkGrant{local}, HTTPProbe: &coreexecution.HTTPProbeStep{URL: probeURL, Mode: "target_local", ExpectedStatus: []int{200}}},
	}
	quote := coreexecution.CostQuote{Amount: "0", Currency: "USD", ExpiresAt: now.Add(30 * time.Minute)}
	spec := observation.Observation.Facts["instance_type"]
	if spec == "" {
		spec = "existing"
	}
	option := coreexecution.PlacementOption{Region: target.Region, Spec: spec, Disk: "existing", Network: "public_https_443_egress", CostQuote: quote}
	out.Placement = coreexecution.PlacementRecommendation{Kind: "existing_target", Minimum: option, Recommended: option, HighPerformance: option}
	return out, nil
}

func exactLoopbackProbe(raw string, expectedPort int) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() == "" || u.Path == "" {
		return "", ErrUncertain
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port != expectedPort || port < 1 || port > 65535 {
		return "", ErrUncertain
	}
	return u.String(), nil
}

func hasAllCapabilities(have []string, required ...string) bool {
	set := map[string]bool{}
	for _, value := range have {
		set[value] = true
	}
	for _, value := range required {
		if !set[value] {
			return false
		}
	}
	return true
}

func targetPinsObservedInstance(target coreexecution.ExecutionTarget, observedInstanceID string) bool {
	if !coreaws.ValidEC2InstanceID(observedInstanceID) {
		return false
	}
	want := coreaws.TargetInstanceCapabilityPrefix + observedInstanceID
	found := 0
	for _, capability := range target.Capabilities {
		if strings.HasPrefix(capability, coreaws.TargetInstanceCapabilityPrefix) {
			if capability != want {
				return false
			}
			found++
		}
	}
	return found == 1
}

func containsSortedString(values []string, want string) bool {
	i := sort.SearchStrings(values, want)
	return i < len(values) && values[i] == want
}

func validOCIAnalysisRuntime(requirement coreexecution.ResourceRequirement) bool {
	_, cpuOK := canonicalPositiveQuantity(requirement.CPU, "")
	_, memoryOK := canonicalPositiveQuantity(requirement.Memory, "MiB")
	_, diskOK := canonicalPositiveQuantity(requirement.Disk, "GiB")
	return cpuOK && memoryOK && diskOK && requirement.GPU == "" &&
		(requirement.Architecture == "x86_64" || requirement.Architecture == "arm64")
}

func runtimeRequirementFits(requirement coreexecution.ResourceRequirement, targetArchitecture string, facts map[string]string) bool {
	if !validOCIAnalysisRuntime(requirement) || requirement.Architecture != targetArchitecture || facts["architecture"] != targetArchitecture {
		return false
	}
	requiredCPU, _ := canonicalPositiveQuantity(requirement.CPU, "")
	requiredMemory, _ := canonicalPositiveQuantity(requirement.Memory, "MiB")
	requiredDisk, _ := canonicalPositiveQuantity(requirement.Disk, "GiB")
	availableCPU, cpuOK := canonicalPositiveQuantity(facts[coreexecution.ObservationFactVCPUCount], "")
	availableMemory, memoryOK := canonicalPositiveQuantity(facts[coreexecution.ObservationFactMemoryMiB], "")
	availableDisk, diskOK := canonicalPositiveQuantity(facts[coreexecution.ObservationFactRootVolumeGiB], "")
	return cpuOK && memoryOK && diskOK && availableCPU >= requiredCPU && availableMemory >= requiredMemory && availableDisk >= requiredDisk
}

func canonicalPositiveQuantity(value, suffix string) (uint64, bool) {
	if suffix != "" && !strings.HasSuffix(value, suffix) {
		return 0, false
	}
	numeric := strings.TrimSuffix(value, suffix)
	parsed, err := strconv.ParseUint(numeric, 10, 64)
	return parsed, err == nil && parsed > 0 && strconv.FormatUint(parsed, 10) == numeric
}

func targetAllows(target coreexecution.ExecutionTarget, want coreexecution.NetworkGrant) bool {
	for _, denied := range target.Network.Deny {
		if normalized, err := denied.Normalize(); err == nil && normalized == want {
			return false
		}
	}
	for _, allowed := range target.Network.Allow {
		if normalized, err := allowed.Normalize(); err == nil && normalized == want {
			return true
		}
	}
	return target.Network.Mode == coreexecution.NetworkPolicyModeObservedHTTPSEgress &&
		(want.Scope == "target_local" || want.Scope == "external" && want.Scheme == "https" && want.Host == coreexecution.PublicHTTPSWildcardHost && want.Port == 443 && want.PathPrefix == "")
}
