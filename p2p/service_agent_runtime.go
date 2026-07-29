package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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

func (e *embeddedTaskExecutor) executeWorkload(ctx context.Context, queued agenttask.Task) (agenttask.Result, error) {
	payload := queued.Spec.Payload.Workload
	if e.workloadHandler == nil || e.workloadService == nil || payload == nil || queued.Lease == nil {
		return agenttask.Result{}, errors.New("embedded workload task is unavailable")
	}
	op, err := e.workloadService.GetOperation(ctx, payload.OperationID)
	if err != nil || op.TaskID != queued.ID || op.PlanID != payload.PlanID || op.WorkloadID != payload.WorkloadID || op.PlanDigest != payload.PlanDigest || op.PlanRevision != payload.PlanRevision || string(op.TargetKind) != payload.TargetKind {
		return agenttask.Result{}, errors.New("workload task binding is invalid")
	}
	fence := coreworkload.TaskFence{
		TaskID: queued.ID, Holder: queued.Lease.Holder, Attempt: queued.Attempt,
		LeaseEpoch: queued.LeaseEpoch, Revision: queued.Revision, ExpiresAt: queued.Lease.ExpiresAt,
	}
	if op.Status == coreworkload.OperationRunning && (op.DispatchState == "dispatched" || op.DispatchState == "uncertain") {
		_, err = e.workloadHandler.Recover(ctx, op.ID, fence)
	} else {
		_, err = e.workloadHandler.Handle(ctx, op.ID, payload.PlanDigest, op.Revision, fence)
	}
	latest, latestErr := e.workloadService.GetOperation(ctx, op.ID)
	if latestErr == nil {
		switch latest.Status {
		case coreworkload.OperationSucceeded, coreworkload.OperationFailed, coreworkload.OperationUncertain, coreworkload.OperationRejected, coreworkload.OperationExpired, coreworkload.OperationCanceled:
			return agenttask.Result{}, agentruntime.ErrTaskFinalized
		case coreworkload.OperationRunning:
			if latest.DispatchState == "dispatched" || latest.DispatchState == "uncertain" {
				return agenttask.Result{}, agentruntime.ErrTaskFinalized
			}
		}
	}
	return agenttask.Result{}, err
}

type embeddedControlRuntime struct {
	awsPort      agentembedded.ActionPort
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
