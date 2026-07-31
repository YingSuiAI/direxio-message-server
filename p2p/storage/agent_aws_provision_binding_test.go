package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
)

func TestPostgresCreateProvisionRejectsForgedPlanSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	built, err := agentaws.BuildEC2ProvisionPlan(agentaws.EC2ProvisionRequest{
		OwnerID: "@owner:example.test", CredentialID: "55555555-5555-4555-8555-555555555555", CredentialRevision: 2,
		Region: "us-east-1", StackName: "geolibre-prod", DisplayName: "geolibre-prod", InstanceType: "t3.small", VolumeGiB: 20, AcknowledgePublicExposure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := built.CorePlan()
	p := agentaws.Provision{ID: "77777777-7777-4777-8777-777777777777", PlanID: plan.ID, CredentialID: plan.CredentialID, CredentialRevision: plan.CredentialRevision, Region: plan.Region, StackName: plan.StackName, Profile: plan.Tags["service"], OwnerDigest: plan.Tags["owner"], PlanRevision: plan.Revision, TemplateSHA256: plan.TemplateSHA256, PlanDigest: agentaws.PlanDigest(plan), State: "planned", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	p.PlanDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	params, _ := json.Marshal(plan.Parameters)
	tags, _ := json.Marshal(plan.Tags)
	caps, _ := json.Marshal(plan.Capabilities)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans")).
		WithArgs(testAWSOwnerID, plan.ID).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "credential_id", "credential_revision", "region", "stack_name", "operation", "template", "template_sha256", "parameters_json", "tags_json", "capabilities_json", "revision", "created_at"}).
			AddRow(plan.ID, plan.CredentialID, plan.CredentialRevision, plan.Region, plan.StackName, string(plan.Operation), plan.Template, plan.TemplateSHA256, params, tags, caps, plan.Revision, plan.CreatedAt))
	mock.ExpectRollback()
	if _, err := repo.CreateProvision(t.Context(), p); !errors.Is(err, agentaws.ErrRevisionConflict) {
		t.Fatalf("forged plan digest error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRetryProvisionFailedDeleteRestoresActiveReadbackAndReplays(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	planID := "88888888-8888-4888-8888-888888888888"
	credentialID := "55555555-5555-4555-8555-555555555555"
	provisionID := "77777777-7777-4777-8777-777777777777"
	changeID := "99999999-9999-4999-8999-999999999999"
	key := "66666666-6666-4666-8666-666666666666"
	now := time.Now().UTC().Truncate(time.Microsecond)
	template := []byte(`{"Resources":{}}`)
	_, templateDigest, _ := agentaws.NormalizeTemplate(template)
	plan := agentaws.Plan{ID: planID, CredentialID: credentialID, CredentialRevision: 2, Region: "us-east-1", StackName: "geolibre-prod", Operation: agentaws.OperationCreate, Template: template, TemplateSHA256: templateDigest, Parameters: map[string]string{}, Tags: map[string]string{"service": agentaws.EC2ServiceProfile, "owner": agentaws.OwnerBindingDigest("@owner:example.test")}, Capabilities: []string{}, Revision: 1, CreatedAt: now}
	planDigest := agentaws.PlanDigest(plan)
	readback, _ := agentaws.ProvisionReadbackFromStack(agentaws.StackOutputs{"StackId": "stack", "InstanceId": "i-1", "SecurityGroupId": "sg-1"}, now)
	rawPlanParams, _ := json.Marshal(plan.Parameters)
	rawPlanTags, _ := json.Marshal(plan.Tags)
	rawPlanCaps, _ := json.Marshal(plan.Capabilities)
	provisionCols := []string{"provision_id", "plan_id", "credential_id", "credential_revision", "region", "stack_name", "profile", "owner_digest", "plan_revision", "template_sha256", "plan_digest", "state", "revision", "create_change_id", "destroy_change_id", "stack_id", "instance_id", "public_ip", "security_group_id", "output_digest", "observed_at", "reconciliation_required", "error_code", "error_summary", "created_at", "updated_at"}
	provisionRow := sqlmock.NewRows(provisionCols).AddRow(provisionID, planID, credentialID, 2, plan.Region, plan.StackName, agentaws.EC2ServiceProfile, plan.Tags["owner"], 1, templateDigest, planDigest, "failed", 4, "", changeID, readback.StackID, readback.InstanceID, readback.PublicIP, readback.SecurityGroupID, readback.OutputDigest, readback.ObservedAt, false, "provider_error", "delete failed", now, now)
	expectCall := func() {
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).WillReturnError(sql.ErrNoRows)
		mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT provision_id::text,plan_id::text,credential_id::text,credential_revision,region,stack_name,profile,owner_digest,plan_revision,template_sha256,plan_digest,state,revision,COALESCE(create_change_id::text,''),COALESCE(destroy_change_id::text,''),stack_id,instance_id,public_ip,security_group_id,output_digest,observed_at,reconciliation_required,error_code,error_summary,created_at,updated_at FROM core_aws_ec2_provisions")).WillReturnRows(provisionRow)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans")).WillReturnRows(sqlmock.NewRows([]string{"plan_id", "credential_id", "credential_revision", "region", "stack_name", "operation", "template", "template_sha256", "parameters_json", "tags_json", "capabilities_json", "revision", "created_at"}).AddRow(planID, credentialID, 2, plan.Region, plan.StackName, "create", template, templateDigest, rawPlanParams, rawPlanTags, rawPlanCaps, 1, now))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT provision_id::text,operation,status FROM core_aws_changes")).WillReturnRows(sqlmock.NewRows([]string{"provision_id", "operation", "status"}).AddRow(provisionID, "delete", "failed"))
		mock.ExpectExec(regexp.QuoteMeta("UPDATE core_aws_ec2_provisions SET state='active',active_change_id=NULL,reconciliation_required=false,error_code='',error_summary=''")).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_aws_ec2_provision_event_counters")).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(5))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_ec2_provision_events")).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json)")).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
	}
	expectCall()
	first, err := repo.RetryProvision(t.Context(), provisionID, 4, key)
	if err != nil || first.State != "active" || first.Readback != readback || first.Revision != 5 {
		t.Fatalf("failed delete retry: %#v %v", first, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(first)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_json"}).AddRow(stringDigest(struct {
		ProvisionID string
		Revision    int64
	}{provisionID, 4}), raw))
	mock.ExpectCommit()
	second, err := repo.RetryProvision(t.Context(), provisionID, 4, key)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("retry replay mismatch: first=%#v second=%#v err=%v", first, second, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRetryProvisionRecreateSelectsCurrentFailedCreateOverStaleDestroy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	planID, credentialID := "88888888-8888-4888-8888-888888888888", "55555555-5555-4555-8555-555555555555"
	provisionID, createID, destroyID, key := "77777777-7777-4777-8777-777777777777", "99999999-9999-4999-8999-999999999999", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "66666666-6666-4666-8666-666666666666"
	now := time.Now().UTC().Truncate(time.Microsecond)
	template := []byte(`{"Resources":{}}`)
	_, templateDigest, _ := agentaws.NormalizeTemplate(template)
	plan := agentaws.Plan{ID: planID, CredentialID: credentialID, CredentialRevision: 2, Region: "us-east-1", StackName: "geolibre-prod", Operation: agentaws.OperationCreate, Template: template, TemplateSHA256: templateDigest, Parameters: map[string]string{}, Tags: map[string]string{"service": agentaws.EC2ServiceProfile, "owner": agentaws.OwnerBindingDigest("@owner:example.test")}, Revision: 1, CreatedAt: now}
	params, _ := json.Marshal(plan.Parameters)
	tags, _ := json.Marshal(plan.Tags)
	caps, _ := json.Marshal(plan.Capabilities)
	pdigest := agentaws.PlanDigest(plan)
	cols := []string{"provision_id", "plan_id", "credential_id", "credential_revision", "region", "stack_name", "profile", "owner_digest", "plan_revision", "template_sha256", "plan_digest", "state", "revision", "create_change_id", "destroy_change_id", "stack_id", "instance_id", "public_ip", "security_group_id", "output_digest", "observed_at", "reconciliation_required", "error_code", "error_summary", "created_at", "updated_at"}
	prow := sqlmock.NewRows(cols).AddRow(provisionID, planID, credentialID, 2, plan.Region, plan.StackName, agentaws.EC2ServiceProfile, plan.Tags["owner"], 1, templateDigest, pdigest, "failed", 4, createID, destroyID, "", "", "", "", "", nil, false, "provider_error", "create failed", now, now)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT provision_id::text,plan_id::text,credential_id::text,credential_revision,region,stack_name,profile,owner_digest,plan_revision,template_sha256,plan_digest,state,revision,COALESCE(create_change_id::text,''),COALESCE(destroy_change_id::text,''),stack_id,instance_id,public_ip,security_group_id,output_digest,observed_at,reconciliation_required,error_code,error_summary,created_at,updated_at FROM core_aws_ec2_provisions")).WillReturnRows(prow)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans")).WillReturnRows(sqlmock.NewRows([]string{"plan_id", "credential_id", "credential_revision", "region", "stack_name", "operation", "template", "template_sha256", "parameters_json", "tags_json", "capabilities_json", "revision", "created_at"}).AddRow(planID, credentialID, 2, plan.Region, plan.StackName, "create", template, templateDigest, params, tags, caps, 1, now))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT provision_id::text,operation,status FROM core_aws_changes")).WillReturnRows(sqlmock.NewRows([]string{"provision_id", "operation", "status"}).AddRow(provisionID, "create", "failed"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT provision_id::text,operation,status FROM core_aws_changes")).WillReturnRows(sqlmock.NewRows([]string{"provision_id", "operation", "status"}).AddRow(provisionID, "delete", "succeeded"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_aws_ec2_provisions SET state='planned',active_change_id=NULL,stack_id='',instance_id='',public_ip='',security_group_id='',output_digest='',observed_at=NULL")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_aws_ec2_provision_event_counters")).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(5))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_ec2_provision_events")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	first, err := repo.RetryProvision(t.Context(), provisionID, 4, key)
	if err != nil || first.State != "planned" || first.Revision != 5 {
		t.Fatalf("stale destroy shadowed failed create: %#v %v", first, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(first)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_json"}).AddRow(stringDigest(struct {
		ProvisionID string
		Revision    int64
	}{provisionID, 4}), raw))
	mock.ExpectCommit()
	second, err := repo.RetryProvision(t.Context(), provisionID, 4, key)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("recreate replay mismatch: first=%#v second=%#v err=%v", first, second, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRetryProvisionRejectsCrossProvisionFailedIntent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	planID, credentialID := "88888888-8888-4888-8888-888888888888", "55555555-5555-4555-8555-555555555555"
	provisionID, changeID := "77777777-7777-4777-8777-777777777777", "99999999-9999-4999-8999-999999999999"
	now := time.Now().UTC().Truncate(time.Microsecond)
	template := []byte(`{"Resources":{}}`)
	_, templateDigest, _ := agentaws.NormalizeTemplate(template)
	plan := agentaws.Plan{ID: planID, CredentialID: credentialID, CredentialRevision: 2, Region: "us-east-1", StackName: "geolibre-prod", Operation: agentaws.OperationCreate, Template: template, TemplateSHA256: templateDigest, Parameters: map[string]string{}, Tags: map[string]string{"service": agentaws.EC2ServiceProfile, "owner": agentaws.OwnerBindingDigest("@owner:example.test")}, Revision: 1, CreatedAt: now}
	params, _ := json.Marshal(plan.Parameters)
	tags, _ := json.Marshal(plan.Tags)
	caps, _ := json.Marshal(plan.Capabilities)
	cols := []string{"provision_id", "plan_id", "credential_id", "credential_revision", "region", "stack_name", "profile", "owner_digest", "plan_revision", "template_sha256", "plan_digest", "state", "revision", "create_change_id", "destroy_change_id", "stack_id", "instance_id", "public_ip", "security_group_id", "output_digest", "observed_at", "reconciliation_required", "error_code", "error_summary", "created_at", "updated_at"}
	prow := sqlmock.NewRows(cols).AddRow(provisionID, planID, credentialID, 2, plan.Region, plan.StackName, agentaws.EC2ServiceProfile, plan.Tags["owner"], 1, templateDigest, agentaws.PlanDigest(plan), "failed", 4, changeID, "", "", "", "", "", "", nil, false, "provider_error", "create failed", now, now)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT provision_id::text,plan_id::text,credential_id::text,credential_revision,region,stack_name,profile,owner_digest,plan_revision,template_sha256,plan_digest,state,revision,COALESCE(create_change_id::text,''),COALESCE(destroy_change_id::text,''),stack_id,instance_id,public_ip,security_group_id,output_digest,observed_at,reconciliation_required,error_code,error_summary,created_at,updated_at FROM core_aws_ec2_provisions")).WillReturnRows(prow)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans")).WillReturnRows(sqlmock.NewRows([]string{"plan_id", "credential_id", "credential_revision", "region", "stack_name", "operation", "template", "template_sha256", "parameters_json", "tags_json", "capabilities_json", "revision", "created_at"}).AddRow(planID, credentialID, 2, plan.Region, plan.StackName, "create", template, templateDigest, params, tags, caps, 1, now))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT provision_id::text,operation,status FROM core_aws_changes")).WillReturnRows(sqlmock.NewRows([]string{"provision_id", "operation", "status"}).AddRow("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "create", "failed"))
	mock.ExpectRollback()
	if _, err := repo.RetryProvision(t.Context(), provisionID, 4, "66666666-6666-4666-8666-666666666666"); !errors.Is(err, agentaws.ErrRevisionConflict) {
		t.Fatalf("cross-provision intent error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
