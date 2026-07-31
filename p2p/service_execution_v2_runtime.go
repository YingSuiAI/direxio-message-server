package p2p

// This file owns the production lifecycle for the execution.v2 stage worker.
// It is deliberately separate from the generic Agent task scheduler: the
// worker consumes only the immutable execution-stage ledger and a typed AWS
// SSM transport.  The service wiring may keep this runtime as a dedicated
// field and call StartExecutionV2Runner during process startup.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionobserve"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionrunner"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executiontarget"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/YingSuiAI/dirextalk-message-server/setup/process"
)

var (
	ErrExecutionV2RuntimeInvalid = errors.New("execution.v2 runtime: invalid configuration")
)

// ExecutionV2RuntimeConfig contains only durable production dependencies.
// ArtifactDir must be an explicit, operator-selected path (normally the
// P2P_AGENT_ARTIFACT_DIR setting).  The constructor intentionally refuses an
// implicit temporary/current-directory fallback.
type ExecutionV2RuntimeConfig struct {
	Store           *storage.DatabaseStore
	OwnerID         string
	ArtifactDir     string
	SecretEnveloper *storage.AgentSecretEnveloper
	Factory         coreaws.TypedClientFactory
	SecretValues    coreaws.SecretValueResolver
	WorkerID        string
	Clock           func() time.Time
	LeaseTTL        time.Duration
	PollInterval    time.Duration
	SweepInterval   time.Duration
}

// ExecutionV2Runtime is a dedicated, durable execution-stage component.  Its
// readiness is false until all immutable resolvers, the runner, and the
// process goroutine have been started successfully.  Any construction or
// runtime failure leaves readiness false (fail closed).
type ExecutionV2Runtime struct {
	mu         sync.RWMutex
	config     ExecutionV2RuntimeConfig
	store      *storage.DatabaseExecutionStore
	coord      *storage.DatabaseExecutionCoordinator
	artifacts  *artifactstore.Store
	outputs    *storage.ExecutionServiceOutputMaterializer
	runner     *executionrunner.Runner
	observer   *executionobserve.Service
	importer   *executiontarget.Service
	provision  *coreaws.EC2ProvisionExecutor
	secrets    *storage.DatabaseExecutionSecretParameterRuntime
	secretExec *coreaws.SecretParameterProvisionExecutor
	secretReap *executionV2SecretParameterRevoker
	reconciler *storage.ExecutionReconciler

	started bool
	stopped bool
	failed  error
	done    chan struct{}
	cancel  context.CancelFunc
}

type executionV2OwnerCredentialStore struct {
	owner string
	store interface {
		GetCredentialRevision(context.Context, string, int64) (coreaws.Credentials, error)
	}
}

// executionV2SecretParameterRevoker rehydrates only the exact AWS credential
// revision pinned by a persisted, redacted parameter intent. It implements
// both normal post-command cleanup and the bounded orphan reaper without ever
// listing or deleting a Parameter Store prefix.
type executionV2SecretParameterRevoker struct {
	store       *storage.DatabaseExecutionSecretParameterRuntime
	executor    *coreaws.SecretParameterProvisionExecutor
	credentials *storage.PostgresAWSRepository
}

func (r *executionV2SecretParameterRevoker) Ready() bool {
	return r != nil && r.store != nil && r.store.Ready() && r.executor != nil && r.executor.Ready() && r.credentials != nil
}

func (r *executionV2SecretParameterRevoker) RevokeAuthorizedSecretParameter(ctx context.Context, lease coreaws.SecretParameterLease) error {
	if !r.Ready() || strings.TrimSpace(lease.OwnerID) == "" || !lease.FenceDigest.Valid() {
		return coreaws.ErrSecretParameterInvalid
	}
	record, err := r.store.GetSecretParameterIntent(ctx, lease.OwnerID, lease.FenceDigest)
	if err != nil {
		return err
	}
	req := record.Intent.Request
	if record.Status == "revoked" {
		return nil
	}
	if record.Status != "completed" || record.Intent.ParameterName != lease.ParameterName ||
		req.OwnerID != lease.OwnerID || req.RunID != lease.RunID || req.StageID != lease.ProvisionStageID ||
		req.AttemptID != lease.ProvisionAttemptID || req.Target.ID != lease.TargetID ||
		req.Target.Revision != lease.TargetRevision || req.Target.Digest != lease.TargetDigest ||
		req.SecretRef != lease.SecretRef || req.FenceDigest != lease.FenceDigest || req.RequestDigest != lease.RequestDigest {
		return coreaws.ErrSecretParameterUncertain
	}
	credential, err := r.credentials.GetCredentialRevision(ctx, req.CredentialID, int64(req.CredentialRevision))
	if err != nil || credential.ID != req.CredentialID || credential.Revision != int64(req.CredentialRevision) ||
		credential.VerifiedRevision != credential.Revision {
		return coreaws.ErrSecretParameterUncertain
	}
	req.Credential = credential
	return r.executor.Revoke(ctx, req)
}

func (r *executionV2SecretParameterRevoker) Reap(ctx context.Context, limit int) error {
	if !r.Ready() {
		return coreaws.ErrSecretParameterInvalid
	}
	leases, err := r.store.ListReapableSecretParameterLeases(ctx, limit)
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if err = r.RevokeAuthorizedSecretParameter(ctx, lease); err != nil {
			return err
		}
	}
	return nil
}

func (s executionV2OwnerCredentialStore) GetCredentialRevision(
	ctx context.Context,
	owner string,
	id string,
	revision int64,
) (coreaws.Credentials, error) {
	if strings.TrimSpace(owner) == "" || owner != s.owner || s.store == nil {
		return coreaws.Credentials{}, coreaws.ErrInvalid
	}
	return s.store.GetCredentialRevision(ctx, id, revision)
}

// NewExecutionV2Runtime builds every production dependency from the database
// store and an explicit artifact root.  No memory store, mock transport, or
// ownerless credential repository can pass this boundary.
func NewExecutionV2Runtime(cfg ExecutionV2RuntimeConfig) (*ExecutionV2Runtime, error) {
	if cfg.Store == nil || cfg.Store.DB() == nil || strings.TrimSpace(cfg.OwnerID) == "" ||
		cfg.SecretEnveloper == nil || strings.TrimSpace(cfg.ArtifactDir) == "" {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Factory == nil {
		cfg.Factory = coreaws.NewSDKFactory()
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 2 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 30 * time.Second
	}
	if strings.TrimSpace(cfg.WorkerID) == "" {
		cfg.WorkerID = "execution-v2"
	}
	artifacts, err := artifactstore.New(strings.TrimSpace(cfg.ArtifactDir), artifactstore.MaxArtifactSize)
	if err != nil {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	executionStore := storage.NewDatabaseExecutionStore(cfg.Store.DB(), cfg.Clock)
	if executionStore == nil {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	coordinator := storage.NewDatabaseExecutionCoordinator(cfg.Store.DB(), cfg.Clock)
	if coordinator == nil {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	credentials, err := storage.NewAgentAWSRepositoryWithEnveloper(cfg.Store, cfg.OwnerID, cfg.SecretEnveloper)
	if err != nil {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	executionSecrets := storage.NewDatabaseExecutionSecretStore(cfg.Store.DB(), cfg.SecretEnveloper, cfg.Clock)
	secretParameters := storage.NewDatabaseExecutionSecretParameterRuntime(cfg.Store.DB(), executionSecrets)
	artifactResolver := storage.NewFilesystemArtifactResolver(executionStore, artifacts)
	receiptResolver := storage.NewDatabaseDispatchReceiptResolver(executionStore, credentials)
	stepResolver := storage.NewExecutionStepResolver(executionStore, credentials, artifactResolver)
	serviceOutputs := storage.NewExecutionServiceOutputMaterializer(executionStore, artifacts, cfg.OwnerID)
	stageStore := storage.NewExecutionStageStoreAdapterWithOutputs(executionStore, serviceOutputs)
	if artifactResolver == nil || receiptResolver == nil || stepResolver == nil || serviceOutputs == nil || stageStore == nil {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	options := []coreaws.SSMTransportOption{
		coreaws.WithImmutableArtifactResolver(artifactResolver),
		coreaws.WithDispatchReceiptResolver(receiptResolver),
	}
	var secretExecutor *coreaws.SecretParameterProvisionExecutor
	var secretRevoker *executionV2SecretParameterRevoker
	if parameterFactory, ok := cfg.Factory.(coreaws.SSMSecretParameterClientFactory); ok && secretParameters.Ready() {
		if provider, providerErr := coreaws.NewSDKSecretParameterProvider(parameterFactory); providerErr == nil {
			if executor, executorErr := coreaws.NewSecretParameterProvisionExecutor(secretParameters, provider, secretParameters); executorErr == nil {
				candidate := &executionV2SecretParameterRevoker{store: secretParameters, executor: executor, credentials: credentials}
				if candidate.Ready() {
					secretExecutor = executor
					secretRevoker = candidate
					options = append(options, coreaws.WithSecretParameterRuntime(secretParameters, candidate))
				}
			}
		}
	}
	// Secret values are optional at construction.  Plans that reference
	// secrets still fail closed inside the typed transport unless this explicit
	// resolver is supplied; plaintext never enters this runtime configuration.
	if cfg.SecretValues != nil {
		options = append(options, coreaws.WithSecretValueResolver(cfg.SecretValues))
	}
	transport, err := coreaws.NewSSMTransport(cfg.Factory, options...)
	if err != nil {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	credentialStore := executionV2OwnerCredentialStore{
		owner: cfg.OwnerID,
		store: credentials,
	}
	observer := executionobserve.New(executionobserve.Config{
		Targets:     executionStore,
		Credentials: credentialStore,
		Transport:   transport,
		Now:         cfg.Clock,
	})
	var reservations executiontarget.ReservationCatalog
	if factory, ok := cfg.Factory.(coreaws.ReservationClientFactory); ok {
		catalog := executiontarget.NewAWSReservationCatalog(factory, cfg.Clock)
		if catalog != nil && catalog.Ready() {
			reservations = catalog
		}
	}
	importer := executiontarget.New(executiontarget.Config{
		Targets: executionStore, Credentials: credentialStore, Transport: transport, Reservations: reservations, Now: cfg.Clock,
	})
	if !observer.Ready() || !importer.Ready() {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	var provisioner *coreaws.EC2ProvisionExecutor
	if factory, ok := cfg.Factory.(coreaws.CloudFormationProvisionClientFactory); ok {
		provider, providerErr := coreaws.NewCloudFormationProvisionProvider(factory)
		if providerErr == nil {
			provisioner, _ = coreaws.NewEC2ProvisionExecutor(executionStore, provider, cfg.Clock)
		}
	}
	availability := storage.ExecutionExecutorAvailability{AWSSSM: transport != nil, ComputeProvision: provisioner != nil, SecretProvision: secretExecutor != nil && secretRevoker != nil}
	executionStore.SetExecutorAvailability(availability)
	coordinator.SetExecutorAvailability(availability)
	reconciler := storage.NewExecutionReconciler(executionStore, receiptResolver, transport, provisioner, cfg.Clock)
	if reconciler == nil {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	if secretExecutor != nil {
		reconciler.SetSecretProvisionReconciler(secretExecutor)
	}
	reconciler.SetOutputHook(serviceOutputs)
	runner, err := executionrunner.NewRunner(executionrunner.Config{
		Store: stageStore, Resolver: stepResolver, Transport: transport,
		EC2Provisioner:    provisioner,
		SecretProvisioner: secretExecutor,
		Holder:            cfg.WorkerID, LeaseTTL: cfg.LeaseTTL,
		PollInterval: cfg.PollInterval, Now: cfg.Clock,
		ReceiptResolver: receiptResolver,
	})
	if err != nil {
		return nil, ErrExecutionV2RuntimeInvalid
	}
	return &ExecutionV2Runtime{
		config: cfg, store: executionStore, coord: coordinator,
		artifacts: artifacts, outputs: serviceOutputs, runner: runner, observer: observer, importer: importer, provision: provisioner,
		secrets: secretParameters, secretExec: secretExecutor, secretReap: secretRevoker, reconciler: reconciler,
		done: make(chan struct{}),
	}, nil
}

func (r *ExecutionV2Runtime) ReconcileReady() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.started && !r.stopped && r.failed == nil && r.reconciler != nil && r.reconciler.Ready()
}

func (r *ExecutionV2Runtime) Reconcile(ctx context.Context, owner string, req agentembedded.ExecutionV2ReconcileRequest) (coreexecution.ExecutionRun, error) {
	if !r.ReconcileReady() || strings.TrimSpace(owner) == "" || owner != r.config.OwnerID {
		return coreexecution.ExecutionRun{}, ErrExecutionV2RuntimeInvalid
	}
	r.mu.RLock()
	reconciler := r.reconciler
	r.mu.RUnlock()
	return reconciler.Reconcile(ctx, storage.ExecutionSSMReconcileCommand{OwnerID: owner, RunID: req.RunID, StageID: req.StageID, ExpectedRevision: req.ExpectedRevision, IdempotencyKey: req.IdempotencyKey})
}

// BindingsReady reports readiness for immutable artifact metadata and
// ServiceBinding list/get. It is intentionally independent from invoke:
// schema-pinned HTTP API invocation remains disabled until its own transport
// and readiness hook are installed.
func (r *ExecutionV2Runtime) BindingsReady() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.stopped && r.failed == nil && r.store != nil && r.artifacts != nil && r.outputs != nil && r.outputs.Ready()
}

// Ready reports whether the dedicated process component is live.  A runtime
// with a failed worker never becomes ready again; operators must construct a
// fresh runtime after fixing the dependency or database fault.
func (r *ExecutionV2Runtime) Ready() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.started && !r.stopped && r.failed == nil && r.runner != nil
}

// ObserveReady is independent from the worker goroutine. It is true only when
// the durable target catalog, owner-scoped credential repository, and typed
// AWS read-only transport were constructed successfully.
func (r *ExecutionV2Runtime) ObserveReady() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.stopped && r.failed == nil && r.observer != nil && r.observer.Ready()
}

// TargetImportReady is independent from the stage worker loop but requires
// the same owner-scoped credential store and typed AWS SSM inspection
// transport. The public action additionally gates on the advertised AWS SSM
// transport capability.
func (r *ExecutionV2Runtime) TargetImportReady() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.stopped && r.failed == nil && r.importer != nil && r.importer.Ready()
}

// ProvisionReady gates both target reservation and compute.provision.  The
// action is intentionally unavailable until the worker lifecycle is live and
// both the signed live-price catalog and deterministic CloudFormation
// executor were constructed.
func (r *ExecutionV2Runtime) ProvisionReady() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.started && !r.stopped && r.failed == nil && r.importer != nil && r.importer.ReserveReady() && r.provision != nil
}

// Observe implements the typed ProductCore observe port without exposing the
// underlying AWS transport or credential repository.
func (r *ExecutionV2Runtime) Observe(
	ctx context.Context,
	owner string,
	req agentembedded.ExecutionV2ObserveRequest,
) (coreaws.TargetObservation, error) {
	if !r.ObserveReady() {
		return coreaws.TargetObservation{}, ErrExecutionV2RuntimeInvalid
	}
	r.mu.RLock()
	observer := r.observer
	r.mu.RUnlock()
	return observer.Observe(ctx, owner, req)
}

// ImportTarget verifies the exact credential revision, AWS account/region,
// EC2 instance and SSM Online state before any target catalog row is created.
func (r *ExecutionV2Runtime) ImportTarget(
	ctx context.Context,
	owner string,
	req agentembedded.ExecutionV2TargetImportRequest,
) (agentembedded.ExecutionV2TargetImportResult, error) {
	if !r.TargetImportReady() {
		return agentembedded.ExecutionV2TargetImportResult{}, ErrExecutionV2RuntimeInvalid
	}
	r.mu.RLock()
	importer := r.importer
	r.mu.RUnlock()
	result, err := importer.Import(ctx, owner, executiontarget.ImportRequest{
		CredentialID: req.CredentialID, CredentialRevision: req.CredentialRevision,
		InstanceID: req.InstanceID, IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		if errors.Is(err, executiontarget.ErrCredential) {
			return agentembedded.ExecutionV2TargetImportResult{}, coreexecution.ErrNotFound
		}
		if errors.Is(err, executiontarget.ErrTargetUnavailable) || errors.Is(err, executiontarget.ErrNotReady) {
			return agentembedded.ExecutionV2TargetImportResult{}, coreexecution.ErrConflict
		}
		if errors.Is(err, executiontarget.ErrInvalid) {
			return agentembedded.ExecutionV2TargetImportResult{}, coreexecution.ErrInvalid
		}
		return agentembedded.ExecutionV2TargetImportResult{}, err
	}
	return agentembedded.ExecutionV2TargetImportResult{
		Target: result.Target, ObservationID: result.ObservationID, Observation: result.Observation,
	}, nil
}

// ReserveTarget resolves an exact live regional offer and persists target
// revision 1.  It performs no provider mutation; compute.provision is executed
// later by the confirmed stage worker.
func (r *ExecutionV2Runtime) ReserveTarget(
	ctx context.Context,
	owner string,
	req agentembedded.ExecutionV2TargetReserveRequest,
) (coreexecution.ExecutionTarget, error) {
	if !r.ProvisionReady() {
		return coreexecution.ExecutionTarget{}, ErrExecutionV2RuntimeInvalid
	}
	r.mu.RLock()
	importer := r.importer
	r.mu.RUnlock()
	target, err := importer.Reserve(ctx, owner, executiontarget.ReserveRequest{
		CredentialID: req.CredentialID, CredentialRevision: req.CredentialRevision,
		InstanceType: req.InstanceType, VolumeGiB: req.VolumeGiB, IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		if errors.Is(err, executiontarget.ErrCredential) {
			return coreexecution.ExecutionTarget{}, coreexecution.ErrNotFound
		}
		if errors.Is(err, executiontarget.ErrTargetUnavailable) || errors.Is(err, executiontarget.ErrNotReady) {
			return coreexecution.ExecutionTarget{}, coreexecution.ErrConflict
		}
		if errors.Is(err, executiontarget.ErrInvalid) {
			return coreexecution.ExecutionTarget{}, coreexecution.ErrInvalid
		}
		return coreexecution.ExecutionTarget{}, err
	}
	return target, nil
}

// Err returns a stable runtime failure for readiness/health projections.
func (r *ExecutionV2Runtime) Err() error {
	if r == nil {
		return ErrExecutionV2RuntimeInvalid
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.failed
}

// StartExecutionV2Runner starts only the dedicated execution.v2 loop.  It
// does not start or depend on the generic Agent scheduler.  ComponentStarted
// is paired with ComponentFinished on every path and process cancellation is
// the graceful shutdown signal for the runner.
func (r *ExecutionV2Runtime) StartExecutionV2Runner(processCtx *process.ProcessContext, workerID string) bool {
	if r == nil || processCtx == nil {
		return false
	}
	workerID = strings.TrimSpace(workerID)
	r.mu.Lock()
	if r.started {
		ready := !r.stopped && r.failed == nil
		r.mu.Unlock()
		return ready
	}
	if workerID != "" && workerID != r.config.WorkerID {
		r.mu.Unlock()
		return false
	}
	if r.runner == nil || r.store == nil || r.coord == nil || r.artifacts == nil {
		r.mu.Unlock()
		return false
	}
	// Recover expired approval DAGs before the first worker can observe a
	// queued task. ClaimNext repeats a bounded sweep as a final fence, but
	// startup recovery must not depend on a claimable task existing.
	if _, err := r.store.SweepExpiredV2Confirmations(processCtx.Context(), 0); err != nil {
		r.failed = err
		r.mu.Unlock()
		processCtx.Degraded(err)
		return false
	}
	if r.secretReap != nil {
		if err := r.secretReap.Reap(processCtx.Context(), 100); err != nil {
			r.failed = err
			r.mu.Unlock()
			processCtx.Degraded(err)
			return false
		}
	}
	processCtx.ComponentStarted()
	runnerCtx, cancel := context.WithCancel(processCtx.Context())
	r.started = true
	r.cancel = cancel
	r.mu.Unlock()
	go func() {
		defer processCtx.ComponentFinished()
		defer cancel()
		err := r.runner.Run(runnerCtx)
		r.mu.Lock()
		r.stopped = true
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			r.failed = err
		}
		r.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			processCtx.Degraded(err)
		}
		close(r.done)
	}()
	go r.sweepExecutionV2Confirmations(runnerCtx, processCtx)
	return true
}

func (r *ExecutionV2Runtime) sweepExecutionV2Confirmations(ctx context.Context, processCtx *process.ProcessContext) {
	interval := r.config.SweepInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.store.SweepExpiredV2Confirmations(ctx, 0); err != nil && !errors.Is(err, context.Canceled) {
				r.mu.Lock()
				if r.failed == nil {
					r.failed = err
				}
				r.mu.Unlock()
				processCtx.Degraded(err)
				return
			}
			if r.secretReap != nil {
				if err := r.secretReap.Reap(ctx, 100); err != nil && !errors.Is(err, context.Canceled) {
					r.mu.Lock()
					if r.failed == nil {
						r.failed = err
					}
					r.mu.Unlock()
					processCtx.Degraded(err)
					return
				}
			}
		}
	}
}

// StartExecutionV2Runner starts the Service-owned execution component. It is
// intentionally a separate startup call from StartEmbeddedScheduler.
func (s *Service) StartExecutionV2Runner(processCtx *process.ProcessContext, workerID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	runtime := s.executionV2Runtime
	s.mu.Unlock()
	if runtime == nil {
		return false
	}
	return runtime.StartExecutionV2Runner(processCtx, workerID)
}

// Done closes when the dedicated worker exits.  Process shutdown remains the
// owner of cancellation; this channel is provided for tests and supervisors.
func (r *ExecutionV2Runtime) Done() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.done
}

// Stop is a narrow test/supervisor hook. Production shutdown should cancel the
// ProcessContext so the component can finish its in-flight poll safely.
func (r *ExecutionV2Runtime) Stop() error {
	if r == nil {
		return ErrExecutionV2RuntimeInvalid
	}
	r.mu.RLock()
	cancel := r.cancel
	r.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	return nil
}
