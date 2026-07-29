package agentembedded

import (
	"context"
	"testing"
	"time"
)

type deploymentContractLedger struct {
	eventPageSize int
}

func (l *deploymentContractLedger) ListDeployments(context.Context, string, DeploymentListOptions) ([]map[string]any, string, error) {
	return []map[string]any{{
		"workload_id":       "11111111-1111-4111-8111-111111111111",
		"target_kind":       "aws-ecs",
		"target":            map[string]any{},
		"summary":           "demo",
		"status":            "running",
		"current_operation": map[string]any{"operation_id": "22222222-2222-4222-8222-222222222222"},
		"actual":            map[string]any{"state": "running"},
	}}, "", nil
}

func (l *deploymentContractLedger) GetDeployment(context.Context, string, string) (map[string]any, bool, error) {
	items, _, err := l.ListDeployments(context.Background(), "", DeploymentListOptions{})
	return items[0], true, err
}

func (l *deploymentContractLedger) ListDeploymentEvents(_ context.Context, _, _ string, _ int64, pageSize int) ([]map[string]any, int64, error) {
	l.eventPageSize = pageSize
	return []map[string]any{{
		"event_id":     "33333333-3333-4333-8333-333333333333",
		"workload_id":  "11111111-1111-4111-8111-111111111111",
		"operation_id": "22222222-2222-4222-8222-222222222222",
		"sequence":     int64(7),
		"type":         "apply.running",
		"status":       "running",
		"message":      "running",
		"occurred_at":  time.Now().UTC().Format(time.RFC3339Nano),
	}}, 7, nil
}

func deploymentContractModule(ledger *deploymentContractLedger) *Module {
	return New(Config{
		OwnerID:     func() string { return "@owner:example.com" },
		Deployments: ledger,
		CapabilityReady: func(capability string) bool {
			return capability == "deployments.server"
		},
	})
}

func TestDeploymentDashboardMatchesGeneratedContract(t *testing.T) {
	result, actionErr := deploymentContractModule(&deploymentContractLedger{}).
		Handlers()["agent.core.dashboard.get"](context.Background(), map[string]any{"recent_limit": int64(5)})
	if actionErr != nil {
		t.Fatalf("dashboard error: %#v", actionErr)
	}
	body := result.(map[string]any)
	for _, key := range []string{"summary", "deployments", "observed_at", "partial", "warnings"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("dashboard response missing %q: %#v", key, body)
		}
	}
	if _, legacy := body["recent_deployments"]; legacy {
		t.Fatalf("dashboard leaked legacy response key: %#v", body)
	}
	if _, err := time.Parse(time.RFC3339Nano, body["observed_at"].(string)); err != nil {
		t.Fatalf("observed_at is not RFC3339: %v", err)
	}
}

func TestDeploymentDetailsAndEventsMatchGeneratedContract(t *testing.T) {
	ledger := &deploymentContractLedger{}
	module := deploymentContractModule(ledger)
	details, actionErr := module.Handlers()["agent.core.deployments.get"](
		context.Background(),
		map[string]any{"workload_id": "11111111-1111-4111-8111-111111111111"},
	)
	if actionErr != nil {
		t.Fatalf("details error: %#v", actionErr)
	}
	body := details.(map[string]any)
	deployment := body["deployment"].(map[string]any)
	if body["current_operation"] == nil || body["actual"] == nil {
		t.Fatalf("details omitted operation or actual: %#v", body)
	}
	if deployment["current_operation"] != nil || deployment["actual"] != nil {
		t.Fatalf("details duplicated nested projections: %#v", deployment)
	}

	events, actionErr := module.Handlers()["agent.core.deployments.events"](
		context.Background(),
		map[string]any{
			"workload_id":    "11111111-1111-4111-8111-111111111111",
			"after_sequence": int64(2),
			"page_size":      int64(25),
		},
	)
	if actionErr != nil {
		t.Fatalf("events error: %#v", actionErr)
	}
	eventBody := events.(map[string]any)
	if eventBody["next_after_sequence"] != int64(7) {
		t.Fatalf("next_after_sequence = %#v", eventBody["next_after_sequence"])
	}
	if _, legacy := eventBody["next_sequence"]; legacy {
		t.Fatalf("events leaked legacy response key: %#v", eventBody)
	}
	if ledger.eventPageSize != 25 {
		t.Fatalf("page_size was not forwarded: %d", ledger.eventPageSize)
	}
}
