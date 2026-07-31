package aws

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Service is the owner-scoped AWS credential boundary. Provider clients are
// used only for credential identity validation and typed V2 operations.
type Service struct {
	repo Repository
	sts  STSProvider
	now  func() time.Time
}

// ReadyForEmbedded reports whether the credential repository and STS identity
// validator are available. V1 plan/change coordination is intentionally absent.
func (s *Service) ReadyForEmbedded() bool {
	return s != nil && s.repo != nil && s.sts != nil
}

// NewService keeps the historical argument shape for callers migrating from
// the removed coordinator; only repository, STS, provider and clock are used.
func NewService(repo Repository, _ any, _ any, sts STSProvider, _ any, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, sts: sts, now: now}
}

func newUUID() string { return uuid.New().String() }

// GetCallerIdentity is the narrow provider operation retained by credentials.
func (s *Service) GetCallerIdentity(ctx context.Context, id string) (Identity, error) {
	if s == nil || s.repo == nil || s.sts == nil {
		return Identity{}, ErrInvalid
	}
	c, err := s.repo.GetCredential(ctx, id)
	if err != nil {
		return Identity{}, err
	}
	return s.sts.GetCallerIdentity(ctx, c.handle())
}
