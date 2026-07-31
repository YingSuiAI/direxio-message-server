package storage

import (
	"context"
	"database/sql"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func TestServiceOutputMaterializerRecoversCASGapAndReplaysExactly(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	database := openExecutionV2Schema(t)
	store := NewDatabaseExecutionStore(database.DB(), func() time.Time { return base })
	analysis, plan := executionStoreFixture(base, 1)
	plan.Purpose = coreexecution.PurposeService
	plan.DeploymentID = "71717171-7171-4717-8717-717171717171"
	plan.Targets[0].Capabilities = []string{
		"target.aws_ec2_instance",
		"target.instance.i-0123456789abcdef0",
		"transport.aws_ssm",
	}
	if _, err := store.CreatePlan(ctx, ExecutionPlanCreate{
		OwnerID: plan.OwnerID, Analysis: analysis, Plan: plan,
		IdempotencyID: "72727272-7272-4727-8727-727272727272",
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := NewDatabaseExecutionCoordinator(database.DB(), func() time.Time { return base })
	materialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{
		OwnerID: plan.OwnerID, PlanID: plan.ID, PlanRevision: 1,
		IdempotencyKey: "73737373-7373-4737-8737-737373737373",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(materialized.Confirmations) != 1 {
		t.Fatalf("confirmations=%d", len(materialized.Confirmations))
	}
	if _, err = coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{
		OwnerID: plan.OwnerID, ConfirmationID: materialized.Confirmations[0].ID,
		ExpectedRevision: materialized.Confirmations[0].Revision,
		IdempotencyKey:   "74747474-7474-4747-8747-747474747474",
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimNextExecutionStage(ctx, "service-output-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	terminalAt := base.Add(time.Minute)
	tx, err := database.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = transitionRunningExecutionStageTx(ctx, tx, claim.OwnerID, claim.RunID, claim.StageID, coreexecution.StageSucceeded, terminalAt); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err = transitionExecutionRunTx(ctx, tx, claim.OwnerID, claim.RunID, coreexecution.RunSucceeded, "", terminalAt, true); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	files, err := artifactstore.New(t.TempDir(), artifactstore.MaxArtifactSize)
	if err != nil {
		t.Fatal(err)
	}
	firstProcess := NewExecutionServiceOutputMaterializer(store, files, plan.OwnerID)
	spec, err := firstProcess.loadSpec(ctx, claim.RunID, false)
	if err != nil {
		t.Fatal(err)
	}
	published, err := firstProcess.publishDocuments(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 {
		t.Fatalf("published files=%d", len(published))
	}
	assertServiceOutputCounts(t, ctx, database.DB(), plan.OwnerID, claim.RunID, 0, 0, 0, 0)

	// Simulate a restart after both atomic filesystem renames but before any
	// database metadata was committed. Recovery must reuse those files.
	restarted := NewExecutionServiceOutputMaterializer(store, files, plan.OwnerID)
	if err = restarted.RecoverPending(ctx); err != nil {
		t.Fatal(err)
	}
	bindings, _, err := store.ListServiceBindings(ctx, plan.OwnerID, plan.ProjectID, "", 10)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("bindings=%#v err=%v", bindings, err)
	}
	binding := bindings[0]
	if binding.Protocol != "ssm" || binding.Endpoint != "ssm://i-0123456789abcdef0" || len(binding.OperationSchemas) != 0 || len(binding.AuthRefs) != 0 || len(binding.ArtifactIDs) != 2 || binding.UsageArtifact.ID != spec.Usage.ArtifactID {
		t.Fatalf("binding=%#v", binding)
	}
	deployment, err := store.GetExecutionDeployment(ctx, plan.OwnerID, plan.DeploymentID)
	if err != nil || deployment.State != "succeeded" || deployment.ReleaseID != spec.ReleaseID {
		t.Fatalf("deployment=%#v err=%v", deployment, err)
	}
	assertServiceOutputCounts(t, ctx, database.DB(), plan.OwnerID, claim.RunID, 2, 1, 1, 1)

	for _, document := range []serviceOutputDocument{spec.Usage, spec.Runbook} {
		metadata, err := store.GetArtifactMetadata(ctx, plan.OwnerID, document.ArtifactID)
		if err != nil || metadata.ContentDigest != document.Digest || metadata.StorageRef != serviceStorageRef(document.Digest) {
			t.Fatalf("metadata=%#v err=%v", metadata, err)
		}
		reader, opened, err := files.Open(ctx, string(document.Digest))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil || opened.Size != int64(len(document.Body)) || string(body) != string(document.Body) {
			t.Fatalf("artifact %s body mismatch readErr=%v", document.Kind, readErr)
		}
		lower := strings.ToLower(string(body))
		for _, forbidden := range []string{"geolibre", "password=", "authorization:", "aws_secret_access_key", "#!/bin/"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("artifact %s leaked project/executable/secret material %q: %s", document.Kind, forbidden, body)
			}
		}
	}

	// Exact terminal replays do not advance revisions, append events, or create
	// additional metadata rows.
	replayed, err := restarted.EnsureRun(ctx, claim.RunID)
	if err != nil || replayed.Digest != binding.Digest || replayed.BindingID != binding.BindingID {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}
	if err = restarted.RecoverPending(ctx); err != nil {
		t.Fatal(err)
	}
	assertServiceOutputCounts(t, ctx, database.DB(), plan.OwnerID, claim.RunID, 2, 1, 1, 1)
	latest, err := store.GetExecutionDeployment(ctx, plan.OwnerID, plan.DeploymentID)
	if err != nil || latest.Revision != deployment.Revision {
		t.Fatalf("deployment replay revision=%d want=%d err=%v", latest.Revision, deployment.Revision, err)
	}
}

func TestServiceDeploymentRetryAdvancesCurrentRunProjection(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 2, 0, 0, 0, 0, time.UTC)
	database := openExecutionV2Schema(t)
	store := NewDatabaseExecutionStore(database.DB(), func() time.Time { return now })
	analysis, plan := executionStoreFixture(now, 1)
	plan.Purpose = coreexecution.PurposeService
	plan.DeploymentID = "81818181-8181-4818-8818-818181818181"
	if _, err := store.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: plan.OwnerID, Analysis: analysis, Plan: plan, IdempotencyID: "82828282-8282-4828-8828-828282828282"}); err != nil {
		t.Fatal(err)
	}
	coordinator := NewDatabaseExecutionCoordinator(database.DB(), func() time.Time { return now })
	first, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: plan.OwnerID, PlanID: plan.ID, PlanRevision: 1, IdempotencyKey: "83838383-8383-4838-8838-838383838383"})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := coordinator.CancelRun(ctx, coreexecution.CancelRunCommand{OwnerID: plan.OwnerID, RunID: first.Run.RunID, ExpectedRevision: first.Run.Revision, IdempotencyKey: "84848484-8484-4848-8848-848484848484"})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := coordinator.RetryRun(ctx, coreexecution.RetryRunCommand{OwnerID: plan.OwnerID, RunID: first.Run.RunID, ExpectedRevision: canceled.Revision, IdempotencyKey: "85858585-8585-4858-8858-858585858585"})
	if err != nil || retry.Run.RunID == first.Run.RunID || retry.Run.DeploymentID != plan.DeploymentID {
		t.Fatalf("retry=%#v err=%v", retry, err)
	}
	var current string
	if err = database.DB().QueryRowContext(ctx, `SELECT current_run_id::text FROM core_execution_deployments WHERE owner_id=$1 AND deployment_id=$2`, plan.OwnerID, plan.DeploymentID).Scan(&current); err != nil || current != retry.Run.RunID {
		t.Fatalf("current=%q retry=%q err=%v", current, retry.Run.RunID, err)
	}
	var count int
	if err = database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_runs WHERE owner_id=$1 AND deployment_id=$2`, plan.OwnerID, plan.DeploymentID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("runs=%d err=%v", count, err)
	}
}

func TestServiceOutputDocumentsUseOnlySanitizedTypedFacts(t *testing.T) {
	spec := serviceOutputSpec{
		Run: coreexecution.ExecutionRun{
			RunID: "11111111-1111-4111-8111-111111111111", DeploymentID: "22222222-2222-4222-8222-222222222222",
		},
		Plan: coreexecution.PlanSnapshot{
			ID: "33333333-3333-4333-8333-333333333333", Revision: 4,
			Digest: coreexecution.Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		},
		Target: coreexecution.ExecutionTarget{
			ID: "44444444-4444-4444-8444-444444444444", Revision: 2,
			Digest: coreexecution.Digest("abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"),
		},
		ReleaseID: "plan-release", Protocol: "https", Endpoint: "https://service.example.test",
		ProbeFacts: []serviceProbeFact{{Kind: "http", Mode: "external", Endpoint: "https://service.example.test", ExpectedStatus: []int{204, 200}}, {Kind: "http", Mode: "target_local"}},
	}
	usage := string(buildServiceUsage(spec))
	runbook := string(buildServiceRunbook(spec))
	for _, text := range []string{usage, runbook} {
		if strings.Contains(text, "?access_token=do-not-copy") || strings.Contains(text, "/private-health-path") || strings.Contains(text, "204 200") || !strings.Contains(text, "[200 204]") {
			t.Fatalf("document was not a deterministic sanitized projection: %s", text)
		}
	}
}

func assertServiceOutputCounts(t *testing.T, ctx context.Context, db *sql.DB, owner, runID string, artifacts, bindings, runEvents, deploymentEvents int) {
	t.Helper()
	queries := []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM core_execution_artifacts WHERE owner_id=$1 AND run_id=$2`, artifacts},
		{`SELECT COUNT(*) FROM core_execution_service_bindings WHERE owner_id=$1 AND run_id=$2`, bindings},
		{`SELECT COUNT(*) FROM core_execution_events WHERE owner_id=$1 AND run_id=$2 AND kind='service.outputs.materialized'`, runEvents},
		{`SELECT COUNT(*) FROM core_execution_deployment_events e JOIN core_execution_deployments d ON d.owner_id=e.owner_id AND d.deployment_id=e.deployment_id WHERE d.owner_id=$1 AND d.current_run_id=$2 AND e.event_json->>'kind'='service.outputs.materialized'`, deploymentEvents},
	}
	for _, check := range queries {
		var got int
		if err := db.QueryRowContext(ctx, check.query, owner, runID).Scan(&got); err != nil || got != check.want {
			t.Fatalf("count query=%q got=%d want=%d err=%v", check.query, got, check.want, err)
		}
	}
}
