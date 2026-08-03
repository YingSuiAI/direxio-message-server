package aws

import (
	"context"
	"strings"
)

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
			return v.view, nil
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
		return mr.SaveCredentialIdempotent(ctx, c, in.IdempotencyKey, digest)
	}
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		return dr.SaveCredentialIdempotent(ctx, c, in.IdempotencyKey, digest)
	}
	v, err := s.repo.CreateCredential(ctx, c)
	return v.View(), err
}

func (s *Service) GetCredential(ctx context.Context, id string) (CredentialView, error) {
	if s == nil || s.repo == nil {
		return CredentialView{}, ErrInvalid
	}
	c, err := s.repo.GetCredential(ctx, id)
	if err != nil {
		return CredentialView{}, err
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
	if s == nil || s.repo == nil {
		return CredentialPage{}, ErrInvalid
	}
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
	if !validUUID(key) || !validUUID(in.ID) || expected < 1 {
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
			return v.view, nil
		}
		mr.mu.Unlock()
	}
	old, err := s.repo.GetCredential(ctx, in.ID)
	if err != nil {
		return CredentialView{}, err
	}
	c := old
	if strings.TrimSpace(in.Name) != "" {
		c.Name = strings.TrimSpace(in.Name)
	}
	if strings.TrimSpace(in.Region) != "" {
		c.Region = strings.TrimSpace(in.Region)
	}
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
		return mr.ReplaceCredentialIdempotent(ctx, c, expected, key, digest)
	}
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		return dr.ReplaceCredentialIdempotent(ctx, c, expected, key, digest)
	}
	v, err := s.repo.UpdateCredential(ctx, c, expected)
	return v.View(), err
}

func (s *Service) DeleteCredential(ctx context.Context, id string, expected int64, idem ...string) error {
	key := ""
	if len(idem) > 0 {
		key = idem[0]
	}
	if key == "" {
		key = newUUID()
	}
	if !validUUID(key) || !validUUID(id) || expected < 1 {
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
		return mr.DeleteCredentialIdempotent(ctx, id, expected, key, digest)
	}
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		return dr.DeleteCredentialIdempotent(ctx, id, expected, key, digest)
	}
	return s.repo.DeleteCredential(ctx, id, expected)
}

func (s *Service) TestCredential(ctx context.Context, id string, expected int64, idem string) (CredentialTest, error) {
	if s == nil || s.repo == nil || s.sts == nil {
		return CredentialTest{}, ErrInvalid
	}
	if !validUUID(id) || expected < 1 || !validUUID(idem) {
		return CredentialTest{}, ErrInvalid
	}
	digest := canonicalDigest(struct {
		ID       string
		Expected int64
	}{id, expected})
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		if v, hit, err := dr.ReplayCredentialTest(ctx, id, expected, idem, digest); hit {
			return v, err
		}
	}
	c, err := s.repo.GetCredential(ctx, id)
	if err != nil {
		return CredentialTest{}, err
	}
	if c.Revision != expected {
		return CredentialTest{}, ErrRevisionConflict
	}
	identity, err := s.sts.GetCallerIdentity(ctx, c.handle())
	if err != nil {
		return CredentialTest{}, ErrProvider
	}
	if dr, ok := s.repo.(credentialReplayRepository); ok {
		return dr.TestCredentialIdempotent(ctx, id, expected, identity, idem, digest)
	}
	updated, err := s.repo.RecordCredentialIdentity(ctx, id, c.Revision, identity)
	if err != nil {
		return CredentialTest{}, err
	}
	return CredentialTest{CredentialID: id, Identity: identity, CredentialRevision: updated.Revision, TestedAt: s.now().UTC()}, nil
}
