package agentembedded

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	extx "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

type MCPActionPortDependencies struct {
	Service       *extx.Service
	Tasks         task.Store
	Confirmations confirmation.Repository
	Runner        func(context.Context, task.Task, extx.Confirmation) error
	Client        func(string, extx.Installation, extx.Version) *extx.MCPClient
	Catalog       MCPCatalog
	Atomic        bool
}

// NewPinnedMCPClientFactory binds every HTTP client to the installation and
// immutable version snapshot supplied by the store. Stdio or malformed
// endpoints return nil and therefore cannot become a capability.
func NewPinnedMCPClientFactory(resolve func(string, string, string) extx.SecretResolver) func(string, extx.Installation, extx.Version) *extx.MCPClient {
	return func(owner string, installation extx.Installation, version extx.Version) *extx.MCPClient {
		if version.Execution.Remote == nil || installation.ID == "" || version.VersionID == "" || resolve == nil {
			return nil
		}
		binding := ""
		for _, grant := range version.SecretGrants {
			if grant.ReferenceID == version.Execution.Remote.CredentialReferenceID {
				binding = grant.BindingDigest
				break
			}
		}
		if binding == "" {
			return nil
		}
		secret := resolve(owner, installation.ID, version.VersionID)
		if secret == nil {
			return nil
		}
		return &extx.MCPClient{OwnerID: owner, InstallationID: installation.ID, VersionID: version.VersionID, BindingDigest: binding, Endpoint: *version.Execution.Remote, Secret: secret}
	}
}

// NewReadyMCPActionPort fails closed unless task, confirmation and execution
// seams are all present. This prevents publishing a half-wired MCP surface.
func NewReadyMCPActionPort(d MCPActionPortDependencies) (ActionPort, error) {
	atomic, ok := d.ServiceStoreAtomic()
	_, finalizerOK := func() (extx.ExecutionFinalizer, bool) {
		if d.Service == nil || d.Service.Store == nil {
			return nil, false
		}
		v, yes := d.Service.Store.(extx.ExecutionFinalizer)
		return v, yes
	}()
	_, lifecycleFinalizerOK := func() (extx.LifecycleFinalizer, bool) {
		if d.Service == nil || d.Service.Store == nil {
			return nil, false
		}
		v, yes := d.Service.Store.(extx.LifecycleFinalizer)
		return v, yes
	}()
	_, toolPinnerOK := func() (extx.ToolPinner, bool) {
		if d.Service == nil || d.Service.Store == nil {
			return nil, false
		}
		v, yes := d.Service.Store.(extx.ToolPinner)
		return v, yes
	}()
	if d.Service == nil || d.Service.Store == nil || (d.Service.SecretWriter == nil && !(ok && atomic)) || d.Tasks == nil || d.Confirmations == nil || d.Runner == nil || d.Client == nil || d.Catalog == nil || !d.Atomic || !ok || !atomic || !finalizerOK || !lifecycleFinalizerOK || !toolPinnerOK {
		return nil, ErrUnavailable
	}
	d.Service.Tasks = TaskAdapter{Store: d.Tasks, Runner: d.Runner}
	d.Service.Confirmations = ConfirmationAdapter{Repository: d.Confirmations}
	port := NewMCPActionPort(d.Service, d.Client)
	port.Catalog = d.Catalog
	return port, nil
}

func (d MCPActionPortDependencies) ServiceStoreAtomic() (bool, bool) {
	if d.Service == nil || d.Service.Store == nil {
		return false, false
	}
	a, ok := d.Service.Store.(extx.AtomicLifecycleStore)
	if !ok {
		return false, false
	}
	return a.SupportsAtomicLifecycle(), true
}

// NewPinnedMCPWorker returns the worker callback used after confirmation
// consumption. It re-loads the immutable installation/version snapshot and
// refuses to rediscover or execute a different digest.
func NewPinnedMCPWorker(store extx.Store, factory func(string, extx.Installation, extx.Version) *extx.MCPClient) func(context.Context, task.Task, extx.Confirmation) error {
	return func(ctx context.Context, t task.Task, c extx.Confirmation) error {
		if store == nil || factory == nil || t.Spec.Payload.Extension == nil {
			return extx.ErrInvalid
		}
		p := t.Spec.Payload.Extension
		if p.Operation != task.ExtensionOperationExecuteTool {
			return extx.ErrInvalid
		}
		i, e := store.Get(ctx, t.OwnerID, p.InstallationID)
		if e != nil {
			return e
		}
		var v extx.Version
		for _, x := range i.Versions {
			if x.VersionID == p.Version {
				v = x
			}
		}
		if v.VersionID == "" || v.ContentDigest != p.Digest || c.Binding.VersionID != v.VersionID || c.Binding.ContentDigest != v.ContentDigest || c.Binding.ExecutionDigest != v.ExecutionDigest || c.Binding.ManifestDigest != v.ManifestDigest || c.Binding.NetworkDigest != v.NetworkDigest || c.Binding.SecretDigest != v.SecretDigest {
			return extx.ErrConflict
		}
		if p.ExpectedRevision == 0 || int64(p.ExpectedRevision) != i.Revision {
			return extx.ErrConflict
		}
		canonical, e := extx.CanonicalizeInput(p.CanonicalInputJSON)
		if e != nil || !reflect.DeepEqual(canonical, p.CanonicalInputJSON) {
			return extx.ErrConflict
		}
		expected, e := extx.ExecutionBinding(t.OwnerID, i, v, p.ToolName, canonical)
		if e != nil || !expected.Equal(c.Binding) {
			return extx.ErrConflict
		}
		client := factory(t.OwnerID, i, v)
		if client == nil {
			return extx.ErrUnavailable
		}
		observed, e := client.ListTools(ctx)
		if e != nil {
			return e
		}
		if len(v.Tools) == 0 || !reflect.DeepEqual(observed, v.Tools) {
			return extx.ErrConflict
		}
		raw := p.CanonicalInputJSON
		if len(raw) == 0 {
			raw = []byte(`{}`)
		}
		_, e = client.CallTool(ctx, p.ToolName, raw)
		return e
	}
}

// ConfirmationAdapter bridges the canonical confirmation repository without
// exposing confirmation-domain types through the embedded MCP boundary.
type ConfirmationAdapter struct{ Repository confirmation.Repository }

func (a ConfirmationAdapter) Binding(ctx context.Context, owner, id string) (extx.ConfirmationBinding, error) {
	if a.Repository == nil {
		return extx.ConfirmationBinding{}, ErrUnavailable
	}
	c, e := a.Repository.Get(ctx, id)
	if e != nil {
		return extx.ConfirmationBinding{}, e
	}
	if c.OwnerID != "" && c.OwnerID != owner {
		return extx.ConfirmationBinding{}, extx.ErrConflict
	}
	return fromConfirmation(c).Binding, nil
}
func (a ConfirmationAdapter) Get(ctx context.Context, owner, id string) (extx.Confirmation, error) {
	if a.Repository == nil {
		return extx.Confirmation{}, ErrUnavailable
	}
	c, e := a.Repository.Get(ctx, id)
	if e != nil {
		return extx.Confirmation{}, e
	}
	if c.OwnerID != "" && c.OwnerID != owner {
		return extx.Confirmation{}, extx.ErrConflict
	}
	return fromConfirmation(c), nil
}

func (a ConfirmationAdapter) Request(ctx context.Context, r extx.ConfirmationRequest) (extx.Confirmation, error) {
	if a.Repository == nil {
		return extx.Confirmation{}, ErrUnavailable
	}
	b := toConfirmationBinding(r.Binding)
	v, e := a.Repository.Request(ctx, confirmation.RequestCommand{OwnerID: r.OwnerID, IdempotencyKey: r.IdempotencyKey, TaskID: r.TaskID, Binding: b, ExpiresAt: r.ExpiresAt, At: time.Now().UTC()})
	if e != nil {
		return extx.Confirmation{}, e
	}
	return fromConfirmation(v), nil
}
func (a ConfirmationAdapter) Consume(ctx context.Context, r extx.ConsumeRequest) (extx.Confirmation, error) {
	if a.Repository == nil {
		return extx.Confirmation{}, ErrUnavailable
	}
	v, e := a.Repository.Consume(ctx, confirmation.ConsumeCommand{OwnerID: r.OwnerID, ConfirmationID: r.ID, IdempotencyKey: r.IdempotencyKey, TaskID: r.TaskID, Attempt: r.Attempt, LeaseEpoch: r.LeaseEpoch, ExpectedRevision: r.ExpectedRevision, ExpectedTaskRevision: r.ExpectedTaskRevision, Binding: toConfirmationBinding(r.Binding), At: time.Now().UTC()})
	if e != nil {
		return extx.Confirmation{}, e
	}
	return fromConfirmation(v), nil
}
func toConfirmationBinding(b extx.ConfirmationBinding) confirmation.Binding {
	operation := extensionOperation(b.Operation)
	g := make([]confirmation.SecretGrant, 0, len(b.SecretGrants))
	for _, x := range b.SecretGrants {
		g = append(g, confirmation.SecretGrant{ReferenceID: x.ReferenceID, Purpose: confirmation.SecretPurpose(x.Purpose), BindingDigest: confirmation.Digest(x.BindingDigest)})
	}
	return confirmation.Binding{OwnerID: b.OwnerID, OperationDomain: "extension." + operation, TargetID: b.TargetID, TargetRevision: b.TargetRevision, TargetKind: "mcp", ExtensionVersionID: b.VersionID, SourceVersion: b.SourceVersion, SourceCommit: b.SourceCommit, ContentDigest: confirmation.Digest(b.ContentDigest), ManifestDigest: confirmation.Digest(b.ManifestDigest), ExecutionDigest: confirmation.Digest(b.ExecutionDigest), PermissionDigest: confirmation.Digest(b.ToolSchemaDigest), ParameterDigest: confirmation.Digest(b.ParameterDigest), NetworkDigest: confirmation.Digest(b.NetworkDigest), SecretGrantDigest: confirmation.Digest(b.SecretDigest), SelectedTool: b.ToolName, NetworkGrants: append([]string(nil), b.NetworkGrants...), SecretGrants: g}
}
func fromConfirmation(v confirmation.Confirmation) extx.Confirmation {
	g := make([]extx.SecretGrant, 0, len(v.Binding.SecretGrants))
	for _, grant := range v.Binding.SecretGrants {
		g = append(g, extx.SecretGrant{ReferenceID: grant.ReferenceID, Purpose: string(grant.Purpose), BindingDigest: string(grant.BindingDigest), Configured: true})
	}
	return extx.Confirmation{ID: v.ConfirmationID, OwnerID: v.OwnerID, TaskID: v.TaskID, State: string(v.State), Revision: v.Revision, ExpiresAt: v.ExpiresAt, Binding: extx.ConfirmationBinding{OwnerID: v.Binding.OwnerID, Operation: extensionOperation(v.Binding.OperationDomain), TargetID: v.Binding.TargetID, VersionID: v.Binding.ExtensionVersionID, TargetRevision: v.Binding.TargetRevision, SourceVersion: v.Binding.SourceVersion, SourceCommit: v.Binding.SourceCommit, ToolName: v.Binding.SelectedTool, ToolSchemaDigest: string(v.Binding.PermissionDigest), ContentDigest: string(v.Binding.ContentDigest), ManifestDigest: string(v.Binding.ManifestDigest), ExecutionDigest: string(v.Binding.ExecutionDigest), ParameterDigest: string(v.Binding.ParameterDigest), NetworkDigest: string(v.Binding.NetworkDigest), SecretDigest: string(v.Binding.SecretGrantDigest), NetworkGrants: append([]string(nil), v.Binding.NetworkGrants...), SecretGrants: g}}
}

func extensionOperation(value string) string {
	value = strings.TrimSpace(value)
	for strings.HasPrefix(value, "extension.") {
		value = strings.TrimPrefix(value, "extension.")
	}
	return value
}

// TaskAdapter creates a canonical extension task in waiting_user state. Run
// is delegated to an explicitly supplied runner; without it readiness fails.
type TaskAdapter struct {
	Store  task.Store
	Runner func(context.Context, task.Task, extx.Confirmation) error
	Holder string
}

func (a TaskAdapter) CreateWaitingUser(ctx context.Context, r extx.TaskRequest) (extx.Task, error) {
	if a.Store == nil {
		return extx.Task{}, ErrUnavailable
	}
	now := time.Now().UTC()
	var payload task.ExtensionTaskPayload
	_ = json.Unmarshal(r.Payload, &payload)
	spec := task.TaskSpec{Kind: task.TaskKindExtension, Goal: r.Goal, Payload: task.TaskPayload{Extension: &payload}, IdempotencyKey: r.IdempotencyKey, AvailableAt: now}
	d := task.Digest(spec)
	v, e := a.Store.CreateTask(ctx, task.CreateTaskCommand{OwnerID: r.OwnerID, Spec: spec, Mutation: task.MutationCommand{IdempotencyKey: r.IdempotencyKey, RequestDigest: d}})
	if e != nil {
		return extx.Task{}, e
	}
	holder := a.Holder
	if holder == "" {
		holder = "embedded-extension"
	}
	claimed, lease, e := a.Store.ClaimTask(ctx, task.ClaimCommand{OwnerID: r.OwnerID, TaskID: v.ID, Holder: holder, ExpectedRevision: v.Revision, LeaseEpoch: v.LeaseEpoch + 1, LeaseTTL: time.Minute, At: now})
	if e != nil {
		return extx.Task{}, e
	}
	if e = a.Store.WaitTask(ctx, task.WaitUserCommand{Fence: task.Fence{TaskID: claimed.ID, Attempt: claimed.Attempt, LeaseEpoch: lease.Epoch, ExpectedRevision: claimed.Revision}, Reason: "confirmation_required", At: now}); e != nil {
		return extx.Task{}, e
	}
	v, e = a.Store.GetTask(ctx, v.ID)
	if e != nil {
		return extx.Task{}, e
	}
	return extx.Task{ID: v.ID, OwnerID: v.OwnerID, Status: string(v.Status), Revision: v.Revision}, nil
}
func (a TaskAdapter) Run(ctx context.Context, t extx.Task, c extx.Confirmation) error {
	if a.Runner == nil {
		return ErrUnavailable
	}
	v, e := a.Store.GetTask(ctx, t.ID)
	if e != nil {
		return e
	}
	return a.Runner(ctx, v, c)
}
func (a TaskAdapter) MarkUncertain(ctx context.Context, t extx.Task, reason string) error {
	v, e := a.Store.GetTask(ctx, t.ID)
	if e != nil {
		return e
	}
	if v.Lease == nil {
		return extx.ErrUncertain
	}
	return a.Store.FailTask(ctx, task.FailCommand{Fence: task.Fence{TaskID: v.ID, Attempt: v.Attempt, LeaseEpoch: v.LeaseEpoch, ExpectedRevision: v.Revision}, ErrorCode: "extension_execution_uncertain", ErrorSummary: reason, At: time.Now().UTC()})
}

func fromConfirmationError(e error) error {
	if errors.Is(e, confirmation.ErrNotFound) {
		return extx.ErrNotFound
	}
	return e
}
