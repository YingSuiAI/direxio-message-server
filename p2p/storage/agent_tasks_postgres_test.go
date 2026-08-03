package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestPostgresTaskQueueClaimAndReclaimEventPayloads(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const (
		owner   = "@task-queue-owner:example.test"
		taskID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		stageID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	spec, err := json.Marshal(task.TaskSpec{
		Kind:           task.TaskKindAgent,
		Goal:           "queue event payload regression",
		ModelProfileID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		IdempotencyKey: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		AvailableAt:    now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,$4,$4)`, taskID, owner, spec, now); err != nil {
		t.Fatal(err)
	}
	stageSpec, _ := json.Marshal(map[string]any{"kind": string(task.TaskKindExecutionStage), "goal": "v2 stage", "idempotency_key": "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "available_at": now})
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,$4,$4)`, stageID, owner, stageSpec, now); err != nil {
		t.Fatal(err)
	}

	queue := NewDatabaseTaskStore(store.DB())
	claimed, lease, err := queue.ClaimNextDue(ctx, "claim-holder", now, time.Minute, 2)
	if err != nil {
		t.Fatalf("claim due task: %v", err)
	}
	if claimed.ID != taskID || claimed.Revision != 2 || claimed.ExecutionStartedAt == nil || lease.Holder != "claim-holder" || lease.Epoch != 1 {
		t.Fatalf("claim = task=%#v lease=%#v", claimed, lease)
	}
	var claimHolder string
	if err = store.DB().QueryRowContext(ctx, `SELECT payload_json->>'holder' FROM agent_task_events WHERE owner_id=$1 AND task_id=$2 AND sequence=1`, owner, taskID).Scan(&claimHolder); err != nil {
		t.Fatal(err)
	}
	if claimHolder != "claim-holder" {
		t.Fatalf("claimed event holder = %q", claimHolder)
	}
	if _, err = queue.RenewLease(ctx, task.RenewLeaseCommand{
		Fence:  task.Fence{TaskID: taskID, Attempt: lease.Attempt, LeaseEpoch: lease.Epoch, ExpectedRevision: claimed.Revision},
		Holder: "claim-holder", LeaseTTL: time.Minute, At: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("renew current claim fence: %v", err)
	}

	if err = queue.ReclaimExpired(ctx, "reclaim-holder", now.Add(2*time.Minute), time.Minute, 2); err != nil {
		t.Fatalf("reclaim expired task: %v", err)
	}
	var previousHolder, nextHolder, previousEpoch string
	if err = store.DB().QueryRowContext(ctx, `SELECT payload_json->>'previous_holder',payload_json->>'next_holder',payload_json->>'previous_lease_epoch' FROM agent_task_events WHERE owner_id=$1 AND task_id=$2 AND sequence=2`, owner, taskID).Scan(&previousHolder, &nextHolder, &previousEpoch); err != nil {
		t.Fatal(err)
	}
	if previousHolder != "claim-holder" || nextHolder != "reclaim-holder" || previousEpoch != "1" {
		t.Fatalf("reclaimed event payload = previous=%q next=%q epoch=%q", previousHolder, nextHolder, previousEpoch)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE agent_tasks SET status='succeeded' WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = queue.ClaimNextDue(ctx, "v2-guard", now.Add(3*time.Minute), time.Minute, 2); err != task.ErrNotFound {
		t.Fatalf("execution_stage task was claimable by legacy queue: %v", err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE agent_tasks SET status='running',lease_expires_at=$2 WHERE task_id=$1`, stageID, now); err != nil {
		t.Fatal(err)
	}
	if err = queue.ReclaimExpired(ctx, "v2-guard", now.Add(4*time.Minute), time.Minute, 2); err != task.ErrNotFound {
		t.Fatalf("execution_stage task was reclaimable by legacy queue: %v", err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE agent_tasks SET status='running',attempt=1,lease_epoch=1,revision=2,lease_holder='v2',lease_expires_at=$2 WHERE task_id=$1`, stageID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	replayKey := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO agent_task_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at) VALUES($1,'cancel',$2,'stale-digest','{}',$3)`, owner, replayKey, now); err != nil {
		t.Fatal(err)
	}
	if _, err = queue.CancelTask(ctx, task.CancelCommand{OwnerID: owner, TaskID: stageID, ExpectedRevision: 2, Reason: "stop", At: now, Mutation: task.MutationCommand{IdempotencyKey: replayKey, ExpectedRevision: 2}}); err != task.ErrConflict {
		t.Fatalf("execution_stage cancel replay bypassed kind fence: %v", err)
	}
	if _, err = queue.transition(ctx, task.Fence{TaskID: stageID, Attempt: 1, LeaseEpoch: 1, ExpectedRevision: 2}, task.StatusFailed, "legacy"); err != task.ErrLeaseConflict {
		t.Fatalf("legacy transition mutated execution_stage task: %v", err)
	}
}
