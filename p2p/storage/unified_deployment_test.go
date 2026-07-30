package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

func TestDeploymentIDForProvisionIsOwnerScopedAndReplayStable(t *testing.T) {
	const provision = "77777777-7777-4777-8777-777777777777"
	a, err := DeploymentIDForProvision("@owner:example.test", provision)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeploymentIDForProvision("@owner:example.test", provision)
	if err != nil || a != b {
		t.Fatalf("mapping is not replay stable: %q %q %v", a, b, err)
	}
	c, err := DeploymentIDForProvision("@other:example.test", provision)
	if err != nil || a == c {
		t.Fatalf("mapping is not owner scoped: %q %q %v", a, c, err)
	}
	if _, err := DeploymentIDForProvision("@owner:example.test", "not-a-uuid"); err == nil {
		t.Fatal("invalid provision identity accepted")
	}
}

func TestPublicDeploymentIDIsCanonicalRFCUUIDWhileLegacyKeyIsPreserved(t *testing.T) {
	const owner = "@owner:example.test"
	const provision = "77777777-7777-4777-8777-777777777777"
	legacy, err := legacyDeploymentIDForProvision(owner, provision)
	if err != nil {
		t.Fatal(err)
	}
	public, err := DeploymentIDForProvision(owner, provision)
	if err != nil {
		t.Fatal(err)
	}
	if public[14] != '3' || !strings.ContainsRune("89ab", rune(public[19])) {
		t.Fatalf("public deployment ID is not a canonical RFC UUID: %q", public)
	}
	if legacy == public {
		t.Fatalf("legacy and public identities unexpectedly alias: %q", legacy)
	}
	raw := [16]byte{0, 1, 2, 3, 4, 5, 0xff, 7, 0xff, 9, 10, 11, 12, 13, 14, 15}
	if got, want := canonicalPublicUUID(raw), "00010203-0405-3f07-bf09-0a0b0c0d0e0f"; got != want {
		t.Fatalf("canonical UUID bytes = %q, want %q", got, want)
	}
}

type unifiedDeploymentScannerFixture struct{ destroyed bool }

func (f unifiedDeploymentScannerFixture) Scan(dest ...any) error {
	state := "ready"
	if f.destroyed {
		state = "destroyed"
	}
	values := []any{
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "cccccccc-cccc-4ccc-8ccc-cccccccccccc", state, "AWS_EC2", int64(3), []byte(`{"deployment_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`), []byte(`{}`), "active", "sha256:out", time.Unix(10, 0).UTC(),
		int64(2), "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "sha256:ok", "ready", []byte(`{"state":"ready","identity":{"endpoint":"http://203.0.113.9"},"applied_plan_digest":"sha256:ok","readback_digest":"sha256:ok"}`),
		"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "apply", int64(1), "plan", "ffffffff-ffff-4fff-8fff-ffffffffffff", "11111111-1111-4111-8111-111111111111", "succeeded", int64(2), "", time.Unix(1, 0).UTC(), time.Unix(10, 0).UTC(), int64(1), nil,
		"aaaaaaaa-aaaa-faaa-faaa-aaaaaaaaaaaa",
	}
	for i := range dest {
		switch ptr := dest[i].(type) {
		case *string:
			*ptr = values[i].(string)
		case *int64:
			*ptr = values[i].(int64)
		case *[]byte:
			*ptr = values[i].([]byte)
		case *time.Time:
			*ptr = values[i].(time.Time)
		case interface{ Scan(any) error }:
			_ = ptr
		}
	}
	return nil
}

func TestScanUnifiedDeploymentGatesUnverifiedEndpoint(t *testing.T) {
	object, _, _, err := scanUnifiedDeployment(unifiedDeploymentScannerFixture{})
	if err != nil {
		t.Fatal(err)
	}
	actual := object["actual"].(map[string]any)
	identity := actual["identity"].(map[string]any)
	if identity["endpoint"] != "http://203.0.113.9" {
		t.Fatalf("verified endpoint was removed: %#v", identity)
	}
	if object["deployment_id"] == "" || object["workload_id"] == "" {
		t.Fatalf("unified identifiers missing: %#v", object)
	}
}

func TestScanUnifiedDeploymentKeepsDestroyedStatusAndHidesEndpoint(t *testing.T) {
	object, _, _, err := scanUnifiedDeployment(unifiedDeploymentScannerFixture{destroyed: true})
	if err != nil {
		t.Fatal(err)
	}
	if object["status"] != "destroyed" {
		t.Fatalf("destroyed status was overwritten: %#v", object)
	}
	actual := object["actual"].(map[string]any)
	identity := actual["identity"].(map[string]any)
	if _, ok := identity["endpoint"]; ok {
		t.Fatalf("destroyed endpoint leaked: %#v", identity)
	}
}

func TestLinkWorkloadDeploymentChecksTypedProvisionAndCredential(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner = "@owner:example.test"
	const provisionID = "77777777-7777-4777-8777-777777777777"
	const credentialID = "55555555-5555-4555-8555-555555555555"
	const awsPlanID = "88888888-8888-4888-8888-888888888888"
	const workloadID = "99999999-9999-4999-8999-999999999999"
	ownerSum := sha256.Sum256([]byte("owner"))
	ownerDigest := "sha256:" + hex.EncodeToString(ownerSum[:])
	p := workload.Plan{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", TargetKind: workload.TargetAWSEC2SSM, Target: workload.TargetSettings{Region: "us-east-1", RequiredInstanceTags: map[string]string{"dirextalk:plan-id": awsPlanID, "owner": ownerDigest}, Labels: map[string]string{"dirextalk:provision-id": provisionID, "dirextalk:provision-revision": "2"}}, SecretGrantRefs: []workload.SecretGrantRef{{ReferenceID: credentialID, Purpose: "aws_credential", Revision: 4}}}
	binding := credentialBinding(owner, credentialID, 4)
	p.SecretGrantRefs[0].BindingDigest = coreconfirmation.Digest(stringDigestHex(binding.BindingDigest[:]))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_id::text,owner_digest,region,credential_id::text,credential_revision,revision,plan_revision")).WithArgs(owner, provisionID).WillReturnRows(sqlmock.NewRows([]string{"plan_id", "owner_digest", "region", "credential_id", "credential_revision", "revision", "plan_revision"}).AddRow(awsPlanID, ownerDigest, "us-east-1", credentialID, 4, 2, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(workload_id::text,''),state,COALESCE(actual_json,'{}'::jsonb)::text")).
		WithArgs(owner, sqlmock.AnyArg(), provisionID).
		WillReturnRows(sqlmock.NewRows([]string{"workload_id", "state", "actual"}).AddRow("", "active", "{}"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_deployments SET workload_id=")).WithArgs(workloadID, string(p.TargetKind), owner, sqlmock.AnyArg(), provisionID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = linkWorkloadDeploymentTx(context.Background(), tx, owner, workloadID, p); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLinkWorkloadDeploymentRebindsOnlyFailedPreDispatchWorkload(t *testing.T) {
	const owner = "@owner:example.test"
	const provisionID = "77777777-7777-4777-8777-777777777777"
	const credentialID = "55555555-5555-4555-8555-555555555555"
	const awsPlanID = "88888888-8888-4888-8888-888888888888"
	const workloadID = "99999999-9999-4999-8999-999999999999"
	const oldWorkloadID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const oldPlanID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	const oldPlanDigest = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	ownerSum := sha256.Sum256([]byte("owner"))
	ownerDigest := "sha256:" + hex.EncodeToString(ownerSum[:])
	p := workload.Plan{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Digest: strings.Repeat("f", 64), TargetKind: workload.TargetAWSEC2SSM, Target: workload.TargetSettings{Region: "us-east-1", RequiredInstanceTags: map[string]string{"dirextalk:plan-id": awsPlanID, "owner": ownerDigest}, Labels: map[string]string{"dirextalk:provision-id": provisionID, "dirextalk:provision-revision": "2"}}, SecretGrantRefs: []workload.SecretGrantRef{{ReferenceID: credentialID, Purpose: "aws_credential", Revision: 4}}}
	binding := credentialBinding(owner, credentialID, 4)
	p.SecretGrantRefs[0].BindingDigest = coreconfirmation.Digest(stringDigestHex(binding.BindingDigest[:]))
	legacyID, err := legacyDeploymentIDForProvision(owner, provisionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, state, opStatus, dispatchState, actual string
		dispatchEpoch                                int64
		extraOperation, wantRebind                   bool
	}{
		{name: "safe expired", state: "failed", opStatus: "expired", dispatchState: "terminal", actual: "{}", wantRebind: true},
		{name: "lazy repair expired", state: "pending", opStatus: "expired", dispatchState: "terminal", actual: "{}", wantRebind: true},
		{name: "running unsafe", state: "ready", opStatus: "running", dispatchState: "dispatched", actual: "{}"},
		{name: "uncertain unsafe", state: "failed", opStatus: "uncertain", dispatchState: "uncertain", actual: "{}"},
		{name: "dispatched terminal unsafe", state: "failed", opStatus: "expired", dispatchState: "terminal", dispatchEpoch: 1, actual: "{}"},
		{name: "readback unsafe", state: "failed", opStatus: "expired", dispatchState: "terminal", actual: `{"state":"ready"}`},
		{name: "older operation unsafe", state: "failed", opStatus: "expired", dispatchState: "terminal", actual: "{}", extraOperation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_id::text,owner_digest,region,credential_id::text,credential_revision,revision,plan_revision")).WithArgs(owner, provisionID).WillReturnRows(sqlmock.NewRows([]string{"plan_id", "owner_digest", "region", "credential_id", "credential_revision", "revision", "plan_revision"}).AddRow(awsPlanID, ownerDigest, "us-east-1", credentialID, 4, 2, 1))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(workload_id::text,''),state,COALESCE(actual_json,'{}'::jsonb)::text")).
				WithArgs(owner, legacyID, provisionID).
				WillReturnRows(sqlmock.NewRows([]string{"workload_id", "state", "actual"}).AddRow(oldWorkloadID, "expired", "{}"))
			operationRows := sqlmock.NewRows([]string{"operation_id", "plan_id", "plan_digest", "target_kind", "operation", "status", "dispatch_state", "dispatch_epoch", "expected_workload_revision"}).
				AddRow("cccccccc-cccc-4ccc-8ccc-cccccccccccc", oldPlanID, oldPlanDigest, string(p.TargetKind), "apply", tc.opStatus, tc.dispatchState, tc.dispatchEpoch, 1)
			if tc.extraOperation {
				operationRows.AddRow("ffffffff-ffff-4fff-8fff-ffffffffffff", oldPlanID, oldPlanDigest, string(p.TargetKind), "apply", "failed", "terminal", 1, 1)
			}
			mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,plan_id::text,plan_digest,target_kind,operation,status,dispatch_state,dispatch_epoch,expected_workload_revision")).
				WithArgs(owner, oldWorkloadID).
				WillReturnRows(operationRows)
			operationSafe := !tc.extraOperation && tc.opStatus == "expired" && tc.dispatchState == "terminal" && tc.dispatchEpoch == 0
			oldRevision := int64(2)
			if tc.state == "pending" {
				oldRevision = 1
			}
			if operationSafe {
				mock.ExpectQuery(regexp.QuoteMeta("SELECT state,plan_id::text,plan_digest,target_kind,COALESCE(actual_snapshot_json,'{}'::jsonb)::text,revision")).
					WithArgs(owner, oldWorkloadID).
					WillReturnRows(sqlmock.NewRows([]string{"state", "plan_id", "plan_digest", "target_kind", "actual", "revision"}).AddRow(tc.state, oldPlanID, oldPlanDigest, string(p.TargetKind), tc.actual, oldRevision))
				mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM core_workload_operations")).
					WithArgs(owner, oldWorkloadID).
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
			}
			workloadSafe := operationSafe && (tc.actual == "{}" || tc.actual == "null")
			if workloadSafe {
				mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(workload_id::text,''),state,COALESCE(actual_json,'{}'::jsonb)::text")).
					WithArgs(owner, legacyID, provisionID).
					WillReturnRows(sqlmock.NewRows([]string{"workload_id", "state", "actual"}).AddRow(oldWorkloadID, "expired", "{}"))
			}
			if workloadSafe && tc.state == "pending" {
				mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workloads SET state='failed',revision=revision+1,updated_at=NOW()")).WithArgs(owner, oldWorkloadID, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			if tc.wantRebind {
				mock.ExpectExec(regexp.QuoteMeta("UPDATE core_deployments SET workload_id=")).WithArgs(workloadID, string(p.TargetKind), owner, legacyID, provisionID, oldWorkloadID, "expired", "{}").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectRollback()
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, gotErr := linkWorkloadDeploymentTx(context.Background(), tx, owner, workloadID, p)
			if tc.wantRebind && gotErr != nil {
				t.Fatalf("safe rebind error = %v", gotErr)
			}
			if !tc.wantRebind && !errors.Is(gotErr, workload.ErrConflict) {
				t.Fatalf("unsafe rebind error = %v", gotErr)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func stringDigestHex(raw []byte) string { return strings.ToLower(hex.EncodeToString(raw)) }
