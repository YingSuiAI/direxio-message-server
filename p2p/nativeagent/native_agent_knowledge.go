package nativeagent

import (
	"context"
	"errors"
	"fmt"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"strings"
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
