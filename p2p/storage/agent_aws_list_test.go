package storage

import (
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestPostgresListProvisionsFirstPageUsesCanonicalNilUUID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}

	query := "SELECT provision_id::text,plan_id::text,credential_id::text,credential_revision,region,stack_name,profile,owner_digest,plan_revision,template_sha256,plan_digest,state,revision,COALESCE(create_change_id::text,''),COALESCE(destroy_change_id::text,''),COALESCE(active_change_id::text,''),stack_id,instance_id,public_ip,security_group_id,output_digest,observed_at,reconciliation_required,error_code,error_summary,created_at,updated_at FROM core_aws_ec2_provisions WHERE owner_id=$1 AND ($2='' OR state=$2) AND provision_id>COALESCE(NULLIF($3,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid) ORDER BY provision_id LIMIT $4"
	firstID := "11111111-1111-4111-8111-111111111111"
	secondID := "22222222-2222-4222-8222-222222222222"
	planID := "33333333-3333-4333-8333-333333333333"
	credentialID := "44444444-4444-4444-8444-444444444444"
	templateDigest := strings.Repeat("a", 64)
	planDigest := strings.Repeat("b", 64)
	outputDigest := strings.Repeat("c", 64)
	now := time.Unix(10, 0).UTC()
	columns := []string{"provision_id", "plan_id", "credential_id", "credential_revision", "region", "stack_name", "profile", "owner_digest", "plan_revision", "template_sha256", "plan_digest", "state", "revision", "create_change_id", "destroy_change_id", "active_change_id", "stack_id", "instance_id", "public_ip", "security_group_id", "output_digest", "observed_at", "reconciliation_required", "error_code", "error_summary", "created_at", "updated_at"}
	row := func(id string) []driver.Value {
		return []driver.Value{id, planID, credentialID, int64(1), "us-east-1", "stack", "ec2", "owner-digest", int64(1), templateDigest, planDigest, "active", int64(1), "", "", "", "stack-id", "instance-id", "", "security-group-id", outputDigest, now, false, "", "", now, now}
	}
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(testAWSOwnerID, "", "", 2).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(row(firstID)...).AddRow(row(secondID)...))

	page, err := repo.ListProvisions(t.Context(), testAWSOwnerID, "", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != firstID || page.NextPageToken != firstID {
		t.Fatalf("first page = %#v, want one item and token %q", page, firstID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresListProvisionsExecutesFirstAndNextPages(t *testing.T) {
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	store, err := NewDatabaseStore(
		t.Context(),
		sqlutil.NewConnectionManager(nil, opts),
		&opts,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const (
		owner           = "@aws-list-owner:example.test"
		otherOwner      = "@aws-list-other:example.test"
		credentialID    = "44444444-4444-4444-8444-444444444444"
		otherCredential = "88888888-8888-4888-8888-888888888888"
		firstPlanID     = "33333333-3333-4333-8333-333333333333"
		secondPlanID    = "55555555-5555-4555-8555-555555555555"
		otherPlanID     = "77777777-7777-4777-8777-777777777777"
		firstID         = "11111111-1111-4111-8111-111111111111"
		secondID        = "22222222-2222-4222-8222-222222222222"
		otherID         = "99999999-9999-4999-8999-999999999999"
	)
	now := time.Unix(10, 0).UTC()
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	ownerDigest := "sha256:" + strings.Repeat("c", 64)

	insertCredential := func(ownerID, id string) {
		t.Helper()
		if _, err := store.db.ExecContext(t.Context(), `INSERT INTO core_aws_credentials(
			owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,
			envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at
		) VALUES($1,$2,1,1,1,NULL,NULL,NULL,$3,'test','us-east-1','','',0,$4,$4)`,
			ownerID, id, digestA, now); err != nil {
			t.Fatal(err)
		}
	}
	insertPlan := func(ownerID, planID, credID string) {
		t.Helper()
		if _, err := store.db.ExecContext(t.Context(), `INSERT INTO core_aws_plans(
			owner_id,plan_id,credential_id,credential_revision,region,stack_name,operation,
			template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at
		) VALUES($1,$2,$3,1,'us-east-1',$4,'create',$5,$6,$7,$7,$8,1,$9)`,
			ownerID, planID, credID, "stack-"+planID[:8], []byte("template"), digestA,
			json.RawMessage(`{}`), json.RawMessage(`[]`), now); err != nil {
			t.Fatal(err)
		}
	}
	insertProvision := func(ownerID, provisionID, planID, credID string) {
		t.Helper()
		if _, err := store.db.ExecContext(t.Context(), `INSERT INTO core_aws_ec2_provisions(
			owner_id,provision_id,plan_id,credential_id,credential_revision,region,stack_name,
			profile,owner_digest,plan_revision,template_sha256,plan_digest,state,revision,created_at,updated_at
		) VALUES($1,$2,$3,$4,1,'us-east-1',$5,'ec2',$6,1,$7,$8,'planned',1,$9,$9)`,
			ownerID, provisionID, planID, credID, "stack-"+planID[:8], ownerDigest,
			digestA, digestB, now); err != nil {
			t.Fatal(err)
		}
	}

	insertCredential(owner, credentialID)
	insertCredential(otherOwner, otherCredential)
	insertPlan(owner, firstPlanID, credentialID)
	insertPlan(owner, secondPlanID, credentialID)
	insertPlan(otherOwner, otherPlanID, otherCredential)
	insertProvision(owner, firstID, firstPlanID, credentialID)
	insertProvision(owner, secondID, secondPlanID, credentialID)
	insertProvision(otherOwner, otherID, otherPlanID, otherCredential)

	repo, err := NewAgentAWSRepository(store, owner)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.ListProvisions(t.Context(), owner, "", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].ID != firstID || first.NextPageToken != firstID {
		t.Fatalf("first page = %#v", first)
	}
	next, err := repo.ListProvisions(t.Context(), owner, "", 1, first.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 1 || next.Items[0].ID != secondID || next.NextPageToken != "" {
		t.Fatalf("next page = %#v", next)
	}
}
