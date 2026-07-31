package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func TestExecutionQueryStorePlanRunAndEvents(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 2, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	analysis, plan := executionStoreFixture(now, 1)
	if _, err := s.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: plan.OwnerID, Analysis: analysis, Plan: plan, IdempotencyID: "abababab-abab-4aba-8aba-abababababab"}); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListExecutionPlanRevisions(ctx, plan.OwnerID, plan.ID, 0, 10)
	if err != nil || len(history.Items) != 1 || !coreexecution.ValidateDigest(string(history.Items[0].Digest)) {
		t.Fatalf("plan history=%#v err=%v", history, err)
	}
	page, err := s.ListExecutionPlans(ctx, plan.OwnerID, "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].PlanID != plan.ID {
		t.Fatalf("plan list=%#v err=%v", page, err)
	}
	coordinator := NewDatabaseExecutionCoordinator(store.DB(), func() time.Time { return now })
	materialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: plan.OwnerID, PlanID: plan.ID, PlanRevision: 1, IdempotencyKey: "cdcdcdcd-cdcd-4cdc-8cdc-cdcdcdcdcdcd"})
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.GetExecutionRun(ctx, plan.OwnerID, materialized.Run.RunID)
	if err != nil || view.Run.RunID != materialized.Run.RunID || len(view.Stages) != len(materialized.Stages) {
		t.Fatalf("run view=%#v err=%v", view, err)
	}
	if _, err := s.AppendExecutionEvent(ctx, ExecutionEventCreate{OwnerID: plan.OwnerID, RunID: materialized.Run.RunID, StageID: materialized.Stages[0].StageID, Kind: "query.test", Payload: map[string]any{"safe": "ok"}}); err != nil {
		t.Fatal(err)
	}
	events, next, err := s.ListV2RunEvents(ctx, plan.OwnerID, materialized.Run.RunID, 0, 10)
	if err != nil || len(events) < 2 || next != 0 || events[0].Sequence != 1 || string(events[len(events)-1].Payload) != `{"safe":"ok"}` {
		t.Fatalf("events=%#v next=%d err=%v", events, next, err)
	}
	if _, err := s.GetExecutionRun(ctx, "@foreign:example.org", materialized.Run.RunID); !errors.Is(err, coreexecution.ErrNotFound) {
		t.Fatalf("foreign run read=%v", err)
	}
}
