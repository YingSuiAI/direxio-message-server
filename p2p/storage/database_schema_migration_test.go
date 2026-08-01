package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	channelsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/channels"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

type executionV2GraphFixture struct {
	Owner          string
	ProjectID      string
	AnalysisID     string
	TargetID       string
	PlanID         string
	PlanRevision   int64
	PlanDigest     string
	RunRevision    int64
	StageID        string
	StageRevision  int64
	StageDigest    string
	TargetRevision int64
	TargetDigest   string
	RunID          string
	TaskID         string
	ConfirmationID string
}

// frozenUpstreamMigrationVersions is intentionally a literal copy of the
// registrations in origin/main through v77.  V78 is the sole branch-local
// registration: a clean install must not silently grow an upgrade chain.
var frozenUpstreamMigrationVersions = []string{
	"p2p: integrated appservice tables v1",
	"p2p: integrated appservice tables v2",
	"p2p: integrated appservice tables v3",
	"p2p: integrated appservice tables v4 member avatars",
	"p2p: integrated appservice tables v5 product mute state",
	"p2p: integrated appservice tables v6 member join order",
	"p2p: integrated appservice tables v7 portal matrix device",
	"p2p: integrated appservice tables v11 channel comment replies",
	"p2p: integrated appservice tables v12 channel comment media",
	"p2p: integrated appservice tables v13 event outbox",
	"p2p: integrated appservice tables v14 channel invite grants",
	"p2p: drop legacy message mirror table v15",
	"p2p: unique contact peer v16",
	"p2p: product conversations v17",
	"p2p: conversation peer mxid v18",
	"p2p: backfill product conversations v19",
	"p2p: conversation last message v20",
	"p2p: member requester node v21",
	"p2p: contact avatars v22",
	"p2p: call lifecycle fields v23",
	"p2p: contact request remark v24",
	"p2p: owner scoped member indexes v25",
	"p2p: public channel visibility index v26",
	"p2p: event dedupe key v27",
	"p2p: contact display name override v28",
	"p2p: portal agent config json v29",
	"p2p: owner blocks v30",
	"p2p: official plugins v31",
	"p2p: plugin secrets v32",
	"p2p: system reports v33",
	"p2p: portal client build v34",
	"p2p: recoverable operations v35",
	"p2p: recoverable operation claims v36",
	"p2p: operation base generations v37",
	"p2p: legacy agent invocation reservations v38",
	"p2p: authoritative read marker order v73",
	"p2p: channel favorite reaction backfill v74",
	"p2p: projected reaction event identity v75",
	"p2p: durable native agent turns v77",
	"p2p: canonical Matrix member membership v76",
}

func newExecutionV2GraphFixture(t *testing.T, db *sql.DB) executionV2GraphFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := executionV2GraphFixture{
		Owner:          "@execution-v2-fixture:example.test",
		ProjectID:      "00000000-0000-4000-8000-000000000301",
		AnalysisID:     "00000000-0000-4000-8000-000000000302",
		TargetID:       "00000000-0000-4000-8000-000000000303",
		PlanID:         "00000000-0000-4000-8000-000000000304",
		PlanRevision:   1,
		PlanDigest:     strings.Repeat("c", 64),
		RunRevision:    1,
		StageID:        "00000000-0000-4000-8000-000000000305",
		StageRevision:  1,
		StageDigest:    strings.Repeat("a", 64),
		TargetRevision: 1,
		TargetDigest:   strings.Repeat("b", 64),
		RunID:          "00000000-0000-4000-8000-000000000306",
		TaskID:         "00000000-0000-4000-8000-000000000307",
		ConfirmationID: "00000000-0000-4000-8000-000000000308",
	}
	planDigest := f.PlanDigest
	stepDigest := strings.Repeat("d", 64)
	previewDigest := strings.Repeat("e", 64)
	graph := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO core_execution_projects(owner_id,project_id,project_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,'{"project":"fixture"}'::jsonb,$4,$4)`, []any{f.Owner, f.ProjectID, planDigest, now}},
		{`INSERT INTO core_execution_analyses(owner_id,analysis_id,project_id,analysis_digest,snapshot_json,created_at) VALUES($1,$2,$3,$4,'{"analysis":"fixture"}'::jsonb,$5)`, []any{f.Owner, f.AnalysisID, f.ProjectID, planDigest, now}},
		{`INSERT INTO core_execution_targets(owner_id,target_id,target_revision,target_digest,provider,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,$4,'fixture','{"target":"fixture"}'::jsonb,$5,$5)`, []any{f.Owner, f.TargetID, f.TargetRevision, f.TargetDigest, now}},
		{`INSERT INTO core_execution_plans(owner_id,plan_id,project_id,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,'{"plan":"fixture"}'::jsonb,$4,$4)`, []any{f.Owner, f.PlanID, f.ProjectID, now}},
		{`INSERT INTO core_execution_plan_revisions(owner_id,plan_id,plan_revision_id,revision,project_id,analysis_id,status,plan_digest,snapshot_json,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,'ready',$7,'{"revision":"fixture"}'::jsonb,$8,$9)`, []any{f.Owner, f.PlanID, "00000000-0000-4000-8000-000000000309", f.PlanRevision, f.ProjectID, f.AnalysisID, planDigest, now.Add(time.Hour), now}},
		{`INSERT INTO core_execution_plan_stages(owner_id,plan_id,plan_revision,stage_key,stage_revision,stage_digest,ordinal,status,snapshot_json) VALUES($1,$2,$3,'deploy',1,$4,1,'ready','{"stage":"fixture"}'::jsonb)`, []any{f.Owner, f.PlanID, f.PlanRevision, f.StageDigest}},
		{`INSERT INTO core_execution_plan_steps(owner_id,plan_id,plan_revision,stage_key,step_key,step_set,step_revision,step_digest,ordinal,status,snapshot_json) VALUES($1,$2,$3,'deploy','apply','forward',1,$4,1,'ready','{"step":"forward"}'::jsonb)`, []any{f.Owner, f.PlanID, f.PlanRevision, stepDigest}},
		{`INSERT INTO core_execution_plan_steps(owner_id,plan_id,plan_revision,stage_key,step_key,step_set,step_revision,step_digest,ordinal,status,snapshot_json) VALUES($1,$2,$3,'deploy','undo','rollback',1,$4,1,'ready','{"step":"rollback"}'::jsonb)`, []any{f.Owner, f.PlanID, f.PlanRevision, stepDigest}},
		{`INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at) VALUES($1,$2,$3::jsonb,'queued',$4,$4,$4)`, []any{f.TaskID, f.Owner, fmt.Sprintf(`{"kind":"execution_stage","idempotency_key":"fixture-stage","payload":{"execution_stage":{"plan_id":%q,"plan_revision":1,"plan_digest":%q,"run_id":%q,"run_revision":1,"stage_id":%q,"stage_revision":1,"stage_digest":%q,"target_id":%q,"target_revision":1,"target_digest":%q}}}`, f.PlanID, f.PlanDigest, f.RunID, f.StageID, f.StageDigest, f.TargetID, f.TargetDigest), now}},
		{`INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,preview_json,preview_digest,task_id,state,expires_at,created_at,updated_at) VALUES($1,$2,'execution:v2:remote_execution',$3,$4,$5,$6::jsonb,'{"preview":"fixture"}'::jsonb,$7,$8,'pending',$9,$10,$10)`, []any{f.ConfirmationID, f.Owner, f.TargetID, f.TargetRevision, planDigest, fmt.Sprintf(`{"plan_id":%q,"plan_revision":1,"plan_digest":%q,"run_id":%q,"run_revision":1,"stage_id":%q,"stage_revision":1,"stage_digest":%q,"target_id":%q,"target_revision":1,"target_digest":%q,"preview_digest":%q}`, f.PlanID, f.PlanDigest, f.RunID, f.StageID, f.StageDigest, f.TargetID, f.TargetDigest, previewDigest), previewDigest, f.TaskID, now.Add(time.Hour), now}},
		{`INSERT INTO core_execution_runs(owner_id,run_id,project_id,plan_id,plan_revision,deployment_id,purpose,plan_digest,run_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,'service',$7,$7,'{"run":"fixture"}'::jsonb,$8,$8)`, []any{f.Owner, f.RunID, f.ProjectID, f.PlanID, f.PlanRevision, "00000000-0000-4000-8000-000000000310", planDigest, now}},
		{`INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,deployment_id,operation,purpose,trigger_kind,plan_digest,status,schema_version,run_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,'execute','service','manual',$8,'pending','execution-run/v2',$8,'{"run":"fixture"}'::jsonb,$9,$9)`, []any{f.Owner, f.RunID, f.RunRevision, f.ProjectID, f.PlanID, f.PlanRevision, "00000000-0000-4000-8000-000000000310", planDigest, now}},
		{`INSERT INTO core_execution_deployments(owner_id,deployment_id,project_id,current_run_id,state,object_json,actual_json,created_at,updated_at) VALUES($1,$2,$3,$4,'pending','{"deployment":"fixture"}'::jsonb,'{}'::jsonb,$5,$5)`, []any{f.Owner, "00000000-0000-4000-8000-000000000310", f.ProjectID, f.RunID, now}},
		{`INSERT INTO core_execution_run_stages(owner_id,run_id,stage_id,project_id,plan_id,plan_revision,plan_digest,run_revision,plan_stage_key,stage_revision,plan_stage_digest,target_id,target_revision,target_digest,task_id,confirmation_id,ordinal,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'deploy',$9,$10,$11,$12,$13,$14,$15,1,'{"run_stage":"fixture"}'::jsonb)`, []any{f.Owner, f.RunID, f.StageID, f.ProjectID, f.PlanID, f.PlanRevision, f.PlanDigest, f.RunRevision, f.StageRevision, f.StageDigest, f.TargetID, f.TargetRevision, f.TargetDigest, f.TaskID, f.ConfirmationID}},
		{`UPDATE core_execution_deployments SET current_stage_id=$3,revision=revision+1,updated_at=clock_timestamp() WHERE owner_id=$1 AND deployment_id=$2`, []any{f.Owner, "00000000-0000-4000-8000-000000000310", f.StageID}},
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range graph {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert execution v2 fixture graph: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit execution v2 fixture graph: %v", err)
	}
	return f
}

func openExecutionV2Schema(t *testing.T) *DatabaseStore {
	t.Helper()
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(closeDB)
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestDatabaseStoreRejectsSQLiteConnectionString(t *testing.T) {
	ctx := context.Background()
	dbOpts := config.DatabaseOptions{ConnectionString: "file::memory:?cache=shared"}

	_, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)

	if err == nil || !strings.Contains(err.Error(), "SQLite") {
		t.Fatalf("expected SQLite connection string to be rejected, got %v", err)
	}
}

func TestExecutionV2RunStageTaskBindingExactScope(t *testing.T) {
	ctx := context.Background()
	store := openExecutionV2Schema(t)
	f := newExecutionV2GraphFixture(t, store.DB())
	fields := []struct {
		name string
		bad  string
		good string
	}{
		{"plan_id", "00000000-0000-4000-8000-000000000401", f.PlanID},
		{"plan_revision", "2", "1"},
		{"plan_digest", strings.Repeat("f", 64), f.PlanDigest},
		{"run_id", "00000000-0000-4000-8000-000000000402", f.RunID},
		{"run_revision", "2", "1"},
		{"stage_id", "00000000-0000-4000-8000-000000000403", f.StageID},
		{"stage_revision", "2", "1"},
		{"stage_digest", strings.Repeat("f", 64), f.StageDigest},
		{"target_id", "00000000-0000-4000-8000-000000000404", f.TargetID},
		{"target_revision", "2", "1"},
		{"target_digest", strings.Repeat("f", 64), f.TargetDigest},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			setTaskPayloadField := func(value string) {
				query := fmt.Sprintf(`UPDATE agent_tasks SET spec_json=jsonb_set(spec_json,'{payload,execution_stage,%s}',to_jsonb($1::text),false) WHERE owner_id=$2 AND task_id=$3`, field.name)
				if _, err := store.DB().ExecContext(ctx, query, value, f.Owner, f.TaskID); err != nil {
					t.Fatalf("set task %s=%q: %v", field.name, value, err)
				}
			}
			setTaskPayloadField(field.bad)
			_, err := store.DB().ExecContext(ctx, `UPDATE core_execution_run_stages SET status='queued' WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, f.Owner, f.RunID, f.StageID)
			if err == nil || !strings.Contains(err.Error(), "execution stage task immutable scope mismatch") {
				t.Fatalf("mismatched task %s accepted or wrong guard: %v", field.name, err)
			}
			setTaskPayloadField(field.good)
			if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_run_stages SET status='queued' WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, f.Owner, f.RunID, f.StageID); err != nil {
				t.Fatalf("exact task %s binding rejected: %v", field.name, err)
			}
		})
	}
}

func TestExecutionV2RunStageConfirmationBindingExactScope(t *testing.T) {
	ctx := context.Background()
	store := openExecutionV2Schema(t)
	f := newExecutionV2GraphFixture(t, store.DB())
	fields := []struct {
		name string
		bad  string
		good string
	}{
		{"plan_id", "00000000-0000-4000-8000-000000000411", f.PlanID},
		{"plan_revision", "2", "1"},
		{"plan_digest", strings.Repeat("f", 64), f.PlanDigest},
		{"run_id", "00000000-0000-4000-8000-000000000412", f.RunID},
		{"run_revision", "2", "1"},
		{"stage_id", "00000000-0000-4000-8000-000000000413", f.StageID},
		{"stage_revision", "2", "1"},
		{"stage_digest", strings.Repeat("f", 64), f.StageDigest},
		{"target_id", "00000000-0000-4000-8000-000000000414", f.TargetID},
		{"target_revision", "2", "1"},
		{"target_digest", strings.Repeat("f", 64), f.TargetDigest},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			query := fmt.Sprintf(`UPDATE agent_confirmations SET binding_json=jsonb_set(binding_json,'{%s}',to_jsonb($1::text),false) WHERE owner_id=$2 AND confirmation_id=$3`, field.name)
			if _, err := store.DB().ExecContext(ctx, query, field.bad, f.Owner, f.ConfirmationID); err != nil {
				t.Fatalf("set confirmation %s=%q: %v", field.name, field.bad, err)
			}
			_, err := store.DB().ExecContext(ctx, `UPDATE core_execution_run_stages SET status='running' WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, f.Owner, f.RunID, f.StageID)
			if err == nil || !strings.Contains(err.Error(), "execution confirmation binding scope mismatch") {
				t.Fatalf("mismatched confirmation %s accepted or wrong guard: %v", field.name, err)
			}
			if _, err := store.DB().ExecContext(ctx, query, field.good, f.Owner, f.ConfirmationID); err != nil {
				t.Fatalf("restore confirmation %s=%q: %v", field.name, field.good, err)
			}
			if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_run_stages SET status='running' WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, f.Owner, f.RunID, f.StageID); err != nil {
				t.Fatalf("exact confirmation %s binding rejected: %v", field.name, err)
			}
		})
	}
}

func TestExecutionV2ConfirmationPreviewDigestBindingGuard(t *testing.T) {
	ctx := context.Background()
	store := openExecutionV2Schema(t)
	f := newExecutionV2GraphFixture(t, store.DB())
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"binding_preview_digest", `UPDATE agent_confirmations SET binding_json=jsonb_set(binding_json,'{preview_digest}',to_jsonb($1::text),false) WHERE owner_id=$2 AND confirmation_id=$3`},
		{"column_preview_digest", `UPDATE agent_confirmations SET preview_digest=$1 WHERE owner_id=$2 AND confirmation_id=$3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Repeat("f", 64)
			if _, err := store.DB().ExecContext(ctx, tc.query, bad, f.Owner, f.ConfirmationID); err == nil || !strings.Contains(err.Error(), "execution confirmation preview/binding mismatch") {
				t.Fatalf("preview mismatch accepted or wrong guard: %v", err)
			}
			if _, err := store.DB().ExecContext(ctx, `UPDATE agent_confirmations SET updated_at=clock_timestamp() WHERE owner_id=$1 AND confirmation_id=$2`, f.Owner, f.ConfirmationID); err != nil {
				t.Fatalf("exact preview digest binding rejected: %v", err)
			}
		})
	}
}

func TestExecutionV2RunRevisionHistoryAndLifecycleSchema(t *testing.T) {
	ctx := context.Background()
	store := openExecutionV2Schema(t)
	f := newExecutionV2GraphFixture(t, store.DB())

	var historyCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_run_revisions WHERE owner_id=$1 AND run_id=$2 AND revision=$3`, f.Owner, f.RunID, f.RunRevision).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 1 {
		t.Fatalf("run revision history rows = %d, want 1", historyCount)
	}

	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	completed := started.Add(30 * time.Second)
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE core_execution_runs SET revision=2,started_at=$1,completed_at=$2,terminal_reason='completed',current_stage='deploy' WHERE owner_id=$3 AND run_id=$4`, started, completed, f.Owner, f.RunID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("advance current run projection revision: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,operation,purpose,trigger_kind,deployment_id,plan_digest,current_stage,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at) SELECT owner_id,run_id,revision,project_id,plan_id,plan_revision,operation,purpose,trigger_kind,deployment_id,plan_digest,current_stage,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("append current run revision: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit current run revision: %v", err)
	}
	var gotStarted, gotCompleted time.Time
	var terminalReason, currentStage string
	if err := store.DB().QueryRowContext(ctx, `SELECT started_at,completed_at,terminal_reason,current_stage FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID).Scan(&gotStarted, &gotCompleted, &terminalReason, &currentStage); err != nil {
		t.Fatal(err)
	}
	if !gotStarted.Equal(started) || !gotCompleted.Equal(completed) || terminalReason != "completed" || currentStage != "deploy" {
		t.Fatalf("run lifecycle roundtrip = %v, %v, %q, %q", gotStarted, gotCompleted, terminalReason, currentStage)
	}
	var pinnedRevision int64
	if err := store.DB().QueryRowContext(ctx, `SELECT run_revision FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, f.Owner, f.RunID, f.StageID).Scan(&pinnedRevision); err != nil {
		t.Fatal(err)
	}
	if pinnedRevision != f.RunRevision {
		t.Fatalf("stage run revision = %d, want %d", pinnedRevision, f.RunRevision)
	}

	stageInsert := `INSERT INTO core_execution_run_stages(owner_id,run_id,stage_id,project_id,plan_id,plan_revision,plan_digest,run_revision,plan_stage_key,stage_revision,plan_stage_digest,ordinal,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'deploy',1,$9,$10,'{}'::jsonb)`
	if _, err := store.DB().ExecContext(ctx, stageInsert, f.Owner, f.RunID, "00000000-0000-4000-8000-000000000399", f.ProjectID, f.PlanID, f.PlanRevision, f.PlanDigest, int64(3), f.StageDigest, int64(2)); err == nil {
		t.Fatal("stage referencing missing run revision was accepted")
	}
	if _, err := store.DB().ExecContext(ctx, stageInsert, f.Owner, f.RunID, "00000000-0000-4000-8000-000000000398", f.ProjectID, f.PlanID, f.PlanRevision, f.PlanDigest, int64(0), f.StageDigest, int64(3)); err == nil {
		t.Fatal("stage referencing invalid run revision was accepted")
	}

	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_run_revisions SET status='running' WHERE owner_id=$1 AND run_id=$2 AND revision=$3`, f.Owner, f.RunID, f.RunRevision); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("run revision history mutation = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM core_execution_run_revisions WHERE owner_id=$1 AND run_id=$2 AND revision=$3`, f.Owner, f.RunID, f.RunRevision); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("run revision history delete = %v", err)
	}

	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,operation,purpose,trigger_kind,deployment_id,plan_digest,current_stage,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at) SELECT owner_id,run_id,revision,project_id,plan_id,plan_revision,'destroy',purpose,trigger_kind,deployment_id,plan_digest,current_stage,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err == nil || !strings.Contains(err.Error(), "revision identity mismatch") {
		t.Fatalf("forged run revision identity = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,operation,purpose,trigger_kind,deployment_id,plan_digest,current_stage,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at) SELECT owner_id,run_id,revision+1,project_id,plan_id,plan_revision,operation,purpose,trigger_kind,deployment_id,plan_digest,current_stage,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err == nil || !strings.Contains(err.Error(), "future or non-current") {
		t.Fatalf("future run revision = %v", err)
	}
	lifecycleTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycleTx.ExecContext(ctx, `UPDATE core_execution_runs SET revision=revision+1,current_stage='verify',updated_at=clock_timestamp() WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err != nil {
		_ = lifecycleTx.Rollback()
		t.Fatalf("advance run for lifecycle history mismatch: %v", err)
	}
	if _, err := lifecycleTx.ExecContext(ctx, `INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,deployment_id,operation,purpose,trigger_kind,plan_digest,current_stage,current_stage_id,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at) SELECT owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,deployment_id,operation,purpose,trigger_kind,plan_digest,current_stage||'-forged',current_stage_id,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err == nil || !strings.Contains(err.Error(), "revision identity mismatch") {
		_ = lifecycleTx.Rollback()
		t.Fatalf("forged run lifecycle history = %v", err)
	}
	_ = lifecycleTx.Rollback()
	gapTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gapTx.ExecContext(ctx, `UPDATE core_execution_runs SET revision=revision+1 WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err != nil {
		_ = gapTx.Rollback()
		t.Fatalf("advance run without history: %v", err)
	}
	if err := gapTx.Commit(); err == nil || !strings.Contains(err.Error(), "requires matching history") {
		t.Fatalf("gapped run revision = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_runs SET revision=1 WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err == nil || !strings.Contains(err.Error(), "revision is not consecutive") {
		t.Fatalf("run revision rollback = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_runs SET revision=revision+1,operation='destroy' WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err == nil || !strings.Contains(err.Error(), "identity/lifecycle") {
		t.Fatalf("run identity mutation = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_runs SET revision=revision+1,completed_at=completed_at+interval '1 second' WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err == nil || !strings.Contains(err.Error(), "identity/lifecycle") {
		t.Fatalf("run terminal lifecycle mutation = %v", err)
	}

	rollbackID := "00000000-0000-4000-8000-000000000397"
	rollbackTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rollbackTx.ExecContext(ctx, `INSERT INTO core_execution_runs(owner_id,run_id,project_id,plan_id,plan_revision,rollback_of_run_id,operation,purpose,trigger_kind,plan_digest,run_digest) VALUES($1,$2,$3,$4,$5,$6,'rollback','job','rollback',$7,$7)`, f.Owner, rollbackID, f.ProjectID, f.PlanID, f.PlanRevision, f.RunID, f.PlanDigest); err != nil {
		_ = rollbackTx.Rollback()
		t.Fatalf("rollback trigger kind rejected: %v", err)
	}
	if _, err := rollbackTx.ExecContext(ctx, `INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,operation,purpose,trigger_kind,plan_digest,current_stage,status,terminal_reason,schema_version,run_digest,snapshot_json,created_at,updated_at) SELECT owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,operation,purpose,trigger_kind,plan_digest,current_stage,status,terminal_reason,schema_version,run_digest,snapshot_json,created_at,updated_at FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, f.Owner, rollbackID); err != nil {
		_ = rollbackTx.Rollback()
		t.Fatalf("append rollback run revision: %v", err)
	}
	if err := rollbackTx.Commit(); err != nil {
		t.Fatalf("commit rollback run: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_execution_runs(owner_id,run_id,project_id,plan_id,plan_revision,operation,purpose,trigger_kind,plan_digest,run_digest) VALUES($1,$2,$3,$4,$5,'execute','job','invalid',$6,$6)`, f.Owner, "00000000-0000-4000-8000-000000000396", f.ProjectID, f.PlanID, f.PlanRevision, f.PlanDigest); err == nil {
		t.Fatal("invalid trigger kind was accepted")
	}
}

func TestExecutionV2DispatchReconciliationAndDeploymentTamperFences(t *testing.T) {
	ctx := context.Background()
	store := openExecutionV2Schema(t)
	f := newExecutionV2GraphFixture(t, store.DB())
	const deploymentID = "00000000-0000-4000-8000-000000000310"
	const attemptID = "00000000-0000-4000-8000-000000000381"
	const receiptID = "00000000-0000-4000-8000-000000000382"
	const leaseID = "00000000-0000-4000-8000-000000000383"
	const token = "00000000-0000-4000-8000-000000000384"
	const intentID = "00000000-0000-4000-8000-000000000385"
	requestDigest, fenceDigest, providerOperationID := strings.Repeat("1", 64), strings.Repeat("2", 64), "provider-operation-fixture"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_execution_step_attempts(owner_id,attempt_id,run_id,stage_id,project_id,plan_id,plan_revision,plan_stage_key,step_key,step_set,step_revision,step_digest,attempt_no,status,input_digest,output_digest,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,$7,'deploy','apply','forward',1,$8,1,'uncertain',$9,$9,'{}'::jsonb)`, f.Owner, attemptID, f.RunID, f.StageID, f.ProjectID, f.PlanID, f.PlanRevision, strings.Repeat("d", 64), requestDigest); err != nil {
		t.Fatalf("seed uncertain step attempt: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_execution_receipts(owner_id,receipt_id,run_id,attempt_id,provider_operation_id,idempotency_digest,request_digest,fence_digest,response_digest,status,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'uncertain','{}'::jsonb)`, f.Owner, receiptID, f.RunID, attemptID, providerOperationID, strings.Repeat("3", 64), requestDigest, fenceDigest, strings.Repeat("4", 64)); err != nil {
		t.Fatalf("seed uncertain receipt: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_execution_target_mutation_leases(owner_id,target_id,target_revision,lease_id,run_id,stage_id,provider_operation_id,receipt_id,token,epoch,revision,status,schema_version,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,1,1,'uncertain','execution-target-lease/v2',clock_timestamp())`, f.Owner, f.TargetID, f.TargetRevision, leaseID, f.RunID, f.StageID, providerOperationID, receiptID, token); err != nil {
		t.Fatalf("seed uncertain target lease: %v", err)
	}
	const lateCommandReceiptID = "00000000-0000-4000-8000-000000000387"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_execution_receipts(owner_id,receipt_id,run_id,attempt_id,idempotency_digest,request_digest,fence_digest,status,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,$7,'accepted','{}'::jsonb)`, f.Owner, lateCommandReceiptID, f.RunID, attemptID, strings.Repeat("8", 64), strings.Repeat("9", 64), strings.Repeat("a", 64)); err != nil {
		t.Fatalf("seed receipt for atomic uncertain command evidence: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_receipts SET provider_operation_id='provider-operation-late',command_id='command-late',response_digest=$1,status='uncertain',revision=revision+1 WHERE owner_id=$2 AND receipt_id=$3 AND status='accepted'`, strings.Repeat("b", 64), f.Owner, lateCommandReceiptID); err != nil {
		t.Fatalf("atomically terminalize uncertain receipt with command id: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_receipts SET command_id='forged-command',revision=revision+1 WHERE owner_id=$1 AND receipt_id=$2`, f.Owner, lateCommandReceiptID); err == nil || !strings.Contains(err.Error(), "terminal evidence") {
		t.Fatalf("uncertain receipt command rewrite = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_execution_dispatch_intents(owner_id,intent_id,run_id,stage_id,attempt_id,receipt_id,task_id,task_lease_epoch,target_id,target_revision,target_digest,plan_id,plan_revision,plan_digest,stage_revision,stage_digest,step_key,step_set,step_revision,step_digest,attempt_no,lease_id,lease_token,lease_epoch,request_digest,fence_digest,status,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,$7,1,$8,$9,$10,$11,$12,$13,$14,$15,'apply','forward',1,$16,1,$17,$18,1,$19,$20,'uncertain','{"frozen":true}'::jsonb)`, f.Owner, intentID, f.RunID, f.StageID, attemptID, receiptID, f.TaskID, f.TargetID, f.TargetRevision, f.TargetDigest, f.PlanID, f.PlanRevision, f.PlanDigest, f.StageRevision, f.StageDigest, strings.Repeat("d", 64), leaseID, token, requestDigest, fenceDigest); err != nil {
		t.Fatalf("seed frozen dispatch intent: %v", err)
	}
	for _, query := range []string{
		`UPDATE core_execution_dispatch_intents SET snapshot_json='{"frozen":false}'::jsonb,revision=revision+1 WHERE owner_id='@execution-v2-fixture:example.test' AND intent_id='00000000-0000-4000-8000-000000000385'`,
		`UPDATE core_execution_dispatch_intents SET status='accepted',revision=revision+1 WHERE owner_id='@execution-v2-fixture:example.test' AND intent_id='00000000-0000-4000-8000-000000000385'`,
	} {
		if _, err := store.DB().ExecContext(ctx, query); err == nil || !strings.Contains(err.Error(), "execution dispatch intent") {
			t.Fatalf("forged dispatch mutation = %v", err)
		}
	}
	uncertainTx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = uncertainTx.ExecContext(ctx, `UPDATE core_execution_run_stages SET status='uncertain',revision=revision+1,started_at=clock_timestamp(),completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND status='blocked'`, f.Owner, f.RunID, f.StageID); err != nil {
		_ = uncertainTx.Rollback()
		t.Fatalf("seed uncertain stage: %v", err)
	}
	if _, err = uncertainTx.ExecContext(ctx, `UPDATE core_execution_runs SET status='uncertain',current_stage='deploy',current_stage_id=$3,terminal_reason='fixture_uncertain',started_at=clock_timestamp(),completed_at=clock_timestamp(),revision=revision+1,updated_at=clock_timestamp() WHERE owner_id=$1 AND run_id=$2 AND status='pending'`, f.Owner, f.RunID, f.StageID); err != nil {
		_ = uncertainTx.Rollback()
		t.Fatalf("seed uncertain run: %v", err)
	}
	if _, err = uncertainTx.ExecContext(ctx, `INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,deployment_id,operation,purpose,trigger_kind,plan_digest,current_stage,current_stage_id,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at) SELECT owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,deployment_id,operation,purpose,trigger_kind,plan_digest,current_stage,current_stage_id,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, f.Owner, f.RunID); err != nil {
		_ = uncertainTx.Rollback()
		t.Fatalf("append uncertain run revision: %v", err)
	}
	if err = uncertainTx.Commit(); err != nil {
		t.Fatalf("commit uncertain run: %v", err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE core_execution_runs SET status='running',terminal_reason='',completed_at=NULL,revision=revision+1,updated_at=clock_timestamp() WHERE owner_id=$1 AND run_id=$2 AND status='uncertain'`, f.Owner, f.RunID); err == nil || !strings.Contains(err.Error(), "identity/lifecycle") {
		t.Fatalf("ordinary uncertain run reopen = %v", err)
	}
	resolution := `INSERT INTO core_execution_reconciliation_resolutions(owner_id,run_id,stage_id,lease_id,token,epoch,receipt_id,provider_operation_id,request_digest,outcome,outcome_digest) VALUES($1,$2,$3,$4,$5,1,$6,$7,$8,'succeeded',$9)`
	if _, err := store.DB().ExecContext(ctx, resolution, f.Owner, f.RunID, f.StageID, leaseID, token, receiptID, providerOperationID, requestDigest, strings.Repeat("5", 64)); err != nil {
		t.Fatalf("append exact reconciliation resolution: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_reconciliation_resolutions SET outcome='failed' WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND lease_id=$4 AND epoch=1`, f.Owner, f.RunID, f.StageID, leaseID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("reconciliation update = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM core_execution_reconciliation_resolutions WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND lease_id=$4 AND epoch=1`, f.Owner, f.RunID, f.StageID, leaseID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("reconciliation delete = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, resolution, f.Owner, f.RunID, f.StageID, leaseID, token, receiptID, providerOperationID, strings.Repeat("6", 64), strings.Repeat("5", 64)); err == nil || !strings.Contains(err.Error(), "reconciliation resolution") {
		t.Fatalf("mismatched reconciliation request digest = %v", err)
	}

	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_deployments SET project_id=$3,revision=revision+1 WHERE owner_id=$1 AND deployment_id=$2`, f.Owner, deploymentID, "00000000-0000-4000-8000-000000000399"); err == nil || !strings.Contains(err.Error(), "execution deployment") {
		t.Fatalf("deployment identity rewrite = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_deployments SET state='queued',revision=revision+2 WHERE owner_id=$1 AND deployment_id=$2`, f.Owner, deploymentID); err == nil || !strings.Contains(err.Error(), "revision is not consecutive") {
		t.Fatalf("deployment revision skip = %v", err)
	}
	for _, state := range []string{"queued", "running", "succeeded"} {
		if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_deployments SET state=$1,revision=revision+1,updated_at=clock_timestamp() WHERE owner_id=$2 AND deployment_id=$3`, state, f.Owner, deploymentID); err != nil {
			t.Fatalf("advance deployment to %s: %v", state, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_deployments SET state='running',revision=revision+1,updated_at=clock_timestamp() WHERE owner_id=$1 AND deployment_id=$2`, f.Owner, deploymentID); err == nil || !strings.Contains(err.Error(), "execution deployment") {
		t.Fatalf("terminal deployment regression = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM core_execution_deployments WHERE owner_id=$1 AND deployment_id=$2`, f.Owner, deploymentID); err == nil || !strings.Contains(err.Error(), "cannot be deleted") {
		t.Fatalf("deployment delete = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_execution_deployment_events(owner_id,deployment_id,event_id,sequence,event_digest,event_json) VALUES($1,$2,'00000000-0000-4000-8000-000000000386',1,$3,'{}'::jsonb)`, f.Owner, deploymentID, strings.Repeat("7", 64)); err != nil {
		t.Fatalf("append deployment event: %v", err)
	}
	for _, query := range []string{
		`UPDATE core_execution_deployment_events SET event_json='{"changed":true}'::jsonb WHERE owner_id='@execution-v2-fixture:example.test' AND deployment_id='00000000-0000-4000-8000-000000000310' AND sequence=1`,
		`DELETE FROM core_execution_deployment_events WHERE owner_id='@execution-v2-fixture:example.test' AND deployment_id='00000000-0000-4000-8000-000000000310' AND sequence=1`,
	} {
		if _, err := store.DB().ExecContext(ctx, query); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("forged deployment event mutation = %v", err)
		}
	}
}

func TestFreshAgentExecutionV2SchemaRegistersOnceWithoutV1Ledgers(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const version = "p2p: agent and execution v2 fresh schema v78"
	for _, run := range []int{1, 2} {
		if run == 2 {
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("reopen migration: %v", err)
			}
		}
		var count int
		if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM db_migrations WHERE version=$1`, version).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("fresh schema registration count = %d, want 1", count)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatalf("close/reopen fresh schema: %v", err)
	}
	defer store.Close()
	expectedMigrations := make(map[string]struct{}, len(frozenUpstreamMigrationVersions)+1)
	for _, frozen := range frozenUpstreamMigrationVersions {
		expectedMigrations[frozen] = struct{}{}
	}
	expectedMigrations[version] = struct{}{}
	rows, err := store.DB().QueryContext(ctx, `SELECT version FROM db_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actualMigrations := make(map[string]struct{}, len(expectedMigrations))
	for rows.Next() {
		var actual string
		if err := rows.Scan(&actual); err != nil {
			t.Fatal(err)
		}
		actualMigrations[actual] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(actualMigrations) != len(expectedMigrations) {
		t.Fatalf("migration registrations = %d, want exactly frozen upstream + v78 = %d: %#v", len(actualMigrations), len(expectedMigrations), actualMigrations)
	}
	for expected := range expectedMigrations {
		if _, ok := actualMigrations[expected]; !ok {
			t.Fatalf("required migration %q is not registered", expected)
		}
	}
	for actual := range actualMigrations {
		if _, ok := expectedMigrations[actual]; !ok {
			t.Fatalf("unexpected migration registration %q", actual)
		}
	}
	for _, table := range []string{
		"p2p_agent_model_profiles", "p2p_agent_secrets", "core_execution_secrets", "core_execution_secret_parameter_intents", "agent_tasks",
		"core_aws_credentials", "core_aws_credential_current", "core_aws_replays", "core_execution_projects", "core_execution_source_artifacts", "core_execution_runs", "core_execution_run_revisions",
		"core_execution_deployments", "core_execution_deployment_events",
		"core_execution_runtime_concurrency", "core_execution_reconciliation_resolutions",
		"p2p_agent_model_profile_syncs", "p2p_agent_model_profile_deletes",
		"p2p_agent_schedule_mutations", "p2p_agent_schedule_confirmations", "p2p_agent_schedule_runs",
		"p2p_native_agent_conversation_mutations", "p2p_native_agent_memory_turns", "p2p_native_agent_memory_embeddings",
		"p2p_native_agent_knowledge_sources", "p2p_native_agent_knowledge_uploads",
	} {
		var present bool
		if err := store.DB().QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&present); err != nil || !present {
			t.Fatalf("fresh V2 table %s present=%v err=%v", table, present, err)
		}
	}
	var channelPostVisibilityColumn, channelPostVisibilityIndex bool
	if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='p2p_channel_posts' AND column_name='visibility')`).Scan(&channelPostVisibilityColumn); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT to_regclass('p2p_channel_posts_channel_visibility_idx') IS NOT NULL`).Scan(&channelPostVisibilityIndex); err != nil {
		t.Fatal(err)
	}
	if !channelPostVisibilityColumn || !channelPostVisibilityIndex {
		t.Fatalf("fresh v78 channel post visibility column=%v index=%v", channelPostVisibilityColumn, channelPostVisibilityIndex)
	}
	for _, forbidden := range []string{
		"core_aws_changes", "core_aws_plans", "core_aws_ec2_provisions",
		"core_aws_ec2_provision_events", "core_aws_ec2_provision_event_counters", "core_aws_ec2_provision_mutation_leases",
		"core_workloads", "core_workload_plans", "core_workload_quotes", "core_workload_operations", "core_workload_events", "core_workload_event_counters", "core_workload_idempotency",
		"core_deployments", "core_deployment_events", "core_deployment_event_counters",
	} {
		var present bool
		if err := store.DB().QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, forbidden).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if present {
			t.Fatalf("V1 table %s must not be in a fresh V2 schema", forbidden)
		}
	}
	for _, column := range []string{"owner_id", "deployment_id", "project_id", "current_run_id", "current_stage_id", "release_id"} {
		var present bool
		if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='core_execution_deployments' AND column_name=$1)`, column).Scan(&present); err != nil || !present {
			t.Fatalf("core_execution_deployments.%s present=%v err=%v", column, present, err)
		}
	}
	for _, column := range []string{"plan_digest", "run_revision", "target_revision"} {
		var present bool
		if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='core_execution_run_stages' AND column_name=$1)`, column).Scan(&present); err != nil || !present {
			t.Fatalf("core_execution_run_stages.%s present=%v err=%v", column, present, err)
		}
	}
	for _, column := range []string{"started_at", "completed_at", "terminal_reason", "current_stage"} {
		var present bool
		if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='core_execution_runs' AND column_name=$1)`, column).Scan(&present); err != nil || !present {
			t.Fatalf("core_execution_runs.%s present=%v err=%v", column, present, err)
		}
	}
	for _, object := range []string{
		"core_execution_deployments_reciprocal_guard",
		"core_execution_runs_deployment_reciprocal_guard",
		"core_execution_artifacts_scope_guard",
		"core_execution_service_bindings_scope_guard",
		"core_execution_events_append_only",
		"core_execution_target_observations_immutable",
		"core_execution_source_artifacts_immutable",
		"core_execution_dispatch_intents_immutable",
		"core_execution_receipt_terminal_evidence_guard",
		"core_execution_reconciliation_scope_guard",
		"core_execution_deployments_immutable",
		"core_execution_deployment_events_append_only",
	} {
		var present bool
		if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname=$1 AND NOT tgisinternal)`, object).Scan(&present); err != nil || !present {
			t.Fatalf("fresh V2 trigger/index %s present=%v err=%v", object, present, err)
		}
	}
	for _, forbiddenColumn := range []string{"plan_id", "plan_revision", "run_id", "attempt_id"} {
		var present bool
		if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='core_execution_source_artifacts' AND column_name=$1)`, forbiddenColumn).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if present {
			t.Fatalf("pre-plan source artifact unexpectedly has %s", forbiddenColumn)
		}
	}
	var fkTarget string
	if err := store.DB().QueryRowContext(ctx, `SELECT c.confrelid::regclass::text FROM pg_constraint c JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=ANY(c.conkey) WHERE c.conrelid='core_execution_service_bindings'::regclass AND c.contype='f' AND a.attname='deployment_id'`).Scan(&fkTarget); err != nil {
		t.Fatal(err)
	}
	if fkTarget != "core_execution_deployments" {
		t.Fatalf("service binding deployment FK target = %q", fkTarget)
	}
	var eventDeleteRule string
	if err := store.DB().QueryRowContext(ctx, `SELECT confdeltype::text FROM pg_constraint WHERE conrelid='core_execution_deployment_events'::regclass AND contype='f'`).Scan(&eventDeleteRule); err != nil {
		t.Fatal(err)
	}
	if eventDeleteRule != "r" {
		t.Fatalf("deployment event FK delete rule = %q, want RESTRICT", eventDeleteRule)
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	const owner = "@fresh-v2:example.test"
	const projectID = "00000000-0000-4000-8000-000000000201"
	const analysisID = "00000000-0000-4000-8000-000000000202"
	const planID = "00000000-0000-4000-8000-000000000203"
	const revisionID = "00000000-0000-4000-8000-000000000204"
	const runID = "00000000-0000-4000-8000-000000000205"
	const deploymentID = "00000000-0000-4000-8000-000000000206"
	digest := strings.Repeat("a", 64)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO core_execution_projects(owner_id,project_id,project_digest) VALUES($1,$2,$3)`, []any{owner, projectID, digest}},
		{`INSERT INTO core_execution_analyses(owner_id,analysis_id,project_id,analysis_digest) VALUES($1,$2,$3,$4)`, []any{owner, analysisID, projectID, digest}},
		{`INSERT INTO core_execution_plans(owner_id,plan_id,project_id) VALUES($1,$2,$3)`, []any{owner, planID, projectID}},
		{`INSERT INTO core_execution_plan_revisions(owner_id,plan_id,plan_revision_id,revision,project_id,analysis_id,plan_digest,snapshot_json) VALUES($1,$2,$3,1,$4,$5,$6,'{"materialization":"fresh"}'::jsonb)`, []any{owner, planID, revisionID, projectID, analysisID, digest}},
		{`INSERT INTO core_execution_runs(owner_id,run_id,project_id,plan_id,plan_revision,deployment_id,plan_digest,purpose,run_digest) VALUES($1,$2,$3,$4,1,$5,$6,'service',$7)`, []any{owner, runID, projectID, planID, deploymentID, digest, digest}},
		{`INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,deployment_id,operation,purpose,trigger_kind,plan_digest,current_stage,current_stage_id,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at) SELECT owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,deployment_id,operation,purpose,trigger_kind,plan_digest,current_stage,current_stage_id,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, []any{owner, runID}},
		{`INSERT INTO core_execution_deployments(owner_id,deployment_id,project_id,current_run_id) VALUES($1,$2,$3,$4)`, []any{owner, deploymentID, projectID, runID}},
	} {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert reciprocal V2 service pair: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("deferred reciprocal V2 service pair commit: %v", err)
	}
}

func TestFreshV78ContainsNativeAgentTurnMetaColumns(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, column := range []string{"model_profile_id", "model_profile_revision", "credential_version"} {
		var present bool
		if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='p2p_native_agent_turns' AND column_name=$1)`, column).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if !present {
			t.Fatalf("fresh v78 missing p2p_native_agent_turns.%s present=%v err=%v", column, present, err)
		}
	}
}

func TestDatabaseMembershipMigrationCanonicalizesJoinedToJoin(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO p2p_members (
			room_id, user_id, channel_id, display_name, avatar_url, domain,
			membership, role, muted, joined_at, requester_node_base_url, request_id
		) VALUES ($1, $2, '', '', '', 'example.com', ' JOINED ', 'member', 0, 1, '', '')
	`, "!room:example.com", "@alice:example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM db_migrations WHERE version = $1`, "p2p: canonical Matrix member membership v76"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var membership string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT membership FROM p2p_members WHERE room_id = $1 AND user_id = $2
	`, "!room:example.com", "@alice:example.com").Scan(&membership); err != nil {
		t.Fatal(err)
	}
	if membership != "join" {
		t.Fatalf("migrated membership = %q, want join", membership)
	}
}

func TestLegacyChannelFavoritesBackfillToOwnerReaction(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.SavePortal(ctx, portalState{OwnerMXID: "@owner:example.com"}); err != nil {
		t.Fatalf("save owner portal: %v", err)
	}
	if err := store.InsertChannelPost(ctx, channelPostRecord{
		PostID: "post_legacy", ChannelID: "channel_legacy", RoomID: "!channel:example.com", EventID: "$post_legacy",
	}); err != nil {
		t.Fatalf("insert channel post: %v", err)
	}
	if err := store.UpsertFavorite(ctx, favoriteRecord{
		ID: 1, EventID: "$post_legacy", RoomID: "!channel:example.com", MessageType: "m.image", CreatedAt: "2026-07-20T00:00:00Z",
	}); err != nil {
		t.Fatalf("insert legacy favorite: %v", err)
	}

	txn, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := backfillLegacyChannelFavorites(ctx, txn); err != nil {
		_ = txn.Rollback()
		t.Fatalf("backfill legacy favorite: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
	reaction, ok, err := store.GetReaction(ctx, "post", "post_legacy", "favorite", "@owner:example.com")
	if err != nil || !ok || !reaction.Active || reaction.ChannelID != "channel_legacy" {
		t.Fatalf("expected active owner favorite reaction, got %#v ok=%v err=%v", reaction, ok, err)
	}
	content := channelsmodule.NewContent(store, nil, nil, nil, channelsmodule.ContentConfig{
		Owner: func() channelsmodule.ContentOwner { return channelsmodule.ContentOwner{MXID: "@owner:example.com"} },
	})
	result, apiErr := content.Posts(ctx, map[string]any{"channel_id": "channel_legacy"})
	if apiErr != nil {
		t.Fatalf("list migrated post: %#v", apiErr)
	}
	posts := result.(map[string]any)["posts"].([]channelsmodule.Post)
	if len(posts) != 1 || posts[0].FavoriteCount != 1 || !posts[0].FavoritedByMe {
		t.Fatalf("owner view should expose migrated favorite state, got %#v", posts)
	}

	txn, err = store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := backfillLegacyChannelFavorites(ctx, txn); err != nil {
		_ = txn.Rollback()
		t.Fatalf("replay backfill: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
	count, err := store.CountActiveReactions(ctx, "post", "post_legacy", "favorite")
	if err != nil || count != 1 {
		t.Fatalf("backfill replay must remain idempotent, count=%d err=%v", count, err)
	}
}

func TestDatabaseReactionEventIdentityDeactivatesCurrentProjection(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertReaction(ctx, reactionRecord{
		EventID: "$favorite", TargetType: "post", TargetID: "post_1", ChannelID: "channel_1", PostID: "post_1",
		Reaction: "favorite", UserID: "@owner:example.com", Active: true,
	}); err != nil {
		t.Fatalf("store favorite reaction: %v", err)
	}
	removed, err := store.DeactivateReactionByEventID(ctx, "$favorite")
	if err != nil || !removed {
		t.Fatalf("deactivate projected reaction = (%t, %v), want (true, nil)", removed, err)
	}
	reaction, ok, err := store.GetReaction(ctx, "post", "post_1", "favorite", "@owner:example.com")
	if err != nil || !ok || reaction.Active || reaction.EventID != "$favorite" {
		t.Fatalf("expected stored reaction to remain identifiable and inactive, got %#v ok=%v err=%v", reaction, ok, err)
	}
}

func TestDatabaseStoreContactPeerUniqueMigrationDeduplicatesExistingRows(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	db, writer, err := sqlutil.NewConnectionManager(nil, dbOpts).Connection(&dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	store := NewUnmigratedDatabaseStore(db, writer)
	defer store.Close()

	if _, execErr := store.DB().ExecContext(ctx, `
		CREATE TABLE p2p_contacts (
			room_id TEXT PRIMARY KEY NOT NULL,
			peer_mxid TEXT NOT NULL,
			display_name TEXT NOT NULL,
			domain TEXT NOT NULL,
			status TEXT NOT NULL
		)
	`); execErr != nil {
		t.Fatal(execErr)
	}
	if _, execErr := store.DB().ExecContext(ctx, `CREATE INDEX p2p_contacts_peer_idx ON p2p_contacts(peer_mxid)`); execErr != nil {
		t.Fatal(execErr)
	}
	duplicates := []contactRecord{
		{RoomID: "!pending:example.com", PeerMXID: "@alice:remote.example", DisplayName: "Pending Alice", Domain: "remote.example", Status: "pending_outbound"},
		{RoomID: "!accepted:example.com", PeerMXID: "@alice:remote.example", DisplayName: "Accepted Alice", Domain: "remote.example", Status: "accepted"},
		{RoomID: "!deleted:example.com", PeerMXID: "@alice:remote.example", DisplayName: "Deleted Alice", Domain: "remote.example", Status: "deleted"},
		{RoomID: "!bob:example.com", PeerMXID: "@bob:remote.example", DisplayName: "Bob", Domain: "remote.example", Status: "pending_outbound"},
	}
	for _, contact := range duplicates {
		if _, execErr := store.DB().ExecContext(ctx, `
			INSERT INTO p2p_contacts (room_id, peer_mxid, display_name, domain, status)
			VALUES ($1, $2, $3, $4, $5)
		`, contact.RoomID, contact.PeerMXID, contact.DisplayName, contact.Domain, contact.Status); execErr != nil {
			t.Fatal(execErr)
		}
	}
	if migrationErr := markP2PMigrationsBeforeContactPeerUnique(ctx, store.DB()); migrationErr != nil {
		t.Fatal(migrationErr)
	}

	if migrationErr := store.Migrate(ctx); migrationErr != nil {
		t.Fatal(migrationErr)
	}

	contacts, err := store.ListContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(contacts) != 2 {
		t.Fatalf("expected duplicate peers to be compacted, got %#v", contacts)
	}
	alice := findStoredContact(contacts, "@alice:remote.example")
	if alice.RoomID != "!accepted:example.com" || alice.Status != "accepted" {
		t.Fatalf("expected migration to keep accepted contact for duplicate peer, got %#v", alice)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO p2p_contacts (room_id, peer_mxid, display_name, domain, status)
		VALUES ($1, $2, $3, $4, $5)
	`, "!new-alice:example.com", "@alice:remote.example", "Alice Duplicate", "remote.example", "pending_outbound"); err == nil {
		t.Fatalf("expected migrated contact peer index to reject duplicates")
	}
}

func findStoredContact(contacts []contactRecord, peerMXID string) contactRecord {
	for _, contact := range contacts {
		if contact.PeerMXID == peerMXID {
			return contact
		}
	}
	return contactRecord{}
}

func markP2PMigrationsBeforeContactPeerUnique(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS db_migrations (
			version TEXT PRIMARY KEY NOT NULL,
			time TEXT NOT NULL,
			dendrite_version TEXT NOT NULL
		)
	`); err != nil {
		return err
	}
	versions := []string{
		"p2p: integrated appservice tables v1",
		"p2p: integrated appservice tables v2",
		"p2p: integrated appservice tables v3",
		"p2p: integrated appservice tables v4 member avatars",
		"p2p: integrated appservice tables v5 product mute state",
		"p2p: integrated appservice tables v6 member join order",
		"p2p: integrated appservice tables v7 portal matrix device",
		"p2p: integrated appservice tables v11 channel comment replies",
		"p2p: integrated appservice tables v12 channel comment media",
		"p2p: integrated appservice tables v13 event outbox",
		"p2p: integrated appservice tables v14 channel invite grants",
		"p2p: drop legacy message mirror table v15",
	}
	for _, version := range versions {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO db_migrations (version, time, dendrite_version)
			VALUES ($1, $2, $3)
		`, version, "2026-06-21T00:00:00Z", "test"); err != nil {
			return err
		}
	}
	return nil
}
