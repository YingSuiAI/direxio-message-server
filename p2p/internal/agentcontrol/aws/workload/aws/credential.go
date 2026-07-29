// Package aws contains typed, opt-in AWS workload providers. It deliberately
// exposes no generic AWS operation or endpoint override.
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
)

var (
	ErrInvalid      = coreworkload.ErrInvalid
	ErrProvider     = coreworkload.ErrProvider
	ErrUncertain    = errors.New("coreworkload/aws: provider response uncertain")
	ErrPrecondition = errors.New("coreworkload/aws: precondition failed")
)

// CredentialHandle is populated by the Agent credential store. Providers
// accept this handle only; they never consult environment, profile, IMDS, or
// endpoint overrides.
type CredentialHandle struct {
	ReferenceID                                string
	Revision                                   int64
	Region, AccountID                          string
	PrincipalARN                               string
	AccessKeyID, SecretAccessKey, SessionToken string
}

func (h CredentialHandle) Validate() error {
	if strings.TrimSpace(h.ReferenceID) == "" || h.Revision < 1 || strings.TrimSpace(h.Region) == "" || strings.TrimSpace(h.AccountID) == "" || strings.TrimSpace(h.PrincipalARN) == "" || h.AccessKeyID == "" || h.SecretAccessKey == "" {
		return ErrInvalid
	}
	return nil
}

func (h CredentialHandle) String() string   { return "[redacted-coreworkload-credential]" }
func (h CredentialHandle) GoString() string { return "aws.CredentialHandle{[redacted]}" }
func (h CredentialHandle) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ReferenceID, Region, AccountID, PrincipalARN string
		Revision                                     int64
		HasAccessKey, HasSecretKey, HasSessionToken  bool
	}{h.ReferenceID, h.Region, h.AccountID, h.PrincipalARN, h.Revision, h.AccessKeyID != "", h.SecretAccessKey != "", h.SessionToken != ""})
}

type CredentialResolver interface {
	ResolveCredential(context.Context, string, int64) (CredentialHandle, error)
}
type SecretResolver interface {
	ResolveSecretReference(context.Context, string) (string, error)
}

// CredentialStore is the narrow durable read seam used by the production
// resolver. Implementations must materialize secret bytes only for this call.
type CredentialStore interface {
	GetCredentialRevision(context.Context, string, int64) (coreaws.Credentials, error)
}

// DurableCredentialResolver resolves one exact, verified credential revision
// for an operation. It deliberately does not cache, serialize, or log the
// returned secret material.
type DurableCredentialResolver struct{ store CredentialStore }

type PostgresCredentialResolver = DurableCredentialResolver

func NewCredentialResolver(store CredentialStore) (*DurableCredentialResolver, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	return &DurableCredentialResolver{store: store}, nil
}

func NewPostgresCredentialResolver(store CredentialStore) (*DurableCredentialResolver, error) {
	return NewCredentialResolver(store)
}

func (r *DurableCredentialResolver) ResolveCredential(ctx context.Context, ref string, revision int64) (CredentialHandle, error) {
	if r == nil || r.store == nil || !canonicalUUID(ref) || revision < 1 {
		return CredentialHandle{}, ErrPrecondition
	}
	c, err := r.store.GetCredentialRevision(ctx, ref, revision)
	if err != nil || c.ID != ref || c.Revision != revision || c.VerifiedRevision != revision || strings.TrimSpace(c.Region) == "" || strings.TrimSpace(c.AccountID) == "" || strings.TrimSpace(c.UserARN) == "" {
		return CredentialHandle{}, ErrPrecondition
	}
	access, secret, session := c.StoredSecretBytes()
	h := CredentialHandle{ReferenceID: ref, Revision: revision, Region: c.Region, AccountID: c.AccountID, PrincipalARN: c.UserARN, AccessKeyID: string(access), SecretAccessKey: string(secret), SessionToken: string(session)}
	if err := h.Validate(); err != nil {
		return CredentialHandle{}, ErrPrecondition
	}
	return h, nil
}

func canonicalUUID(v string) bool {
	u, err := uuid.Parse(strings.TrimSpace(v))
	return err == nil && u != uuid.Nil && u.String() == v
}

// CanonicalSecretReference is an ARN-only resolver. A lookup function may
// translate a durable reference ID to an ARN, but plaintext values are never
// accepted or returned.
type CanonicalSecretReference struct {
	Lookup func(context.Context, string) (string, error)
}

func (r CanonicalSecretReference) ResolveSecretReference(ctx context.Context, ref string) (string, error) {
	if _, ok := coreworkload.CanonicalAWSSecretARN(ref); !ok {
		return "", ErrPrecondition
	}
	if r.Lookup == nil {
		return canonicalSecretARN(ref)
	}
	value, err := r.Lookup(ctx, ref)
	if err != nil {
		return "", ErrPrecondition
	}
	return canonicalSecretARN(value)
}

// ResolveSecretReferenceExact is optional provider binding evidence. The
// reference, purpose, and digest are checked before the ARN is released.
func (r CanonicalSecretReference) ResolveSecretReferenceExact(ctx context.Context, ref string, purpose coreconfirmation.SecretPurpose, binding coreconfirmation.Digest) (string, error) {
	if !binding.Valid() || !validSecretPurpose(purpose) || string(binding) != coreworkload.SecretGrantBindingDigest(ref, purpose) {
		return "", ErrPrecondition
	}
	return r.ResolveSecretReference(ctx, ref)
}

type ParsedSecretARN struct{ Partition, Service, Region, Account, Resource string }

var arnPattern = regexp.MustCompile(`^arn:([a-z0-9-]+):(secretsmanager|ssm):([a-z0-9-]+):([0-9]{12}):(.+)$`)

func canonicalSecretARN(v string) (string, error) {
	if canonical, ok := coreworkload.CanonicalAWSSecretARN(v); !ok {
		return "", ErrPrecondition
	} else {
		v = canonical
	}
	if v != strings.TrimSpace(v) {
		return "", ErrPrecondition
	}
	m := arnPattern.FindStringSubmatch(v)
	if m == nil || strings.ContainsAny(v, "\r\n\x00 \t") || strings.Contains(m[5], "//") {
		return "", ErrPrecondition
	}
	if m[1] != "aws" && m[1] != "aws-us-gov" && m[1] != "aws-cn" {
		return "", ErrPrecondition
	}
	if m[2] == "secretsmanager" && !strings.HasPrefix(m[5], "secret:") || m[2] == "ssm" && !strings.HasPrefix(m[5], "parameter/") {
		return "", ErrPrecondition
	}
	return fmt.Sprintf("arn:%s:%s:%s:%s:%s", m[1], m[2], m[3], m[4], m[5]), nil
}

func ParseSecretARN(v string) (ParsedSecretARN, error) {
	canonical, err := canonicalSecretARN(v)
	if err != nil {
		return ParsedSecretARN{}, err
	}
	m := arnPattern.FindStringSubmatch(canonical)
	return ParsedSecretARN{Partition: m[1], Service: m[2], Region: m[3], Account: m[4], Resource: m[5]}, nil
}

func ValidateSecretARNForTarget(v, region, account string) error {
	p, err := ParseSecretARN(v)
	if err != nil || p.Region != strings.TrimSpace(region) || p.Account != strings.TrimSpace(account) || p.Partition != ExpectedAWSPartition(region) {
		return ErrPrecondition
	}
	return nil
}

func ExpectedAWSPartition(region string) string {
	region = strings.TrimSpace(region)
	if strings.HasPrefix(region, "cn-") {
		return "aws-cn"
	}
	if strings.HasPrefix(region, "us-gov-") {
		return "aws-us-gov"
	}
	return "aws"
}

// CredentialReference extracts the plan-pinned credential revision. Providers
// must pass both fields to the resolver, never consult a mutable latest row.
func CredentialReference(plan coreworkload.Plan) (string, int64, error) {
	var ref string
	var revision int64
	for _, grant := range plan.SecretGrantRefs {
		if grant.Purpose != coreconfirmation.SecretPurposeAWSCredential {
			continue
		}
		if ref != "" || strings.TrimSpace(grant.ReferenceID) == "" || grant.Revision < 1 {
			return "", 0, ErrInvalid
		}
		ref = grant.ReferenceID
		revision = grant.Revision
	}
	if ref == "" {
		return "", 0, ErrPrecondition
	}
	return ref, revision, nil
}

func resolveApplicationRefs(ctx context.Context, plan coreworkload.Plan, resolver SecretResolver) error {
	for _, grant := range plan.SecretGrantRefs {
		if !validSecretPurpose(grant.Purpose) {
			return ErrPrecondition
		}
		if grant.Purpose == coreconfirmation.SecretPurposeAWSCredential {
			continue
		}
		if _, ok := coreworkload.CanonicalAWSSecretARN(grant.ReferenceID); !ok {
			return ErrPrecondition
		}
		if resolver == nil {
			return ErrPrecondition
		}
		if !grant.BindingDigest.Valid() {
			return ErrPrecondition
		}
		var arn string
		var err error
		if exact, ok := resolver.(interface {
			ResolveSecretReferenceExact(context.Context, string, coreconfirmation.SecretPurpose, coreconfirmation.Digest) (string, error)
		}); ok {
			arn, err = exact.ResolveSecretReferenceExact(ctx, grant.ReferenceID, grant.Purpose, grant.BindingDigest)
		} else {
			arn, err = resolver.ResolveSecretReference(ctx, grant.ReferenceID)
		}
		if err != nil || !validSecretARN(arn) || ValidateSecretARNForTarget(arn, plan.Target.Region, plan.Target.AccountID) != nil {
			return ErrPrecondition
		}
	}
	return nil
}

func ResolveApplicationRefs(ctx context.Context, plan coreworkload.Plan, resolver SecretResolver) error {
	return resolveApplicationRefs(ctx, plan, resolver)
}

func validSecretARN(v string) bool {
	_, err := canonicalSecretARN(v)
	return err == nil
}

func validSecretPurpose(v coreconfirmation.SecretPurpose) bool {
	switch v {
	case coreconfirmation.SecretPurposeModelAPIKey, coreconfirmation.SecretPurposeMCPCredential, coreconfirmation.SecretPurposeSkillSecret, coreconfirmation.SecretPurposeAWSCredential, coreconfirmation.SecretPurposeOtherExtensionSecret:
		return true
	default:
		return false
	}
}

var _ CredentialResolver = (*DurableCredentialResolver)(nil)
var _ SecretResolver = CanonicalSecretReference{}
