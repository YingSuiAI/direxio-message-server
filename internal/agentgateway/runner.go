package agentgateway

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrUnsupportedAction is returned before any capability RPC is attempted
// when a ProductCore action has no explicit Agent capability mapping. Keeping a
// sentinel lets the HTTP/WS facade expose a stable not-implemented response
// instead of misclassifying a contract gap as a transient gateway failure.
var ErrUnsupportedAction = errors.New("native agent action is unsupported")

// ObservationInterruptedError means only the live Watch attachment ended.
// The durable turn remains authoritative and can be observed again from
// Sequence with the same immutable turn identity.
type ObservationInterruptedError struct {
	IdempotencyKey string
	ConversationID string
	TurnID         string
	Revision       int64
	Sequence       int64
	Cause          error
}

func (e *ObservationInterruptedError) Error() string {
	return "native agent observation was interrupted; execution continues"
}

func (e *ObservationInterruptedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// RunnerConfig contains only server-derived identity data. No caller/model
// parameter is allowed to override OwnerID or AccountGeneration.
type RunnerConfig struct {
	OwnerID           func() string
	AccountGeneration func() int64
	GrantCodec        capv1.GrantCodec
	GrantPrivateKey   []byte
	GrantScopes       func(action string) []string
}

// Runner adapts the capability operation protocol to the Native Agent runner
// contract used by ProductCore and the HTTP/SSE facade.
// Native Agent execution happens in dirextalk-agent; message-server only
// admits, watches and translates the operation stream.
type Runner struct {
	client *Client
	config RunnerConfig
}

func NewRunner(client *Client, config RunnerConfig) *Runner {
	return &Runner{client: client, config: config}
}

func (r *Runner) Apply(ctx context.Context, action string) error {
	_, err := r.invokeOperation(ctx, action, map[string]any{})
	return err
}

func (r *Runner) Invoke(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	var authority actionResultAuthority
	result, replayed, err := r.invokeOperationWithReplay(ctx, action, params, &authority)
	if err != nil {
		return nil, err
	}
	var output map[string]any
	if len(result) == 0 {
		output = map[string]any{}
		if actionPublishesReplay(strings.TrimSpace(action)) {
			output["replayed"] = replayed
		}
		return adaptActionResultForRequestWithAuthority(strings.TrimSpace(action), params, output, authority)
	}
	if err := json.Unmarshal(result, &output); err != nil {
		return nil, fmt.Errorf("agent operation returned invalid JSON: %w", err)
	}
	if actionPublishesReplay(strings.TrimSpace(action)) {
		// Replay is StartOperation transport metadata, not part of the durable
		// Core business receipt. Overlay it only on mutations whose
		// public schema explicitly defines the field.
		output["replayed"] = replayed
	}
	return adaptActionResultForRequestWithAuthority(strings.TrimSpace(action), params, output, authority)
}

func (r *Runner) Stream(ctx context.Context, action string, params map[string]any, emit func(agentstream.Event) error) error {
	if emit == nil {
		return fmt.Errorf("native agent stream callback is required")
	}
	if r == nil || r.client == nil {
		return fmt.Errorf("agent gateway is not configured")
	}
	if err := ValidateActionRequest(action, params); err != nil {
		return err
	}
	conversationID, _ := params["conversation_id"].(string)
	operationID, requestJSON, permission, err := r.prepare(action, params)
	if err != nil {
		return err
	}
	authority := actionResultAuthority{ownerID: permission.GetAuthenticatedOwnerId(), accountGeneration: permission.GetAccountGeneration()}
	r.syncPeerGeneration(permission.AccountGeneration)
	callCtx := r.client.createCallContext(operationID)
	binding, ok := actionBindingFor(action)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnsupportedAction, action)
	}
	requestJSON, err = transformCapabilityRequest(action, operationID, params, binding)
	if err != nil {
		return err
	}
	digest, operation, err := r.requestDigest(ctx, callCtx, operationID, action, binding, requestJSON, permission)
	if err != nil {
		return err
	}
	if operation.GetOperationType() == capv1.OperationType_OPERATION_TYPE_READ {
		return fmt.Errorf("native agent stream action %q is read-only and cannot use a stream", action)
	}
	response, err := r.client.StartOperation(ctx, operationID, binding.capabilityID, binding.operation, requestJSON, digest, 0, permission, callCtx)
	if err != nil {
		return err
	}
	if err := operationResponseError(response); err != nil {
		return err
	}
	if err := operationStateError(response.GetState()); err != nil {
		return err
	}
	return r.watchDurableChat(ctx, operationID, conversationID, afterSequence(params), permission, callCtx, authority, emit)
}

// WatchDurableChat attaches to an already admitted chat operation without
// replaying its mutation. The operation id is the public turn/idempotency id;
// Agent remains the durable event source and applies the owner/generation
// fence to this fresh read-only control grant.
func (r *Runner) WatchDurableChat(ctx context.Context, operationID, conversationID string, afterSeq int64, emit func(agentstream.Event) error) error {
	if emit == nil {
		return fmt.Errorf("native agent stream callback is required")
	}
	if r == nil || r.client == nil {
		return fmt.Errorf("agent gateway is not configured")
	}
	if !canonicalTurnUUID(operationID) || !canonicalTurnUUID(conversationID) || afterSeq < 0 {
		return fmt.Errorf("%w: durable chat watch identity is invalid", ErrInvalidActionRequest)
	}
	owner := ""
	if r.config.OwnerID != nil {
		owner = strings.TrimSpace(r.config.OwnerID())
	}
	generation := int64(0)
	if r.config.AccountGeneration != nil {
		generation = r.config.AccountGeneration()
	}
	if owner == "" || generation <= 0 {
		return fmt.Errorf("authenticated owner is unavailable")
	}
	permission := &capv1.PermissionContext{AuthenticatedOwnerId: owner, AccountGeneration: generation}
	r.syncPeerGeneration(generation)
	callCtx := r.client.createCallContext(operationID)
	authority := actionResultAuthority{ownerID: owner, accountGeneration: generation}
	return r.watchDurableChat(ctx, operationID, conversationID, afterSeq, permission, callCtx, authority, emit)
}

func (r *Runner) watchDurableChat(
	ctx context.Context,
	operationID, conversationID string,
	afterSeq int64,
	permission *capv1.PermissionContext,
	callCtx *capv1.CallContext,
	authority actionResultAuthority,
	emit func(agentstream.Event) error,
) error {
	watchCallCtx := r.client.refreshCallContext(callCtx)
	controlPermission, err := r.operationControlPermission(watchCallCtx, operationID, "watch", permission)
	if err != nil {
		return err
	}
	stream, err := r.client.WatchOperation(ctx, operationID, afterSeq, controlPermission, watchCallCtx)
	if err != nil {
		return err
	}
	defer func() {
		if closer, ok := stream.(interface{ Close() }); ok {
			closer.Close()
		}
	}()
	var observed durableChatObservation
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			if turn := r.lookupDurableChatTurn(ctx, operationID, conversationID); turn != nil {
				if terminal := terminalChatEventFromTurn(turn, observed.sequence); terminal != nil {
					return emit(*terminal)
				}
				observed.captureTurn(turn)
			}
			if observed.valid() && observationAttachmentLoss(recvErr) {
				return observed.interrupted(recvErr)
			}
			return recvErr
		}
		if event == nil {
			continue
		}
		if result, ok := event.Event.(*capv1.WatchOperationEvent_Result); ok && result.Result != nil {
			turn := r.lookupDurableChatTurn(ctx, operationID, conversationID)
			nativeEvents, projectionErr := nativeEventsFromCompletedResult(result.Result.ResultJson, event.Sequence, operationID, authority, turn)
			if projectionErr != nil {
				return projectionErr
			}
			for _, nativeEvent := range nativeEvents {
				if err := emit(nativeEvent); err != nil {
					return err
				}
			}
			return nil
		}
		nativeEvent, terminal, terminalErr := nativeEventFromProto(event, authority)
		if nativeEvent == nil && terminal {
			if _, isCapabilityError := event.Event.(*capv1.WatchOperationEvent_Error); isCapabilityError {
				turn := r.lookupDurableChatTurn(ctx, operationID, conversationID)
				nativeEvent = terminalChatEventFromTurn(turn, event.Sequence)
				if nativeEvent == nil {
					observed.captureTurn(turn)
					if observed.valid() {
						return observed.interrupted(terminalErr)
					}
				}
			}
		}
		if nativeEvent != nil {
			if err := emit(*nativeEvent); err != nil {
				return err
			}
			observed.captureEvent(nativeEvent)
		}
		if terminal {
			if terminalErr != nil {
				return terminalErr
			}
			return nil
		}
	}
}

type durableChatObservation struct {
	idempotencyKey string
	conversationID string
	turnID         string
	revision       int64
	sequence       int64
}

func (o *durableChatObservation) captureEvent(event *agentstream.Event) {
	if o == nil || event == nil || event.Data == nil {
		return
	}
	turnID, _ := event.Data["turn_id"].(string)
	idempotencyKey, _ := event.Data["idempotency_key"].(string)
	conversationID, _ := event.Data["conversation_id"].(string)
	revision, revisionOK := turnInt64(event.Data["revision"])
	if !canonicalTurnUUID(turnID) || !canonicalTurnUUID(idempotencyKey) || !canonicalTurnUUID(conversationID) || !revisionOK || revision <= 0 {
		return
	}
	o.idempotencyKey, o.conversationID, o.turnID = idempotencyKey, conversationID, turnID
	o.revision = revision
	if event.Seq > o.sequence {
		o.sequence = event.Seq
	}
}

func (o *durableChatObservation) captureTurn(turn map[string]any) {
	if o == nil || !validCanonicalTurn(turn) {
		return
	}
	revision, revisionOK := turnInt64(turn["revision"])
	sequence, sequenceOK := turnInt64(turn["last_sequence"])
	if !revisionOK || !sequenceOK {
		return
	}
	o.idempotencyKey = turn["idempotency_key"].(string)
	o.conversationID = turn["conversation_id"].(string)
	o.turnID = turn["turn_id"].(string)
	o.revision = revision
	if sequence > o.sequence {
		o.sequence = sequence
	}
}

func (o durableChatObservation) valid() bool {
	return canonicalTurnUUID(o.idempotencyKey) && canonicalTurnUUID(o.conversationID) &&
		canonicalTurnUUID(o.turnID) && o.revision > 0 && o.sequence >= 0
}

func (o durableChatObservation) interrupted(cause error) error {
	return &ObservationInterruptedError{
		IdempotencyKey: o.idempotencyKey, ConversationID: o.conversationID,
		TurnID: o.turnID, Revision: o.revision, Sequence: o.sequence, Cause: cause,
	}
}

func observationAttachmentLoss(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrWatchIdleTimeout) || errors.Is(err, io.EOF) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}

func (r *Runner) invokeOperation(ctx context.Context, action string, params map[string]any) ([]byte, error) {
	result, _, err := r.invokeOperationWithReplay(ctx, action, params, nil)
	return result, err
}

func (r *Runner) invokeOperationWithReplay(ctx context.Context, action string, params map[string]any, authorityOut *actionResultAuthority) ([]byte, bool, error) {
	if r == nil || r.client == nil {
		return nil, false, fmt.Errorf("agent gateway is not configured")
	}
	if err := ValidateActionRequest(action, params); err != nil {
		return nil, false, err
	}
	operationID, requestJSON, permission, err := r.prepare(action, params)
	if err != nil {
		return nil, false, err
	}
	if authorityOut != nil {
		*authorityOut = actionResultAuthority{
			ownerID:           permission.GetAuthenticatedOwnerId(),
			accountGeneration: permission.GetAccountGeneration(),
		}
	}
	r.syncPeerGeneration(permission.AccountGeneration)
	callCtx := r.client.createCallContext(operationID)
	binding, ok := actionBindingFor(action)
	if !ok {
		return nil, false, fmt.Errorf("%w: %q", ErrUnsupportedAction, action)
	}
	requestJSON, err = transformCapabilityRequest(action, operationID, params, binding)
	if err != nil {
		return nil, false, err
	}
	digest, operation, err := r.requestDigest(ctx, callCtx, operationID, action, binding, requestJSON, permission)
	if err != nil {
		return nil, false, err
	}
	if operation.GetOperationType() == capv1.OperationType_OPERATION_TYPE_READ {
		response, queryErr := r.client.Query(ctx, operationID, binding.capabilityID, binding.operation, requestJSON, permission, callCtx)
		if queryErr != nil {
			return nil, false, queryErr
		}
		if response == nil {
			return nil, false, fmt.Errorf("agent returned an empty query response")
		}
		if response.Error != nil {
			return nil, false, capabilityErrorFromProto(response.Error)
		}
		return response.ResultJson, false, nil
	}
	response, err := r.client.StartOperation(ctx, operationID, binding.capabilityID, binding.operation, requestJSON, digest, 0, permission, callCtx)
	if err != nil {
		return nil, false, err
	}
	if err := operationResponseError(response); err != nil {
		return nil, false, err
	}
	if err := operationStateError(response.GetState()); err != nil {
		return nil, false, err
	}
	watchCallCtx := r.client.refreshCallContext(callCtx)
	controlPermission, err := r.operationControlPermission(watchCallCtx, operationID, "watch", permission)
	if err != nil {
		return nil, false, err
	}
	stream, err := r.client.WatchOperation(ctx, operationID, 0, controlPermission, watchCallCtx)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if closer, ok := stream.(interface{ Close() }); ok {
			closer.Close()
		}
	}()
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			return nil, false, recvErr
		}
		if event == nil {
			continue
		}
		switch value := event.Event.(type) {
		case *capv1.WatchOperationEvent_Result:
			return value.Result.ResultJson, response.GetReplayed(), nil
		case *capv1.WatchOperationEvent_Error:
			if value.Error != nil && value.Error.Error != nil {
				return nil, false, capabilityErrorFromProto(value.Error.Error)
			}
			return nil, false, capabilityError(capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED)
		case *capv1.WatchOperationEvent_Cancelled:
			return nil, false, capabilityError(capv1.ErrorCode_ERROR_CODE_CONFLICT)
		}
	}
}

func (r *Runner) prepare(action string, params map[string]any) (string, []byte, *capv1.PermissionContext, error) {
	operationID := r.operationIDFor(params)
	if params == nil {
		params = map[string]any{}
	}
	requestJSON, err := json.Marshal(params)
	if err != nil {
		return "", nil, nil, fmt.Errorf("marshal native agent request: %w", err)
	}
	requestJSON, err = capv1.CanonicalizeJSON(requestJSON)
	if err != nil {
		return "", nil, nil, fmt.Errorf("canonicalize native agent request: %w", err)
	}
	owner := ""
	if r.config.OwnerID != nil {
		owner = strings.TrimSpace(r.config.OwnerID())
	}
	if owner == "" {
		return "", nil, nil, fmt.Errorf("authenticated owner is unavailable")
	}
	generation := int64(0)
	if r.config.AccountGeneration != nil {
		generation = r.config.AccountGeneration()
	}
	var scopes []string
	if r.config.GrantScopes != nil {
		scopes = r.config.GrantScopes(action)
	}
	permission := &capv1.PermissionContext{AuthenticatedOwnerId: owner, GrantedScopes: append([]string(nil), scopes...), AccountGeneration: generation}
	return operationID, requestJSON, permission, nil
}

func (r *Runner) operationIDFor(params map[string]any) string {
	if params != nil {
		if value, ok := params["operation_id"].(string); ok {
			if parsed, err := uuid.Parse(strings.TrimSpace(value)); err == nil {
				return parsed.String()
			}
		}
		if value, ok := params["idempotency_key"].(string); ok {
			if parsed, err := uuid.Parse(strings.TrimSpace(value)); err == nil {
				return parsed.String()
			}
		}
	}
	return uuid.New().String()
}

func (r *Runner) syncPeerGeneration(generation int64) {
	if r != nil && r.client != nil {
		r.client.SetAccountGeneration(generation)
	}
}

// operationControlPermission issues a fresh, domain-separated grant for an
// operation lifecycle control call.  Root capability grants authorize the
// operation's business request only; Get/Watch/Cancel must never replay that
// grant or rebuild it from the original request.  The short-lived control
// grant is bound to the existing chain, operation id, action, entry route and
// account generation by the shared capability codec.
func (r *Runner) operationControlPermission(callCtx *capv1.CallContext, operationID, action string, base *capv1.PermissionContext) (*capv1.PermissionContext, error) {
	if r == nil || base == nil {
		return nil, fmt.Errorf("permission context is required for operation control")
	}
	if callCtx == nil {
		return nil, fmt.Errorf("capability call context is required for operation control")
	}
	if len(r.config.GrantPrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("capability grant private key is required")
	}
	action = strings.TrimSpace(action)
	if action != "get" && action != "watch" && action != "cancel" && action != "reconcile" {
		return nil, fmt.Errorf("unsupported operation control action %q", action)
	}
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return nil, fmt.Errorf("operation id is required for operation control")
	}
	controlScope := "operation:control:" + action
	grant, err := r.config.GrantCodec.SignOperationControlGrant(capv1.OperationControlGrant{
		ChainID:           callCtx.GetChainId(),
		OwnerID:           base.GetAuthenticatedOwnerId(),
		AccountGeneration: base.GetAccountGeneration(),
		OperationID:       operationID,
		ControlAction:     action,
		ControlScope:      controlScope,
		EntryRoute:        callCtx.GetRoute(),
		EntryHop:          callCtx.GetHop(),
		DeadlineUnixMs:    callCtx.GetDeadlineUnixMs(),
	}, r.config.GrantPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("issue operation control grant: %w", err)
	}
	return &capv1.PermissionContext{
		AuthenticatedOwnerId: base.GetAuthenticatedOwnerId(),
		GrantedScopes:        []string{controlScope},
		CapabilityGrant:      grant,
		AccountGeneration:    base.GetAccountGeneration(),
	}, nil
}

func (r *Runner) requestDigest(ctx context.Context, callCtx *capv1.CallContext, operationID, action string, binding actionBinding, requestJSON []byte, permission *capv1.PermissionContext) ([]byte, *capv1.OperationDescriptor, error) {
	if r == nil || r.client == nil {
		return nil, nil, fmt.Errorf("agent gateway is not configured")
	}
	if callCtx == nil {
		return nil, nil, fmt.Errorf("capability call context is required")
	}
	if permission == nil || len(permission.CapabilityGrant) == 0 {
		if permission == nil {
			return nil, nil, fmt.Errorf("permission context is required for request digest")
		}
	}
	// RootRequestDigest deliberately excludes the opaque grant to avoid a
	// circular dependency. The final operation digest below includes the grant
	// hash and is checked by Agent's exact StartOperation binding.
	catalog, descriptor, operation, err := r.lookupOperation(ctx, callCtx, action, binding)
	if err != nil {
		return nil, nil, err
	}
	schemaDigest := sha256.Sum256([]byte(operation.GetInputSchemaJson()))
	businessInput, err := capv1.ParseBusinessInput(requestJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("parse canonical capability request: %w", err)
	}
	rootDigest, err := capv1.ComputeRootRequestDigest(
		descriptor.GetProtocolVersion(), descriptor.GetCapabilityId(), descriptor.GetSemanticVersion(),
		schemaDigest[:], operation.GetOperationId(), 0, businessInput, nil,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("compute root capability request digest: %w", err)
	}
	if _, _, err := r.prepareGrantWithCatalog(catalog, callCtx, operationID, descriptor, operation, permission, rootDigest); err != nil {
		return nil, nil, err
	}
	grantDigest := sha256.Sum256(permission.GetCapabilityGrant())
	schemaDigest = sha256.Sum256([]byte(operation.GetInputSchemaJson()))
	digest, err := capv1.ComputeRequestDigest(
		descriptor.GetProtocolVersion(), descriptor.GetCapabilityId(), descriptor.GetSemanticVersion(),
		schemaDigest[:], operation.GetOperationId(), 0, businessInput, nil, grantDigest[:],
	)
	if err != nil {
		return nil, nil, fmt.Errorf("compute canonical request digest: %w", err)
	}
	return digest, operation, nil
}

func (r *Runner) lookupOperation(ctx context.Context, callCtx *capv1.CallContext, action string, binding actionBinding) (*capv1.DescribeCapabilitiesResponse, *capv1.CapabilityDescriptor, *capv1.OperationDescriptor, error) {
	requirement, err := catalogRequirementForLookup(action)
	if err != nil {
		return nil, nil, nil, err
	}
	catalog, err := r.client.DescribeCapabilities(ctx, callCtx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("describe agent capabilities: %w", err)
	}
	// A live catalog is the schema source used below for both digest
	// computation and grant signing. Revalidate the exact action requirement
	// on every lookup so a catalog replacement cannot slip an unpinned schema
	// between readiness and execution.
	if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err != nil {
		return nil, nil, nil, fmt.Errorf("validate agent capability catalog: %w", err)
	}
	for _, descriptor := range catalog.GetCapabilities() {
		if descriptor == nil || descriptor.GetCapabilityId() != binding.capabilityID {
			continue
		}
		for _, operation := range descriptor.GetOperations() {
			if operation != nil && operation.GetOperationId() == binding.operation {
				return catalog, descriptor, operation, nil
			}
		}
	}
	// A capability may be intentionally disabled in a deployment profile (for
	// example AWS or execution.v2). Treat an absent advertised binding as a
	// stable contract gap so the ProductCore facade returns 501 instead of a
	// transient 502. Required base bindings are separately enforced by the
	// readiness catalog probe.
	return nil, nil, nil, fmt.Errorf("%w: capability operation %q/%q is not advertised", ErrUnsupportedAction, binding.capabilityID, binding.operation)
}

func (r *Runner) prepareGrantWithCatalog(catalog *capv1.DescribeCapabilitiesResponse, callCtx *capv1.CallContext, operationID string, descriptor *capv1.CapabilityDescriptor, operation *capv1.OperationDescriptor, permission *capv1.PermissionContext, rootRequestDigest []byte) (*capv1.CapabilityDescriptor, *capv1.OperationDescriptor, error) {
	if catalog == nil || descriptor == nil || operation == nil {
		return nil, nil, fmt.Errorf("agent capability operation is unavailable")
	}
	if len(r.config.GrantPrivateKey) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf("capability grant private key is required")
	}
	if len(rootRequestDigest) != sha256.Size {
		return nil, nil, fmt.Errorf("root capability request digest is required")
	}
	schemaDigest := sha256.Sum256([]byte(operation.GetInputSchemaJson()))
	scopes := append([]string(nil), permission.GetGrantedScopes()...)
	for _, required := range operation.GetRequiredScopes() {
		if !containsScope(scopes, required) {
			scopes = append(scopes, required)
		}
	}
	sort.Strings(scopes)
	uniqueScopes := scopes[:0]
	for _, scope := range scopes {
		if len(uniqueScopes) == 0 || uniqueScopes[len(uniqueScopes)-1] != scope {
			uniqueScopes = append(uniqueScopes, scope)
		}
	}
	permission.GrantedScopes = uniqueScopes
	grant, err := r.config.GrantCodec.Sign(capv1.GrantClaims{
		ChainID: callCtx.GetChainId(), RootOperationID: operationID,
		EntryRoute: callCtx.GetRoute(), EntryHop: callCtx.GetHop(),
		OwnerID: permission.GetAuthenticatedOwnerId(), AccountGeneration: permission.GetAccountGeneration(),
		Scopes: uniqueScopes, RootCapabilityID: descriptor.GetCapabilityId(), RootOperation: operation.GetOperationId(),
		RootRequestDigest: rootRequestDigest,
		CatalogDigest:     catalog.GetCatalogDigest(), SchemaDigest: schemaDigest[:], MaxHop: capv1.MaxCallHop, MaxRouteLength: capv1.MaxRouteLength,
	}, r.config.GrantPrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("issue native agent capability grant: %w", err)
	}
	permission.CapabilityGrant = grant
	// Keep the opaque permission envelope self-contained for an authorized
	// Agent→Product delegation. Agent verifies the signed grant independently,
	// then forwards this exact root digest to the Product delegation broker;
	// omitting it makes every nested Product call fail closed.
	permission.RootRequestDigest = append([]byte(nil), rootRequestDigest...)
	return descriptor, operation, nil
}

func containsScope(scopes []string, wanted string) bool {
	for _, scope := range scopes {
		if scope == wanted {
			return true
		}
	}
	return false
}

func operationResponseError(response *capv1.StartOperationResponse) error {
	if response == nil {
		return fmt.Errorf("agent returned an empty operation response")
	}
	if response.Error != nil {
		return capabilityErrorFromProto(response.Error)
	}
	return nil
}

func operationStateError(state capv1.OperationState) error {
	switch state {
	case capv1.OperationState_OPERATION_STATE_UNCERTAIN:
		return capabilityError(capv1.ErrorCode_ERROR_CODE_UNCERTAIN)
	default:
		return nil
	}
}

// actionPublishesReplay identifies public receipts which include StartOperation
// transport metadata. Other mutations may still be idempotently replayable;
// their public projections simply do not expose that transport detail.
func actionPublishesReplay(action string) bool {
	switch strings.TrimSpace(action) {
	case "agent.chat.conversations.create", "agent.chat.conversations.rename", "agent.chat.conversations.delete",
		"agent.knowledge.sources.delete", "agent.knowledge.upload.start":
		return true
	default:
		return false
	}
}

func afterSequence(params map[string]any) int64 {
	if params == nil {
		return 0
	}
	switch value := params["after_seq"].(type) {
	case int:
		if value > 0 {
			return int64(value)
		}
	case int64:
		if value > 0 {
			return value
		}
	case float64:
		if value > 0 {
			return int64(value)
		}
	case json.Number:
		if parsed, err := value.Int64(); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}

// nativeEventsFromResult projects the durable operation's canonical Agent
// ChatResponse into one public done event. Legacy collected-stream envelopes
// are intentionally rejected; progress arrives through WatchOperation events.
func nativeEventsFromResult(resultJSON []byte, resultSequence int64, operationID string, authority actionResultAuthority) ([]agentstream.Event, error) {
	var response map[string]any
	if len(resultJSON) == 0 || json.Unmarshal(resultJSON, &response) != nil || response == nil {
		return nil, fmt.Errorf("%w: canonical chat operation result is missing", ErrInvalidActionResult)
	}
	if err := validateDurableChatResult(response, authority); err != nil {
		return nil, err
	}
	if !canonicalTurnUUID(operationID) || response["idempotency_key"] != operationID {
		return nil, fmt.Errorf("%w: durable chat result idempotency_key does not match its operation", ErrInvalidActionResult)
	}
	sequence := positiveSequence(resultSequence)
	data := chatResult(response)
	data["idempotency_key"] = operationID
	data["conversation_id"] = response["conversation_id"]
	data["turn_id"] = operationID
	data["revision"] = response["revision"]
	data["sequence"] = sequence
	return []agentstream.Event{{Event: "done", Seq: sequence, Data: data}}, nil
}

// nativeEventsFromCompletedResult binds a terminal capability result back to
// the authoritative durable turn. ChatResponse.revision is the conversation
// revision, while SSE revision is the turn revision and may already be much
// higher after tool rounds. The completed turn ledger is therefore the only
// valid source for terminal turn identity and revision.
func nativeEventsFromCompletedResult(resultJSON []byte, resultSequence int64, operationID string, authority actionResultAuthority, turn map[string]any) ([]agentstream.Event, error) {
	if !validCanonicalTurn(turn) || turn["state"] != "completed" || turn["idempotency_key"] != operationID {
		return nil, fmt.Errorf("%w: completed chat turn is unavailable", ErrInvalidActionResult)
	}
	events, err := nativeEventsFromResult(resultJSON, resultSequence, operationID, authority)
	if err != nil {
		return nil, err
	}
	if len(events) != 1 || events[0].Data["conversation_id"] != turn["conversation_id"] {
		return nil, fmt.Errorf("%w: completed chat result does not match its turn", ErrInvalidActionResult)
	}
	revision, ok := turnInt64(turn["revision"])
	if !ok || revision <= 0 {
		return nil, fmt.Errorf("%w: completed chat turn revision is invalid", ErrInvalidActionResult)
	}
	events[0].Data["turn_id"] = turn["turn_id"]
	events[0].Data["revision"] = revision
	return events, nil
}

func nativeEventFromProto(event *capv1.WatchOperationEvent, authority actionResultAuthority) (*agentstream.Event, bool, error) {
	if event == nil {
		return nil, false, nil
	}
	sequence := positiveSequence(event.Sequence)
	base := map[string]any{"sequence": sequence}
	switch value := event.Event.(type) {
	case *capv1.WatchOperationEvent_Accepted:
		// The capability protocol acceptance is transport state and contains no
		// Agent-authored turn identity. Only the durable business accepted
		// progress event may become a ProductCore accepted frame.
		return nil, false, nil
	case *capv1.WatchOperationEvent_Progress:
		var payload map[string]any
		if len(value.Progress.EventJson) > 0 {
			if err := json.Unmarshal(value.Progress.EventJson, &payload); err != nil {
				return nil, true, fmt.Errorf("%w: chat stream progress is not canonical JSON", ErrInvalidActionResult)
			}
		}
		if payload == nil {
			payload = map[string]any{}
		}
		for key, item := range base {
			payload[key] = item
		}
		if err := validateChatStreamEvent(payload, authority); err != nil {
			return nil, true, err
		}
		eventName := strings.ToLower(strings.TrimSpace(fmt.Sprint(payload["kind"])))
		switch eventName {
		case "accepted":
			return &agentstream.Event{Event: "accepted", Seq: sequence, Data: payload}, false, nil
		case "started":
			return &agentstream.Event{Event: "started", Seq: sequence, Data: payload}, false, nil
		case "tool", "tool_call", "tool_result":
			return &agentstream.Event{Event: "tool", Seq: sequence, Data: payload}, false, nil
		case "waiting_confirmation":
			return &agentstream.Event{Event: "waiting_confirmation", Seq: sequence, Data: payload}, false, nil
		case "worker_status":
			return &agentstream.Event{Event: "worker_status", Seq: sequence, Data: payload}, false, nil
		case "error":
			return &agentstream.Event{Event: "error", Seq: sequence, Data: payload}, true, fmt.Errorf("agent operation failed")
		case "done":
			if err := promoteChatResultFields(payload, authority); err != nil {
				return nil, true, err
			}
			return &agentstream.Event{Event: "done", Seq: sequence, Data: payload}, true, nil
		default:
			return &agentstream.Event{Event: "delta", Seq: sequence, Data: payload}, false, nil
		}
	case *capv1.WatchOperationEvent_Result:
		events, err := nativeEventsFromResult(value.Result.ResultJson, event.Sequence, event.OperationId, authority)
		if err != nil {
			return nil, true, err
		}
		return &events[0], true, nil
	case *capv1.WatchOperationEvent_Error:
		capabilityErr := capabilityErrorFromProto(nil)
		if value.Error != nil {
			capabilityErr = capabilityErrorFromProto(value.Error.Error)
		}
		// A capability error is transport terminal state and carries no durable
		// turn identity. Projecting it directly would make the ProductCore
		// boundary reject the event as an invalid business turn. Runner.Stream
		// reconciles an exact failed turn from the authoritative Agent ledger;
		// when that is unavailable, only this sanitized capability error escapes.
		return nil, true, capabilityErr
	case *capv1.WatchOperationEvent_Cancelled:
		return nil, true, capabilityError(capv1.ErrorCode_ERROR_CODE_CONFLICT)
	case *capv1.WatchOperationEvent_Gap:
		base["earliest_sequence"] = value.Gap.EarliestAvailableSequence
		base["latest_sequence"] = value.Gap.LatestAvailableSequence
		return &agentstream.Event{Event: "gap", Seq: sequence, Data: base}, false, nil
	default:
		return nil, false, nil
	}
}

// lookupDurableChatTurn reads the exact turn ledger entry for an observation
// that ended without a complete business terminal event.
func (r *Runner) lookupDurableChatTurn(ctx context.Context, operationID, conversationID string) map[string]any {
	if r == nil || !canonicalTurnUUID(operationID) || !canonicalTurnUUID(conversationID) {
		return nil
	}
	result, err := r.Invoke(ctx, "agent.chat.turns.list", map[string]any{
		"conversation_id": conversationID,
		"limit":           int64(1000),
	})
	if err != nil {
		return nil
	}
	turns, ok := result["turns"].([]any)
	if !ok {
		return nil
	}
	for _, raw := range turns {
		turn, ok := raw.(map[string]any)
		if !ok || turn["idempotency_key"] != operationID || turn["conversation_id"] != conversationID {
			continue
		}
		return turn
	}
	return nil
}

func terminalChatEventFromTurn(turn map[string]any, sequence int64) *agentstream.Event {
	if turn == nil || turn["state"] != "failed" {
		return nil
	}
	event, err := terminalChatErrorFromTurn(turn, sequence)
	if err != nil {
		return nil
	}
	return event
}

func terminalChatErrorFromTurn(turn map[string]any, sequence int64) (*agentstream.Event, error) {
	if !validCanonicalTurn(turn) || turn["state"] != "failed" {
		return nil, fmt.Errorf("%w: terminal chat turn is not an authoritative failure", ErrInvalidActionResult)
	}
	revision, ok := turnInt64(turn["revision"])
	if !ok || revision <= 0 {
		return nil, fmt.Errorf("%w: terminal chat turn revision is invalid", ErrInvalidActionResult)
	}
	errorCode, codeOK := turn["terminal_code"].(string)
	errorSummary, summaryOK := turn["terminal_summary"].(string)
	errorCode = strings.TrimSpace(errorCode)
	errorSummary = strings.TrimSpace(errorSummary)
	if !codeOK || !summaryOK || errorCode == "" || errorSummary == "" {
		return nil, fmt.Errorf("%w: terminal chat failure details are missing", ErrInvalidActionResult)
	}
	data := map[string]any{
		"kind":            "error",
		"idempotency_key": turn["idempotency_key"],
		"conversation_id": turn["conversation_id"],
		"turn_id":         turn["turn_id"],
		"revision":        revision,
		"error_code":      errorCode,
		"error_summary":   errorSummary,
		"sequence":        positiveSequence(sequence),
	}
	if err := validateChatStreamEvent(data, actionResultAuthority{}); err != nil {
		return nil, err
	}
	return &agentstream.Event{Event: "error", Seq: positiveSequence(sequence), Data: data}, nil
}

func positiveSequence(sequence int64) int64 {
	if sequence > 0 {
		return sequence
	}
	return 0
}

type actionBinding struct{ capabilityID, operation string }

// actionBindings is the explicit public mapping between message-server action
// names and the Agent Core v1 catalog. There is no
// heuristic action→operation fallback: an action absent here is rejected
// before any capability request is sent.
var actionBindings = map[string]actionBinding{
	"agent.account.deprovision": {"agent.account.v1", "deprovision_account"},
	// Conversation and runtime.
	"agent.config.get":                 {"agent.config.v1", "get"},
	"agent.config.update":              {"agent.config.v1", "update"},
	"agent.backends.get":               {"agent.info.v1", "get_backends"},
	"agent.chat":                       {"agent.chat.v1", "chat"},
	"agent.chat.stream":                {"agent.chat.v1", "stream_chat"},
	"agent.chat.attachment.begin":      {"agent.chat.v1", "upload_attachment_begin"},
	"agent.chat.attachment.append":     {"agent.chat.v1", "upload_attachment_append"},
	"agent.chat.attachment.commit":     {"agent.chat.v1", "upload_attachment_commit"},
	"agent.chat.conversations.create":  {"agent.chat.v1", "create_conversation"},
	"agent.chat.conversations.list":    {"agent.chat.v1", "list_conversations"},
	"agent.chat.conversations.get":     {"agent.chat.v1", "get_conversation"},
	"agent.chat.conversations.rename":  {"agent.chat.v1", "rename_conversation"},
	"agent.chat.conversations.delete":  {"agent.chat.v1", "delete_conversation"},
	"agent.chat.turn.stop":             {"agent.chat.v1", "stop_turn"},
	"agent.chat.turn.steer":            {"agent.chat.v1", "steer_turn"},
	"agent.chat.turns.list":            {"agent.chat.v1", "list_turns"},
	"agent.context.compress":           {"agent.chat.v1", "compress_context"},
	"agent.summarize":                  {"agent.chat.v1", "summarize"},
	"agent.models.list":                {"agent.info.v1", "list_models"},
	"agent.runtime.inspect":            {"agent.runtime.v1", "inspect"},
	"agent.runtime.install":            {"agent.runtime.v1", "install"},
	"agent.runtime.which":              {"agent.runtime.v1", "which"},
	"agent.runtime.run":                {"agent.runtime.v1", "run"},
	"agent.web_search.config.get":      {"agent.web_search.v1", "get_config"},
	"agent.web_search.config.update":   {"agent.web_search.v1", "update_config"},
	"agent.web_search.test":            {"agent.web_search.v1", "test"},
	"agent.memory.config.get":          {"agent.memory.v1", "get_config"},
	"agent.memory.config.update":       {"agent.memory.v1", "update_config"},
	"agent.memory.status":              {"agent.memory.v1", "status"},
	"agent.memory.facts.update":        {"agent.memory.v1", "update_fact"},
	"agent.memory.facts.delete":        {"agent.memory.v1", "delete_fact"},
	"agent.text_tools.config.get":      {"agent.text_tools.v1", "get_config"},
	"agent.text_tools.config.update":   {"agent.text_tools.v1", "update_config"},
	"agent.text_tools.execute":         {"agent.text_tools.v1", "execute"},
	"agent.image_tools.upload.begin":   {"agent.image_tools.v1", "upload_begin"},
	"agent.image_tools.upload.append":  {"agent.image_tools.v1", "upload_append"},
	"agent.image_tools.upload.commit":  {"agent.image_tools.v1", "upload_commit"},
	"agent.image_tools.extract_text":   {"agent.image_tools.v1", "extract_text"},
	"agent.image_tools.translate_text": {"agent.image_tools.v1", "translate_text"},

	// Model profiles use the current authoritative sync API. Only operations
	// published by ProductCore are bound here.
	"agent.model_profiles.sync":   {"agent.models.v1", "sync_models"},
	"agent.model_profiles.list":   {"agent.models.v1", "list_models"},
	"agent.model_profiles.get":    {"agent.models.v1", "get_model"},
	"agent.model_profiles.test":   {"agent.models.v1", "test_model"},
	"agent.model_profiles.delete": {"agent.models.v1", "delete_model"},

	// Knowledge, vector search and long-term memory.
	"agent.knowledge.config.get":     {"agent.knowledge.v1", "get_config"},
	"agent.knowledge.config.update":  {"agent.knowledge.v1", "update_config"},
	"agent.knowledge.sources.list":   {"agent.knowledge.v1", "list_sources"},
	"agent.knowledge.sources.delete": {"agent.knowledge.v1", "delete_source"},
	"agent.knowledge.upload.start":   {"agent.knowledge.v1", "start_upload"},
	"agent.knowledge.upload.chunk":   {"agent.knowledge.v1", "append_upload_chunk"},
	"agent.knowledge.upload.finish":  {"agent.knowledge.v1", "commit_upload"},
	"agent.knowledge.search":         {"agent.knowledge.v1", "search_knowledge"},
	"agent.knowledge.status":         {"agent.knowledge.v1", "status"},

	// Agent-owned static-site releases. Message Server forwards only the
	// owner-scoped release inventory and exact delete mutation.
	"agent.static_sites.list":   {"agent.static_sites.v1", "list_releases"},
	"agent.static_sites.delete": {"agent.static_sites.v1", "delete_release"},

	// Persistent SSH Worker management. The capability is optional and appears
	// only after the Agent has one current verified AWS credential.
	"agent.workers.list":          {"agent.worker.v1", "list_workers"},
	"agent.workers.get":           {"agent.worker.v1", "get_worker"},
	"agent.workers.destroy":       {"agent.worker.v1", "destroy_worker"},
	"agent.workers.bind_domain":   {"agent.worker.v1", "bind_domain"},
	"agent.workers.unbind_domain": {"agent.worker.v1", "unbind_domain"},

	// Core Skill/MCP lifecycle operations keep their typed operation IDs and
	// confirmation fences.
	"agent.core.mcp.discover":    {"agent.skills.v1", "discover_mcp"},
	"agent.core.mcp.get":         {"agent.skills.v1", "get_mcp"},
	"agent.core.mcp.list":        {"agent.skills.v1", "list_mcp"},
	"agent.core.mcp.inspect":     {"agent.skills.v1", "inspect_mcp"},
	"agent.core.mcp.install":     {"agent.skills.v1", "install_mcp"},
	"agent.core.mcp.update":      {"agent.skills.v1", "update_mcp"},
	"agent.core.mcp.remove":      {"agent.skills.v1", "remove_mcp"},
	"agent.core.mcp.list_tools":  {"agent.skills.v1", "list_tools"},
	"agent.core.mcp.execute":     {"agent.skills.v1", "execute_mcp"},
	"agent.core.skills.discover": {"agent.skills.v1", "discover_skill"},
	"agent.core.skills.get":      {"agent.skills.v1", "get_skill"},
	"agent.core.skills.list":     {"agent.skills.v1", "list_skills"},
	"agent.core.skills.inspect":  {"agent.skills.v1", "inspect_skill"},
	"agent.core.skills.install":  {"agent.skills.v1", "install_skill"},
	"agent.core.skills.update":   {"agent.skills.v1", "update_skill"},
	"agent.core.skills.remove":   {"agent.skills.v1", "remove_skill"},
	"agent.core.skills.execute":  {"agent.skills.v1", "invoke_skill"},

	// Durable tasks and schedules.
	"agent.core.tasks.get":         {"agent.tasks.v1", "get_task"},
	"agent.core.tasks.list":        {"agent.tasks.v1", "list_tasks"},
	"agent.core.tasks.cancel":      {"agent.tasks.v1", "cancel_task"},
	"agent.core.tasks.retry":       {"agent.tasks.v1", "retry_task"},
	"agent.core.tasks.events":      {"agent.tasks.v1", "list_task_events"},
	"agent.core.schedules.create":  {"agent.schedules.v1", "create_schedule"},
	"agent.core.schedules.get":     {"agent.schedules.v1", "get_schedule"},
	"agent.core.schedules.list":    {"agent.schedules.v1", "list_schedules"},
	"agent.core.schedules.update":  {"agent.schedules.v1", "update_schedule"},
	"agent.core.schedules.pause":   {"agent.schedules.v1", "pause_schedule"},
	"agent.core.schedules.resume":  {"agent.schedules.v1", "resume_schedule"},
	"agent.core.schedules.trigger": {"agent.schedules.v1", "trigger_schedule"},
	"agent.core.schedules.delete":  {"agent.schedules.v1", "delete_schedule"},

	// Confirmation and cloud/workload operations are distinct typed domains;
	// the message-server only forwards them and never opens local stores.
	"agent.core.confirmations.get":                                       {"agent.confirmations.v1", "get"},
	"agent.core.confirmations.list":                                      {"agent.confirmations.v1", "list"},
	"agent.core.confirmations.confirm":                                   {"agent.confirmations.v1", "confirm"},
	"agent.core.confirmations.reject":                                    {"agent.confirmations.v1", "reject"},
	"agent.core.confirmations.acknowledge_extension_execution_uncertain": {"agent.confirmations.v1", "acknowledge_extension_execution_uncertain"},
	"agent.core.aws.credentials.create":                                  {"agent.aws.v1", "create_credential"},
	"agent.core.aws.credentials.update":                                  {"agent.aws.v1", "update_credential"},
	"agent.core.aws.credentials.delete":                                  {"agent.aws.v1", "delete_credential"},
	"agent.core.aws.credentials.list":                                    {"agent.aws.v1", "list_credentials"},
	"agent.core.aws.credentials.test":                                    {"agent.aws.v1", "test_credential"},

	// Voice sessions and execution.v2 remain Agent-owned. No local audio,
	// runner, AWS or execution stores are initialized in external mode.
	"agent.voice.session.create":     {"agent.voice.v1", "create_session"},
	"agent.voice.session.start":      {"agent.voice.v1", "start_session"},
	"agent.voice.session.transcript": {"agent.voice.v1", "submit_transcript"},
	"agent.voice.session.interrupt":  {"agent.voice.v1", "interrupt_session"},
	"agent.voice.session.end":        {"agent.voice.v1", "end_session"},
	"agent.voice.session.stream":     {"agent.voice.v1", "stream_session"},

	"agent.execution.v2.plans.get":          {"agent.execution.v2", "plans_get"},
	"agent.execution.v2.plans.list":         {"agent.execution.v2", "plans_list"},
	"agent.execution.v2.runs.get":           {"agent.execution.v2", "runs_get"},
	"agent.execution.v2.runs.list":          {"agent.execution.v2", "runs_list"},
	"agent.execution.v2.runs.cancel":        {"agent.execution.v2", "runs_cancel"},
	"agent.execution.v2.runs.events":        {"agent.execution.v2", "runs_events"},
	"agent.execution.v2.artifacts.get":      {"agent.execution.v2", "artifacts_get"},
	"agent.execution.v2.artifacts.download": {"agent.execution.v2", "artifacts_download"},
	"agent.execution.v2.artifacts.delete":   {"agent.execution.v2", "artifacts_delete"},

	// Product actions use the Agent Skills capability as a typed bridge; the
	// child capability and operation scopes are selected below.
	"agent.contacts.list":           {"agent.skills.v1", "invoke_product"},
	"agent.contacts.search":         {"agent.skills.v1", "invoke_product"},
	"agent.rooms.search":            {"agent.skills.v1", "invoke_product"},
	"agent.messages.list":           {"agent.skills.v1", "invoke_product"},
	"agent.messages.send":           {"agent.skills.v1", "invoke_product"},
	"agent.room_members.list":       {"agent.skills.v1", "invoke_product"},
	"agent.channel_posts.list":      {"agent.skills.v1", "invoke_product"},
	"agent.channel_comments.list":   {"agent.skills.v1", "invoke_product"},
	"agent.channel_comments.create": {"agent.skills.v1", "invoke_product"},
}

var productActionBindings = map[string]struct{ capabilityID, operation string }{
	"agent.contacts.list":           {"product.contacts.v1", "list"},
	"agent.contacts.search":         {"product.contacts.v1", "search"},
	"agent.rooms.search":            {"product.rooms.v1", "search"},
	"agent.messages.list":           {"product.messages.v1", "list"},
	"agent.messages.send":           {"product.messages.v1", "send"},
	"agent.room_members.list":       {"product.members.v1", "list"},
	"agent.channel_posts.list":      {"product.channels.v1", "get_posts"},
	"agent.channel_comments.list":   {"product.channel_comments.v1", "list"},
	"agent.channel_comments.create": {"product.channel_comments.v1", "create"},
}

func actionBindingFor(action string) (actionBinding, bool) {
	action = strings.TrimSpace(action)
	binding, ok := actionBindings[action]
	return binding, ok
}

func transformCapabilityRequest(action, operationID string, params map[string]any, binding actionBinding) ([]byte, error) {
	input := cloneParams(params)
	// operation_id is the transport idempotency key selected by the gateway;
	// it is not part of any capability's business schema.
	delete(input, "operation_id")
	// after_seq is a reconnect cursor for the gateway Watch call, not part of
	// the business operation. Keeping it out of the canonical request makes a
	// retry/reconnect use the same operation digest regardless of the last
	// sequence observed by the client.
	delete(input, "after_seq")
	if action == "agent.chat" || action == "agent.chat.stream" {
		// ProductCore and the Agent capability share one closed request shape.
		// Reconnect cursors and stream correlation metadata are consumed by
		// the gateway and must never cross the capability boundary.
		canonical := make(map[string]any, 8)
		for _, key := range []string{
			"idempotency_key", "conversation_id", "message",
			"model_profile_id", "model_profile_revision", "credential_version",
			"accepted_attachment_ids", "extensions",
		} {
			if value, present := input[key]; present {
				canonical[key] = value
			}
		}
		input = canonical
	}
	if action == "agent.models.list" {
		modelKind, exists := input["model_kind"]
		if !exists {
			input["model_kind"] = "conversation"
		} else if kind, ok := modelKind.(string); !ok || strings.TrimSpace(kind) == "" {
			// ValidateActionRequest rejects this before the capability lookup. Keep
			// this branch defensive for callers that invoke the transformer in
			// isolation; it must never silently select a default for an explicit
			// invalid value.
			return nil, invalidActionRequest(action, "model_kind", "must be conversation, embedding, or speech")
		}
	}
	translateProductCoreInput(action, input)
	if product, ok := productActionBindings[action]; ok && binding.operation == "invoke_product" {
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("marshal Product capability input: %w", err)
		}
		input = map[string]any{
			"capability_id": product.capabilityID,
			"operation":     product.operation,
			"request_json":  json.RawMessage(raw),
			"operation_id":  operationID,
		}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal external capability request: %w", err)
	}
	canonical, err := capv1.CanonicalizeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize external capability request: %w", err)
	}
	return canonical, nil
}

func cloneParams(params map[string]any) map[string]any {
	copy := make(map[string]any, len(params))
	for key, value := range params {
		copy[key] = value
	}
	return copy
}
