package agentmemory

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
)

func testEmbedding(vector []float32, err error) KnowledgeEmbeddingSessionFunc {
	return func(context.Context) (KnowledgeEmbeddingSession, error) {
		return KnowledgeEmbeddingSession{ProfileID: "profile", Revision: 1, Model: "model", Embed: func(context.Context, string) ([]float32, error) { return vector, err }}, nil
	}
}

func TestKnowledgeSourceUploadReplayConflictAndOwnerBoundary(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	u, replay, err := s.StartKnowledgeUpload(ctx, "owner-a", "a.txt", "text/plain", 3, "start")
	if err != nil || replay {
		t.Fatalf("start replay=%v err=%v", replay, err)
	}
	if again, replay, err := s.StartKnowledgeUpload(ctx, "owner-a", "a.txt", "text/plain", 3, "start"); err != nil || !replay || again.ID != u.ID {
		t.Fatalf("start replay=%+v replay=%v err=%v", again, replay, err)
	}
	if _, _, err := s.StartKnowledgeUpload(ctx, "owner-a", "changed.txt", "text/plain", 3, "start"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed start err=%v", err)
	}
	data := []byte("abc")
	sum := sha256.Sum256(data)
	if _, err := s.AppendKnowledgeUpload(ctx, "owner-b", u.ID, 0, data, "append", sum); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner append err=%v", err)
	}
	if _, err := s.AppendKnowledgeUpload(ctx, "owner-a", u.ID, 1, data, "append", sum); err == nil {
		t.Fatal("offset mismatch accepted")
	}
	bad := sha256.Sum256([]byte("different"))
	if _, err := s.AppendKnowledgeUpload(ctx, "owner-a", u.ID, 0, data, "append", bad); err == nil {
		t.Fatal("bad chunk digest accepted")
	}
	if _, err := s.AppendKnowledgeUpload(ctx, "owner-a", u.ID, 0, data, "append", sum); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendKnowledgeUpload(ctx, "owner-a", u.ID, 0, data, "append", sum); err != nil {
		t.Fatalf("append replay: %v", err)
	}
}

func TestKnowledgeSourceFinishAtomicSuccessAndDeleteReplay(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	data := []byte("first paragraph\n\nsecond paragraph")
	u, _, err := s.StartKnowledgeUpload(ctx, "owner-a", "a.txt", "text/plain", int64(len(data)), "start")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if _, err := s.AppendKnowledgeUpload(ctx, "owner-a", u.ID, 0, data, "append", sum); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishKnowledgeUpload(ctx, "owner-a", u.ID, "doc", sum, "finish", testEmbedding(nil, errors.New("embed down"))); err == nil {
		t.Fatal("embedding failure accepted")
	}
	if sources, _, err := s.ListKnowledgeSources(ctx, "owner-a", 10, ""); err != nil || len(sources) != 0 {
		t.Fatalf("failure left sources=%v err=%v", sources, err)
	}
	if items, _, err := s.SearchKnowledgeMemory(ctx, "owner-a", "", 10, ""); err != nil || len(items) != 0 {
		t.Fatalf("failure left chunks=%v err=%v", items, err)
	}
	src, err := s.FinishKnowledgeUpload(ctx, "owner-a", u.ID, "doc", sum, "finish", testEmbedding([]float32{1, 0}, nil))
	if err != nil || src.TotalChunks != 2 {
		t.Fatalf("finish src=%+v err=%v", src, err)
	}
	if sources, _, err := s.ListKnowledgeSources(ctx, "owner-a", 10, ""); err != nil || len(sources) != 1 || sources[0].ID != src.ID {
		t.Fatalf("sources=%v err=%v", sources, err)
	}
	if items, _, err := s.SearchKnowledgeMemory(ctx, "owner-a", "paragraph", 10, ""); err != nil || len(items) != 2 {
		t.Fatalf("indexed chunks=%v err=%v", items, err)
	}
	d := sha256.Sum256([]byte("delete"))
	if _, replay, err := s.DeleteKnowledgeSource(ctx, "owner-a", src.ID, src.Revision, "delete", d); err != nil || replay {
		t.Fatalf("delete replay=%v err=%v", replay, err)
	}
	if _, replay, err := s.DeleteKnowledgeSource(ctx, "owner-a", src.ID, src.Revision, "delete", d); err != nil || !replay {
		t.Fatalf("delete replay=%v err=%v", replay, err)
	}
	if items, _, err := s.SearchKnowledgeMemory(ctx, "owner-a", "", 10, ""); err != nil || len(items) != 0 {
		t.Fatalf("delete left chunks=%v err=%v", items, err)
	}
}

func TestManagedKnowledgeExcludesSourceChunksAndCursorAdvances(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	data := []byte("document")
	u, _, err := s.StartKnowledgeUpload(ctx, "owner", "doc.txt", "text/plain", int64(len(data)), "start")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if _, err := s.AppendKnowledgeUpload(ctx, "owner", u.ID, 0, data, "append", sum); err != nil {
		t.Fatal(err)
	}
	src, err := s.FinishKnowledgeUpload(ctx, "owner", u.ID, "doc", sum, "finish", testEmbedding([]float32{1}, nil))
	if err != nil {
		t.Fatal(err)
	}
	managed, _, err := s.CreateKnowledgeMemory(ctx, "owner", "managed", "note", nil, "managed", KnowledgeDigest("managed", "note", nil))
	if err != nil {
		t.Fatal(err)
	}
	items, token, err := s.ListKnowledgeMemories(ctx, "owner", 1, "")
	if err != nil || len(items) != 1 || items[0].ID != managed.ID || token != "" {
		t.Fatalf("items=%+v token=%q err=%v", items, token, err)
	}
	digest := KnowledgeDigest("x", "y", nil)
	if _, _, err := s.UpdateKnowledgeMemory(ctx, "owner", src.ID+"-0", "x", "y", nil, 1, "update-source", digest, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source update err=%v", err)
	}
	if _, _, err := s.DeleteKnowledgeMemory(ctx, "owner", src.ID+"-0", 1, "delete-source", digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source delete err=%v", err)
	}
}

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
