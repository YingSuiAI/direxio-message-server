package storage

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcore"
)

func TestWorkloadDeploymentEventTypeMapsInternalKinds(t *testing.T) {
	tests := map[string]string{
		"requested":          "queued",
		"consumed":           "dispatch",
		"provider_error":     "error",
		"uncertain":          "error",
		"readback":           "progress",
		"recovered_readback": "progress",
		"completed":          "complete",
		"succeeded":          "succeeded",
		"destroyed":          "destroyed",
	}
	for kind, want := range tests {
		if got := workloadDeploymentEventType(kind, "running"); got != want {
			t.Fatalf("kind %q mapped to %q, want %q", kind, got, want)
		}
	}
	if got := workloadDeploymentEventType("legacy_unknown", "succeeded"); got != "succeeded" {
		t.Fatalf("status fallback = %q, want succeeded", got)
	}
	if got := workloadDeploymentEventType("credential=secret", "running"); got != "unknown" {
		t.Fatalf("unsafe kind = %q, want unknown", got)
	}
}

func TestWorkloadDeploymentEventStatusIsPublicProjection(t *testing.T) {
	for raw, want := range map[string]string{
		"waiting_user": "pending",
		"queued":       "pending",
		"running":      "running",
		"uncertain":    "uncertain",
		"completed":    "succeeded",
		"canceled":     "failed",
	} {
		got := normalizeDeploymentStatusForStorage(raw)
		if raw == "uncertain" {
			// Uncertain is retained by the workload domain until reconciliation;
			// it is not silently presented as success.
			if got != want {
				t.Fatalf("status %q normalized to %q, want %q", raw, got, want)
			}
			continue
		}
		if got != want {
			t.Fatalf("status %q normalized to %q, want %q", raw, got, want)
		}
	}
}

func TestUnifiedDeploymentListKeepsLegacyCursorAcrossPublicUUIDMigration(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := NewWorkloadDeploymentSource(&DatabaseStore{db: db})
	updated := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	const owner = "@owner:example.test"
	const legacyA = "00000000-0000-f000-f000-000000000003"
	const legacyB = "00000000-0000-1001-1000-000000000002"
	const legacyC = "00000000-0000-e000-e000-000000000001"
	const publicA = "00000000-0000-3000-b000-000000000003"
	const publicB = "00000000-0000-3001-9000-000000000002"
	const publicC = "00000000-0000-3000-a000-000000000001"
	query := regexp.QuoteMeta("ORDER BY d.updated_at DESC,d.deployment_id DESC LIMIT 2")
	rows := sqlmock.NewRows(unifiedDeploymentColumns()).AddRow(unifiedDeploymentRow(publicA, legacyA, updated)...).AddRow(unifiedDeploymentRow(publicC, legacyC, updated)...)
	mock.ExpectQuery(query).WithArgs(owner).WillReturnRows(rows)
	page1, token, err := source.ListDeployments(context.Background(), owner, agentcore.DeploymentListOptions{PageSize: 1})
	if err != nil || len(page1) != 1 || page1[0]["deployment_id"] != publicA {
		t.Fatalf("page 1 = %#v token=%q err=%v", page1, token, err)
	}
	cursor, err := decodeDeploymentPageToken(token)
	if err != nil || cursor.DeploymentID != "" || cursor.WorkloadID != legacyA {
		t.Fatalf("v107 cursor compatibility = %#v, err=%v", cursor, err)
	}
	rows = sqlmock.NewRows(unifiedDeploymentColumns()).AddRow(unifiedDeploymentRow(publicC, legacyC, updated)...).AddRow(unifiedDeploymentRow(publicB, legacyB, updated)...)
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY d.updated_at DESC,d.deployment_id DESC LIMIT 3")).WithArgs(owner, updated, legacyA).WillReturnRows(rows)
	page2, _, err := source.ListDeployments(context.Background(), owner, agentcore.DeploymentListOptions{PageSize: 2, PageToken: token})
	if err != nil || len(page2) != 2 || page2[0]["deployment_id"] != publicC || page2[1]["deployment_id"] != publicB {
		t.Fatalf("page 2 = %#v err=%v", page2, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func unifiedDeploymentColumns() []string {
	return []string{"public_deployment_id", "provision_id", "workload_id", "state", "target_kind", "revision", "object_json", "actual_json", "provision_state", "output_digest", "updated_at", "workload_revision", "plan_id", "plan_digest", "workload_state", "workload_actual", "operation_id", "operation", "plan_revision", "operation_plan_digest", "task_id", "confirmation_id", "operation_status", "operation_revision", "failure_code", "operation_created_at", "operation_updated_at", "dispatch_epoch", "dispatch_lease_until", "legacy_deployment_id"}
}

func unifiedDeploymentRow(publicID, legacyID string, updated time.Time) []driver.Value {
	return []driver.Value{publicID, "", "", "pending", "AWS_EC2", int64(1), []byte(`{}`), []byte(`{}`), "", "", updated, int64(0), "", "", "", []byte(`{}`), "", "", int64(0), "", "", "", "", int64(0), "", updated, updated, int64(0), nil, legacyID}
}
