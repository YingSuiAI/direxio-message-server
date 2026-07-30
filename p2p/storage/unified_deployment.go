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
func deploymentIDForProvision(ownerID, provisionID string) (string, error) {
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

// DeploymentIDForProvision exposes the stable mapping to the embedding layer
// without exposing SQL or allowing callers to choose a deployment identity.
func DeploymentIDForProvision(ownerID, provisionID string) (string, error) {
	return deploymentIDForProvision(ownerID, provisionID)
}

func ensureCoreDeploymentTx(ctx context.Context, tx *sql.Tx, ownerID, provisionID string, p agentaws.Provision) (string, error) {
	deploymentID, err := deploymentIDForProvision(ownerID, provisionID)
	if err != nil {
		return "", err
	}
	object, _ := json.Marshal(map[string]any{
		"deployment_id": deploymentID,
		"provision_id":  p.ID,
		"plan_id":       p.PlanID,
		"plan_digest":   p.PlanDigest,
		"target_kind":   "AWS_EC2",
		"status":        p.State,
		"revision":      p.Revision,
	})
	_, err = tx.ExecContext(ctx, `INSERT INTO core_deployments(owner_id,deployment_id,provision_id,state,target_kind,revision,object_json,created_at,updated_at)
		VALUES($1,$2,$3,$4,'AWS_EC2',$5,$6,NOW(),NOW())
		ON CONFLICT DO NOTHING`,
		ownerID, deploymentID, provisionID, p.State, p.Revision, object)
	if err != nil {
		return "", err
	}
	var canonicalID string
	if err := tx.QueryRowContext(ctx, `SELECT deployment_id::text FROM core_deployments WHERE owner_id=$1 AND provision_id=$2 FOR UPDATE`, ownerID, provisionID).Scan(&canonicalID); err != nil {
		return "", err
	}
	if canonicalID != deploymentID {
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
	deploymentID, err := deploymentIDForProvision(ownerID, provisionID)
	if err != nil {
		return "", workload.ErrInvalid
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_deployments SET workload_id=$1,state='pending',target_kind=$2,revision=revision+1,object_json=jsonb_set(jsonb_set(object_json,'{workload_id}',to_jsonb($1::text),true),'{target_kind}',to_jsonb($2::text),true),updated_at=NOW()
		WHERE owner_id=$3 AND deployment_id=$4 AND provision_id=$5 AND (workload_id IS NULL OR workload_id=$1)`, workloadID, string(p.TargetKind), ownerID, deploymentID, provisionID)
	if err != nil {
		return "", err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return "", workload.ErrConflict
	}
	_ = provisionOwnerDigest // owner-bound digest is checked by the typed plan validator/provider.
	return deploymentID, nil
}
