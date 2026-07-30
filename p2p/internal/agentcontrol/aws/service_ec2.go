package aws

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

type EC2ProvisionResult struct {
	Plan      PlanView
	Quote     Quote
	Provision Provision
}

func (s *Service) CreateEC2Provision(ctx context.Context, req EC2ProvisionRequest, idempotencyKey string) (EC2ProvisionResult, error) {
	if s == nil || s.repo == nil || !validUUID(idempotencyKey) || ValidateEC2ProvisionRequest(req) != nil {
		return EC2ProvisionResult{}, ErrInvalid
	}
	built, err := BuildEC2ProvisionPlan(req)
	if err != nil {
		return EC2ProvisionResult{}, err
	}
	plan := built.CorePlan()
	now := s.now().UTC()
	provision := Provision{ID: uuid.NewSHA1(uuid.Nil, []byte("ec2-provision:"+plan.ID)).String(), PlanID: plan.ID, CredentialID: plan.CredentialID, Region: plan.Region, StackName: plan.StackName, Profile: plan.Tags["service"], OwnerDigest: plan.Tags["owner"], CredentialRevision: plan.CredentialRevision, PlanRevision: plan.Revision, TemplateSHA256: plan.TemplateSHA256, PlanDigest: PlanDigest(plan), State: "planned", Revision: 1, CreatedAt: now, UpdatedAt: now}
	digest := canonicalDigest(struct {
		Owner, Key string
		Request    EC2ProvisionRequest
	}{req.OwnerID, idempotencyKey, req})
	repo, ok := s.repo.(EC2ProvisionRepository)
	if !ok {
		return EC2ProvisionResult{}, ErrInvalid
	}
	plan, provision, err = repo.CreateEC2Provision(ctx, plan, provision, idempotencyKey, digest)
	if err != nil {
		return EC2ProvisionResult{}, err
	}
	return EC2ProvisionResult{Plan: plan.View(), Quote: quoteFor(plan), Provision: provision}, nil
}

func (s *Service) GetPlanForOwner(ctx context.Context, id, owner string) (PlanView, error) {
	p, err := s.repo.GetPlan(ctx, id)
	if err != nil {
		return PlanView{}, err
	}
	if !ownerMatchesPlan(p, owner) {
		return PlanView{}, ErrNotFound
	}
	return p.View(), nil
}
func (s *Service) GetProvisionForOwner(ctx context.Context, id, owner string) (Provision, error) {
	p, err := s.repo.GetProvision(ctx, id)
	if err != nil {
		return Provision{}, err
	}
	if strings.TrimSpace(owner) == "" || p.OwnerDigest != OwnerBindingDigest(owner) {
		return Provision{}, ErrNotFound
	}
	return p, nil
}
func (s *Service) ListProvisions(ctx context.Context, owner, state string, size int, token string) (Page[Provision], error) {
	repo, ok := s.repo.(EC2ProvisionRepository)
	if !ok {
		return Page[Provision]{}, ErrInvalid
	}
	return repo.ListProvisions(ctx, owner, state, size, token)
}
func (s *Service) ListProvisionEvents(ctx context.Context, id, owner string, after uint64, limit int) ([]ProvisionEvent, uint64, error) {
	repo, ok := s.repo.(EC2ProvisionRepository)
	if !ok {
		return nil, 0, ErrInvalid
	}
	return repo.ListProvisionEvents(ctx, id, owner, after, limit)
}

func ownerMatchesPlan(p Plan, owner string) bool {
	return strings.TrimSpace(owner) != "" && p.Tags["owner"] == OwnerBindingDigest(owner)
}

func provisionImmutableMatches(p Provision, plan Plan) bool {
	return p.PlanID == plan.ID && p.CredentialID == plan.CredentialID && p.CredentialRevision == plan.CredentialRevision && p.Region == plan.Region && p.StackName == plan.StackName && p.Profile == plan.Tags["service"] && p.OwnerDigest == plan.Tags["owner"] && p.PlanRevision == plan.Revision && p.TemplateSHA256 == plan.TemplateSHA256 && p.PlanDigest == PlanDigest(plan)
}

func (s *Service) RequestEC2Create(ctx context.Context, provisionID string, expectedRevision int64, idempotencyKey string, owner string) (ChangeRequestResult, error) {
	if expectedRevision < 1 || !validUUID(idempotencyKey) {
		return ChangeRequestResult{}, ErrInvalid
	}
	p, err := s.GetProvisionForOwner(ctx, provisionID, owner)
	if err != nil {
		return ChangeRequestResult{}, err
	}
	if p.Revision < expectedRevision || (p.Revision == expectedRevision && p.State != "planned") || (p.Revision > expectedRevision && p.ActiveChangeID == "") {
		return ChangeRequestResult{}, ErrRevisionConflict
	}
	plan, err := s.repo.GetPlan(ctx, p.PlanID)
	if err != nil {
		return ChangeRequestResult{}, err
	}
	if plan.Operation != OperationCreate || !ownerMatchesPlan(plan, owner) || !provisionImmutableMatches(p, plan) {
		return ChangeRequestResult{}, ErrRevisionConflict
	}
	return s.RequestChange(ctx, RequestChangeInput{PlanID: plan.ID, ProvisionID: p.ID, ExpectedProvisionRevision: expectedRevision, IdempotencyKey: idempotencyKey})
}

func (s *Service) RequestEC2Destroy(ctx context.Context, provisionID string, expectedRevision int64, idempotencyKey string, owner string) (ChangeRequestResult, error) {
	if expectedRevision < 1 || !validUUID(idempotencyKey) {
		return ChangeRequestResult{}, ErrInvalid
	}
	p, err := s.GetProvisionForOwner(ctx, provisionID, owner)
	if err != nil {
		return ChangeRequestResult{}, err
	}
	if p.Revision < expectedRevision || (p.Revision == expectedRevision && p.State != "active") || (p.Revision > expectedRevision && p.ActiveChangeID == "") {
		return ChangeRequestResult{}, ErrRevisionConflict
	}
	original, err := s.repo.GetPlan(ctx, p.PlanID)
	if err != nil {
		return ChangeRequestResult{}, err
	}
	if original.Operation != OperationCreate || !ownerMatchesPlan(original, owner) || !provisionImmutableMatches(p, original) {
		return ChangeRequestResult{}, ErrRevisionConflict
	}
	deleteID := uuid.NewSHA1(uuid.Nil, []byte("ec2-destroy:"+p.ID)).String()
	deletePlan := original
	deletePlan.ID = deleteID
	deletePlan.Operation = OperationDelete
	deletePlan.CreatedAt = s.now().UTC()
	deletePlan.Revision = 1
	repo, ok := s.repo.(EC2ProvisionRepository)
	if !ok {
		return ChangeRequestResult{}, ErrInvalid
	}
	if _, err = repo.CreateDerivedDeletePlan(ctx, deletePlan); err != nil {
		return ChangeRequestResult{}, err
	}
	return s.RequestChange(ctx, RequestChangeInput{PlanID: deleteID, ProvisionID: p.ID, ExpectedProvisionRevision: expectedRevision, IdempotencyKey: idempotencyKey})
}

func (s *Service) RetryEC2Provision(ctx context.Context, id string, expected int64, key, owner string) (Provision, error) {
	p, err := s.GetProvisionForOwner(ctx, id, owner)
	if err != nil {
		return Provision{}, err
	}
	return s.RetryProvision(ctx, p.ID, expected, key)
}
