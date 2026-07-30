package storage

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

const requestPlanID = "11111111-1111-4111-8111-111111111111"
const requestCredentialID = "22222222-2222-4222-8222-222222222222"

type bindingJSONMatcher struct {
	want coreconfirmation.Binding
}

func (m bindingJSONMatcher) Match(value driver.Value) bool {
	raw, ok := value.([]byte)
	if !ok {
		if s, ok := value.(string); ok {
			raw = []byte(s)
		}
	}
	var got coreconfirmation.Binding
	if json.Unmarshal(raw, &got) != nil {
		return false
	}
	return got.Equal(m.want)
}

func requestPlanRow(now time.Time) (agentaws.Plan, []byte, []byte, []byte) {
	template := []byte(`{"Resources":{}}`)
	_, digest, _ := agentaws.NormalizeTemplate(template)
	plan := agentaws.Plan{ID: requestPlanID, CredentialID: requestCredentialID, CredentialRevision: 2, Region: "us-east-1", StackName: "request-stack", Operation: agentaws.OperationCreate, Template: template, TemplateSHA256: digest, Parameters: map[string]string{"InstanceType": "t3.small"}, Tags: map[string]string{"service": agentaws.EC2ServiceProfile}, Capabilities: []string{}, Revision: 1, CreatedAt: now}
	params, _ := json.Marshal(plan.Parameters)
	tags, _ := json.Marshal(plan.Tags)
	caps, _ := json.Marshal(plan.Capabilities)
	return plan, params, tags, caps
}

func expectRequestPlanReads(mock sqlmock.Sqlmock, plan agentaws.Plan, params, tags, caps []byte, now time.Time) {
	planSQL := "SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans"
	row := sqlmock.NewRows([]string{"plan_id", "credential_id", "credential_revision", "region", "stack_name", "operation", "template", "template_sha256", "parameters_json", "tags_json", "capabilities_json", "revision", "created_at"}).AddRow(plan.ID, plan.CredentialID, plan.CredentialRevision, plan.Region, plan.StackName, string(plan.Operation), plan.Template, plan.TemplateSHA256, params, tags, caps, plan.Revision, plan.CreatedAt)
	mock.ExpectQuery(regexp.QuoteMeta(planSQL+" WHERE owner_id=$1 AND plan_id=$2")).WithArgs(testAWSOwnerID, plan.ID).WillReturnRows(row)
}

func expectLockedRequestPlan(mock sqlmock.Sqlmock, plan agentaws.Plan, params, tags, caps []byte) {
	planSQL := "SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans"
	row := sqlmock.NewRows([]string{"plan_id", "credential_id", "credential_revision", "region", "stack_name", "operation", "template", "template_sha256", "parameters_json", "tags_json", "capabilities_json", "revision", "created_at"}).AddRow(plan.ID, plan.CredentialID, plan.CredentialRevision, plan.Region, plan.StackName, string(plan.Operation), plan.Template, plan.TemplateSHA256, params, tags, caps, plan.Revision, plan.CreatedAt)
	mock.ExpectQuery(regexp.QuoteMeta(planSQL+" WHERE owner_id=$1 AND plan_id=$2 FOR UPDATE")).WithArgs(testAWSOwnerID, plan.ID).WillReturnRows(row)
}

func expectRequestCredentialMetadata(mock sqlmock.Sqlmock, plan agentaws.Plan, now time.Time) {
	expectRequestCredentialMetadataWithVerified(mock, plan, now, 2)
}

func expectRequestCredentialMetadataWithVerified(mock sqlmock.Sqlmock, plan agentaws.Plan, now time.Time, verified int64) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT name,region,account_id,user_arn,verified_revision,revision,created_at,updated_at FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$3 FOR SHARE")).WithArgs(testAWSOwnerID, plan.CredentialID, plan.CredentialRevision).WillReturnRows(sqlmock.NewRows([]string{"name", "region", "account_id", "user_arn", "verified_revision", "revision", "created_at", "updated_at"}).AddRow("deploy", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/deploy", verified, int64(2), now, now))
}

func TestPostgresRequestChangeRejectsForgedExpectedBindingBeforeInserts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	plan, params, tags, caps := requestPlanRow(now)
	expectRequestPlanReads(mock, plan, params, tags, caps, now)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).WillReturnError(sql.ErrNoRows)
	expectLockedRequestPlan(mock, plan, params, tags, caps)
	expectRequestCredentialMetadata(mock, plan, now)
	cred := agentaws.RehydrateCredentialMetadata(plan.CredentialID, "deploy", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/deploy", 2, 2, now, now)
	forged := agentaws.BindingForPlan(plan, cred)
	forged.ParameterDigest = coreconfirmation.Digest("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	mock.ExpectRollback()
	if _, err = repo.RequestChange(t.Context(), agentaws.RequestChangeInput{PlanID: plan.ID, Binding: forged, IdempotencyKey: "33333333-3333-4333-8333-333333333333"}); !errors.Is(err, agentaws.ErrRevisionConflict) {
		t.Fatalf("forged binding error = %v, want revision conflict", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRequestChangePersistsCanonicalBindingFromMetadata(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	plan, params, tags, caps := requestPlanRow(now)
	expectRequestPlanReads(mock, plan, params, tags, caps, now)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).WillReturnError(sql.ErrNoRows)
	expectLockedRequestPlan(mock, plan, params, tags, caps)
	expectRequestCredentialMetadata(mock, plan, now)
	cred := agentaws.RehydrateCredentialMetadata(plan.CredentialID, "deploy", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/deploy", 2, 2, now, now)
	want := agentaws.BindingForPlan(plan, cred)
	want.OwnerID = testAWSOwnerID
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_tasks")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmations")).WithArgs(sqlmock.AnyArg(), testAWSOwnerID, plan.ID, plan.Revision, sqlmock.AnyArg(), bindingJSONMatcher{want: want}, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_changes")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	result, err := repo.RequestChange(t.Context(), agentaws.RequestChangeInput{PlanID: plan.ID, IdempotencyKey: "44444444-4444-4444-8444-444444444444"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Confirmation.Binding.Equal(want) {
		t.Fatalf("persisted binding = %#v, want %#v", result.Confirmation.Binding, want)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRequestChangeMapsLiveTargetDuplicateToConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	plan, params, tags, caps := requestPlanRow(now)
	expectRequestPlanReads(mock, plan, params, tags, caps, now)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).WillReturnError(sql.ErrNoRows)
	expectLockedRequestPlan(mock, plan, params, tags, caps)
	expectRequestCredentialMetadata(mock, plan, now)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_tasks")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmations")).WillReturnError(errors.New("pq: duplicate key value violates unique constraint agent_confirmations_live_target_idx"))
	mock.ExpectRollback()
	_, err = repo.RequestChange(t.Context(), agentaws.RequestChangeInput{PlanID: plan.ID, IdempotencyKey: "66666666-6666-4666-8666-666666666666"})
	if !errors.Is(err, agentaws.ErrConflict) {
		t.Fatalf("live target duplicate error = %v, want agentaws.ErrConflict", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRequestChangeRejectsUnverifiedHistoricalCredential(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	plan, params, tags, caps := requestPlanRow(now)
	expectRequestPlanReads(mock, plan, params, tags, caps, now)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).WillReturnError(sql.ErrNoRows)
	expectLockedRequestPlan(mock, plan, params, tags, caps)
	expectRequestCredentialMetadataWithVerified(mock, plan, now, 0)
	mock.ExpectRollback()
	if _, err = repo.RequestChange(t.Context(), agentaws.RequestChangeInput{PlanID: plan.ID, IdempotencyKey: "55555555-5555-4555-8555-555555555555"}); !errors.Is(err, agentaws.ErrConflict) {
		t.Fatalf("unverified historical credential error = %v, want conflict", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
