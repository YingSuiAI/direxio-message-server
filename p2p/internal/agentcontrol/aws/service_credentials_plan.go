package aws

import (
	"context"
	"strconv"
	"strings"
)

// credentialReplayRepository is deliberately optional so the domain stays
// storage-agnostic. Durable repositories must check and record the replay in
// the same transaction as the credential projection and secret revision.
type credentialReplayRepository interface {
	ReplayCredential(context.Context, string, string, string) (CredentialView, bool, error)
	SaveCredentialIdempotent(context.Context, Credentials, string, string) (CredentialView, error)
	ReplaceCredentialIdempotent(context.Context, Credentials, int64, string, string) (CredentialView, error)
	DeleteCredentialIdempotent(context.Context, string, int64, string, string) error
}

func (s *Service) SaveCredential(ctx context.Context, in CredentialInput) (CredentialView, error) {
	if s == nil || s.repo == nil {
		return CredentialView{}, ErrInvalid
	}
	if in.IdempotencyKey == "" {
		in.IdempotencyKey = newUUID()
	}
	if !validUUID(in.IdempotencyKey) {
		return CredentialView{}, ErrInvalid
	}
	digest := credentialInputDigest(in)
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		if v, hit, e := dr.ReplayCredential(ctx, "credential-save", in.IdempotencyKey, digest); hit {
			return v, e
		}
	}
	if mr, ok := s.repo.(*MemoryRepository); ok {
		mr.mu.Lock()
		if v, hit, e := mr.replayLocked("credential-save", in.IdempotencyKey, digest); hit {
			mr.mu.Unlock()
			if e != nil {
				return CredentialView{}, e
			}
			return *v.credential, nil
		}
		mr.mu.Unlock()
	}
	id := in.ID
	if id == "" {
		id = newUUID()
	}
	now := s.now().UTC()
	c := Credentials{ID: id, Name: strings.TrimSpace(in.Name), Region: strings.TrimSpace(in.Region), private: &credentialPayload{accessKeyID: in.AccessKeyID, secretAccessKey: in.SecretAccessKey, sessionToken: in.SessionToken}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := c.Validate(); err != nil {
		return CredentialView{}, err
	}
	if mr, ok := s.repo.(*MemoryRepository); ok {
		view, e := mr.saveCredentialIdempotent(ctx, c, in.IdempotencyKey, digest)
		return view, e
	}
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		return dr.SaveCredentialIdempotent(ctx, c, in.IdempotencyKey, digest)
	}
	v, e := s.repo.CreateCredential(ctx, c)
	return v.View(), e
}
func (s *Service) GetCredential(ctx context.Context, id string) (CredentialView, error) {
	c, e := s.repo.GetCredential(ctx, id)
	if e != nil {
		return CredentialView{}, e
	}
	return c.View(), nil
}
func (s *Service) GetCredentialRevision(ctx context.Context, id string, revision int64) (Credentials, error) {
	if s == nil || s.repo == nil || !validUUID(id) || revision < 1 {
		return Credentials{}, ErrInvalid
	}
	return s.repo.GetCredentialRevision(ctx, id, revision)
}
func (s *Service) ListCredentials(ctx context.Context, size int, token string) (CredentialPage, error) {
	return s.repo.ListCredentials(ctx, size, token)
}
func (s *Service) ReplaceCredential(ctx context.Context, in CredentialInput, expected int64, idem ...string) (CredentialView, error) {
	key := ""
	if len(idem) > 0 {
		key = idem[0]
	}
	if key == "" {
		key = newUUID()
	}
	if !validUUID(key) {
		return CredentialView{}, ErrInvalid
	}
	digest := canonicalDigest(struct {
		InputDigest string
		Expected    int64
	}{credentialInputDigest(in), expected})
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		if v, hit, e := dr.ReplayCredential(ctx, "credential-replace", key, digest); hit {
			return v, e
		}
	}
	if mr, ok := s.repo.(*MemoryRepository); ok {
		mr.mu.Lock()
		if v, hit, e := mr.replayLocked("credential-replace", key, digest); hit {
			mr.mu.Unlock()
			if e != nil {
				return CredentialView{}, e
			}
			return *v.credential, nil
		}
		mr.mu.Unlock()
	}
	if !validUUID(in.ID) {
		return CredentialView{}, ErrInvalid
	}
	old, e := s.repo.GetCredential(ctx, in.ID)
	if e != nil {
		return CredentialView{}, e
	}
	c := old
	c.Name = strings.TrimSpace(in.Name)
	c.Region = strings.TrimSpace(in.Region)
	if c.private == nil {
		c.private = &credentialPayload{}
	}
	if in.AccessKeyID != "" {
		c.private.accessKeyID = in.AccessKeyID
	}
	if in.SecretAccessKey != "" {
		c.private.secretAccessKey = in.SecretAccessKey
	}
	if in.SessionToken != "" {
		c.private.sessionToken = in.SessionToken
	}
	c.Revision = expected + 1
	c.AccountID, c.UserARN, c.VerifiedRevision = "", "", 0
	c.UpdatedAt = s.now().UTC()
	if err := c.Validate(); err != nil {
		return CredentialView{}, err
	}
	if mr, ok := s.repo.(*MemoryRepository); ok {
		view, e := mr.replaceCredentialIdempotent(ctx, c, expected, key, digest)
		return view, e
	}
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		return dr.ReplaceCredentialIdempotent(ctx, c, expected, key, digest)
	}
	v, e := s.repo.UpdateCredential(ctx, c, expected)
	if e != nil {
		return CredentialView{}, e
	}
	view := v.View()
	return view, nil
}
func (s *Service) DeleteCredential(ctx context.Context, id string, expected int64, idem ...string) error {
	key := ""
	if len(idem) > 0 {
		key = idem[0]
	}
	if key == "" {
		key = newUUID()
	}
	if !validUUID(key) {
		return ErrInvalid
	}
	digest := canonicalDigest(struct {
		ID       string
		Expected int64
	}{id, expected})
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		if _, hit, e := dr.ReplayCredential(ctx, "credential-delete", key, digest); hit {
			return e
		}
	}
	if mr, ok := s.repo.(*MemoryRepository); ok {
		return mr.deleteCredentialIdempotent(ctx, id, expected, key, digest)
	}
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		return dr.DeleteCredentialIdempotent(ctx, id, expected, key, digest)
	}
	return s.repo.DeleteCredential(ctx, id, expected)
}
func (s *Service) TestCredential(ctx context.Context, id string) (CredentialTest, error) {
	if s.sts == nil {
		return CredentialTest{}, ErrProvider
	}
	c, e := s.repo.GetCredential(ctx, id)
	if e != nil {
		return CredentialTest{}, e
	}
	identity, e := s.sts.GetCallerIdentity(ctx, c.handle())
	if e != nil {
		return CredentialTest{}, ErrProvider
	}
	updated, ue := s.repo.RecordCredentialIdentity(ctx, id, c.Revision, identity)
	if ue != nil {
		return CredentialTest{}, ue
	}
	c = updated
	return CredentialTest{CredentialID: id, Identity: identity, CredentialRevision: c.Revision, TestedAt: s.now().UTC()}, nil
}

func (s *Service) CreatePlan(ctx context.Context, in PlanInput) (PlanView, error) {
	if s == nil || s.repo == nil {
		return PlanView{}, ErrInvalid
	}
	if in.IdempotencyKey == "" {
		in.IdempotencyKey = newUUID()
	}
	if !validUUID(in.IdempotencyKey) {
		return PlanView{}, ErrInvalid
	}
	dig := canonicalDigest(in)
	if mr, ok := s.repo.(*MemoryRepository); ok {
		mr.mu.Lock()
		if v, hit, e := mr.replayLocked("plan-create", in.IdempotencyKey, dig); hit {
			mr.mu.Unlock()
			if e != nil {
				return PlanView{}, e
			}
			return s.planViewFromView(ctx, *v.plan)
		}
		mr.mu.Unlock()
	}
	if !validUUID(in.CredentialID) {
		return PlanView{}, ErrInvalid
	}
	if in.ExpectedCredentialRevision < 0 {
		return PlanView{}, ErrInvalid
	}
	cred, e := s.repo.GetCredential(ctx, in.CredentialID)
	if e != nil {
		return PlanView{}, e
	}
	if in.ExpectedCredentialRevision > 0 && in.ExpectedCredentialRevision != cred.Revision {
		return PlanView{}, ErrRevisionConflict
	}
	if !credentialReadyForPlan(cred) {
		return PlanView{}, ErrConflict
	}
	norm, digest, e := normalizeTemplate(in.Template)
	if e != nil {
		return PlanView{}, e
	}
	id := in.ID
	if id == "" {
		id = newUUID()
	}
	p := Plan{ID: id, CredentialID: in.CredentialID, CredentialRevision: cred.Revision, Region: in.Region, StackName: in.StackName, Operation: in.Operation, Template: norm, TemplateSHA256: digest, Parameters: cloneMap(in.Parameters), Tags: cloneMap(in.Tags), Capabilities: append([]string(nil), in.Capabilities...), Revision: 1, CreatedAt: s.now().UTC()}
	if p.Region == "" {
		p.Region = cred.Region
	}
	if err := p.Validate(); err != nil {
		return PlanView{}, err
	}
	if p.Region != cred.Region {
		return PlanView{}, ErrConflict
	}
	if mr, ok := s.repo.(*MemoryRepository); ok {
		view, err := mr.createPlanIdempotent(ctx, p, in.IdempotencyKey, dig)
		if err != nil {
			return PlanView{}, err
		}
		return s.planViewFromView(ctx, view)
	}
	v, e := s.repo.CreatePlan(ctx, p)
	if e != nil {
		return PlanView{}, e
	}
	return s.planViewWithCredential(ctx, v)
}
func (s *Service) GetPlan(ctx context.Context, id string) (PlanView, error) {
	p, e := s.repo.GetPlan(ctx, id)
	if e != nil {
		return PlanView{}, e
	}
	return s.planViewWithCredential(ctx, p)
}
func (s *Service) ListPlans(ctx context.Context, size int, token string) (PlanPage, error) {
	page, err := s.repo.ListPlans(ctx, size, token)
	if err != nil {
		return PlanPage{}, err
	}
	if batch, ok := s.repo.(CredentialMetadataBatchRepository); ok {
		refs := make([]CredentialRevisionRef, 0, len(page.Items))
		for _, view := range page.Items {
			refs = append(refs, CredentialRevisionRef{ID: view.CredentialID, Revision: view.CredentialRevision})
		}
		metadata, batchErr := batch.ListCredentialRevisionMetadata(ctx, refs)
		if batchErr != nil {
			return PlanPage{}, batchErr
		}
		for i, view := range page.Items {
			key := view.CredentialID + ":" + strconv.FormatInt(view.CredentialRevision, 10)
			cred, found := metadata[key]
			if !found {
				return PlanPage{}, ErrNotFound
			}
			if !credentialReadyForPlan(cred) {
				return PlanPage{}, ErrConflict
			}
			page.Items[i] = viewWithCredential(view, cred)
		}
		return page, nil
	}
	for i, view := range page.Items {
		page.Items[i], err = s.planViewFromView(ctx, view)
		if err != nil {
			return PlanPage{}, err
		}
	}
	return page, nil
}

func viewWithCredential(v PlanView, cred Credentials) PlanView {
	p := Plan{ID: v.ID, CredentialID: v.CredentialID, CredentialRevision: v.CredentialRevision, Region: v.Region, StackName: v.StackName, Operation: v.Operation, TemplateSHA256: v.TemplateSHA256, Parameters: cloneMap(v.Parameters), Tags: cloneMap(v.Tags), Capabilities: append([]string(nil), v.Capabilities...), Revision: v.Revision, CreatedAt: v.CreatedAt}
	return p.ViewWithCredentials(cred)
}

func (s *Service) planViewWithCredential(ctx context.Context, p Plan) (PlanView, error) {
	if s == nil || s.repo == nil || p.CredentialRevision < 1 {
		return PlanView{}, ErrInvalid
	}
	var cred Credentials
	var err error
	if metadata, ok := s.repo.(CredentialMetadataRepository); ok {
		cred, err = metadata.GetCredentialRevisionMetadata(ctx, p.CredentialID, p.CredentialRevision)
	} else {
		cred, err = s.repo.GetCredentialRevision(ctx, p.CredentialID, p.CredentialRevision)
	}
	if err != nil {
		return PlanView{}, err
	}
	if !credentialReadyForPlan(cred) {
		return PlanView{}, ErrConflict
	}
	return p.ViewWithCredentials(cred), nil
}

func credentialReadyForPlan(c Credentials) bool {
	return c.Revision > 0 && c.VerifiedRevision == c.Revision && strings.TrimSpace(c.AccountID) != "" && strings.TrimSpace(c.UserARN) != ""
}

// CredentialReadyForPlan exposes the fail-closed identity gate to storage
// adapters that rehydrate historical credential metadata inside transactions.
func CredentialReadyForPlan(c Credentials) bool { return credentialReadyForPlan(c) }

func (s *Service) requireCredentialReady(ctx context.Context, id string, revision int64) error {
	if s == nil || s.repo == nil || revision < 1 {
		return ErrInvalid
	}
	var c Credentials
	var err error
	if metadata, ok := s.repo.(CredentialMetadataRepository); ok {
		c, err = metadata.GetCredentialRevisionMetadata(ctx, id, revision)
	} else {
		c, err = s.repo.GetCredentialRevision(ctx, id, revision)
	}
	if err != nil {
		return err
	}
	if !credentialReadyForPlan(c) {
		return ErrConflict
	}
	return nil
}

// planViewFromView rehydrates only the non-secret immutable plan fields. This
// keeps ListPlans storage-agnostic while still deriving binding digests from
// the historical credential revision selected by the plan.
func (s *Service) planViewFromView(ctx context.Context, v PlanView) (PlanView, error) {
	var cred Credentials
	var err error
	if metadata, ok := s.repo.(CredentialMetadataRepository); ok {
		cred, err = metadata.GetCredentialRevisionMetadata(ctx, v.CredentialID, v.CredentialRevision)
	} else {
		cred, err = s.repo.GetCredentialRevision(ctx, v.CredentialID, v.CredentialRevision)
	}
	if err != nil {
		return PlanView{}, err
	}
	if !credentialReadyForPlan(cred) {
		return PlanView{}, ErrConflict
	}
	return viewWithCredential(v, cred), nil
}
func (s *Service) Quote(ctx context.Context, id string) (Quote, error) {
	p, e := s.repo.GetPlan(ctx, id)
	if e != nil {
		return Quote{}, e
	}
	return quoteFor(p), nil
}
