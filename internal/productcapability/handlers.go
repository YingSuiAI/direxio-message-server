package productcapability

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *Server) DescribeCapabilities(ctx context.Context, req *capv1.DescribeCapabilitiesRequest) (*capv1.DescribeCapabilitiesResponse, error) {
	descriptors := s.registry.List()
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].CapabilityId < descriptors[j].CapabilityId })
	return &capv1.DescribeCapabilitiesResponse{Capabilities: descriptors, CatalogVersion: 1, CatalogDigest: s.catalogDigest()}, nil
}

// catalogDigest is the stable digest of the currently advertised Product
// catalog.  Keep descriptor ordering local to the digest calculation so the
// registry's map iteration order can never alter grant bindings.
func (s *Server) catalogDigest() []byte {
	descriptors := s.registry.List()
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].CapabilityId < descriptors[j].CapabilityId })
	digest := sha256.New()
	for _, descriptor := range descriptors {
		encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(descriptor)
		if err != nil {
			// Registered descriptors are validated at startup.  Returning a fixed
			// digest on an impossible marshal error fails grant verification closed
			// instead of producing an unstable or partially bound grant.
			return nil
		}
		_, _ = digest.Write(encoded)
	}
	return digest.Sum(nil)
}

// ExchangeProductDelegation is the private Agent→message-server broker. The
// parent grant is the Agent-bound grant issued by message-server; this method
// never forwards it to Product handlers. It verifies that parent grant at the
// actual Product route, checks that the requested Product operation is within
// the parent's scopes, and signs a fresh child grant bound to the exact
// Product descriptor, schema, request digest and outer root operation.
func (s *Server) ExchangeProductDelegation(ctx context.Context, req *capv1.ExchangeProductDelegationRequest) (*capv1.ExchangeProductDelegationResponse, error) {
	if err := s.acquireMutationSem(ctx); err != nil {
		return nil, err
	}
	defer s.releaseMutationSem()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	provider, operation, capabilityErr := s.resolveProvider(req.CapabilityId, req.Operation, capv1.OperationType_OPERATION_TYPE_UNSPECIFIED)
	if capabilityErr != nil {
		return nil, status.Error(grpcCodeForCapability(capabilityErr.Code), capabilityErr.Message)
	}
	requestJSON := req.RequestJson
	if len(requestJSON) == 0 {
		requestJSON = []byte(`{}`)
	}
	// The shared API validator is deliberately called after defaulting an
	// omitted input to canonical `{}`.  This keeps broker requests strict while
	// preserving the public convention that an empty input means no fields.
	brokerRequest := proto.Clone(req).(*capv1.ExchangeProductDelegationRequest)
	brokerRequest.RequestJson = requestJSON
	if err := capv1.ValidateProductDelegationExchangeRequest(brokerRequest); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateOperationInput(operation, requestJSON); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.ParentPermission == nil || strings.TrimSpace(req.ParentPermission.AuthenticatedOwnerId) == "" {
		return nil, status.Error(codes.PermissionDenied, "parent permission context required")
	}
	if req.CallContext == nil {
		return nil, status.Error(codes.InvalidArgument, "call_context is required")
	}
	if err := s.validatePermissionMetadata(ctx, req.ParentPermission); err != nil {
		return nil, status.Error(grpcCodeForCapability(err.Code), err.Message)
	}
	if len(req.ParentPermission.RootRequestDigest) != sha256.Size {
		return nil, status.Error(codes.Unauthenticated, "parent root request digest is required")
	}
	parentClaims, err := capv1.VerifyProductDelegationParent(req.ParentPermission.CapabilityGrant, s.config.GrantPublicKey, grantNow(s.config.GrantCodec), capv1.RootGrantBinding{
		CallContext:       req.CallContext,
		RootOperationID:   req.CallContext.RootOperationId,
		OwnerID:           req.ParentPermission.AuthenticatedOwnerId,
		AccountGeneration: req.ParentPermission.AccountGeneration,
		RootRequestDigest: req.ParentPermission.RootRequestDigest,
		RequiredScopes:    append([]string(nil), req.ParentPermission.GrantedScopes...),
	})
	if err != nil || !sameStrings(parentClaims.Scopes, req.ParentPermission.GrantedScopes) {
		return nil, status.Error(codes.Unauthenticated, "parent capability grant binding failed")
	}
	if !strings.HasPrefix(parentClaims.RootCapabilityID, "agent.") || !scopesAllow(parentClaims.Scopes, []string{"agent:product:execute"}) {
		return nil, status.Error(codes.PermissionDenied, "parent grant cannot delegate Product capability")
	}
	if !scopesAllow(parentClaims.Scopes, operation.RequiredScopes) {
		return nil, status.Error(codes.PermissionDenied, "parent grant does not cover Product operation scope")
	}
	rootDigest, err := rootRequestDigest(provider.Descriptor, operation, requestJSON, req.ExpectedRevision)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	catalogDigest := s.catalogDigest()
	schemaDigest := sha256.Sum256([]byte(operation.InputSchemaJson))
	childGrant, err := s.config.GrantCodec.SignProductDelegationFromParent(capv1.ProductDelegationIssue{
		ParentClaims:      parentClaims,
		CallContext:       req.CallContext,
		ChildOperationID:  req.GetChildOperationId(),
		CapabilityID:      req.CapabilityId,
		Operation:         operation.OperationId,
		RequiredScopes:    append([]string(nil), operation.RequiredScopes...),
		TargetKind:        req.GetTargetKind(),
		RootRequestDigest: rootDigest,
		CatalogDigest:     catalogDigest,
		SchemaDigest:      schemaDigest[:],
	}, s.config.GrantPrivateKey)
	if err != nil {
		return nil, status.Error(codes.Internal, "issue Product delegation")
	}
	claims, err := capv1.VerifyProductDelegationGrant(childGrant, s.config.GrantPublicKey, grantNow(s.config.GrantCodec), capv1.ProductGrantBinding{
		CallContext:       req.CallContext,
		RootOperationID:   req.CallContext.RootOperationId,
		ChildOperationID:  req.GetChildOperationId(),
		OwnerID:           req.ParentPermission.AuthenticatedOwnerId,
		AccountGeneration: req.ParentPermission.AccountGeneration,
		RequiredScopes:    append([]string(nil), operation.RequiredScopes...),
		CapabilityID:      req.CapabilityId,
		Operation:         operation.OperationId,
		TargetKind:        req.GetTargetKind(),
		RootRequestDigest: rootDigest,
		CatalogDigest:     catalogDigest,
		SchemaDigest:      schemaDigest[:],
	})
	if err != nil || claims.RootCapabilityID != req.CapabilityId || claims.RootOperation != operation.OperationId || !equalBytes(claims.CatalogDigest, catalogDigest) || !equalBytes(claims.SchemaDigest, schemaDigest[:]) {
		return nil, status.Error(codes.Internal, "issued Product delegation failed exact binding")
	}
	return &capv1.ExchangeProductDelegationResponse{
		ProductPermission: &capv1.PermissionContext{
			AuthenticatedOwnerId: req.ParentPermission.AuthenticatedOwnerId,
			// The child grant is narrowed to the exact Product operation scopes.
			// Returning the parent scope set here would make the opaque context
			// claim broader than the signed child grant and fail closed consumers
			// that bind the permission metadata byte-for-byte.
			GrantedScopes:     append([]string(nil), operation.RequiredScopes...),
			CapabilityGrant:   childGrant,
			AccountGeneration: req.ParentPermission.AccountGeneration,
			RootRequestDigest: rootDigest,
		},
		ExpiresAtUnixMs: claims.ExpiresAtUnixMs,
	}, nil
}

func (s *Server) Query(ctx context.Context, req *capv1.QueryRequest) (*capv1.QueryResponse, error) {
	if err := s.acquireReadSem(ctx); err != nil {
		return nil, err
	}
	defer s.releaseReadSem()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	provider, operation, err := s.resolveProvider(req.CapabilityId, req.OperationId, capv1.OperationType_OPERATION_TYPE_READ)
	if err != nil {
		return &capv1.QueryResponse{Error: err}, nil
	}
	requestJSON := req.RequestJson
	if len(requestJSON) == 0 {
		requestJSON = []byte(`{}`)
	}
	if inputErr := validateOperationInput(operation, requestJSON); inputErr != nil {
		return &capv1.QueryResponse{Error: capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, inputErr.Error())}, nil
	}
	if err := s.validateRootPermission(ctx, req.Permission, req.CallContext, provider.Descriptor, operation, requestJSON, 0, "", capv1.ExchangeProductTargetKind_EXCHANGE_PRODUCT_TARGET_KIND_QUERY); err != nil {
		return &capv1.QueryResponse{Error: err}, nil
	}
	if !scopesAllow(req.Permission.GrantedScopes, operation.RequiredScopes) {
		return &capv1.QueryResponse{Error: capabilityError(capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "required capability scope is missing")}, nil
	}
	wrapped, marshalErr := json.Marshal(map[string]any{"operation": operation.OperationId, "input": json.RawMessage(requestJSON)})
	if marshalErr != nil {
		return &capv1.QueryResponse{Error: capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, marshalErr.Error())}, nil
	}
	result, invokeErr := provider.Handler(ctx, wrapped)
	if invokeErr != nil {
		return &capv1.QueryResponse{Error: capabilityError(capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED, invokeErr.Error())}, nil
	}
	return &capv1.QueryResponse{ResultJson: result}, nil
}

func (s *Server) StartOperation(ctx context.Context, req *capv1.StartOperationRequest) (*capv1.StartOperationResponse, error) {
	if err := s.acquireMutationSem(ctx); err != nil {
		return nil, err
	}
	defer s.releaseMutationSem()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	provider, operation, err := s.resolveProvider(req.CapabilityId, req.Operation, capv1.OperationType_OPERATION_TYPE_MUTATION)
	if err != nil {
		return &capv1.StartOperationResponse{OperationId: req.OperationId, State: capv1.OperationState_OPERATION_STATE_FAILED, Error: err}, nil
	}
	if strings.TrimSpace(req.OperationId) == "" {
		return &capv1.StartOperationResponse{Error: capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "operation_id is required")}, nil
	}
	requestJSON := req.RequestJson
	if len(requestJSON) == 0 {
		requestJSON = []byte(`{}`)
	}
	if inputErr := validateOperationInput(operation, requestJSON); inputErr != nil {
		return &capv1.StartOperationResponse{OperationId: req.OperationId, State: capv1.OperationState_OPERATION_STATE_FAILED, Error: capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, inputErr.Error())}, nil
	}
	if err := s.validateRootPermission(ctx, req.Permission, req.CallContext, provider.Descriptor, operation, requestJSON, req.ExpectedRevision, req.OperationId, capv1.ExchangeProductTargetKind_EXCHANGE_PRODUCT_TARGET_KIND_START_OPERATION); err != nil {
		return &capv1.StartOperationResponse{OperationId: req.OperationId, State: capv1.OperationState_OPERATION_STATE_FAILED, Error: err}, nil
	}
	if !scopesAllow(req.Permission.GrantedScopes, operation.RequiredScopes) {
		return &capv1.StartOperationResponse{OperationId: req.OperationId, State: capv1.OperationState_OPERATION_STATE_FAILED, Error: capabilityError(capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "required capability scope is missing")}, nil
	}
	digest, digestErr := canonicalRequestDigest(req, provider.Descriptor, operation, requestJSON)
	if digestErr != nil {
		return &capv1.StartOperationResponse{OperationId: req.OperationId, State: capv1.OperationState_OPERATION_STATE_FAILED, Error: capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, digestErr.Error())}, nil
	}
	if len(req.RequestDigest) > 0 && !equalBytes(req.RequestDigest, digest) {
		return &capv1.StartOperationResponse{OperationId: req.OperationId, State: capv1.OperationState_OPERATION_STATE_FAILED, Error: capabilityError(capv1.ErrorCode_ERROR_CODE_CONFLICT, "request_digest does not match request_json")}, nil
	}
	rootDigest, rootDigestErr := rootRequestDigest(provider.Descriptor, operation, requestJSON, req.ExpectedRevision)
	if rootDigestErr != nil {
		return &capv1.StartOperationResponse{OperationId: req.OperationId, State: capv1.OperationState_OPERATION_STATE_FAILED, Error: capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, rootDigestErr.Error())}, nil
	}
	record := &operationRecord{ID: req.OperationId, Capability: req.CapabilityId, Operation: operation.OperationId, OwnerID: req.Permission.AuthenticatedOwnerId, Generation: req.Permission.AccountGeneration, RootDigest: rootDigest, Digest: digest}
	existing, startErr := s.operations.start(ctx, record)
	if startErr != nil {
		code := capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED
		if startErr == errOperationConflict {
			code = capv1.ErrorCode_ERROR_CODE_CONFLICT
		}
		return &capv1.StartOperationResponse{OperationId: req.OperationId, State: capv1.OperationState_OPERATION_STATE_FAILED, Error: capabilityError(code, startErr.Error())}, nil
	}
	if !existing.Created {
		// Matrix mutations are prepared before SendEvents.  When the process
		// restart fence left one uncertain operation, claim it once and rerun the
		// provider; the MCP layer reconciles the staged event or submits the
		// identical PDU. A live pending/running handler is never duplicated.
		if isPreparedMatrixMutation(req.CapabilityId, operation.OperationId) && existing.State == capv1.OperationState_OPERATION_STATE_UNCERTAIN {
			if claimed, claimErr := s.operations.claimReplay(ctx, existing.ID); claimErr != nil {
				return nil, status.Error(codes.Internal, "claim uncertain Matrix operation")
			} else if claimed {
				s.executeOperation(context.Background(), existing, provider, operation.OperationId, requestJSON)
			}
		}
		return s.startOperationResponse(ctx, req.CallContext, req.Permission, existing)
	}
	if existing.State != capv1.OperationState_OPERATION_STATE_PENDING {
		return s.startOperationResponse(ctx, req.CallContext, req.Permission, existing)
	}
	operationCtx, cancel := context.WithCancel(context.Background())
	s.operations.registerCancel(existing.ID, cancel)
	go func() {
		defer cancel()
		defer s.operations.clearCancel(existing.ID)
		s.executeOperation(operationCtx, existing, provider, operation.OperationId, requestJSON)
	}()
	return s.startOperationResponse(ctx, req.CallContext, req.Permission, existing)
}

func (s *Server) executeOperation(ctx context.Context, record *operationRecord, provider *Provider, operationIDName string, requestJSON []byte) {
	if record == nil {
		return
	}
	operationID := record.ID
	ctx = dirextalktransport.WithCapabilityOperationContext(ctx, dirextalktransport.CapabilityOperationContext{
		OperationID: operationID,
		OwnerID:     record.OwnerID,
		Generation:  record.Generation,
		RootDigest:  record.RootDigest,
	})
	wrapped, err := json.Marshal(map[string]any{"operation": operationIDName, "input": json.RawMessage(requestJSON)})
	if err != nil {
		_ = s.operations.finish(ctx, operationID, nil, capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, err.Error()))
		return
	}
	result, invokeErr := provider.Handler(ctx, wrapped)
	if invokeErr != nil {
		if errors.Is(invokeErr, dirextalktransport.ErrMatrixEventUnknown) {
			_ = s.operations.markUncertain(context.Background(), operationID, capabilityError(capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED, invokeErr.Error()))
			return
		}
		_ = s.operations.finish(ctx, operationID, nil, capabilityError(capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED, invokeErr.Error()))
		return
	}
	_ = s.operations.finish(ctx, operationID, result, nil)
}

func (s *Server) GetOperation(ctx context.Context, req *capv1.GetOperationRequest) (*capv1.GetOperationResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validateControlPermission(ctx, req.Permission, req.CallContext, req.OperationId, "get"); err != nil {
		return nil, status.Error(grpcCodeForCapability(err.Code), err.Message)
	}
	record, err := s.operations.get(ctx, req.OperationId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "operation not found")
	}
	if record.OwnerID != req.Permission.AuthenticatedOwnerId || record.Generation != req.Permission.AccountGeneration {
		return nil, status.Error(codes.PermissionDenied, "operation owner mismatch")
	}
	response := &capv1.GetOperationResponse{OperationId: record.ID, State: record.State, ResultJson: append([]byte(nil), record.Result...), Sequence: s.operations.eventCount(record.ID)}
	response.Error = record.Err
	return response, nil
}

func (s *Server) WatchOperation(req *capv1.WatchOperationRequest, stream capv1.ProductCapabilityService_WatchOperationServer) error {
	if req == nil {
		return status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validateCallContext(req); err != nil {
		return err
	}
	if err := s.validateControlPermission(stream.Context(), req.Permission, req.CallContext, req.OperationId, "watch"); err != nil {
		return status.Error(grpcCodeForCapability(err.Code), err.Message)
	}
	record, err := s.operations.get(stream.Context(), req.OperationId)
	if err != nil {
		return status.Error(codes.NotFound, "operation not found")
	}
	if record.OwnerID != req.Permission.AuthenticatedOwnerId || record.Generation != req.Permission.AccountGeneration {
		return status.Error(codes.PermissionDenied, "operation owner mismatch")
	}
	events, err := s.operations.watch(stream.Context(), req.OperationId, req.AfterSequence)
	if err != nil {
		return status.Error(codes.NotFound, "operation events not found")
	}
	for event := range events {
		if err := stream.Send(event.Event); err != nil {
			return err
		}
		if isTerminalEvent(event.Event) {
			return nil
		}
	}
	return nil
}

func (s *Server) CancelOperation(ctx context.Context, req *capv1.CancelOperationRequest) (*capv1.CancelOperationResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validateControlPermission(ctx, req.Permission, req.CallContext, req.OperationId, "cancel"); err != nil {
		return nil, status.Error(grpcCodeForCapability(err.Code), err.Message)
	}
	record, err := s.operations.get(ctx, req.OperationId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "operation not found")
	}
	if record.OwnerID != req.Permission.AuthenticatedOwnerId || record.Generation != req.Permission.AccountGeneration {
		return nil, status.Error(codes.PermissionDenied, "operation owner mismatch")
	}
	if err := s.operations.cancel(ctx, req.OperationId); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &capv1.CancelOperationResponse{State: capv1.OperationState_OPERATION_STATE_CANCELLED}, nil
}

func (s *Server) ReconcileOperation(ctx context.Context, req *capv1.ReconcileOperationRequest) (*capv1.ReconcileOperationResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.validateControlPermission(ctx, req.Permission, req.CallContext, req.OperationId, "reconcile"); err != nil {
		return nil, status.Error(grpcCodeForCapability(err.Code), err.Message)
	}
	record, err := s.operations.get(ctx, req.OperationId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "operation not found")
	}
	if record.OwnerID != req.Permission.AuthenticatedOwnerId || record.Generation != req.Permission.AccountGeneration {
		return nil, status.Error(codes.PermissionDenied, "operation owner mismatch")
	}
	return &capv1.ReconcileOperationResponse{State: record.State, ResultJson: append([]byte(nil), record.Result...), Error: record.Err}, nil
}

func (s *Server) resolveProvider(capabilityID, operationID string, expectedType capv1.OperationType) (*Provider, *capv1.OperationDescriptor, *capv1.CapabilityError) {
	provider, ok := s.registry.Get(strings.TrimSpace(capabilityID))
	if !ok || provider == nil || provider.Descriptor == nil {
		return nil, nil, capabilityError(capv1.ErrorCode_ERROR_CODE_NOT_FOUND, "capability not found")
	}
	if !provider.Descriptor.Readiness {
		return nil, nil, capabilityError(capv1.ErrorCode_ERROR_CODE_NOT_READY, provider.Descriptor.ReadinessReason)
	}
	operation, ok := s.registry.Operation(capabilityID, operationID)
	if !ok {
		return nil, nil, capabilityError(capv1.ErrorCode_ERROR_CODE_NOT_FOUND, "operation not found")
	}
	if expectedType != capv1.OperationType_OPERATION_TYPE_UNSPECIFIED && operation.OperationType != expectedType {
		return nil, nil, capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "operation type is not supported by this RPC")
	}
	return provider, operation, nil
}

func (s *Server) validateRootPermission(ctx context.Context, permission *capv1.PermissionContext, callCtx *capv1.CallContext, descriptor *capv1.CapabilityDescriptor, operation *capv1.OperationDescriptor, requestJSON []byte, expectedRevision int64, childOperationID string, targetKind capv1.ExchangeProductTargetKind) *capv1.CapabilityError {
	if permission == nil || strings.TrimSpace(permission.AuthenticatedOwnerId) == "" {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "permission context required")
	}
	if len(s.config.GrantPublicKey) != ed25519.PublicKeySize || len(permission.CapabilityGrant) == 0 {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, "signed capability grant is required")
	}
	if callCtx == nil || strings.TrimSpace(callCtx.RootOperationId) == "" {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "call_context root operation is required")
	}
	if err := s.validatePermissionMetadata(ctx, permission); err != nil {
		return err
	}
	rootDigest, err := rootRequestDigest(descriptor, operation, requestJSON, expectedRevision)
	if err != nil {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, err.Error())
	}
	if len(permission.RootRequestDigest) != sha256.Size || !equalBytes(permission.RootRequestDigest, rootDigest) {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, "product root request digest does not match permission")
	}
	catalogDigest := s.catalogDigest()
	schemaDigest := sha256.Sum256([]byte(operation.GetInputSchemaJson()))
	claims, err := capv1.VerifyProductDelegationGrant(permission.CapabilityGrant, s.config.GrantPublicKey, grantNow(s.config.GrantCodec), capv1.ProductGrantBinding{
		CallContext:       callCtx,
		RootOperationID:   callCtx.RootOperationId,
		ChildOperationID:  childOperationID,
		OwnerID:           permission.AuthenticatedOwnerId,
		AccountGeneration: permission.AccountGeneration,
		RequiredScopes:    append([]string(nil), permission.GrantedScopes...),
		CapabilityID:      descriptor.GetCapabilityId(),
		Operation:         operation.GetOperationId(),
		TargetKind:        targetKind,
		RootRequestDigest: rootDigest,
		CatalogDigest:     catalogDigest,
		SchemaDigest:      schemaDigest[:],
	})
	if err != nil {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, "capability grant binding failed")
	}
	if claims.OwnerID != permission.AuthenticatedOwnerId || claims.AccountGeneration != permission.AccountGeneration {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, "capability grant verification failed")
	}
	if !sameStrings(claims.Scopes, permission.GrantedScopes) {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, "permission scopes do not match grant")
	}
	// VerifyProductDelegationGrant above rejects the Agent-bound parent grant
	// and binds this request to the current Product descriptor/catalog/schema.
	_ = claims
	return nil
}

func (s *Server) validateControlPermission(ctx context.Context, permission *capv1.PermissionContext, callCtx *capv1.CallContext, operationID, action string) *capv1.CapabilityError {
	if permission == nil || strings.TrimSpace(permission.AuthenticatedOwnerId) == "" {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "permission context required")
	}
	if len(s.config.GrantPublicKey) != ed25519.PublicKeySize || len(permission.CapabilityGrant) == 0 {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, "signed operation control grant is required")
	}
	if callCtx == nil || strings.TrimSpace(callCtx.RootOperationId) == "" {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "call_context root operation is required")
	}
	if err := s.validatePermissionMetadata(ctx, permission); err != nil {
		return err
	}
	action = strings.TrimSpace(action)
	scope := "operation:control:" + action
	_, err := s.config.GrantCodec.VerifyProductOperationControlGrant(permission.CapabilityGrant, s.config.GrantPublicKey, capv1.OperationControlGrantBinding{
		CallContext:       callCtx,
		OwnerID:           permission.AuthenticatedOwnerId,
		AccountGeneration: permission.AccountGeneration,
		OperationID:       strings.TrimSpace(operationID),
		ControlAction:     action,
		ControlScope:      scope,
	})
	if err != nil {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, "operation control grant binding failed")
	}
	return nil
}

func (s *Server) validatePermissionMetadata(ctx context.Context, permission *capv1.PermissionContext) *capv1.CapabilityError {
	if md, ok := metadata.FromIncomingContext(ctx); !ok {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, "capability metadata is required")
	} else {
		generationValue := strings.TrimSpace(firstMetadata(md, capv1.CapabilityGenerationMetadata))
		generation, err := strconv.ParseInt(generationValue, 10, 64)
		if err != nil || generation != permission.AccountGeneration {
			return capabilityError(capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, "permission generation does not match authenticated peer")
		}
	}
	return nil
}

func (s *Server) startOperationResponse(ctx context.Context, callCtx *capv1.CallContext, permission *capv1.PermissionContext, record *operationRecord) (*capv1.StartOperationResponse, error) {
	if record == nil {
		return nil, status.Error(codes.Internal, "operation record is unavailable")
	}
	response := &capv1.StartOperationResponse{OperationId: record.ID, State: record.State, Error: record.Err, Replayed: !record.Created}
	grants, err := s.issueOperationControlGrants(callCtx, permission, record.ID)
	if err != nil {
		return nil, status.Error(codes.Internal, "issue operation control grants")
	}
	response.ControlGrants = grants
	return response, nil
}

func isPreparedMatrixMutation(capabilityID, operationID string) bool {
	capabilityID = strings.TrimSpace(capabilityID)
	operationID = strings.TrimSpace(operationID)
	return (capabilityID == "product.messages.v1" && operationID == "send") ||
		(capabilityID == "product.channel_comments.v1" && operationID == "create") ||
		(capabilityID == "product.spi.messages_send.v1" && operationID == "invoke") ||
		(capabilityID == "product.spi.channel_comments_create.v1" && operationID == "invoke")
}

func (s *Server) issueOperationControlGrants(callCtx *capv1.CallContext, permission *capv1.PermissionContext, operationID string) ([]*capv1.OperationControlGrantEnvelope, error) {
	if callCtx == nil || permission == nil || strings.TrimSpace(operationID) == "" {
		return nil, fmt.Errorf("operation control grant binding is incomplete")
	}
	entryRoute, entryHop, err := productGrantEntry(callCtx)
	if err != nil {
		return nil, err
	}
	actions := []string{"get", "watch", "cancel", "reconcile"}
	grants := make([]*capv1.OperationControlGrantEnvelope, 0, len(actions))
	for _, action := range actions {
		grant, signErr := s.config.GrantCodec.SignOperationControlGrant(capv1.OperationControlGrant{
			ChainID:           callCtx.ChainId,
			OwnerID:           permission.AuthenticatedOwnerId,
			AccountGeneration: permission.AccountGeneration,
			OperationID:       operationID,
			ControlAction:     action,
			ControlScope:      "operation:control:" + action,
			EntryRoute:        entryRoute,
			EntryHop:          entryHop,
			DeadlineUnixMs:    callCtx.DeadlineUnixMs,
		}, s.config.GrantPrivateKey)
		if signErr != nil {
			return nil, signErr
		}
		claims, verifyErr := s.config.GrantCodec.VerifyProductOperationControlGrant(grant, s.config.GrantPublicKey, capv1.OperationControlGrantBinding{
			CallContext:       callCtx,
			OwnerID:           permission.AuthenticatedOwnerId,
			AccountGeneration: permission.AccountGeneration,
			OperationID:       operationID,
			ControlAction:     action,
			ControlScope:      "operation:control:" + action,
		})
		if verifyErr != nil {
			return nil, fmt.Errorf("verify freshly signed %s control grant: %w", action, verifyErr)
		}
		grants = append(grants, &capv1.OperationControlGrantEnvelope{Action: action, Grant: grant, ExpiresAtUnixMs: claims.ExpiresAtUnixMs})
	}
	if err := capv1.ValidateOperationControlGrantEnvelopes(grants, grantNow(s.config.GrantCodec).UnixMilli()); err != nil {
		return nil, err
	}
	return grants, nil
}

func grantNow(codec capv1.GrantCodec) time.Time {
	if codec.Now != nil {
		return codec.Now()
	}
	return time.Now()
}

func productGrantEntry(callCtx *capv1.CallContext) (string, int32, error) {
	if callCtx == nil || strings.TrimSpace(callCtx.Route) == "" {
		return "", 0, fmt.Errorf("product call route is required")
	}
	parts := strings.Split(callCtx.Route, capv1.RouteSeparator)
	if len(parts) == 0 {
		return "", 0, fmt.Errorf("product call route is invalid")
	}
	switch parts[0] {
	case capv1.NodeMessage, capv1.NodeAgent:
		return parts[0], 1, nil
	default:
		return "", 0, fmt.Errorf("product call route has an invalid entry node")
	}
}

func canonicalRequestDigest(req *capv1.StartOperationRequest, descriptor *capv1.CapabilityDescriptor, operation *capv1.OperationDescriptor, requestJSON []byte) ([]byte, error) {
	if descriptor == nil || operation == nil {
		return nil, fmt.Errorf("capability descriptor is unavailable")
	}
	businessInput, err := capv1.ParseBusinessInput(requestJSON)
	if err != nil {
		return nil, err
	}
	// Keep this in lockstep with the Agent capability server: the schema digest
	// is derived from the advertised input schema bytes, while the opaque grant
	// is separately verified above and then bound into the
	// canonical digest.
	schemaDigest := sha256.Sum256([]byte(operation.GetInputSchemaJson()))
	grantDigest := sha256.Sum256(req.GetPermission().GetCapabilityGrant())
	return capv1.ComputeRequestDigest(
		descriptor.GetProtocolVersion(), descriptor.GetCapabilityId(), descriptor.GetSemanticVersion(),
		schemaDigest[:], operation.GetOperationId(), req.GetExpectedRevision(), businessInput, nil, grantDigest[:],
	)
}

func rootRequestDigest(descriptor *capv1.CapabilityDescriptor, operation *capv1.OperationDescriptor, requestJSON []byte, expectedRevision int64) ([]byte, error) {
	if descriptor == nil || operation == nil {
		return nil, fmt.Errorf("capability descriptor is unavailable")
	}
	businessInput, err := capv1.ParseBusinessInput(requestJSON)
	if err != nil {
		return nil, err
	}
	schemaDigest := sha256.Sum256([]byte(operation.GetInputSchemaJson()))
	return capv1.ComputeRootRequestDigest(
		descriptor.GetProtocolVersion(), descriptor.GetCapabilityId(), descriptor.GetSemanticVersion(),
		schemaDigest[:], operation.GetOperationId(), expectedRevision, businessInput, nil,
	)
}

func capabilityError(code capv1.ErrorCode, message string) *capv1.CapabilityError {
	return &capv1.CapabilityError{Code: code, Message: message}
}

func grpcCodeForCapability(code capv1.ErrorCode) codes.Code {
	switch code {
	case capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return codes.InvalidArgument
	case capv1.ErrorCode_ERROR_CODE_TRUST_FAILED:
		return codes.Unauthenticated
	case capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return codes.PermissionDenied
	case capv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return codes.NotFound
	case capv1.ErrorCode_ERROR_CODE_CONFLICT:
		return codes.AlreadyExists
	case capv1.ErrorCode_ERROR_CODE_NOT_READY, capv1.ErrorCode_ERROR_CODE_UNAVAILABLE:
		return codes.Unavailable
	case capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED:
		return codes.ResourceExhausted
	case capv1.ErrorCode_ERROR_CODE_CYCLE_DETECTED, capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED:
		return codes.FailedPrecondition
	default:
		return codes.Internal
	}
}
func isTerminalEvent(event *capv1.WatchOperationEvent) bool {
	if event == nil {
		return false
	}
	switch event.Event.(type) {
	case *capv1.WatchOperationEvent_Result, *capv1.WatchOperationEvent_Error, *capv1.WatchOperationEvent_Cancelled:
		return true
	}
	return false
}
func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy, rightCopy := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}

func scopesAllow(granted, required []string) bool {
	if len(required) == 0 {
		return true
	}
	for _, requirement := range required {
		matched := false
		for _, candidate := range granted {
			candidate, requirement = strings.TrimSpace(candidate), strings.TrimSpace(requirement)
			if candidate == requirement || candidate == "*" || candidate == "product:*" || (strings.HasSuffix(candidate, ":*") && strings.HasPrefix(requirement, strings.TrimSuffix(candidate, "*"))) {
				matched = true
				break
			}
			// An agent-wide read/write grant can satisfy a product operation of
			// the same direction, but never a mutation with a read-only grant.
			if strings.HasPrefix(requirement, "product:") && (candidate == "agent:read" && strings.HasSuffix(requirement, ":read") || candidate == "agent:write" && strings.HasSuffix(requirement, ":write")) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func (s *Server) acquireReadSem(ctx context.Context) error {
	select {
	case s.readSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return status.Error(codes.DeadlineExceeded, "timeout acquiring read semaphore")
	case <-time.After(time.Second):
		return status.Error(codes.ResourceExhausted, "read semaphore exhausted")
	}
}

func (s *Server) releaseReadSem() { <-s.readSem }

func (s *Server) acquireMutationSem(ctx context.Context) error {
	select {
	case s.mutationSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return status.Error(codes.DeadlineExceeded, "timeout acquiring mutation semaphore")
	case <-time.After(time.Second):
		return status.Error(codes.ResourceExhausted, "mutation semaphore exhausted")
	}
}

func (s *Server) releaseMutationSem() { <-s.mutationSem }
