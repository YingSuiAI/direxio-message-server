package nativeagent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"strings"
	"time"
)

func (r *Runtime) knowledgeStore(ctx context.Context) (KnowledgeStore, error) {
	if r.knowledge == nil {
		return nil, fmt.Errorf("knowledge memory store is not configured")
	}
	if r.effectiveOwner(ctx) == "" {
		return nil, fmt.Errorf("owner context is required")
	}
	return r.knowledge, nil
}
func (r *Runtime) createKnowledgeMemory(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"title": "string", "content": "string", "tags": "array", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"content", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	s, err := r.knowledgeStore(ctx)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(trimString(p["title"]))
	content := trimString(p["content"])
	if len([]rune(title)) > 256 || len([]rune(content)) > 65536 {
		return nil, validationErrorf("memory title or content is too long")
	}
	tags := stringSlice(p["tags"])
	if len(tags) > 16 {
		return nil, validationErrorf("too many tags")
	}
	idem := trimString(p["idempotency_key"])
	if idem == "" {
		return nil, validationErrorf("idempotency_key is required")
	}
	digest := agentmemory.KnowledgeDigest(title, content, tags)
	if semantic, ok := s.(agentmemory.SemanticKnowledgeStore); ok && r.embedding != nil {
		item, replayed, indexed, profile, err := semantic.CreateKnowledgeMemorySemantic(ctx, r.effectiveOwner(ctx), title, content, tags, idem, digest, r.embedding)
		if err != nil {
			if errors.Is(err, agentmemory.ErrNoEmbeddingProfile) {
				item, replayed, legacyErr := s.CreateKnowledgeMemory(ctx, r.effectiveOwner(ctx), title, content, tags, idem, digest)
				if legacyErr != nil {
					return nil, legacyErr
				}
				return map[string]any{"memory_id": item.ID, "title": item.Title, "content": item.Content, "tags": item.Tags, "created_at": item.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), "replayed": replayed, "embedding_indexed": false}, nil
			}
			return nil, err
		}
		result := map[string]any{"memory_id": item.ID, "title": item.Title, "content": item.Content, "tags": item.Tags, "created_at": item.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), "replayed": replayed, "embedding_indexed": indexed}
		if profile.ProfileID != "" {
			result["embedding_profile_id"], result["embedding_profile_revision"], result["embedding_model"] = profile.ProfileID, profile.Revision, profile.Model
		}
		return result, nil
	}
	item, replayed, err := s.CreateKnowledgeMemory(ctx, r.effectiveOwner(ctx), title, content, tags, idem, digest)
	if err != nil {
		return nil, err
	}
	return map[string]any{"memory_id": item.ID, "title": item.Title, "content": item.Content, "tags": item.Tags, "created_at": item.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), "replayed": replayed}, nil
}
func (r *Runtime) searchKnowledgeMemory(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"query": "string", "page_size": "integer", "page_token": "string"}); err != nil {
		return nil, err
	}
	s, err := r.knowledgeStore(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(int64Param(p["page_size"]))
	if limit <= 0 {
		limit = 20
	}
	query := trimString(p["query"])
	if semantic, ok := s.(agentmemory.SemanticKnowledgeStore); ok && strings.TrimSpace(query) != "" && r.embedding != nil {
		items, next, profile, err := semantic.SearchKnowledgeMemorySemantic(ctx, r.effectiveOwner(ctx), query, limit, trimString(p["page_token"]), r.embedding)
		if err != nil {
			if errors.Is(err, agentmemory.ErrNoEmbeddingProfile) {
				items, next, legacyErr := s.SearchKnowledgeMemory(ctx, r.effectiveOwner(ctx), query, limit, trimString(p["page_token"]))
				if legacyErr != nil {
					return nil, legacyErr
				}
				out := make([]map[string]any, 0, len(items))
				for _, i := range items {
					out = append(out, map[string]any{"memory_id": i.ID, "title": i.Title, "content": i.Content, "tags": i.Tags, "created_at": i.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")})
				}
				return map[string]any{"items": out, "next_cursor": next, "search_mode": "text"}, nil
			}
			return nil, err
		}
		out := make([]map[string]any, 0, len(items))
		for _, i := range items {
			out = append(out, map[string]any{"memory_id": i.ID, "title": i.Title, "content": i.Content, "tags": i.Tags, "created_at": i.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")})
		}
		result := map[string]any{"items": out, "next_cursor": next, "search_mode": "semantic"}
		if profile.ProfileID != "" {
			result["embedding_profile_id"], result["embedding_profile_revision"], result["embedding_model"] = profile.ProfileID, profile.Revision, profile.Model
		}
		return result, nil
	}
	items, next, err := s.SearchKnowledgeMemory(ctx, r.effectiveOwner(ctx), query, limit, trimString(p["page_token"]))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	for _, i := range items {
		out = append(out, map[string]any{"memory_id": i.ID, "title": i.Title, "content": i.Content, "tags": i.Tags, "created_at": i.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")})
	}
	return map[string]any{"items": out, "next_cursor": next, "search_mode": "text"}, nil
}
func (r *Runtime) knowledgeStatus(ctx context.Context) (map[string]any, error) {
	s, err := r.knowledgeStore(ctx)
	if err != nil {
		return nil, err
	}
	if semantic, ok := s.(agentmemory.SemanticKnowledgeStore); ok && r.embedding != nil {
		total, indexed, stale, profile, err := semantic.KnowledgeStatusSemantic(ctx, r.effectiveOwner(ctx), r.embedding)
		if err != nil {
			if errors.Is(err, agentmemory.ErrNoEmbeddingProfile) {
				n, legacyErr := s.KnowledgeStatus(ctx, r.effectiveOwner(ctx))
				if legacyErr != nil {
					return nil, legacyErr
				}
				return map[string]any{"supported": true, "count": n, "embedding_indexed": 0, "embedding_stale": n}, nil
			}
			return nil, err
		}
		result := map[string]any{"supported": true, "count": total, "embedding_indexed": indexed, "embedding_stale": stale}
		if profile.ProfileID != "" {
			result["embedding_profile_id"], result["embedding_profile_revision"], result["embedding_model"] = profile.ProfileID, profile.Revision, profile.Model
		}
		return result, nil
	}
	n, err := s.KnowledgeStatus(ctx, r.effectiveOwner(ctx))
	if err != nil {
		return nil, err
	}
	return map[string]any{"supported": true, "count": n, "embedding_indexed": 0, "embedding_stale": n}, nil
}

func knowledgeMemoryMap(i agentmemory.KnowledgeMemory) map[string]any {
	out := map[string]any{"memory_id": i.ID, "title": i.Title, "content": i.Content, "tags": i.Tags, "revision": i.Revision, "created_at": i.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": i.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	return out
}

func (r *Runtime) listKnowledgeMemories(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"page_size": "integer", "page_token": "string"}); err != nil {
		return nil, err
	}
	s, err := r.knowledgeStore(ctx)
	if err != nil {
		return nil, err
	}
	managed, ok := s.(agentmemory.ManagedKnowledgeStore)
	if !ok {
		return nil, fmt.Errorf("knowledge memory management is not supported")
	}
	n := int(int64Param(p["page_size"]))
	if n <= 0 {
		n = 20
	}
	if n > 100 {
		return nil, validationErrorf("page_size is out of range")
	}
	items, next, err := managed.ListKnowledgeMemories(ctx, r.effectiveOwner(ctx), n, trimString(p["page_token"]))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	for _, i := range items {
		out = append(out, knowledgeMemoryMap(i))
	}
	return map[string]any{"items": out, "next_page_token": next}, nil
}

func (r *Runtime) updateKnowledgeMemory(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"memory_id": "string", "title": "string", "content": "string", "tags": "array", "expected_revision": "integer", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"memory_id", "content", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	if int64Param(p["expected_revision"]) <= 0 {
		return nil, validationErrorf("expected_revision must be positive")
	}
	s, err := r.knowledgeStore(ctx)
	if err != nil {
		return nil, err
	}
	managed, ok := s.(agentmemory.ManagedKnowledgeStore)
	if !ok {
		return nil, fmt.Errorf("knowledge memory management is not supported")
	}
	if r.embedding == nil {
		return nil, fmt.Errorf("default embedding profile is required for memory update")
	}
	title := strings.TrimSpace(trimString(p["title"]))
	content := trimString(p["content"])
	tags := stringSlice(p["tags"])
	if len([]rune(title)) > 256 || len([]rune(content)) > 65536 || len(tags) > 16 {
		return nil, validationErrorf("memory fields are too long")
	}
	idem := trimString(p["idempotency_key"])
	digest := managedMemoryDigest("update", trimString(p["memory_id"]), int64Param(p["expected_revision"]), title, content, tags)
	item, replayed, err := managed.UpdateKnowledgeMemory(ctx, r.effectiveOwner(ctx), trimString(p["memory_id"]), title, content, tags, int64Param(p["expected_revision"]), idem, digest, r.embedding)
	if err != nil {
		return nil, err
	}
	out := knowledgeMemoryMap(item)
	out["replayed"] = replayed
	return out, nil
}

func (r *Runtime) deleteKnowledgeMemory(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"memory_id": "string", "expected_revision": "integer", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"memory_id", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	if int64Param(p["expected_revision"]) <= 0 {
		return nil, validationErrorf("expected_revision must be positive")
	}
	s, err := r.knowledgeStore(ctx)
	if err != nil {
		return nil, err
	}
	managed, ok := s.(agentmemory.ManagedKnowledgeStore)
	if !ok {
		return nil, fmt.Errorf("knowledge memory management is not supported")
	}
	digest := managedMemoryDigest("delete", trimString(p["memory_id"]), int64Param(p["expected_revision"]), "", "", nil)
	item, replayed, err := managed.DeleteKnowledgeMemory(ctx, r.effectiveOwner(ctx), trimString(p["memory_id"]), int64Param(p["expected_revision"]), trimString(p["idempotency_key"]), digest)
	if err != nil {
		return nil, err
	}
	out := knowledgeMemoryMap(item)
	out["replayed"] = replayed
	return out, nil
}

func managedMemoryDigest(action, id string, revision int64, title, content string, tags []string) [32]byte {
	h := sha256.New()
	for _, value := range []string{action, id, fmt.Sprintf("%d", revision), title, content} {
		h.Write([]byte(value))
		h.Write([]byte{0})
	}
	for _, tag := range tags {
		h.Write([]byte(tag))
		h.Write([]byte{0})
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func (r *Runtime) knowledgeSourceStore(ctx context.Context) (agentmemory.KnowledgeSourceStore, error) {
	if r.sources == nil {
		return nil, fmt.Errorf("knowledge source store is not configured")
	}
	if r.effectiveOwner(ctx) == "" {
		return nil, fmt.Errorf("owner context is required")
	}
	return r.sources, nil
}
func sourceMap(s agentmemory.KnowledgeSource) map[string]any {
	return map[string]any{"source_id": s.ID, "kind": s.Kind, "status": s.Status, "title": s.Title, "mime_type": s.MimeType, "size": s.Size, "total_chunks": s.TotalChunks, "indexed_chunks": s.IndexedChunks, "revision": s.Revision, "created_at": s.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": s.UpdatedAt.UTC().Format(time.RFC3339Nano), "error": s.Error}
}
func uploadProgress(u agentmemory.KnowledgeUpload) float64 {
	if u.Size <= 0 {
		return 0
	}
	p := float64(u.Received) / float64(u.Size)
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}
func (r *Runtime) listKnowledgeSources(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"page_size": "integer", "page_token": "string"}); err != nil {
		return nil, err
	}
	s, err := r.knowledgeSourceStore(ctx)
	if err != nil {
		return nil, err
	}
	n := int(int64Param(p["page_size"]))
	if n <= 0 {
		n = 20
	}
	if n > 100 {
		return nil, validationErrorf("page_size is out of range")
	}
	items, next, err := s.ListKnowledgeSources(ctx, r.effectiveOwner(ctx), n, trimString(p["page_token"]))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	for _, i := range items {
		out = append(out, sourceMap(i))
	}
	return map[string]any{"sources": out, "next_page_token": next}, nil
}
func (r *Runtime) deleteKnowledgeSource(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"source_id": "string", "expected_revision": "integer", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"source_id", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	if int64Param(p["expected_revision"]) <= 0 {
		return nil, validationErrorf("expected_revision must be positive")
	}
	s, err := r.knowledgeSourceStore(ctx)
	if err != nil {
		return nil, err
	}
	d := managedMemoryDigest("source.delete", trimString(p["source_id"]), int64Param(p["expected_revision"]), "", "", nil)
	src, replayed, err := s.DeleteKnowledgeSource(ctx, r.effectiveOwner(ctx), trimString(p["source_id"]), int64Param(p["expected_revision"]), trimString(p["idempotency_key"]), d)
	if err != nil {
		return nil, err
	}
	return map[string]any{"source": sourceMap(src), "replayed": replayed}, nil
}
func (r *Runtime) startKnowledgeUpload(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"filename": "string", "mime_type": "string", "size": "integer", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"filename", "mime_type", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	mime := strings.ToLower(strings.TrimSpace(trimString(p["mime_type"])))
	switch mime {
	case "text/plain", "text/markdown", "text/csv", "application/json":
	default:
		return nil, validationErrorf("unsupported mime_type")
	}
	size := int64Param(p["size"])
	if size < 0 || size > 10*1024*1024 {
		return nil, validationErrorf("size must be between 0 and 10 MiB")
	}
	s, err := r.knowledgeSourceStore(ctx)
	if err != nil {
		return nil, err
	}
	u, replayed, err := s.StartKnowledgeUpload(ctx, r.effectiveOwner(ctx), trimString(p["filename"]), mime, size, trimString(p["idempotency_key"]))
	if err != nil {
		return nil, err
	}
	return map[string]any{"upload_id": u.ID, "source_id": u.SourceID, "status": u.Status, "size": u.Size, "received_size": u.Received, "max_chunk_bytes": 256 * 1024, "progress": uploadProgress(u), "replayed": replayed}, nil
}
func (r *Runtime) chunkKnowledgeUpload(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"upload_id": "string", "offset": "integer", "data": "string", "chunk_sha256": "string", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"upload_id", "data", "chunk_sha256", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	raw, err := base64.StdEncoding.DecodeString(trimString(p["data"]))
	if err != nil || base64.StdEncoding.EncodeToString(raw) != trimString(p["data"]) {
		return nil, validationErrorf("data must be canonical base64")
	}
	if len(raw) > 256*1024 {
		return nil, validationErrorf("chunk exceeds 256 KiB")
	}
	sum := sha256.Sum256(raw)
	if fmt.Sprintf("%x", sum) != strings.ToLower(trimString(p["chunk_sha256"])) {
		return nil, validationErrorf("chunk_sha256 mismatch")
	}
	s, err := r.knowledgeSourceStore(ctx)
	if err != nil {
		return nil, err
	}
	u, err := s.AppendKnowledgeUpload(ctx, r.effectiveOwner(ctx), trimString(p["upload_id"]), int64Param(p["offset"]), raw, trimString(p["idempotency_key"]), sum)
	if err != nil {
		return nil, err
	}
	return map[string]any{"upload_id": u.ID, "source_id": u.SourceID, "status": u.Status, "size": u.Size, "received_size": u.Received, "max_chunk_bytes": 256 * 1024, "progress": uploadProgress(u)}, nil
}
func (r *Runtime) finishKnowledgeUpload(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"upload_id": "string", "title": "string", "content_sha256": "string", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"upload_id", "content_sha256", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	sumRaw, err := hex.DecodeString(trimString(p["content_sha256"]))
	if err != nil || len(sumRaw) != sha256.Size {
		return nil, validationErrorf("content_sha256 must be hex SHA-256")
	}
	var sum [32]byte
	copy(sum[:], sumRaw)
	s, err := r.knowledgeSourceStore(ctx)
	if err != nil {
		return nil, err
	}
	src, err := s.FinishKnowledgeUpload(ctx, r.effectiveOwner(ctx), trimString(p["upload_id"]), trimString(p["title"]), sum, trimString(p["idempotency_key"]), r.embedding)
	if err != nil {
		return nil, err
	}
	return map[string]any{"source": sourceMap(src)}, nil
}
func stringSlice(v any) []string {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		if s := strings.TrimSpace(fmt.Sprint(x)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
