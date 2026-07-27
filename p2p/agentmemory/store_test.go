package agentmemory

import (
	"context"
	"testing"
)

func TestInMemorySemanticReplayStaleAndOwnerIsolation(t *testing.T) {
	s := NewInMemoryStore()
	calls := 0
	profile := KnowledgeEmbeddingSession{ProfileID: "p1", Revision: 1, Model: "m", Embed: func(context.Context, string) ([]float32, error) { calls++; return []float32{1, 0}, nil }}
	open := func(context.Context) (KnowledgeEmbeddingSession, error) { return profile, nil }
	digest := KnowledgeDigest("a", "alpha", nil)
	if _, replay, indexed, _, err := s.CreateKnowledgeMemorySemantic(context.Background(), "owner", "a", "alpha", nil, "id", digest, open); err != nil || replay || !indexed {
		t.Fatalf("create replay=%v indexed=%v err=%v", replay, indexed, err)
	}
	firstCalls := calls
	if _, replay, indexed, _, err := s.CreateKnowledgeMemorySemantic(context.Background(), "owner", "a", "alpha", nil, "id", digest, open); err != nil || !replay || !indexed || calls != firstCalls {
		t.Fatalf("replay calls=%d first=%d err=%v", calls, firstCalls, err)
	}
	profile.Revision = 2
	if _, _, indexed, _, err := s.CreateKnowledgeMemorySemantic(context.Background(), "owner", "a", "alpha", nil, "id", digest, func(context.Context) (KnowledgeEmbeddingSession, error) { return profile, nil }); err != nil || !indexed {
		t.Fatal(err)
	}
	if calls != firstCalls+1 {
		t.Fatalf("stale replay calls=%d", calls)
	}
	otherDigest := KnowledgeDigest("b", "beta", nil)
	profile.Revision = 2
	if _, _, _, _, err := s.CreateKnowledgeMemorySemantic(context.Background(), "other", "b", "beta", nil, "other-id", otherDigest, func(context.Context) (KnowledgeEmbeddingSession, error) { return profile, nil }); err != nil {
		t.Fatal(err)
	}
	items, _, _, err := s.SearchKnowledgeMemorySemantic(context.Background(), "owner", "q", 10, "", func(context.Context) (KnowledgeEmbeddingSession, error) {
		return KnowledgeEmbeddingSession{ProfileID: "p1", Revision: 2, Model: "m", Embed: func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }}, nil
	})
	if err != nil || len(items) != 1 || items[0].ID != "id" {
		t.Fatalf("items=%v err=%v", items, err)
	}
}

func TestInMemorySemanticIndexFenceRejectsProfileRotation(t *testing.T) {
	s := NewInMemoryStore()
	calls := 0
	open := func(context.Context) (KnowledgeEmbeddingSession, error) {
		calls++
		return KnowledgeEmbeddingSession{ProfileID: "p1", Revision: 1, Model: "m", Embed: func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }, ValidateCurrent: func(context.Context) error { return ErrEmbeddingSessionStale }}, nil
	}
	_, _, indexed, _, err := s.CreateKnowledgeMemorySemantic(context.Background(), "owner", "t", "body", nil, "id", KnowledgeDigest("t", "body", nil), open)
	if err != ErrEmbeddingSessionStale || indexed {
		t.Fatalf("indexed=%v err=%v", indexed, err)
	}
}

func TestInMemorySemanticGenerationFenceInterleaving(t *testing.T) {
	s := NewInMemoryStore()
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	p1 := func(context.Context) (KnowledgeEmbeddingSession, error) {
		return KnowledgeEmbeddingSession{ProfileID: "p1", Revision: 1, Model: "m", Embed: func(context.Context, string) ([]float32, error) {
			close(started)
			<-release
			return []float32{1, 0}, nil
		}}, nil
	}
	go func() {
		_, _, _, _, err := s.CreateKnowledgeMemorySemantic(context.Background(), "owner", "t", "body", nil, "id", KnowledgeDigest("t", "body", nil), p1)
		result <- err
	}()
	<-started
	p2 := func(context.Context) (KnowledgeEmbeddingSession, error) {
		return KnowledgeEmbeddingSession{ProfileID: "p2", Revision: 1, Model: "m", Embed: func(context.Context, string) ([]float32, error) { return []float32{0, 1}, nil }}, nil
	}
	if _, _, indexed, _, err := s.CreateKnowledgeMemorySemantic(context.Background(), "owner", "t", "body", nil, "id", KnowledgeDigest("t", "body", nil), p2); err != nil || !indexed {
		t.Fatalf("p2 indexed=%v err=%v", indexed, err)
	}
	close(release)
	if err := <-result; err != ErrEmbeddingSessionStale {
		t.Fatalf("late p1 err=%v", err)
	}
}
