package artifactstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrSourceMetadataMismatch = errors.New("artifactstore: source metadata mismatch")

// SourceArtifact is the immutable, owner/project-scoped catalog identity for
// a source archive. Content bytes remain in Store and are addressed only by
// ContentDigest/StorageRef; absolute filesystem paths are never persisted or
// exposed through this type.
type SourceArtifact struct {
	OwnerID        string          `json:"owner_id"`
	ArtifactID     string          `json:"artifact_id"`
	ProjectID      string          `json:"project_id"`
	ContentDigest  string          `json:"content_digest"`
	StorageBackend string          `json:"storage_backend"`
	StorageRef     string          `json:"storage_ref"`
	SizeBytes      int64           `json:"size_bytes"`
	MediaType      string          `json:"media_type"`
	Revision       uint64          `json:"revision"`
	Status         string          `json:"status"`
	SchemaVersion  string          `json:"schema_version"`
	Metadata       json.RawMessage `json:"metadata"`
	CreatedAt      time.Time       `json:"created_at"`
}

// SourceArtifactRegistration contains only metadata computed or normalized by
// the control plane. In particular, ContentDigest, SizeBytes and StorageRef
// come from Store.Put rather than from a client-provided authoritative value.
type SourceArtifactRegistration struct {
	OwnerID       string
	ArtifactID    string
	ProjectID     string
	IdempotencyID string
	ContentDigest string
	StorageRef    string
	SizeBytes     int64
	MediaType     string
	Metadata      any
}

// SourceArtifactMetadataStore is the durable half of SourceCatalog. Its
// implementation must provide exact idempotent replay and immutable rows.
type SourceArtifactMetadataStore interface {
	RegisterSourceArtifact(context.Context, SourceArtifactRegistration) (SourceArtifact, error)
	GetSourceArtifact(context.Context, string, string, string) (SourceArtifact, error)
}

// SourcePutOptions describe one logical upload. ArtifactID and IdempotencyID
// are caller-generated UUIDs, while the content identity is always computed
// by the content-addressed Store.
type SourcePutOptions struct {
	OwnerID       string
	ArtifactID    string
	ProjectID     string
	IdempotencyID string
	MediaType     string
	Metadata      any
	MaxSize       int64
}

// SourceCatalog joins the atomic filesystem CAS with durable source metadata.
// A database failure may leave an unreferenced CAS object, which is safe: the
// object is immutable and is not readable through an owner catalog until the
// metadata transaction succeeds.
type SourceCatalog struct {
	content  *Store
	metadata SourceArtifactMetadataStore
}

func NewSourceCatalog(content *Store, metadata SourceArtifactMetadataStore) *SourceCatalog {
	if content == nil || metadata == nil {
		return nil
	}
	return &SourceCatalog{content: content, metadata: metadata}
}

// PutSource computes and atomically publishes the content object before
// registering its owner/project identity. Retrying the same UUID
// idempotency key with the same bytes and metadata returns the exact original
// SourceArtifact; changing any request fact conflicts in the metadata store.
func (c *SourceCatalog) PutSource(ctx context.Context, r io.Reader, opts SourcePutOptions) (SourceArtifact, error) {
	if c == nil || c.content == nil || c.metadata == nil || r == nil ||
		strings.TrimSpace(opts.OwnerID) == "" || !validNonNilUUID(opts.ArtifactID) ||
		!validNonNilUUID(opts.ProjectID) || !validNonNilUUID(opts.IdempotencyID) ||
		!ValidMediaType(opts.MediaType) {
		return SourceArtifact{}, ErrInvalidObject
	}
	stored, err := c.content.Put(ctx, r, PutOptions{MaxSize: opts.MaxSize})
	if err != nil {
		return SourceArtifact{}, err
	}
	registered, err := c.metadata.RegisterSourceArtifact(ctx, SourceArtifactRegistration{
		OwnerID:       strings.TrimSpace(opts.OwnerID),
		ArtifactID:    opts.ArtifactID,
		ProjectID:     opts.ProjectID,
		IdempotencyID: opts.IdempotencyID,
		ContentDigest: stored.Digest,
		StorageRef:    stored.StorageRef,
		SizeBytes:     stored.Size,
		MediaType:     opts.MediaType,
		Metadata:      opts.Metadata,
	})
	if err != nil {
		return SourceArtifact{}, err
	}
	if registered.OwnerID != strings.TrimSpace(opts.OwnerID) || registered.ArtifactID != opts.ArtifactID ||
		registered.ProjectID != opts.ProjectID || registered.ContentDigest != stored.Digest ||
		registered.StorageBackend != "filesystem" || registered.StorageRef != stored.StorageRef ||
		registered.SizeBytes != stored.Size || registered.MediaType != opts.MediaType ||
		registered.Revision != 1 || registered.Status != "available" ||
		registered.SchemaVersion != "execution-source-artifact/v2" {
		return SourceArtifact{}, ErrSourceMetadataMismatch
	}
	return registered, nil
}

// OpenSource resolves an exact owner/project/artifact identity and opens only
// its verified content address. It never accepts a path or URI, making it the
// safe input boundary for a future bounded static archive analyzer.
func (c *SourceCatalog) OpenSource(ctx context.Context, ownerID, projectID, artifactID string) (io.ReadCloser, SourceArtifact, error) {
	ownerID = strings.TrimSpace(ownerID)
	if c == nil || c.content == nil || c.metadata == nil || ownerID == "" ||
		!validNonNilUUID(projectID) || !validNonNilUUID(artifactID) {
		return nil, SourceArtifact{}, ErrInvalidObject
	}
	source, err := c.metadata.GetSourceArtifact(ctx, ownerID, projectID, artifactID)
	if err != nil {
		return nil, SourceArtifact{}, err
	}
	if !validSourceArtifactIdentity(source, ownerID, projectID, artifactID) {
		return nil, SourceArtifact{}, ErrSourceMetadataMismatch
	}
	reader, opened, err := c.content.Open(ctx, source.ContentDigest)
	if err != nil {
		return nil, SourceArtifact{}, err
	}
	if opened.Digest != source.ContentDigest || opened.StorageRef != source.StorageRef || opened.Size != source.SizeBytes {
		_ = reader.Close()
		return nil, SourceArtifact{}, ErrSourceMetadataMismatch
	}
	return reader, source, nil
}

// ValidMediaType accepts only a canonical, parameter-free MIME media type.
// Media types are durable binding facts, so equivalent alternate spellings
// are rejected rather than normalized after confirmation.
func ValidMediaType(value string) bool {
	if value == "" || len(value) > 255 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	base, params, err := mime.ParseMediaType(value)
	parts := strings.Split(base, "/")
	return err == nil && len(params) == 0 && base == value && len(parts) == 2 &&
		parts[0] != "" && parts[1] != "" && parts[0] != "*" && parts[1] != "*"
}

func validNonNilUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validSourceArtifactIdentity(source SourceArtifact, ownerID, projectID, artifactID string) bool {
	return source.OwnerID == ownerID && source.ProjectID == projectID && source.ArtifactID == artifactID &&
		validateDigest(source.ContentDigest) == nil && source.StorageBackend == "filesystem" &&
		source.StorageRef == objectRef(source.ContentDigest) && source.SizeBytes > 0 &&
		source.SizeBytes <= MaxArtifactSize && ValidMediaType(source.MediaType) && source.Revision == 1 &&
		source.Status == "available" && source.SchemaVersion == "execution-source-artifact/v2" &&
		!source.CreatedAt.IsZero()
}
