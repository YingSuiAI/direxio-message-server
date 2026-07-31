package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalid             = errors.New("coreaws: invalid")
	ErrNotFound            = errors.New("coreaws: not found")
	ErrConflict            = errors.New("coreaws: conflict")
	ErrRevisionConflict    = errors.New("coreaws: revision conflict")
	ErrIdempotencyConflict = errors.New("coreaws: idempotency conflict")
	ErrProvider            = errors.New("coreaws: provider operation failed")
)

type Credentials struct {
	ID, Name, Region           string
	private                    *credentialPayload
	AccountID, UserARN         string
	VerifiedRevision, Revision int64
	CreatedAt, UpdatedAt       time.Time
}

type credentialPayload struct{ accessKeyID, secretAccessKey, sessionToken string }

// CredentialHandle is the only form accepted by provider clients. Its secret
// payload remains private to this package and is never serialized.
type CredentialHandle struct {
	credential *credentialPayload
	Region     string
	AccountID  string
	UserARN    string
}

func (c Credentials) handle() CredentialHandle {
	if c.private == nil {
		return CredentialHandle{Region: c.Region, AccountID: c.AccountID, UserARN: c.UserARN}
	}
	return CredentialHandle{credential: &credentialPayload{c.private.accessKeyID, c.private.secretAccessKey, c.private.sessionToken}, Region: c.Region, AccountID: c.AccountID, UserARN: c.UserARN}
}

func RehydrateCredentials(id, name, region, accountID, userARN string, accessKeyID, secretAccessKey, sessionToken []byte, verifiedRevision, revision int64, createdAt, updatedAt time.Time) Credentials {
	return Credentials{ID: id, Name: name, Region: region, AccountID: accountID, UserARN: userARN, VerifiedRevision: verifiedRevision, Revision: revision, CreatedAt: createdAt, UpdatedAt: updatedAt, private: &credentialPayload{string(accessKeyID), string(secretAccessKey), string(sessionToken)}}
}

func RehydrateCredentialMetadata(id, name, region, accountID, userARN string, verifiedRevision, revision int64, createdAt, updatedAt time.Time) Credentials {
	return Credentials{ID: id, Name: name, Region: region, AccountID: accountID, UserARN: userARN, VerifiedRevision: verifiedRevision, Revision: revision, CreatedAt: createdAt, UpdatedAt: updatedAt}
}

func (c Credentials) String() string   { return "[redacted-coreaws-credentials]" }
func (c Credentials) GoString() string { return "coreaws.Credentials{[redacted]}" }
func (c Credentials) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID, Name, Region, AccountID, UserARN        string
		HasAccessKey, HasSecretKey, HasSessionToken bool
		Revision                                    int64
	}{c.ID, c.Name, c.Region, c.AccountID, c.UserARN, c.private != nil && c.private.accessKeyID != "", c.private != nil && c.private.secretAccessKey != "", c.private != nil && c.private.sessionToken != "", c.Revision})
}

type CredentialView struct {
	ID, Name, Region, AccountID, UserARN                                   string
	HasAccessKey, HasSecretKey, HasSessionToken                            bool
	AccessKeyConfigured, SecretAccessKeyConfigured, SessionTokenConfigured bool
	Revision, VerifiedRevision                                             int64
	CreatedAt, UpdatedAt                                                   time.Time
}

func (c Credentials) View() CredentialView {
	access := c.private != nil && c.private.accessKeyID != ""
	secret := c.private != nil && c.private.secretAccessKey != ""
	session := c.private != nil && c.private.sessionToken != ""
	return CredentialView{ID: c.ID, Name: c.Name, Region: c.Region, AccountID: c.AccountID, UserARN: c.UserARN, HasAccessKey: access, HasSecretKey: secret, HasSessionToken: session, AccessKeyConfigured: access, SecretAccessKeyConfigured: secret, SessionTokenConfigured: session, Revision: c.Revision, VerifiedRevision: c.VerifiedRevision, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

func (c Credentials) StoredSecretBytes() (accessKeyID, secretAccessKey, sessionToken []byte) {
	if c.private == nil {
		return nil, nil, nil
	}
	return []byte(c.private.accessKeyID), []byte(c.private.secretAccessKey), []byte(c.private.sessionToken)
}

func (c Credentials) Validate() error {
	if !validUUID(c.ID) || strings.TrimSpace(c.Name) == "" || !validRegion(c.Region) || c.private == nil || c.private.accessKeyID == "" || c.private.secretAccessKey == "" || c.Revision < 1 {
		return ErrInvalid
	}
	return nil
}

type Identity struct {
	AccountID, UserARN, PrincipalID string
}

type CredentialTest struct {
	CredentialID       string
	Identity           Identity
	CredentialRevision int64
	TestedAt           time.Time
}

type Page[T any] struct {
	Items         []T
	NextPageToken string
}

type CredentialPage = Page[CredentialView]
type CredentialInput struct{ ID, Name, Region, AccessKeyID, SecretAccessKey, SessionToken, IdempotencyKey string }

func (in CredentialInput) String() string   { return "[redacted-coreaws-credential-input]" }
func (in CredentialInput) GoString() string { return "coreaws.CredentialInput{[redacted]}" }
func (in CredentialInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID, Name, Region, IdempotencyKey            string
		HasAccessKey, HasSecretKey, HasSessionToken bool
	}{in.ID, in.Name, in.Region, in.IdempotencyKey, in.AccessKeyID != "", in.SecretAccessKey != "", in.SessionToken != ""})
}
func (in CredentialInput) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", in.ID), slog.String("name", in.Name), slog.String("region", in.Region), slog.String("idempotency_key", in.IdempotencyKey), slog.Bool("has_access_key", in.AccessKeyID != ""), slog.Bool("has_secret_key", in.SecretAccessKey != ""), slog.Bool("has_session_token", in.SessionToken != ""))
}

func credentialInputDigest(in CredentialInput) string {
	return canonicalDigest(struct {
		ID, Name, Region, IdempotencyKey                               string
		AccessKeyFingerprint, SecretKeyFingerprint, SessionFingerprint string
		HasAccessKey, HasSecretKey, HasSessionToken                    bool
	}{in.ID, in.Name, in.Region, in.IdempotencyKey, secretFingerprint(in.AccessKeyID), secretFingerprint(in.SecretAccessKey), secretFingerprint(in.SessionToken), in.AccessKeyID != "", in.SecretAccessKey != "", in.SessionToken != ""})
}

func secretFingerprint(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validRegion(s string) bool {
	return regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z]+-\d$`).MatchString(strings.TrimSpace(s))
}
func validUUID(s string) bool {
	u, err := uuid.Parse(strings.TrimSpace(s))
	return err == nil && u != uuid.Nil && u.String() == strings.TrimSpace(s)
}
func canonicalDigest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
