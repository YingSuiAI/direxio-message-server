package coreworkload

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type reconcileProvider struct {
	reads, applies, destroys int
	readback                 Readback
	readErr                  error
}

func (p *reconcileProvider) Apply(context.Context, Plan, Operation) (Readback, error) {
	p.applies++
	return Readback{}, nil
}
func (p *reconcileProvider) Destroy(context.Context, Plan, Operation) (Readback, error) {
	p.destroys++
	return Readback{}, nil
}
func (p *reconcileProvider) Read(context.Context, Plan, Operation) (Readback, error) {
	p.reads++
	return p.readback, p.readErr
}

func uncertainMemoryHandler(t *testing.T, provider Provider) (*Handler, *MemoryStore, string, TargetIdentity) {
	t.Helper()
	s := NewMemoryStore(time.Now)
	planID, operationID, workloadID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	identity := TargetIdentity{Kind: TargetAWSEC2SSM, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-123"}
	s.plans[planID] = Plan{ID: planID, Revision: 1, Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetKind: TargetAWSEC2SSM, Target: TargetSettings{Identity: identity}, ExpiresAt: time.Now().Add(time.Hour)}
	s.workloads[workloadID] = Workload{ID: workloadID, Revision: 2, PlanID: planID, PlanDigest: s.plans[planID].Digest, TargetKind: TargetAWSEC2SSM, State: "uncertain"}
	s.operations[operationID] = Operation{ID: operationID, WorkloadID: workloadID, PlanID: planID, Kind: OperationApply, PlanRevision: 1, PlanDigest: s.plans[planID].Digest, TargetKind: TargetAWSEC2SSM, Status: OperationUncertain, DispatchState: "uncertain", Revision: 3, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	h, err := NewHandler(s, provider)
	if err != nil {
		t.Fatal(err)
	}
	return h, s, operationID, identity
}

func TestRecoverUncertainUsesReadOnlyProviderPath(t *testing.T) {
	provider := &reconcileProvider{}
	h, s, operationID, identity := uncertainMemoryHandler(t, provider)
	provider.readback = Readback{WorkloadID: s.operations[operationID].WorkloadID, TargetKind: TargetAWSEC2SSM, State: "ready", Identity: identity}
	got, err := h.Reconcile(context.Background(), operationID)
	if err != nil || got.Status != OperationSucceeded {
		t.Fatalf("reconcile = %+v, err=%v", got, err)
	}
	if provider.reads != 1 || provider.applies != 0 || provider.destroys != 0 {
		t.Fatalf("provider calls reads=%d applies=%d destroys=%d", provider.reads, provider.applies, provider.destroys)
	}
	if workload, err := s.GetWorkload(context.Background(), got.WorkloadID); err != nil || workload.State != "ready" {
		t.Fatalf("workload after reconcile = %+v, err=%v", workload, err)
	}
}

func TestRecoverUncertainRetainsAmbiguousState(t *testing.T) {
	provider := &reconcileProvider{readback: Readback{}}
	h, s, operationID, _ := uncertainMemoryHandler(t, provider)
	got, err := h.Reconcile(context.Background(), operationID)
	if err != nil || got.Status != OperationUncertain || got.DispatchState != "uncertain" {
		t.Fatalf("ambiguous reconcile = %+v, err=%v", got, err)
	}
	if provider.reads != 1 || provider.applies != 0 || provider.destroys != 0 {
		t.Fatalf("provider calls reads=%d applies=%d destroys=%d", provider.reads, provider.applies, provider.destroys)
	}
	if workload, err := s.GetWorkload(context.Background(), got.WorkloadID); err != nil || workload.State != "uncertain" {
		t.Fatalf("workload after ambiguous reconcile = %+v, err=%v", workload, err)
	}
}

func TestRecoverUncertainRedactsProviderErrorInEvents(t *testing.T) {
	secret := "AKIA_TEST_SECRET_DO_NOT_PERSIST"
	provider := &reconcileProvider{readErr: errors.New(secret)}
	h, s, operationID, _ := uncertainMemoryHandler(t, provider)
	got, err := h.Reconcile(context.Background(), operationID)
	if err != nil || got.Status != OperationUncertain {
		t.Fatalf("secret error reconcile = %+v, err=%v", got, err)
	}
	for _, event := range s.events[operationID] {
		if strings.Contains(event.Message, secret) {
			t.Fatalf("provider secret leaked into event: %+v", event)
		}
	}
}
