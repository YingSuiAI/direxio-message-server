package execution

import (
	"context"
	"sync"
	"testing"
	"time"

	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

func coordinatorClock() func() time.Time {
	return func() time.Time { return time.Date(2030, 1, 1, 0, 5, 0, 0, time.UTC) }
}

func createMemoryRun(t *testing.T, p ExecutionPlan, mode FakeMode) (*MemoryCoordinator, RunMaterialization) {
	t.Helper()
	s, err := NewMemoryCoordinatorStore(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewMemoryCoordinator(s, FakeExecutor{Mode: mode}, coordinatorClock())
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, IdempotencyKey: "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	return c, m
}

func TestMemoryCoordinatorConfirmationAndSequentialReceipts(t *testing.T) {
	c, m := createMemoryRun(t, plan(), FakeSuccess)
	if len(m.Stages) != 1 || len(m.Tasks) != 1 || len(m.Confirmations) != 1 || m.Stages[0].Status != StageWaitingUser {
		t.Fatalf("materialization %#v", m)
	}
	if q := m.Tasks[0].Spec.Payload.ExecutionStage; q.RunRevision == 0 || q.RunRevision != uint64(m.Confirmations[0].Binding.RunRevision) {
		t.Fatalf("task/confirmation run revision fence mismatch: task=%#v binding=%#v", q, m.Confirmations[0].Binding)
	}
	conf := m.Confirmations[0]
	confirmed, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: conf.ID, ExpectedRevision: conf.Revision, IdempotencyKey: "22222222-2222-4222-8222-222222222222"})
	if err != nil || confirmed.State != "confirmed" {
		t.Fatalf("confirm: %#v %v", confirmed, err)
	}
	if err = c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker-a", time.Time{}); err != nil {
		t.Fatal(err)
	}
	r, err := c.GetRun(context.Background(), ownerID, m.Run.RunID)
	if err != nil || r.Status != RunSucceeded {
		t.Fatalf("run %#v %v", r, err)
	}
	s, err := c.GetStage(context.Background(), ownerID, m.Run.RunID, m.Stages[0].StageID)
	if err != nil || s.Status != StageSucceeded {
		t.Fatalf("stage %#v %v", s, err)
	}
	if got := c.Attempts(context.Background(), ownerID, s.StageID); len(got) != 1 || got[0].Status != AttemptSucceeded || got[0].ReceiptID == "" {
		t.Fatalf("attempts %#v", got)
	}
}

func TestMemoryCoordinatorAutoStageUnlocksDependentConfirmation(t *testing.T) {
	p := plan()
	deployStep := p.Stages[0].Steps[0]
	p.Stages[0].Risk, p.Stages[0].Gate = RiskR0, GateNone
	p.Stages[0].Steps[0] = ExecutionStep{StepKey: "inspect", Kind: StepTargetInspect, TargetID: targetID, TargetRevision: 1, TargetDigest: sha, TimeoutSeconds: 1, IdempotencyMarker: "inspect", OutputPolicy: OutputDiscard, TargetInspect: &TargetInspectStep{ObservationID: artifactID}}
	p.Stages = append(p.Stages, ExecutionStage{StageKey: "deploy2", Revision: 1, Kind: "deploy", Risk: RiskR2, Gate: GateRemotePrivilegedExecution, Effects: []Gate{GateRemotePrivilegedExecution}, DependsOn: []string{"deploy"}, TargetID: targetID, TargetRevision: 1, TargetDigest: sha, Steps: []ExecutionStep{deployStep}, TimeoutSeconds: 60})
	p.Stages[1].Steps[0].StepKey = "deploy-step"
	p.Stages[1].Steps[0].IdempotencyMarker = "deploy-marker"
	p.Stages[1].Steps[0].ScriptRun.IdempotencyMarker = "deploy-marker"
	c, m := createMemoryRun(t, p, FakeSuccess)
	if len(m.Tasks) != 1 || len(m.Confirmations) != 0 || m.Stages[0].Status != StageQueued {
		t.Fatalf("initial %#v", m)
	}
	if err := c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Reconstructing over the same durable aggregate is the in-memory restart
	// simulation; the dependent card must remain exactly materialized once.
	c, err := NewMemoryCoordinator(c.store, FakeExecutor{Mode: FakeSuccess}, coordinatorClock())
	if err != nil {
		t.Fatal(err)
	}
	stage := c.store.stages[stageMapKey(m.Run.RunID, deterministicID("execution:v2:stage:"+m.Run.RunID+":deploy2"))]
	if stage.Status != StageWaitingUser || stage.TaskID == "" || stage.ConfirmationID == "" {
		t.Fatalf("dependent %#v", stage)
	}
	run, err := c.GetRun(context.Background(), ownerID, m.Run.RunID)
	if err != nil || run.Status != RunRunning || run.Validate() != nil {
		t.Fatalf("started run regressed to a pre-start state: run=%#v err=%v validate=%v", run, err, run.Validate())
	}
}

func TestMemoryCoordinatorUncertainDoesNotRedispatchAndReconciles(t *testing.T) {
	c, m := createMemoryRun(t, plan(), FakeUncertainAfterDispatch)
	conf := m.Confirmations[0]
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: conf.ID, ExpectedRevision: 1, IdempotencyKey: "33333333-3333-4333-8333-333333333333"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{}); err != ErrUncertain {
		t.Fatalf("got %v", err)
	}
	s, err := c.GetStage(context.Background(), ownerID, m.Run.RunID, m.Stages[0].StageID)
	if err != nil || s.Status != StageUncertain {
		t.Fatalf("stage %#v %v", s, err)
	}
	if err := c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{}); err == nil {
		t.Fatal("redispatch accepted")
	}
	c, err = NewMemoryCoordinator(c.store, FakeExecutor{Mode: FakeReconcileSuccess}, coordinatorClock())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReconcileStage(context.Background(), ownerID, m.Run.RunID, m.Stages[0].StageID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, held := c.store.reservations[conf.ID]; held {
		t.Fatal("reconciliation released the target but retained its confirmation reservation")
	}
	r, err := c.GetRun(context.Background(), ownerID, m.Run.RunID)
	if err != nil || r.Status != RunUncertain {
		t.Fatalf("run %#v %v", r, err)
	}
	if len(c.store.resolutions[m.Run.RunID]) != 1 || !c.store.resolutions[m.Run.RunID][0].Succeeded {
		t.Fatalf("missing immutable reconciliation successor: %#v", c.store.resolutions[m.Run.RunID])
	}
	if _, err = c.ReconcileStage(context.Background(), ownerID, m.Run.RunID, m.Stages[0].StageID, time.Time{}); err != nil || len(c.store.resolutions[m.Run.RunID]) != 1 {
		t.Fatalf("resolution replay was not idempotent: err=%v resolutions=%#v", err, c.store.resolutions[m.Run.RunID])
	}
	successor, err := c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, IdempotencyKey: "33333333-3333-4333-8333-333333333334"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: successor.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: "33333333-3333-4333-8333-333333333335"}); err != nil {
		t.Fatal(err)
	}
	if err = c.ExecuteClaimedStage(context.Background(), ownerID, successor.Tasks[0].ID, "successor", time.Time{}); err != nil {
		t.Fatalf("resolved uncertain lease still blocked successor: %v", err)
	}
}

func TestMemoryCoordinatorCreateReplayMismatchConflicts(t *testing.T) {
	c, m := createMemoryRun(t, plan(), FakeSuccess)
	if _, err := c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, IdempotencyKey: "11111111-1111-4111-8111-111111111111"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, Operation: RunOperationDeploy, IdempotencyKey: "11111111-1111-4111-8111-111111111111"}); err == nil {
		t.Fatal("mismatch accepted")
	}
	if m.Run.RunID == "" {
		t.Fatal("missing run")
	}
}

func TestMemoryCoordinatorCreateRequiresExactPlanRevisionAndDigest(t *testing.T) {
	p := plan()
	store, err := NewMemoryCoordinatorStore(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewMemoryCoordinator(store, FakeExecutor{Mode: FakeSuccess}, coordinatorClock())
	if err != nil {
		t.Fatal(err)
	}
	key := "11111111-1111-4111-8111-111111111119"
	if _, err = c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, IdempotencyKey: key}); err == nil {
		t.Fatal("accepted an unpinned plan revision")
	}
	if _, err = c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, IdempotencyKey: key}); err != nil {
		t.Fatal(err)
	}
	drift := plan()
	drift.Stages[0].Steps[0].ScriptRun.Argv = []string{"-x"}
	drift.Digest = ""
	drift.Stages[0].Digest = ""
	drift.Stages[0].Steps[0].Digest = ""
	drift, err = drift.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	store.plans[planID] = drift
	if _, err = c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, IdempotencyKey: key}); err == nil {
		t.Fatal("same-revision plan content drift replayed an old run")
	}
}

func TestMemoryCoordinatorLeaseTakeoverFencesStaleWorker(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 5, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store, err := NewMemoryCoordinatorStore(plan())
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}, 1), make(chan struct{})
	first, err := NewMemoryCoordinator(store, FakeExecutor{Mode: FakeBlocking, Started: started, Continue: release}, clock)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewMemoryCoordinator(store, FakeExecutor{Mode: FakeSuccess}, clock)
	if err != nil {
		t.Fatal(err)
	}
	create := func(c *MemoryCoordinator, key string) RunMaterialization {
		m, e := c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, IdempotencyKey: key})
		if e != nil {
			t.Fatal(e)
		}
		conf := m.Confirmations[0]
		if _, e = c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: conf.ID, ExpectedRevision: conf.Revision, IdempotencyKey: deterministicID("confirm:" + key)}); e != nil {
			t.Fatal(e)
		}
		return m
	}
	m1 := create(first, "11111111-1111-4111-8111-111111111112")
	m2 := create(second, "11111111-1111-4111-8111-111111111113")
	done := make(chan error, 1)
	go func() {
		done <- first.ExecuteClaimedStage(context.Background(), ownerID, m1.Tasks[0].ID, "old-worker", time.Time{})
	}()
	<-started
	if err = second.ExecuteClaimedStage(context.Background(), ownerID, m2.Tasks[0].ID, "new-worker", time.Time{}); err == nil {
		t.Fatal("live target mutation accepted")
	}
	now = now.Add(2 * time.Minute) // intent fence is deliberately non-expiring.
	if err = second.ExecuteClaimedStage(context.Background(), ownerID, m2.Tasks[0].ID, "new-worker", time.Time{}); err != ErrConflict {
		t.Fatalf("expired intent fence accepted takeover: %v", err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatalf("intent owner err=%v", err)
	}
}

func TestMemoryCoordinatorFailBeforeDispatchHasNoReceipt(t *testing.T) {
	c, m := createMemoryRun(t, plan(), FakeFailBeforeDispatch)
	conf := m.Confirmations[0]
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: conf.ID, ExpectedRevision: 1, IdempotencyKey: "44444444-4444-4444-8444-444444444444"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{}); err != nil {
		t.Fatal(err)
	}
	stage, _ := c.GetStage(context.Background(), ownerID, m.Run.RunID, m.Stages[0].StageID)
	if stage.Status != StageFailed || len(c.Attempts(context.Background(), ownerID, stage.StageID)) != 0 {
		t.Fatalf("stage=%#v attempts=%#v", stage, c.Attempts(context.Background(), ownerID, stage.StageID))
	}
	if len(c.store.receipts) != 0 {
		t.Fatal("pre-dispatch failure wrote receipt")
	}
}

func TestMemoryCoordinatorConfirmationCASReplayAndDrift(t *testing.T) {
	c, m := createMemoryRun(t, plan(), FakeSuccess)
	conf := m.Confirmations[0]
	cmd := ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: conf.ID, ExpectedRevision: conf.Revision, IdempotencyKey: "55555555-5555-4555-8555-555555555555"}
	first, err := c.ConfirmStage(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := c.ConfirmStage(context.Background(), cmd); err != nil || replay.Revision != first.Revision {
		t.Fatalf("replay %#v %v", replay, err)
	}
	changedRevision := cmd
	changedRevision.ExpectedRevision++
	if _, err := c.ConfirmStage(context.Background(), changedRevision); err == nil {
		t.Fatal("confirmation replay accepted changed expected revision")
	}
	foreign := cmd
	foreign.OwnerID = "@foreign:example.org"
	if _, err := c.ConfirmStage(context.Background(), foreign); err == nil {
		t.Fatal("foreign confirmation replay was authorized")
	}
	cmd.ExpectedRevision++
	if _, err := c.ConfirmStage(context.Background(), cmd); err == nil {
		t.Fatal("confirmation replay mismatch accepted")
	}

	// A separate pending card fails when the authoritative target snapshot is
	// replaced. The client cannot repair this by echoing the old binding.
	c, m = createMemoryRun(t, plan(), FakeSuccess)
	c.store.mu.Lock()
	mutated := c.store.plans[planID]
	mutated.Targets[0].Digest = Digest("3123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	c.store.plans[planID] = mutated
	c.store.mu.Unlock()
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: "66666666-6666-4666-8666-666666666666"}); err == nil {
		t.Fatal("target drift accepted")
	}
	for i, mutate := range []func(*ExecutionPlan){
		func(p *ExecutionPlan) { p.Placement.Recommended.CostQuote.Amount = "999" },
		func(p *ExecutionPlan) { p.Artifacts[0].Size++ },
		func(p *ExecutionPlan) { p.Stages[0].Title = "changed preview" },
		func(p *ExecutionPlan) {
			p.Stages[0].RollbackSteps = append(p.Stages[0].RollbackSteps, p.Stages[0].Steps[0])
		},
	} {
		c, m = createMemoryRun(t, plan(), FakeSuccess)
		c.store.mu.Lock()
		changed := c.store.plans[planID]
		mutate(&changed)
		c.store.plans[planID] = changed
		c.store.mu.Unlock()
		key := deterministicID("drift-confirm:" + string(rune('a'+i)))
		if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: key}); err == nil {
			t.Fatalf("drift %d accepted", i)
		}
	}
}

func TestMemoryCoordinatorPostIntentCancellationIsUncertain(t *testing.T) {
	started, block := make(chan struct{}, 1), make(chan struct{})
	c, m := createMemoryRun(t, plan(), FakeSuccess)
	c.executor = FakeExecutor{Mode: FakeBlocking, Started: started, Continue: block}
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: "dddddddd-dddd-4ddd-8ddd-dddddddddddd"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.ExecuteClaimedStage(ctx, ownerID, m.Tasks[0].ID, "worker", time.Time{}) }()
	<-started
	cancel()
	if err := <-done; err != ErrUncertain {
		t.Fatalf("cancel result %v", err)
	}
	stage, _ := c.GetStage(context.Background(), ownerID, m.Run.RunID, m.Stages[0].StageID)
	if stage.Status != StageUncertain || !c.store.leases[targetID].uncertain || len(c.store.receipts) != 1 {
		t.Fatalf("intent was not durably fenced: stage=%#v lease=%#v receipts=%d", stage, c.store.leases[targetID], len(c.store.receipts))
	}
}

func TestMemoryCoordinatorPostDispatchErrorIsUncertain(t *testing.T) {
	c, m := createMemoryRun(t, plan(), FakeErrorAfterDispatch)
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{}); err != ErrUncertain {
		t.Fatalf("post-dispatch error=%v", err)
	}
	if got := c.store.stages[stageMapKey(m.Run.RunID, m.Stages[0].StageID)]; got.Status != StageUncertain || len(c.store.receipts) != 1 || !c.store.leases[targetID].uncertain {
		t.Fatalf("post-dispatch error lost fence: stage=%#v receipts=%d lease=%#v", got, len(c.store.receipts), c.store.leases[targetID])
	}
}

func TestMemoryCoordinatorConcurrentReadersDuringDispatch(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	c, m := createMemoryRun(t, plan(), FakeSuccess)
	c.executor = FakeExecutor{Mode: FakeBlocking, Started: started, Continue: release}
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: "ffffffff-ffff-4fff-8fff-ffffffffffff"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{})
	}()
	<-started
	var readers sync.WaitGroup
	for i := 0; i < 16; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 64; j++ {
				_, _ = c.GetRun(context.Background(), ownerID, m.Run.RunID)
				_, _ = c.GetStage(context.Background(), ownerID, m.Run.RunID, m.Stages[0].StageID)
			}
		}()
	}
	readers.Wait()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCoordinatorUncertainReconcileReleasesReservationWithoutRedispatch(t *testing.T) {
	calls := 0
	c, m := createMemoryRun(t, plan(), FakeUncertainAfterDispatch)
	c.executor.Dispatches = &calls
	conf := m.Confirmations[0]
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: conf.ID, ExpectedRevision: 1, IdempotencyKey: "77777777-7777-4777-8777-777777777777"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{}); err != ErrUncertain {
		t.Fatal(err)
	}
	if calls != 1 || !c.store.reservations[conf.ID].Active || !c.store.leases[targetID].uncertain {
		t.Fatalf("calls=%d reservation=%#v lease=%#v", calls, c.store.reservations[conf.ID], c.store.leases[targetID])
	}
	c.executor = FakeExecutor{Mode: FakeReconcileSuccess, Dispatches: &calls}
	if _, err := c.ReconcileStage(context.Background(), ownerID, m.Run.RunID, m.Stages[0].StageID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("reconcile dispatched %d times", calls)
	}
	if _, active := c.store.reservations[conf.ID]; active {
		t.Fatal("reconciliation retained an active reservation")
	}
	stage := c.store.stages[stageMapKey(m.Run.RunID, m.Stages[0].StageID)]
	attempts := c.store.attempts[stage.StageID]
	if stage.Status != StageUncertain || len(attempts) != 1 || attempts[0].Status != AttemptUncertain {
		t.Fatalf("reconciliation rewrote original uncertain evidence: stage=%#v attempts=%#v", stage, attempts)
	}
}

func TestMemoryCoordinatorAuthorizesBeforeCrossOwnerReplay(t *testing.T) {
	c, _ := createMemoryRun(t, plan(), FakeSuccess)
	_, err := c.CreateRun(context.Background(), CreateRunCommand{OwnerID: "other-owner", PlanID: planID, PlanRevision: 1, IdempotencyKey: "11111111-1111-4111-8111-111111111111"})
	if err != ErrConflict {
		t.Fatalf("foreign replay error = %v, want conflict", err)
	}
	if len(c.store.runs) != 1 {
		t.Fatalf("foreign replay changed durable runs: %d", len(c.store.runs))
	}
}

func TestMemoryCoordinatorConfirmationDriftHasNoPartialClaimState(t *testing.T) {
	c, m := createMemoryRun(t, plan(), FakeSuccess)
	conf, stage, task := m.Confirmations[0], m.Stages[0], m.Tasks[0]
	c.store.mu.Lock()
	p := c.store.plans[planID]
	p.Targets[0].Digest = Digest("3123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	c.store.plans[planID] = p
	c.store.mu.Unlock()
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: conf.ID, ExpectedRevision: conf.Revision, IdempotencyKey: "88888888-8888-4888-8888-888888888888"}); err == nil {
		t.Fatal("drift confirmation accepted")
	}
	if got := c.store.stages[stageMapKey(stage.RunID, stage.StageID)]; got.Status != StageWaitingUser || got.TaskID != task.ID {
		t.Fatalf("stage partially advanced: %#v", got)
	}
	if got := c.store.tasks[task.ID]; got.Status != coretask.StatusWaitingUser || got.Lease != nil {
		t.Fatalf("task partially claimed: %#v", got)
	}
	if _, ok := c.store.leases[targetID]; ok {
		t.Fatal("drift confirmation created lease")
	}
}

func TestMemoryCoordinatorRejectsTaskRunRevisionFenceMismatch(t *testing.T) {
	c, m := createMemoryRun(t, plan(), FakeSuccess)
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: "abababab-abab-4bab-8bab-abababababab"}); err != nil {
		t.Fatal(err)
	}
	c.store.mu.Lock()
	task := c.store.tasks[m.Tasks[0].ID]
	task.Spec.Payload.ExecutionStage.RunRevision++
	c.store.tasks[task.ID] = task
	c.store.mu.Unlock()
	if err := c.ExecuteClaimedStage(context.Background(), ownerID, task.ID, "worker", time.Time{}); err != ErrConflict {
		t.Fatalf("run revision mismatch accepted: %v", err)
	}
}

func TestMemoryCoordinatorUncertainFenceSurvivesTTLAndOtherRun(t *testing.T) {
	now := coordinatorClock()()
	clock := func() time.Time { return now }
	store, err := NewMemoryCoordinatorStore(plan())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := NewMemoryCoordinator(store, FakeExecutor{Mode: FakeUncertainAfterDispatch}, clock)
	second, _ := NewMemoryCoordinator(store, FakeExecutor{Mode: FakeSuccess}, clock)
	makeRun := func(c *MemoryCoordinator, key string) RunMaterialization {
		m, e := c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, IdempotencyKey: key})
		if e != nil {
			t.Fatal(e)
		}
		_, e = c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: deterministicID("confirm:" + key)})
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	m1 := makeRun(first, "99999999-9999-4999-8999-999999999991")
	m2 := makeRun(second, "99999999-9999-4999-8999-999999999992")
	if err := first.ExecuteClaimedStage(context.Background(), ownerID, m1.Tasks[0].ID, "one", time.Time{}); err != ErrUncertain {
		t.Fatal(err)
	}
	now = now.Add(48 * time.Hour)
	if err := second.ExecuteClaimedStage(context.Background(), ownerID, m2.Tasks[0].ID, "two", time.Time{}); err != ErrConflict {
		t.Fatalf("uncertain target redispatched: %v", err)
	}
}

func TestMemoryCoordinatorSequentialDispatchStopsAtUncertainStep(t *testing.T) {
	p := plan()
	second := cloneSteps([]ExecutionStep{p.Stages[0].Steps[0]})[0]
	second.StepKey, second.IdempotencyMarker = "deploy-step-2", "deploy-marker-2"
	second.ScriptRun.IdempotencyMarker = second.IdempotencyMarker
	second.Digest = ""
	p.Stages[0].Steps = append(p.Stages[0].Steps, second)
	p.Stages[0].Digest, p.Digest = "", ""
	c, m := createMemoryRun(t, p, FakeSuccess)
	calls := 0
	c.executor = FakeExecutor{UncertainOnDispatch: FakeUncertainAfterDispatch, Dispatches: &calls}
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: "99999999-9999-4999-8999-999999999999"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{}); err != ErrUncertain {
		t.Fatal(err)
	}
	got := c.Attempts(context.Background(), ownerID, m.Stages[0].StageID)
	if calls != 2 || len(got) != 2 || got[0].Status != AttemptSucceeded || got[1].Status != AttemptUncertain || got[0].ReceiptID == "" || got[1].ReceiptID == "" {
		t.Fatalf("dispatch/receipt order not durable: calls=%d attempts=%#v", calls, got)
	}
}

func TestMemoryCoordinatorMaterializedAggregateValidates(t *testing.T) {
	c, m := createMemoryRun(t, plan(), FakeSuccess)
	if _, err := c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, r := range c.store.runs {
		if err := r.Validate(); err != nil {
			t.Fatalf("run invalid: %v", err)
		}
	}
	for _, s := range c.store.stages {
		if err := s.Validate(); err != nil {
			t.Fatalf("stage invalid: %v", err)
		}
	}
	for _, task := range c.store.tasks {
		if err := task.Validate(); err != nil {
			t.Fatalf("task invalid: %v", err)
		}
	}
	for _, a := range c.store.attempts {
		for _, v := range a {
			if err := v.Validate(); err != nil {
				t.Fatalf("attempt invalid: %v", err)
			}
		}
	}
	for _, r := range c.store.receipts {
		if err := r.Validate(); err != nil {
			t.Fatalf("receipt invalid: %v", err)
		}
	}
	for _, events := range c.store.events {
		for _, e := range events {
			if err := e.Validate(); err != nil {
				t.Fatalf("event invalid: %v", err)
			}
		}
	}
}

func TestMemoryCoordinatorRollbackSelectsOnlyApprovedRollbackSteps(t *testing.T) {
	p := plan()
	p.Stages[0].Risk, p.Stages[0].Gate = RiskR0, GateNone
	p.Stages[0].Steps = []ExecutionStep{{StepKey: "forward-inspect", Kind: StepTargetInspect, TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest, TimeoutSeconds: 1, IdempotencyMarker: "forward-inspect", OutputPolicy: OutputDiscard, TargetInspect: &TargetInspectStep{ObservationID: artifactID}}}
	rb := ExecutionStep{StepKey: "rollback-cleanup", Kind: StepCleanup, TimeoutSeconds: 1, IdempotencyMarker: "rollback-cleanup", Cleanup: &CleanupStep{Resource: "resource"}}
	p.Stages[0].RollbackSteps = []ExecutionStep{rb}
	p.Stages[0].RollbackPolicy = &RollbackPolicy{Risk: RiskR4, Gate: GateRollback}
	p.Stages[0].Digest, p.Digest = "", ""
	store, err := NewMemoryCoordinatorStore(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewMemoryCoordinator(store, FakeExecutor{Mode: FakeSuccess}, coordinatorClock())
	if err != nil {
		t.Fatal(err)
	}
	source, err := c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, IdempotencyKey: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb0"})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ExecuteClaimedStage(context.Background(), ownerID, source.Tasks[0].ID, "source-worker", time.Time{}); err != nil {
		t.Fatal(err)
	}
	m, err := c.CreateRun(context.Background(), CreateRunCommand{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, Operation: RunOperationRollback, RollbackOfRunID: source.Run.RunID, IdempotencyKey: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Confirmations[0].Binding.RiskLevel != string(RiskR4) || m.Confirmations[0].Binding.GateType != string(GateRollback) || m.Confirmations[0].Binding.OperationDomain != "execution:v2:rollback" {
		t.Fatalf("rollback confirmation bound forward policy: %#v", m.Confirmations[0].Binding)
	}
	if _, err = c.ConfirmStage(context.Background(), ConfirmStageCommand{OwnerID: ownerID, ConfirmationID: m.Confirmations[0].ID, ExpectedRevision: 1, IdempotencyKey: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}); err != nil {
		t.Fatal(err)
	}
	if err = c.ExecuteClaimedStage(context.Background(), ownerID, m.Tasks[0].ID, "worker", time.Time{}); err != nil {
		t.Fatal(err)
	}
	got := c.Attempts(context.Background(), ownerID, m.Stages[0].StageID)
	if len(got) != 1 || got[0].StepKey != "rollback-cleanup" {
		t.Fatalf("rollback executed forward steps: %#v", got)
	}
}

func TestMemoryCoordinatorRollbackUsesInverseDependencyOrder(t *testing.T) {
	p := plan()
	p.Stages[0].Risk, p.Stages[0].Gate = RiskR0, GateNone
	p.Stages[0].Steps = []ExecutionStep{{
		StepKey: "forward-a", Kind: StepTargetInspect,
		TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest,
		TimeoutSeconds: 1, IdempotencyMarker: "forward-a", OutputPolicy: OutputDiscard,
		TargetInspect: &TargetInspectStep{ObservationID: artifactID},
	}}
	p.Stages[0].RollbackSteps = []ExecutionStep{{
		StepKey: "rollback-a", Kind: StepCleanup, TimeoutSeconds: 1,
		IdempotencyMarker: "rollback-a", Cleanup: &CleanupStep{Resource: "a"},
	}}
	p.Stages[0].RollbackPolicy = &RollbackPolicy{Risk: RiskR4, Gate: GateRollback}
	p.Stages = append(p.Stages, ExecutionStage{
		StageKey: "dependent", Revision: 1, Kind: "deploy",
		Risk: RiskR0, Gate: GateNone, DependsOn: []string{"deploy"},
		TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest,
		Steps: []ExecutionStep{{
			StepKey: "forward-b", Kind: StepTargetInspect,
			TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest,
			TimeoutSeconds: 1, IdempotencyMarker: "forward-b", OutputPolicy: OutputDiscard,
			TargetInspect: &TargetInspectStep{ObservationID: artifactID},
		}},
		RollbackSteps: []ExecutionStep{{
			StepKey: "rollback-b", Kind: StepCleanup, TimeoutSeconds: 1,
			IdempotencyMarker: "rollback-b", Cleanup: &CleanupStep{Resource: "b"},
		}},
		RollbackPolicy: &RollbackPolicy{Risk: RiskR4, Gate: GateRollback},
		TimeoutSeconds: 60,
	})
	p.Digest = ""
	p.Stages[0].Digest = ""
	p.Stages[1].Digest = ""
	store, err := NewMemoryCoordinatorStore(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewMemoryCoordinator(store, FakeExecutor{Mode: FakeSuccess}, coordinatorClock())
	if err != nil {
		t.Fatal(err)
	}
	source, err := c.CreateRun(context.Background(), CreateRunCommand{
		OwnerID: ownerID, PlanID: planID, PlanRevision: 1,
		IdempotencyKey: "dddddddd-dddd-4ddd-8ddd-dddddddddd01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ExecuteClaimedStage(context.Background(), ownerID, source.Tasks[0].ID, "source-a", time.Time{}); err != nil {
		t.Fatal(err)
	}
	var sourceB RunStage
	for _, stage := range c.store.stages {
		if stage.RunID == source.Run.RunID && stage.StageKey == "dependent" {
			sourceB = stage
		}
	}
	if sourceB.TaskID == "" {
		t.Fatal("dependent source stage was not materialized")
	}
	if err = c.ExecuteClaimedStage(context.Background(), ownerID, sourceB.TaskID, "source-b", time.Time{}); err != nil {
		t.Fatal(err)
	}

	rollback, err := c.CreateRun(context.Background(), CreateRunCommand{
		OwnerID: ownerID, PlanID: planID, PlanRevision: 1,
		Operation: RunOperationRollback, RollbackOfRunID: source.Run.RunID,
		IdempotencyKey: "dddddddd-dddd-4ddd-8ddd-dddddddddd02",
	})
	if err != nil {
		t.Fatal(err)
	}
	var rollbackA, rollbackB RunStage
	for _, stage := range rollback.Stages {
		switch stage.StageKey {
		case "deploy":
			rollbackA = stage
		case "dependent":
			rollbackB = stage
		}
	}
	if rollbackB.Status != StageWaitingUser || rollbackA.Status != StageBlocked {
		t.Fatalf("rollback did not start from the dependent stage: a=%#v b=%#v", rollbackA, rollbackB)
	}
	confirmAndRun := func(stage RunStage, confirmationKey, holder string) {
		t.Helper()
		confirmation := c.store.confirmations[stage.ConfirmationID]
		if _, e := c.ConfirmStage(context.Background(), ConfirmStageCommand{
			OwnerID: ownerID, ConfirmationID: confirmation.ID,
			ExpectedRevision: confirmation.Revision, IdempotencyKey: confirmationKey,
		}); e != nil {
			t.Fatal(e)
		}
		if e := c.ExecuteClaimedStage(context.Background(), ownerID, stage.TaskID, holder, time.Time{}); e != nil {
			t.Fatal(e)
		}
	}
	confirmAndRun(rollbackB, "dddddddd-dddd-4ddd-8ddd-dddddddddd03", "rollback-b")
	rollbackA = c.store.stages[stageMapKey(rollback.Run.RunID, rollbackA.StageID)]
	if rollbackA.Status != StageWaitingUser {
		t.Fatalf("prerequisite rollback unlocked before dependent completed: %#v", rollbackA)
	}
	confirmAndRun(rollbackA, "dddddddd-dddd-4ddd-8ddd-dddddddddd04", "rollback-a")
	if attempts := c.Attempts(context.Background(), ownerID, rollbackB.StageID); len(attempts) != 1 || attempts[0].StepKey != "rollback-b" {
		t.Fatalf("dependent rollback leaked forward steps: %#v", attempts)
	}
	if attempts := c.Attempts(context.Background(), ownerID, rollbackA.StageID); len(attempts) != 1 || attempts[0].StepKey != "rollback-a" {
		t.Fatalf("prerequisite rollback leaked forward steps: %#v", attempts)
	}
}
