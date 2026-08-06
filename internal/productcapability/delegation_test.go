package productcapability

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func newDelegationTestServer(t *testing.T) (*Server, ed25519.PrivateKey, time.Time) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_700_000_000_000)
	registry := NewRegistry()
	descriptor := &capv1.CapabilityDescriptor{
		CapabilityId:    "product.test.v1",
		SemanticVersion: "1.0.0",
		ProtocolVersion: 1,
		Readiness:       true,
		Operations: []*capv1.OperationDescriptor{
			{OperationId: "list", OperationType: capv1.OperationType_OPERATION_TYPE_READ, RequiredScopes: []string{"product:test:read"}, Audience: []capv1.Audience{capv1.Audience_AUDIENCE_NATIVE_AGENT}, InputSchemaJson: `{"type":"object"}`},
			{OperationId: "send", OperationType: capv1.OperationType_OPERATION_TYPE_MUTATION, RequiredScopes: []string{"product:test:write"}, Audience: []capv1.Audience{capv1.Audience_AUDIENCE_NATIVE_AGENT}, InputSchemaJson: `{"type":"object"}`},
		},
	}
	if err := registry.Register(&Provider{Descriptor: descriptor, Handler: func(context.Context, []byte) ([]byte, error) {
		return []byte(`{"ok":true}`), nil
	}}); err != nil {
		t.Fatal(err)
	}
	return &Server{
		config:   &Config{GrantPublicKey: publicKey, GrantPrivateKey: privateKey, GrantCodec: capv1.GrantCodec{Now: func() time.Time { return now }, MaxTTL: 10 * time.Minute}},
		registry: registry, operations: newOperationStore(nil),
		readSem: make(chan struct{}, 8), mutationSem: make(chan struct{}, 8),
	}, privateKey, now
}

func delegationTestCallContext(now time.Time) *capv1.CallContext {
	return &capv1.CallContext{
		ChainId: uuid.NewString(), RootOperationId: uuid.NewString(),
		Route: capv1.NodeMessage + capv1.RouteSeparator + capv1.NodeAgent + capv1.RouteSeparator + capv1.NodeProduct,
		Hop:   3, DeadlineUnixMs: now.Add(time.Minute).UnixMilli(),
	}
}

func delegationTestParent(t *testing.T, callCtx *capv1.CallContext, privateKey ed25519.PrivateKey, now time.Time) *capv1.PermissionContext {
	t.Helper()
	rootDigest := sha256.Sum256([]byte("agent-root"))
	scopes := []string{"agent:product:execute", "product:test:read", "product:test:write"}
	grant, err := (capv1.GrantCodec{Now: func() time.Time { return now }, MaxTTL: 10 * time.Minute}).Sign(capv1.GrantClaims{
		ChainID: callCtx.ChainId, RootOperationID: callCtx.RootOperationId,
		OwnerID: "owner-1", AccountGeneration: 1, Scopes: scopes,
		RootCapabilityID: "agent.skills.v1", RootOperation: "invoke_product",
		RootRequestDigest: rootDigest[:], CatalogDigest: bytes.Repeat([]byte{1}, sha256.Size), SchemaDigest: bytes.Repeat([]byte{2}, sha256.Size),
		EntryRoute: capv1.NodeMessage, EntryHop: 1, MaxHop: capv1.MaxCallHop, MaxRouteLength: capv1.MaxRouteLength,
		EntryDeadlineUnixMs: callCtx.DeadlineUnixMs,
	}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return &capv1.PermissionContext{AuthenticatedOwnerId: "owner-1", AccountGeneration: 1, GrantedScopes: scopes, CapabilityGrant: grant, RootRequestDigest: rootDigest[:]}
}

func delegationTestContext() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(capv1.CapabilityGenerationMetadata, "1"))
}

func TestProductDelegationExchangeOnlyAcceptsChildAtQueryAndStart(t *testing.T) {
	server, privateKey, now := newDelegationTestServer(t)
	callCtx := delegationTestCallContext(now)
	parent := delegationTestParent(t, callCtx, privateKey, now)
	operationID := uuid.NewString()
	requestJSON := []byte(`{}`)

	resp, err := server.ExchangeProductDelegation(delegationTestContext(), &capv1.ExchangeProductDelegationRequest{
		CallContext: callCtx, ParentPermission: parent,
		CapabilityId: "product.test.v1", Operation: "list", RequestJson: requestJSON,
		TargetKind: capv1.ExchangeProductTargetKind_EXCHANGE_PRODUCT_TARGET_KIND_QUERY,
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp == nil || resp.ProductPermission == nil || bytes.Equal(resp.ProductPermission.CapabilityGrant, parent.CapabilityGrant) {
		t.Fatal("exchange did not return a distinct Product child grant")
	}
	listDescriptor := server.registry.GetMust("product.test.v1").Descriptor
	listOperation := server.registry.OperationMust("product.test.v1", "list")
	listRootDigest, err := rootRequestDigest(listDescriptor, listOperation, requestJSON, 0)
	if err != nil || !bytes.Equal(resp.ProductPermission.RootRequestDigest, listRootDigest) {
		t.Fatalf("child root digest is not bound to the Product request: got=%x want=%x err=%v", resp.ProductPermission.RootRequestDigest, listRootDigest, err)
	}

	query, err := server.Query(delegationTestContext(), &capv1.QueryRequest{
		CallContext: callCtx, Permission: resp.ProductPermission, CapabilityId: "product.test.v1", OperationId: "list", RequestJson: requestJSON,
	})
	if err != nil || query.GetError() != nil || string(query.GetResultJson()) != `{"ok":true}` {
		t.Fatalf("child query failed: response=%#v err=%v", query, err)
	}
	parentQuery, err := server.Query(delegationTestContext(), &capv1.QueryRequest{
		CallContext: callCtx, Permission: parent, CapabilityId: "product.test.v1", OperationId: "list", RequestJson: requestJSON,
	})
	if err != nil || parentQuery.GetError() == nil || parentQuery.GetError().GetCode() != capv1.ErrorCode_ERROR_CODE_TRUST_FAILED {
		t.Fatalf("parent grant was accepted by Product query: response=%#v err=%v", parentQuery, err)
	}

	mutationResp, err := server.ExchangeProductDelegation(delegationTestContext(), &capv1.ExchangeProductDelegationRequest{
		CallContext: callCtx, ParentPermission: parent, ChildOperationId: operationID,
		CapabilityId: "product.test.v1", Operation: "send", RequestJson: requestJSON,
		TargetKind: capv1.ExchangeProductTargetKind_EXCHANGE_PRODUCT_TARGET_KIND_START_OPERATION,
	})
	if err != nil {
		t.Fatalf("mutation exchange: %v", err)
	}
	requestDigest, err := canonicalRequestDigest(&capv1.StartOperationRequest{Permission: mutationResp.ProductPermission}, server.registry.GetMust("product.test.v1").Descriptor, server.registry.OperationMust("product.test.v1", "send"), requestJSON)
	if err != nil {
		t.Fatal(err)
	}
	started, err := server.StartOperation(delegationTestContext(), &capv1.StartOperationRequest{
		CallContext: callCtx, Permission: mutationResp.ProductPermission, OperationId: operationID,
		CapabilityId: "product.test.v1", Operation: "send", RequestJson: requestJSON, RequestDigest: requestDigest,
	})
	if err != nil || started.GetError() != nil || len(started.GetControlGrants()) != 4 {
		t.Fatalf("child start failed: response=%#v err=%v", started, err)
	}

	wrongOperationID := uuid.NewString()
	wrongDigest := requestDigest
	wrongRequest := &capv1.StartOperationRequest{
		CallContext: callCtx, Permission: mutationResp.ProductPermission, OperationId: wrongOperationID,
		CapabilityId: "product.test.v1", Operation: "send", RequestJson: requestJSON, RequestDigest: wrongDigest,
	}
	wrongResponse, err := server.StartOperation(delegationTestContext(), wrongRequest)
	if err != nil || wrongResponse.GetError() == nil || wrongResponse.GetError().GetCode() != capv1.ErrorCode_ERROR_CODE_TRUST_FAILED {
		t.Fatalf("Product accepted child grant under a different child operation id: response=%#v err=%v", wrongResponse, err)
	}
}

func TestProductDelegationRejectsMalformedAndRequestDigestMismatch(t *testing.T) {
	server, privateKey, now := newDelegationTestServer(t)
	if _, err := server.ExchangeProductDelegation(delegationTestContext(), &capv1.ExchangeProductDelegationRequest{}); err == nil {
		t.Fatal("malformed exchange request was accepted")
	}
	callCtx := delegationTestCallContext(now)
	parent := delegationTestParent(t, callCtx, privateKey, now)
	childOperationID := uuid.NewString()
	resp, err := server.ExchangeProductDelegation(delegationTestContext(), &capv1.ExchangeProductDelegationRequest{
		CallContext: callCtx, ParentPermission: parent, ChildOperationId: childOperationID,
		CapabilityId: "product.test.v1", Operation: "send", RequestJson: []byte(`{}`),
		TargetKind: capv1.ExchangeProductTargetKind_EXCHANGE_PRODUCT_TARGET_KIND_START_OPERATION,
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	request := &capv1.StartOperationRequest{
		CallContext: callCtx, Permission: resp.ProductPermission, OperationId: childOperationID,
		CapabilityId: "product.test.v1", Operation: "send", RequestJson: []byte(`{}`), RequestDigest: bytes.Repeat([]byte{0x7f}, sha256.Size),
	}
	started, err := server.StartOperation(delegationTestContext(), request)
	if err != nil {
		t.Fatalf("digest mismatch returned transport error: %v", err)
	}
	if started == nil || started.GetError() == nil || started.GetError().GetCode() != capv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Fatalf("digest mismatch was not rejected: %#v", started)
	}
}

func TestProductDelegationRejectsServerDerivedIdentityBeforeExchange(t *testing.T) {
	server, privateKey, now := newDelegationTestServer(t)
	callCtx := delegationTestCallContext(now)
	parent := delegationTestParent(t, callCtx, privateKey, now)

	_, err := server.ExchangeProductDelegation(delegationTestContext(), &capv1.ExchangeProductDelegationRequest{
		CallContext: callCtx, ParentPermission: parent,
		CapabilityId: "product.test.v1", Operation: "list", RequestJson: []byte(`{"owner_id":"attacker"}`),
		TargetKind: capv1.ExchangeProductTargetKind_EXCHANGE_PRODUCT_TARGET_KIND_QUERY,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("forged owner exchange error = %v, want InvalidArgument", err)
	}
}

func TestProductQueryRejectsServerDerivedIdentityBeforeProvider(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_700_000_000_000)
	invoked := false
	registry, err := NewRegistryWithInvokerChecked(func(context.Context, string, map[string]any) (any, error) {
		invoked = true
		return map[string]any{"contacts": []any{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	codec := capv1.GrantCodec{Now: func() time.Time { return now }, MaxTTL: 10 * time.Minute}
	server := &Server{
		config:   &Config{GrantPublicKey: publicKey, GrantPrivateKey: privateKey, GrantCodec: codec},
		registry: registry, readSem: make(chan struct{}, 1),
	}
	callCtx := delegationTestCallContext(now)
	requestJSON := []byte(`{"query":"alice","owner_mxid":"@attacker:example.test"}`)
	descriptor := registry.GetMust("product.contacts.v1").Descriptor
	operation := registry.OperationMust("product.contacts.v1", "list")
	rootDigest, err := rootRequestDigest(descriptor, operation, requestJSON, 0)
	if err != nil {
		t.Fatal(err)
	}
	parentRootDigest := sha256.Sum256([]byte("agent-root"))
	parentClaims := capv1.GrantClaims{
		ChainID: callCtx.ChainId, RootOperationID: callCtx.RootOperationId,
		GrantKind: capv1.GrantKindRoot,
		OwnerID:   "owner-1", AccountGeneration: 1, Scopes: []string{"agent:product:execute", "product:contacts:read"},
		RootCapabilityID: "agent.skills.v1", RootOperation: "invoke_product",
		RootRequestDigest: parentRootDigest[:], CatalogDigest: bytes.Repeat([]byte{1}, sha256.Size), SchemaDigest: bytes.Repeat([]byte{2}, sha256.Size),
		IssuedAtUnixMs: now.UnixMilli(), ExpiresAtUnixMs: now.Add(2 * time.Minute).UnixMilli(),
		EntryRoute: capv1.NodeMessage, EntryHop: 1, MaxHop: capv1.MaxCallHop, MaxRouteLength: capv1.MaxRouteLength,
		EntryDeadlineUnixMs: callCtx.DeadlineUnixMs,
	}
	schemaDigest := sha256.Sum256([]byte(operation.GetInputSchemaJson()))
	grant, err := codec.SignProductDelegationFromParent(capv1.ProductDelegationIssue{
		ParentClaims: parentClaims, CallContext: callCtx,
		CapabilityID: descriptor.GetCapabilityId(), Operation: operation.GetOperationId(),
		RequiredScopes: operation.GetRequiredScopes(), TargetKind: capv1.ExchangeProductTargetKind_EXCHANGE_PRODUCT_TARGET_KIND_QUERY,
		RootRequestDigest: rootDigest, CatalogDigest: server.catalogDigest(), SchemaDigest: schemaDigest[:],
	}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	permission := &capv1.PermissionContext{
		AuthenticatedOwnerId: "owner-1", AccountGeneration: 1,
		GrantedScopes: operation.GetRequiredScopes(), CapabilityGrant: grant, RootRequestDigest: rootDigest,
	}

	response, err := server.Query(delegationTestContext(), &capv1.QueryRequest{
		CallContext: callCtx, Permission: permission,
		CapabilityId: "product.contacts.v1", OperationId: "list",
		RequestJson: requestJSON,
	})
	if err != nil {
		t.Fatalf("query returned transport error: %v", err)
	}
	if response.GetError().GetCode() != capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("forged owner query error = %#v, want INVALID_ARGUMENT", response.GetError())
	}
	if invoked {
		t.Fatal("forged owner query reached the Product provider")
	}
}

func TestProductInterceptorAdvancesExchangeCallContext(t *testing.T) {
	now := time.Now()
	callCtx := &capv1.CallContext{ChainId: uuid.NewString(), RootOperationId: uuid.NewString(), Route: capv1.NodeMessage + capv1.RouteSeparator + capv1.NodeAgent, Hop: 2, DeadlineUnixMs: now.Add(time.Minute).UnixMilli()}
	advanced, err := capv1.ValidateAndAdvanceProductCallContext(callCtx)
	if err != nil {
		t.Fatal(err)
	}
	req := &capv1.ExchangeProductDelegationRequest{CallContext: callCtx}
	setRequestCallContext(req, advanced)
	if req.GetCallContext().GetRoute() != capv1.NodeMessage+capv1.RouteSeparator+capv1.NodeAgent+capv1.RouteSeparator+capv1.NodeProduct || req.GetCallContext().GetHop() != 3 {
		t.Fatalf("exchange call context was not advanced: %#v", req.GetCallContext())
	}
}

// Small test-only lookup helpers keep the broker test focused on the boundary
// assertions without widening Registry's production API.
func (r *Registry) GetMust(capabilityID string) *Provider {
	provider, ok := r.Get(capabilityID)
	if !ok {
		panic("missing test provider")
	}
	return provider
}

func (r *Registry) OperationMust(capabilityID, operationID string) *capv1.OperationDescriptor {
	operation, ok := r.Operation(capabilityID, operationID)
	if !ok {
		panic("missing test operation")
	}
	return operation
}
