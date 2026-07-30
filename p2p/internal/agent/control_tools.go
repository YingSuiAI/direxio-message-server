package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

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
				if strings.HasSuffix(action, "geolibre_install.request") {
					_, hasWorkload := request["workload_id"]
					_, hasWorkloadRevision := request["expected_workload_revision"]
					if hasWorkload != hasWorkloadRevision {
						return nil, fmt.Errorf("GeoLibre workload_id and expected_workload_revision must be supplied together")
					}
					expiry, ok := request["expires_at"].(string)
					if !ok || strings.TrimSpace(expiry) == "" {
						return nil, fmt.Errorf("GeoLibre install request requires the exact plan expiry")
					}
				}
				if strings.HasSuffix(action, "geolibre_install.plan") {
					issuedAt, ok := nativeagent.RequestIssuedAt(ctx)
					if !ok || issuedAt.After(time.Now().UTC().Add(time.Minute)) || time.Now().UTC().After(issuedAt.Add(geoLibrePlanTTL)) {
						return nil, fmt.Errorf("GeoLibre plan issuance context is unavailable or expired")
					}
					request["expires_at"] = issuedAt.Add(geoLibrePlanTTL).Format(time.RFC3339Nano)
				}
				result, err := control.Invoke(ctx, action, request)
				if err != nil {
					return nil, err
				}
				return redactNativeControlResult(result), nil
			},
		}
	}
	return []nativeagent.Tool{
		tool("native_agent_aws_credentials_list", "List redacted AWS credentials.", "agent.core.aws.credentials.list", pageSchema(), []string{"page_size", "page_token"}, false, false),
		tool("native_agent_aws_credentials_test", "Test an AWS credential without returning secret material.", "agent.core.aws.credentials.test", strictObjectSchema(map[string]any{"credential_id": stringSchema()}, "credential_id"), []string{"credential_id"}, false, false),
		// Generic workload plan/list projections are intentionally not exposed:
		// their public DTOs may contain command/image material. Native Agent can
		// inspect the operation and verified actual readback instead.
		tool("native_agent_workload_operations_get", "Get a workload operation.", "agent.core.workloads.operations.get", strictObjectSchema(map[string]any{"operation_id": stringSchema()}, "operation_id"), []string{"operation_id"}, false, false),
		tool("native_agent_workload_operations_events", "List workload operation events.", "agent.core.workloads.operations.events", strictObjectSchema(map[string]any{"operation_id": stringSchema(), "after_sequence": integerSchema()}, "operation_id"), []string{"operation_id", "after_sequence"}, false, false),
		tool("native_agent_workload_actual_get", "Get verified workload readback.", "agent.core.workloads.actual.get", strictObjectSchema(map[string]any{"workload_id": stringSchema()}, "workload_id"), []string{"workload_id"}, false, false),
		tool("native_agent_deployments_list", "List owner deployments.", "agent.core.deployments.list", strictObjectSchema(map[string]any{"page_size": integerSchema(), "page_token": stringSchema(), "status": stringSchema(), "target_kind": stringSchema()}), []string{"page_size", "page_token", "status", "target_kind"}, false, false),
		tool("native_agent_deployments_get", "Get an owner deployment by canonical deployment_id or legacy workload_id.", "agent.core.deployments.get", strictObjectSchema(map[string]any{"deployment_id": stringSchema(), "workload_id": stringSchema()}), []string{"deployment_id", "workload_id"}, false, false),
		tool("native_agent_deployments_events", "List owner deployment events by canonical deployment_id or legacy workload_id.", "agent.core.deployments.events", strictObjectSchema(map[string]any{"deployment_id": stringSchema(), "workload_id": stringSchema(), "after_sequence": integerSchema(), "page_size": integerSchema()}), []string{"deployment_id", "workload_id", "after_sequence", "page_size"}, false, false),

		// EC2 provisioning is intentionally typed. The plan action persists a
		// reviewable plan only; create/destroy merely create a task and pending
		// confirmation. No provider call or confirmation consumer is exposed to
		// the model.
		tool("native_agent_aws_ec2_provisions_plan", "Create a typed EC2 provision plan for owner review.", "agent.core.aws.ec2_provisions.plan", ec2ProvisionPlanSchema(), []string{"credential_id", "expected_credential_revision", "region", "stack_name", "display_name", "instance_type", "volume_gib", "public_http", "acknowledge_public_exposure"}, true, true),
		tool("native_agent_aws_ec2_provisions_get", "Get an owner-bound EC2 provision.", "agent.core.aws.ec2_provisions.get", strictObjectSchema(map[string]any{"provision_id": stringSchema()}, "provision_id"), []string{"provision_id"}, false, false),
		tool("native_agent_aws_ec2_provisions_list", "List owner-bound EC2 provisions.", "agent.core.aws.ec2_provisions.list", strictObjectSchema(map[string]any{"page_size": integerSchema(), "page_token": stringSchema(), "state": stringSchema()}), []string{"page_size", "page_token", "state"}, false, false),
		tool("native_agent_aws_ec2_provisions_events", "List EC2 provision events after a sequence.", "agent.core.aws.ec2_provisions.events", strictObjectSchema(map[string]any{"provision_id": stringSchema(), "after_sequence": integerSchema(), "limit": integerSchema()}, "provision_id"), []string{"provision_id", "after_sequence", "limit"}, false, false),
		tool("native_agent_aws_ec2_provisions_create_request", "Request EC2 creation; the owner must confirm in the client.", "agent.core.aws.ec2_provisions.create.request", strictObjectSchema(map[string]any{"provision_id": stringSchema(), "expected_revision": integerSchema()}, "provision_id", "expected_revision"), []string{"provision_id", "expected_revision"}, true, true),
		tool("native_agent_aws_ec2_provisions_destroy_request", "Request EC2 destruction; the owner must confirm in the client.", "agent.core.aws.ec2_provisions.destroy.request", strictObjectSchema(map[string]any{"provision_id": stringSchema(), "expected_revision": integerSchema()}, "provision_id", "expected_revision"), []string{"provision_id", "expected_revision"}, true, true),
		tool("native_agent_aws_ec2_geolibre_install_plan", "Create the fixed GeoLibre install plan for owner review.", "agent.core.aws.ec2_provisions.geolibre_install.plan", strictObjectSchema(map[string]any{"provision_id": stringSchema(), "expected_revision": integerSchema()}, "provision_id", "expected_revision"), []string{"provision_id", "expected_revision"}, true, true),
		tool("native_agent_aws_ec2_geolibre_install_request", "Request fixed GeoLibre installation; the owner must confirm in the client.", "agent.core.aws.ec2_provisions.geolibre_install.request", strictObjectSchema(map[string]any{"provision_id": stringSchema(), "expected_revision": integerSchema(), "plan_id": stringSchema(), "plan_revision": integerSchema(), "plan_digest": stringSchema(), "expires_at": stringSchema(), "workload_id": stringSchema(), "expected_workload_revision": integerSchema()}, "provision_id", "expected_revision", "plan_id", "plan_revision", "plan_digest", "expires_at"), []string{"provision_id", "expected_revision", "plan_id", "plan_revision", "plan_digest", "expires_at", "workload_id", "expected_workload_revision"}, true, true),
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

// redactNativeControlResult is the Native Agent boundary. ProductCore actions
// may return richer DTOs to trusted client callers; model/tool memory receives
// only immutable digests, IDs and typed readback, never owners, labels,
// desired plans, image URIs, commands, or selected command details.
func redactNativeControlResult(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "owner_id", "selected_command", "command_steps", "image_uri", "desired_plan", "labels", "typed_secret_grants":
				continue
			}
			out[key] = redactNativeControlResult(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, redactNativeControlResult(item))
		}
		return out
	default:
		return value
	}
}

// RedactNativeControlResult applies the Native Agent output boundary before a
// tool result is persisted or handed to confirmation-card extraction.
func RedactNativeControlResult(value any) any { return redactNativeControlResult(value) }

func controlIdempotencyKey(ctx context.Context, _ string, _ map[string]any) string {
	owner, conversation, _ := nativeagent.RequestContext(ctx)
	intent := nativeagent.RequestIntent(ctx)
	scope := strings.TrimSpace(owner) + "\x00" + strings.TrimSpace(conversation) + "\x00" + strings.TrimSpace(intent)
	digest := sha256.Sum256([]byte(scope))
	return uuid.NewSHA1(uuid.Nil, digest[:]).String()
}

func pageSchema() map[string]any {
	return strictObjectSchema(map[string]any{"page_size": integerSchema(), "page_token": stringSchema()})
}

func deploymentListSchema() map[string]any {
	return strictObjectSchema(map[string]any{"page_size": integerSchema(), "page_token": stringSchema(), "status": stringSchema(), "target_kind": stringSchema()})
}

func ec2ProvisionPlanSchema() map[string]any {
	return strictObjectSchema(map[string]any{
		"credential_id":                stringSchema(),
		"expected_credential_revision": integerSchema(),
		"region":                       stringSchema(),
		"stack_name":                   stringSchema(),
		"display_name":                 stringSchema(),
		"instance_type":                stringSchema(),
		"volume_gib":                   integerSchema(),
		"public_http":                  boolSchema(),
		"acknowledge_public_exposure":  boolSchema(),
	}, "credential_id", "expected_credential_revision", "region", "stack_name", "display_name", "instance_type", "volume_gib", "public_http", "acknowledge_public_exposure")
}

func boolSchema() map[string]any { return map[string]any{"type": "boolean"} }

func integerSchema() map[string]any { return map[string]any{"type": "integer"} }

const geoLibrePlanTTL = 15 * time.Minute

func strictObjectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
