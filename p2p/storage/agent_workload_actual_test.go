package storage

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
)

func TestPostgresGetWorkloadNormalizesReadbackPayload(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner = "@actual:example.test"
	const workloadID = "11111111-1111-4111-8111-111111111111"
	const planID = "22222222-2222-4222-8222-222222222222"
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	repo, err := NewAgentWorkloadStore(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), owner)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text,revision,plan_id::text,plan_digest,target_kind,state,actual_snapshot_json,updated_at FROM core_workloads WHERE owner_id=$1 AND workload_id=$2")).
		WithArgs(owner, workloadID).
		WillReturnRows(sqlmock.NewRows([]string{"workload_id", "revision", "plan_id", "plan_digest", "target_kind", "state", "actual_snapshot_json", "updated_at"}).
			AddRow(workloadID, int64(2), planID, digest, "AWS_EC2_SSM", "ready", []byte(`{"target_kind":"AWS_EC2_SSM","workload_id":"11111111-1111-4111-8111-111111111111","state":"ready","identity":{"kind":"AWS_EC2_SSM","account_id":"123456789012","region":"us-east-1","instance_id":"i-0123456789abcdef0"},"provider_version":"aws-ssm-v1","digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","at":"2026-07-30T23:59:00Z"}`), now))
	got, err := repo.GetWorkload(t.Context(), workloadID)
	if err != nil {
		t.Fatalf("GetWorkload: %v", err)
	}
	if got.Actual.Revision != 2 || got.Actual.AppliedPlanID != planID || got.Actual.AppliedPlanDigest != digest || got.Actual.ReadbackDigest == "" || got.Actual.UpdatedAt != now {
		t.Fatalf("normalized durable actual = %+v", got.Actual)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizePersistedWorkloadActualReadbackShape(t *testing.T) {
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	parent := workload.Workload{
		ID: "11111111-1111-4111-8111-111111111111", Revision: 2,
		PlanID: "22222222-2222-4222-8222-222222222222", PlanDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetKind: workload.TargetAWSEC2SSM, State: "ready", UpdatedAt: now,
	}
	raw := []byte(`{"target_kind":"AWS_EC2_SSM","workload_id":"11111111-1111-4111-8111-111111111111","state":"ready","identity":{"kind":"AWS_EC2_SSM","account_id":"123456789012","region":"us-east-1","instance_id":"i-0123456789abcdef0"},"provider_version":"aws-ssm-v1","digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","at":"2026-07-30T23:59:00Z"}`)
	actual, err := normalizePersistedWorkloadActual(raw, parent)
	if err != nil {
		t.Fatalf("normalize readback: %v", err)
	}
	if actual.WorkloadID != parent.ID || actual.Revision != parent.Revision || actual.State != parent.State || actual.AppliedPlanID != parent.PlanID || actual.AppliedPlanDigest != parent.PlanDigest || actual.ReadbackDigest != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || actual.UpdatedAt != now {
		t.Fatalf("normalized actual = %+v", actual)
	}
	if actual.ObservedAt.IsZero() || actual.ProviderVersion != "aws-ssm-v1" {
		t.Fatalf("readback fields were not retained: %+v", actual)
	}
}

func TestNormalizePersistedWorkloadActualLegacyTargetKindMustMatchParent(t *testing.T) {
	parent := workload.Workload{ID: "11111111-1111-4111-8111-111111111111", Revision: 2, PlanID: "22222222-2222-4222-8222-222222222222", PlanDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetKind: workload.TargetAWSEC2SSM, State: "ready", UpdatedAt: time.Now().UTC()}
	base := `{"target_kind":%q,"workload_id":"11111111-1111-4111-8111-111111111111","state":"ready","identity":{"kind":"AWS_EC2_SSM","account_id":"123456789012","region":"us-east-1","instance_id":"i-0123456789abcdef0"},"provider_version":"aws-ssm-v1","digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","at":"2026-07-30T23:59:00Z"}`
	for _, targetKind := range []string{"AWS_ECS", "", "unknown"} {
		raw := []byte(fmt.Sprintf(base, targetKind))
		actual, err := normalizePersistedWorkloadActual(raw, parent)
		if err != nil {
			t.Fatalf("normalize target kind %q: %v", targetKind, err)
		}
		if actual.Revision != 0 || actual.AppliedPlanID != "" || actual.AppliedPlanDigest != "" || !actual.UpdatedAt.IsZero() {
			t.Fatalf("target kind %q inherited row metadata: %+v", targetKind, actual)
		}
	}
}

func TestNormalizePersistedWorkloadActualEmptyRemainsZero(t *testing.T) {
	parent := workload.Workload{ID: "11111111-1111-4111-8111-111111111111", Revision: 1, PlanID: "22222222-2222-4222-8222-222222222222", PlanDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: "pending", UpdatedAt: time.Now().UTC()}
	for _, raw := range [][]byte{nil, []byte(`{}`), []byte(`null`)} {
		actual, err := normalizePersistedWorkloadActual(raw, parent)
		if err != nil {
			t.Fatalf("normalize empty %q: %v", raw, err)
		}
		if actual != (workload.ActualSnapshot{}) {
			t.Fatalf("empty payload %q normalized to %+v, want zero", raw, actual)
		}
	}
}

func TestNormalizePersistedWorkloadActualPreservesExplicitEmptyFields(t *testing.T) {
	parent := workload.Workload{ID: "11111111-1111-4111-8111-111111111111", Revision: 2, PlanID: "22222222-2222-4222-8222-222222222222", PlanDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: "ready", UpdatedAt: time.Now().UTC()}
	actual, err := normalizePersistedWorkloadActual([]byte(`{"workload_id":"","state":"ready","revision":0,"identity":{},"provider_version":"","readback_digest":"","observed_at":null,"applied_plan_id":"","applied_plan_digest":"","updated_at":null}`), parent)
	if err != nil {
		t.Fatalf("normalize explicit corruption: %v", err)
	}
	if actual.WorkloadID != "" || actual.Revision != 0 || actual.ProviderVersion != "" || actual.AppliedPlanID != "" || actual.AppliedPlanDigest != "" || !actual.ObservedAt.IsZero() || !actual.UpdatedAt.IsZero() {
		t.Fatalf("explicit corrupt fields were backfilled: %+v", actual)
	}
}

func TestNormalizePersistedWorkloadActualCurrentShapeMissingMetadataIsNotInherited(t *testing.T) {
	parent := workload.Workload{ID: "11111111-1111-4111-8111-111111111111", Revision: 2, PlanID: "22222222-2222-4222-8222-222222222222", PlanDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: "ready", UpdatedAt: time.Now().UTC()}
	actual, err := normalizePersistedWorkloadActual([]byte(`{"workload_id":"11111111-1111-4111-8111-111111111111","state":"ready","identity":{"kind":"AWS_EC2_SSM","account_id":"123456789012","region":"us-east-1","instance_id":"i-0123456789abcdef0"},"provider_version":"aws-ssm-v1","readback_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","observed_at":"2026-07-30T23:59:00Z"}`), parent)
	if err != nil {
		t.Fatalf("normalize current omission: %v", err)
	}
	if actual.Revision != 0 || actual.AppliedPlanID != "" || actual.AppliedPlanDigest != "" || !actual.UpdatedAt.IsZero() {
		t.Fatalf("current shape inherited row-only metadata: %+v", actual)
	}
}

func TestNormalizePersistedWorkloadActualHybridDoesNotOverwriteExplicitFields(t *testing.T) {
	parent := workload.Workload{ID: "11111111-1111-4111-8111-111111111111", Revision: 2, PlanID: "22222222-2222-4222-8222-222222222222", PlanDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: "ready", UpdatedAt: time.Now().UTC()}
	actual, err := normalizePersistedWorkloadActual([]byte(`{"target_kind":"AWS_EC2_SSM","workload_id":"11111111-1111-4111-8111-111111111111","state":"ready","identity":{"kind":"AWS_EC2_SSM","account_id":"123456789012","region":"us-east-1","instance_id":"i-0123456789abcdef0"},"provider_version":"aws-ssm-v1","digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","at":"2026-07-30T23:59:00Z","readback_digest":""}`), parent)
	if err != nil {
		t.Fatalf("normalize hybrid corruption: %v", err)
	}
	if actual.ReadbackDigest != "" || actual.Revision != 0 || actual.AppliedPlanID != "" || actual.AppliedPlanDigest != "" || !actual.UpdatedAt.IsZero() {
		t.Fatalf("hybrid payload was classified as legacy: %+v", actual)
	}
}
