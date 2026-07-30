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
	CreateProvision(context.Context, Provision) (Provision, error)
	// RetryProvision explicitly re-arms a failed/destroyed deterministic
	// provision after an owner-supplied revision and idempotency fence. It does
	// not overwrite the row or erase prior change/event history.
	RetryProvision(context.Context, string, int64, string) (Provision, error)
	GetProvision(context.Context, string) (Provision, error)
	GetProvisionByChange(context.Context, string) (Provision, error)
}

// EC2ProvisionRepository is the narrow atomic boundary for typed EC2 plans.
// Implementations must persist the immutable plan and its planned provision in
// one transaction and replay an identical idempotency key exactly.
type EC2ProvisionRepository interface {
	Repository
	CreateEC2Provision(context.Context, Plan, Provision, string, string) (Plan, Provision, error)
	CreateDerivedDeletePlan(context.Context, Plan) (Plan, error)
	ListProvisions(context.Context, string, string, int, string) (Page[Provision], error)
	ListProvisionEvents(context.Context, string, string, uint64, int) ([]ProvisionEvent, uint64, error)
}
