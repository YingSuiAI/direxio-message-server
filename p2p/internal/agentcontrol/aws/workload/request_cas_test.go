package coreworkload

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemoryRequestOperationWorkloadRevisionCAS(t *testing.T) {
	s := NewMemoryStore(time.Now)
	p := Plan{ID: uuid.NewString(), Revision: 1, Digest: "digest", TargetKind: TargetAWSEC2SSM, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	s.plans[p.ID] = p
	cmd := func(id string, revision uint64, kind OperationKind) RequestCommand {
		return RequestCommand{PlanID: p.ID, WorkloadID: id, ExpectedWorkloadRevision: revision, Kind: kind, IdempotencyKey: uuid.NewString(), ExpiresAt: p.ExpiresAt}
	}
	missing := uuid.NewString()
	if _, err := s.RequestOperation(context.Background(), cmd(missing, 1, OperationApply)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v, want not found", err)
	}
	id := uuid.NewString()
	s.workloads[id] = Workload{ID: id, Revision: 2, PlanID: p.ID, PlanDigest: p.Digest, TargetKind: p.TargetKind, State: "ready"}
	if _, err := s.RequestOperation(context.Background(), cmd(id, 1, OperationApply)); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale = %v", err)
	}
	if _, err := s.RequestOperation(context.Background(), cmd(id, 2, OperationApply)); err != nil {
		t.Fatalf("exact allowed = %v", err)
	}
	id = uuid.NewString()
	s.workloads[id] = Workload{ID: id, Revision: 2, PlanID: uuid.NewString(), PlanDigest: p.Digest, TargetKind: p.TargetKind, State: "ready"}
	if _, err := s.RequestOperation(context.Background(), cmd(id, 2, OperationDestroy)); !errors.Is(err, ErrConflict) {
		t.Fatalf("binding mismatch = %v", err)
	}
}

func TestMemoryConsumeTerminalizesStaleWorkloadBeforeDispatch(t *testing.T) {
	now := time.Now().UTC()
	s := NewMemoryStore(func() time.Time { return now })
	p := Plan{ID: uuid.NewString(), Revision: 1, Digest: "digest", TargetKind: TargetAWSEC2SSM, ExpiresAt: now.Add(time.Hour)}
	s.plans[p.ID] = p
	id := uuid.NewString()
	s.workloads[id] = Workload{ID: id, Revision: 1, PlanID: p.ID, PlanDigest: p.Digest, TargetKind: p.TargetKind, State: "ready"}
	requested, err := s.RequestOperation(context.Background(), RequestCommand{PlanID: p.ID, WorkloadID: id, ExpectedWorkloadRevision: 1, Kind: OperationApply, IdempotencyKey: uuid.NewString(), ExpiresAt: p.ExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Confirm(context.Background(), requested.Confirmation.ConfirmationID, requested.Confirmation.Revision); err != nil {
		t.Fatal(err)
	}
	s.workloads[id] = Workload{ID: id, Revision: 2, PlanID: p.ID, PlanDigest: p.Digest, TargetKind: p.TargetKind, State: "ready"}
	if _, _, err = s.Consume(context.Background(), requested.Operation.ID, requested.Confirmation.ConfirmationID, p.Digest, requested.Operation.Revision); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale consume = %v, want revision conflict", err)
	}
	op, err := s.GetOperation(context.Background(), requested.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != OperationFailed || op.DispatchState != "terminal" || op.FailureCode != "workload_revision_conflict" {
		t.Fatalf("stale operation was not terminalized: %+v", op)
	}
}
