package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func catalogAnalysisFixture(now time.Time, projectID, analysisID string) coreexecution.ProjectAnalysis {
	return coreexecution.ProjectAnalysis{
		AnalysisID: analysisID,
		ProjectID:  projectID,
		Source: coreexecution.SourceRef{
			Kind: "git_https", Location: "https://example.org/catalog", Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true,
		},
		DetectedStacks: []string{"go", "container"},
		Ports:          []int{8080},
		Revision:       1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func catalogTargetFixture(id string, revision uint64) coreexecution.ExecutionTarget {
	return coreexecution.ExecutionTarget{ID: id, Provider: "aws", Kind: "aws_ec2_instance", AccountID: "123456789012", Region: "us-east-1", Architecture: "x86_64", Revision: revision}
}

func TestExecutionCatalogPostgresProjectAnalysisOwnerAndReplay(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	owner := "@catalog-owner:example.org"
	projectID := "11111111-1111-4111-8111-111111111111"
	project, err := s.CreateProject(ctx, ProjectCreateRequest{OwnerID: owner, ProjectID: projectID, IdempotencyID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
	if err != nil || project.Revision != 1 || project.Status != "active" {
		t.Fatalf("project=%+v err=%v", project, err)
	}
	replay, err := s.CreateProject(ctx, ProjectCreateRequest{OwnerID: owner, ProjectID: projectID, IdempotencyID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
	if err != nil || replay.Digest != project.Digest {
		t.Fatalf("project replay=%+v err=%v", replay, err)
	}
	if _, err := s.CreateProject(ctx, ProjectCreateRequest{OwnerID: owner, ProjectID: "22222222-2222-4222-8222-222222222222", IdempotencyID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("idempotency drift=%v", err)
	}
	analysisID := "33333333-3333-4333-8333-333333333333"
	analysis, err := s.CreateAnalysis(ctx, AnalysisCreateRequest{OwnerID: owner, Analysis: catalogAnalysisFixture(now, projectID, analysisID), IdempotencyID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"})
	if err != nil || !analysis.Digest.Valid() {
		t.Fatalf("analysis=%+v err=%v", analysis, err)
	}
	got, err := s.GetAnalysis(ctx, owner, analysisID)
	if err != nil || got.Digest != analysis.Digest || got.Source.Commit != analysis.Source.Commit {
		t.Fatalf("analysis readback=%+v err=%v", got, err)
	}
	if _, err := s.GetProject(ctx, "@other:example.org", projectID); !errors.Is(err, coreexecution.ErrNotFound) {
		t.Fatalf("cross-owner project=%v", err)
	}
	archived, err := s.ArchiveProject(ctx, owner, projectID, 1)
	if err != nil || archived.Status != "archived" || archived.Revision != 2 {
		t.Fatalf("archive=%+v err=%v", archived, err)
	}
	if _, err := s.ArchiveProject(ctx, owner, projectID, 1); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("stale archive=%v", err)
	}
}

func TestExecutionCatalogPostgresAnalysisBootstrapsProjectAndReplaysAcrossClockChange(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 2, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	owner := "@catalog-bootstrap:example.org"
	projectID := "91919191-9191-4191-8191-919191919191"
	analysisID := "92929292-9292-4292-8292-929292929292"
	key := "93939393-9393-4393-8393-939393939393"

	created, err := s.CreateAnalysis(ctx, AnalysisCreateRequest{
		OwnerID: owner, Analysis: catalogAnalysisFixture(time.Time{}, projectID, analysisID), IdempotencyID: key,
	})
	if err != nil || !created.Digest.Valid() {
		t.Fatalf("analysis bootstrap=%+v err=%v", created, err)
	}
	project, err := s.GetProject(ctx, owner, projectID)
	if err != nil || project.Status != "active" || project.Revision != 1 {
		t.Fatalf("bootstrapped project=%+v err=%v", project, err)
	}
	now = now.Add(24 * time.Hour)
	replay, err := s.CreateAnalysis(ctx, AnalysisCreateRequest{
		OwnerID: owner, Analysis: catalogAnalysisFixture(time.Time{}, projectID, analysisID), IdempotencyID: key,
	})
	if err != nil || replay.Digest != created.Digest || !replay.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("analysis replay=%+v err=%v", replay, err)
	}
}

func TestExecutionCatalogPostgresTargetRevisionObservationAndPagination(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	owner := "@catalog-target:example.org"
	projectID := "44444444-4444-4444-8444-444444444444"
	if _, err := s.CreateProject(ctx, ProjectCreateRequest{OwnerID: owner, ProjectID: projectID, IdempotencyID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}); err != nil {
		t.Fatal(err)
	}
	targetID := "55555555-5555-4555-8555-555555555555"
	target, err := s.CreateTarget(ctx, TargetCreateRequest{OwnerID: owner, Target: catalogTargetFixture(targetID, 99), IdempotencyID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd"})
	if err != nil || target.Revision != 1 || !target.Digest.Valid() {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	replayedTarget, err := s.CreateTarget(ctx, TargetCreateRequest{OwnerID: owner, Target: catalogTargetFixture(targetID, 0), IdempotencyID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd"})
	if err != nil || replayedTarget.Revision != 1 || replayedTarget.Digest != target.Digest {
		t.Fatalf("target replay=%+v err=%v", replayedTarget, err)
	}
	target.Region = "us-west-2"
	if _, err := s.CreateTarget(ctx, TargetCreateRequest{OwnerID: owner, Target: target, ExpectedRevision: 0, IdempotencyID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("stale target revision=%v", err)
	}
	target2, err := s.CreateTarget(ctx, TargetCreateRequest{OwnerID: owner, Target: target, ExpectedRevision: 1, IdempotencyID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"})
	if err != nil || target2.Revision != 2 {
		t.Fatalf("target successor=%+v err=%v", target2, err)
	}
	if _, err := s.CreateTarget(ctx, TargetCreateRequest{OwnerID: owner, Target: target2, ExpectedRevision: 2, IdempotencyID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("idempotency expected revision drift=%v", err)
	}
	obs, err := s.CreateTargetObservation(ctx, TargetObservationCreateRequest{OwnerID: owner, ObservationID: "66666666-6666-4666-8666-666666666666", Observation: coreexecution.TargetObservation{TargetID: targetID, TargetRevision: 2, ObservedAt: now, State: "ready", Partial: true, Stale: false, Facts: map[string]string{"cpu_percent": "12"}, Warnings: []string{"memory unavailable"}}, IdempotencyID: "ffffffff-ffff-4fff-8fff-ffffffffffff"})
	if err != nil || obs.Observation.Digest == "" || !obs.Observation.Partial || len(obs.Observation.Warnings) != 1 {
		t.Fatalf("observation=%+v err=%v", obs, err)
	}
	gotObs, err := s.GetTargetObservation(ctx, owner, obs.ObservationID)
	if err != nil || gotObs.Observation.Digest != obs.Observation.Digest || !gotObs.Observation.Partial || gotObs.Observation.Warnings[0] != "memory unavailable" {
		t.Fatalf("observation readback=%+v err=%v", gotObs, err)
	}
	if _, err := s.CreateTargetObservation(ctx, TargetObservationCreateRequest{OwnerID: owner, ObservationID: "77777777-7777-4777-8777-777777777777", Observation: coreexecution.TargetObservation{TargetID: targetID, TargetRevision: 1, ObservedAt: now, State: "ready"}, IdempotencyID: "12121212-1212-4121-8121-121212121212"}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("historical observation=%v", err)
	}
	page, err := s.ListTargets(ctx, owner, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].Revision != 2 {
		t.Fatalf("target page=%+v err=%v", page, err)
	}
	if page.NextCursor != "" && !strings.HasPrefix(page.NextCursor, targetID[:8]) {
		t.Fatalf("unexpected target cursor=%q", page.NextCursor)
	}
	if _, err := s.CreateTargetObservation(ctx, TargetObservationCreateRequest{OwnerID: owner, ObservationID: "88888888-8888-4888-8888-888888888888", Observation: coreexecution.TargetObservation{TargetID: targetID, TargetRevision: 2, ObservedAt: now, State: "ready", Facts: map[string]string{"api_token": "redacted"}}, IdempotencyID: "13131313-1313-4131-8131-131313131313"}); !errors.Is(err, ErrExecutionStoreInvalid) {
		t.Fatalf("secret fact=%v", err)
	}
}

func TestExecutionCatalogPostgresRejectsTamperedSnapshots(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	owner := "@catalog-tamper:example.org"
	projectID := "99999999-9999-4999-8999-999999999999"
	if _, err := s.CreateProject(ctx, ProjectCreateRequest{OwnerID: owner, ProjectID: projectID, IdempotencyID: "21212121-2121-4121-8121-212121212121"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAnalysis(ctx, AnalysisCreateRequest{OwnerID: owner, Analysis: catalogAnalysisFixture(now, projectID, "abababab-abab-4aba-8aba-abababababab"), IdempotencyID: "23232323-2323-4232-8232-232323232323"}); err != nil {
		t.Fatal(err)
	}
	targetID := "24242424-2424-4242-8242-242424242424"
	if _, err := s.CreateTarget(ctx, TargetCreateRequest{OwnerID: owner, Target: catalogTargetFixture(targetID, 1), IdempotencyID: "25252525-2525-4252-8252-252525252525"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTargetObservation(ctx, TargetObservationCreateRequest{OwnerID: owner, ObservationID: "26262626-2626-4262-8262-262626262626", Observation: coreexecution.TargetObservation{TargetID: targetID, TargetRevision: 1, ObservedAt: now, State: "ready"}, IdempotencyID: "27272727-2727-4272-8272-272727272727"}); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name  string
		query string
		args  []any
		read  func() error
	}{
		{"project", `UPDATE core_execution_projects SET snapshot_json='{"tampered":true}'::jsonb WHERE owner_id=$1 AND project_id=$2`, []any{owner, projectID}, func() error { _, err := s.GetProject(ctx, owner, projectID); return err }},
		{"analysis", `ALTER TABLE core_execution_analyses DISABLE TRIGGER core_execution_analyses_immutable`, nil, func() error { _, err := s.GetAnalysis(ctx, owner, "abababab-abab-4aba-8aba-abababababab"); return err }},
		{"target", `ALTER TABLE core_execution_targets DISABLE TRIGGER core_execution_targets_immutable`, nil, func() error { _, err := s.GetTarget(ctx, owner, targetID, 1); return err }},
		{"observation", `UPDATE core_execution_target_observations SET snapshot_json='{"tampered":true}'::jsonb WHERE owner_id=$1 AND observation_id=$2`, []any{owner, "26262626-2626-4262-8262-262626262626"}, func() error {
			_, err := s.GetTargetObservation(ctx, owner, "26262626-2626-4262-8262-262626262626")
			return err
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if mutation.name == "analysis" || mutation.name == "target" {
				table := map[string]string{"analysis": "core_execution_analyses", "target": "core_execution_targets"}[mutation.name]
				trigger := map[string]string{"analysis": "core_execution_analyses_immutable", "target": "core_execution_targets_immutable"}[mutation.name]
				defer func() {
					_, _ = store.DB().ExecContext(ctx, `ALTER TABLE `+table+` ENABLE TRIGGER `+trigger)
				}()
				if _, err := store.DB().ExecContext(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER `+trigger); err != nil {
					t.Fatal(err)
				}
				query := `UPDATE ` + table + ` SET snapshot_json='{"tampered":true}'::jsonb WHERE owner_id=$1`
				if mutation.name == "analysis" {
					query += ` AND analysis_id=$2`
				} else {
					query += ` AND target_id=$2`
				}
				if _, err := store.DB().ExecContext(ctx, query, owner, map[string]string{"analysis": "abababab-abab-4aba-8aba-abababababab", "target": targetID}[mutation.name]); err != nil {
					t.Fatal(err)
				}
			} else if mutation.name == "observation" {
				if _, err := store.DB().ExecContext(ctx, `ALTER TABLE core_execution_target_observations DISABLE TRIGGER core_execution_target_observations_immutable`); err != nil {
					t.Fatal(err)
				}
				defer func() {
					_, _ = store.DB().ExecContext(ctx, `ALTER TABLE core_execution_target_observations ENABLE TRIGGER core_execution_target_observations_immutable`)
				}()
				if _, err := store.DB().ExecContext(ctx, mutation.query, mutation.args...); err != nil {
					t.Fatal(err)
				}
			} else if _, err := store.DB().ExecContext(ctx, mutation.query, mutation.args...); err != nil {
				t.Fatal(err)
			}
			if err := mutation.read(); !errors.Is(err, ErrExecutionStoreDrift) {
				t.Fatalf("tamper read err=%v", err)
			}
		})
	}
}

func TestExecutionCatalogPostgresObservationImmutableTrigger(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	owner := "@catalog-observation-immutable:example.org"
	targetID := "35353535-3535-4535-8535-353535353535"
	observationID := "36363636-3636-4636-8636-363636363636"
	if _, err := s.CreateTarget(ctx, TargetCreateRequest{OwnerID: owner, Target: catalogTargetFixture(targetID, 1), IdempotencyID: "37373737-3737-4737-8737-373737373737"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTargetObservation(ctx, TargetObservationCreateRequest{OwnerID: owner, ObservationID: observationID, Observation: coreexecution.TargetObservation{TargetID: targetID, TargetRevision: 1, ObservedAt: now, State: "ready"}, IdempotencyID: "38383838-3838-4838-8838-383838383838"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_target_observations SET snapshot_json='{"tampered":true}'::jsonb WHERE owner_id=$1 AND observation_id=$2`, owner, observationID); err == nil {
		t.Fatal("observation UPDATE bypassed immutable trigger")
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM core_execution_target_observations WHERE owner_id=$1 AND observation_id=$2`, owner, observationID); err == nil {
		t.Fatal("observation DELETE bypassed immutable trigger")
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_target_observations WHERE owner_id=$1 AND observation_id=$2`, owner, observationID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("observation count=%d err=%v", count, err)
	}
}
