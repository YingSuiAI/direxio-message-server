package artifactstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

const (
	sourceOwner    = "@source-owner:example.test"
	sourceArtifact = "11111111-1111-4111-8111-111111111111"
	sourceProject  = "22222222-2222-4222-8222-222222222222"
	sourceIdem     = "33333333-3333-4333-8333-333333333333"
)

type sourceMetadataStub struct {
	registration SourceArtifactRegistration
	record       SourceArtifact
	err          error
}

func (s *sourceMetadataStub) RegisterSourceArtifact(_ context.Context, in SourceArtifactRegistration) (SourceArtifact, error) {
	s.registration = in
	if s.err != nil {
		return SourceArtifact{}, s.err
	}
	out := s.record
	if out.ArtifactID == "" {
		out = SourceArtifact{
			OwnerID: in.OwnerID, ArtifactID: in.ArtifactID, ProjectID: in.ProjectID,
			ContentDigest: in.ContentDigest, StorageBackend: "filesystem", StorageRef: in.StorageRef,
			SizeBytes: in.SizeBytes, MediaType: in.MediaType, Revision: 1, Status: "available",
			SchemaVersion: "execution-source-artifact/v2", Metadata: json.RawMessage(`{"purpose":"source"}`),
			CreatedAt: time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	return out, nil
}

func (s *sourceMetadataStub) GetSourceArtifact(_ context.Context, ownerID, projectID, artifactID string) (SourceArtifact, error) {
	if s.err != nil {
		return SourceArtifact{}, s.err
	}
	if s.record.OwnerID != ownerID || s.record.ProjectID != projectID || s.record.ArtifactID != artifactID {
		return SourceArtifact{}, ErrInvalidObject
	}
	return s.record, nil
}

func TestSourceCatalogComputesContentIdentityBeforeRegistration(t *testing.T) {
	content := newTestStore(t)
	metadata := &sourceMetadataStub{}
	catalog := NewSourceCatalog(content, metadata)
	payload := "immutable source archive"

	got, err := catalog.PutSource(context.Background(), strings.NewReader(payload), SourcePutOptions{
		OwnerID: sourceOwner, ArtifactID: sourceArtifact, ProjectID: sourceProject,
		IdempotencyID: sourceIdem, MediaType: "application/zip",
		Metadata: map[string]any{"purpose": "source"}, MaxSize: testArtifactMaxSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.registration.ContentDigest != digestOf([]byte(payload)) ||
		metadata.registration.SizeBytes != int64(len(payload)) ||
		metadata.registration.StorageRef != "sha256/"+metadata.registration.ContentDigest[:2]+"/"+metadata.registration.ContentDigest {
		t.Fatalf("registration did not use computed CAS facts: %+v", metadata.registration)
	}
	if got.ContentDigest != metadata.registration.ContentDigest || got.ArtifactID != sourceArtifact {
		t.Fatalf("source artifact = %+v", got)
	}
	if _, err = content.Stat(context.Background(), got.ContentDigest); err != nil {
		t.Fatalf("source object was not published: %v", err)
	}
}

func TestSourceCatalogFailsClosedOnMetadataDriftAndInvalidIdentity(t *testing.T) {
	content := newTestStore(t)
	metadata := &sourceMetadataStub{record: SourceArtifact{
		OwnerID: sourceOwner, ArtifactID: sourceArtifact, ProjectID: sourceProject,
		ContentDigest: strings.Repeat("a", 64), StorageBackend: "filesystem",
		StorageRef: "sha256/aa/" + strings.Repeat("a", 64), SizeBytes: 1,
		MediaType: "application/zip", Revision: 1, Status: "available",
		SchemaVersion: "execution-source-artifact/v2", Metadata: json.RawMessage(`{}`),
		CreatedAt: time.Now().UTC(),
	}}
	catalog := NewSourceCatalog(content, metadata)
	opts := SourcePutOptions{OwnerID: sourceOwner, ArtifactID: sourceArtifact, ProjectID: sourceProject, IdempotencyID: sourceIdem, MediaType: "application/zip", MaxSize: testArtifactMaxSize}
	if _, err := catalog.PutSource(context.Background(), strings.NewReader("different"), opts); !errors.Is(err, ErrSourceMetadataMismatch) {
		t.Fatalf("metadata drift error = %v", err)
	}
	opts.ArtifactID = "not-a-uuid"
	if _, err := catalog.PutSource(context.Background(), strings.NewReader("unused"), opts); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("invalid identity error = %v", err)
	}
	for _, mediaType := range []string{"", "*/*", "application/*", "Application/zip", "application/zip; charset=utf-8", "application /zip", "application/zip\ntext/plain"} {
		if ValidMediaType(mediaType) {
			t.Fatalf("non-canonical media type accepted: %q", mediaType)
		}
	}
}

func TestSourceCatalogOpensOnlyVerifiedContentAddress(t *testing.T) {
	content := newTestStore(t)
	payload := "archive bytes"
	stored, err := content.Put(context.Background(), strings.NewReader(payload), PutOptions{MaxSize: testArtifactMaxSize})
	if err != nil {
		t.Fatal(err)
	}
	record := SourceArtifact{
		OwnerID: sourceOwner, ArtifactID: sourceArtifact, ProjectID: sourceProject,
		ContentDigest: stored.Digest, StorageBackend: "filesystem", StorageRef: stored.StorageRef,
		SizeBytes: stored.Size, MediaType: "application/zip", Revision: 1, Status: "available",
		SchemaVersion: "execution-source-artifact/v2", Metadata: json.RawMessage(`{}`),
		CreatedAt: time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	metadata := &sourceMetadataStub{record: record}
	catalog := NewSourceCatalog(content, metadata)
	reader, got, err := catalog.OpenSource(context.Background(), sourceOwner, sourceProject, sourceArtifact)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	bytes, err := io.ReadAll(reader)
	if err != nil || string(bytes) != payload || got.ContentDigest != stored.Digest {
		t.Fatalf("open source bytes=%q metadata=%+v err=%v", bytes, got, err)
	}
	metadata.record.StorageRef = "sha256/aa/" + strings.Repeat("a", 64)
	if reader, _, err = catalog.OpenSource(context.Background(), sourceOwner, sourceProject, sourceArtifact); !errors.Is(err, ErrSourceMetadataMismatch) || reader != nil {
		t.Fatalf("drifted source open reader=%v err=%v", reader, err)
	}
}
