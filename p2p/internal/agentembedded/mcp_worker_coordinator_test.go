package agentembedded

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

type coordinatorExtensionStore struct {
	extensions.Store
	installation extensions.Installation
}

func (s coordinatorExtensionStore) Get(context.Context, string, string) (extensions.Installation, error) {
	return s.installation, nil
}

type coordinatorConfirmationRepository struct {
	coreconfirmation.Repository
	pending, consumed coreconfirmation.Confirmation
	consumeCalls      int
}

func (r *coordinatorConfirmationRepository) Get(context.Context, string) (coreconfirmation.Confirmation, error) {
	return r.pending, nil
}

func (r *coordinatorConfirmationRepository) Consume(context.Context, coreconfirmation.ConsumeCommand) (coreconfirmation.Confirmation, error) {
	r.consumeCalls++
	return r.consumed, nil
}

type coordinatorFinalizer struct {
	calls int
	last  extensions.ExecutionFinalizeRequest
}

func (f *coordinatorFinalizer) FinalizeExecution(_ context.Context, r extensions.ExecutionFinalizeRequest) error {
	f.calls++
	f.last = r
	return nil
}

type coordinatorSecretResolver struct{}

func (coordinatorSecretResolver) Resolve(context.Context, string, string, string, string, string) ([]byte, error) {
	return []byte("token"), nil
}

type coordinatorRoundTripper struct {
	toolCalls int
}

func (r *coordinatorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var rpc struct {
		ID     uint64 `json:"id"`
		Method string `json:"method"`
	}
	if err := json.NewDecoder(req.Body).Decode(&rpc); err != nil {
		return nil, err
	}
	body := "{}"
	status := http.StatusOK
	contentType := "application/json"
	switch rpc.Method {
	case "initialize":
		body = `{"jsonrpc":"2.0","id":` + jsonNumberForCoordinator(rpc.ID) + `,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}}`
	case "notifications/initialized":
		status = http.StatusAccepted
	case "tools/list":
		// This schema intentionally differs from the pinned version below.
		body = `{"jsonrpc":"2.0","id":` + jsonNumberForCoordinator(rpc.ID) + `,"result":{"tools":[{"name":"echo","description":"echo","inputSchema":{"type":"string"}}]}}`
	case "tools/call":
		r.toolCalls++
		body = `{"jsonrpc":"2.0","id":` + jsonNumberForCoordinator(rpc.ID) + `,"result":{"content":[]}}`
	default:
		return nil, errors.New("unexpected MCP method: " + rpc.Method)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func jsonNumberForCoordinator(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type coordinatorExecuteFixture struct {
	worker        *PinnedMCPWorkerCoordinator
	claimed       task.Task
	confirmations *coordinatorConfirmationRepository
	finalizer     *coordinatorFinalizer
	transport     *coordinatorRoundTripper
}

func newCoordinatorExecuteFixture(t *testing.T) coordinatorExecuteFixture {
	t.Helper()
	const owner = "owner"
	const installationID = "00000000-0000-0000-0000-000000000001"
	const versionID = "00000000-0000-0000-0000-000000000002"
	const confirmationID = "00000000-0000-0000-0000-000000000003"
	contentDigest := strings.Repeat("a", 64)
	bindingDigest := strings.Repeat("b", 64)
	pinnedSchema := json.RawMessage(`{"type":"object"}`)
	version := extensions.Version{
		VersionID: versionID, ContentDigest: contentDigest,
		ManifestDigest: contentDigest, ExecutionDigest: contentDigest,
		NetworkDigest: contentDigest, SecretDigest: contentDigest,
		Execution: extensions.Execution{Remote: &extensions.Endpoint{URL: "https://8.8.8.8/mcp", CredentialReferenceID: "00000000-0000-0000-0000-000000000004"}},
		Tools:     []extensions.Tool{{Name: "echo", Description: "echo", InputSchema: pinnedSchema, InputSchemaDigest: extensions.DigestBytes(pinnedSchema)}},
	}
	installation := extensions.Installation{ID: installationID, OwnerID: owner, Revision: 1, ActiveVersionID: versionID, Versions: []extensions.Version{version}}
	expectedBinding, err := extensions.ExecutionBinding(owner, installation, version, "echo", []byte(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	pending := coreconfirmation.Confirmation{ConfirmationID: confirmationID, OwnerID: owner, TaskID: "task", State: coreconfirmation.StateConfirmed, Revision: 1, Binding: toConfirmationBinding(expectedBinding)}
	consumed := pending
	consumed.State = coreconfirmation.StateConsumed
	finalizer := &coordinatorFinalizer{}
	transport := &coordinatorRoundTripper{}
	client := &extensions.MCPClient{
		OwnerID: owner, InstallationID: installationID, VersionID: versionID,
		BindingDigest: bindingDigest, Endpoint: *version.Execution.Remote,
		Secret: coordinatorSecretResolver{}, HTTP: &http.Client{Transport: transport},
	}
	claimed := task.Task{
		OwnerID: owner, ID: "task", Attempt: 1, LeaseEpoch: 1, Revision: 3,
		Lease: &task.Lease{Holder: "worker"},
		Spec: task.TaskSpec{Kind: task.TaskKindExtension, Payload: task.TaskPayload{Extension: &task.ExtensionTaskPayload{
			Operation: task.ExtensionOperationExecuteTool, InstallationID: installationID,
			ExpectedRevision: 1, Version: versionID, Digest: contentDigest,
			ConfirmationID: confirmationID, ToolName: "echo", CanonicalInputJSON: json.RawMessage(`{"a":1,"b":2}`),
		}}},
	}
	confirmations := &coordinatorConfirmationRepository{pending: pending, consumed: consumed}
	worker := &PinnedMCPWorkerCoordinator{
		Extensions:    coordinatorExtensionStore{installation: installation},
		Confirmations: ConfirmationAdapter{Repository: confirmations},
		Finalizer:     finalizer,
		Client:        func(string, extensions.Installation, extensions.Version) *extensions.MCPClient { return client },
	}
	return coordinatorExecuteFixture{worker: worker, claimed: claimed, confirmations: confirmations, finalizer: finalizer, transport: transport}
}

func TestPinnedMCPWorkerCoordinatorRejectsToolSchemaDriftBeforeCall(t *testing.T) {
	fixture := newCoordinatorExecuteFixture(t)

	err := fixture.worker.RunClaimed(context.Background(), "owner", fixture.claimed)
	if !errors.Is(err, extensions.ErrConflict) {
		t.Fatalf("RunClaimed error = %v, want schema conflict", err)
	}
	if fixture.transport.toolCalls != 0 {
		t.Fatalf("tools/call count = %d, want 0 after schema drift", fixture.transport.toolCalls)
	}
	if fixture.finalizer.calls != 1 || fixture.finalizer.last.ErrorCode != "extension_tool_schema_changed" || fixture.finalizer.last.Uncertain {
		t.Fatalf("finalizer request = %#v, want deterministic schema-drift finalization", fixture.finalizer.last)
	}
}

func TestPinnedMCPWorkerCoordinatorRejectsTamperedInputBeforeConsume(t *testing.T) {
	fixture := newCoordinatorExecuteFixture(t)
	fixture.claimed.Spec.Payload.Extension.CanonicalInputJSON = json.RawMessage(`{"x":2}`)
	err := fixture.worker.RunClaimed(context.Background(), "owner", fixture.claimed)
	if !errors.Is(err, extensions.ErrConflict) {
		t.Fatalf("RunClaimed error = %v, want binding conflict", err)
	}
	if fixture.confirmations.consumeCalls != 0 || fixture.finalizer.calls != 0 || fixture.transport.toolCalls != 0 {
		t.Fatalf("tampered input crossed confirmation/provider boundary: consume=%d finalize=%d calls=%d", fixture.confirmations.consumeCalls, fixture.finalizer.calls, fixture.transport.toolCalls)
	}
}

func TestPinnedMCPWorkerCoordinatorRejectsNonCanonicalInputBeforeConsume(t *testing.T) {
	for _, raw := range []string{` {"a":1,"b":2} `, `{"b":2,"a":1}`} {
		t.Run(raw, func(t *testing.T) {
			fixture := newCoordinatorExecuteFixture(t)
			fixture.claimed.Spec.Payload.Extension.CanonicalInputJSON = json.RawMessage(raw)
			err := fixture.worker.RunClaimed(context.Background(), "owner", fixture.claimed)
			if !errors.Is(err, extensions.ErrConflict) {
				t.Fatalf("RunClaimed error = %v, want canonical-input conflict", err)
			}
			if fixture.confirmations.consumeCalls != 0 || fixture.finalizer.calls != 0 || fixture.transport.toolCalls != 0 {
				t.Fatalf("noncanonical input crossed confirmation/provider boundary: consume=%d finalize=%d calls=%d", fixture.confirmations.consumeCalls, fixture.finalizer.calls, fixture.transport.toolCalls)
			}
		})
	}
}

func TestPinnedMCPWorkerCoordinatorRejectsSchemaBindingMismatchBeforeConsume(t *testing.T) {
	fixture := newCoordinatorExecuteFixture(t)
	fixture.confirmations.pending.Binding.PermissionDigest = coreconfirmation.Digest(strings.Repeat("c", 64))
	err := fixture.worker.RunClaimed(context.Background(), "owner", fixture.claimed)
	if !errors.Is(err, extensions.ErrConflict) {
		t.Fatalf("RunClaimed error = %v, want schema binding conflict", err)
	}
	if fixture.confirmations.consumeCalls != 0 || fixture.finalizer.calls != 0 || fixture.transport.toolCalls != 0 {
		t.Fatalf("schema mismatch crossed confirmation/provider boundary: consume=%d finalize=%d calls=%d", fixture.confirmations.consumeCalls, fixture.finalizer.calls, fixture.transport.toolCalls)
	}
}
