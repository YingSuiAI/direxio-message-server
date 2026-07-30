package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	workaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/aws"
	workecs "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/ecs"
	workssm "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/ssm"
	coreextensions "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
	extensioncatalog "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions/catalog"
	agentruntime "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/runtime"
	agenttask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

// embeddedTaskExecutor is the only generic worker execution switch. Domain
// coordinators may be added as typed fields, but no branch may spawn a
// process or fall back to a transport/RPC runner.
type embeddedTaskExecutor struct {
	agent           *agentmodule.Module
	aws             *coreaws.Service
	workloadService *coreworkload.Service
	workloadHandler *coreworkload.Handler
	mcp             *agentembedded.PinnedMCPWorkerCoordinator
	// Test-only seams keep the GeoLibre recovery fence executable without
	// weakening the production service boundary.
	geoAWS           geoAWSRuntime
	geoWorkloads     geoWorkloadRuntime
	geoHandler       geoWorkloadHandler
	validateGeoLibre func(coreworkload.Plan, coreaws.Provision, coreaws.Credentials, string, coreaws.PlanView, bool) error
}

type geoAWSRuntime interface {
	AcquireProvisionMutation(context.Context, string) (coreaws.ProvisionMutationLease, error)
	ClaimProvisionMutation(context.Context, string, string) (coreaws.ProvisionMutationLease, error)
	GetProvisionForOwner(context.Context, string, string) (coreaws.Provision, error)
	GetCredentialRevision(context.Context, string, int64) (coreaws.Credentials, error)
	GetPlanForOwner(context.Context, string, string) (coreaws.PlanView, error)
}

type geoWorkloadRuntime interface {
	GetOperation(context.Context, string) (coreworkload.Operation, error)
	GetPlan(context.Context, string) (coreworkload.Plan, error)
}

type geoWorkloadHandler interface {
	Handle(context.Context, string, string, uint64, ...coreworkload.TaskFence) (coreworkload.Operation, error)
	Recover(context.Context, string, ...coreworkload.TaskFence) (coreworkload.Operation, error)
}

func (e *embeddedTaskExecutor) Execute(ctx context.Context, queued agenttask.Task) (agenttask.Result, error) {
	if e == nil {
		return agenttask.Result{}, errors.New("embedded task executor is unavailable")
	}
	switch queued.Spec.Kind {
	case "", agenttask.TaskKindAgent:
		return e.executeAgent(ctx, queued)
	case agenttask.TaskKindExtension:
		return e.executeExtension(ctx, queued)
	case agenttask.TaskKindAWSChange:
		return e.executeAWSChange(ctx, queued)
	case agenttask.TaskKindWorkload:
		return e.executeWorkload(ctx, queued)
	default:
		return agenttask.Result{}, fmt.Errorf("unsupported embedded task kind %q", queued.Spec.Kind)
	}
}

func (e *embeddedTaskExecutor) executeAgent(ctx context.Context, queued agenttask.Task) (agenttask.Result, error) {
	if e.agent == nil {
		return agenttask.Result{}, errors.New("native agent runtime is unavailable")
	}
	// References require a resolver and immutable execution snapshot. Until
	// that dependency is present, fail before invoking the model rather than
	// silently dropping caller input or enabling an unpinned extension.
	if len(queued.Spec.AttachmentRefs) != 0 || len(queued.Spec.KnowledgeRefs) != 0 || len(queued.Spec.Extensions) != 0 {
		return agenttask.Result{}, errors.New("task references are not available in the embedded runtime")
	}
	handler := e.agent.Handlers()["agent.chat"]
	if handler == nil {
		return agenttask.Result{}, errors.New("native agent chat action is unavailable")
	}
	params := map[string]any{
		"prompt":           queued.Spec.Goal,
		"model_profile_id": queued.Spec.ModelProfileID,
	}
	if queued.Spec.ConversationID != "" {
		params["conversation_id"] = queued.Spec.ConversationID
	}
	value, actionErr := handler(ctx, params)
	if actionErr != nil {
		return agenttask.Result{}, fmt.Errorf("native agent task failed: %s", actionErr.Error)
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > agenttask.MaxResultBytes {
		return agenttask.Result{}, errors.New("native agent task returned an invalid result")
	}
	result := agenttask.Result{JSON: raw}
	if object, ok := value.(map[string]any); ok {
		result.Text = firstResultString(object, "message", "text", "output")
		result.Summary = firstResultString(object, "summary")
	}
	if result.Summary == "" {
		result.Summary = boundedRuntimeResult(result.Text)
	}
	if err := result.Validate(); err != nil {
		return agenttask.Result{}, errors.New("native agent task returned an invalid result")
	}
	return result, nil
}

func (e *embeddedTaskExecutor) ready(capability string) bool {
	if e == nil {
		return false
	}
	switch capability {
	case "mcp":
		return e.mcp != nil
	case "aws.control":
		return e.aws != nil && e.aws.ReadyForEmbedded()
	case "workload.aws_ssm", "workload.aws_ecs":
		return e.workloadService != nil && e.workloadHandler != nil && e.workloadService.ReadyForEmbedded(e.workloadHandler)
	default:
		return false
	}
}

func (e *embeddedTaskExecutor) executeExtension(ctx context.Context, queued agenttask.Task) (agenttask.Result, error) {
	payload := queued.Spec.Payload.Extension
	if e.mcp == nil || payload == nil || payload.ConfirmationID == "" || payload.Operation == agenttask.ExtensionOperationExecuteSkill {
		return agenttask.Result{}, errors.New("embedded MCP task is unavailable")
	}
	err := e.mcp.RunClaimed(ctx, queued.OwnerID, queued)
	if err == nil || errors.Is(err, coreextensions.ErrUncertain) || errors.Is(err, coreextensions.ErrAlreadyFinalized) {
		return agenttask.Result{}, agentruntime.ErrTaskFinalized
	}
	confirmation, confirmationErr := e.mcp.Confirmations.Get(ctx, queued.OwnerID, payload.ConfirmationID)
	if confirmationErr == nil && confirmation.State == "consumed" {
		// A consumed confirmation is a non-idempotent fence. The domain
		// finalizer/recovery path owns it; a generic failure write would split
		// receipt, confirmation and task state.
		return agenttask.Result{}, agentruntime.ErrTaskFinalized
	}
	return agenttask.Result{}, err
}

func (e *embeddedTaskExecutor) executeAWSChange(ctx context.Context, queued agenttask.Task) (agenttask.Result, error) {
	payload := queued.Spec.Payload.AWSChange
	if e.aws == nil || payload == nil || payload.ChangeID == "" || queued.Lease == nil {
		return agenttask.Result{}, errors.New("embedded AWS task is unavailable")
	}
	change, err := e.aws.GetChange(ctx, payload.ChangeID)
	if err != nil || change.TaskID != queued.ID {
		return agenttask.Result{}, errors.New("AWS change task binding is invalid")
	}
	fence, err := e.aws.ExecutionFence(ctx, change.ConfirmationID)
	if err != nil || fence.Task.ID != queued.ID || fence.Task.Attempt != queued.Attempt || fence.Task.LeaseEpoch != queued.LeaseEpoch || uint64(fence.Task.Revision) < queued.Revision {
		return agenttask.Result{}, errors.New("AWS change task fence is stale")
	}
	if fence.Confirmation.State == "confirmed" {
		_, err = e.aws.ConsumeChange(ctx, coreaws.ConsumeChangeCommand{
			ChangeID:                     change.ID,
			ConfirmationID:               change.ConfirmationID,
			TaskID:                       queued.ID,
			IdempotencyKey:               queued.Spec.IdempotencyKey,
			Attempt:                      queued.Attempt,
			LeaseEpoch:                   queued.LeaseEpoch,
			ExpectedChangeRevision:       change.Revision,
			ExpectedConfirmationRevision: fence.Confirmation.Revision,
			ExpectedTaskRevision:         fence.Task.Revision,
			Binding:                      fence.Confirmation.Binding,
		})
		if err != nil {
			// A cryptographic/immutable binding mismatch is deterministic.  Let
			// the generic worker terminalize it; deferring would retry forever.
			if errors.Is(err, coreaws.ErrInvalid) {
				return agenttask.Result{}, err
			}
			// A confirmation expiry can race the worker between its initial fence
			// read and ConsumeChange.  When the durable rows still prove that no
			// provider-facing transition occurred, the retry coordinator owns the
			// orphan; do not let the generic worker add a second terminal failure.
			recovery := awsConsumeRecovery(ctx, e.aws, change, queued)
			if recovery == awsConsumeFinalized {
				return agenttask.Result{}, agentruntime.ErrTaskFinalized
			}
			if recovery == awsConsumeDeferred {
				return agenttask.Result{}, agentruntime.ErrTaskDeferred
			}
			return agenttask.Result{}, err
		}
	}
	_, executeErr := e.aws.ExecuteChange(ctx, change.ConfirmationID)
	latest, latestErr := e.aws.GetChange(ctx, change.ID)
	if latestErr == nil {
		if latest.Status == coreaws.ChangeSucceeded || latest.Status == coreaws.ChangeFailed || latest.Status == coreaws.ChangeCanceled || latest.Stage == coreaws.StageReconciling {
			return agenttask.Result{}, agentruntime.ErrTaskFinalized
		}
		if latestFence, fenceErr := e.aws.ExecutionFence(ctx, latest.ConfirmationID); fenceErr == nil &&
			latestFence.Confirmation.State == "consumed" &&
			latestFence.Reservation.Active {
			// Once confirmation consumption is durable, only the AWS
			// coordinator may terminalize the task. This also covers an
			// ambiguous database result after a provider response.
			return agenttask.Result{}, agentruntime.ErrTaskFinalized
		}
	}
	if errors.Is(executeErr, coreaws.ErrResponseUncertain) {
		// The durable change is reconciling. Let the lease expire so a
		// successor performs typed readback without repeating a mutation.
		return agenttask.Result{}, agentruntime.ErrTaskFinalized
	}
	return agenttask.Result{}, executeErr
}

type awsConsumeRecoveryResult uint8

const (
	awsConsumeInvalid awsConsumeRecoveryResult = iota
	awsConsumeDeferred
	awsConsumeFinalized
)

func awsConsumeRecovery(ctx context.Context, aws *coreaws.Service, change coreaws.Change, queued agenttask.Task) awsConsumeRecoveryResult {
	if aws == nil || change.Status != coreaws.ChangeWaitingUser || change.Stage != coreaws.StageRequested || change.TaskID != queued.ID {
		return awsConsumeInvalid
	}
	latest, err := aws.GetChange(ctx, change.ID)
	if err != nil {
		return awsConsumeDeferred
	}
	if latest.ID != change.ID || latest.TaskID != queued.ID || latest.ConfirmationID != change.ConfirmationID {
		return awsConsumeInvalid
	}
	fence, err := aws.ExecutionFence(ctx, latest.ConfirmationID)
	if err != nil {
		return awsConsumeDeferred
	}
	if latest.Status == coreaws.ChangeSucceeded || latest.Status == coreaws.ChangeFailed || latest.Status == coreaws.ChangeCanceled || latest.Stage == coreaws.StageReconciling || latest.Stage == coreaws.StageReconciliationRequired || (fence.Confirmation.State == "consumed" && fence.Reservation.Active) {
		return awsConsumeFinalized
	}
	if latest.Status != coreaws.ChangeWaitingUser || latest.Stage != coreaws.StageRequested || latest.ChangeSetID != "" {
		return awsConsumeInvalid
	}
	if fence.Change.ID == latest.ID && fence.Task.ID == queued.ID && fence.Task.Status == "running" &&
		fence.Task.Attempt == queued.Attempt && fence.Task.LeaseEpoch == queued.LeaseEpoch && uint64(fence.Task.Revision) >= queued.Revision &&
		fence.Confirmation.State == "confirmed" && fence.Confirmation.ExpiresAt.After(time.Now().UTC()) && !fence.Reservation.Active && latest.Status == coreaws.ChangeWaitingUser && latest.Stage == coreaws.StageRequested && latest.ChangeSetID == "" {
		return awsConsumeDeferred
	}
	return awsConsumeInvalid
}

func (e *embeddedTaskExecutor) executeWorkload(ctx context.Context, queued agenttask.Task) (agenttask.Result, error) {
	payload := queued.Spec.Payload.Workload
	workloads := e.geoWorkloads
	if workloads == nil {
		workloads = e.workloadService
	}
	handler := e.geoHandler
	if handler == nil {
		handler = e.workloadHandler
	}
	if handler == nil || workloads == nil || payload == nil || queued.Lease == nil {
		return agenttask.Result{}, errors.New("embedded workload task is unavailable")
	}
	op, err := workloads.GetOperation(ctx, payload.OperationID)
	if err != nil || op.TaskID != queued.ID || op.PlanID != payload.PlanID || op.WorkloadID != payload.WorkloadID || op.PlanDigest != payload.PlanDigest || op.PlanRevision != payload.PlanRevision || string(op.TargetKind) != payload.TargetKind {
		return agenttask.Result{}, errors.New("workload task binding is invalid")
	}
	plan, err := workloads.GetPlan(ctx, op.PlanID)
	if err != nil {
		return agenttask.Result{}, errors.New("workload plan is unavailable")
	}
	recovering := op.Status == coreworkload.OperationRunning && (op.DispatchState == "dispatched" || op.DispatchState == "uncertain")
	var mutationLease coreaws.ProvisionMutationLease
	retainMutationLease := false
	markMutationUncertain := func() {
		if mutationLease != nil {
			retainMutationLease = true
			markCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = mutationLease.MarkUncertain(markCtx, op.ID)
		}
	}
	if plan.Target.EC2CleanupProfile == coreworkload.EC2CleanupProfileGeoLibreStaticV1 {
		aws := e.geoAWS
		if aws == nil {
			aws = e.aws
		}
		if aws == nil {
			return agenttask.Result{}, errors.New("GeoLibre AWS provision control is unavailable")
		}
		provisionID := strings.TrimSpace(plan.Target.Labels["dirextalk:provision-id"])
		provisionRevision, parseErr := strconv.ParseInt(plan.Target.Labels["dirextalk:provision-revision"], 10, 64)
		if parseErr != nil || provisionRevision < 1 {
			return agenttask.Result{}, errors.New("GeoLibre provision revision binding is invalid")
		}
		if recovering {
			mutationLease, err = aws.ClaimProvisionMutation(ctx, provisionID, op.ID)
		} else {
			mutationLease, err = aws.AcquireProvisionMutation(ctx, provisionID)
		}
		if err != nil {
			// A recovered task can be reclaimed by the generic 30s task lease
			// while its prior provider-mutation lease is still live for up to ten
			// minutes. ClaimProvisionMutation only permits that same operation to
			// take the lease after expiry (or from uncertain). Do not let the
			// generic worker turn that safe, non-blocking defer into a terminal
			// failure; its next reclaimed attempt will reconcile read-only.
			if recovering && errors.Is(err, coreaws.ErrConflict) {
				return agenttask.Result{}, agentruntime.ErrTaskDeferred
			}
			return agenttask.Result{}, errors.New("GeoLibre provision mutation lock is unavailable")
		}
		if binder, ok := mutationLease.(coreaws.ProvisionMutationOperationBinder); ok {
			bindCtx, bindCancel := context.WithTimeout(ctx, 5*time.Second)
			bindErr := binder.BindOperation(bindCtx, op.ID)
			bindCancel()
			if bindErr != nil {
				retainMutationLease = false
				return agenttask.Result{}, errors.New("GeoLibre provision mutation operation binding is unavailable")
			}
		}
		retainMutationLease = true
		// The generic task worker renews its dispatch lease on the same cadence.
		// Keep the durable provider fence alive with short CAS transactions and
		// cancel the provider call when either fence is lost.
		leaseCtx, leaseCancel := context.WithCancel(ctx)
		renewDone := make(chan struct{})
		go func() {
			defer close(renewDone)
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-leaseCtx.Done():
					return
				case <-ticker.C:
					renewCtx, renewCancel := context.WithTimeout(leaseCtx, 5*time.Second)
					renewErr := mutationLease.Renew(renewCtx)
					renewCancel()
					if renewErr != nil {
						leaseCancel()
						return
					}
				}
			}
		}()
		defer func() {
			leaseCancel()
			<-renewDone
			if !retainMutationLease {
				releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = mutationLease.Release(releaseCtx)
			}
		}()
		provision, provisionErr := aws.GetProvisionForOwner(ctx, provisionID, queued.OwnerID)
		if !recovering && (provisionErr != nil || provision.Revision != provisionRevision || provision.State != "active" || provision.ActiveChangeID != "" || provision.ReconciliationRequired || provision.Readback.Validate() != nil) {
			retainMutationLease = false
			return agenttask.Result{}, errors.New("GeoLibre provision changed after confirmation")
		}
		credential, credentialErr := aws.GetCredentialRevision(ctx, provision.CredentialID, provision.CredentialRevision)
		provisionPlan, provisionPlanErr := aws.GetPlanForOwner(ctx, provision.PlanID, queued.OwnerID)
		validate := e.validateGeoLibre
		if validate == nil {
			validate = agentembedded.ValidateGeoLibreWorkerPreDispatch
		}
		if credentialErr != nil || provisionPlanErr != nil || validate(plan, provision, credential, queued.OwnerID, provisionPlan, recovering) != nil {
			retainMutationLease = false
			return agenttask.Result{}, errors.New("GeoLibre immutable binding changed after confirmation")
		}
		ctx = leaseCtx
		if identity, ok := mutationLease.(coreaws.ProvisionMutationLeaseIdentity); ok {
			ctx = coreworkload.WithMutationLeaseFence(ctx, coreworkload.MutationLeaseFence{OwnerID: queued.OwnerID, ProvisionID: provisionID, OperationID: op.ID, Token: identity.Token(), Epoch: identity.Epoch()})
		}
	}
	fence := coreworkload.TaskFence{
		TaskID: queued.ID, Holder: queued.Lease.Holder, Attempt: queued.Attempt,
		LeaseEpoch: queued.LeaseEpoch, Revision: queued.Revision, ExpiresAt: queued.Lease.ExpiresAt,
	}
	if mutationLease != nil {
		assertCtx, assertCancel := context.WithTimeout(ctx, 5*time.Second)
		assertErr := mutationLease.Assert(assertCtx)
		assertCancel()
		if assertErr != nil {
			markMutationUncertain()
			return agenttask.Result{}, agentruntime.ErrExecutionUncertain
		}
	}
	if recovering {
		_, err = handler.Recover(ctx, op.ID, fence)
	} else {
		_, err = handler.Handle(ctx, op.ID, payload.PlanDigest, op.Revision, fence)
	}
	// CompleteDispatchFenced terminalizes the operation and clears the
	// mutation token atomically. Read that durable state before asserting the
	// lease: an assertion against the intentionally cleared token is not an
	// uncertain provider outcome.
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	latest, latestErr := workloads.GetOperation(readCtx, op.ID)
	readCancel()
	if latestErr == nil {
		switch latest.Status {
		case coreworkload.OperationSucceeded, coreworkload.OperationFailed, coreworkload.OperationUncertain, coreworkload.OperationRejected, coreworkload.OperationExpired, coreworkload.OperationCanceled:
			if mutationLease != nil && latest.Status == coreworkload.OperationUncertain {
				markMutationUncertain()
				return agenttask.Result{}, agentruntime.ErrExecutionUncertain
			}
			retainMutationLease = false
			return agenttask.Result{}, agentruntime.ErrTaskFinalized
		case coreworkload.OperationRunning:
			if latest.DispatchState == "dispatched" || latest.DispatchState == "uncertain" {
				return agenttask.Result{}, agentruntime.ErrTaskFinalized
			}
		}
	}
	if mutationLease != nil {
		assertCtx, assertCancel := context.WithTimeout(context.Background(), 5*time.Second)
		assertErr := mutationLease.Assert(assertCtx)
		assertCancel()
		if assertErr != nil {
			markMutationUncertain()
			return agenttask.Result{}, agentruntime.ErrExecutionUncertain
		}
	}
	readCtx, readCancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()
	latest, latestErr = workloads.GetOperation(readCtx, op.ID)
	if latestErr == nil {
		switch latest.Status {
		case coreworkload.OperationSucceeded, coreworkload.OperationFailed, coreworkload.OperationUncertain, coreworkload.OperationRejected, coreworkload.OperationExpired, coreworkload.OperationCanceled:
			if mutationLease != nil && latest.Status == coreworkload.OperationUncertain {
				markMutationUncertain()
				return agenttask.Result{}, agentruntime.ErrExecutionUncertain
			}
			retainMutationLease = false
			return agenttask.Result{}, agentruntime.ErrTaskFinalized
		case coreworkload.OperationRunning:
			if latest.DispatchState == "dispatched" || latest.DispatchState == "uncertain" {
				return agenttask.Result{}, agentruntime.ErrTaskFinalized
			}
		}
	}
	if mutationLease != nil {
		markMutationUncertain()
		return agenttask.Result{}, agentruntime.ErrExecutionUncertain
	}
	return agenttask.Result{}, err
}

type embeddedControlRuntime struct {
	awsPort      agentembedded.ActionPort
	geolibrePort agentembedded.ActionPort
	workloadPort agentembedded.ActionPort
	mcpPort      agentembedded.ActionPort
	aws          *coreaws.Service
	workload     *coreworkload.Service
	handler      *coreworkload.Handler
	mcp          *agentembedded.PinnedMCPWorkerCoordinator
}

// newEmbeddedControlRuntime wires only in-process, PostgreSQL-backed domains.
// Each domain fails closed independently, so a missing provider never disables
// Matrix/ProductCore or advertises a half-configured Agent capability.
func newEmbeddedControlRuntime(db *p2pstorage.DatabaseStore, tasks *p2pstorage.DatabaseTaskStore, confirmations *p2pstorage.DatabaseConfirmationStore, owner string, enveloper *p2pstorage.AgentSecretEnveloper) embeddedControlRuntime {
	var out embeddedControlRuntime
	if db == nil || tasks == nil || confirmations == nil || strings.TrimSpace(owner) == "" || enveloper == nil {
		return out
	}

	awsRepo, repoErr := p2pstorage.NewAgentAWSRepositoryWithEnveloper(db, owner, enveloper)
	awsProvider, providerErr := coreaws.NewSDKProvider(coreaws.NewSDKFactory())
	if repoErr == nil && providerErr == nil {
		out.aws = coreaws.NewServiceWithCoordinator(awsRepo, awsRepo, nil, nil, awsProvider, awsProvider, time.Now)
		out.awsPort, _ = agentembedded.NewAWSActionPort(func(requestOwner string) (*coreaws.Service, error) {
			if strings.TrimSpace(requestOwner) != owner {
				return nil, errors.New("agent owner mismatch")
			}
			return out.aws, nil
		})
	}

	if repoErr == nil {
		workloadStore, storeErr := p2pstorage.NewAgentWorkloadStore(db, owner)
		credentialResolver, credentialErr := workaws.NewCredentialResolver(awsRepo)
		secretResolver := workaws.CanonicalSecretReference{}
		ssmProvider, ssmErr := workssm.NewProvider(workssm.StaticFactory{}, credentialResolver, secretResolver)
		ecsProvider, ecsErr := workecs.NewProvider(workecs.StaticFactory{}, credentialResolver, secretResolver)
		if storeErr == nil && credentialErr == nil && ssmErr == nil && ecsErr == nil {
			registry, registryErr := coreworkload.NewProviderRegistry(map[coreworkload.TargetKind]coreworkload.Provider{
				coreworkload.TargetAWSEC2SSM: ssmProvider,
				coreworkload.TargetAWSECS:    ecsProvider,
			})
			workloadService, serviceErr := coreworkload.NewService(workloadStore, time.Now)
			workloadHandler, handlerErr := coreworkload.NewHandler(workloadStore, registry)
			if registryErr == nil && serviceErr == nil && handlerErr == nil && workloadService.ReadyForEmbedded(workloadHandler) {
				out.workload, out.handler = workloadService, workloadHandler
				out.workloadPort, _ = agentembedded.NewWorkloadActionPort(func(requestOwner string) (*coreworkload.Service, *coreworkload.Handler, error) {
					if strings.TrimSpace(requestOwner) != owner {
						return nil, nil, errors.New("agent owner mismatch")
					}
					return workloadService, workloadHandler, nil
				})
			}
		}
	}
	if out.aws != nil && out.workload != nil {
		out.geolibrePort = agentembedded.ActionPortFunc(func(ctx context.Context, requestOwner, action string, params map[string]any) (any, *actionbase.Error) {
			if strings.TrimSpace(requestOwner) != owner {
				return nil, actionbase.CodedError(412, "agent_embedded_unavailable", agentembedded.ErrUnavailable.Error())
			}
			return agentembedded.NewGeoLibreActionPort(out.aws, out.workload).Handle(ctx, requestOwner, action, params)
		})
	}

	extensionStore, extensionErr := p2pstorage.NewAtomicExtensionStore(db, enveloper)
	mcpCatalog, catalogErr := extensioncatalog.New(extensioncatalog.Config{})
	if extensionErr == nil && catalogErr == nil {
		clientFactory := agentembedded.NewPinnedMCPClientFactory(func(requestOwner, installationID, _ string) coreextensions.SecretResolver {
			if strings.TrimSpace(requestOwner) != owner {
				return nil
			}
			return &p2pstorage.AgentExtensionSecretResolver{Store: db, Enveloper: enveloper, InstallationID: installationID}
		})
		extensionService := &coreextensions.Service{Store: extensionStore, Now: time.Now}
		runner := agentembedded.NewPinnedMCPWorker(extensionStore, clientFactory)
		out.mcpPort, _ = agentembedded.NewReadyMCPActionPort(agentembedded.MCPActionPortDependencies{
			Service: extensionService, Tasks: tasks, Confirmations: confirmations,
			Runner: runner, Client: clientFactory, Catalog: mcpCatalog, Atomic: true,
		})
		if out.mcpPort != nil {
			out.mcp = &agentembedded.PinnedMCPWorkerCoordinator{
				Extensions: extensionStore,
				Tasks:      tasks,
				Confirmations: agentembedded.ConfirmationAdapter{
					Repository: confirmations,
				},
				Finalizer: extensionStore,
				Client:    clientFactory,
			}
		}
	}
	return out
}

func firstResultString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func boundedRuntimeResult(value string) string {
	if len([]byte(value)) <= agenttask.MaxSummaryBytes {
		return value
	}
	var out strings.Builder
	for _, r := range value {
		part := string(r)
		if out.Len()+len([]byte(part)) > agenttask.MaxSummaryBytes {
			break
		}
		out.WriteString(part)
	}
	return out.String()
}

var _ agentruntime.Executor = (*embeddedTaskExecutor)(nil)
