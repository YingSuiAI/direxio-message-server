package aws

import "context"

// Repository is the owner-scoped credential persistence boundary. Secrets are
// accepted only through Credentials and must be encrypted by the adapter.
type Repository interface {
	CreateCredential(context.Context, Credentials) (Credentials, error)
	GetCredential(context.Context, string) (Credentials, error)
	GetCredentialRevision(context.Context, string, int64) (Credentials, error)
	ListCredentials(context.Context, int, string) (CredentialPage, error)
	UpdateCredential(context.Context, Credentials, int64) (Credentials, error)
	DeleteCredential(context.Context, string, int64) error
	RecordCredentialIdentity(context.Context, string, int64, Identity) (Credentials, error)
}

type CredentialReplayRepository interface {
	ReplayCredential(context.Context, string, string, string) (CredentialView, bool, error)
	ReplayCredentialTest(context.Context, string, int64, string, string) (CredentialTest, bool, error)
	SaveCredentialIdempotent(context.Context, Credentials, string, string) (CredentialView, error)
	ReplaceCredentialIdempotent(context.Context, Credentials, int64, string, string) (CredentialView, error)
	DeleteCredentialIdempotent(context.Context, string, int64, string, string) error
	TestCredentialIdempotent(context.Context, string, int64, Identity, string, string) (CredentialTest, error)
}

type CredentialRevisionRef struct {
	ID       string
	Revision int64
}
type CredentialMetadataBatchRepository interface {
	ListCredentialRevisionMetadata(context.Context, []CredentialRevisionRef) (map[string]Credentials, error)
}

type credentialReplayRepository = CredentialReplayRepository
