package aws

// This file owns the lower, provider-typed boundary for execution secrets.
// It deliberately has no database or ProductCore dependency. A caller must
// durably reserve the exact intent before PutParameter and must supply an
// authorization resolver that proves a consumed secret_access approval.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const (
	ExecutionSecretPurposeAIProviderAPIKey = "ai_provider_api_key"
	SecretParameterDeliveryTargetSecure    = "target_secure_parameter"
	secretParameterSchema                  = "execution-secret-parameter/v1"
	secretParameterPrefix                  = "/dirextalk/execution-v2"
	maxExecutionSecretBytes                = 4096
)

var (
	ErrSecretParameterInvalid      = errors.New("aws secret parameter: invalid request")
	ErrSecretParameterUnauthorized = errors.New("aws secret parameter: authorization unavailable")
	ErrSecretParameterUncertain    = errors.New("aws secret parameter: outcome uncertain")
	ErrSecretParameterFailed       = errors.New("aws secret parameter: provider operation failed")
)

// AuthorizedSecretRequest contains only immutable, secret-free pins. Mode is
// provision while the confirmed secret.provision stage is running/uncertain,
// and consume when a later remote stage uses the already provisioned handle.
type AuthorizedSecretRequest struct {
	Mode                                             string
	OwnerID, PlanID, RunID, StageID, AttemptID       string
	PlanRevision, RunRevision, StageRevision         uint64
	PlanDigest, RunDigest, StageDigest, TargetDigest execution.Digest
	TargetID                                         string
	TargetRevision                                   uint64
	SecretRefs                                       []execution.CredentialRef
}

type SecretAccessAuthorization struct {
	RunID, StageID, ConfirmationID string
	Gate                           execution.Gate
	StageStatus                    execution.StageStatus
	SecretGrantDigest              execution.Digest
}

type AuthorizedSecretValue struct {
	Ref   execution.CredentialRef
	Value []byte
}

type AuthorizedSecretSet struct {
	Authorization SecretAccessAuthorization
	Values        []AuthorizedSecretValue
}

// AuthorizedSecretResolver is intentionally stronger than a raw secret
// lookup. Implementations must first prove the exact consumed/succeeded
// secret_access stage, then open the active encrypted revision transiently.
type AuthorizedSecretResolver interface {
	ResolveAuthorizedSecretValues(context.Context, AuthorizedSecretRequest) (AuthorizedSecretSet, error)
}

// SecretParameterLeaseResolver is the runtime half of the two-stage flow. It
// returns only a persisted parameter handle plus fresh authoritative proof
// that the distinct secret_access stage succeeded. It never opens plaintext.
type SecretParameterLeaseResolver interface {
	ResolveAuthorizedSecretParameterLease(context.Context, AuthorizedSecretRequest) (SecretParameterLease, SecretAccessAuthorization, error)
}

// SecretParameterLeaseRevoker performs exact, idempotent provider cleanup
// after the remote command is terminal. Implementations must also durably mark
// the lease revoked; a failure leaves the command outcome uncertain.
type SecretParameterLeaseRevoker interface {
	RevokeAuthorizedSecretParameter(context.Context, SecretParameterLease) error
}

type SecretParameterProvisionRequest struct {
	OwnerID            string
	PlanID             string
	PlanRevision       uint64
	PlanDigest         execution.Digest
	RunID              string
	RunRevision        uint64
	RunDigest          execution.Digest
	StageID            string
	StageRevision      uint64
	StageDigest        execution.Digest
	AttemptID          string
	StepRevision       uint64
	StepDigest         execution.Digest
	Target             execution.ExecutionTarget
	SecretRef          execution.CredentialRef
	Delivery           string
	FenceDigest        execution.Digest
	RequestDigest      execution.Digest
	Credential         Credentials
	CredentialID       string
	CredentialRevision uint64
}

// SecretParameterLease is safe to persist. It contains a deterministic
// parameter handle and authorization evidence, never plaintext or a secret
// fingerprint.
type SecretParameterLease struct {
	SchemaVersion      string                    `json:"schema_version"`
	OwnerID            string                    `json:"owner_id"`
	RunID              string                    `json:"run_id"`
	ProvisionStageID   string                    `json:"provision_stage_id"`
	ProvisionAttemptID string                    `json:"provision_attempt_id"`
	Authorization      SecretAccessAuthorization `json:"authorization"`
	TargetID           string                    `json:"target_id"`
	TargetRevision     uint64                    `json:"target_revision"`
	TargetDigest       execution.Digest          `json:"target_digest"`
	SecretRef          execution.CredentialRef   `json:"secret_ref"`
	ParameterName      string                    `json:"parameter_name"`
	ContainerMountPath string                    `json:"container_mount_path"`
	FenceDigest        execution.Digest          `json:"fence_digest"`
	RequestDigest      execution.Digest          `json:"request_digest"`
	ProviderVersion    int64                     `json:"provider_version"`
}

type SecretParameterIntent struct {
	OwnerID, ParameterName string
	FenceDigest            execution.Digest
	RequestDigest          execution.Digest
	Request                SecretParameterProvisionRequest
}

type SecretParameterIntentRecord struct {
	Intent          SecretParameterIntent
	Status          string
	ProviderVersion int64
	Revision        uint64
}

// SecretParameterIntentStore is the required durable seam. No production
// executor is ready until an implementation reserves the intent before the
// first provider mutation and can reload it for readback-only reconciliation.
type SecretParameterIntentStore interface {
	ReserveSecretParameterIntent(context.Context, SecretParameterIntent) (SecretParameterIntentRecord, bool, error)
	GetSecretParameterIntent(context.Context, string, execution.Digest) (SecretParameterIntentRecord, error)
	RecordSecretParameterVersion(context.Context, string, execution.Digest, int64) (SecretParameterIntentRecord, error)
	MarkSecretParameterUncertain(context.Context, string, execution.Digest) error
	CompleteSecretParameter(context.Context, SecretParameterLease) error
	RevokeSecretParameter(context.Context, string, execution.Digest) error
}

type SecretParameterReadback struct {
	Exists, Matches bool
	Version         int64
}

type SecretParameterProvider interface {
	Put(context.Context, SecretParameterProvisionRequest, string, []byte) (int64, error)
	Readback(context.Context, SecretParameterProvisionRequest, string, []byte) (SecretParameterReadback, error)
	Revoke(context.Context, SecretParameterProvisionRequest, string) error
}

type SecretParameterProvisionExecutor struct {
	store    SecretParameterIntentStore
	provider SecretParameterProvider
	secrets  AuthorizedSecretResolver
}

func NewSecretParameterProvisionExecutor(store SecretParameterIntentStore, provider SecretParameterProvider, secrets AuthorizedSecretResolver) (*SecretParameterProvisionExecutor, error) {
	if store == nil || provider == nil || secrets == nil {
		return nil, ErrSecretParameterInvalid
	}
	return &SecretParameterProvisionExecutor{store: store, provider: provider, secrets: secrets}, nil
}

func (e *SecretParameterProvisionExecutor) Ready() bool {
	return e != nil && e.store != nil && e.provider != nil && e.secrets != nil
}

func CanonicalSecretParameterRequestDigest(req SecretParameterProvisionRequest) (execution.Digest, error) {
	return execution.CanonicalDigest(struct {
		Schema                                                 string
		OwnerID, PlanID, RunID, StageID, AttemptID             string
		PlanRevision, RunRevision, StageRevision, StepRevision uint64
		PlanDigest, RunDigest, StageDigest, StepDigest         execution.Digest
		TargetID                                               string
		TargetRevision                                         uint64
		TargetDigest, FenceDigest                              execution.Digest
		SecretRef                                              execution.CredentialRef
		Delivery, CredentialID                                 string
		CredentialRevision                                     uint64
	}{secretParameterSchema, req.OwnerID, req.PlanID, req.RunID, req.StageID, req.AttemptID, req.PlanRevision, req.RunRevision, req.StageRevision, req.StepRevision, req.PlanDigest, req.RunDigest, req.StageDigest, req.StepDigest, req.Target.ID, req.Target.Revision, req.Target.Digest, req.FenceDigest, req.SecretRef, req.Delivery, req.CredentialID, req.CredentialRevision})
}

func SecretParameterName(targetID, attemptID string, ref execution.CredentialRef) (string, error) {
	if !execution.ValidateUUID(targetID) || !execution.ValidateUUID(attemptID) || !validExecutionSecretRef(ref) {
		return "", ErrSecretParameterInvalid
	}
	digest, err := execution.CanonicalDigest(struct {
		Ref, Purpose string
		Revision     uint64
		Binding      execution.Digest
	}{ref.Ref, ref.Purpose, ref.Revision, ref.BindingDigest})
	if err != nil {
		return "", ErrSecretParameterInvalid
	}
	return fmt.Sprintf("%s/%s/%s/%s", secretParameterPrefix, targetID, attemptID, string(digest)[:32]), nil
}

func SecretContainerMountPath(ref execution.CredentialRef) (string, error) {
	if !validExecutionSecretRef(ref) {
		return "", ErrSecretParameterInvalid
	}
	return "/run/secrets/dirextalk/" + ref.Purpose, nil
}

func (e *SecretParameterProvisionExecutor) Execute(ctx context.Context, req SecretParameterProvisionRequest) (SecretParameterLease, error) {
	var zero SecretParameterLease
	if !e.Ready() {
		return zero, ErrSecretParameterInvalid
	}
	normalized, intent, err := normalizeSecretParameterProvisionRequest(req)
	if err != nil {
		return zero, err
	}
	// Prove the independent secret_access gate before reserving any provider
	// mutation. This prevents an unauthorized invocation from poisoning the
	// deterministic intent key and blocking a later authorized attempt.
	set, values, err := e.resolve(ctx, normalized, "provision")
	if err != nil {
		return zero, err
	}
	defer zeroSecretValues(values)
	record, created, err := e.store.ReserveSecretParameterIntent(ctx, intent)
	if err != nil || record.Intent.OwnerID != intent.OwnerID || record.Intent.FenceDigest != intent.FenceDigest || record.Intent.RequestDigest != intent.RequestDigest || record.Intent.ParameterName != intent.ParameterName {
		return zero, ErrSecretParameterUncertain
	}
	if !created {
		return e.finishReadback(ctx, normalized, record, set, values[0])
	}
	version, err := e.provider.Put(ctx, normalized, intent.ParameterName, values[0])
	if err != nil || version <= 0 {
		_ = e.store.MarkSecretParameterUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		return zero, ErrSecretParameterUncertain
	}
	record, err = e.store.RecordSecretParameterVersion(ctx, intent.OwnerID, intent.FenceDigest, version)
	if err != nil || record.ProviderVersion != version {
		_ = e.store.MarkSecretParameterUncertain(ctx, intent.OwnerID, intent.FenceDigest)
		return zero, ErrSecretParameterUncertain
	}
	return e.finishReadback(ctx, normalized, record, set, values[0])
}

// Reconcile has no Put path. It reloads the exact durable intent, opens the
// same authorized immutable secret revision transiently, and performs only
// GetParameter readback.
func (e *SecretParameterProvisionExecutor) Reconcile(ctx context.Context, req SecretParameterProvisionRequest) (SecretParameterLease, error) {
	var zero SecretParameterLease
	if !e.Ready() {
		return zero, ErrSecretParameterInvalid
	}
	normalized, intent, err := normalizeSecretParameterProvisionRequest(req)
	if err != nil {
		return zero, err
	}
	record, err := e.store.GetSecretParameterIntent(ctx, intent.OwnerID, intent.FenceDigest)
	if err != nil || record.Intent.RequestDigest != intent.RequestDigest || record.Intent.ParameterName != intent.ParameterName {
		return zero, ErrSecretParameterUncertain
	}
	return e.reconcile(ctx, normalized, record)
}

func (e *SecretParameterProvisionExecutor) reconcile(ctx context.Context, req SecretParameterProvisionRequest, record SecretParameterIntentRecord) (SecretParameterLease, error) {
	set, values, err := e.resolve(ctx, req, "provision")
	if err != nil {
		return SecretParameterLease{}, err
	}
	defer zeroSecretValues(values)
	return e.finishReadback(ctx, req, record, set, values[0])
}

func (e *SecretParameterProvisionExecutor) finishReadback(ctx context.Context, req SecretParameterProvisionRequest, record SecretParameterIntentRecord, set AuthorizedSecretSet, value []byte) (SecretParameterLease, error) {
	readback, err := e.provider.Readback(ctx, req, record.Intent.ParameterName, value)
	if err != nil || !readback.Exists || !readback.Matches || readback.Version <= 0 || record.ProviderVersion > 0 && readback.Version != record.ProviderVersion {
		_ = e.store.MarkSecretParameterUncertain(ctx, req.OwnerID, req.FenceDigest)
		return SecretParameterLease{}, ErrSecretParameterUncertain
	}
	mount, err := SecretContainerMountPath(req.SecretRef)
	if err != nil {
		return SecretParameterLease{}, err
	}
	lease := SecretParameterLease{SchemaVersion: secretParameterSchema, OwnerID: req.OwnerID, RunID: req.RunID, ProvisionStageID: req.StageID, ProvisionAttemptID: req.AttemptID, Authorization: set.Authorization, TargetID: req.Target.ID, TargetRevision: req.Target.Revision, TargetDigest: req.Target.Digest, SecretRef: req.SecretRef, ParameterName: record.Intent.ParameterName, ContainerMountPath: mount, FenceDigest: req.FenceDigest, RequestDigest: req.RequestDigest, ProviderVersion: readback.Version}
	if err = validateSecretParameterLease(lease); err != nil || e.store.CompleteSecretParameter(ctx, lease) != nil {
		return SecretParameterLease{}, ErrSecretParameterUncertain
	}
	return lease, nil
}

// Revoke is an exact cleanup operation. It is safe to retry and does not
// expose the value. The durable lease is marked revoked only after provider
// deletion succeeds.
func (e *SecretParameterProvisionExecutor) Revoke(ctx context.Context, req SecretParameterProvisionRequest) error {
	if !e.Ready() {
		return ErrSecretParameterInvalid
	}
	normalized, intent, err := normalizeSecretParameterProvisionRequest(req)
	if err != nil {
		return err
	}
	record, err := e.store.GetSecretParameterIntent(ctx, intent.OwnerID, intent.FenceDigest)
	if err != nil || record.Intent.ParameterName != intent.ParameterName || record.Intent.RequestDigest != intent.RequestDigest {
		return ErrSecretParameterUncertain
	}
	if err = e.provider.Revoke(ctx, normalized, intent.ParameterName); err != nil {
		return ErrSecretParameterUncertain
	}
	if err = e.store.RevokeSecretParameter(ctx, intent.OwnerID, intent.FenceDigest); err != nil {
		return ErrSecretParameterUncertain
	}
	return nil
}

func (e *SecretParameterProvisionExecutor) resolve(ctx context.Context, req SecretParameterProvisionRequest, mode string) (AuthorizedSecretSet, [][]byte, error) {
	authReq := AuthorizedSecretRequest{Mode: mode, OwnerID: req.OwnerID, PlanID: req.PlanID, PlanRevision: req.PlanRevision, PlanDigest: req.PlanDigest, RunID: req.RunID, RunRevision: req.RunRevision, RunDigest: req.RunDigest, StageID: req.StageID, StageRevision: req.StageRevision, StageDigest: req.StageDigest, AttemptID: req.AttemptID, TargetID: req.Target.ID, TargetRevision: req.Target.Revision, TargetDigest: req.Target.Digest, SecretRefs: []execution.CredentialRef{req.SecretRef}}
	set, err := e.secrets.ResolveAuthorizedSecretValues(ctx, authReq)
	if err != nil || validateAuthorizedSecretSet(authReq, set) != nil {
		zeroAuthorizedSet(&set)
		return AuthorizedSecretSet{}, nil, ErrSecretParameterUnauthorized
	}
	authorization := set.Authorization
	values := make([][]byte, len(set.Values))
	for i := range set.Values {
		values[i] = append([]byte(nil), set.Values[i].Value...)
	}
	zeroAuthorizedSet(&set)
	return AuthorizedSecretSet{Authorization: authorization}, values, nil
}

func normalizeSecretParameterProvisionRequest(req SecretParameterProvisionRequest) (SecretParameterProvisionRequest, SecretParameterIntent, error) {
	var empty SecretParameterIntent
	if !validOwner(req.OwnerID) || !execution.ValidateUUID(req.PlanID) || req.PlanRevision == 0 || !req.PlanDigest.Valid() || !execution.ValidateUUID(req.RunID) || req.RunRevision == 0 || !req.RunDigest.Valid() || !execution.ValidateUUID(req.StageID) || req.StageRevision == 0 || !req.StageDigest.Valid() || !execution.ValidateUUID(req.AttemptID) || req.StepRevision == 0 || !req.StepDigest.Valid() || !req.FenceDigest.Valid() || req.Delivery != SecretParameterDeliveryTargetSecure || !validExecutionSecretRef(req.SecretRef) {
		return SecretParameterProvisionRequest{}, empty, ErrSecretParameterInvalid
	}
	target, err := NormalizeInfrastructureTarget(req.Target)
	if err != nil || target.ID == "" || target.Kind != execution.TargetKindAWSEC2Instance || target.Revision != req.Target.Revision || target.Digest != req.Target.Digest {
		return SecretParameterProvisionRequest{}, empty, ErrSecretParameterInvalid
	}
	if validateCredentialBinding(req.Credential, req.CredentialID, req.CredentialRevision, target.AccountID, target.Region) != nil {
		return SecretParameterProvisionRequest{}, empty, ErrSecretParameterInvalid
	}
	name, err := SecretParameterName(target.ID, req.AttemptID, req.SecretRef)
	if err != nil {
		return SecretParameterProvisionRequest{}, empty, err
	}
	req.Target = target
	digest, err := CanonicalSecretParameterRequestDigest(req)
	if err != nil || req.RequestDigest != "" && req.RequestDigest != digest {
		return SecretParameterProvisionRequest{}, empty, ErrSecretParameterInvalid
	}
	req.RequestDigest = digest
	return req, SecretParameterIntent{OwnerID: req.OwnerID, ParameterName: name, FenceDigest: req.FenceDigest, RequestDigest: digest, Request: redactedSecretParameterRequest(req)}, nil
}

func redactedSecretParameterRequest(req SecretParameterProvisionRequest) SecretParameterProvisionRequest {
	req.Credential = Credentials{ID: req.Credential.ID, Name: req.Credential.Name, Region: req.Credential.Region, AccountID: req.Credential.AccountID, UserARN: req.Credential.UserARN, Revision: req.Credential.Revision, VerifiedRevision: req.Credential.VerifiedRevision}
	return req
}

func validateAuthorizedSecretSet(req AuthorizedSecretRequest, set AuthorizedSecretSet) error {
	if req.Mode != "provision" && req.Mode != "consume" || len(req.SecretRefs) != 1 || len(set.Values) != 1 || set.Values[0].Ref != req.SecretRefs[0] || !validExecutionSecretValue(set.Values[0].Value) || validateSecretAccessAuthorization(req, set.Authorization) != nil {
		return ErrSecretParameterUnauthorized
	}
	return nil
}

func validateSecretAccessAuthorization(req AuthorizedSecretRequest, authorization SecretAccessAuthorization) error {
	if authorization.RunID != req.RunID || authorization.Gate != execution.GateSecretAccess || !execution.ValidateUUID(authorization.StageID) || !execution.ValidateUUID(authorization.ConfirmationID) {
		return ErrSecretParameterUnauthorized
	}
	digest, err := execution.CanonicalDigest(req.SecretRefs)
	if err != nil || authorization.SecretGrantDigest != digest {
		return ErrSecretParameterUnauthorized
	}
	if req.Mode == "provision" {
		if authorization.StageID != req.StageID || authorization.StageStatus != execution.StageRunning && authorization.StageStatus != execution.StageUncertain {
			return ErrSecretParameterUnauthorized
		}
	} else if authorization.StageID == req.StageID || authorization.StageStatus != execution.StageSucceeded {
		return ErrSecretParameterUnauthorized
	}
	return nil
}

func validateConsumedSecretParameterLease(req AuthorizedSecretRequest, lease SecretParameterLease, authorization SecretAccessAuthorization) error {
	if req.Mode != "consume" || len(req.SecretRefs) != 1 || validateSecretAccessAuthorization(req, authorization) != nil || validateSecretParameterLease(lease) != nil || lease.OwnerID != req.OwnerID || lease.RunID != req.RunID || lease.ProvisionStageID != authorization.StageID || lease.TargetID != req.TargetID || lease.TargetRevision != req.TargetRevision || lease.TargetDigest != req.TargetDigest || lease.SecretRef != req.SecretRefs[0] {
		return ErrSecretParameterUnauthorized
	}
	return nil
}

func validExecutionSecretRef(ref execution.CredentialRef) bool {
	return execution.ValidateUUID(ref.Ref) && ref.Purpose == ExecutionSecretPurposeAIProviderAPIKey && ref.Revision > 0 && ref.BindingDigest.Valid()
}

func validExecutionSecretValue(value []byte) bool {
	return len(value) > 0 && len(value) <= maxExecutionSecretBytes && utf8.Valid(value) && !strings.ContainsAny(string(value), "\x00\r\n")
}

func validateSecretParameterLease(lease SecretParameterLease) error {
	expectedName, err := SecretParameterName(lease.TargetID, lease.ProvisionAttemptID, lease.SecretRef)
	if err != nil {
		return err
	}
	if lease.SchemaVersion != secretParameterSchema || !validOwner(lease.OwnerID) || !execution.ValidateUUID(lease.RunID) || !execution.ValidateUUID(lease.ProvisionStageID) || !execution.ValidateUUID(lease.ProvisionAttemptID) || !execution.ValidateUUID(lease.TargetID) || lease.TargetRevision == 0 || !lease.TargetDigest.Valid() || lease.ParameterName != expectedName || lease.ContainerMountPath != "/run/secrets/dirextalk/"+lease.SecretRef.Purpose || !lease.FenceDigest.Valid() || !lease.RequestDigest.Valid() || lease.ProviderVersion <= 0 {
		return ErrSecretParameterInvalid
	}
	return nil
}

func zeroSecretValues(values [][]byte) {
	for i := range values {
		for j := range values[i] {
			values[i][j] = 0
		}
	}
}

func zeroAuthorizedSet(set *AuthorizedSecretSet) {
	if set == nil {
		return
	}
	for i := range set.Values {
		for j := range set.Values[i].Value {
			set.Values[i].Value[j] = 0
		}
	}
	set.Values = nil
}

// SSMSecretParameterClient is deliberately separated from command dispatch.
// Production uses one-attempt writes; ambiguous writes are reconciled only by
// GetParameter through the durable intent executor above.
type SSMSecretParameterClient interface {
	PutParameter(context.Context, *ssm.PutParameterInput, ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	DeleteParameter(context.Context, *ssm.DeleteParameterInput, ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
}

type SSMSecretParameterClientFactory interface {
	NewSSMSecretParameter(Credentials) (SSMSecretParameterClient, error)
}

type SDKSecretParameterProvider struct {
	factory SSMSecretParameterClientFactory
}

func NewSDKSecretParameterProvider(factory SSMSecretParameterClientFactory) (*SDKSecretParameterProvider, error) {
	if factory == nil {
		return nil, ErrSecretParameterInvalid
	}
	return &SDKSecretParameterProvider{factory: factory}, nil
}

func (p *SDKSecretParameterProvider) Put(ctx context.Context, req SecretParameterProvisionRequest, name string, value []byte) (int64, error) {
	if p == nil || p.factory == nil || !validExecutionSecretValue(value) || name == "" {
		return 0, ErrSecretParameterInvalid
	}
	client, err := p.factory.NewSSMSecretParameter(req.Credential)
	if err != nil || client == nil {
		return 0, ErrSecretParameterUncertain
	}
	out, err := client.PutParameter(ctx, &ssm.PutParameterInput{Name: awsapi.String(name), Value: awsapi.String(string(value)), Type: ssmtypes.ParameterTypeSecureString, Overwrite: awsapi.Bool(false), Description: awsapi.String("Dirextalk execution.v2 target-scoped secret")})
	if err != nil || out == nil || out.Version <= 0 {
		return 0, ErrSecretParameterUncertain
	}
	return out.Version, nil
}

func (p *SDKSecretParameterProvider) Readback(ctx context.Context, req SecretParameterProvisionRequest, name string, expected []byte) (SecretParameterReadback, error) {
	if p == nil || p.factory == nil || !validExecutionSecretValue(expected) || name == "" {
		return SecretParameterReadback{}, ErrSecretParameterInvalid
	}
	client, err := p.factory.NewSSMSecretParameter(req.Credential)
	if err != nil || client == nil {
		return SecretParameterReadback{}, ErrSecretParameterUncertain
	}
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: awsapi.String(name), WithDecryption: awsapi.Bool(true)})
	if err != nil || out == nil || out.Parameter == nil || awsapi.ToString(out.Parameter.Name) != name || out.Parameter.Type != ssmtypes.ParameterTypeSecureString || out.Parameter.Version <= 0 {
		return SecretParameterReadback{}, ErrSecretParameterUncertain
	}
	actual := []byte(awsapi.ToString(out.Parameter.Value))
	matched := len(actual) == len(expected) && subtle.ConstantTimeCompare(actual, expected) == 1
	for i := range actual {
		actual[i] = 0
	}
	return SecretParameterReadback{Exists: true, Matches: matched, Version: out.Parameter.Version}, nil
}

func (p *SDKSecretParameterProvider) Revoke(ctx context.Context, req SecretParameterProvisionRequest, name string) error {
	if p == nil || p.factory == nil || name == "" {
		return ErrSecretParameterInvalid
	}
	client, err := p.factory.NewSSMSecretParameter(req.Credential)
	if err != nil || client == nil {
		return ErrSecretParameterUncertain
	}
	if _, err = client.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: awsapi.String(name)}); err != nil {
		var missing *ssmtypes.ParameterNotFound
		if !errors.As(err, &missing) {
			return ErrSecretParameterUncertain
		}
	}
	return nil
}

var _ SecretParameterProvider = (*SDKSecretParameterProvider)(nil)
