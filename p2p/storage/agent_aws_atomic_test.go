package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
)

const (
	testAWSOwnerID        = "@owner:example.test"
	testAWSChangeID       = "11111111-1111-4111-8111-111111111111"
	testAWSTaskID         = "22222222-2222-4222-8222-222222222222"
	testAWSConfirmationID = "33333333-3333-4333-8333-333333333333"
)

func testAWSProviderMutationCommand() agentaws.ProviderMutationCommand {
	return agentaws.ProviderMutationCommand{
		ChangeID:                     testAWSChangeID,
		ConfirmationID:               testAWSConfirmationID,
		TaskID:                       testAWSTaskID,
		Attempt:                      2,
		LeaseEpoch:                   7,
		ExpectedChangeRevision:       4,
		ExpectedTaskRevision:         9,
		ExpectedConfirmationRevision: 3,
		Kind:                         agentaws.ProviderMutationCreate,
		OperationKey:                 "66666666-6666-4666-8666-666666666666",
	}
}

func TestPostgresAWSListsRejectMalformedUUIDPageTokens(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string]func() error{
		"credentials": func() error {
			_, err := repo.ListCredentials(t.Context(), 20, "not-a-uuid")
			return err
		},
		"plans": func() error {
			_, err := repo.ListPlans(t.Context(), 20, "not-a-uuid")
			return err
		},
		"changes_page": func() error {
			_, err := repo.ListChanges(t.Context(), 20, "", "not-a-uuid")
			return err
		},
		"changes_plan": func() error {
			_, err := repo.ListChanges(t.Context(), 20, "not-a-uuid", "")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := list(); !errors.Is(err, agentaws.ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestPostgresAWSCommitProviderMutationUsesAtomicFence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := testAWSProviderMutationCommand()
	reservation, _ := json.Marshal(map[string]any{
		"task_id":       cmd.TaskID,
		"attempt":       cmd.Attempt,
		"lease_epoch":   cmd.LeaseEpoch,
		"task_revision": cmd.ExpectedTaskRevision,
		"active":        true,
	})
	dispatch, _ := json.Marshal(awsProviderMutationDispatch{
		ChangeID:              cmd.ChangeID,
		ConfirmationID:        cmd.ConfirmationID,
		TaskID:                cmd.TaskID,
		Kind:                  cmd.Kind,
		Status:                "dispatched",
		ClaimedChangeRevision: cmd.ExpectedChangeRevision,
	})

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT task_id::text,confirmation_id::text,operation,status,stage,change_set_id,revision FROM core_aws_changes")).
		WillReturnRows(sqlmock.NewRows([]string{"task_id", "confirmation_id", "operation", "status", "stage", "change_set_id", "revision"}).
			AddRow(cmd.TaskID, cmd.ConfirmationID, "create", "running", "reconciling", "", cmd.ExpectedChangeRevision))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,revision,attempt,lease_epoch,lease_expires_at FROM agent_tasks")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "revision", "attempt", "lease_epoch", "lease_expires_at"}).
			AddRow("running", cmd.ExpectedTaskRevision+1, cmd.Attempt, cmd.LeaseEpoch, time.Now().UTC().Add(time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations")).
		WillReturnRows(sqlmock.NewRows([]string{"state", "revision", "task_id", "reservation_json"}).
			AddRow("consumed", cmd.ExpectedConfirmationRevision, cmd.TaskID, reservation))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).
		WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_json"}).
			AddRow(awsProviderMutationDigest(cmd), dispatch))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_aws_changes SET stage=")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_aws_replays SET response_json=")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_aws_event_counters")).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_events")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT change_id::text,plan_id::text,credential_id::text,COALESCE(provision_id::text,''),task_id::text,confirmation_id::text,operation,status,stage,change_set_id,provider_request_digest,provider_token,revision,error_code,error_summary,created_at,updated_at FROM core_aws_changes")).
		WillReturnRows(sqlmock.NewRows([]string{"change_id", "plan_id", "credential_id", "provision_id", "task_id", "confirmation_id", "operation", "status", "stage", "change_set_id", "provider_request_digest", "provider_token", "revision", "error_code", "error_summary", "created_at", "updated_at"}).
			AddRow(cmd.ChangeID, "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555", "", cmd.TaskID, cmd.ConfirmationID, "create", "running", "change_set_ready", "changeset-1", "digest", "token", cmd.ExpectedChangeRevision+1, "", "", time.Now().UTC(), time.Now().UTC()))

	got, err := repo.CommitProviderMutation(t.Context(), agentaws.ProviderMutationResult{
		Command:             cmd,
		Success:             true,
		ProviderChangeSetID: "changeset-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != agentaws.StageChangeSetReady || got.Revision != cmd.ExpectedChangeRevision+1 {
		t.Fatalf("unexpected committed change: %+v", got)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresAWSCommitProviderMutationRejectsExpiredTaskLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := testAWSProviderMutationCommand()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT task_id::text,confirmation_id::text,operation,status,stage,change_set_id,revision FROM core_aws_changes")).
		WillReturnRows(sqlmock.NewRows([]string{"task_id", "confirmation_id", "operation", "status", "stage", "change_set_id", "revision"}).
			AddRow(cmd.TaskID, cmd.ConfirmationID, "create", "running", "reconciling", "", cmd.ExpectedChangeRevision))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,revision,attempt,lease_epoch,lease_expires_at FROM agent_tasks")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "revision", "attempt", "lease_epoch", "lease_expires_at"}).
			AddRow("running", cmd.ExpectedTaskRevision, cmd.Attempt, cmd.LeaseEpoch, time.Now().UTC().Add(-time.Minute)))
	mock.ExpectRollback()

	_, err = repo.CommitProviderMutation(t.Context(), agentaws.ProviderMutationResult{
		Command:             cmd,
		Success:             true,
		ProviderChangeSetID: "changeset-1",
	})
	if !errors.Is(err, agentaws.ErrRevisionConflict) {
		t.Fatalf("expected revision conflict, got %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresAWSClaimProviderMutationPersistsDispatchBeforeProviderCall(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := testAWSProviderMutationCommand()
	template := []byte(`{"Resources":{}}`)
	_, templateDigest, err := agentaws.NormalizeTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	plan := agentaws.Plan{ID: testAWSPlanID, CredentialID: "55555555-5555-4555-8555-555555555555", CredentialRevision: 1, Region: "us-east-1", StackName: "claim-provider", Operation: agentaws.OperationCreate, Template: template, TemplateSHA256: templateDigest, Parameters: map[string]string{}, Tags: map[string]string{"service": agentaws.EC2ServiceProfile, "owner": agentaws.OwnerBindingDigest(testAWSOwnerID)}, Capabilities: []string{}, Revision: 1, CreatedAt: time.Now().UTC()}
	params, _ := json.Marshal(plan.Parameters)
	tags, _ := json.Marshal(plan.Tags)
	caps, _ := json.Marshal(plan.Capabilities)
	providerDigest := agentaws.ProviderRequestDigest(plan, cmd.ConfirmationID)
	reservation, _ := json.Marshal(map[string]any{
		"task_id":       cmd.TaskID,
		"attempt":       cmd.Attempt,
		"lease_epoch":   cmd.LeaseEpoch,
		"task_revision": cmd.ExpectedTaskRevision,
		"active":        true,
	})
	ambiguousCommit := errors.New("commit result unavailable")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT task_id::text,confirmation_id::text,operation,status,stage,change_set_id,revision FROM core_aws_changes")).
		WillReturnRows(sqlmock.NewRows([]string{"task_id", "confirmation_id", "operation", "status", "stage", "change_set_id", "revision"}).
			AddRow(cmd.TaskID, cmd.ConfirmationID, "create", "running", "change_set_creating", "", cmd.ExpectedChangeRevision))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,revision,attempt,lease_epoch,lease_expires_at FROM agent_tasks")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "revision", "attempt", "lease_epoch", "lease_expires_at"}).
			AddRow("running", cmd.ExpectedTaskRevision, cmd.Attempt, cmd.LeaseEpoch, time.Now().UTC().Add(time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations")).
		WillReturnRows(sqlmock.NewRows([]string{"state", "revision", "task_id", "reservation_json"}).
			AddRow("consumed", cmd.ExpectedConfirmationRevision, cmd.TaskID, reservation))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash FROM core_aws_replays")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_id::text,provider_token,provider_request_digest FROM core_aws_changes")).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "provider_token", "provider_request_digest"}).AddRow(plan.ID, cmd.ConfirmationID, providerDigest))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans")).
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "credential_id", "credential_revision", "region", "stack_name", "operation", "template", "template_sha256", "parameters_json", "tags_json", "capabilities_json", "revision", "created_at"}).AddRow(plan.ID, plan.CredentialID, plan.CredentialRevision, plan.Region, plan.StackName, string(plan.Operation), plan.Template, plan.TemplateSHA256, params, tags, caps, plan.Revision, plan.CreatedAt))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM core_aws_events")).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_aws_changes SET stage='reconciling'")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_replays")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_aws_event_counters")).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_events")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(ambiguousCommit)

	_, err = repo.ClaimProviderMutation(t.Context(), cmd)
	if !errors.Is(err, ambiguousCommit) {
		t.Fatalf("expected ambiguous commit error, got %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresAWSCredentialReplaySnapshotsAndDigestConflicts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	key := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	view := agentaws.CredentialView{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Name: "prod", Region: "us-east-1", Revision: 2}
	raw, _ := json.Marshal(view)
	for _, operation := range []string{"credential-save", "credential-replace"} {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).
			WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_json"}).AddRow("digest", raw))
		got, hit, e := repo.ReplayCredential(t.Context(), operation, key, "digest")
		if e != nil || !hit || got != view {
			t.Fatalf("%s replay: %#v %v %v", operation, got, hit, e)
		}
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).
		WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_json"}).AddRow("digest", []byte(`{"deleted":true}`)))
	if _, hit, e := repo.ReplayCredential(t.Context(), "credential-delete", key, "digest"); e != nil || !hit {
		t.Fatalf("delete replay: hit=%v err=%v", hit, e)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).
		WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_json"}).AddRow("digest", raw))
	if _, hit, e := repo.ReplayCredential(t.Context(), "credential-save", key, "different"); !hit || !errors.Is(e, agentaws.ErrIdempotencyConflict) {
		t.Fatalf("digest conflict: hit=%v err=%v", hit, e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresAWSCredentialMutationReplayDoesNotWriteAgain(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keyring, err := LoadOrCreateAgentSecretKeyring(t.TempDir() + "/keyring.json")
	if err != nil {
		t.Fatal(err)
	}
	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := NewAgentAWSRepositoryWithEnveloper(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID, enveloper)
	if err != nil {
		t.Fatal(err)
	}
	credentialID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	key := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	view := agentaws.CredentialView{ID: credentialID, Name: "original", Region: "us-east-1", Revision: 1}
	raw, _ := json.Marshal(view)
	expectReplay := func(operation string, response []byte) {
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).
			WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_json"}).AddRow("digest", response))
		mock.ExpectCommit()
	}
	expectReplay("credential-save", raw)
	c := agentaws.RehydrateCredentials(credentialID, "new", "us-east-1", "", "", []byte("access"), []byte("secret"), nil, 0, 1, time.Now().UTC(), time.Now().UTC())
	if got, e := repo.SaveCredentialIdempotent(t.Context(), c, key, "digest"); e != nil || got != view {
		t.Fatalf("save replay: %#v %v", got, e)
	}
	expectReplay("credential-replace", raw)
	c = agentaws.RehydrateCredentials(credentialID, "new", "us-east-1", "", "", []byte("access"), []byte("secret"), nil, 0, 2, time.Now().UTC(), time.Now().UTC())
	if got, e := repo.ReplaceCredentialIdempotent(t.Context(), c, 1, key, "digest"); e != nil || got != view {
		t.Fatalf("replace replay: %#v %v", got, e)
	}
	expectReplay("credential-delete", []byte(`{"deleted":true}`))
	if e := repo.DeleteCredentialIdempotent(t.Context(), credentialID, 2, key, "digest"); e != nil {
		t.Fatalf("delete replay: %v", e)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
