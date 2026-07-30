package storage

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
)

// deploymentIDForProvision is deliberately derived from the immutable
// owner/provision identity. A response replay therefore never creates a
// second deployment row, while two owners can safely use the same provision
// UUID in isolated owner namespaces.
func legacyDeploymentIDForProvision(ownerID, provisionID string) (string, error) {
	ownerID = strings.TrimSpace(ownerID)
	provisionID = strings.TrimSpace(provisionID)
	if ownerID == "" || !validAWSUUID(provisionID) {
		return "", errors.New("storage: invalid deployment identity")
	}
	// Keep this byte-for-byte identical to the SQL md5(... )::uuid expression
	// in v106 backfill, so restart/replay paths converge on one identity.
	sum := md5.Sum([]byte("dirextalk:deployment:v1:" + ownerID + ":" + provisionID))
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(sum[0:4]), hex.EncodeToString(sum[4:6]), hex.EncodeToString(sum[6:8]), hex.EncodeToString(sum[8:10]), hex.EncodeToString(sum[10:16])), nil
}

// canonicalPublicUUID changes only RFC UUID version and variant bits. It must
// stay byte-for-byte identical to core_canonical_public_uuid in v108.
func canonicalPublicUUID(raw [16]byte) string {
	raw[6] = (raw[6] & 0x0f) | 0x30
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(raw[0:4]), hex.EncodeToString(raw[4:6]), hex.EncodeToString(raw[6:8]), hex.EncodeToString(raw[8:10]), hex.EncodeToString(raw[10:16]))
}

func publicDeploymentIDForProvision(ownerID, provisionID string) (string, error) {
	ownerID = strings.TrimSpace(ownerID)
	provisionID = strings.TrimSpace(provisionID)
	if ownerID == "" || !validAWSUUID(provisionID) {
		return "", errors.New("storage: invalid deployment identity")
	}
	sum := md5.Sum([]byte("dirextalk:deployment:v1:" + ownerID + ":" + provisionID))
	return canonicalPublicUUID(sum), nil
}

// DeploymentIDForProvision exposes the stable mapping to the embedding layer
// without exposing SQL or allowing callers to choose a deployment identity.
func DeploymentIDForProvision(ownerID, provisionID string) (string, error) {
	return publicDeploymentIDForProvision(ownerID, provisionID)
}

func ensureCoreDeploymentTx(ctx context.Context, tx *sql.Tx, ownerID, provisionID string, p agentaws.Provision) (string, error) {
	deploymentID, err := legacyDeploymentIDForProvision(ownerID, provisionID)
	if err != nil {
		return "", err
	}
	publicDeploymentID, err := publicDeploymentIDForProvision(ownerID, provisionID)
	if err != nil {
		return "", err
	}
	object, _ := json.Marshal(map[string]any{
		"deployment_id": publicDeploymentID,
		"provision_id":  p.ID,
		"plan_id":       p.PlanID,
		"plan_digest":   p.PlanDigest,
		"target_kind":   "AWS_EC2",
		"status":        p.State,
		"revision":      p.Revision,
	})
	_, err = tx.ExecContext(ctx, `INSERT INTO core_deployments(owner_id,deployment_id,public_deployment_id,provision_id,state,target_kind,revision,object_json,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,'AWS_EC2',$6,$7,NOW(),NOW())
		ON CONFLICT DO NOTHING`,
		ownerID, deploymentID, publicDeploymentID, provisionID, p.State, p.Revision, object)
	if err != nil {
		return "", err
	}
	var canonicalID, canonicalPublicID string
	if err := tx.QueryRowContext(ctx, `SELECT deployment_id::text,public_deployment_id::text FROM core_deployments WHERE owner_id=$1 AND provision_id=$2 FOR UPDATE`, ownerID, provisionID).Scan(&canonicalID, &canonicalPublicID); err != nil {
		return "", err
	}
	if canonicalID != deploymentID || canonicalPublicID != publicDeploymentID {
		return "", errors.New("storage: deployment identity conflict")
	}
	return deploymentID, nil
}

// linkWorkloadDeploymentTx binds a typed workload plan to the exact
// owner-scoped provision encoded in its persisted GeoLibre labels. It is
// intentionally exported only for the storage package's parent adapter: the
// caller must already hold the RequestOperation transaction and have inserted
// the workload row, so the FK and CAS are one atomic commit.
func linkWorkloadDeploymentTx(ctx context.Context, tx *sql.Tx, ownerID, workloadID string, p workload.Plan) (string, error) {
	provisionID := strings.TrimSpace(p.Target.Labels["dirextalk:provision-id"])
	if provisionID == "" {
		return "", nil
	}
	if !validAWSUUID(provisionID) || !workload.ValidUUID(workloadID) {
		return "", workload.ErrInvalid
	}
	var provisionPlanID, provisionOwnerDigest, provisionRegion, provisionCredentialID string
	var provisionRevision, provisionPlanRevision, provisionCredentialRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT plan_id::text,owner_digest,region,credential_id::text,credential_revision,revision,plan_revision
		FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2 FOR SHARE`, ownerID, provisionID).
		Scan(&provisionPlanID, &provisionOwnerDigest, &provisionRegion, &provisionCredentialID, &provisionCredentialRevision, &provisionRevision, &provisionPlanRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", workload.ErrNotFound
		}
		return "", err
	}
	if provisionPlanID != p.Target.RequiredInstanceTags["dirextalk:plan-id"] {
		return "", workload.ErrConflict
	}
	if p.Target.Labels["dirextalk:provision-revision"] != fmt.Sprintf("%d", provisionRevision) || provisionPlanRevision < 1 || provisionRegion != p.Target.Region || p.Target.RequiredInstanceTags["owner"] != provisionOwnerDigest {
		return "", workload.ErrRevisionConflict
	}
	if len(p.SecretGrantRefs) != 1 || p.SecretGrantRefs[0].Purpose != "aws_credential" || p.SecretGrantRefs[0].ReferenceID != provisionCredentialID || p.SecretGrantRefs[0].Revision != provisionCredentialRevision {
		return "", workload.ErrConflict
	}
	binding := credentialBinding(ownerID, provisionCredentialID, provisionCredentialRevision)
	if string(p.SecretGrantRefs[0].BindingDigest) != hex.EncodeToString(binding.BindingDigest[:]) {
		return "", workload.ErrConflict
	}
	deploymentID, err := legacyDeploymentIDForProvision(ownerID, provisionID)
	if err != nil {
		return "", workload.ErrInvalid
	}
	var oldWorkloadID, deploymentState, deploymentActual string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(workload_id::text,''),state,COALESCE(actual_json,'{}'::jsonb)::text
		FROM core_deployments WHERE owner_id=$1 AND deployment_id=$2 AND provision_id=$3`, ownerID, deploymentID, provisionID).
		Scan(&oldWorkloadID, &deploymentState, &deploymentActual); err != nil {
		return "", err
	}
	if oldWorkloadID == "" || oldWorkloadID == workloadID {
		result, err := tx.ExecContext(ctx, `UPDATE core_deployments SET workload_id=$1,state='pending',target_kind=$2,revision=revision+1,object_json=jsonb_set(jsonb_set(object_json,'{workload_id}',to_jsonb($1::uuid::text),true),'{target_kind}',to_jsonb($2::text),true),updated_at=NOW()
			WHERE owner_id=$3 AND deployment_id=$4 AND provision_id=$5 AND (workload_id IS NULL OR workload_id=$1)`, workloadID, string(p.TargetKind), ownerID, deploymentID, provisionID)
		if err != nil {
			return "", err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return "", workload.ErrConflict
		}
		return deploymentID, nil
	}
	if (deploymentState != "expired" && deploymentState != "rejected" && deploymentState != "canceled") || (deploymentActual != "{}" && deploymentActual != "null") {
		return "", workload.ErrConflict
	}

	// Match the confirmation terminalizer's operation -> workload ->
	// deployment lock order. Requiring exactly one pre-dispatch operation is
	// intentionally conservative: a workload with any older operation cannot
	// prove that no provider side effect occurred and must reconcile instead.
	operationRows, err := tx.QueryContext(ctx, `SELECT operation_id::text,plan_id::text,plan_digest,target_kind,operation,status,dispatch_state,dispatch_epoch,expected_workload_revision
		FROM core_workload_operations WHERE owner_id=$1 AND workload_id=$2 ORDER BY operation_id FOR UPDATE`, ownerID, oldWorkloadID)
	if err != nil {
		return "", err
	}
	defer operationRows.Close()
	var operationID, operationPlanID, operationPlanDigest, operationTargetKind, operationKind, operationStatus, dispatchState string
	var expectedWorkloadRevision, dispatchEpoch int64
	operationCount := 0
	for operationRows.Next() {
		operationCount++
		if operationCount > 1 {
			return "", workload.ErrConflict
		}
		if err = operationRows.Scan(&operationID, &operationPlanID, &operationPlanDigest, &operationTargetKind, &operationKind, &operationStatus, &dispatchState, &dispatchEpoch, &expectedWorkloadRevision); err != nil {
			return "", err
		}
	}
	if err = operationRows.Err(); err != nil {
		return "", err
	}
	if err = operationRows.Close(); err != nil {
		return "", err
	}
	if operationCount != 1 || operationID == "" || operationTargetKind != string(p.TargetKind) || operationKind != string(workload.OperationApply) ||
		dispatchEpoch != 0 || dispatchState != "terminal" ||
		(operationStatus != "expired" && operationStatus != "rejected" && operationStatus != "canceled") {
		return "", workload.ErrConflict
	}

	var oldState, oldPlanID, oldPlanDigest, oldTargetKind, actualRaw string
	var oldRevision int64
	if err = tx.QueryRowContext(ctx, `SELECT state,plan_id::text,plan_digest,target_kind,COALESCE(actual_snapshot_json,'{}'::jsonb)::text,revision
		FROM core_workloads WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`, ownerID, oldWorkloadID).
		Scan(&oldState, &oldPlanID, &oldPlanDigest, &oldTargetKind, &actualRaw, &oldRevision); err != nil {
		return "", err
	}
	var operationCountAfterWorkloadLock int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM core_workload_operations WHERE owner_id=$1 AND workload_id=$2`, ownerID, oldWorkloadID).
		Scan(&operationCountAfterWorkloadLock); err != nil {
		return "", err
	}
	if operationCountAfterWorkloadLock != 1 ||
		(oldState != "failed" && oldState != "pending") ||
		oldPlanID != operationPlanID || oldPlanDigest != operationPlanDigest || oldTargetKind != operationTargetKind ||
		(oldState == "failed" && oldRevision != expectedWorkloadRevision+1) ||
		(oldState == "pending" && oldRevision != expectedWorkloadRevision) ||
		(actualRaw != "{}" && actualRaw != "null") {
		return "", workload.ErrConflict
	}

	var lockedWorkloadID, lockedDeploymentState, lockedDeploymentActual string
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(workload_id::text,''),state,COALESCE(actual_json,'{}'::jsonb)::text
		FROM core_deployments WHERE owner_id=$1 AND deployment_id=$2 AND provision_id=$3 FOR UPDATE`, ownerID, deploymentID, provisionID).
		Scan(&lockedWorkloadID, &lockedDeploymentState, &lockedDeploymentActual); err != nil {
		return "", err
	}
	if lockedWorkloadID != oldWorkloadID || lockedDeploymentState != deploymentState || lockedDeploymentActual != deploymentActual {
		return "", workload.ErrConflict
	}
	if oldState == "pending" {
		result, err := tx.ExecContext(ctx, `UPDATE core_workloads SET state='failed',revision=revision+1,updated_at=NOW()
			WHERE owner_id=$1 AND workload_id=$2 AND state='pending' AND revision=$3
			  AND (actual_snapshot_json IS NULL OR actual_snapshot_json='{}'::jsonb OR actual_snapshot_json='null'::jsonb)`, ownerID, oldWorkloadID, expectedWorkloadRevision)
		if err != nil {
			return "", err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return "", workload.ErrConflict
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_deployments SET workload_id=$1,state='pending',target_kind=$2,revision=revision+1,object_json=jsonb_set(jsonb_set(object_json,'{workload_id}',to_jsonb($1::uuid::text),true),'{target_kind}',to_jsonb($2::text),true),updated_at=NOW()
		WHERE owner_id=$3 AND deployment_id=$4 AND provision_id=$5 AND workload_id=$6
		  AND state=$7 AND COALESCE(actual_json,'{}'::jsonb)::text=$8`, workloadID, string(p.TargetKind), ownerID, deploymentID, provisionID, oldWorkloadID, deploymentState, deploymentActual)
	if err != nil {
		return "", err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return "", workload.ErrConflict
	}
	_ = provisionOwnerDigest // owner-bound digest is checked by the typed plan validator/provider.
	return deploymentID, nil
}
