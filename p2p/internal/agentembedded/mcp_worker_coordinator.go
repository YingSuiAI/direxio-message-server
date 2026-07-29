package agentembedded

import (
	"context"
	"encoding/json"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

// PinnedMCPWorkerCoordinator is the single post-claim execution owner. It
// consumes confirmation once, finalizes lifecycle projections, and never
// redispatches a call after an ambiguous provider response.
type PinnedMCPWorkerCoordinator struct {
	Extensions    ext.Store
	Tasks         task.Store
	Confirmations ConfirmationAdapter
	Finalizer     ext.ExecutionFinalizer
	Client        func(string, ext.Installation, ext.Version) *ext.MCPClient
}

func (w *PinnedMCPWorkerCoordinator) RunClaimed(ctx context.Context, owner string, claimed task.Task) error {
	if w == nil || w.Extensions == nil || w.Tasks == nil || w.Finalizer == nil || w.Client == nil || claimed.Spec.Payload.Extension == nil {
		return ext.ErrInvalid
	}
	p := claimed.Spec.Payload.Extension
	if p.ConfirmationID == "" {
		return ext.ErrConflict
	}
	cv, e := w.Confirmations.Get(ctx, owner, p.ConfirmationID)
	if e != nil {
		return e
	}
	i, e := w.Extensions.Get(ctx, owner, p.InstallationID)
	if e != nil {
		return e
	}
	var v ext.Version
	for _, x := range i.Versions {
		if x.VersionID == p.Version {
			v = x
		}
	}
	if v.VersionID == "" || v.ContentDigest != p.Digest {
		return ext.ErrConflict
	}
	if p.Operation != task.ExtensionOperationInstall && p.Operation != task.ExtensionOperationUpdate && p.Operation != task.ExtensionOperationUninstall && p.Operation != task.ExtensionOperationExecuteTool {
		return ext.ErrInvalid
	}
	requestDigest := task.Digest(struct {
		Owner, Install, Version, Tool string
		Input                         json.RawMessage
	}{owner, p.InstallationID, p.Version, p.ToolName, p.CanonicalInputJSON})
	holder := ""
	if claimed.Lease != nil {
		holder = claimed.Lease.Holder
	}
	var client *ext.MCPClient
	if p.Operation == task.ExtensionOperationExecuteTool {
		client = w.Client(owner, i, v)
		if client == nil {
			return ext.ErrUnavailable
		}
	}
	if cv.State == string(coreconfirmation.StateConsumed) {
		if p.Operation == task.ExtensionOperationExecuteTool {
			err := w.Finalizer.FinalizeExecution(ctx, ext.ExecutionFinalizeRequest{
				OwnerID: owner, TaskID: claimed.ID, ConfirmationID: p.ConfirmationID,
				InstallationID: p.InstallationID, VersionID: p.Version,
				RequestDigest: requestDigest, LeaseHolder: holder,
				ErrorCode:    "extension_execution_uncertain",
				ErrorSummary: "previous provider response was lost; execution was not replayed",
				Attempt:      claimed.Attempt, LeaseEpoch: claimed.LeaseEpoch,
				TaskRevision: claimed.Revision, InstallationRevision: p.ExpectedRevision,
				Uncertain: true, ReconcileConsumed: true,
			})
			if err != nil {
				return err
			}
			return ext.ErrUncertain
		}
		return w.finalizeLifecycleRecovery(ctx, claimed, cv, i, v, p.Operation)
	}
	got, e := w.Confirmations.Consume(ctx, ext.ConsumeRequest{OwnerID: owner, ID: p.ConfirmationID, TaskID: claimed.ID, IdempotencyKey: p.ConfirmationID, Attempt: claimed.Attempt, LeaseEpoch: claimed.LeaseEpoch, ExpectedRevision: cv.Revision, ExpectedTaskRevision: int64(claimed.Revision), Binding: cv.Binding})
	if e != nil {
		return e
	}
	if p.Operation == task.ExtensionOperationInstall || p.Operation == task.ExtensionOperationUpdate || p.Operation == task.ExtensionOperationUninstall {
		return w.finalizeLifecycle(ctx, claimed, got, i, v, p.Operation)
	}
	// The confirmation binds the immutable version snapshot, but the remote
	// provider may have changed its advertised tool schema since installation.
	// Re-inspect immediately after consuming the confirmation and before the
	// first tools/call.  A drift (or an unavailable inspection) must close the
	// consumed task fence without issuing an external side effect.
	observed, e := client.ListTools(ctx)
	if e != nil {
		fe := w.Finalizer.FinalizeExecution(ctx, ext.ExecutionFinalizeRequest{
			OwnerID: owner, TaskID: claimed.ID, ConfirmationID: p.ConfirmationID,
			InstallationID: p.InstallationID, VersionID: p.Version,
			RequestDigest: requestDigest, LeaseHolder: holder,
			ErrorCode:    "extension_tool_schema_unavailable",
			ErrorSummary: "remote MCP tool schema could not be revalidated",
			Attempt:      claimed.Attempt, LeaseEpoch: claimed.LeaseEpoch,
			TaskRevision: claimed.Revision, InstallationRevision: p.ExpectedRevision,
		})
		if fe != nil {
			return fe
		}
		return ext.ErrUnavailable
	}
	if len(v.Tools) == 0 || !ext.PinnedToolsEqual(observed, v.Tools) {
		fe := w.Finalizer.FinalizeExecution(ctx, ext.ExecutionFinalizeRequest{
			OwnerID: owner, TaskID: claimed.ID, ConfirmationID: p.ConfirmationID,
			InstallationID: p.InstallationID, VersionID: p.Version,
			RequestDigest: requestDigest, LeaseHolder: holder,
			ErrorCode:    "extension_tool_schema_changed",
			ErrorSummary: "remote MCP tool schema changed after confirmation",
			Attempt:      claimed.Attempt, LeaseEpoch: claimed.LeaseEpoch,
			TaskRevision: claimed.Revision, InstallationRevision: p.ExpectedRevision,
		})
		if fe != nil {
			return fe
		}
		return ext.ErrConflict
	}
	result, e := client.CallTool(ctx, p.ToolName, p.CanonicalInputJSON)
	if e != nil {
		fe := w.Finalizer.FinalizeExecution(ctx, ext.ExecutionFinalizeRequest{OwnerID: owner, TaskID: claimed.ID, ConfirmationID: p.ConfirmationID, InstallationID: p.InstallationID, VersionID: p.Version, RequestDigest: requestDigest, LeaseHolder: holder, ErrorCode: "extension_execution_uncertain", ErrorSummary: "provider outcome is unknown", Attempt: claimed.Attempt, LeaseEpoch: claimed.LeaseEpoch, TaskRevision: claimed.Revision, InstallationRevision: p.ExpectedRevision, Uncertain: true})
		if fe != nil {
			return fe
		}
		return ext.ErrUncertain
	}
	return w.Finalizer.FinalizeExecution(ctx, ext.ExecutionFinalizeRequest{OwnerID: owner, TaskID: claimed.ID, ConfirmationID: p.ConfirmationID, InstallationID: p.InstallationID, VersionID: p.Version, RequestDigest: requestDigest, LeaseHolder: holder, ResultDigest: ext.DigestBytes(result), Attempt: claimed.Attempt, LeaseEpoch: claimed.LeaseEpoch, TaskRevision: claimed.Revision, InstallationRevision: p.ExpectedRevision, Success: true})
}
func (w *PinnedMCPWorkerCoordinator) finalizeLifecycle(ctx context.Context, t task.Task, c ext.Confirmation, i ext.Installation, v ext.Version, op task.ExtensionOperation) error {
	f, ok := w.Finalizer.(ext.LifecycleFinalizer)
	if !ok || f == nil {
		return ext.ErrUnavailable
	}
	holder := ""
	if t.Lease != nil {
		holder = t.Lease.Holder
	}
	requestDigest := task.Digest(struct {
		Owner, Install, Version, Operation string
	}{t.OwnerID, i.ID, v.VersionID, string(op)})
	return f.FinalizeLifecycle(ctx, ext.ExecutionFinalizeRequest{OwnerID: t.OwnerID, TaskID: t.ID, ConfirmationID: c.ID, InstallationID: i.ID, VersionID: v.VersionID, RequestDigest: requestDigest, LeaseHolder: holder, Operation: string(op), Attempt: t.Attempt, LeaseEpoch: t.LeaseEpoch, TaskRevision: t.Revision, InstallationRevision: pRevision(i), Success: true})
}

func (w *PinnedMCPWorkerCoordinator) finalizeLifecycleRecovery(ctx context.Context, t task.Task, c ext.Confirmation, i ext.Installation, v ext.Version, op task.ExtensionOperation) error {
	f, ok := w.Finalizer.(ext.LifecycleFinalizer)
	if !ok || f == nil {
		return ext.ErrUnavailable
	}
	holder := ""
	if t.Lease != nil {
		holder = t.Lease.Holder
	}
	requestDigest := task.Digest(struct {
		Owner, Install, Version, Operation string
	}{t.OwnerID, i.ID, v.VersionID, string(op)})
	return f.FinalizeLifecycle(ctx, ext.ExecutionFinalizeRequest{
		OwnerID: t.OwnerID, TaskID: t.ID, ConfirmationID: c.ID,
		InstallationID: i.ID, VersionID: v.VersionID,
		RequestDigest: requestDigest, LeaseHolder: holder, Operation: string(op),
		Attempt: t.Attempt, LeaseEpoch: t.LeaseEpoch, TaskRevision: t.Revision,
		InstallationRevision: pRevision(i), Success: true, ReconcileConsumed: true,
	})
}

func pRevision(i ext.Installation) uint64 {
	if i.Revision < 1 {
		return 0
	}
	return uint64(i.Revision)
}
func decodeExtensionPayload(raw json.RawMessage) (task.ExtensionTaskPayload, error) {
	var p task.ExtensionTaskPayload
	err := json.Unmarshal(raw, &p)
	return p, err
}
