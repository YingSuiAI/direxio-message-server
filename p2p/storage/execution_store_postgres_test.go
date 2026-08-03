package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func executionStoreFixture(now time.Time, revision uint64) (coreexecution.ProjectAnalysis, coreexecution.ExecutionPlan) {
	owner := "@execution-store:example.org"
	project := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	analysisID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	targetID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	planID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	analysis := coreexecution.ProjectAnalysis{AnalysisID: analysisID, ProjectID: project, Source: coreexecution.SourceRef{Kind: "git_https", Location: "https://example.org/repo", Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true}, Revision: 1, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	target := coreexecution.ExecutionTarget{ID: targetID, Provider: "aws", Kind: "aws_ec2_instance", AccountID: "123456789012", Region: "us-east-1", Revision: 1}
	placement := coreexecution.PlacementRecommendation{Kind: "existing_target", Minimum: coreexecution.PlacementOption{Region: "us-east-1", Spec: "t3.small", Disk: "20GiB", Network: "private", CostQuote: coreexecution.CostQuote{Amount: "1", Currency: "USD", ExpiresAt: now.Add(time.Hour)}}, Recommended: coreexecution.PlacementOption{Region: "us-east-1", Spec: "t3.small", Disk: "20GiB", Network: "private", CostQuote: coreexecution.CostQuote{Amount: "1", Currency: "USD", ExpiresAt: now.Add(time.Hour)}}, HighPerformance: coreexecution.PlacementOption{Region: "us-east-1", Spec: "t3.small", Disk: "20GiB", Network: "private", CostQuote: coreexecution.CostQuote{Amount: "1", Currency: "USD", ExpiresAt: now.Add(time.Hour)}}}
	step := coreexecution.ExecutionStep{StepKey: "cleanup", Kind: coreexecution.StepCleanup, TargetID: targetID, TargetRevision: 1, TimeoutSeconds: 1, IdempotencyMarker: "cleanup-1", Cleanup: &coreexecution.CleanupStep{Resource: "instance"}}
	stage := coreexecution.ExecutionStage{StageKey: "deploy", Revision: 1, Kind: "deploy", Risk: coreexecution.RiskR2, Gate: coreexecution.GateRemoteExecution, TargetID: targetID, TargetRevision: 1, Steps: []coreexecution.ExecutionStep{step}, RollbackSteps: []coreexecution.ExecutionStep{{StepKey: "cleanup", Kind: coreexecution.StepCleanup, TargetID: targetID, TargetRevision: 1, TimeoutSeconds: 1, IdempotencyMarker: "rollback-1", Cleanup: &coreexecution.CleanupStep{Resource: "instance"}}}, RollbackPolicy: &coreexecution.RollbackPolicy{Risk: coreexecution.RiskR4, Gate: coreexecution.GateRollback}, TimeoutSeconds: 10}
	plan := coreexecution.ExecutionPlan{SchemaVersion: coreexecution.SchemaVersion, ID: planID, Revision: revision, OwnerID: owner, ProjectID: project, AnalysisID: analysisID, Purpose: coreexecution.PurposeJob, Placement: placement, Targets: []coreexecution.ExecutionTarget{target}, Stages: []coreexecution.ExecutionStage{stage}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), Status: coreexecution.PlanReady}
	return analysis, plan
}

func TestExecutionStorePostgresRoundTripRestartAndReplay(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return base })
	analysis, plan := executionStoreFixture(base, 1)
	created, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: plan.OwnerID, Analysis: analysis, Plan: plan, IdempotencyID: "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Stages[0].RollbackSteps) != 1 {
		t.Fatal("rollback step not retained")
	}
	got, err := s.GetPlanRevision(ctx, plan.OwnerID, plan.ID, 1)
	if err != nil || len(got.Stages[0].RollbackSteps) != 1 {
		t.Fatalf("readback=%v err=%v", got, err)
	}
	replay, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: plan.OwnerID, Analysis: analysis, Plan: plan, IdempotencyID: "11111111-1111-4111-8111-111111111111"})
	if err != nil || replay.Digest != created.Digest {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_plan_steps SET step_set='rollback' WHERE owner_id=$1 AND plan_id=$2 AND plan_revision=1 AND step_set='forward'`, plan.OwnerID, plan.ID); err == nil {
		t.Fatal("step_set mutation unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE core_execution_idempotency DISABLE TRIGGER core_execution_idempotency_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_idempotency SET response_json='{"tampered":true}'::jsonb WHERE owner_id=$1 AND idempotency_id=$2`, plan.OwnerID, "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE core_execution_idempotency ENABLE TRIGGER core_execution_idempotency_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: plan.OwnerID, Analysis: analysis, Plan: plan, IdempotencyID: "11111111-1111-4111-8111-111111111111"}); !errors.Is(err, ErrExecutionStoreDrift) {
		t.Fatalf("tampered response=%v", err)
	}
	if _, err := s.GetCurrentPlan(ctx, "@other:example.org", plan.ID); !errors.Is(err, coreexecution.ErrNotFound) && !errors.Is(err, ErrExecutionStoreInvalid) {
		t.Fatalf("cross-owner=%v", err)
	}
}

func TestExecutionStorePostgresRevisionCASAndExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	a, p1 := executionStoreFixture(now, 1)
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: p1.OwnerID, Analysis: a, Plan: p1, IdempotencyID: "22222222-2222-4222-8222-222222222222"}); err != nil {
		t.Fatal(err)
	}
	_, p2 := executionStoreFixture(now, 2)
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: p2.OwnerID, Analysis: a, Plan: p2, IdempotencyID: "33333333-3333-4333-8333-333333333333"}); err != nil {
		t.Fatalf("successor=%v", err)
	}
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: p2.OwnerID, Analysis: a, Plan: p2, IdempotencyID: "44444444-4444-4444-8444-444444444444"}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("duplicate revision=%v", err)
	}
	_, expired := executionStoreFixture(now, 3)
	expired.ExpiresAt = now
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: expired.OwnerID, Analysis: a, Plan: expired, IdempotencyID: "55555555-5555-4555-8555-555555555555"}); !errors.Is(err, coreexecution.ErrExpired) {
		t.Fatalf("expiry=%v", err)
	}
}

func TestExecutionStorePostgresRevisePlanSuccessAndIdentityFences(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2035, 1, 1, 0, 0, 0, 123456789, time.UTC)
	now := base.Add(time.Minute)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	analysis, initial := executionStoreFixture(base, 1)
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: initial.OwnerID, Analysis: analysis, Plan: initial, IdempotencyID: "56565656-5656-4565-8565-565656565656"}); err != nil {
		t.Fatal(err)
	}

	candidate := initial
	candidate.CreatedAt = now
	candidate.ExpiresAt = base.Add(3 * time.Hour)
	candidate.Artifacts = []coreexecution.ArtifactRef{{ID: "67676767-6767-4676-8676-676767676767", Digest: coreexecution.Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"), MediaType: "application/x-sh", Size: 12, Immutable: true}}
	revised, err := s.RevisePlan(ctx, initial.OwnerID, candidate, 1, "57575757-5757-4575-8575-575757575757")
	if err != nil {
		t.Fatal(err)
	}
	if revised.Revision != 2 || revised.ID != initial.ID || revised.ProjectID != initial.ProjectID || revised.AnalysisID != initial.AnalysisID {
		t.Fatalf("revised plan identity = %#v", revised)
	}
	if revised.CreatedAt.Nanosecond()%1000 != 0 || revised.ExpiresAt.Nanosecond()%1000 != 0 {
		t.Fatalf("revised timestamps were not sealed at PostgreSQL precision: created=%s expires=%s", revised.CreatedAt, revised.ExpiresAt)
	}
	got, err := s.GetCurrentPlan(ctx, initial.OwnerID, initial.ID)
	if err != nil || !samePlan(got, revised) {
		t.Fatalf("current revised plan = %#v, err=%v", got, err)
	}
	var artifactPlanRevision uint64
	if err = store.DB().QueryRowContext(ctx, `SELECT plan_revision FROM core_execution_artifacts WHERE owner_id=$1 AND artifact_id=$2`, initial.OwnerID, candidate.Artifacts[0].ID).Scan(&artifactPlanRevision); err != nil || artifactPlanRevision != 2 {
		t.Fatalf("revised artifact plan revision=%d err=%v", artifactPlanRevision, err)
	}
	var headRevision, revisionCount uint64
	if err = store.DB().QueryRowContext(ctx, `SELECT revision FROM core_execution_plans WHERE owner_id=$1 AND plan_id=$2`, initial.OwnerID, initial.ID).Scan(&headRevision); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_plan_revisions WHERE owner_id=$1 AND plan_id=$2`, initial.OwnerID, initial.ID).Scan(&revisionCount); err != nil {
		t.Fatal(err)
	}
	if headRevision != 2 || revisionCount != 2 {
		t.Fatalf("head revision=%d immutable revision count=%d", headRevision, revisionCount)
	}
	wantResponseDigest, err := coreexecution.CanonicalDigest(revised)
	if err != nil {
		t.Fatal(err)
	}
	var storedResponseDigest string
	if err = store.DB().QueryRowContext(ctx, `SELECT response_digest FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2`, initial.OwnerID, "57575757-5757-4575-8575-575757575757").Scan(&storedResponseDigest); err != nil {
		t.Fatal(err)
	}
	if storedResponseDigest != string(wantResponseDigest) {
		t.Fatalf("response digest=%s want canonical=%s", storedResponseDigest, wantResponseDigest)
	}

	identityCases := []struct {
		name string
		key  string
		edit func(*coreexecution.ExecutionPlan)
	}{
		{name: "project", key: "58585858-5858-4585-8585-585858585858", edit: func(p *coreexecution.ExecutionPlan) { p.ProjectID = "12121212-1212-4121-8121-121212121212" }},
		{name: "analysis", key: "59595959-5959-4595-8595-595959595959", edit: func(p *coreexecution.ExecutionPlan) { p.AnalysisID = "13131313-1313-4131-8131-131313131313" }},
	}
	for _, tc := range identityCases {
		t.Run(tc.name, func(t *testing.T) {
			bad := revised
			bad.Digest = ""
			bad.CreatedAt = now.Add(time.Minute)
			bad.ExpiresAt = base.Add(4 * time.Hour)
			tc.edit(&bad)
			if _, err := s.RevisePlan(ctx, initial.OwnerID, bad, 2, tc.key); !errors.Is(err, coreexecution.ErrConflict) {
				t.Fatalf("identity rewrite error = %v", err)
			}
			var count int
			if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2`, initial.OwnerID, tc.key).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rolled-back identity idempotency count=%d err=%v", count, err)
			}
		})
	}
}

func TestExecutionStorePostgresRevisePlanRejectsStaleRevision(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(time.Minute)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	analysis, initial := executionStoreFixture(base, 1)
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: initial.OwnerID, Analysis: analysis, Plan: initial, IdempotencyID: "60606060-6060-4606-8606-606060606060"}); err != nil {
		t.Fatal(err)
	}
	candidate := initial
	candidate.CreatedAt = now
	candidate.ExpiresAt = base.Add(3 * time.Hour)
	if _, err := s.RevisePlan(ctx, initial.OwnerID, candidate, 1, "61616161-6161-4616-8616-616161616161"); err != nil {
		t.Fatal(err)
	}

	stale := initial
	stale.CreatedAt = now.Add(time.Minute)
	stale.ExpiresAt = base.Add(4 * time.Hour)
	if _, err := s.RevisePlan(ctx, initial.OwnerID, stale, 1, "62626262-6262-4626-8626-626262626262"); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	var headRevision, revisionCount, replayCount uint64
	if err := store.DB().QueryRowContext(ctx, `SELECT revision FROM core_execution_plans WHERE owner_id=$1 AND plan_id=$2`, initial.OwnerID, initial.ID).Scan(&headRevision); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_plan_revisions WHERE owner_id=$1 AND plan_id=$2`, initial.OwnerID, initial.ID).Scan(&revisionCount); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2`, initial.OwnerID, "62626262-6262-4626-8626-626262626262").Scan(&replayCount); err != nil {
		t.Fatal(err)
	}
	if headRevision != 2 || revisionCount != 2 || replayCount != 0 {
		t.Fatalf("stale mutation leaked: head=%d revisions=%d idempotency=%d", headRevision, revisionCount, replayCount)
	}
}

func TestExecutionStorePostgresRevisePlanReplaysAcrossServerTimestamps(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base.Add(time.Minute)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return clock })
	analysis, initial := executionStoreFixture(base, 1)
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: initial.OwnerID, Analysis: analysis, Plan: initial, IdempotencyID: "63636363-6363-4636-8636-636363636363"}); err != nil {
		t.Fatal(err)
	}
	key := "64646464-6464-4646-8646-646464646464"
	firstRequest := initial
	firstRequest.CreatedAt = clock
	firstRequest.ExpiresAt = base.Add(3 * time.Hour)
	first, err := s.RevisePlan(ctx, initial.OwnerID, firstRequest, 1, key)
	if err != nil {
		t.Fatal(err)
	}

	clock = base.Add(10 * time.Minute)
	replayRequest := initial
	replayRequest.CreatedAt = clock
	replayRequest.ExpiresAt = base.Add(6 * time.Hour)
	replay, err := s.RevisePlan(ctx, initial.OwnerID, replayRequest, 1, key)
	if err != nil {
		t.Fatal(err)
	}
	if !samePlan(first, replay) || !replay.CreatedAt.Equal(first.CreatedAt) || !replay.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("replay did not return exact first response: first=%#v replay=%#v", first, replay)
	}
	var idemCount, revisionCount int
	if err = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2`, initial.OwnerID, key).Scan(&idemCount); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_plan_revisions WHERE owner_id=$1 AND plan_id=$2`, initial.OwnerID, initial.ID).Scan(&revisionCount); err != nil {
		t.Fatal(err)
	}
	if idemCount != 1 || revisionCount != 2 {
		t.Fatalf("replay duplicated durable state: idempotency=%d revisions=%d", idemCount, revisionCount)
	}
}

func TestExecutionStorePostgresRevisePlanRejectsConflictingIdempotency(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(time.Minute)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	analysis, initial := executionStoreFixture(base, 1)
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: initial.OwnerID, Analysis: analysis, Plan: initial, IdempotencyID: "65656565-6565-4656-8656-656565656565"}); err != nil {
		t.Fatal(err)
	}
	key := "67676767-6767-4676-8676-676767676767"
	firstRequest := initial
	firstRequest.CreatedAt = now
	firstRequest.ExpiresAt = base.Add(3 * time.Hour)
	first, err := s.RevisePlan(ctx, initial.OwnerID, firstRequest, 1, key)
	if err != nil {
		t.Fatal(err)
	}

	conflict := initial
	conflict.CreatedAt = now.Add(time.Minute)
	conflict.ExpiresAt = base.Add(4 * time.Hour)
	conflict.Stages[0].Title = "different deployment semantics"
	if _, err := s.RevisePlan(ctx, initial.OwnerID, conflict, 1, key); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("conflicting idempotency error = %v", err)
	}
	var responseDigest string
	if err = store.DB().QueryRowContext(ctx, `SELECT response_digest FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2`, initial.OwnerID, key).Scan(&responseDigest); err != nil {
		t.Fatal(err)
	}
	want, err := coreexecution.CanonicalDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	if responseDigest != string(want) {
		t.Fatalf("conflict changed first response digest: got=%s want=%s", responseDigest, want)
	}
}

func TestExecutionStorePostgresConcurrentReplayAndAtomicRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	a, p := executionStoreFixture(now, 1)
	in := ExecutionPlanCreate{OwnerID: p.OwnerID, Analysis: a, Plan: p, IdempotencyID: "66666666-6666-4666-8666-666666666666"}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.CreatePlan(ctx, in); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent replay: %v", err)
		}
	}

	badAnalysis, badPlan := executionStoreFixture(now, 1)
	badAnalysis.AnalysisID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	badPlan.AnalysisID = badAnalysis.AnalysisID
	badPlan.ProjectID = "99999999-9999-4999-8999-999999999999"
	badAnalysis.ProjectID = badPlan.ProjectID
	badPlan.ID = "88888888-8888-4888-8888-888888888888"
	badPlan.Targets[0].AccountID = "999999999999" // conflicts with the existing target identity/revision
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: badPlan.OwnerID, Analysis: badAnalysis, Plan: badPlan, IdempotencyID: "77777777-7777-4777-8777-777777777777"}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("mid-graph conflict=%v", err)
	}
	var projects int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_projects WHERE owner_id=$1 AND project_id=$2`, badPlan.OwnerID, badPlan.ProjectID).Scan(&projects); err != nil || projects != 0 {
		t.Fatalf("mid-graph rollback projects=%d err=%v", projects, err)
	}
}

func TestExecutionStorePostgresRejectsArchivedTargetAndRevokedSkill(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	a, p := executionStoreFixture(now, 1)
	p.Skills = []coreexecution.SkillRef{{ID: "deploy", Version: "1.0.0", Digest: coreexecution.Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")}}
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: p.OwnerID, Analysis: a, Plan: p, IdempotencyID: "99999999-9999-4999-8999-999999999999"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE core_execution_targets DISABLE TRIGGER core_execution_targets_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_targets SET status='archived' WHERE owner_id=$1 AND target_id=$2`, p.OwnerID, p.Targets[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE core_execution_targets ENABLE TRIGGER core_execution_targets_immutable`); err != nil {
		t.Fatal(err)
	}
	_, successor := executionStoreFixture(now, 1)
	successor.ID = "12121212-1212-4121-8121-121212121212"
	successor.Skills = p.Skills
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: successor.OwnerID, Analysis: a, Plan: successor, IdempotencyID: "abababab-abab-4aba-8aba-abababababab"}); !errors.Is(err, ErrExecutionStoreDrift) {
		t.Fatalf("archived target=%v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE core_execution_skill_versions DISABLE TRIGGER core_execution_skill_versions_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_skill_versions SET status='revoked' WHERE owner_id=$1 AND id='deploy' AND version='1.0.0'`, p.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE core_execution_skill_versions ENABLE TRIGGER core_execution_skill_versions_immutable`); err != nil {
		t.Fatal(err)
	}
	_, successor2 := executionStoreFixture(now, 1)
	successor2.ID = "34343434-3434-4343-8434-343434343434"
	successor2.Skills = p.Skills
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: successor2.OwnerID, Analysis: a, Plan: successor2, IdempotencyID: "cdcdcdcd-cdcd-4cdc-8cdc-cdcdcdcdcdcd"}); !errors.Is(err, ErrExecutionStoreDrift) {
		t.Fatalf("revoked skill=%v", err)
	}
}

func TestExecutionStorePostgresDispatchFenceAndResolve(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	a, p := executionStoreFixture(now, 1)
	createdPlan, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: p.OwnerID, Analysis: a, Plan: p, IdempotencyID: "abababab-abab-4aba-8aba-abababababab"})
	if err != nil {
		t.Fatal(err)
	}
	c := NewDatabaseExecutionCoordinator(store.DB(), func() time.Time { return now })
	m, err := c.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: p.OwnerID, PlanID: p.ID, PlanRevision: 1, IdempotencyKey: "cdcdcdcd-cdcd-4cdc-8cdc-cdcdcdcdcdcd"})
	if err != nil {
		t.Fatal(err)
	}
	stage := m.Stages[0]
	if _, err := c.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{OwnerID: p.OwnerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: m.Confirmations[0].Revision, IdempotencyKey: "12121212-1212-4121-8121-121212121212"}); err != nil {
		t.Fatal(err)
	}
	var wins int
	var claimed ExecutionStageLeaseClaim
	for i := 0; i < 2; i++ {
		claim, e := s.ClaimNextExecutionStage(ctx, "worker", time.Hour)
		ok := e == nil && claim.TaskID != ""
		if i == 1 && errors.Is(e, coreexecution.ErrNotFound) {
			continue
		}
		if e != nil {
			t.Fatal(e)
		}
		if ok {
			wins++
			claimed = claim
		}
	}
	if wins != 1 {
		t.Fatalf("claim wins=%d", wins)
	}
	running, err := s.GetExecutionRun(ctx, p.OwnerID, m.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if running.Run.Status != coreexecution.RunRunning ||
		running.Run.CurrentStageID != claimed.StageID ||
		running.Run.CurrentStage != stage.StageKey ||
		running.Run.StartedAt.IsZero() {
		t.Fatalf("claim did not atomically promote run: %+v", running.Run)
	}
	step := createdPlan.Stages[0].Steps[0]
	attemptID, receiptID := "03030303-0303-4303-8303-030303030303", "04040404-0404-4404-8404-040404040404"
	reqDigest := coreexecution.Digest("1111111111111111111111111111111111111111111111111111111111111111")
	fenceDigest := coreexecution.Digest("2222222222222222222222222222222222222222222222222222222222222222")
	// A dispatch intent without a complete immutable frozen snapshot was only
	// accepted by the retired compatibility path. V2 must reject it before any
	// provider command can be associated with the stage.
	if err := s.RecordDispatchIntent(ctx, ExecutionDispatchIntent{TaskID: claimed.TaskID, TaskHolder: claimed.Holder, TaskAttempt: claimed.Attempt, TaskRevision: claimed.ExpectedTaskRevision, TaskLeaseEpoch: claimed.TaskLeaseEpoch, RequestDigest: reqDigest, FenceDigest: fenceDigest, StepSet: coreexecution.StepSetForward, TargetID: stage.TargetID, TargetRevision: stage.TargetRevision, TargetDigest: stage.TargetDigest, LeaseID: claimed.LeaseID, LeaseToken: claimed.LeaseToken, LeaseEpoch: claimed.LeaseEpoch, Attempt: coreexecution.StepAttempt{AttemptID: attemptID, RunID: stage.RunID, StageID: stage.StageID, OwnerID: p.OwnerID, PlanID: createdPlan.ID, PlanRevision: createdPlan.Revision, PlanDigest: createdPlan.Digest, StageRevision: stage.StageRevision, StageDigest: stage.StageDigest, StepRevision: 1, StepDigest: step.Digest, StepKey: step.StepKey, Attempt: 1}, Receipt: coreexecution.Receipt{ReceiptID: receiptID, RunID: stage.RunID, OwnerID: p.OwnerID, AttemptID: attemptID, Status: coreexecution.ReceiptAccepted, IdempotencyKey: step.IdempotencyMarker}}); !errors.Is(err, ErrExecutionStoreInvalid) {
		t.Fatalf("zero snapshot=%v", err)
	}
}
