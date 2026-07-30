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
		owner  = "@task-queue-owner:example.test"
		taskID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
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

	queue := NewDatabaseTaskStore(store.DB())
	claimed, lease, err := queue.ClaimNextDue(ctx, "claim-holder", now, time.Minute, 2)
	if err != nil {
		t.Fatalf("claim due task: %v", err)
	}
	if claimed.ID != taskID || lease.Holder != "claim-holder" || lease.Epoch != 1 {
		t.Fatalf("claim = task=%#v lease=%#v", claimed, lease)
	}
	var claimHolder string
	if err = store.DB().QueryRowContext(ctx, `SELECT payload_json->>'holder' FROM agent_task_events WHERE owner_id=$1 AND task_id=$2 AND sequence=1`, owner, taskID).Scan(&claimHolder); err != nil {
		t.Fatal(err)
	}
	if claimHolder != "claim-holder" {
		t.Fatalf("claimed event holder = %q", claimHolder)
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
}
