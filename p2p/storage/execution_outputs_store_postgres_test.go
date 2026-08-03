package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func TestExecutionOutputsPostgresArtifactsEventsAndBindings(t *testing.T) {
	ctx := context.Background()
	store := openExecutionV2Schema(t)
	f := newExecutionV2GraphFixture(t, store.DB())
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	d := coreexecution.Digest(strings.Repeat("1", 64))
	artifactID := "00000000-0000-4000-8000-000000000311"
	if _, err := s.CreateArtifactMetadata(ctx, ExecutionArtifactCreate{OwnerID: f.Owner, ArtifactID: artifactID, ProjectID: f.ProjectID, PlanID: f.PlanID, PlanRevision: 1, ContentDigest: d, StorageRef: fmt.Sprintf("sha256/%s/%s", string(d)[:2], d), SizeBytes: 12, MediaType: "text/plain", Metadata: map[string]any{"secret_value": "must-not-persist"}}); !errors.Is(err, ErrExecutionStoreInvalid) {
		t.Fatalf("sensitive metadata err=%v", err)
	}
	meta, err := s.CreateArtifactMetadata(ctx, ExecutionArtifactCreate{OwnerID: f.Owner, ArtifactID: artifactID, ProjectID: f.ProjectID, PlanID: f.PlanID, PlanRevision: 1, ContentDigest: d, StorageRef: fmt.Sprintf("sha256/%s/%s", string(d)[:2], d), SizeBytes: 12, MediaType: "text/plain", Metadata: map[string]any{"purpose": "usage"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(meta.Metadata), "must-not-persist") {
		t.Fatalf("metadata leaked sensitive value: %s", meta.Metadata)
	}
	got, err := s.GetArtifactMetadata(ctx, f.Owner, artifactID)
	if err != nil || got.ContentDigest != d {
		t.Fatalf("artifact readback=%#v err=%v", got, err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_artifacts SET metadata_json='{"tampered":true}'::jsonb WHERE owner_id=$1 AND artifact_id=$2`, f.Owner, artifactID); err == nil {
		t.Fatal("artifact metadata tamper unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM core_execution_artifacts WHERE owner_id=$1 AND artifact_id=$2`, f.Owner, artifactID); err == nil {
		t.Fatal("artifact delete unexpectedly succeeded")
	}
	if _, err := s.CreateArtifactMetadata(ctx, ExecutionArtifactCreate{OwnerID: f.Owner, ArtifactID: artifactID, ProjectID: f.ProjectID, PlanID: f.PlanID, PlanRevision: 1, ContentDigest: coreexecution.Digest(strings.Repeat("2", 64)), StorageRef: "sha256/22/" + strings.Repeat("2", 64), SizeBytes: 12}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("artifact drift=%v", err)
	}

	e1, err := s.AppendExecutionEvent(ctx, ExecutionEventCreate{OwnerID: f.Owner, RunID: f.RunID, StageID: f.StageID, Kind: "stage.output", EventKey: "stage-output-1", Status: "recorded", Payload: map[string]any{"message": "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.AppendExecutionEvent(ctx, ExecutionEventCreate{OwnerID: f.Owner, RunID: f.RunID, StageID: f.StageID, Kind: "stage.output", EventKey: "stage-output-1", Status: "recorded", Payload: map[string]any{"message": "ok"}})
	if err != nil || replay.EventID != e1.EventID {
		t.Fatalf("event replay=%#v err=%v", replay, err)
	}
	if _, err := s.AppendExecutionEvent(ctx, ExecutionEventCreate{OwnerID: f.Owner, RunID: f.RunID, StageID: f.StageID, Kind: "stage.output", EventKey: "stage-output-1", Status: "failed", Payload: map[string]any{"message": "drift"}}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("event drift=%v", err)
	}
	if _, err := s.AppendExecutionEvent(ctx, ExecutionEventCreate{OwnerID: f.Owner, RunID: f.RunID, StageID: f.StageID, Kind: "stage.output", EventKey: "stage-output-2", Payload: map[string]any{"n": 2}}); err != nil {
		t.Fatal(err)
	}
	items, next, err := s.ListExecutionEvents(ctx, f.Owner, f.RunID, 0, 1)
	if err != nil || len(items) != 1 || next == 0 || items[0].Sequence != 1 {
		t.Fatalf("event page=%#v next=%d err=%v", items, next, err)
	}
	if page, _, err := s.ListExecutionEvents(ctx, f.Owner, f.RunID, next, 10); err != nil || len(page) != 1 || page[0].Sequence != 2 {
		t.Fatalf("event second page=%#v err=%v", page, err)
	}

	if _, err := s.AppendDeploymentEvent(ctx, DeploymentEventCreate{OwnerID: f.Owner, DeploymentID: "00000000-0000-4000-8000-000000000310", EventKey: "deploy-secret", Kind: "deployment.created", Payload: map[string]any{"password": "hidden"}}); !errors.Is(err, ErrExecutionStoreInvalid) {
		t.Fatalf("sensitive deployment payload err=%v", err)
	}
	de, err := s.AppendDeploymentEvent(ctx, DeploymentEventCreate{OwnerID: f.Owner, DeploymentID: "00000000-0000-4000-8000-000000000310", EventKey: "deploy-1", Kind: "deployment.created", Payload: map[string]any{"state": "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(de.Payload), "pending") {
		t.Fatalf("deployment payload missing state: %s", de.Payload)
	}
	if page, next, err := s.ListDeploymentEvents(ctx, f.Owner, "00000000-0000-4000-8000-000000000310", 0, 10); err != nil || len(page) != 1 || next != 0 {
		t.Fatalf("deployment events=%#v next=%d err=%v", page, next, err)
	}

	bindingID := "00000000-0000-4000-8000-000000000312"
	binding := coreexecution.ServiceBinding{BindingID: bindingID, OwnerID: f.Owner, DeploymentID: "00000000-0000-4000-8000-000000000310", ProjectID: f.ProjectID, RunID: f.RunID, TargetID: f.TargetID, TargetRevision: 1, TargetDigest: coreexecution.Digest(f.TargetDigest), Protocol: "https", Endpoint: "https://service.example.test", OperationSchemas: []coreexecution.OperationSchema{{Name: "health", Version: "1"}}, UsageArtifact: coreexecution.ArtifactRef{ID: artifactID, Digest: d, Immutable: true, Size: 12, MediaType: "text/plain"}}
	created, err := s.CreateServiceBinding(ctx, ServiceBindingCreate{OwnerID: f.Owner, Binding: binding, IdempotencyID: "00000000-0000-4000-8000-000000000313"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || !created.Digest.Valid() || created.OperationSchemas[0].Digest == "" {
		t.Fatalf("binding=%#v", created)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE core_execution_service_bindings SET revision=revision+1,endpoint='https://tampered.example.test' WHERE owner_id=$1 AND binding_id=$2`, f.Owner, bindingID); err == nil {
		t.Fatal("service binding snapshot tamper unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM core_execution_service_bindings WHERE owner_id=$1 AND binding_id=$2`, f.Owner, bindingID); err == nil {
		t.Fatal("service binding delete unexpectedly succeeded")
	}
	replayed, err := s.CreateServiceBinding(ctx, ServiceBindingCreate{OwnerID: f.Owner, Binding: binding, IdempotencyID: "00000000-0000-4000-8000-000000000313"})
	if err != nil || replayed.Digest != created.Digest {
		t.Fatalf("binding replay=%#v err=%v", replayed, err)
	}
	updatedInput := created
	updatedInput.Endpoint = "https://service-v2.example.test"
	updatedInput.Digest = ""
	updated, err := s.CreateServiceBinding(ctx, ServiceBindingCreate{OwnerID: f.Owner, Binding: updatedInput, ExpectedRevision: 1, IdempotencyID: "00000000-0000-4000-8000-000000000314"})
	if err != nil || updated.Revision != 2 || updated.Endpoint != "https://service-v2.example.test" {
		t.Fatalf("binding update=%#v err=%v", updated, err)
	}
	if _, err := s.CreateServiceBinding(ctx, ServiceBindingCreate{OwnerID: f.Owner, Binding: updatedInput, ExpectedRevision: 1, IdempotencyID: "00000000-0000-4000-8000-000000000315"}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("stale binding revision=%v", err)
	}
	read, err := s.GetServiceBinding(ctx, f.Owner, bindingID)
	if err != nil || read.Revision != 2 {
		t.Fatalf("binding get=%#v err=%v", read, err)
	}
	list, cursor, err := s.ListServiceBindings(ctx, f.Owner, f.ProjectID, "", 10)
	if err != nil || len(list) != 1 || cursor != "" {
		t.Fatalf("binding list=%#v cursor=%q err=%v", list, cursor, err)
	}
}
