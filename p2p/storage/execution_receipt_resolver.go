package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

// DatabaseDispatchReceiptResolver reconstructs a frozen request only from
// the immutable intent snapshot and the exact credential revision.
type DatabaseDispatchReceiptResolver struct {
	Store       *DatabaseExecutionStore
	Credentials CredentialRevisionResolver
}

func NewDatabaseDispatchReceiptResolver(store *DatabaseExecutionStore, credentials CredentialRevisionResolver) *DatabaseDispatchReceiptResolver {
	if store == nil || credentials == nil {
		return nil
	}
	return &DatabaseDispatchReceiptResolver{Store: store, Credentials: credentials}
}

func (r *DatabaseDispatchReceiptResolver) ResolveDispatchReceipt(ctx context.Context, owner string, fence coreexecution.Digest) (coreaws.DispatchReceipt, error) {
	var out coreaws.DispatchReceipt
	if r == nil || r.Store == nil || r.Credentials == nil || strings.TrimSpace(owner) == "" || !fence.Valid() {
		return out, ErrExecutionStoreInvalid
	}
	var receiptRaw, intentRaw []byte
	var status, commandID string
	err := r.Store.db.QueryRowContext(ctx, `SELECT r.snapshot_json,r.status,COALESCE(r.command_id,''),COALESCE(i.snapshot_json,'{}'::jsonb) FROM core_execution_receipts r LEFT JOIN core_execution_dispatch_intents i ON i.owner_id=r.owner_id AND i.receipt_id=r.receipt_id WHERE r.owner_id=$1 AND r.fence_digest=$2`, owner, fence).Scan(&receiptRaw, &status, &commandID, &intentRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, coreexecution.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	var envelope struct {
		Frozen coreaws.FrozenRequestSnapshot `json:"frozen_request_snapshot"`
	}
	if len(intentRaw) == 0 || json.Unmarshal(intentRaw, &envelope) != nil || envelope.Frozen.OwnerID != owner || envelope.Frozen.FenceDigest != fence || !envelope.Frozen.RequestDigest.Valid() {
		return out, ErrExecutionStoreDrift
	}
	s := envelope.Frozen
	if len(s.Target.CredentialRefs) == 0 || s.CredentialID == "" || s.CredentialRevision == 0 {
		return out, ErrExecutionStoreDrift
	}
	var ref coreexecution.CredentialRef
	for _, candidate := range s.Target.CredentialRefs {
		if candidate.Ref == s.CredentialID && candidate.Revision == s.CredentialRevision {
			ref = candidate
			break
		}
	}
	if ref.Ref == "" {
		return out, ErrExecutionStoreDrift
	}
	cred, err := r.Credentials.ResolveCredentialRevision(ctx, owner, s.CredentialID, s.CredentialRevision)
	if err != nil || cred.AccountID != s.CredentialAccountID || cred.Region != s.CredentialRegion || cred.UserARN != s.CredentialUserARN {
		return out, ErrExecutionStoreDrift
	}
	bound, err := coreaws.CredentialBindingDigest(owner, ref, cred)
	if err != nil || bound != ref.BindingDigest {
		return out, ErrExecutionStoreDrift
	}
	if s.Script.Artifact.ID == "" || s.Script.Artifact.Digest == "" || strings.TrimSpace(s.Script.Artifact.URI) != "" {
		return out, ErrExecutionStoreDrift
	}
	meta, err := r.Store.GetArtifactMetadata(ctx, owner, s.Script.Artifact.ID)
	if err != nil || meta.Status != "available" || meta.StorageBackend != "filesystem" || meta.ContentDigest != s.Script.Artifact.Digest || meta.SizeBytes != s.Script.Artifact.Size {
		return out, ErrExecutionStoreDrift
	}
	frozen := coreaws.FrozenRequest{OwnerID: s.OwnerID, PlanID: s.PlanID, PlanRevision: s.PlanRevision, PlanDigest: s.PlanDigest, RunID: s.RunID, RunRevision: s.RunRevision, RunDigest: s.RunDigest, StageID: s.StageID, StageRevision: s.StageRevision, StageDigest: s.StageDigest, StepKey: s.StepKey, StepRevision: s.StepRevision, StepDigest: s.StepDigest, AttemptID: s.AttemptID, Attempt: s.Attempt, Fence: s.Fence, FenceDigest: s.FenceDigest, RequestDigest: s.RequestDigest, Target: s.Target, TargetID: s.TargetID, TargetRevision: s.TargetRevision, TargetDigest: s.TargetDigest, InstanceID: s.InstanceID, Credential: cred, CredentialID: s.CredentialID, CredentialRevision: s.CredentialRevision, Observation: s.Observation, Script: s.Script}
	if frozen.TargetID != frozen.Target.ID || frozen.TargetRevision != frozen.Target.Revision || frozen.TargetDigest != frozen.Target.Digest || frozen.Observation.TargetID != frozen.TargetID || frozen.Observation.TargetRevision != frozen.TargetRevision {
		return out, ErrExecutionStoreDrift
	}
	fd, err := coreaws.CanonicalFenceDigest(frozen)
	if err != nil || fd != fence {
		return out, ErrExecutionStoreDrift
	}
	rd, err := coreaws.CanonicalRequestDigest(frozen)
	if err != nil || rd != frozen.RequestDigest {
		return out, ErrExecutionStoreDrift
	}
	if strings.TrimSpace(commandID) == "" || strings.ContainsAny(commandID, "\r\n\x00") || (status != "accepted" && status != "running" && status != "uncertain") {
		return out, ErrExecutionStoreDrift
	}
	if err := json.Unmarshal(receiptRaw, &struct{}{}); err != nil {
		return out, ErrExecutionStoreDrift
	}
	dispatchStatus := coreaws.DispatchAccepted
	if status == string(coreexecution.ReceiptUncertain) {
		dispatchStatus = coreaws.DispatchUncertain
	}
	out = coreaws.DispatchReceipt{Frozen: frozen, RequestDigest: frozen.RequestDigest, FenceDigest: frozen.FenceDigest, CommandID: commandID, InstanceID: frozen.InstanceID, Status: dispatchStatus}
	return out, nil
}

var _ coreaws.DispatchReceiptResolver = (*DatabaseDispatchReceiptResolver)(nil)
