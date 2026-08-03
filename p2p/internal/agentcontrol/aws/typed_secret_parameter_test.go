package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type secretIntentStoreFake struct {
	record                       SecretParameterIntentRecord
	reserve, versions, uncertain int
	complete, revoked            int
	completed                    SecretParameterLease
}

func (s *secretIntentStoreFake) ReserveSecretParameterIntent(_ context.Context, in SecretParameterIntent) (SecretParameterIntentRecord, bool, error) {
	s.reserve++
	if s.record.Intent.OwnerID != "" {
		return s.record, false, nil
	}
	s.record = SecretParameterIntentRecord{Intent: in, Status: "reserved", Revision: 1}
	return s.record, true, nil
}
func (s *secretIntentStoreFake) GetSecretParameterIntent(_ context.Context, owner string, fence execution.Digest) (SecretParameterIntentRecord, error) {
	if s.record.Intent.OwnerID != owner || s.record.Intent.FenceDigest != fence {
		return SecretParameterIntentRecord{}, execution.ErrNotFound
	}
	return s.record, nil
}
func (s *secretIntentStoreFake) RecordSecretParameterVersion(_ context.Context, owner string, fence execution.Digest, version int64) (SecretParameterIntentRecord, error) {
	if s.record.Intent.OwnerID != owner || s.record.Intent.FenceDigest != fence || version <= 0 {
		return SecretParameterIntentRecord{}, execution.ErrConflict
	}
	s.versions++
	s.record.ProviderVersion = version
	s.record.Status = "dispatched"
	s.record.Revision++
	return s.record, nil
}
func (s *secretIntentStoreFake) MarkSecretParameterUncertain(_ context.Context, owner string, fence execution.Digest) error {
	if s.record.Intent.OwnerID != owner || s.record.Intent.FenceDigest != fence {
		return execution.ErrConflict
	}
	s.uncertain++
	s.record.Status = "uncertain"
	return nil
}
func (s *secretIntentStoreFake) CompleteSecretParameter(_ context.Context, lease SecretParameterLease) error {
	s.complete++
	s.completed = lease
	s.record.Status = "complete"
	return nil
}
func (s *secretIntentStoreFake) RevokeSecretParameter(_ context.Context, owner string, fence execution.Digest) error {
	if s.record.Intent.OwnerID != owner || s.record.Intent.FenceDigest != fence {
		return execution.ErrConflict
	}
	s.revoked++
	s.record.Status = "revoked"
	return nil
}

type authorizedSecretFake struct {
	value    []byte
	mutate   func(*SecretAccessAuthorization)
	requests []AuthorizedSecretRequest
}

func (s *authorizedSecretFake) ResolveAuthorizedSecretValues(_ context.Context, req AuthorizedSecretRequest) (AuthorizedSecretSet, error) {
	s.requests = append(s.requests, req)
	digest, _ := execution.CanonicalDigest(req.SecretRefs)
	status := execution.StageRunning
	stageID := req.StageID
	if req.Mode == "consume" {
		status = execution.StageSucceeded
		stageID = "99999999-9999-4999-8999-999999999999"
	}
	auth := SecretAccessAuthorization{RunID: req.RunID, StageID: stageID, ConfirmationID: "88888888-8888-4888-8888-888888888888", Gate: execution.GateSecretAccess, StageStatus: status, SecretGrantDigest: digest}
	if s.mutate != nil {
		s.mutate(&auth)
	}
	return AuthorizedSecretSet{Authorization: auth, Values: []AuthorizedSecretValue{{Ref: req.SecretRefs[0], Value: append([]byte(nil), s.value...)}}}, nil
}

type secretParameterProviderFake struct {
	store                      *secretIntentStoreFake
	puts, reads, revokes       int
	putErr, readErr, revokeErr error
	value                      []byte
	name                       string
}

func (p *secretParameterProviderFake) Put(_ context.Context, _ SecretParameterProvisionRequest, name string, value []byte) (int64, error) {
	if p.store == nil || p.store.reserve == 0 {
		return 0, errors.New("put before durable intent")
	}
	p.puts++
	p.name = name
	p.value = append([]byte(nil), value...)
	if p.putErr != nil {
		return 0, p.putErr
	}
	return 7, nil
}
func (p *secretParameterProviderFake) Readback(_ context.Context, _ SecretParameterProvisionRequest, name string, expected []byte) (SecretParameterReadback, error) {
	p.reads++
	if p.readErr != nil {
		return SecretParameterReadback{}, p.readErr
	}
	return SecretParameterReadback{Exists: len(p.value) > 0, Matches: name == p.name && bytes.Equal(expected, p.value), Version: 7}, nil
}
func (p *secretParameterProviderFake) Revoke(_ context.Context, _ SecretParameterProvisionRequest, name string) error {
	p.revokes++
	if p.revokeErr != nil || name != p.name {
		return ErrSecretParameterUncertain
	}
	p.value = nil
	return nil
}

func secretParameterFixture(t *testing.T) (SecretParameterProvisionRequest, *secretIntentStoreFake, *secretParameterProviderFake, *authorizedSecretFake, *SecretParameterProvisionExecutor) {
	t.Helper()
	now := time.Date(2036, 1, 2, 3, 4, 5, 0, time.UTC)
	credentialID := "11111111-1111-4111-8111-111111111111"
	credential := RehydrateCredentials(credentialID, "aws", "us-east-1", "123456789012", "arn:aws:iam::123456789012:role/control", []byte("AKIAABCDEFGHIJKLMNOP"), []byte("aws-control-secret"), nil, 4, 4, now, now)
	target, err := NormalizeInfrastructureTarget(execution.ExecutionTarget{ID: "22222222-2222-4222-8222-222222222222", Provider: "aws", Kind: execution.TargetKindAWSEC2Instance, InfrastructureProfileID: InfrastructureProfileContainerHostV1, AccountID: credential.AccountID, Region: credential.Region, Architecture: "x86_64", Capabilities: []string{"runtime.container", "target.aws_ec2_instance", "transport.aws_ssm"}, Network: execution.NetworkPolicy{Mode: execution.NetworkPolicyModeObservedHTTPSEgress}, Revision: 2})
	if err != nil {
		t.Fatal(err)
	}
	ref := execution.CredentialRef{Ref: "33333333-3333-4333-8333-333333333333", Purpose: ExecutionSecretPurposeAIProviderAPIKey, Revision: 3, BindingDigest: execution.Digest(strings.Repeat("a", 64))}
	req := SecretParameterProvisionRequest{OwnerID: "@owner:example.org", PlanID: "44444444-4444-4444-8444-444444444444", PlanRevision: 1, PlanDigest: execution.Digest(strings.Repeat("b", 64)), RunID: "55555555-5555-4555-8555-555555555555", RunRevision: 1, RunDigest: execution.Digest(strings.Repeat("c", 64)), StageID: "66666666-6666-4666-8666-666666666666", StageRevision: 1, StageDigest: execution.Digest(strings.Repeat("d", 64)), AttemptID: "77777777-7777-4777-8777-777777777777", StepRevision: 1, StepDigest: execution.Digest(strings.Repeat("e", 64)), Target: target, SecretRef: ref, Delivery: SecretParameterDeliveryTargetSecure, FenceDigest: execution.Digest(strings.Repeat("f", 64)), Credential: credential, CredentialID: credential.ID, CredentialRevision: uint64(credential.Revision)}
	store := &secretIntentStoreFake{}
	provider := &secretParameterProviderFake{store: store}
	secrets := &authorizedSecretFake{value: []byte("provider-api-key-value")}
	executor, err := NewSecretParameterProvisionExecutor(store, provider, secrets)
	if err != nil {
		t.Fatal(err)
	}
	return req, store, provider, secrets, executor
}

func TestSecretParameterProvisionPersistsRedactedIntentBeforeSecureStringPut(t *testing.T) {
	req, store, provider, _, executor := secretParameterFixture(t)
	lease, err := executor.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if store.reserve != 1 || store.versions != 1 || store.complete != 1 || provider.puts != 1 || provider.reads != 1 || lease.ParameterName != provider.name || lease.ContainerMountPath != "/run/secrets/dirextalk/ai_provider_api_key" {
		t.Fatalf("lease=%+v calls=%d/%d/%d/%d/%d", lease, store.reserve, store.versions, store.complete, provider.puts, provider.reads)
	}
	encoded, err := json.Marshal(struct {
		Intent SecretParameterIntent
		Lease  SecretParameterLease
	}{store.record.Intent, lease})
	if err != nil || bytes.Contains(encoded, []byte("provider-api-key-value")) || bytes.Contains(encoded, []byte("aws-control-secret")) {
		t.Fatalf("durable secret projection leaked: %s err=%v", encoded, err)
	}
}

func TestSecretParameterAmbiguousPutReconcilesByReadbackWithoutRedispatch(t *testing.T) {
	req, store, provider, _, executor := secretParameterFixture(t)
	provider.putErr = errors.New("response lost")
	if _, err := executor.Execute(context.Background(), req); !errors.Is(err, ErrSecretParameterUncertain) || provider.puts != 1 || provider.reads != 0 || store.uncertain != 1 {
		t.Fatalf("first err=%v calls=%d/%d uncertain=%d", err, provider.puts, provider.reads, store.uncertain)
	}
	provider.putErr = nil
	lease, err := executor.Reconcile(context.Background(), req)
	if err != nil || provider.puts != 1 || provider.reads != 1 || store.complete != 1 || lease.ProviderVersion != 7 {
		t.Fatalf("reconcile=%+v err=%v calls=%d/%d complete=%d", lease, err, provider.puts, provider.reads, store.complete)
	}
}

func TestSecretParameterRejectsWrongGateBeforeIntentOrProviderMutation(t *testing.T) {
	req, store, provider, secrets, executor := secretParameterFixture(t)
	secrets.mutate = func(auth *SecretAccessAuthorization) { auth.Gate = execution.GateExternalAuth }
	if _, err := executor.Execute(context.Background(), req); !errors.Is(err, ErrSecretParameterUnauthorized) || store.reserve != 0 || provider.puts != 0 {
		t.Fatalf("err=%v reserve=%d puts=%d", err, store.reserve, provider.puts)
	}
}

func TestSecretParameterRevokeDeletesExactHandleThenMarksDurableLease(t *testing.T) {
	req, store, provider, _, executor := secretParameterFixture(t)
	if _, err := executor.Execute(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := executor.Revoke(context.Background(), req); err != nil || provider.revokes != 1 || store.revoked != 1 || store.record.Status != "revoked" {
		t.Fatalf("err=%v provider=%d store=%d status=%s", err, provider.revokes, store.revoked, store.record.Status)
	}
}

type secretParameterSDKFake struct {
	put, get, delete int
	lastPut          *ssm.PutParameterInput
	lastGet          *ssm.GetParameterInput
	lastDelete       *ssm.DeleteParameterInput
	value            string
	deleteErr        error
}

func (f *secretParameterSDKFake) NewSSMSecretParameter(Credentials) (SSMSecretParameterClient, error) {
	return f, nil
}
func (f *secretParameterSDKFake) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	f.put++
	f.lastPut = in
	f.value = awsapi.ToString(in.Value)
	return &ssm.PutParameterOutput{Version: 3}, nil
}
func (f *secretParameterSDKFake) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	f.get++
	f.lastGet = in
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Name: in.Name, Value: awsapi.String(f.value), Type: ssmtypes.ParameterTypeSecureString, Version: 3}}, nil
}
func (f *secretParameterSDKFake) DeleteParameter(_ context.Context, in *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	f.delete++
	f.lastDelete = in
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &ssm.DeleteParameterOutput{}, nil
}

func TestSDKSecretParameterProviderUsesExactNonOverwritingSecureString(t *testing.T) {
	req, _, _, _, _ := secretParameterFixture(t)
	name, err := SecretParameterName(req.Target.ID, req.AttemptID, req.SecretRef)
	if err != nil {
		t.Fatal(err)
	}
	sdk := &secretParameterSDKFake{}
	provider, err := NewSDKSecretParameterProvider(sdk)
	if err != nil {
		t.Fatal(err)
	}
	value := []byte("one-use-provider-key")
	version, err := provider.Put(context.Background(), req, name, value)
	if err != nil || version != 3 || sdk.put != 1 || sdk.lastPut.Type != ssmtypes.ParameterTypeSecureString || awsapi.ToBool(sdk.lastPut.Overwrite) || awsapi.ToString(sdk.lastPut.Name) != name {
		t.Fatalf("version=%d err=%v input=%+v", version, err, sdk.lastPut)
	}
	readback, err := provider.Readback(context.Background(), req, name, value)
	if err != nil || !readback.Exists || !readback.Matches || !awsapi.ToBool(sdk.lastGet.WithDecryption) || awsapi.ToString(sdk.lastGet.Name) != name {
		t.Fatalf("readback=%+v err=%v input=%+v", readback, err, sdk.lastGet)
	}
	if err := provider.Revoke(context.Background(), req, name); err != nil || sdk.delete != 1 || awsapi.ToString(sdk.lastDelete.Name) != name {
		t.Fatalf("revoke err=%v input=%+v", err, sdk.lastDelete)
	}
	sdk.deleteErr = &ssmtypes.ParameterNotFound{}
	if err := provider.Revoke(context.Background(), req, name); err != nil || sdk.delete != 2 {
		t.Fatalf("idempotent missing revoke err=%v calls=%d", err, sdk.delete)
	}
}
