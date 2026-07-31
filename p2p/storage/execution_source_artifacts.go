package storage

// Durable source-archive catalog for execution/v2. Source archives exist
// before an analysis or plan, so they deliberately do not share the
// plan-scoped core_execution_artifacts table used for executor inputs and
// outputs.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

// RegisterSourceArtifact persists a server-computed content address under one
// owner/project identity. The row and replay response are immutable. Reusing
// the idempotency key with any different request fact conflicts.
func (s *DatabaseExecutionStore) RegisterSourceArtifact(ctx context.Context, in artifactstore.SourceArtifactRegistration) (artifactstore.SourceArtifact, error) {
	owner := strings.TrimSpace(in.OwnerID)
	if s == nil || s.db == nil || owner == "" ||
		!validUUID(in.ArtifactID) || !validUUID(in.ProjectID) ||
		!coreexecution.ValidateDigest(in.ContentDigest) || in.SizeBytes <= 0 ||
		in.SizeBytes > artifactstore.MaxArtifactSize ||
		!validSourceStorageRef(in.StorageRef, in.ContentDigest) ||
		!artifactstore.ValidMediaType(in.MediaType) {
		return artifactstore.SourceArtifact{}, ErrExecutionStoreInvalid
	}
	idem, err := parseCatalogIdempotency(in.IdempotencyID)
	if err != nil {
		return artifactstore.SourceArtifact{}, err
	}
	metadataValue := in.Metadata
	if metadataValue == nil {
		metadataValue = map[string]any{}
	}
	metadata, err := canonicalRedactedJSON(metadataValue)
	if err != nil || !sourceMetadataObject(metadata) {
		return artifactstore.SourceArtifact{}, ErrExecutionStoreInvalid
	}
	request, err := json.Marshal(struct {
		SchemaVersion  string          `json:"schema_version"`
		OwnerID        string          `json:"owner_id"`
		ArtifactID     string          `json:"artifact_id"`
		ProjectID      string          `json:"project_id"`
		ContentDigest  string          `json:"content_digest"`
		StorageBackend string          `json:"storage_backend"`
		StorageRef     string          `json:"storage_ref"`
		SizeBytes      int64           `json:"size_bytes"`
		MediaType      string          `json:"media_type"`
		Metadata       json.RawMessage `json:"metadata"`
	}{
		SchemaVersion: "execution-source-artifact-register/v2", OwnerID: owner,
		ArtifactID: in.ArtifactID, ProjectID: in.ProjectID, ContentDigest: in.ContentDigest,
		StorageBackend: "filesystem", StorageRef: in.StorageRef, SizeBytes: in.SizeBytes,
		MediaType: in.MediaType, Metadata: metadata,
	})
	if err != nil {
		return artifactstore.SourceArtifact{}, ErrExecutionStoreInvalid
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return artifactstore.SourceArtifact{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, canonicalAdvisoryLockIdentity("execution-source-artifact", owner, in.ArtifactID)); err != nil {
		return artifactstore.SourceArtifact{}, err
	}
	var projectStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM core_execution_projects WHERE owner_id=$1 AND project_id=$2 FOR KEY SHARE`, owner, in.ProjectID).Scan(&projectStatus); errors.Is(err, sql.ErrNoRows) {
		return artifactstore.SourceArtifact{}, coreexecution.ErrNotFound
	} else if err != nil {
		return artifactstore.SourceArtifact{}, err
	} else if projectStatus != "active" {
		return artifactstore.SourceArtifact{}, coreexecution.ErrConflict
	}

	record, loadErr := getSourceArtifactTx(ctx, tx, s.db, owner, in.ProjectID, in.ArtifactID)
	if loadErr != nil && !errors.Is(loadErr, coreexecution.ErrNotFound) {
		return artifactstore.SourceArtifact{}, loadErr
	}
	if errors.Is(loadErr, coreexecution.ErrNotFound) {
		record = artifactstore.SourceArtifact{
			OwnerID: owner, ArtifactID: in.ArtifactID, ProjectID: in.ProjectID,
			ContentDigest: in.ContentDigest, StorageBackend: "filesystem", StorageRef: in.StorageRef,
			SizeBytes: in.SizeBytes, MediaType: in.MediaType, Revision: 1, Status: "available",
			SchemaVersion: "execution-source-artifact/v2", Metadata: metadata,
			CreatedAt: s.now().UTC().Truncate(time.Microsecond),
		}
	}
	if !sourceArtifactMatchesRegistration(record, in, metadata) {
		return artifactstore.SourceArtifact{}, coreexecution.ErrConflict
	}
	response, err := json.Marshal(record)
	if err != nil {
		return artifactstore.SourceArtifact{}, ErrExecutionStoreInvalid
	}
	insertedReplay, oldResponse, err := catalogIdempotency(ctx, tx, owner, idem, request, response, record.CreatedAt)
	if err != nil {
		return artifactstore.SourceArtifact{}, err
	}
	if !insertedReplay {
		replay, decodeErr := decodeSourceArtifact(oldResponse)
		if decodeErr != nil || !sourceArtifactMatchesRegistration(replay, in, metadata) {
			return artifactstore.SourceArtifact{}, ErrExecutionStoreDrift
		}
		if err = tx.Commit(); err != nil {
			return artifactstore.SourceArtifact{}, err
		}
		return replay, nil
	}
	if loadErr == nil {
		if err = tx.Commit(); err != nil {
			return artifactstore.SourceArtifact{}, err
		}
		return record, nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_source_artifacts(owner_id,artifact_id,project_id,content_digest,storage_backend,storage_ref,size_bytes,media_type,revision,status,schema_version,metadata_json,created_at) VALUES($1,$2,$3,$4,'filesystem',$5,$6,$7,1,'available','execution-source-artifact/v2',$8,$9)`, record.OwnerID, record.ArtifactID, record.ProjectID, record.ContentDigest, record.StorageRef, record.SizeBytes, record.MediaType, record.Metadata, record.CreatedAt); err != nil {
		return artifactstore.SourceArtifact{}, mapExecutionConflict(err)
	}
	stored, err := getSourceArtifactTx(ctx, tx, s.db, record.OwnerID, record.ProjectID, record.ArtifactID)
	if err != nil || !sourceArtifactEqual(stored, record) {
		return artifactstore.SourceArtifact{}, ErrExecutionStoreDrift
	}
	if err = tx.Commit(); err != nil {
		return artifactstore.SourceArtifact{}, err
	}
	return stored, nil
}

// GetSourceArtifact requires the complete owner/project/artifact identity, so
// an artifact UUID learned in one project cannot be probed or reused in
// another.
func (s *DatabaseExecutionStore) GetSourceArtifact(ctx context.Context, owner, projectID, artifactID string) (artifactstore.SourceArtifact, error) {
	owner = strings.TrimSpace(owner)
	if s == nil || s.db == nil || owner == "" || !validUUID(projectID) || !validUUID(artifactID) {
		return artifactstore.SourceArtifact{}, ErrExecutionStoreInvalid
	}
	return getSourceArtifactTx(ctx, nil, s.db, owner, projectID, artifactID)
}

func getSourceArtifactTx(ctx context.Context, tx *sql.Tx, db *sql.DB, owner, projectID, artifactID string) (artifactstore.SourceArtifact, error) {
	var out artifactstore.SourceArtifact
	err := queryRow(ctx, tx, db, `SELECT owner_id,artifact_id::text,project_id::text,content_digest,storage_backend,storage_ref,size_bytes,media_type,revision,status,schema_version,metadata_json,created_at FROM core_execution_source_artifacts WHERE owner_id=$1 AND project_id=$2 AND artifact_id=$3`, owner, projectID, artifactID).Scan(
		&out.OwnerID, &out.ArtifactID, &out.ProjectID, &out.ContentDigest, &out.StorageBackend,
		&out.StorageRef, &out.SizeBytes, &out.MediaType, &out.Revision, &out.Status,
		&out.SchemaVersion, &out.Metadata, &out.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return artifactstore.SourceArtifact{}, coreexecution.ErrNotFound
	}
	if err != nil {
		return artifactstore.SourceArtifact{}, err
	}
	out.CreatedAt = out.CreatedAt.UTC()
	if err = validateStoredSourceArtifact(out); err != nil {
		return artifactstore.SourceArtifact{}, err
	}
	return out, nil
}

func decodeSourceArtifact(raw []byte) (artifactstore.SourceArtifact, error) {
	var out artifactstore.SourceArtifact
	if strictJSON(raw, &out) != nil || validateStoredSourceArtifact(out) != nil {
		return artifactstore.SourceArtifact{}, ErrExecutionStoreDrift
	}
	return out, nil
}

func validateStoredSourceArtifact(out artifactstore.SourceArtifact) error {
	if strings.TrimSpace(out.OwnerID) == "" || !validUUID(out.ArtifactID) || !validUUID(out.ProjectID) ||
		!coreexecution.ValidateDigest(out.ContentDigest) || out.StorageBackend != "filesystem" ||
		!validSourceStorageRef(out.StorageRef, out.ContentDigest) || out.SizeBytes <= 0 ||
		out.SizeBytes > artifactstore.MaxArtifactSize || !artifactstore.ValidMediaType(out.MediaType) ||
		out.Revision != 1 || out.Status != "available" ||
		out.SchemaVersion != "execution-source-artifact/v2" || out.CreatedAt.IsZero() ||
		!sourceMetadataObject(out.Metadata) {
		return ErrExecutionStoreDrift
	}
	var metadata any
	if strictJSON(out.Metadata, &metadata) != nil || validateCatalogSensitiveData(metadata) != nil {
		return ErrExecutionStoreDrift
	}
	return nil
}

func sourceArtifactMatchesRegistration(out artifactstore.SourceArtifact, in artifactstore.SourceArtifactRegistration, metadata []byte) bool {
	return validateStoredSourceArtifact(out) == nil && out.OwnerID == strings.TrimSpace(in.OwnerID) &&
		out.ArtifactID == in.ArtifactID && out.ProjectID == in.ProjectID &&
		out.ContentDigest == in.ContentDigest && out.StorageBackend == "filesystem" &&
		out.StorageRef == in.StorageRef && out.SizeBytes == in.SizeBytes &&
		out.MediaType == in.MediaType && jsonEqual(out.Metadata, metadata)
}

func sourceArtifactEqual(a, b artifactstore.SourceArtifact) bool {
	return a.OwnerID == b.OwnerID && a.ArtifactID == b.ArtifactID && a.ProjectID == b.ProjectID &&
		a.ContentDigest == b.ContentDigest && a.StorageBackend == b.StorageBackend &&
		a.StorageRef == b.StorageRef && a.SizeBytes == b.SizeBytes && a.MediaType == b.MediaType &&
		a.Revision == b.Revision && a.Status == b.Status && a.SchemaVersion == b.SchemaVersion &&
		a.CreatedAt.Equal(b.CreatedAt) && jsonEqual(a.Metadata, b.Metadata)
}

func sourceMetadataObject(raw []byte) bool {
	var value map[string]any
	return len(raw) > 0 && strictJSON(raw, &value) == nil && value != nil
}

func validSourceStorageRef(ref, digest string) bool {
	return coreexecution.ValidateDigest(digest) && ref == "sha256/"+digest[:2]+"/"+digest
}

var _ artifactstore.SourceArtifactMetadataStore = (*DatabaseExecutionStore)(nil)
