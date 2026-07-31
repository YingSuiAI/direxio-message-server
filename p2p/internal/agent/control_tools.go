package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/google/uuid"
)

// ControlTools exposes the intentionally small first-party control surface to
// Native chat. A write may create an immutable analysis/plan/run or invoke a
// schema-pinned service binding, but confirmation consumption and provider
// transports are deliberately absent. Every tool remains hidden until its
// exact ProductCore action reports ready.
func ControlTools(control ControlInvoker) []nativeagent.Tool {
	if control == nil {
		return nil
	}
	tool := func(name, description, action string, parameters map[string]any, allowed []string, write bool, idempotent bool) nativeagent.Tool {
		return nativeagent.Tool{
			Name: name, Description: description, Parameters: parameters, Write: write,
			Available: func() bool { return control.Available(action) },
			Handler: func(ctx context.Context, params map[string]any) (any, error) {
				request, err := projectControlParams(params, allowed)
				if err != nil {
					return nil, err
				}
				if err := rejectNativeControlSecrets(request); err != nil {
					return nil, err
				}
				if err := validateNativeControlRequest(action, request); err != nil {
					return nil, err
				}
				if idempotent {
					owner, conversation, _ := nativeagent.RequestContext(ctx)
					intent := nativeagent.RequestIntent(ctx)
					if strings.TrimSpace(owner) == "" || strings.TrimSpace(conversation) == "" || strings.TrimSpace(intent) == "" {
						return nil, fmt.Errorf("native agent control write requires owner, conversation, and turn context")
					}
					request["idempotency_key"] = controlIdempotencyKey(ctx, action, request)
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
		tool("native_agent_aws_credentials_test", "Test an exact AWS credential revision without returning secret material.", "agent.core.aws.credentials.test", strictObjectSchema(map[string]any{"credential_id": uuidSchema(), "expected_revision": positiveIntegerSchema()}, "credential_id", "expected_revision"), []string{"credential_id", "expected_revision"}, true, true),

		tool("native_agent_execution_v2_projects_analyze", "Analyze one immutable project source. The server computes the analysis and its digest.", "agent.execution.v2.projects.analyze", strictObjectSchema(map[string]any{
			"project_id": uuidSchema(), "source": immutableSourceSchema(),
		}, "project_id", "source"), []string{"project_id", "source"}, true, true),
		tool("native_agent_execution_v2_targets_list", "List owner-scoped imported or reserved targets with their latest revision and capabilities.", "agent.execution.v2.targets.list", pageSchema(), []string{"page_size", "page_token"}, false, false),
		tool("native_agent_execution_v2_targets_get", "Get one owner-scoped imported or reserved target at an exact revision.", "agent.execution.v2.targets.get", strictObjectSchema(map[string]any{
			"target_id": uuidSchema(), "revision": positiveIntegerSchema(),
		}, "target_id"), []string{"target_id", "revision"}, false, false),
		tool("native_agent_execution_v2_plans_create", "Compile and persist a reviewable execution-plan/v2 snapshot from a server-authored analysis and a pinned built-in recipe.", "agent.execution.v2.plans.create", strictObjectSchema(map[string]any{
			"project_id": uuidSchema(), "analysis_id": uuidSchema(), "intent": nameTokenSchema(), "recipe_id": nameTokenSchema(),
			"target_id": uuidSchema(), "target_revision": positiveIntegerSchema(), "purpose": enumStringSchema("service", "job"),
		}, "project_id", "analysis_id", "intent", "recipe_id", "target_id", "target_revision", "purpose"), []string{"project_id", "analysis_id", "intent", "recipe_id", "target_id", "target_revision", "purpose"}, true, true),
		tool("native_agent_execution_v2_plans_get", "Get an immutable execution-plan/v2 snapshot for review.", "agent.execution.v2.plans.get", strictObjectSchema(map[string]any{"plan_id": uuidSchema(), "revision": positiveIntegerSchema()}, "plan_id"), []string{"plan_id", "revision"}, false, false),
		tool("native_agent_execution_v2_runs_create", "Create an initial deployment run from an exact plan revision. Gated stages still require owner confirmation in the client; upgrade, repair, destroy, and rollback recipes are not enabled yet.", "agent.execution.v2.runs.create", strictObjectSchema(map[string]any{
			"plan_id": uuidSchema(), "plan_revision": positiveIntegerSchema(), "operation": enumStringSchema("execute", "deploy"),
			"trigger_kind": enumStringSchema("manual", "schedule", "retry"), "rollback_of_run_id": uuidSchema(),
		}, "plan_id", "plan_revision", "operation"), []string{"plan_id", "plan_revision", "operation", "trigger_kind", "rollback_of_run_id"}, true, true),
		tool("native_agent_execution_v2_runs_get", "Get a run and its materialized stage states.", "agent.execution.v2.runs.get", strictObjectSchema(map[string]any{"run_id": uuidSchema()}, "run_id"), []string{"run_id"}, false, false),
		tool("native_agent_execution_v2_runs_status", "Get the authoritative status of a run and its materialized stages.", "agent.execution.v2.runs.get", strictObjectSchema(map[string]any{"run_id": uuidSchema()}, "run_id"), []string{"run_id"}, false, false),
		tool("native_agent_execution_v2_runs_events", "List ordered, redacted events for a run.", "agent.execution.v2.runs.events", strictObjectSchema(map[string]any{"run_id": uuidSchema(), "after_sequence": nonNegativeIntegerSchema(), "limit": positiveIntegerSchema()}, "run_id"), []string{"run_id", "after_sequence", "limit"}, false, false),
		tool("native_agent_execution_v2_service_bindings_list", "List machine-readable service bindings created by successful deployments.", "agent.execution.v2.service_bindings.list", strictObjectSchema(map[string]any{"project_id": uuidSchema(), "page_size": positiveIntegerSchema(), "page_token": stringSchema()}), []string{"project_id", "page_size", "page_token"}, false, false),
		tool("native_agent_execution_v2_service_bindings_get", "Get one schema-pinned service binding by ID.", "agent.execution.v2.service_bindings.get", strictObjectSchema(map[string]any{"binding_id": uuidSchema()}, "binding_id"), []string{"binding_id"}, false, false),
		tool("native_agent_execution_v2_service_bindings_invoke", "Invoke a schema-pinned operation on an exact service-binding revision. This cannot call an arbitrary URL.", "agent.execution.v2.service_bindings.invoke", strictObjectSchema(map[string]any{
			"binding_id": uuidSchema(), "operation": stringSchema(), "expected_revision": positiveIntegerSchema(), "input": openObjectSchema(),
		}, "binding_id", "operation", "expected_revision", "input"), []string{"binding_id", "operation", "expected_revision", "input"}, true, true),
	}
}

func projectControlParams(params map[string]any, allowed []string) (map[string]any, error) {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range params {
		if _, ok := allowedSet[key]; !ok {
			return nil, fmt.Errorf("native agent control request contains unknown field %q", key)
		}
	}
	out := make(map[string]any, len(allowed))
	for _, key := range allowed {
		if value, ok := params[key]; ok {
			out[key] = value
		}
	}
	return out, nil
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
			if nativeControlSensitiveKey(key) {
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
	case string:
		if nativeControlSensitiveValue(typed) {
			return "[REDACTED]"
		}
		return typed
	default:
		valueOf := reflect.ValueOf(value)
		if valueOf.IsValid() {
			switch valueOf.Kind() {
			case reflect.Map, reflect.Struct, reflect.Slice, reflect.Array, reflect.Pointer:
				raw, err := json.Marshal(value)
				if err != nil {
					return nil
				}
				decoder := json.NewDecoder(strings.NewReader(string(raw)))
				decoder.UseNumber()
				var decoded any
				if err := decoder.Decode(&decoded); err != nil {
					return nil
				}
				return redactNativeControlResult(decoded)
			}
		}
		return value
	}
}

// rejectNativeControlSecrets prevents model-authored tool arguments from
// becoming a secret transport. Credential/auth references remain allowed;
// secret bytes, bearer material, private keys, and signed URLs do not.
func rejectNativeControlSecrets(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if nativeControlInputSecretKey(key) {
				return fmt.Errorf("native agent control request contains forbidden secret field %q", key)
			}
			if err := rejectNativeControlSecrets(item); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := rejectNativeControlSecrets(item); err != nil {
				return err
			}
		}
	case string:
		lower := strings.ToLower(strings.TrimSpace(typed))
		for _, marker := range []string{"-----begin private key", "-----begin rsa private key", "bearer ", "access_token=", "refresh_token=", "x-amz-signature=", "x-amz-credential="} {
			if strings.Contains(lower, marker) {
				return fmt.Errorf("native agent control request contains forbidden secret material")
			}
		}
	default:
		valueOf := reflect.ValueOf(value)
		if valueOf.IsValid() && valueOf.Kind() == reflect.Map {
			return fmt.Errorf("native agent control request contains an unsupported map type")
		}
	}
	return nil
}

func validateNativeControlRequest(action string, request map[string]any) error {
	requireUUID := func(key string) error {
		value, ok := request[key].(string)
		if !ok || uuid.Validate(strings.TrimSpace(value)) != nil {
			return fmt.Errorf("native agent control request requires a valid %s", key)
		}
		return nil
	}
	optionalUUID := func(key string) error {
		if _, ok := request[key]; !ok {
			return nil
		}
		return requireUUID(key)
	}
	positive := func(key string, required bool) error {
		value, ok := request[key]
		if !ok {
			if required {
				return fmt.Errorf("native agent control request requires %s", key)
			}
			return nil
		}
		if n, ok := nativeControlInteger(value); !ok || n < 1 {
			return fmt.Errorf("native agent control request requires a positive %s", key)
		}
		return nil
	}
	nonNegative := func(key string) error {
		value, ok := request[key]
		if !ok {
			return nil
		}
		if n, ok := nativeControlInteger(value); !ok || n < 0 {
			return fmt.Errorf("native agent control request requires a non-negative %s", key)
		}
		return nil
	}

	switch action {
	case "agent.core.aws.credentials.list":
		return positive("page_size", false)
	case "agent.core.aws.credentials.test":
		if err := requireUUID("credential_id"); err != nil {
			return err
		}
		return positive("expected_revision", true)
	case "agent.execution.v2.projects.analyze":
		if err := requireUUID("project_id"); err != nil {
			return err
		}
		return validateNativeSource(request["source"])
	case "agent.execution.v2.targets.list":
		if err := positive("page_size", false); err != nil {
			return err
		}
		if token, ok := request["page_token"]; ok {
			value, stringOK := token.(string)
			if !stringOK || len(value) > 512 {
				return fmt.Errorf("native agent control request contains invalid page_token")
			}
		}
		return nil
	case "agent.execution.v2.targets.get":
		if err := requireUUID("target_id"); err != nil {
			return err
		}
		return positive("revision", false)
	case "agent.execution.v2.plans.create":
		for _, key := range []string{"project_id", "analysis_id", "target_id"} {
			if err := requireUUID(key); err != nil {
				return err
			}
		}
		if err := positive("target_revision", true); err != nil {
			return err
		}
		for _, key := range []string{"intent", "recipe_id"} {
			value, ok := request[key].(string)
			if !ok || !nativeControlNameToken(value) {
				return fmt.Errorf("native agent control request contains invalid %s", key)
			}
		}
		purpose, ok := request["purpose"].(string)
		if !ok || (purpose != "service" && purpose != "job") {
			return fmt.Errorf("native agent control request contains invalid purpose")
		}
		return nil
	case "agent.execution.v2.plans.get":
		if err := requireUUID("plan_id"); err != nil {
			return err
		}
		return positive("revision", false)
	case "agent.execution.v2.runs.create":
		if err := requireUUID("plan_id"); err != nil {
			return err
		}
		if err := positive("plan_revision", true); err != nil {
			return err
		}
		operation, ok := request["operation"].(string)
		if !ok {
			return fmt.Errorf("native agent control request requires operation")
		}
		switch strings.TrimSpace(operation) {
		case "execute", "deploy":
		default:
			return fmt.Errorf("native agent control request contains unsupported operation %q", operation)
		}
		if trigger, present := request["trigger_kind"]; present {
			value, stringOK := trigger.(string)
			if !stringOK {
				return fmt.Errorf("native agent control request contains invalid trigger_kind")
			}
			switch strings.TrimSpace(value) {
			case "manual", "schedule", "retry":
			default:
				return fmt.Errorf("native agent control request contains unsupported trigger_kind %q", value)
			}
		}
		rollbackID, hasRollback := request["rollback_of_run_id"]
		if operation == "rollback" {
			value, stringOK := rollbackID.(string)
			if !hasRollback || !stringOK || uuid.Validate(strings.TrimSpace(value)) != nil {
				return fmt.Errorf("native agent control rollback requires rollback_of_run_id")
			}
		} else if hasRollback {
			return fmt.Errorf("native agent control rollback_of_run_id is only valid for rollback")
		}
		return nil
	case "agent.execution.v2.runs.get":
		return requireUUID("run_id")
	case "agent.execution.v2.runs.events":
		if err := requireUUID("run_id"); err != nil {
			return err
		}
		if err := nonNegative("after_sequence"); err != nil {
			return err
		}
		return positive("limit", false)
	case "agent.execution.v2.service_bindings.list":
		if err := optionalUUID("project_id"); err != nil {
			return err
		}
		return positive("page_size", false)
	case "agent.execution.v2.service_bindings.get":
		return requireUUID("binding_id")
	case "agent.execution.v2.service_bindings.invoke":
		if err := requireUUID("binding_id"); err != nil {
			return err
		}
		if err := positive("expected_revision", true); err != nil {
			return err
		}
		operation, ok := request["operation"].(string)
		if !ok || strings.TrimSpace(operation) == "" || len(strings.TrimSpace(operation)) > 128 {
			return fmt.Errorf("native agent control request requires a bounded operation")
		}
		if _, ok := request["input"].(map[string]any); !ok {
			return fmt.Errorf("native agent control request requires an object input")
		}
		return nil
	default:
		return fmt.Errorf("native agent control action %q is not allowlisted", strings.TrimSpace(action))
	}
}

func nativeControlInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) <= math.MaxInt64 {
			return int64(typed), true
		}
	case uint64:
		if typed <= math.MaxInt64 {
			return int64(typed), true
		}
	case float64:
		if typed >= math.MinInt64 && typed <= math.MaxInt64 && math.Trunc(typed) == typed {
			return int64(typed), true
		}
	}
	return 0, false
}

func validateNativeSource(value any) error {
	source, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("native agent control request requires an immutable source object")
	}
	kind, kindOK := source["kind"].(string)
	immutable, immutableOK := source["immutable"].(bool)
	kind = strings.TrimSpace(kind)
	if !kindOK || !immutableOK || !immutable {
		return fmt.Errorf("native agent control request requires an immutable source object")
	}
	allowed := map[string]bool{"kind": true, "location": true, "immutable": true}
	switch kind {
	case "git_https":
		allowed["commit"] = true
		allowed["credential_ref"] = true
		allowed["credential_revision"] = true
		location, ok := source["location"].(string)
		if !ok {
			return fmt.Errorf("native agent control git_https source requires a repository location")
		}
		parsed, err := url.Parse(strings.TrimSpace(location))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return fmt.Errorf("native agent control source must use credential-free HTTPS")
		}
		commit, ok := source["commit"].(string)
		if !ok || (!nativeControlHexPin(commit, 40) && !nativeControlHexPin(commit, 64)) {
			return fmt.Errorf("native agent control git source requires an exact commit")
		}
		credentialRef, hasRef := source["credential_ref"]
		credentialRevision, hasRevision := source["credential_revision"]
		if hasRef != hasRevision {
			return fmt.Errorf("native agent control credential_ref and credential_revision must be supplied together")
		}
		if hasRef {
			ref, refOK := credentialRef.(string)
			if !refOK || uuid.Validate(strings.TrimSpace(ref)) != nil {
				return fmt.Errorf("native agent control source contains invalid credential_ref")
			}
			if n, revisionOK := nativeControlInteger(credentialRevision); !revisionOK || n < 1 {
				return fmt.Errorf("native agent control source contains invalid credential_revision")
			}
		}
	case "oci_image":
		location, ok := source["location"].(string)
		parts := strings.Split(strings.TrimSpace(location), "@sha256:")
		if !ok || len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || !nativeControlHexPin(parts[1], 64) {
			return fmt.Errorf("native agent control OCI source requires an exact image digest")
		}
	case "uploaded_artifact":
		delete(allowed, "location")
		allowed["artifact_id"] = true
		artifactID, ok := source["artifact_id"].(string)
		if !ok || uuid.Validate(strings.TrimSpace(artifactID)) != nil {
			return fmt.Errorf("native agent control uploaded source requires an artifact_id")
		}
	default:
		return fmt.Errorf("native agent control request contains unsupported source kind %q", kind)
	}
	for key := range source {
		if !allowed[key] {
			return fmt.Errorf("native agent control source contains unknown field %q", key)
		}
	}
	return nil
}

func nativeControlHexPin(value string, size int) bool {
	value = strings.TrimSpace(value)
	if len(value) != size || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func nativeControlNameToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || !((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= '0' && value[0] <= '9')) {
		return false
	}
	previousSeparator := false
	for _, r := range value {
		letterOrDigit := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		separator := r == '.' || r == '_' || r == '-'
		if !letterOrDigit && !separator || separator && previousSeparator {
			return false
		}
		previousSeparator = separator
	}
	return !previousSeparator
}

func nativeControlInputSecretKey(key string) bool {
	switch normalizedControlKey(key) {
	case "secret", "secretvalue", "secretbytes", "password", "passphrase", "token", "accesstoken", "refreshtoken", "sessiontoken", "authorization", "proxyauthorization", "cookie", "setcookie", "apikey", "privatekey", "secretaccesskey":
		return true
	default:
		return false
	}
}

func nativeControlSensitiveKey(key string) bool {
	switch normalizedControlKey(key) {
	case "ownerid", "selectedcommand", "commandsteps", "imageuri", "desiredplan", "labels", "typedsecretgrants", "uri",
		"secret", "secretvalue", "secretbytes", "password", "passphrase", "token", "accesstoken", "refreshtoken", "sessiontoken", "authorization", "proxyauthorization", "cookie", "setcookie", "apikey", "privatekey", "secretaccesskey", "accesskeyid":
		return true
	default:
		return false
	}
}

func nativeControlSensitiveValue(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	for _, marker := range []string{
		"bearer ", "basic ", "-----begin private key", "-----begin rsa private key",
		"access_token=", "refresh_token=", "cookie:", "set-cookie:", "x-amz-signature=", "x-amz-credential=",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func normalizedControlKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.NewReplacer("_", "", "-", "", ".", "").Replace(key)
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

func integerSchema() map[string]any { return map[string]any{"type": "integer"} }

func nonNegativeIntegerSchema() map[string]any {
	return map[string]any{"type": "integer", "minimum": 0}
}

func positiveIntegerSchema() map[string]any {
	return map[string]any{"type": "integer", "minimum": 1}
}

func uuidSchema() map[string]any { return map[string]any{"type": "string", "format": "uuid"} }

func nameTokenSchema() map[string]any {
	return map[string]any{"type": "string", "pattern": `^[a-z0-9]+(?:[._-][a-z0-9]+)*$`, "maxLength": 128}
}

func enumStringSchema(values ...string) map[string]any {
	enum := make([]any, 0, len(values))
	for _, value := range values {
		enum = append(enum, value)
	}
	return map[string]any{"type": "string", "enum": enum}
}

func openObjectSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": true}
}

func immutableSourceSchema() map[string]any {
	base := func(kind string, properties map[string]any, required ...string) map[string]any {
		properties["kind"] = map[string]any{"type": "string", "const": kind}
		properties["immutable"] = map[string]any{"type": "boolean", "const": true}
		return strictObjectSchema(properties, append([]string{"kind", "immutable"}, required...)...)
	}
	return map[string]any{"oneOf": []any{
		base("git_https", map[string]any{"location": stringSchema(), "commit": stringSchema(), "credential_ref": uuidSchema(), "credential_revision": positiveIntegerSchema()}, "location", "commit"),
		base("oci_image", map[string]any{"location": stringSchema()}, "location"),
		base("uploaded_artifact", map[string]any{"artifact_id": uuidSchema()}, "artifact_id"),
	}}
}

func strictObjectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
