package aws

import "context"

// Repository is the persistence boundary for AWS profiles, immutable plans
// and change records. Implementations must serialize mutations and replay
// identical idempotency keys without exposing credential bytes.
type Repository interface {
	CreateCredential(context.Context, Credentials) (Credentials, error)
	GetCredential(context.Context, string) (Credentials, error)
	// GetCredentialRevision returns an immutable historical revision. It is
	// intentionally separate from GetCredential, which is the current (and
	// non-deleted) projection used by credential management.
	GetCredentialRevision(context.Context, string, int64) (Credentials, error)
	ListCredentials(context.Context, int, string) (CredentialPage, error)
	UpdateCredential(context.Context, Credentials, int64) (Credentials, error)
	DeleteCredential(context.Context, string, int64) error
	RecordCredentialIdentity(context.Context, string, int64, Identity) (Credentials, error)
	CreatePlan(context.Context, Plan) (Plan, error)
	GetPlan(context.Context, string) (Plan, error)
	ListPlans(context.Context, int, string) (PlanPage, error)
	CreateChange(context.Context, Change) (Change, error)
	GetChange(context.Context, string) (Change, error)
	GetChangeByConfirmation(context.Context, string) (Change, error)
	ListChanges(context.Context, int, string, string) (ChangePage, error)
	UpdateChange(context.Context, Change, int64) (Change, error)
}
