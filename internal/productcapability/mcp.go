package productcapability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
)

// ProductInvoker is the domain-owned ProductCore/Matrix boundary. The
// capability layer never reaches product tables directly; the p2p service
// supplies this callback from its existing MCP/domain modules.
type ProductInvoker func(context.Context, string, map[string]any) (any, error)

// RegistryOptions controls optional write descriptors whose implementation
// depends on a durable Matrix prepared-event port. Read-only capabilities are
// always registered; unsafe write fallbacks are omitted when false.
type RegistryOptions struct {
	MatrixMutationReady bool
}

// NewRegistryWithInvoker builds the production capability catalog on top of
// the existing ProductCore and Matrix implementation.  It preserves the
// historical no-error constructor for tests and callers that do not need
// startup diagnostics; production startup should use the Checked variant.
func NewRegistryWithInvoker(invoker ProductInvoker) *Registry {
	registry, err := NewRegistryWithInvokerChecked(invoker)
	if err != nil {
		// An invalid descriptor must never become a partially advertised
		// capability. Returning an empty registry is fail-closed for legacy
		// callers; production wiring returns the error from the Checked API.
		return NewRegistry()
	}
	return registry
}

// NewRegistryWithInvokerChecked is the production constructor. It validates
// every descriptor (including canonical JSON schemas and unique operation
// IDs) before returning a registry, so a catalog registration error prevents
// the Product Capability server from starting with a partial surface.
func NewRegistryWithInvokerChecked(invoker ProductInvoker) (*Registry, error) {
	return NewRegistryWithInvokerAndOptionsChecked(invoker, RegistryOptions{MatrixMutationReady: true})
}

func NewRegistryWithInvokerAndOptionsChecked(invoker ProductInvoker, options RegistryOptions) (*Registry, error) {
	registry := NewRegistry()
	if invoker == nil {
		return registry, nil
	}
	var registrationErr error
	register := func(id, name, description string, operations []operationBinding) {
		if registrationErr != nil {
			return
		}
		provider := &Provider{Descriptor: &capv1.CapabilityDescriptor{
			CapabilityId:    id,
			SemanticVersion: "1.0.0",
			ProtocolVersion: 1,
			DisplayName:     name,
			Description:     description,
			Readiness:       true,
		}}
		for _, binding := range operations {
			binding := binding
			risk := binding.risk
			if risk == capv1.RiskLevel_RISK_LEVEL_UNSPECIFIED {
				risk = capv1.RiskLevel_RISK_LEVEL_SAFE
			}
			inputDigest := sha256.Sum256([]byte(binding.schema))
			resultSchema := `{"additionalProperties":true,"type":"object"}`
			resultDigest := sha256.Sum256([]byte(resultSchema))
			provider.Descriptor.Operations = append(provider.Descriptor.Operations, &capv1.OperationDescriptor{
				OperationId:        binding.operation,
				DisplayName:        binding.displayName,
				Description:        binding.description,
				OperationType:      binding.operationType,
				Audience:           []capv1.Audience{capv1.Audience_AUDIENCE_NATIVE_AGENT},
				RiskLevel:          risk,
				RequiredScopes:     []string{binding.scope},
				InputSchemaJson:    binding.schema,
				InputSchemaDigest:  inputDigest[:],
				ResultSchemaJson:   resultSchema,
				ResultSchemaDigest: resultDigest[:],
				// The operation ledger is durable and has no expiry worker in the
				// fresh deployment.  Advertise the protocol's explicit "0 =
				// permanent" value instead of promising a 24h window that is not
				// actually enforced.
				IdempotencyWindowSeconds: 0,
				MaxRequestSizeBytes:      1 << 20,
				TimeoutClass:             "medium",
			})
		}
		provider.Handler = func(ctx context.Context, input []byte) ([]byte, error) {
			var request struct {
				Operation string          `json:"operation"`
				Input     json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(input, &request); err != nil {
				return nil, fmt.Errorf("invalid capability input: %w", err)
			}
			binding, ok := findBinding(operations, request.Operation)
			if !ok {
				return nil, fmt.Errorf("operation %q is not registered for %s", request.Operation, id)
			}
			params := map[string]any{}
			if len(request.Input) > 0 && string(request.Input) != "null" {
				if err := json.Unmarshal(request.Input, &params); err != nil {
					return nil, fmt.Errorf("invalid operation input: %w", err)
				}
			}
			// The gRPC consumer validates this before delegation. Keep the same
			// check here as a defense for direct internal Provider invocation.
			if err := rejectForgedIdentity(params); err != nil {
				return nil, err
			}
			value, err := invoker(ctx, binding.action, params)
			if err != nil {
				return nil, err
			}
			return json.Marshal(value)
		}
		if err := validateCapabilityDescriptor(provider.Descriptor); err != nil {
			registrationErr = fmt.Errorf("validate capability %s: %w", id, err)
			return
		}
		if err := registry.Register(provider); err != nil {
			registrationErr = err
		}
	}

	register("product.contacts.v1", "Contacts", "Owner-scoped contacts backed by ProductCore.", []operationBinding{
		{operation: "list", action: dirextalkmcp.ActionContactsList, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:contacts:read", displayName: "List contacts", schema: objectSchema(`"query":{"type":"string"},"limit":{"type":"number"}`)},
		{operation: "search", action: dirextalkmcp.ActionContactsSearch, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:contacts:read", displayName: "Search contacts", schema: objectSchema(`"query":{"type":"string"},"limit":{"type":"number"}`)},
	})
	register("product.rooms.v1", "Rooms", "Joined Matrix/ProductCore rooms visible to the owner.", []operationBinding{
		{operation: "list", action: dirextalkmcp.ActionRoomsSearch, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:rooms:read", displayName: "List rooms", schema: objectSchema(`"query":{"type":"string"},"type":{"type":"string","enum":["all","contact","group","channel"]},"limit":{"type":"number"}`)},
		{operation: "search", action: dirextalkmcp.ActionRoomsSearch, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:rooms:read", displayName: "Search rooms", schema: objectSchema(`"query":{"type":"string"},"type":{"type":"string","enum":["all","contact","group","channel"]},"limit":{"type":"number"}`)},
	})
	messagesOperations := []operationBinding{
		{operation: "list", action: dirextalkmcp.ActionMessagesList, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:messages:read", displayName: "List messages", schema: objectSchema(`"room_id":{"type":"string"},"from_time":{"type":"string"},"to_time":{"type":"string"},"cursor":{"type":"string"},"limit":{"type":"number"}`, "room_id")},
	}
	if options.MatrixMutationReady {
		messagesOperations = append(messagesOperations, operationBinding{operation: "send", action: dirextalkmcp.ActionMessagesSend, operationType: capv1.OperationType_OPERATION_TYPE_MUTATION, scope: "product:messages:write", displayName: "Send message", risk: capv1.RiskLevel_RISK_LEVEL_LOW, schema: objectSchema(`"room_id":{"type":"string"},"msg":{"type":"string"},"agent_gateway":{"type":"boolean"}`, "room_id", "msg")})
	}
	register("product.messages.v1", "Messages", "Matrix timeline reads and owner-authorized sends.", messagesOperations)
	register("product.members.v1", "Room members", "Joined Matrix room members through the ProductCore member projection.", []operationBinding{
		{operation: "list", action: dirextalkmcp.ActionRoomMembersList, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:members:read", displayName: "List members", schema: objectSchema(`"room_id":{"type":"string"},"channel_id":{"type":"string"},"status":{"type":"string"},"role":{"type":"string"},"limit":{"type":"number"}`)},
		{operation: "get", action: dirextalkmcp.ActionRoomMembersList, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:members:read", displayName: "Get member", schema: objectSchema(`"room_id":{"type":"string"},"user_id":{"type":"string"}`, "room_id", "user_id")},
	})
	register("product.channels.v1", "Channels", "Unified channel posts backed by ProductCore and Matrix.", []operationBinding{
		{operation: "list", action: dirextalkmcp.ActionRoomsSearch, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:channels:read", displayName: "List channels", schema: objectSchema(`"query":{"type":"string"},"type":{"type":"string","enum":["all","contact","group","channel"]},"limit":{"type":"number"}`)},
		{operation: "get_posts", action: dirextalkmcp.ActionChannelPostsList, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:channels:read", displayName: "List channel posts", schema: objectSchema(`"room_id":{"type":"string"},"from_time":{"type":"string"},"to_time":{"type":"string"},"cursor":{"type":"string"},"limit":{"type":"number"}`, "room_id")},
	})
	commentOperations := []operationBinding{
		{operation: "list", action: dirextalkmcp.ActionChannelCommentsList, operationType: capv1.OperationType_OPERATION_TYPE_READ, scope: "product:channels:read", displayName: "List comments", schema: objectSchema(`"post_id":{"type":"string"},"from_time":{"type":"string"},"to_time":{"type":"string"},"cursor":{"type":"string"},"limit":{"type":"number"}`, "post_id")},
	}
	if options.MatrixMutationReady {
		commentOperations = append(commentOperations, operationBinding{operation: "create", action: dirextalkmcp.ActionChannelCommentsCreate, operationType: capv1.OperationType_OPERATION_TYPE_MUTATION, scope: "product:channels:write", displayName: "Create comment", risk: capv1.RiskLevel_RISK_LEVEL_LOW, schema: objectSchema(`"post_id":{"type":"string"},"msg":{"type":"string"}`, "post_id", "msg")})
	}
	register("product.channel_comments.v1", "Channel comments", "Channel comments backed by ProductCore Matrix transport.", commentOperations)
	// Generic SPI bridge: every registered Dirextalk MCP tool gets a stable
	// capability descriptor and fixed action binding. Adding a tool therefore
	// changes the catalog, not the public HTTP/gRPC surface, while callers still
	// cannot provide an arbitrary ProductCore action string.
	for _, tool := range dirextalkmcp.Tools() {
		if registrationErr != nil {
			break
		}
		tool := tool
		if !options.MatrixMutationReady && (tool.Action == dirextalkmcp.ActionMessagesSend || tool.Action == dirextalkmcp.ActionChannelCommentsCreate) {
			continue
		}
		capabilityID := "product.spi." + capabilitySlug(tool.Action) + ".v1"
		scope := "product:mcp:read"
		operationType := capv1.OperationType_OPERATION_TYPE_READ
		risk := capv1.RiskLevel_RISK_LEVEL_SAFE
		// Only the explicit read effect receives read scope; unknown effects stay
		// fail-closed as mutations in the private capability catalog.
		if tool.Effect != dirextalkmcp.ToolEffectRead {
			scope = "product:mcp:write"
			operationType = capv1.OperationType_OPERATION_TYPE_MUTATION
			risk = capv1.RiskLevel_RISK_LEVEL_LOW
		}
		schema, _ := json.Marshal(tool.InputSchema)
		canonicalInput, canonicalErr := capv1.CanonicalizeJSON(schema)
		if canonicalErr != nil {
			registrationErr = fmt.Errorf("validate MCP tool %s input schema: %w", tool.Action, canonicalErr)
			break
		}
		schema = canonicalInput
		inputDigest := sha256.Sum256(schema)
		resultSchema := `{"additionalProperties":true,"type":"object"}`
		resultDigest := sha256.Sum256([]byte(resultSchema))
		provider := &Provider{Descriptor: &capv1.CapabilityDescriptor{
			CapabilityId: capabilityID, SemanticVersion: "1.0.0", ProtocolVersion: 1,
			DisplayName: tool.Name, Description: tool.Description, Readiness: true,
			Operations: []*capv1.OperationDescriptor{{OperationId: "invoke", DisplayName: tool.Name, OperationType: operationType, Audience: []capv1.Audience{capv1.Audience_AUDIENCE_NATIVE_AGENT}, RiskLevel: risk, RequiredScopes: []string{scope}, InputSchemaJson: string(schema), InputSchemaDigest: inputDigest[:], ResultSchemaJson: resultSchema, ResultSchemaDigest: resultDigest[:], IdempotencyWindowSeconds: 0, MaxRequestSizeBytes: 1 << 20, TimeoutClass: "medium"}},
		}, Handler: func(ctx context.Context, input []byte) ([]byte, error) {
			var request struct {
				Operation string          `json:"operation"`
				Input     json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(input, &request); err != nil {
				return nil, err
			}
			if request.Operation != "invoke" {
				return nil, fmt.Errorf("operation %q is not registered for %s", request.Operation, capabilityID)
			}
			params := map[string]any{}
			if len(request.Input) > 0 && string(request.Input) != "null" {
				if err := json.Unmarshal(request.Input, &params); err != nil {
					return nil, err
				}
			}
			// Keep the provider boundary fail-closed even when an internal caller
			// invokes it without going through the gRPC input validator.
			if err := rejectForgedIdentity(params); err != nil {
				return nil, err
			}
			value, err := invoker(ctx, tool.Action, params)
			if err != nil {
				return nil, err
			}
			return json.Marshal(value)
		}}
		if err := validateCapabilityDescriptor(provider.Descriptor); err != nil {
			registrationErr = fmt.Errorf("validate capability %s: %w", capabilityID, err)
			break
		}
		if err := registry.Register(provider); err != nil {
			registrationErr = err
			break
		}
	}
	if registrationErr != nil {
		return nil, registrationErr
	}
	return registry, nil
}

type operationBinding struct {
	operation, action, scope, displayName, description string
	schema                                             string
	risk                                               capv1.RiskLevel
	operationType                                      capv1.OperationType
}

func objectSchema(properties string, required ...string) string {
	var raw string
	if strings.TrimSpace(properties) == "" {
		raw = `{"type":"object"}`
	} else {
		raw = `{"type":"object","properties":{` + properties + `}}`
	}
	if len(required) > 0 {
		var schema map[string]any
		if err := json.Unmarshal([]byte(raw), &schema); err != nil {
			return ""
		}
		schema["required"] = append([]string(nil), required...)
		encoded, err := json.Marshal(schema)
		if err != nil {
			return ""
		}
		raw = string(encoded)
	}
	canonical, err := capv1.CanonicalizeJSON([]byte(raw))
	if err != nil {
		return ""
	}
	return string(canonical)
}

func validateCapabilityDescriptor(descriptor *capv1.CapabilityDescriptor) error {
	if descriptor == nil || strings.TrimSpace(descriptor.GetCapabilityId()) == "" || descriptor.GetProtocolVersion() <= 0 || strings.TrimSpace(descriptor.GetSemanticVersion()) == "" {
		return fmt.Errorf("capability descriptor identity is incomplete")
	}
	seen := make(map[string]struct{}, len(descriptor.GetOperations()))
	for _, operation := range descriptor.GetOperations() {
		if operation == nil || strings.TrimSpace(operation.GetOperationId()) == "" {
			return fmt.Errorf("operation descriptor identity is incomplete")
		}
		if _, exists := seen[operation.GetOperationId()]; exists {
			return fmt.Errorf("duplicate operation %q", operation.GetOperationId())
		}
		seen[operation.GetOperationId()] = struct{}{}
		schema := []byte(strings.TrimSpace(operation.GetInputSchemaJson()))
		if len(schema) == 0 || !json.Valid(schema) {
			return fmt.Errorf("operation %q input schema is invalid", operation.GetOperationId())
		}
		canonical, err := capv1.CanonicalizeJSON(schema)
		if err != nil || string(canonical) != string(schema) {
			return fmt.Errorf("operation %q input schema must be canonical JSON", operation.GetOperationId())
		}
		if len(operation.GetRequiredScopes()) == 0 {
			return fmt.Errorf("operation %q required scopes are missing", operation.GetOperationId())
		}
		if len(operation.GetAudience()) == 0 {
			return fmt.Errorf("operation %q audience is missing", operation.GetOperationId())
		}
		inputDigest := sha256.Sum256(schema)
		if len(operation.GetInputSchemaDigest()) != sha256.Size || !bytes.Equal(operation.GetInputSchemaDigest(), inputDigest[:]) {
			return fmt.Errorf("operation %q input schema digest is missing or mismatched", operation.GetOperationId())
		}
		resultSchema := []byte(strings.TrimSpace(operation.GetResultSchemaJson()))
		if len(resultSchema) == 0 || !json.Valid(resultSchema) {
			return fmt.Errorf("operation %q result schema is invalid", operation.GetOperationId())
		}
		canonicalResult, err := capv1.CanonicalizeJSON(resultSchema)
		if err != nil || !bytes.Equal(canonicalResult, resultSchema) {
			return fmt.Errorf("operation %q result schema must be canonical JSON", operation.GetOperationId())
		}
		resultDigest := sha256.Sum256(resultSchema)
		if len(operation.GetResultSchemaDigest()) != sha256.Size || !bytes.Equal(operation.GetResultSchemaDigest(), resultDigest[:]) {
			return fmt.Errorf("operation %q result schema digest is missing or mismatched", operation.GetOperationId())
		}
	}
	return nil
}

func findBinding(bindings []operationBinding, operation string) (operationBinding, bool) {
	for _, binding := range bindings {
		if binding.operation == strings.TrimSpace(operation) {
			return binding, true
		}
	}
	return operationBinding{}, false
}

func capabilitySlug(action string) string {
	action = strings.TrimSpace(action)
	action = strings.TrimPrefix(action, "mcp.")
	action = strings.NewReplacer(".", "_", "-", "_").Replace(action)
	if action == "" {
		return "tool"
	}
	return action
}
