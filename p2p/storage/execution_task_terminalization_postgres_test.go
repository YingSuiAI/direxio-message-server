package storage

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestExecutionTaskTerminalizationReleasesConcurrencyExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := openExecutionV2Schema(t)
	now := time.Date(2035, 5, 6, 7, 8, 9, 0, time.UTC)
	for n, outcome := range []string{"succeeded", "failed", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			owner := fmt.Sprintf("@execution-task-terminal-%d:example.test", n)
			taskID := fmt.Sprintf("00000000-0000-4000-8000-0000000004%02d", n+20)
			if _, err := store.DB().ExecContext(ctx, `INSERT INTO agent_task_runtime_concurrency(singleton,running_count,max_concurrent,revision,updated_at) VALUES(true,1,1,1,$1) ON CONFLICT(singleton) DO UPDATE SET running_count=1,revision=agent_task_runtime_concurrency.revision+1,updated_at=EXCLUDED.updated_at`, now); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,lease_epoch,lease_holder,lease_expires_at,revision,available_at,created_at,updated_at) VALUES($1,$2,'{"kind":"execution_stage"}'::jsonb,'running',1,1,'terminal-worker',$3,1,$4,$4,$4)`, taskID, owner, now.Add(time.Hour), now); err != nil {
				t.Fatal(err)
			}
			terminalize := func() {
				tx, err := store.DB().BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = terminalizeExecutionTaskTx(ctx, tx, owner, taskID, outcome, "test_outcome", "test outcome", now); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			terminalize()
			terminalize()
			var gotStatus string
			var running int
			if err := store.DB().QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE owner_id=$1 AND task_id=$2`, owner, taskID).Scan(&gotStatus); err != nil {
				t.Fatal(err)
			}
			if gotStatus != outcome {
				t.Fatalf("task status=%q, want %q", gotStatus, outcome)
			}
			if err := store.DB().QueryRowContext(ctx, `SELECT running_count FROM agent_task_runtime_concurrency WHERE singleton=true`).Scan(&running); err != nil {
				t.Fatal(err)
			}
			if running != 0 {
				t.Fatalf("running_count=%d, want 0 after idempotent terminalization", running)
			}
		})
	}
}

func TestExecutionTaskTerminalizationReconciledSuccessDoesNotDecrementAgain(t *testing.T) {
	ctx := context.Background()
	store := openExecutionV2Schema(t)
	now := time.Date(2035, 5, 6, 7, 8, 9, 0, time.UTC)
	owner := "@execution-task-reconcile-success:example.test"
	taskID := "00000000-0000-4000-8000-000000000451"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO agent_task_runtime_concurrency(singleton,running_count,max_concurrent,revision,updated_at) VALUES(true,0,1,1,$1) ON CONFLICT(singleton) DO UPDATE SET running_count=0,revision=agent_task_runtime_concurrency.revision+1,updated_at=EXCLUDED.updated_at`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,revision,available_at,created_at,updated_at,failure_code,failure_summary) VALUES($1,$2,'{"kind":"execution_stage"}'::jsonb,'failed',4,$3,$3,$3,'execution_outcome_uncertain','external dispatch outcome uncertain')`, taskID, owner, now); err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = terminalizeExecutionTaskTx(ctx, tx, owner, taskID, "succeeded", "", "", now); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var status string
	var running int
	if err = store.DB().QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE owner_id=$1 AND task_id=$2`, owner, taskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" {
		t.Fatalf("status=%q, want succeeded", status)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT running_count FROM agent_task_runtime_concurrency WHERE singleton=true`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 0 {
		t.Fatalf("running_count=%d, want 0", running)
	}
}
