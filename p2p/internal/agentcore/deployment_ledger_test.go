package agentcore

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

type emptyDeploymentLedger struct{}

func (emptyDeploymentLedger) UpsertDeploymentMutation(context.Context, string, DeploymentMutation) error {
	return nil
}
func (emptyDeploymentLedger) UpsertDeploymentEvent(context.Context, string, map[string]any) error {
	return nil
}
func (emptyDeploymentLedger) ListDeployments(context.Context, string, DeploymentListOptions) ([]map[string]any, string, error) {
	return []map[string]any{}, "", nil
}
func (emptyDeploymentLedger) GetDeployment(context.Context, string, string) (map[string]any, bool, error) {
	return nil, false, nil
}
func (emptyDeploymentLedger) ListDeploymentEvents(context.Context, string, string, int64, int) ([]map[string]any, int64, error) {
	return []map[string]any{}, 0, nil
}
func (emptyDeploymentLedger) LastDeploymentOperationSequence(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (emptyDeploymentLedger) DeploymentCandidates(context.Context, string, int) ([]DeploymentReconcileCandidate, error) {
	return []DeploymentReconcileCandidate{}, nil
}
func (emptyDeploymentLedger) ClaimDeploymentBatch(context.Context, string, string, int64, int) ([]DeploymentReconcileCandidate, error) {
	return []DeploymentReconcileCandidate{}, nil
}
func (emptyDeploymentLedger) ReleaseDeploymentLease(context.Context, string, string, string, int64, bool) error {
	return nil
}
func (emptyDeploymentLedger) CommitDeploymentReconciliation(context.Context, string, string, string, int64, DeploymentMutation, ...map[string]any) error {
	return nil
}

func TestDashboardReadDoesNotRequireRetiredCore(t *testing.T) {
	client, err := New(Config{
		DeploymentLedger: emptyDeploymentLedger{},
		OwnerID:          func() string { return "owner" },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, actionErr := client.dashboardGet(context.Background(), nil)
	if actionErr != nil {
		t.Fatalf("dashboardGet() action error = %#v", actionErr)
	}
	dashboard := result.(map[string]any)
	if dashboard["partial"] != true {
		t.Fatalf("dashboard partial = %#v, want true without migrated task aggregates", dashboard["partial"])
	}
}

func TestDeploymentObjectSanitizesTargetAndInfersCount(t *testing.T) {
	op := map[string]any{
		"workload_id": "w", "operation_id": "o", "plan_id": "p", "plan_digest": "sha",
		"kind": "apply", "status": "succeeded", "target_kind": "aws-ecs", "task_id": "t",
		"desired_plan": map[string]any{"target": map[string]any{"identity": map[string]any{
			"kind": "aws-ecs", "aws_region": "us-east-1", "aws_account_id": "123", "cluster": "c", "service": "s", "aws_ecs_desired_count": int64(3), "secret_access_key": "never",
		}}},
	}
	obj := DeploymentObject(DeploymentMutation{Operation: op})
	if got := obj["desired_server_count"]; got != int64(3) {
		t.Fatalf("desired count = %#v", got)
	}
	target := obj["target"].(map[string]any)
	if _, ok := target["secret_access_key"]; ok {
		t.Fatal("secret target field persisted")
	}
	if obj["estimated_monthly_usd"] != nil || obj["estimated_accrued_usd"] != nil {
		t.Fatal("missing quote must remain nullable")
	}
	current := obj["current_operation"].(map[string]any)
	if _, ok := current["desired_plan"]; ok {
		t.Fatal("desired plan persisted in current operation")
	}
}

func TestDeploymentOperationAndErrorDoNotPersistFreeFormFailureText(t *testing.T) {
	op := map[string]any{"workload_id": "w", "operation_id": "o", "plan_id": "p", "kind": "credential=AKIA/secret", "status": "failed", "failure_code": "AKIA-secret/credential", "failure_summary": "secret-canary"}
	obj := DeploymentObject(DeploymentMutation{Operation: op})
	if errObj, ok := obj["error"].(map[string]any); !ok || errObj["code"] != "deployment_failed" || errObj["summary"] != "Deployment failed" {
		t.Fatalf("error summary leaked: %#v", obj["error"])
	}
	operation := SanitizedDeploymentOperation(op)
	if _, ok := operation["failure_summary"]; ok {
		t.Fatalf("failure summary persisted: %#v", operation)
	}
	if operation["failure_code"] != "deployment_failed" || operation["kind"] != "unknown" {
		t.Fatalf("unsafe operation identifiers: %#v", operation)
	}
}

func TestSanitizedDeploymentCodeRejectsUnsafeAndOverlongValues(t *testing.T) {
	if got := SanitizedDeploymentCode("credential=AKIA/secret", "unknown"); got != "unknown" {
		t.Fatalf("unsafe code=%q", got)
	}
	if got := SanitizedDeploymentCode("valid_code", "unknown"); got != "valid_code" {
		t.Fatalf("valid code=%q", got)
	}
	if got := SanitizedDeploymentCode(string(make([]byte, 65)), "unknown"); got != "unknown" {
		t.Fatalf("overlong code=%q", got)
	}
	if got := SanitizedDeploymentFailureCode(strings.Repeat("a", 64)); got != "deployment_failed" {
		t.Fatalf("unknown lowercase failure code=%q", got)
	}
	if got := SanitizedDeploymentEventKind(strings.Repeat("a", 64)); got != "unknown" {
		t.Fatalf("unknown lowercase event kind=%q", got)
	}
}

func TestMergeDeploymentObjectDoesNotDoubleCountSameOperation(t *testing.T) {
	previous := map[string]any{"latest_operation_id": "o", "estimated_accrued_usd": float64(10)}
	current := map[string]any{"latest_operation_id": "o", "estimated_accrued_usd": float64(3)}
	merged := MergeDeploymentObject(previous, current)
	if merged["estimated_accrued_usd"] != float64(3) {
		t.Fatalf("same operation accrued=%v, want 3", merged["estimated_accrued_usd"])
	}
}

func TestMergeDestroyAccruesOnlySinceLastSync(t *testing.T) {
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	t1 := t0.Add(1 * time.Hour)
	t2 := t0.Add(2 * time.Hour)
	rate := 73.0
	previous := map[string]any{"latest_operation_id": "apply", "status": "succeeded", "estimated_monthly_usd": rate, "estimated_accrued_usd": rate / 730, "active_from": t0.Format(time.RFC3339Nano), "last_synced": t1.Format(time.RFC3339Nano)}
	current := map[string]any{"latest_operation_id": "destroy", "status": "destroyed", "destroyed_at": t2.Format(time.RFC3339Nano), "estimated_accrued_usd": nil, "estimated_monthly_usd": nil}
	merged := MergeDeploymentObject(previous, current)
	want := rate / 730 * 2
	if got, _ := deploymentNumber(merged["estimated_accrued_usd"]); math.Abs(got-want) > 0.001 {
		t.Fatalf("destroy accrued=%v want=%v", got, want)
	}
	if merged["estimated_monthly_usd"] != nil {
		t.Fatalf("destroy retained monthly rate: %#v", merged["estimated_monthly_usd"])
	}
}

func TestMergeRateSwitchRetainsPriorAccruedBase(t *testing.T) {
	previous := map[string]any{"latest_operation_id": "apply-1", "estimated_accrued_usd": float64(10), "estimated_monthly_usd": float64(73)}
	current := map[string]any{"latest_operation_id": "apply-2", "estimated_accrued_usd": float64(4), "estimated_monthly_usd": float64(146)}
	merged := MergeDeploymentObject(previous, current)
	if got, _ := deploymentNumber(merged["estimated_accrued_usd"]); got != 14 {
		t.Fatalf("rate switch accrued=%v want=14", got)
	}
	next := MergeDeploymentObject(merged, map[string]any{"latest_operation_id": "apply-2", "estimated_accrued_usd": float64(5), "estimated_monthly_usd": float64(146)})
	if got, _ := deploymentNumber(next["estimated_accrued_usd"]); got != 15 {
		t.Fatalf("same rate segment accrued=%v want=15", got)
	}
	third := MergeDeploymentObject(next, map[string]any{"latest_operation_id": "apply-2", "estimated_accrued_usd": float64(6), "estimated_monthly_usd": float64(146)})
	if got, _ := deploymentNumber(third["estimated_accrued_usd"]); got != 16 || third["_accrued_base_usd"] != float64(10) {
		t.Fatalf("third same rate accrued/base=%v/%v want 16/10", got, third["_accrued_base_usd"])
	}
	if public := StripDeploymentInternalFields(third); public["_accrued_base_usd"] != nil {
		t.Fatalf("internal accrued base leaked: %#v", public)
	}
}

func TestMergeDestroyReplayPreservesAccruedTotal(t *testing.T) {
	previous := map[string]any{"latest_operation_id": "destroy", "status": "destroyed", "estimated_accrued_usd": float64(15), "_accrued_base_usd": float64(15), "estimated_monthly_usd": nil, "active_from": "2026-01-01T00:00:00Z", "last_synced": "2026-01-02T00:00:00Z"}
	current := map[string]any{"latest_operation_id": "destroy", "status": "destroyed", "destroyed_at": "2026-01-02T00:00:00Z", "estimated_accrued_usd": nil, "estimated_monthly_usd": nil}
	merged := MergeDeploymentObject(previous, current)
	if got, _ := deploymentNumber(merged["estimated_accrued_usd"]); got != 15 {
		t.Fatalf("destroy replay accrued=%v want=15", got)
	}
}

func TestMergeRunningDestroyAdvancesFromLastSync(t *testing.T) {
	t1 := time.Now().UTC().Add(-2 * time.Hour)
	rate := 73.0
	previous := map[string]any{"latest_operation_id": "destroy", "status": "running", "estimated_accrued_usd": float64(10), "_accrued_base_usd": float64(10), "estimated_monthly_usd": rate, "active_from": t1.Add(-time.Hour).Format(time.RFC3339Nano), "last_synced": t1.Format(time.RFC3339Nano)}
	current := map[string]any{"latest_operation_id": "destroy", "status": "running", "current_operation": map[string]any{"kind": "destroy"}, "last_synced": time.Now().UTC().Format(time.RFC3339Nano), "estimated_monthly_usd": nil, "estimated_accrued_usd": nil}
	merged := MergeDeploymentObject(previous, current)
	if got, _ := deploymentNumber(merged["estimated_accrued_usd"]); got <= 10 {
		t.Fatalf("running destroy did not advance accrued=%v", got)
	}
}

func TestMergeRunningDestroyAccumulatesEveryIntervalThenTerminal(t *testing.T) {
	t0 := time.Now().UTC().Add(-5 * time.Hour)
	previous := map[string]any{"latest_operation_id": "destroy", "status": "running", "estimated_accrued_usd": float64(10), "_accrued_base_usd": float64(10), "estimated_monthly_usd": float64(730), "active_from": t0.Format(time.RFC3339Nano), "last_synced": t0.Add(time.Hour).Format(time.RFC3339Nano), "current_operation": map[string]any{"kind": "destroy"}}
	run1 := MergeDeploymentObject(previous, map[string]any{"latest_operation_id": "destroy", "status": "running", "last_synced": t0.Add(2 * time.Hour).Format(time.RFC3339Nano), "current_operation": map[string]any{"kind": "destroy"}})
	run2 := MergeDeploymentObject(run1, map[string]any{"latest_operation_id": "destroy", "status": "running", "last_synced": t0.Add(3 * time.Hour).Format(time.RFC3339Nano), "current_operation": map[string]any{"kind": "destroy"}})
	terminal := MergeDeploymentObject(run2, map[string]any{"latest_operation_id": "destroy", "status": "destroyed", "destroyed_at": t0.Add(4 * time.Hour).Format(time.RFC3339Nano), "current_operation": map[string]any{"kind": "destroy"}})
	if got, _ := deploymentNumber(terminal["estimated_accrued_usd"]); math.Abs(got-13) > 0.001 {
		t.Fatalf("running destroy total=%v want=13", got)
	}
}
