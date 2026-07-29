package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcore"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestMemoryDeploymentLeaseCASAndExpiry(t *testing.T) {
	s := NewMemoryStore()
	op := map[string]any{"workload_id": "w", "operation_id": "o", "plan_id": "p", "kind": "apply", "status": "running", "target_kind": "core-runner"}
	if err := s.UpsertDeploymentMutation(context.Background(), "owner", agentcore.DeploymentMutation{Operation: op}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimDeploymentBatch(context.Background(), "owner", "worker-a", 25, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	if other, err := s.ClaimDeploymentBatch(context.Background(), "owner", "worker-b", 25, 1); err != nil || len(other) != 0 {
		t.Fatalf("active lease claim=%#v err=%v", other, err)
	}
	if err := s.ReleaseDeploymentLease(context.Background(), "owner", "w", "worker-b", claimed[0].Revision, false); err == nil {
		t.Fatal("wrong worker CAS accepted")
	}
	time.Sleep(35 * time.Millisecond)
	if takeover, err := s.ClaimDeploymentBatch(context.Background(), "owner", "worker-b", 25, 1); err != nil || len(takeover) != 1 {
		t.Fatalf("expired lease takeover=%#v err=%v", takeover, err)
	} else if err := s.CommitDeploymentReconciliation(context.Background(), "owner", "w", "worker-a", claimed[0].Revision, agentcore.DeploymentMutation{Operation: op}, map[string]any{"workload_id": "w", "operation_id": "o", "sequence": int64(1), "kind": "dispatch", "status": "running"}); !errors.Is(err, agentcore.ErrDeploymentLeaseCAS) {
		t.Fatalf("stale reconciliation commit=%v", err)
	}
	if events, _, err := s.ListDeploymentEvents(context.Background(), "owner", "w", 0, 10); err != nil || len(events) != 0 {
		t.Fatalf("stale reconciliation persisted events=%#v err=%v", events, err)
	}
}

func TestMemoryDeploymentEventReplayIsIdempotentAndLinkedToClaimedWorkload(t *testing.T) {
	s := NewMemoryStore()
	op := map[string]any{"workload_id": "w-events", "operation_id": "o-events", "plan_id": "p", "kind": "apply", "status": "running", "target_kind": "core-runner"}
	if err := s.UpsertDeploymentMutation(context.Background(), "owner", agentcore.DeploymentMutation{Operation: op}); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"workload_id": "w-events", "operation_id": "o-events", "sequence": int64(1), "kind": "dispatch", "status": "running", "actual": nil}
	if err := s.UpsertDeploymentEvent(context.Background(), "owner", event); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDeploymentEvent(context.Background(), "owner", event); err != nil {
		t.Fatalf("replay event: %v", err)
	}
	events, last, err := s.ListDeploymentEvents(context.Background(), "owner", "w-events", 0, 10)
	if err != nil || last != 1 || len(events) != 1 {
		t.Fatalf("events=%#v last=%d err=%v", events, last, err)
	}
}

func TestMemoryDeploymentEventsUsePublicWorkloadCursorAndSafeMessage(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	for _, op := range []map[string]any{
		{"workload_id": "w-public", "operation_id": "o-1", "plan_id": "p", "kind": "apply", "status": "running", "target_kind": "core-runner"},
	} {
		if err := s.UpsertDeploymentMutation(ctx, "owner", agentcore.DeploymentMutation{Operation: op}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertDeploymentEvent(ctx, "owner", map[string]any{"workload_id": "w-public", "operation_id": "o-1", "sequence": int64(1), "kind": "dispatch", "status": "running", "message": "secret-canary"}); err != nil {
		t.Fatal(err)
	}
	op2 := map[string]any{"workload_id": "w-public", "operation_id": "o-2", "plan_id": "p", "kind": "destroy", "status": "running", "target_kind": "core-runner"}
	if err := s.UpsertDeploymentMutation(ctx, "owner", agentcore.DeploymentMutation{Operation: op2}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDeploymentEvent(ctx, "owner", map[string]any{"workload_id": "w-public", "operation_id": "o-2", "sequence": int64(1), "kind": "credential=AKIA/secret", "status": "running", "message": "secret-canary"}); err != nil {
		t.Fatal(err)
	}
	events, last, err := s.ListDeploymentEvents(ctx, "owner", "w-public", 0, 10)
	if err != nil || len(events) != 2 || last != 2 {
		t.Fatalf("events=%#v last=%d err=%v", events, last, err)
	}
	if events[0]["sequence"] != int64(1) || events[1]["sequence"] != int64(2) || events[0]["event_id"] == events[1]["event_id"] || events[0]["message"] != "" || events[1]["message"] != "" || events[1]["type"] != "unknown" {
		t.Fatalf("public event contract mismatch: %#v", events)
	}
}

func TestMemoryDeploymentEventSequenceFencePreventsStatusRegression(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	op := map[string]any{"workload_id": "w-fence", "operation_id": "o-fence", "plan_id": "p", "kind": "apply", "status": "running", "target_kind": "core-runner"}
	if err := s.UpsertDeploymentMutation(ctx, "owner", agentcore.DeploymentMutation{Operation: op}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDeploymentEvent(ctx, "owner", map[string]any{"workload_id": "w-fence", "operation_id": "o-fence", "sequence": int64(10), "kind": "complete", "status": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDeploymentEvent(ctx, "owner", map[string]any{"workload_id": "w-fence", "operation_id": "o-fence", "sequence": int64(1), "kind": "late", "status": "pending"}); err != nil {
		t.Fatal(err)
	}
	object, ok, err := s.GetDeployment(ctx, "owner", "w-fence")
	if err != nil || !ok || object["status"] != "succeeded" {
		t.Fatalf("out-of-order event regressed object=%#v ok=%v err=%v", object, ok, err)
	}
}

func TestMemoryDeploymentTerminalMutationReplayDoesNotReactivateAndDestroySuccessorAdvances(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	completed := map[string]any{"workload_id": "w-terminal", "operation_id": "apply-1", "plan_id": "p", "kind": "apply", "status": "succeeded", "target_kind": "core-runner"}
	if err := s.UpsertDeploymentMutation(ctx, "owner", agentcore.DeploymentMutation{Operation: completed}); err != nil {
		t.Fatal(err)
	}
	stale := map[string]any{"workload_id": "w-terminal", "operation_id": "apply-1", "plan_id": "p", "kind": "apply", "status": "aborting", "target_kind": "core-runner"}
	if err := s.UpsertDeploymentMutation(ctx, "owner", agentcore.DeploymentMutation{Operation: stale}); err != nil {
		t.Fatal(err)
	}
	object, ok, err := s.GetDeployment(ctx, "owner", "w-terminal")
	if err != nil || !ok || object["status"] != "succeeded" || object["latest_operation_id"] != "apply-1" {
		t.Fatalf("terminal replay changed deployment=%#v ok=%v err=%v", object, ok, err)
	}
	if candidates, err := s.DeploymentCandidates(ctx, "owner", 10); err != nil || len(candidates) != 0 {
		t.Fatalf("terminal replay reactivated reconciliation=%#v err=%v", candidates, err)
	}

	destroy := map[string]any{"workload_id": "w-terminal", "operation_id": "destroy-2", "plan_id": "p", "kind": "destroy", "status": "running", "target_kind": "core-runner"}
	if err := s.UpsertDeploymentMutation(ctx, "owner", agentcore.DeploymentMutation{Operation: destroy}); err != nil {
		t.Fatal(err)
	}
	object, ok, err = s.GetDeployment(ctx, "owner", "w-terminal")
	if err != nil || !ok || object["status"] != "running" || object["latest_operation_id"] != "destroy-2" {
		t.Fatalf("destroy successor did not advance deployment=%#v ok=%v err=%v", object, ok, err)
	}
	if candidates, err := s.DeploymentCandidates(ctx, "owner", 10); err != nil || len(candidates) != 1 || candidates[0].OperationID != "destroy-2" {
		t.Fatalf("destroy successor candidates=%#v err=%v", candidates, err)
	}
	destroy["status"] = "succeeded"
	if err := s.UpsertDeploymentMutation(ctx, "owner", agentcore.DeploymentMutation{Operation: destroy}); err != nil {
		t.Fatal(err)
	}
	object, ok, err = s.GetDeployment(ctx, "owner", "w-terminal")
	if err != nil || !ok || object["status"] != "destroyed" {
		t.Fatalf("destroy successor terminal state=%#v ok=%v err=%v", object, ok, err)
	}
}

func TestMemoryDeploymentTerminalReconciliationReplayDropsEventsAndReleasesLease(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	completed := map[string]any{"workload_id": "w-terminal-commit", "operation_id": "apply-1", "plan_id": "p", "kind": "apply", "status": "succeeded", "target_kind": "core-runner"}
	if err := s.UpsertDeploymentMutation(ctx, "owner", agentcore.DeploymentMutation{Operation: completed}); err != nil {
		t.Fatal(err)
	}
	key := "owner\x00w-terminal-commit"
	s.mu.Lock()
	row := s.deployments[key]
	row.leaseOwner = "worker"
	row.revision++
	s.deployments[key] = row
	s.mu.Unlock()
	stale := map[string]any{"workload_id": "w-terminal-commit", "operation_id": "apply-1", "plan_id": "p", "kind": "apply", "status": "running", "target_kind": "core-runner"}
	event := map[string]any{"workload_id": "w-terminal-commit", "operation_id": "apply-1", "sequence": int64(9), "kind": "progress", "status": "running"}
	if err := s.CommitDeploymentReconciliation(ctx, "owner", "w-terminal-commit", "worker", row.revision, agentcore.DeploymentMutation{Operation: stale}, event); err != nil {
		t.Fatal(err)
	}
	object, ok, err := s.GetDeployment(ctx, "owner", "w-terminal-commit")
	if err != nil || !ok || object["status"] != "succeeded" {
		t.Fatalf("terminal reconciliation replay changed deployment=%#v ok=%v err=%v", object, ok, err)
	}
	if events, _, err := s.ListDeploymentEvents(ctx, "owner", "w-terminal-commit", 0, 10); err != nil || len(events) != 0 {
		t.Fatalf("terminal reconciliation replay persisted stale events=%#v err=%v", events, err)
	}
	s.mu.RLock()
	updated := s.deployments[key]
	s.mu.RUnlock()
	if updated.leaseOwner != "" || !updated.leaseUntil.IsZero() || updated.revision != row.revision+1 {
		t.Fatalf("terminal reconciliation did not release exact lease=%#v", updated)
	}
}

func TestPostgresDeploymentTerminalMutationReplaySkipsUpdate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, sqlutil.NewExclusiveWriter())
	completed := map[string]any{"workload_id": "w-terminal", "operation_id": "apply-1", "plan_id": "p", "kind": "apply", "status": "succeeded", "target_kind": "core-runner"}
	previous, err := json.Marshal(agentcore.DeploymentObject(agentcore.DeploymentMutation{Operation: completed}))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT object_json,actual_json,quote_json FROM p2p_agent_deployments WHERE owner_id=$1 AND workload_id=$2`)).WithArgs("owner", "w-terminal").WillReturnRows(sqlmock.NewRows([]string{"object_json", "actual_json", "quote_json"}).AddRow(previous, []byte(`{}`), []byte(`{}`)))
	mock.ExpectCommit()
	stale := map[string]any{"workload_id": "w-terminal", "operation_id": "apply-1", "plan_id": "p", "kind": "apply", "status": "pending", "target_kind": "core-runner"}
	if err := store.UpsertDeploymentMutation(context.Background(), "owner", agentcore.DeploymentMutation{Operation: stale}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("terminal replay issued a write: %v", err)
	}
}

func TestPostgresDeploymentTerminalReconciliationReplayDropsEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, sqlutil.NewExclusiveWriter())
	completed := map[string]any{"workload_id": "w-terminal-commit", "operation_id": "apply-1", "plan_id": "p", "kind": "apply", "status": "succeeded", "target_kind": "core-runner"}
	previous, err := json.Marshal(agentcore.DeploymentObject(agentcore.DeploymentMutation{Operation: completed}))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT object_json,actual_json,quote_json,revision,lease_owner FROM p2p_agent_deployments WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`)).WithArgs("owner", "w-terminal-commit").WillReturnRows(sqlmock.NewRows([]string{"object_json", "actual_json", "quote_json", "revision", "lease_owner"}).AddRow(previous, []byte(`{}`), []byte(`{}`), int64(7), "worker"))
	mock.ExpectExec(`UPDATE p2p_agent_deployments SET operation_id=`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	stale := map[string]any{"workload_id": "w-terminal-commit", "operation_id": "apply-1", "plan_id": "p", "kind": "apply", "status": "pending", "target_kind": "core-runner"}
	event := map[string]any{"workload_id": "w-terminal-commit", "operation_id": "apply-1", "sequence": int64(9), "kind": "progress", "status": "pending"}
	if err := store.CommitDeploymentReconciliation(context.Background(), "owner", "w-terminal-commit", "worker", 7, agentcore.DeploymentMutation{Operation: stale}, event); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("terminal reconciliation replay persisted stale event or missed lease release: %v", err)
	}
}

func TestMemoryDeploymentListPaginatesBeyondRecentDashboardLimit(t *testing.T) {
	s := NewMemoryStore()
	for i := 0; i < 25; i++ {
		op := map[string]any{"workload_id": fmt.Sprintf("w-%02d", i), "operation_id": fmt.Sprintf("o-%02d", i), "plan_id": "p", "kind": "apply", "status": "succeeded", "target_kind": "core-runner"}
		if err := s.UpsertDeploymentMutation(context.Background(), "owner", agentcore.DeploymentMutation{Operation: op}); err != nil {
			t.Fatal(err)
		}
	}
	token := ""
	total := 0
	for page := 0; page < 10; page++ {
		items, next, err := s.ListDeployments(context.Background(), "owner", agentcore.DeploymentListOptions{PageSize: 10, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		total += len(items)
		if next == "" {
			break
		}
		if next == token {
			t.Fatal("deployment page token did not advance")
		}
		token = next
	}
	if total != 25 {
		t.Fatalf("paginated deployment total=%d, want 25", total)
	}
}

func TestPostgresDeploymentLeaseCASAndGuardedCommit(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	s, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	op := map[string]any{"workload_id": "pg-w", "operation_id": "pg-o", "plan_id": "p", "kind": "apply", "status": "running", "target_kind": "core-runner", "failure_code": "provider_failed", "failure_summary": "secret-canary"}
	if err := s.UpsertDeploymentMutation(ctx, "owner", agentcore.DeploymentMutation{Operation: op}); err != nil {
		t.Fatal(err)
	}
	if object, ok, err := s.GetDeployment(ctx, "owner", "pg-w"); err != nil || !ok {
		t.Fatalf("get deployment=%#v ok=%v err=%v", object, ok, err)
	} else if current, ok := object["current_operation"].(map[string]any); !ok {
		t.Fatalf("current operation missing: %#v", object)
	} else if _, leaked := current["failure_summary"]; leaked || object["error"].(map[string]any)["summary"] != "Deployment failed" {
		t.Fatalf("failure text leaked: %#v", object)
	}
	old, err := s.ClaimDeploymentBatch(ctx, "owner", "old-worker", 5, 1)
	if err != nil || len(old) != 1 {
		t.Fatalf("old claim=%#v err=%v", old, err)
	}
	time.Sleep(10 * time.Millisecond)
	takeover, err := s.ClaimDeploymentBatch(ctx, "owner", "new-worker", time.Minute.Milliseconds(), 1)
	if err != nil || len(takeover) != 1 {
		t.Fatalf("takeover=%#v err=%v", takeover, err)
	}
	if err := s.CommitDeploymentReconciliation(ctx, "owner", "pg-w", "old-worker", old[0].Revision, agentcore.DeploymentMutation{Operation: op}, map[string]any{"workload_id": "pg-w", "operation_id": "pg-o", "sequence": int64(1), "kind": "dispatch", "status": "running"}); !errors.Is(err, agentcore.ErrDeploymentLeaseCAS) {
		t.Fatalf("stale postgres commit=%v", err)
	}
	if events, _, err := s.ListDeploymentEvents(ctx, "owner", "pg-w", 0, 10); err != nil || len(events) != 0 {
		t.Fatalf("stale postgres reconciliation persisted events=%#v err=%v", events, err)
	}
	if err := s.ReleaseDeploymentLease(ctx, "owner", "pg-w", "old-worker", old[0].Revision, false); !errors.Is(err, agentcore.ErrDeploymentLeaseCAS) {
		t.Fatalf("stale postgres release=%v", err)
	}
}
