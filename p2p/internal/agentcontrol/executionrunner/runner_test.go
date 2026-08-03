package executionrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

type fakeStore struct {
	mu                             sync.Mutex
	claim                          StageLease
	claims                         []StageLease
	claimErr                       error
	claimCalls                     int
	claimTimes                     []time.Time
	steps                          []NextStep
	intents                        []DispatchIntent
	preDispatchFailures            []PreDispatchFailure
	accepted, uncertain, finalized int
	renews                         int
}

func (s *fakeStore) ClaimNextExecutionStage(context.Context, string, time.Duration) (StageLease, error) {
	s.mu.Lock()
	s.claimCalls++
	s.claimTimes = append(s.claimTimes, time.Now())
	err := s.claimErr
	claim := s.claim
	if len(s.claims) > 0 {
		idx := s.claimCalls - 1
		if idx >= len(s.claims) {
			idx = len(s.claims) - 1
		}
		claim = s.claims[idx]
	}
	s.mu.Unlock()
	if err != nil {
		return StageLease{}, err
	}
	return claim, nil
}
func (s *fakeStore) NextExecutableStep(context.Context, string, string, string) (NextStep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.steps) == 0 {
		return NextStep{}, coreexecution.ErrNotFound
	}
	n := s.steps[0]
	s.steps = s.steps[1:]
	return n, nil
}
func (s *fakeStore) RenewExecutionStageLease(_ context.Context, c StageLease, ttl time.Duration) (StageLease, error) {
	s.mu.Lock()
	s.renews++
	s.mu.Unlock()
	c.ExpectedTaskRevision++
	c.ExpiresAt = time.Now().Add(ttl)
	return c, nil
}
func (s *fakeStore) FailPreDispatch(_ context.Context, failure PreDispatchFailure) error {
	s.mu.Lock()
	s.preDispatchFailures = append(s.preDispatchFailures, failure)
	// A production terminal transition removes this task from the claim set.
	// Model that here so the worker-loop test proves it keeps polling instead of
	// repeatedly selecting the failed stage.
	s.claimErr = coreexecution.ErrNotFound
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) RecordDispatchIntent(_ context.Context, in DispatchIntent) error {
	s.mu.Lock()
	s.intents = append(s.intents, in)
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) MarkDispatchUncertain(context.Context, string, string, string, string, ...coreexecution.Digest) error {
	s.mu.Lock()
	s.uncertain++
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) MarkProviderDispatchUncertain(context.Context, string, string, string, string, ...coreexecution.Digest) error {
	s.mu.Lock()
	s.uncertain++
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) RecordAccepted(context.Context, string, string, string, string) error {
	s.mu.Lock()
	s.accepted++
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) FinalizeDispatchReceipt(context.Context, string, string, string, coreexecution.ReceiptStatus, coreexecution.Digest) error {
	s.mu.Lock()
	s.finalized++
	s.mu.Unlock()
	return nil
}

type fakeResolver struct{}

type StepResolverFunc func(context.Context, StageLease, NextStep) (PreparedStep, error)

func (f StepResolverFunc) ResolveStep(ctx context.Context, claim StageLease, next NextStep) (PreparedStep, error) {
	return f(ctx, claim, next)
}

func (fakeResolver) ResolveStep(_ context.Context, c StageLease, n NextStep) (PreparedStep, error) {
	attemptID, receiptID := "00000000-0000-4000-8000-000000000011", "00000000-0000-4000-8000-000000000012"
	if n.StepKey == "two" {
		attemptID, receiptID = "00000000-0000-4000-8000-000000000021", "00000000-0000-4000-8000-000000000022"
	}
	planID := "00000000-0000-4000-8000-000000000001"
	requestDigest, fenceDigest := digest("request"), digest("fence")
	planDigest, stageDigest := digest("plan"), digest("stage")
	return PreparedStep{Frozen: coreaws.FrozenRequest{OwnerID: c.OwnerID, PlanID: planID, PlanRevision: 1, PlanDigest: planDigest, RunID: c.RunID, RunRevision: 1, RunDigest: digest("run"), StageID: c.StageID, StageRevision: 1, StageDigest: stageDigest, StepKey: n.StepKey, StepRevision: n.StepRevision, StepDigest: n.StepDigest, AttemptID: attemptID, RequestDigest: requestDigest, FenceDigest: fenceDigest, TargetID: "target", TargetRevision: 1, TargetDigest: digest("target")}, Attempt: coreexecution.StepAttempt{AttemptID: attemptID, OwnerID: c.OwnerID, RunID: c.RunID, StageID: c.StageID, PlanID: planID, PlanRevision: 1, PlanDigest: planDigest, StageRevision: 1, StageDigest: stageDigest, StepRevision: n.StepRevision, StepKey: n.StepKey, StepDigest: n.StepDigest}, Receipt: coreexecution.Receipt{ReceiptID: receiptID, OwnerID: c.OwnerID, RunID: c.RunID, AttemptID: attemptID, Status: coreexecution.ReceiptAccepted, RequestDigest: requestDigest, FenceDigest: fenceDigest}}, nil
}

type secretResolver struct{ reconcile bool }

func (r secretResolver) ResolveStep(_ context.Context, c StageLease, n NextStep) (PreparedStep, error) {
	attemptID := "00000000-0000-4000-8000-000000000031"
	planID := "00000000-0000-4000-8000-000000000001"
	requestDigest, fenceDigest := digest("secret-request"), digest("secret-fence")
	planDigest, stageDigest, targetDigest, runDigest := digest("plan"), digest("stage"), digest("target"), digest("run")
	ref := coreexecution.CredentialRef{Ref: "00000000-0000-4000-8000-000000000041", Purpose: coreaws.ExecutionSecretPurposeAIProviderAPIKey, Revision: 1, BindingDigest: digest("binding")}
	req := coreaws.SecretParameterProvisionRequest{OwnerID: c.OwnerID, PlanID: planID, PlanRevision: 1, PlanDigest: planDigest, RunID: c.RunID, RunRevision: 1, RunDigest: runDigest, StageID: c.StageID, StageRevision: 1, StageDigest: stageDigest, AttemptID: attemptID, StepRevision: n.StepRevision, StepDigest: n.StepDigest, Target: coreexecution.ExecutionTarget{ID: "00000000-0000-4000-8000-000000000051", Provider: "aws", Kind: coreexecution.TargetKindAWSEC2Instance, Revision: 1, Digest: targetDigest}, SecretRef: ref, Delivery: coreaws.SecretParameterDeliveryTargetSecure, FenceDigest: fenceDigest, RequestDigest: requestDigest, CredentialID: "00000000-0000-4000-8000-000000000061", CredentialRevision: 1}
	attempt := coreexecution.StepAttempt{AttemptID: attemptID, OwnerID: c.OwnerID, RunID: c.RunID, StageID: c.StageID, PlanID: planID, PlanRevision: 1, PlanDigest: planDigest, StageRevision: 1, StageDigest: stageDigest, StepRevision: n.StepRevision, StepDigest: n.StepDigest, StepKey: n.StepKey, Attempt: 1, Revision: 1, Status: coreexecution.AttemptRunning}
	receipt := NewReceipt(c.OwnerID, c.RunID, attemptID, string(requestDigest))
	receipt.RequestDigest, receipt.FenceDigest = requestDigest, fenceDigest
	return PreparedStep{SecretProvision: &req, Attempt: attempt, Receipt: receipt, ReconcileOnly: r.reconcile}, nil
}

type fakeSecretProvisioner struct {
	execute, reconcile int
	err                error
}

func (p *fakeSecretProvisioner) Execute(context.Context, coreaws.SecretParameterProvisionRequest) (coreaws.SecretParameterLease, error) {
	p.execute++
	if p.err != nil {
		return coreaws.SecretParameterLease{}, p.err
	}
	return coreaws.SecretParameterLease{SchemaVersion: "execution-secret-parameter/v1", OwnerID: "@owner:test", RunID: "00000000-0000-4000-8000-000000000002", ProvisionStageID: "00000000-0000-4000-8000-000000000003", ProvisionAttemptID: "00000000-0000-4000-8000-000000000031", TargetID: "00000000-0000-4000-8000-000000000051", TargetRevision: 1, TargetDigest: digest("target"), SecretRef: coreexecution.CredentialRef{Ref: "00000000-0000-4000-8000-000000000041", Purpose: coreaws.ExecutionSecretPurposeAIProviderAPIKey, Revision: 1, BindingDigest: digest("binding")}, ParameterName: "/dirextalk/execution-v2/param", FenceDigest: digest("secret-fence"), RequestDigest: digest("secret-request"), ProviderVersion: 1}, nil
}
func (p *fakeSecretProvisioner) Reconcile(context.Context, coreaws.SecretParameterProvisionRequest) (coreaws.SecretParameterLease, error) {
	p.reconcile++
	if p.err != nil {
		return coreaws.SecretParameterLease{}, p.err
	}
	return coreaws.SecretParameterLease{SchemaVersion: "execution-secret-parameter/v1", OwnerID: "@owner:test", RunID: "00000000-0000-4000-8000-000000000002", ProvisionStageID: "00000000-0000-4000-8000-000000000003", ProvisionAttemptID: "00000000-0000-4000-8000-000000000031", TargetID: "00000000-0000-4000-8000-000000000051", TargetRevision: 1, TargetDigest: digest("target"), SecretRef: coreexecution.CredentialRef{Ref: "00000000-0000-4000-8000-000000000041", Purpose: coreaws.ExecutionSecretPurposeAIProviderAPIKey, Revision: 1, BindingDigest: digest("binding")}, ParameterName: "/dirextalk/execution-v2/param", FenceDigest: digest("secret-fence"), RequestDigest: digest("secret-request"), ProviderVersion: 1}, nil
}

type failingResolver struct {
	err error
}

func (r failingResolver) ResolveStep(context.Context, StageLease, NextStep) (PreparedStep, error) {
	return PreparedStep{}, r.err
}

type delayedFailingResolver struct {
	delay time.Duration
	err   error
}

func (r delayedFailingResolver) ResolveStep(ctx context.Context, _ StageLease, _ NextStep) (PreparedStep, error) {
	select {
	case <-time.After(r.delay):
		return PreparedStep{}, r.err
	case <-ctx.Done():
		return PreparedStep{}, ctx.Err()
	}
}

type invalidPreparedResolver struct{}

func (invalidPreparedResolver) ResolveStep(context.Context, StageLease, NextStep) (PreparedStep, error) {
	return PreparedStep{}, nil
}

type fakeTransport struct {
	mu           sync.Mutex
	dispatch     int
	dispatchErrs []error
	poll         int
	dispatchErr  error
	pollErr      error
	status       coreaws.PollStatus
	pollDelay    time.Duration
}

type capabilityTransport struct {
	fakeTransport
	supported bool
}

func (t *capabilityTransport) CanExecuteStep(coreexecution.ExecutionStep) bool { return t.supported }

func (t *fakeTransport) Dispatch(context.Context, coreaws.FrozenRequest) (coreaws.DispatchResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dispatch++
	if len(t.dispatchErrs) > 0 {
		err := t.dispatchErrs[0]
		t.dispatchErrs = t.dispatchErrs[1:]
		if err != nil {
			return coreaws.DispatchResult{}, err
		}
	}
	if t.dispatchErr != nil {
		return coreaws.DispatchResult{}, t.dispatchErr
	}
	return coreaws.DispatchResult{Status: coreaws.DispatchAccepted, CommandID: "cmd"}, nil
}
func (t *fakeTransport) dispatchCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dispatch
}
func (t *fakeTransport) Poll(ctx context.Context, _ coreaws.PollRequest) (coreaws.CommandResult, error) {
	t.mu.Lock()
	t.poll++
	t.mu.Unlock()
	if t.pollDelay > 0 {
		select {
		case <-time.After(t.pollDelay):
		case <-ctx.Done():
			return coreaws.CommandResult{}, ctx.Err()
		}
	}
	if t.pollErr != nil {
		return coreaws.CommandResult{}, t.pollErr
	}
	return coreaws.CommandResult{Status: t.status, CommandID: "cmd"}, nil
}
func digest(s string) coreexecution.Digest {
	h := sha256.Sum256([]byte(s))
	return coreexecution.Digest(hex.EncodeToString(h[:]))
}
func fixtureStore() *fakeStore {
	return &fakeStore{claim: StageLease{OwnerID: "@owner:test", RunID: "00000000-0000-4000-8000-000000000002", StageID: "00000000-0000-4000-8000-000000000003", TaskID: "00000000-0000-4000-8000-000000000004", Holder: "worker", Attempt: 1, LeaseEpoch: 1, TaskLeaseEpoch: 1, ExpectedTaskRevision: 1, LeaseID: "00000000-0000-4000-8000-000000000005", LeaseToken: "00000000-0000-4000-8000-000000000006"}, steps: []NextStep{{OwnerID: "@owner:test", RunID: "00000000-0000-4000-8000-000000000002", StageID: "00000000-0000-4000-8000-000000000003", StepKey: "one", StepSet: coreexecution.StepSetForward, StepRevision: 1, StepDigest: digest("one")}, {OwnerID: "@owner:test", RunID: "00000000-0000-4000-8000-000000000002", StageID: "00000000-0000-4000-8000-000000000003", StepKey: "two", StepSet: coreexecution.StepSetForward, StepRevision: 1, StepDigest: digest("two")}}}
}

func secretFixtureStore() *fakeStore {
	s := fixtureStore()
	s.steps = []NextStep{{OwnerID: s.claim.OwnerID, RunID: s.claim.RunID, StageID: s.claim.StageID, StepKey: "secret", StepSet: coreexecution.StepSetForward, StepRevision: 1, StepDigest: digest("secret")}}
	return s
}

func TestRunnerSecretProvisionPersistsRedactedIntentAndFinalizes(t *testing.T) {
	s := secretFixtureStore()
	provider := &fakeSecretProvisioner{}
	r, err := NewRunner(Config{Store: s, Resolver: secretResolver{}, SecretProvisioner: provider, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.execute != 1 || provider.reconcile != 0 || len(s.intents) != 1 || s.finalized != 1 || s.uncertain != 0 {
		t.Fatalf("execute=%d reconcile=%d intents=%d finalized=%d uncertain=%d", provider.execute, provider.reconcile, len(s.intents), s.finalized, s.uncertain)
	}
	if s.intents[0].SecretProvision == nil {
		t.Fatal("secret intent missing redacted request")
	}
	access, secret, token := s.intents[0].SecretProvision.Credential.StoredSecretBytes()
	if len(access) != 0 || len(secret) != 0 || len(token) != 0 {
		t.Fatal("secret credential payload leaked into dispatch intent")
	}
}

func TestRunnerSecretProvisionReclaimedLeaseUsesReadbackOnly(t *testing.T) {
	s := secretFixtureStore()
	provider := &fakeSecretProvisioner{}
	r, err := NewRunner(Config{Store: s, Resolver: secretResolver{reconcile: true}, SecretProvisioner: provider, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.execute != 0 || provider.reconcile != 1 || s.finalized != 1 || s.uncertain != 0 {
		t.Fatalf("execute=%d reconcile=%d finalized=%d uncertain=%d", provider.execute, provider.reconcile, s.finalized, s.uncertain)
	}
}

func TestRunnerSecretProvisionReturnsOnlyStableSafeDiagnostic(t *testing.T) {
	s := secretFixtureStore()
	raw := "provider-sensitive-diagnostic-must-not-cross-runner"
	provider := &fakeSecretProvisioner{err: fmt.Errorf("%s: %w", raw, coreaws.ErrSecretParameterUnauthorized)}
	r, err := NewRunner(Config{Store: s, Resolver: secretResolver{}, SecretProvisioner: provider, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	err = r.RunOnce(context.Background())
	if !errors.Is(err, ErrRunnerUncertain) || !strings.Contains(err.Error(), "secret_provision_unauthorized") || strings.Contains(err.Error(), raw) {
		t.Fatalf("unsafe secret diagnostic: %v", err)
	}
	if provider.execute != 1 || s.uncertain != 1 || s.finalized != 0 {
		t.Fatalf("execute=%d uncertain=%d finalized=%d", provider.execute, s.uncertain, s.finalized)
	}
}

func TestRunnerContinuesStepsAndRecordsFenceOrder(t *testing.T) {
	s := fixtureStore()
	tr := &fakeTransport{status: coreaws.PollSucceeded, pollDelay: 40 * time.Millisecond}
	r, _ := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: tr, Holder: "worker", LeaseTTL: 30 * time.Millisecond, PollInterval: time.Millisecond})
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.dispatch != 2 || s.accepted != 2 || s.finalized != 2 || len(s.intents) != 2 {
		t.Fatalf("dispatch=%d accepted=%d finalized=%d intents=%d", tr.dispatch, s.accepted, s.finalized, len(s.intents))
	}
	if s.intents[1].TaskRevision <= s.intents[0].TaskRevision {
		t.Fatalf("renewed task revision not propagated: first=%d second=%d", s.intents[0].TaskRevision, s.intents[1].TaskRevision)
	}
}

func TestRunnerWaitsForLaterWorkWhenQueueIsEmpty(t *testing.T) {
	s := fixtureStore()
	s.claimErr = coreexecution.ErrNotFound
	r, err := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: &fakeTransport{}, Holder: "worker", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			s.mu.Lock()
			calls := s.claimCalls
			s.mu.Unlock()
			if calls >= 2 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want cancellation after idle polling", err)
	}
	s.mu.Lock()
	calls := s.claimCalls
	s.mu.Unlock()
	if calls < 2 {
		t.Fatalf("claim calls=%d, runner exited instead of polling", calls)
	}
}

func TestRunnerResolverFailureIsRedactedTerminalAndWorkerContinues(t *testing.T) {
	s := fixtureStore()
	secretBearing := errors.New("resolver rejected Authorization: Bearer super-secret")
	r, err := NewRunner(Config{Store: s, Resolver: failingResolver{err: secretBearing}, Transport: &fakeTransport{}, Holder: "worker", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	for {
		s.mu.Lock()
		calls := s.claimCalls
		failures := append([]PreDispatchFailure(nil), s.preDispatchFailures...)
		s.mu.Unlock()
		if calls >= 2 && len(failures) == 1 {
			if failures[0].Code != FailureStepResolution || !failures[0].EvidenceDigest.Valid() {
				t.Fatalf("failure=%#v", failures[0])
			}
			if failures[0].EvidenceDigest != PreDispatchEvidenceDigest(failures[0]) {
				t.Fatal("pre-dispatch evidence is not deterministic")
			}
			cancel()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want cancellation after continuing past failed stage", err)
	}
	if strings.Contains(string(s.preDispatchFailures[0].EvidenceDigest), "super-secret") {
		t.Fatal("raw resolver error leaked into durable failure")
	}
}

func TestRunnerPreparedValidationFailureNeverDispatches(t *testing.T) {
	s := fixtureStore()
	tr := &fakeTransport{status: coreaws.PollSucceeded}
	r, err := NewRunner(Config{Store: s, Resolver: invalidPreparedResolver{}, Transport: tr, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.preDispatchFailures) != 1 || s.preDispatchFailures[0].Code != FailurePreparedStep {
		t.Fatalf("failures=%#v", s.preDispatchFailures)
	}
	if tr.dispatch != 0 || len(s.intents) != 0 || s.uncertain != 0 {
		t.Fatalf("dispatch=%d intents=%d uncertain=%d", tr.dispatch, len(s.intents), s.uncertain)
	}
}

func TestRunnerMissingExecutorFailsDurablyBeforeDispatch(t *testing.T) {
	s := fixtureStore()
	r, err := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.preDispatchFailures) != 1 || s.preDispatchFailures[0].Code != FailureExecutor || !s.preDispatchFailures[0].EvidenceDigest.Valid() {
		t.Fatalf("failures=%#v", s.preDispatchFailures)
	}
	if len(s.intents) != 0 || s.accepted != 0 || s.uncertain != 0 || s.finalized != 0 {
		t.Fatalf("intent=%d accepted=%d uncertain=%d finalized=%d", len(s.intents), s.accepted, s.uncertain, s.finalized)
	}
}

func TestRunnerUnavailableSecretRuntimeFailsBeforeDurableDispatchIntent(t *testing.T) {
	s := fixtureStore()
	transport := &capabilityTransport{}
	r, err := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: transport, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.preDispatchFailures) != 1 || s.preDispatchFailures[0].Code != FailureExecutor || transport.dispatch != 0 || len(s.intents) != 0 || s.uncertain != 0 {
		t.Fatalf("failures=%#v dispatch=%d intents=%d uncertain=%d", s.preDispatchFailures, transport.dispatch, len(s.intents), s.uncertain)
	}
}

func TestRunnerPreDispatchFailureUsesLatestRenewedFence(t *testing.T) {
	s := fixtureStore()
	r, err := NewRunner(Config{
		Store: s, Resolver: delayedFailingResolver{delay: 45 * time.Millisecond, err: errors.New("rejected")},
		Transport: &fakeTransport{}, Holder: "worker", LeaseTTL: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renews == 0 || len(s.preDispatchFailures) != 1 {
		t.Fatalf("renews=%d failures=%d", s.renews, len(s.preDispatchFailures))
	}
	if got := s.preDispatchFailures[0].Claim.ExpectedTaskRevision; got <= s.claim.ExpectedTaskRevision {
		t.Fatalf("failure used stale task revision %d, initial %d", got, s.claim.ExpectedTaskRevision)
	}
}

func TestRunnerDispatchFailureRemainsUncertainNotPreDispatchFailed(t *testing.T) {
	s := fixtureStore()
	raw := "Authorization: Bearer provider-secret"
	tr := &fakeTransport{dispatchErr: fmt.Errorf("%s: %w", raw, coreaws.ErrTypedUncertain)}
	r, _ := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: tr, Holder: "worker"})
	err := r.RunOnce(context.Background())
	if !errors.Is(err, ErrRunnerUncertain) || !strings.Contains(err.Error(), "ssm_dispatch_uncertain") || strings.Contains(err.Error(), raw) {
		t.Fatalf("unexpected safe diagnostic: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.preDispatchFailures) != 0 || s.uncertain != 1 || tr.dispatch != 1 {
		t.Fatalf("pre-dispatch=%d uncertain=%d dispatch=%d", len(s.preDispatchFailures), s.uncertain, tr.dispatch)
	}
}
func TestRunnerLostDispatchResponseIsUncertainAndNeverResent(t *testing.T) {
	s := fixtureStore()
	tr := &fakeTransport{dispatchErr: errors.New("lost response")}
	r, _ := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: tr, Holder: "worker"})
	if !errors.Is(r.RunOnce(context.Background()), ErrRunnerUncertain) {
		t.Fatal("expected uncertain")
	}
	if tr.dispatch != 1 || s.uncertain != 1 {
		t.Fatalf("dispatch=%d uncertain=%d", tr.dispatch, s.uncertain)
	}
}

func TestRunnerTakeoverWithoutProviderCommandIsPermanentlyUncertain(t *testing.T) {
	s := fixtureStore()
	s.steps = s.steps[:1]
	tr := &fakeTransport{status: coreaws.PollSucceeded}
	resolver := StepResolverFunc(func(_ context.Context, c StageLease, n NextStep) (PreparedStep, error) {
		prepared, err := (fakeResolver{}).ResolveStep(context.Background(), c, n)
		prepared.ReconcileOnly = true
		return prepared, err
	})
	r, err := NewRunner(Config{Store: s, Resolver: resolver, Transport: tr, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(context.Background()); !errors.Is(err, ErrRunnerUncertain) {
		t.Fatalf("RunOnce error = %v, want durable uncertainty", err)
	}
	if tr.dispatch != 0 || len(s.intents) != 0 || s.uncertain != 1 {
		t.Fatalf("dispatch=%d intents=%d uncertain=%d, takeover without command was redispatched", tr.dispatch, len(s.intents), s.uncertain)
	}
}

func TestRunnerContinuesAfterDurableUncertaintyWithoutBusyLoop(t *testing.T) {
	s := fixtureStore()
	secondClaim := s.claim
	secondClaim.RunID = "00000000-0000-4000-8000-000000000102"
	secondClaim.StageID = "00000000-0000-4000-8000-000000000103"
	secondClaim.TaskID = "00000000-0000-4000-8000-000000000104"
	secondClaim.LeaseID = "00000000-0000-4000-8000-000000000105"
	secondClaim.LeaseToken = "00000000-0000-4000-8000-000000000106"
	s.claims = []StageLease{s.claim, secondClaim}
	s.steps = []NextStep{
		{OwnerID: s.claim.OwnerID, RunID: s.claim.RunID, StageID: s.claim.StageID, StepKey: "one", StepSet: coreexecution.StepSetForward, StepRevision: 1, StepDigest: digest("one")},
		{OwnerID: secondClaim.OwnerID, RunID: secondClaim.RunID, StageID: secondClaim.StageID, StepKey: "two", StepSet: coreexecution.StepSetForward, StepRevision: 1, StepDigest: digest("two")},
	}
	tr := &fakeTransport{status: coreaws.PollSucceeded, dispatchErrs: []error{errors.New("lost response")}}
	pollInterval := 25 * time.Millisecond
	r, err := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: tr, Holder: "worker", PollInterval: pollInterval})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	deadline := time.After(2 * time.Second)
	for {
		if tr.dispatchCount() >= 2 {
			break
		}
		select {
		case <-deadline:
			cancel()
			s.mu.Lock()
			claims := s.claimCalls
			s.mu.Unlock()
			t.Fatalf("runner did not continue to unrelated step: dispatch=%d claims=%d", tr.dispatchCount(), claims)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want cancellation after continued work", err)
	}
	if s.uncertain != 1 || tr.dispatchCount() != 2 {
		t.Fatalf("uncertain=%d dispatch=%d, want one settled claim and one unrelated dispatch", s.uncertain, tr.dispatchCount())
	}
	s.mu.Lock()
	if len(s.intents) != 2 || s.intents[1].Attempt.RunID != secondClaim.RunID {
		s.mu.Unlock()
		t.Fatalf("second intent=%#v, want unrelated run %s", s.intents, secondClaim.RunID)
	}
	s.mu.Unlock()
	s.mu.Lock()
	claimTimes := append([]time.Time(nil), s.claimTimes...)
	s.mu.Unlock()
	if len(claimTimes) < 2 || claimTimes[1].Sub(claimTimes[0]) < pollInterval/2 {
		t.Fatalf("uncertain retry was a busy loop: claim times=%v", claimTimes)
	}
}
func TestRunnerPollFailureAfterIntentIsUncertain(t *testing.T) {
	s := fixtureStore()
	raw := "provider response included secret-value"
	tr := &fakeTransport{pollErr: fmt.Errorf("%s: %w", raw, coreaws.ErrTypedProvider)}
	r, _ := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: tr, Holder: "worker"})
	err := r.RunOnce(context.Background())
	if !errors.Is(err, ErrRunnerUncertain) || !strings.Contains(err.Error(), "ssm_poll_provider") || strings.Contains(err.Error(), raw) {
		t.Fatalf("unexpected safe diagnostic: %v", err)
	}
	if tr.dispatch != 1 || s.accepted != 1 || s.uncertain != 1 {
		t.Fatalf("dispatch=%d accepted=%d uncertain=%d", tr.dispatch, s.accepted, s.uncertain)
	}
}

func TestTypedSSMErrorCodeNeverIncludesRawProviderText(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, "invalid_status"},
		{context.Canceled, "context_canceled"},
		{context.DeadlineExceeded, "context_deadline"},
		{fmt.Errorf("raw credential text: %w", coreaws.ErrTypedInvalid), "invalid"},
		{coreaws.ErrTypedUnavailable, "unavailable"},
		{coreaws.ErrTypedNotFound, "not_found"},
		{coreaws.ErrTypedUncertain, "uncertain"},
		{coreaws.ErrTypedProvider, "provider"},
		{coreaws.ErrTypedTerminal, "terminal"},
		{coreaws.ErrTypedReplay, "replay"},
		{coreaws.ErrTypedArtifact, "artifact"},
		{errors.New("raw unknown provider text"), "unclassified"},
	}
	for _, tt := range tests {
		if got := typedSSMErrorCode(tt.err); got != tt.want || strings.Contains(got, "raw") {
			t.Fatalf("typedSSMErrorCode(%v)=%q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestRunnerCancellationAfterIntentIsUncertain(t *testing.T) {
	s := fixtureStore()
	tr := &fakeTransport{pollErr: context.Canceled}
	r, _ := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: tr, Holder: "worker"})
	if !errors.Is(r.RunOnce(context.Background()), ErrRunnerUncertain) {
		t.Fatal("expected uncertain")
	}
	if tr.dispatch != 1 || s.uncertain != 1 {
		t.Fatalf("dispatch=%d uncertain=%d", tr.dispatch, s.uncertain)
	}
}

func TestRunnerRestartConsumesOnlyStoreSelectedNextStep(t *testing.T) {
	s := fixtureStore()
	s.steps = s.steps[1:] // the store has already fenced step one succeeded.
	tr := &fakeTransport{status: coreaws.PollSucceeded}
	r, _ := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: tr, Holder: "worker"})
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.dispatch != 1 || len(s.intents) != 1 || s.intents[0].Attempt.StepKey != "two" {
		t.Fatalf("dispatch=%d intents=%d", tr.dispatch, len(s.intents))
	}
}

func TestRunnerRenewsTaskAndTargetFence(t *testing.T) {
	s := fixtureStore()
	tr := &fakeTransport{status: coreaws.PollSucceeded, pollDelay: 40 * time.Millisecond}
	r, _ := NewRunner(Config{Store: s, Resolver: fakeResolver{}, Transport: tr, Holder: "worker", LeaseTTL: 30 * time.Millisecond, PollInterval: time.Millisecond})
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.renews == 0 {
		t.Fatal("expected lease renewal")
	}
}
