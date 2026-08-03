package storage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func TestExecutionSourceArtifactsArePrePlanImmutableAndExactlyReplayable(t *testing.T) {
	ctx := context.Background()
	database := openExecutionV2Schema(t)
	now := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
	store := NewDatabaseExecutionStore(database.DB(), func() time.Time { return now })
	owner := "@source-catalog:example.test"
	projectID := "10000000-0000-4000-8000-000000000001"
	artifactID := "10000000-0000-4000-8000-000000000002"
	projectIdem := "10000000-0000-4000-8000-000000000003"
	artifactIdem := "10000000-0000-4000-8000-000000000004"
	if _, err := store.CreateProject(ctx, ProjectCreateRequest{OwnerID: owner, ProjectID: projectID, IdempotencyID: projectIdem}); err != nil {
		t.Fatal(err)
	}

	content, err := artifactstore.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	catalog := artifactstore.NewSourceCatalog(content, store)
	payload := "pinned uploaded source archive"
	request := artifactstore.SourcePutOptions{
		OwnerID: owner, ArtifactID: artifactID, ProjectID: projectID,
		IdempotencyID: artifactIdem, MediaType: "application/zip",
		Metadata: map[string]any{"filename": "source.zip", "purpose": "project-source"}, MaxSize: 1 << 20,
	}
	created, err := catalog.PutSource(ctx, strings.NewReader(payload), request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := catalog.PutSource(ctx, strings.NewReader(payload), request)
	if err != nil || !reflect.DeepEqual(replayed, created) {
		t.Fatalf("exact replay = %#v, err=%v; want %#v", replayed, err, created)
	}
	read, err := store.GetSourceArtifact(ctx, owner, projectID, artifactID)
	if err != nil || !reflect.DeepEqual(read, created) {
		t.Fatalf("readback = %#v, err=%v; want %#v", read, err, created)
	}

	drift := request
	drift.MediaType = "application/x-tar"
	if _, err = catalog.PutSource(ctx, strings.NewReader(payload), drift); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("same-key request drift error = %v", err)
	}
	drift = request
	drift.IdempotencyID = "10000000-0000-4000-8000-000000000005"
	if same, sameErr := catalog.PutSource(ctx, strings.NewReader(payload), drift); sameErr != nil || !reflect.DeepEqual(same, created) {
		t.Fatalf("same immutable identity with new key = %#v, err=%v", same, sameErr)
	}

	sensitive := artifactstore.SourceArtifactRegistration{
		OwnerID: owner, ArtifactID: "10000000-0000-4000-8000-000000000006", ProjectID: projectID,
		IdempotencyID: "10000000-0000-4000-8000-000000000007", ContentDigest: strings.Repeat("a", 64),
		StorageRef: "sha256/aa/" + strings.Repeat("a", 64), SizeBytes: 1, MediaType: "application/zip",
		Metadata: map[string]any{"access_token": "must-not-persist"},
	}
	if _, err = store.RegisterSourceArtifact(ctx, sensitive); !errors.Is(err, ErrExecutionStoreInvalid) {
		t.Fatalf("sensitive source metadata error = %v", err)
	}
	if _, err = database.DB().ExecContext(ctx, `UPDATE core_execution_source_artifacts SET metadata_json='{}'::jsonb WHERE owner_id=$1 AND artifact_id=$2`, owner, artifactID); err == nil {
		t.Fatal("source artifact metadata update unexpectedly succeeded")
	}
	if _, err = database.DB().ExecContext(ctx, `DELETE FROM core_execution_source_artifacts WHERE owner_id=$1 AND artifact_id=$2`, owner, artifactID); err == nil {
		t.Fatal("source artifact delete unexpectedly succeeded")
	}
	for name, statement := range map[string]string{
		"zero size":          `INSERT INTO core_execution_source_artifacts(owner_id,artifact_id,project_id,content_digest,storage_ref,size_bytes,media_type) VALUES($1,'10000000-0000-4000-8000-000000000009',$2,'` + strings.Repeat("b", 64) + `','sha256/bb/` + strings.Repeat("b", 64) + `',0,'application/zip')`,
		"noncanonical media": `INSERT INTO core_execution_source_artifacts(owner_id,artifact_id,project_id,content_digest,storage_ref,size_bytes,media_type) VALUES($1,'10000000-0000-4000-8000-000000000010',$2,'` + strings.Repeat("c", 64) + `','sha256/cc/` + strings.Repeat("c", 64) + `',1,'Application/zip')`,
		"storage mismatch":   `INSERT INTO core_execution_source_artifacts(owner_id,artifact_id,project_id,content_digest,storage_ref,size_bytes,media_type) VALUES($1,'10000000-0000-4000-8000-000000000011',$2,'` + strings.Repeat("d", 64) + `','sha256/ee/` + strings.Repeat("e", 64) + `',1,'application/zip')`,
	} {
		if _, directErr := database.DB().ExecContext(ctx, statement, owner, projectID); directErr == nil {
			t.Fatalf("database accepted %s source metadata", name)
		}
	}
	if _, err = store.GetSourceArtifact(ctx, owner, "10000000-0000-4000-8000-000000000008", artifactID); !errors.Is(err, coreexecution.ErrNotFound) {
		t.Fatalf("cross-project read error = %v", err)
	}
}
