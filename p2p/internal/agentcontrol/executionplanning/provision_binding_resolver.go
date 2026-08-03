package executionplanning

// This file owns the server-authoritative binding for the reservation -> EC2
// provision plan. ProductCore supplies only immutable catalog identities. The
// selected profile, account, region, credential revision, network posture,
// machine shape, AMI and cost quote all come from target revision 1.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type productionProvisionBindingReader interface {
	GetAnalysis(context.Context, string, string) (coreexecution.ProjectAnalysis, error)
}

type ProductionProvisionBindingResolver struct {
	store productionProvisionBindingReader
	now   func() time.Time
}

func NewProductionProvisionBindingResolver(store *storage.DatabaseExecutionStore, clock func() time.Time) *ProductionProvisionBindingResolver {
	if store == nil {
		return nil
	}
	if clock == nil {
		clock = time.Now
	}
	return &ProductionProvisionBindingResolver{store: store, now: clock}
}

func (r *ProductionProvisionBindingResolver) ResolveBindings(
	ctx context.Context,
	owner string,
	req agentembedded.ExecutionV2PlanCreateRequest,
	recipe agentrecipes.RecipeManifest,
	target coreexecution.ExecutionTarget,
) (BindingFacts, error) {
	var out BindingFacts
	owner = strings.TrimSpace(owner)
	if r == nil || r.store == nil || r.now == nil || owner == "" || recipe.ID != "aws-ec2-provision" ||
		!coreexecution.ValidateUUID(req.ProjectID) || !coreexecution.ValidateUUID(req.AnalysisID) ||
		req.TargetID != target.ID || req.TargetRevision != target.Revision ||
		(req.Purpose != coreexecution.PurposeService && req.Purpose != coreexecution.PurposeJob) {
		return out, ErrUncertain
	}
	normalized, err := target.Normalize()
	if err != nil || normalized.Digest != target.Digest || target.Provider != "aws" ||
		target.Kind != coreexecution.TargetKindAWSComputeReservation || target.Revision != 1 ||
		target.InfrastructureProfileID != coreaws.InfrastructureProfileGeneralLinuxSSMV1 ||
		target.ComputeReservation == nil || target.Network.Mode != "restricted" ||
		len(target.Network.Allow) != 0 || len(target.Network.Deny) != 0 || len(target.CredentialRefs) != 1 ||
		len(target.Capabilities) != 3 || !hasAllCapabilities(target.Capabilities, "compute.catalog", "compute.provision", "target.aws_compute_reservation") {
		return out, ErrUncertain
	}
	credential := target.CredentialRefs[0]
	reservation := *target.ComputeReservation
	if credential.Purpose != "aws" || credential.Revision == 0 || !credential.BindingDigest.Valid() ||
		reservation.InfrastructureProfileID != target.InfrastructureProfileID ||
		reservation.InfrastructureProfileID != coreaws.InfrastructureProfileGeneralLinuxSSMV1 ||
		reservation.AMIParameter != coreexecution.AWSAL2023X8664AMIParameter ||
		reservation.Architecture != target.Architecture || reservation.Architecture != "x86_64" ||
		reservation.ManagementTransport != "aws_ssm" || !reservation.PublicIP || reservation.PublicInbound ||
		!coreexecution.ValidateAvailabilityZone(target.Region, reservation.AvailabilityZone) ||
		reservation.InstanceType == "" || reservation.VolumeGiB < 8 || reservation.VolumeGiB > 16384 ||
		reservation.CostQuote.Validate() != nil || !reservation.CostQuote.ExpiresAt.After(r.now().UTC()) {
		return out, ErrUncertain
	}
	analysis, err := r.store.GetAnalysis(ctx, owner, req.AnalysisID)
	if err != nil {
		return out, err
	}
	analysisNormalized, err := analysis.Normalize()
	if err != nil || analysisNormalized.Digest != analysis.Digest || analysis.AnalysisID != req.AnalysisID ||
		analysis.ProjectID != req.ProjectID || len(analysis.BlockingUncertainties) != 0 {
		return out, ErrUncertain
	}
	out.StepBindings = map[string]coreexecution.ExecutionStep{
		"provision-compute": {
			StepKey: "provision-compute", Kind: coreexecution.StepComputeProvision,
			ComputeProvision: &coreexecution.ComputeProvisionStep{
				InfrastructureProfileID: reservation.InfrastructureProfileID,
				AMIParameter:            reservation.AMIParameter,
				InstanceType:            reservation.InstanceType,
				AvailabilityZone:        reservation.AvailabilityZone,
				VolumeGiB:               reservation.VolumeGiB,
				Region:                  target.Region,
				Architecture:            reservation.Architecture,
				ManagementTransport:     reservation.ManagementTransport,
				PublicIP:                reservation.PublicIP,
				PublicInbound:           reservation.PublicInbound,
			},
		},
	}
	option := coreexecution.PlacementOption{
		Region: target.Region, Spec: reservation.InstanceType,
		Disk: fmt.Sprintf("%dGiB", reservation.VolumeGiB), Network: "restricted",
		CostQuote: reservation.CostQuote,
	}
	kind := "new_persistent_target"
	if req.Purpose == coreexecution.PurposeJob {
		kind = "new_ephemeral_target"
	}
	out.Placement = coreexecution.PlacementRecommendation{Kind: kind, Minimum: option, Recommended: option, HighPerformance: option}
	return out, nil
}

// ProductionBindingResolver is a closed recipe router. Each recipe keeps its
// own fail-closed proof rules; an unknown recipe is never handed to a generic
// best-effort resolver.
type ProductionBindingResolver struct {
	provision *ProductionProvisionBindingResolver
	container *ProductionContainerBindingResolver
}

func NewProductionBindingResolver(store *storage.DatabaseExecutionStore, clock func() time.Time) *ProductionBindingResolver {
	if store == nil {
		return nil
	}
	return &ProductionBindingResolver{
		provision: NewProductionProvisionBindingResolver(store, clock),
		container: NewProductionContainerBindingResolver(store, clock),
	}
}

func (r *ProductionBindingResolver) ResolveBindings(ctx context.Context, owner string, req agentembedded.ExecutionV2PlanCreateRequest, recipe agentrecipes.RecipeManifest, target coreexecution.ExecutionTarget) (BindingFacts, error) {
	if r == nil {
		return BindingFacts{}, ErrNotReady
	}
	switch recipe.ID {
	case "aws-ec2-provision":
		if r.provision == nil {
			return BindingFacts{}, ErrNotReady
		}
		return r.provision.ResolveBindings(ctx, owner, req, recipe, target)
	case "generic-container-service":
		if r.container == nil {
			return BindingFacts{}, ErrNotReady
		}
		return r.container.ResolveBindings(ctx, owner, req, recipe, target)
	default:
		return BindingFacts{}, ErrUncertain
	}
}

var _ StepBindingResolver = (*ProductionProvisionBindingResolver)(nil)
var _ StepBindingResolver = (*ProductionBindingResolver)(nil)
