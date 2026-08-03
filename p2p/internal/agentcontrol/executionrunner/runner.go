package executionrunner

// Runner is the execution.v2 stage worker.  It deliberately depends on a
// narrow store seam and typed provider boundaries; it never receives a
// generic task store, raw shell transport, or AWS SDK passthrough.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/google/uuid"
)

var (
	ErrRunnerInvalid   = errors.New("execution runner: invalid configuration")
	ErrRunnerLease     = errors.New("execution runner: lease lost")
	ErrRunnerUncertain = errors.New("execution runner: outcome uncertain")
)

const (
	FailureStepResolution = "execution_step_resolution_failed"
	FailurePreparedStep   = "execution_pre_dispatch_validation_failed"
	FailureExecutor       = "execution_executor_unavailable"
)

// StageLease is the immutable task+target fence returned by the production
// claim path.  Storage adapters convert their concrete lease type into this
// value, keeping the worker independent from SQL details.
type StageLease struct {
	OwnerID, RunID, StageID, TaskID                  string
	Holder                                           string
	Attempt                                          uint32
	LeaseEpoch, TaskLeaseEpoch, ExpectedTaskRevision uint64
	LeaseID, LeaseToken                              string
	ExpiresAt                                        time.Time
}

type NextStep struct {
	OwnerID, RunID, StageID, StepKey string
	StepSet                          coreexecution.StepSet
	StepRevision                     uint64
	StepDigest                       coreexecution.Digest
}

// PreparedStep is produced by an authoritative plan/target snapshot resolver.
// FrozenRequest contains no command bytes or secret values; the typed
// transport resolves immutable artifacts and transient secret refs itself.
type PreparedStep struct {
	Frozen          coreaws.FrozenRequest
	EC2Provision    *coreaws.EC2ProvisionRequest
	EC2Credential   coreaws.Credentials
	SecretProvision *coreaws.SecretParameterProvisionRequest
	ReconcileOnly   bool
	Attempt         coreexecution.StepAttempt
	Receipt         coreexecution.Receipt
}

// StepResolver must read the immutable plan, run, stage, target and
// credential snapshots after the stage has been claimed.
type StepResolver interface {
	ResolveStep(context.Context, StageLease, NextStep) (PreparedStep, error)
}

type StageStore interface {
	ClaimNextExecutionStage(context.Context, string, time.Duration) (StageLease, error)
	NextExecutableStep(context.Context, string, string, string) (NextStep, error)
	RenewExecutionStageLease(context.Context, StageLease, time.Duration) (StageLease, error)
	FailPreDispatch(context.Context, PreDispatchFailure) error
	RecordDispatchIntent(context.Context, DispatchIntent) error
	MarkDispatchUncertain(context.Context, string, string, string, string, ...coreexecution.Digest) error
	MarkProviderDispatchUncertain(context.Context, string, string, string, string, ...coreexecution.Digest) error
	RecordAccepted(context.Context, string, string, string, string) error
	FinalizeDispatchReceipt(context.Context, string, string, string, coreexecution.ReceiptStatus, coreexecution.Digest) error
}

// PreDispatchFailure is deliberately incapable of carrying an error string.
// It is used only before a durable provider intent exists, so persistence can
// fail the exact step attempt and release its task/target leases without
// weakening the poll-only semantics of an accepted SSM command.
type PreDispatchFailure struct {
	Claim          StageLease
	Step           NextStep
	Code           string
	EvidenceDigest coreexecution.Digest
}

func NewPreDispatchFailure(claim StageLease, step NextStep, code string) (PreDispatchFailure, error) {
	f := PreDispatchFailure{Claim: claim, Step: step, Code: code}
	if !validPreDispatchFailure(f) {
		return PreDispatchFailure{}, ErrRunnerInvalid
	}
	f.EvidenceDigest = PreDispatchEvidenceDigest(f)
	return f, nil
}

// PreDispatchEvidenceDigest is deterministic for the exact task, target lease
// and immutable step fence. It intentionally excludes the resolver's raw
// error, which may contain provider, path, repository, or credential details.
func PreDispatchEvidenceDigest(f PreDispatchFailure) coreexecution.Digest {
	return evidenceDigest(
		"pre_dispatch_failure/v2",
		f.Code,
		f.Claim.OwnerID,
		f.Claim.RunID,
		f.Claim.StageID,
		f.Claim.TaskID,
		f.Claim.Holder,
		strconv.FormatUint(uint64(f.Claim.Attempt), 10),
		strconv.FormatUint(f.Claim.TaskLeaseEpoch, 10),
		strconv.FormatUint(f.Claim.ExpectedTaskRevision, 10),
		f.Claim.LeaseID,
		f.Claim.LeaseToken,
		strconv.FormatUint(f.Claim.LeaseEpoch, 10),
		string(f.Step.StepSet),
		f.Step.StepKey,
		strconv.FormatUint(f.Step.StepRevision, 10),
		string(f.Step.StepDigest),
	)
}

func validPreDispatchFailure(f PreDispatchFailure) bool {
	return (f.Code == FailureStepResolution || f.Code == FailurePreparedStep || f.Code == FailureExecutor) &&
		strings.TrimSpace(f.Claim.OwnerID) != "" &&
		coreexecution.ValidateUUID(f.Claim.RunID) &&
		coreexecution.ValidateUUID(f.Claim.StageID) &&
		coreexecution.ValidateUUID(f.Claim.TaskID) &&
		strings.TrimSpace(f.Claim.Holder) != "" &&
		f.Claim.Attempt > 0 &&
		f.Claim.TaskLeaseEpoch > 0 &&
		f.Claim.ExpectedTaskRevision > 0 &&
		coreexecution.ValidateUUID(f.Claim.LeaseID) &&
		coreexecution.ValidateUUID(f.Claim.LeaseToken) &&
		f.Claim.LeaseEpoch > 0 &&
		f.Step.OwnerID == f.Claim.OwnerID &&
		f.Step.RunID == f.Claim.RunID &&
		f.Step.StageID == f.Claim.StageID &&
		strings.TrimSpace(f.Step.StepKey) != "" &&
		(f.Step.StepSet == coreexecution.StepSetForward || f.Step.StepSet == coreexecution.StepSetRollback) &&
		f.Step.StepRevision > 0 &&
		f.Step.StepDigest.Valid()
}

type DispatchIntent struct {
	Attempt                      coreexecution.StepAttempt
	Receipt                      coreexecution.Receipt
	TaskID, TaskHolder           string
	TaskAttempt                  uint32
	TaskRevision, TaskLeaseEpoch uint64
	TargetID                     string
	TargetRevision               uint64
	TargetDigest                 coreexecution.Digest
	LeaseID, LeaseToken          string
	LeaseEpoch                   uint64
	StepSet                      coreexecution.StepSet
	RequestDigest, FenceDigest   coreexecution.Digest
	Snapshot                     coreaws.FrozenRequestSnapshot
	EC2Provision                 *coreaws.EC2ProvisionRequest
	SecretProvision              *coreaws.SecretParameterProvisionRequest
}

type TypedSSMTransport interface {
	Dispatch(context.Context, coreaws.FrozenRequest) (coreaws.DispatchResult, error)
	Poll(context.Context, coreaws.PollRequest) (coreaws.CommandResult, error)
}

type TypedEC2Provisioner interface {
	Execute(context.Context, coreaws.EC2ProvisionRequest, coreaws.Credentials) (coreaws.EC2ProvisionCompletion, error)
}

// TypedSecretProvisioner is the only runner boundary for secret.provision.
// Execute may perform the first provider mutation only after a durable intent
// has been recorded by the executor. Reconcile is readback-only and is used
// after a lease takeover or an ambiguous provider/persistence outcome.
type TypedSecretProvisioner interface {
	Execute(context.Context, coreaws.SecretParameterProvisionRequest) (coreaws.SecretParameterLease, error)
	Reconcile(context.Context, coreaws.SecretParameterProvisionRequest) (coreaws.SecretParameterLease, error)
}

type Config struct {
	Store             StageStore
	Resolver          StepResolver
	Transport         TypedSSMTransport
	EC2Provisioner    TypedEC2Provisioner
	SecretProvisioner TypedSecretProvisioner
	Holder            string
	LeaseTTL          time.Duration
	PollInterval      time.Duration
}

type Runner struct {
	store                  StageStore
	resolver               StepResolver
	transport              TypedSSMTransport
	ec2Provisioner         TypedEC2Provisioner
	secretProvisioner      TypedSecretProvisioner
	holder                 string
	leaseTTL, pollInterval time.Duration
	uncertainMu            sync.Mutex
	uncertainReceipts      map[string]struct{}
}

func NewRunner(c Config) (*Runner, error) {
	if c.Store == nil || c.Resolver == nil || strings.TrimSpace(c.Holder) == "" {
		return nil, ErrRunnerInvalid
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 2 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	return &Runner{store: c.Store, resolver: c.Resolver, transport: c.Transport, ec2Provisioner: c.EC2Provisioner, secretProvisioner: c.SecretProvisioner, holder: c.Holder, leaseTTL: c.LeaseTTL, pollInterval: c.PollInterval, uncertainReceipts: make(map[string]struct{})}, nil
}

// RunOnce claims exactly one execution stage and drives it to a durable
// terminal state (or uncertainty).  A not-found claim is returned unchanged.
func (r *Runner) RunOnce(ctx context.Context) error {
	if r == nil {
		return ErrRunnerInvalid
	}
	claim, err := r.store.ClaimNextExecutionStage(ctx, r.holder, r.leaseTTL)
	if err != nil {
		return err
	}
	return r.RunClaimed(ctx, claim)
}

// Run is the standalone execution-stage worker loop.  It is intentionally
// separate from the generic Core task worker loop.
func (r *Runner) Run(ctx context.Context) error {
	if r == nil {
		return ErrRunnerInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := r.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, coreexecution.ErrNotFound) || errors.Is(err, ErrRunnerUncertain) {
				// An empty queue is a normal steady state for the dedicated
				// execution.v2 worker. Keep the component alive and poll again;
				// returning here would make a production runner silently exit
				// before a later confirmation queues a stage. An uncertain claim
				// is already durably fenced by RunClaimed; the same delay also
				// prevents a lost provider response from creating a busy loop.
				if err := r.waitForNextPoll(ctx); err != nil {
					return err
				}
				continue
			}
			return err
		}
	}
}

func (r *Runner) waitForNextPoll(ctx context.Context) error {
	wait := r.pollInterval
	if wait <= 0 {
		wait = time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) RunClaimed(parent context.Context, claim StageLease) error {
	if r == nil || r.store == nil || r.resolver == nil || claim.OwnerID == "" || claim.RunID == "" || claim.StageID == "" {
		return ErrRunnerInvalid
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var mu sync.Mutex
	var current *PreparedStep
	var renewErr error
	currentClaim := claim
	stop := make(chan struct{})
	renewDone := make(chan struct{})
	var stopOnce sync.Once
	stopRenewal := func() {
		stopOnce.Do(func() { close(stop) })
		<-renewDone
	}
	defer stopRenewal()
	go func() {
		defer close(renewDone)
		t := time.NewTicker(r.leaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			default:
			}
			select {
			case <-t.C:
				mu.Lock()
				renewalClaim := currentClaim
				mu.Unlock()
				nextClaim, err := r.store.RenewExecutionStageLease(ctx, renewalClaim, r.leaseTTL)
				if err != nil {
					mu.Lock()
					renewErr = err
					mu.Unlock()
					cancel()
					return
				}
				mu.Lock()
				currentClaim = nextClaim
				mu.Unlock()
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			mu.Lock()
			p := current
			re := renewErr
			mu.Unlock()
			if p != nil {
				r.uncertain(context.Background(), p, err)
			}
			if re != nil {
				return ErrRunnerLease
			}
			return err
		}
		mu.Lock()
		claim = currentClaim
		mu.Unlock()
		next, err := r.store.NextExecutableStep(ctx, claim.OwnerID, claim.RunID, claim.StageID)
		if err != nil {
			if errors.Is(err, coreexecution.ErrNotFound) {
				return nil
			}
			return err
		}
		prepared, err := r.resolver.ResolveStep(ctx, claim, next)
		if err != nil {
			stopRenewal()
			mu.Lock()
			re := renewErr
			claim = currentClaim
			mu.Unlock()
			if parent.Err() != nil {
				return parent.Err()
			}
			if re != nil {
				return ErrRunnerLease
			}
			return r.failPreDispatch(claim, next, FailureStepResolution)
		}
		mu.Lock()
		re := renewErr
		claim = currentClaim
		mu.Unlock()
		if re != nil {
			return ErrRunnerLease
		}
		if err := validatePrepared(claim, next, &prepared); err != nil {
			stopRenewal()
			mu.Lock()
			re = renewErr
			claim = currentClaim
			mu.Unlock()
			if parent.Err() != nil {
				return parent.Err()
			}
			if re != nil {
				return ErrRunnerLease
			}
			return r.failPreDispatch(claim, next, FailurePreparedStep)
		}
		transportUnsupported := false
		if prepared.SecretProvision == nil && prepared.EC2Provision == nil && r.transport != nil {
			if capability, ok := r.transport.(interface {
				CanExecuteStep(coreexecution.ExecutionStep) bool
			}); ok {
				transportUnsupported = !capability.CanExecuteStep(prepared.Frozen.Script.Step)
			}
		}
		secretUnsupported := prepared.SecretProvision != nil && r.secretProvisioner == nil
		if prepared.EC2Provision != nil && r.ec2Provisioner == nil || prepared.SecretProvision == nil && prepared.EC2Provision == nil && (r.transport == nil || transportUnsupported) || secretUnsupported {
			stopRenewal()
			mu.Lock()
			re = renewErr
			claim = currentClaim
			mu.Unlock()
			if parent.Err() != nil {
				return parent.Err()
			}
			if re != nil {
				return ErrRunnerLease
			}
			return r.failPreDispatch(claim, next, FailureExecutor)
		}
		mu.Lock()
		current = &prepared
		mu.Unlock()
		err = r.dispatchStep(ctx, claim, next, prepared)
		mu.Lock()
		current = nil
		re = renewErr
		mu.Unlock()
		if re != nil {
			return ErrRunnerLease
		}
		if err != nil {
			return err
		}
	}
}

func (r *Runner) failPreDispatch(claim StageLease, next NextStep, code string) error {
	failure, err := NewPreDispatchFailure(claim, next, code)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.store.FailPreDispatch(bounded, failure); err != nil {
		return err
	}
	// The claimed task/stage is now durably failed. Returning nil lets Run
	// continue to unrelated work instead of treating a rejected immutable step
	// as a global worker failure.
	return nil
}

func validatePrepared(c StageLease, n NextStep, p *PreparedStep) error {
	if p == nil || !coreexecution.ValidateUUID(c.RunID) || !coreexecution.ValidateUUID(c.StageID) || !coreexecution.ValidateUUID(c.TaskID) || !coreexecution.ValidateUUID(c.LeaseID) || !coreexecution.ValidateUUID(c.LeaseToken) {
		return ErrRunnerInvalid
	}
	if (p.SecretProvision != nil && p.EC2Provision != nil) || (p.SecretProvision != nil && p.Frozen.RequestDigest.Valid()) || (p.EC2Provision != nil && p.Frozen.RequestDigest.Valid()) {
		return ErrRunnerInvalid
	}
	if p.SecretProvision != nil {
		req := p.SecretProvision
		if req.OwnerID != c.OwnerID || req.RunID != c.RunID || req.StageID != c.StageID || req.StepRevision != n.StepRevision || req.StepDigest != n.StepDigest || !req.RequestDigest.Valid() || !req.FenceDigest.Valid() || req.Target.ID == "" || req.Target.Revision == 0 || req.Target.Digest == "" || req.SecretRef.Ref == "" {
			return ErrRunnerInvalid
		}
	} else if p.EC2Provision != nil {
		req := p.EC2Provision
		intent := coreaws.EC2ProvisionIntent{OwnerID: req.OwnerID, FenceDigest: req.FenceDigest, RequestDigest: req.RequestDigest, ProviderOperationKey: coreaws.EC2ProvisionOperationKey(req.Target.ID), Request: *req}
		if coreaws.ValidateEC2ProvisionIntentSnapshot(intent) != nil || req.OwnerID != c.OwnerID || req.RunID != c.RunID || req.StageID != c.StageID || req.Step.StepKey != n.StepKey || req.Step.Digest != n.StepDigest || req.StepRevision != n.StepRevision {
			return ErrRunnerInvalid
		}
	} else if p.Frozen.OwnerID != c.OwnerID || p.Frozen.RunID != c.RunID || p.Frozen.StageID != c.StageID || p.Frozen.StepKey != n.StepKey || p.Frozen.StepRevision != n.StepRevision || p.Frozen.StepDigest != n.StepDigest || !p.Frozen.RequestDigest.Valid() || !p.Frozen.FenceDigest.Valid() {
		return ErrRunnerInvalid
	}
	planID, planRevision, planDigest, stageRevision, stageDigest, attemptID, requestDigest, fenceDigest := p.Frozen.PlanID, p.Frozen.PlanRevision, p.Frozen.PlanDigest, p.Frozen.StageRevision, p.Frozen.StageDigest, p.Frozen.AttemptID, p.Frozen.RequestDigest, p.Frozen.FenceDigest
	if p.SecretProvision != nil {
		planID, planRevision, planDigest = p.SecretProvision.PlanID, p.SecretProvision.PlanRevision, p.SecretProvision.PlanDigest
		stageRevision, stageDigest, attemptID = p.SecretProvision.StageRevision, p.SecretProvision.StageDigest, p.SecretProvision.AttemptID
		requestDigest, fenceDigest = p.SecretProvision.RequestDigest, p.SecretProvision.FenceDigest
	} else if p.EC2Provision != nil {
		planID, planRevision, planDigest = p.EC2Provision.PlanID, p.EC2Provision.PlanRevision, p.EC2Provision.PlanDigest
		stageRevision, stageDigest, attemptID = p.EC2Provision.StageRevision, p.EC2Provision.StageDigest, p.EC2Provision.AttemptID
		requestDigest, fenceDigest = p.EC2Provision.RequestDigest, p.EC2Provision.FenceDigest
	}
	if !coreexecution.ValidateUUID(attemptID) || !coreexecution.ValidateUUID(p.Attempt.PlanID) || !coreexecution.ValidateUUID(p.Attempt.StageID) || p.Attempt.AttemptID != attemptID || p.Attempt.OwnerID != c.OwnerID || p.Attempt.RunID != c.RunID || p.Attempt.StageID != c.StageID || p.Attempt.PlanID != planID || p.Attempt.PlanRevision != planRevision || p.Attempt.PlanDigest != planDigest || p.Attempt.StageRevision != stageRevision || p.Attempt.StageDigest != stageDigest || p.Attempt.StepRevision != n.StepRevision || p.Attempt.StepKey != n.StepKey || p.Attempt.StepDigest != n.StepDigest || !coreexecution.ValidateUUID(p.Receipt.ReceiptID) || p.Receipt.OwnerID != c.OwnerID || p.Receipt.RunID != c.RunID || p.Receipt.AttemptID != p.Attempt.AttemptID || (p.Receipt.RequestDigest != "" && p.Receipt.RequestDigest != requestDigest) || (p.Receipt.FenceDigest != "" && p.Receipt.FenceDigest != fenceDigest) || p.Receipt.Status != coreexecution.ReceiptAccepted {
		return ErrRunnerInvalid
	}
	return nil
}

func (r *Runner) dispatchStep(ctx context.Context, claim StageLease, next NextStep, p PreparedStep) error {
	if p.SecretProvision != nil {
		return r.dispatchSecretProvision(ctx, claim, next, p)
	}
	if p.EC2Provision != nil {
		return r.dispatchEC2Provision(ctx, claim, next, p)
	}
	in := DispatchIntent{Attempt: p.Attempt, Receipt: p.Receipt, TaskID: claim.TaskID, TaskHolder: claim.Holder, TaskAttempt: claim.Attempt, TaskRevision: claim.ExpectedTaskRevision, TaskLeaseEpoch: claim.TaskLeaseEpoch, TargetID: p.Frozen.TargetID, TargetRevision: p.Frozen.TargetRevision, TargetDigest: p.Frozen.TargetDigest, LeaseID: claim.LeaseID, LeaseToken: claim.LeaseToken, LeaseEpoch: claim.LeaseEpoch, StepSet: next.StepSet, RequestDigest: p.Frozen.RequestDigest, FenceDigest: p.Frozen.FenceDigest, Snapshot: coreaws.SnapshotFromFrozen(p.Frozen)}
	commandID := p.Receipt.SSMCommandID
	// A reclaimed durable intent without a provider command id has no safe
	// readback key. It is permanently uncertain: never rewrite the intent or
	// issue a second provider mutation. The production resolver marks reclaimed
	// intents as readback-only; this guard also protects test/custom resolvers
	// that surface the same immutable fact.
	if p.ReconcileOnly && commandID == "" {
		r.uncertain(context.Background(), &p, nil)
		return ErrRunnerUncertain
	}
	if commandID == "" {
		if err := r.store.RecordDispatchIntent(ctx, in); err != nil {
			return fmt.Errorf("execution runner: persist dispatch intent: %w", err)
		}
		result, err := r.transport.Dispatch(ctx, p.Frozen)
		if err != nil || result.Status != coreaws.DispatchAccepted || result.CommandID == "" {
			r.uncertain(context.Background(), &p, err)
			return fmt.Errorf("%w: ssm_dispatch_%s", ErrRunnerUncertain, typedSSMErrorCode(err))
		}
		commandID = result.CommandID
		p.Receipt.SSMCommandID = commandID
		if err := r.store.RecordAccepted(ctx, claim.OwnerID, p.Receipt.ReceiptID, p.Attempt.AttemptID, commandID); err != nil {
			r.uncertain(context.Background(), &p, err)
			return fmt.Errorf("%w: persist accepted receipt: %w", ErrRunnerUncertain, err)
		}
	}
	pollReq := coreaws.PollRequest{OwnerID: claim.OwnerID, Frozen: p.Frozen, CommandID: commandID, Known: true, FenceDigest: p.Frozen.FenceDigest}
	for {
		if err := ctx.Err(); err != nil {
			r.uncertain(context.Background(), &p, err)
			return ErrRunnerUncertain
		}
		polled, pollErr := r.transport.Poll(ctx, pollReq)
		if pollErr != nil || polled.Status == coreaws.PollUncertain {
			r.uncertain(context.Background(), &p, pollErr)
			return fmt.Errorf("%w: ssm_poll_%s", ErrRunnerUncertain, typedSSMErrorCode(pollErr))
		}
		switch polled.Status {
		case coreaws.PollPending, coreaws.PollRunning, coreaws.PollCancellationRequested:
			select {
			case <-ctx.Done():
				r.uncertain(context.Background(), &p, ctx.Err())
				return ErrRunnerUncertain
			case <-time.After(r.pollInterval):
			}
			continue
		case coreaws.PollSucceeded, coreaws.PollFailed, coreaws.PollCanceled:
			status := coreexecution.ReceiptStatus(coreaws.PollSucceeded)
			if polled.Status != coreaws.PollSucceeded {
				status = coreexecution.ReceiptFailed
			}
			digest := polled.OutputDigest
			if !digest.Valid() {
				digest = evidenceDigest(string(polled.Status), polled.CommandID, polled.Stdout, polled.Stderr)
			}
			if err := r.store.FinalizeDispatchReceipt(ctx, claim.OwnerID, p.Receipt.ReceiptID, p.Attempt.AttemptID, status, digest); err != nil {
				// The provider is already terminal. Do not let a generic task
				// retry issue another mutation if persistence races or fails.
				r.uncertain(context.Background(), &p, err)
				return fmt.Errorf("%w: persist terminal receipt: %w", ErrRunnerUncertain, err)
			}
			return nil
		default:
			r.uncertain(context.Background(), &p, nil)
			return ErrRunnerUncertain
		}
	}
}

// typedSSMErrorCode is the only provider diagnostic allowed to cross the
// runner boundary. It deliberately maps errors by stable sentinel identity
// and never returns provider messages, request values, command output, or AWS
// identifiers.
func typedSSMErrorCode(err error) string {
	switch {
	case err == nil:
		return "invalid_status"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline"
	case errors.Is(err, coreaws.ErrTypedInvalid):
		return "invalid"
	case errors.Is(err, coreaws.ErrTypedUnavailable):
		return "unavailable"
	case errors.Is(err, coreaws.ErrTypedNotFound):
		return "not_found"
	case errors.Is(err, coreaws.ErrTypedUncertain):
		return "uncertain"
	case errors.Is(err, coreaws.ErrTypedProvider):
		return "provider"
	case errors.Is(err, coreaws.ErrTypedTerminal):
		return "terminal"
	case errors.Is(err, coreaws.ErrTypedReplay):
		return "replay"
	case errors.Is(err, coreaws.ErrTypedArtifact):
		return "artifact"
	default:
		return "unclassified"
	}
}

func (r *Runner) dispatchEC2Provision(ctx context.Context, claim StageLease, next NextStep, p PreparedStep) error {
	if r.ec2Provisioner == nil || p.EC2Provision == nil {
		return ErrRunnerInvalid
	}
	req := *p.EC2Provision
	in := DispatchIntent{Attempt: p.Attempt, Receipt: p.Receipt, TaskID: claim.TaskID, TaskHolder: claim.Holder, TaskAttempt: claim.Attempt, TaskRevision: claim.ExpectedTaskRevision, TaskLeaseEpoch: claim.TaskLeaseEpoch, TargetID: req.Target.ID, TargetRevision: req.Target.Revision, TargetDigest: req.Target.Digest, LeaseID: claim.LeaseID, LeaseToken: claim.LeaseToken, LeaseEpoch: claim.LeaseEpoch, StepSet: next.StepSet, RequestDigest: req.RequestDigest, FenceDigest: req.FenceDigest, EC2Provision: &req}
	if err := r.store.RecordDispatchIntent(ctx, in); err != nil {
		return err
	}
	for {
		completion, err := r.ec2Provisioner.Execute(ctx, req, p.EC2Credential)
		switch {
		case err == nil:
			digest, digestErr := coreexecution.CanonicalDigest(struct {
				Target      coreexecution.ExecutionTarget
				Observation coreexecution.TargetObservation
			}{completion.Target, completion.Observation})
			if digestErr != nil || r.store.FinalizeDispatchReceipt(ctx, claim.OwnerID, p.Receipt.ReceiptID, p.Attempt.AttemptID, coreexecution.ReceiptSucceeded, digest) != nil {
				r.uncertain(context.Background(), &p, digestErr)
				return ErrRunnerUncertain
			}
			return nil
		case errors.Is(err, coreaws.ErrEC2ProvisionPending):
			select {
			case <-ctx.Done():
				r.uncertain(context.Background(), &p, ctx.Err())
				return ErrRunnerUncertain
			case <-time.After(r.pollInterval):
			}
			continue
		case errors.Is(err, coreaws.ErrEC2ProvisionFailed):
			digest := evidenceDigest("cloudformation_create_failed", req.AttemptID, string(req.RequestDigest))
			if finalizeErr := r.store.FinalizeDispatchReceipt(ctx, claim.OwnerID, p.Receipt.ReceiptID, p.Attempt.AttemptID, coreexecution.ReceiptFailed, digest); finalizeErr != nil {
				r.uncertain(context.Background(), &p, finalizeErr)
				return ErrRunnerUncertain
			}
			return nil
		default:
			r.uncertain(context.Background(), &p, err)
			return fmt.Errorf("%w: %v", ErrRunnerUncertain, err)
		}
	}
}

func (r *Runner) dispatchSecretProvision(ctx context.Context, claim StageLease, next NextStep, p PreparedStep) error {
	if r.secretProvisioner == nil || p.SecretProvision == nil {
		return ErrRunnerInvalid
	}
	req := *p.SecretProvision
	// The intent contains only the redacted request. The provider receives the
	// authoritative request with its transient AWS credential in memory.
	in := DispatchIntent{
		Attempt: p.Attempt, Receipt: p.Receipt, TaskID: claim.TaskID,
		TaskHolder: claim.Holder, TaskAttempt: claim.Attempt,
		TaskRevision: claim.ExpectedTaskRevision, TaskLeaseEpoch: claim.TaskLeaseEpoch,
		TargetID: req.Target.ID, TargetRevision: req.Target.Revision, TargetDigest: req.Target.Digest,
		LeaseID: claim.LeaseID, LeaseToken: claim.LeaseToken, LeaseEpoch: claim.LeaseEpoch,
		StepSet: next.StepSet, RequestDigest: req.RequestDigest, FenceDigest: req.FenceDigest,
		SecretProvision: redactSecretProvisionRequest(&req),
	}
	// If an accepted intent already exists (lease takeover), only reconcile the
	// exact durable provider intent. Execute is intentionally unreachable on
	// that path, preventing a second PutParameter.
	reconcileOnly := p.ReconcileOnly
	if err := r.store.RecordDispatchIntent(ctx, in); err != nil {
		return fmt.Errorf("execution runner: persist secret dispatch intent: %w", err)
	}
	var lease coreaws.SecretParameterLease
	var err error
	if reconcileOnly {
		lease, err = r.secretProvisioner.Reconcile(ctx, req)
	} else {
		lease, err = r.secretProvisioner.Execute(ctx, req)
	}
	if err != nil {
		// The provider may have accepted the put while its response was lost;
		// all failures after the durable intent are therefore uncertain. The
		// coordinator/reconciler must decide whether the parameter exists.
		r.uncertain(context.Background(), &p, err)
		return fmt.Errorf("%w: secret_provision_%s", ErrRunnerUncertain, typedSecretProvisionErrorCode(err))
	}
	digest, digestErr := coreexecution.CanonicalDigest(struct {
		SchemaVersion   string
		OwnerID         string
		RunID           string
		StageID         string
		TargetID        string
		TargetRevision  uint64
		TargetDigest    coreexecution.Digest
		SecretRef       coreexecution.CredentialRef
		ParameterName   string
		ProviderVersion int64
		FenceDigest     coreexecution.Digest
		RequestDigest   coreexecution.Digest
	}{lease.SchemaVersion, lease.OwnerID, lease.RunID, lease.ProvisionStageID, lease.TargetID, lease.TargetRevision, lease.TargetDigest, lease.SecretRef, lease.ParameterName, lease.ProviderVersion, lease.FenceDigest, lease.RequestDigest})
	if digestErr != nil || !digest.Valid() {
		r.uncertain(context.Background(), &p, digestErr)
		return fmt.Errorf("%w: secret_lease_digest", ErrRunnerUncertain)
	}
	if err = r.store.FinalizeDispatchReceipt(ctx, claim.OwnerID, p.Receipt.ReceiptID, p.Attempt.AttemptID, coreexecution.ReceiptSucceeded, digest); err != nil {
		r.uncertain(context.Background(), &p, err)
		return fmt.Errorf("%w: persist secret receipt: %w", ErrRunnerUncertain, err)
	}
	return nil
}

func typedSecretProvisionErrorCode(err error) string {
	switch {
	case err == nil:
		return "invalid_status"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline"
	case errors.Is(err, coreaws.ErrSecretParameterInvalid):
		return "invalid"
	case errors.Is(err, coreaws.ErrSecretParameterUnauthorized):
		return "unauthorized"
	case errors.Is(err, coreaws.ErrSecretParameterUncertain):
		return "uncertain"
	case errors.Is(err, coreaws.ErrSecretParameterFailed):
		return "failed"
	default:
		return "unclassified"
	}
}

func redactSecretProvisionRequest(req *coreaws.SecretParameterProvisionRequest) *coreaws.SecretParameterProvisionRequest {
	if req == nil {
		return nil
	}
	copyReq := *req
	cred := req.Credential
	copyReq.Credential = coreaws.RehydrateCredentialMetadata(cred.ID, cred.Name, cred.Region, cred.AccountID, cred.UserARN, cred.VerifiedRevision, cred.Revision, cred.CreatedAt, cred.UpdatedAt)
	return &copyReq
}

func (r *Runner) uncertain(ctx context.Context, p *PreparedStep, cause error) {
	if p == nil {
		return
	}
	d := evidenceDigest("uncertain", p.Attempt.AttemptID, errorText(cause))
	r.uncertainMu.Lock()
	if _, exists := r.uncertainReceipts[p.Receipt.ReceiptID]; exists {
		r.uncertainMu.Unlock()
		return
	}
	r.uncertainReceipts[p.Receipt.ReceiptID] = struct{}{}
	r.uncertainMu.Unlock()
	bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if p.EC2Provision != nil {
		_ = r.store.MarkProviderDispatchUncertain(bounded, p.Attempt.OwnerID, p.Attempt.AttemptID, p.Receipt.ReceiptID, coreaws.EC2ProvisionOperationKey(p.EC2Provision.Target.ID), d)
		return
	}
	_ = r.store.MarkDispatchUncertain(bounded, p.Attempt.OwnerID, p.Attempt.AttemptID, p.Receipt.ReceiptID, p.Receipt.SSMCommandID, d)
}

func evidenceDigest(parts ...string) coreexecution.Digest {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return coreexecution.Digest(hex.EncodeToString(h.Sum(nil)))
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// NewReceipt creates a receipt identity for adapters which do not persist one
// before resolving a step. It is never used as a provider idempotency key.
func NewReceipt(owner, runID, attemptID, idempotency string) coreexecution.Receipt {
	return coreexecution.Receipt{ReceiptID: uuid.NewString(), OwnerID: owner, RunID: runID, AttemptID: attemptID, Status: coreexecution.ReceiptAccepted, IdempotencyKey: idempotency}
}
