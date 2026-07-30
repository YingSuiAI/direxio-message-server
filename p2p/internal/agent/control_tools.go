package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/google/uuid"
)

// ControlTools exposes the intentionally small first-party deployment read
// surface to Native chat. Mutating plans and confirmations remain client-owned
// actions; the model cannot supply provider targets, commands, or secret grants.
func ControlTools(control ControlInvoker) []nativeagent.Tool {
	if control == nil {
		return nil
	}
	tool := func(name, description, action string, parameters map[string]any, allowed []string, write bool, idempotent bool) nativeagent.Tool {
		return nativeagent.Tool{
			Name: name, Description: description, Parameters: parameters, Write: write,
			Available: func() bool { return control.Available(action) },
			Handler: func(ctx context.Context, params map[string]any) (any, error) {
				request := projectControlParams(params, allowed)
				if idempotent {
					owner, conversation, _ := nativeagent.RequestContext(ctx)
					intent := nativeagent.RequestIntent(ctx)
					if strings.TrimSpace(owner) == "" || strings.TrimSpace(conversation) == "" || strings.TrimSpace(intent) == "" {
						return nil, fmt.Errorf("native agent control write requires owner, conversation, and turn context")
					}
					request["idempotency_key"] = controlIdempotencyKey(ctx, action, request)
				}
				return control.Invoke(ctx, action, request)
			},
		}
	}
	return []nativeagent.Tool{
		tool("native_agent_aws_credentials_list", "List redacted AWS credentials.", "agent.core.aws.credentials.list", pageSchema(), []string{"page_size", "page_token"}, false, false),
		tool("native_agent_aws_credentials_test", "Test an AWS credential without returning secret material.", "agent.core.aws.credentials.test", objectSchema(map[string]any{"credential_id": stringSchema()}), []string{"credential_id"}, false, false),
		tool("native_agent_workloads_list", "List workload deployment plans.", "agent.core.workloads.list", pageSchema(), []string{"page_size", "page_token"}, false, false),
		tool("native_agent_workloads_get", "Get a workload deployment plan.", "agent.core.workloads.get", objectSchema(map[string]any{"plan_id": stringSchema()}), []string{"plan_id"}, false, false),
		tool("native_agent_workloads_quote", "Quote a workload deployment plan.", "agent.core.workloads.quote", objectSchema(map[string]any{"plan_id": stringSchema()}), []string{"plan_id"}, false, false),
		tool("native_agent_workload_operations_get", "Get a workload operation.", "agent.core.workloads.operations.get", objectSchema(map[string]any{"operation_id": stringSchema()}), []string{"operation_id"}, false, false),
		tool("native_agent_workload_operations_events", "List workload operation events.", "agent.core.workloads.operations.events", objectSchema(map[string]any{"operation_id": stringSchema(), "after_sequence": numberSchema()}), []string{"operation_id", "after_sequence"}, false, false),
		tool("native_agent_deployments_list", "List owner deployments.", "agent.core.deployments.list", deploymentListSchema(), []string{"page_size", "page_token", "status", "target_kind"}, false, false),
		tool("native_agent_deployments_get", "Get an owner deployment.", "agent.core.deployments.get", objectSchema(map[string]any{"workload_id": stringSchema()}), []string{"workload_id"}, false, false),
		tool("native_agent_deployments_events", "List owner deployment events.", "agent.core.deployments.events", objectSchema(map[string]any{"workload_id": stringSchema(), "after_sequence": numberSchema(), "page_size": numberSchema()}), []string{"workload_id", "after_sequence", "page_size"}, false, false),
	}
}

func projectControlParams(params map[string]any, allowed []string) map[string]any {
	out := make(map[string]any, len(allowed))
	for _, key := range allowed {
		if value, ok := params[key]; ok {
			out[key] = value
		}
	}
	return out
}

func controlIdempotencyKey(ctx context.Context, _ string, _ map[string]any) string {
	owner, conversation, _ := nativeagent.RequestContext(ctx)
	intent := nativeagent.RequestIntent(ctx)
	scope := strings.TrimSpace(owner) + "\x00" + strings.TrimSpace(conversation) + "\x00" + strings.TrimSpace(intent)
	digest := sha256.Sum256([]byte(scope))
	return uuid.NewSHA1(uuid.Nil, digest[:]).String()
}

func pageSchema() map[string]any {
	return objectSchema(map[string]any{"page_size": numberSchema(), "page_token": stringSchema()})
}

func deploymentListSchema() map[string]any {
	return objectSchema(map[string]any{"page_size": numberSchema(), "page_token": stringSchema(), "status": stringSchema(), "target_kind": stringSchema()})
}
