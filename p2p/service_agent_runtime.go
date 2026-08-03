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
	agent *agentmodule.Module
	aws   *coreaws.Service
	mcp   *agentembedded.PinnedMCPWorkerCoordinator
	// executionStage is intentionally a typed worker seam. The generic task
	// store only hands the claimed payload to this runner; it never executes
	// execution.v2 steps itself.
	executionStage interface {
		Execute(context.Context, agenttask.Task) (agenttask.Result, error)
	}
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
	case agenttask.TaskKindExecutionStage:
		if e.executionStage == nil {
			return agenttask.Result{}, errors.New("execution stage runtime is unavailable")
		}
		return e.executionStage.Execute(ctx, queued)
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

type embeddedControlRuntime struct {
	awsPort agentembedded.ActionPort
	mcpPort agentembedded.ActionPort
	aws     *coreaws.Service
	mcp     *agentembedded.PinnedMCPWorkerCoordinator
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
		out.aws = coreaws.NewService(awsRepo, nil, nil, awsProvider, nil, time.Now)
		out.awsPort, _ = agentembedded.NewAWSActionPort(func(requestOwner string) (*coreaws.Service, error) {
			if strings.TrimSpace(requestOwner) != owner {
				return nil, errors.New("agent owner mismatch")
			}
			return out.aws, nil
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
