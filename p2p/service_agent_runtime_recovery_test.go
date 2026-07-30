package p2p

import (
	"context"
	"errors"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	agentruntime "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/runtime"
	agenttask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

type runtimeWorkloadFake struct {
	op   coreworkload.Operation
	plan coreworkload.Plan
}

func (f *runtimeWorkloadFake) GetOperation(context.Context, string) (coreworkload.Operation, error) {
	return f.op, nil
}
func (f *runtimeWorkloadFake) GetPlan(context.Context, string) (coreworkload.Plan, error) {
	return f.plan, nil
}

type runtimeAWSFake struct {
	claimErr error
	lease    coreaws.ProvisionMutationLease
}

func (f runtimeAWSFake) AcquireProvisionMutation(context.Context, string) (coreaws.ProvisionMutationLease, error) {
	return f.lease, nil
}
func (f runtimeAWSFake) ClaimProvisionMutation(context.Context, string, string) (coreaws.ProvisionMutationLease, error) {
	return f.lease, f.claimErr
}
func (runtimeAWSFake) GetProvisionForOwner(context.Context, string, string) (coreaws.Provision, error) {
	return coreaws.Provision{}, nil
}
func (runtimeAWSFake) GetCredentialRevision(context.Context, string, int64) (coreaws.Credentials, error) {
	return coreaws.Credentials{}, nil
}
func (runtimeAWSFake) GetPlanForOwner(context.Context, string, string) (coreaws.PlanView, error) {
	return coreaws.PlanView{}, nil
}

type runtimeLeaseFake struct{ asserts, uncertain, releases int }

func (f *runtimeLeaseFake) Renew(context.Context) error                 { return nil }
func (f *runtimeLeaseFake) Assert(context.Context) error                { f.asserts++; return nil }
func (f *runtimeLeaseFake) MarkUncertain(context.Context, string) error { f.uncertain++; return nil }
func (f *runtimeLeaseFake) Release(context.Context) error               { f.releases++; return nil }

type runtimeHandlerFake struct{ workloads *runtimeWorkloadFake }

func (f runtimeHandlerFake) Handle(context.Context, string, string, uint64, ...coreworkload.TaskFence) (coreworkload.Operation, error) {
	f.workloads.op.Status, f.workloads.op.DispatchState = coreworkload.OperationSucceeded, "terminal"
	return f.workloads.op, nil
}
func (f runtimeHandlerFake) Recover(ctx context.Context, id string, fence ...coreworkload.TaskFence) (coreworkload.Operation, error) {
	return f.Handle(ctx, id, "", 0, fence...)
}

func runtimeGeoTask() agenttask.Task {
	return agenttask.Task{OwnerID: "owner", ID: "task", Attempt: 1, LeaseEpoch: 1, Revision: 1, Lease: &agenttask.Lease{Holder: "worker", ExpiresAt: time.Now().Add(time.Minute)}, Spec: agenttask.TaskSpec{Payload: agenttask.TaskPayload{Workload: &agenttask.WorkloadTaskPayload{OperationID: "operation", PlanID: "plan", WorkloadID: "workload", PlanRevision: 1, PlanDigest: "digest", TargetKind: string(coreworkload.TargetAWSEC2SSM)}}}}
}

func runtimeGeoPlan() coreworkload.Plan {
	return coreworkload.Plan{ID: "plan", Target: coreworkload.TargetSettings{EC2CleanupProfile: coreworkload.EC2CleanupProfileGeoLibreStaticV1, Labels: map[string]string{"dirextalk:provision-id": "provision", "dirextalk:provision-revision": "1"}}}
}

func TestEmbeddedTaskExecutorDefersRecoveredGeoLibreLiveMutationLease(t *testing.T) {
	workloads := &runtimeWorkloadFake{op: coreworkload.Operation{ID: "operation", TaskID: "task", PlanID: "plan", WorkloadID: "workload", PlanRevision: 1, PlanDigest: "digest", TargetKind: coreworkload.TargetAWSEC2SSM, Status: coreworkload.OperationRunning, DispatchState: "dispatched"}, plan: runtimeGeoPlan()}
	executor := embeddedTaskExecutor{geoWorkloads: workloads, geoAWS: runtimeAWSFake{claimErr: coreaws.ErrConflict}}
	_, err := executor.executeWorkload(context.Background(), runtimeGeoTask())
	if !errors.Is(err, agentruntime.ErrTaskDeferred) {
		t.Fatalf("live recovered mutation lease error = %v, want ErrTaskDeferred", err)
	}
	if workloads.op.Status != coreworkload.OperationRunning || workloads.op.DispatchState != "dispatched" {
		t.Fatalf("deferred recovery terminalized operation: %+v", workloads.op)
	}
}

func TestEmbeddedTaskExecutorReturnsFinalizedBeforeClearedMutationLeaseAssert(t *testing.T) {
	lease := &runtimeLeaseFake{}
	workloads := &runtimeWorkloadFake{op: coreworkload.Operation{ID: "operation", TaskID: "task", PlanID: "plan", WorkloadID: "workload", PlanRevision: 1, PlanDigest: "digest", TargetKind: coreworkload.TargetAWSEC2SSM, Status: coreworkload.OperationRunning, DispatchState: "dispatched", Revision: 1}, plan: runtimeGeoPlan()}
	executor := embeddedTaskExecutor{geoWorkloads: workloads, geoAWS: runtimeAWSFake{lease: lease}, geoHandler: runtimeHandlerFake{workloads: workloads}, validateGeoLibre: func(coreworkload.Plan, coreaws.Provision, coreaws.Credentials, string, coreaws.PlanView, bool) error {
		return nil
	}}
	_, err := executor.executeWorkload(context.Background(), runtimeGeoTask())
	if !errors.Is(err, agentruntime.ErrTaskFinalized) {
		t.Fatalf("terminal dispatch error = %v, want ErrTaskFinalized", err)
	}
	if lease.asserts != 1 || lease.uncertain != 0 || lease.releases != 1 {
		t.Fatalf("lease after terminal dispatch = asserts:%d uncertain:%d releases:%d, want 1/0/1", lease.asserts, lease.uncertain, lease.releases)
	}
}
