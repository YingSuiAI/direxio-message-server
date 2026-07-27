package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"github.com/lib/pq"
)

func TestDatabaseSemanticMemoryVectors(t *testing.T) {
	ctx := context.Background()
	store := newConversationTestStore(t, ctx)
	profiles, err := NewDatabaseModelProfileStore(ctx, store, filepath.Join(t.TempDir(), "model.key"))
	if err != nil {
		t.Fatal(err)
	}
	{
		key := "k"
		if _, err = profiles.SyncModelProfilesWithDefaults(ctx, "owner", "profile-sync", ModelProfileDefaults{EmbeddingClientProfileID: "embedding"}, []ModelProfileSyncEntry{{ClientProfileID: "embedding", Provider: "openai", BaseURL: "https://example.com", Model: "m", ModelKind: ModelKindEmbedding, APIKey: &key}}); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := profiles.ResolveDefaultModelProfile(ctx, "owner", ModelKindEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	digest := agentmemory.KnowledgeDigest("alpha", "alpha", nil)
	open := func(context.Context) (agentmemory.KnowledgeEmbeddingSession, error) {
		return agentmemory.KnowledgeEmbeddingSession{ProfileID: profile.ProfileID, Revision: profile.Revision, Model: "m", Embed: func(_ context.Context, input string) ([]float32, error) {
			if strings.Contains(input, "alpha") || input == "query" {
				return []float32{1, 0}, nil
			}
			return []float32{0, 1}, nil
		}}, nil
	}
	if _, _, indexed, _, err := store.CreateKnowledgeMemorySemantic(ctx, "owner", "alpha", "alpha", nil, "a", digest, open); err != nil || !indexed {
		t.Fatalf("create err=%v indexed=%v", err, indexed)
	}
	if _, _, _, _, err := store.CreateKnowledgeMemorySemantic(ctx, "owner", "beta", "beta", nil, "b", agentmemory.KnowledgeDigest("beta", "beta", nil), open); err != nil {
		t.Fatal(err)
	}
	items, _, _, err := store.SearchKnowledgeMemorySemantic(ctx, "owner", "query", 10, "", open)
	if err != nil || len(items) != 2 || items[0].ID != "a" {
		t.Fatalf("ranking=%v err=%v", items, err)
	}
	other, _, _, err := store.SearchKnowledgeMemorySemantic(ctx, "other", "query", 10, "", open)
	if err != nil || len(other) != 0 {
		t.Fatalf("owner isolation=%v err=%v", other, err)
	}
	if _, _, _, _, err := store.CreateKnowledgeMemorySemantic(ctx, "owner", "alpha", "alpha", nil, "a", digest, open); err != nil {
		t.Fatal(err)
	}
	key2 := "k2"
	if _, err := profiles.SyncModelProfilesWithDefaults(ctx, "owner", "profile-sync-2", ModelProfileDefaults{EmbeddingClientProfileID: "embedding-v2"}, []ModelProfileSyncEntry{{ClientProfileID: "embedding-v2", Provider: "openai", BaseURL: "https://example.com", Model: "m", ModelKind: ModelKindEmbedding, APIKey: &key2}}); err != nil {
		t.Fatal(err)
	}
	profile2, err := profiles.ResolveDefaultModelProfile(ctx, "owner", ModelKindEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	open2 := func(context.Context) (agentmemory.KnowledgeEmbeddingSession, error) {
		return agentmemory.KnowledgeEmbeddingSession{ProfileID: profile2.ProfileID, Revision: profile2.Revision, Model: profile2.Model, Embed: func(context.Context, string) ([]float32, error) { return []float32{0, 1}, nil }}, nil
	}
	if _, _, indexed, _, err := store.CreateKnowledgeMemorySemantic(ctx, "owner", "alpha", "alpha", nil, "a", digest, open2); err != nil || !indexed {
		t.Fatalf("rotation err=%v indexed=%v", err, indexed)
	}
	var storedProfile string
	var storedVector []float64
	if err := store.DB().QueryRowContext(ctx, `SELECT profile_id,vector FROM p2p_native_agent_memory_embeddings WHERE owner_id='owner' AND memory_id='a'`).Scan(&storedProfile, pq.Array(&storedVector)); err != nil || storedProfile != profile2.ProfileID || len(storedVector) != 2 || storedVector[0] != 0 || storedVector[1] != 1 {
		t.Fatalf("stored profile=%q vector=%v err=%v", storedProfile, storedVector, err)
	}
	if _, _, indexed, _, err := store.CreateKnowledgeMemorySemantic(ctx, "owner", "alpha", "alpha", nil, "a", digest, open); err != agentmemory.ErrEmbeddingSessionStale || indexed {
		t.Fatalf("late stale err=%v indexed=%v", err, indexed)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT profile_id,vector FROM p2p_native_agent_memory_embeddings WHERE owner_id='owner' AND memory_id='a'`).Scan(&storedProfile, pq.Array(&storedVector)); err != nil || storedProfile != profile2.ProfileID || len(storedVector) != 2 || storedVector[0] != 0 || storedVector[1] != 1 {
		t.Fatalf("late write changed row profile=%q vector=%v err=%v", storedProfile, storedVector, err)
	}
	if _, _, err := store.CreateKnowledgeMemory(ctx, "owner", "missing", "missing", nil, "missing", agentmemory.KnowledgeDigest("missing", "missing", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO p2p_native_agent_memory_embeddings(owner_id,memory_id,profile_id,profile_revision,model,dimension,content_digest,vector,indexed_at) VALUES('owner','missing','p',1,'m',2,decode('0000000000000000000000000000000000000000000000000000000000000000','hex'),ARRAY[1.0],NOW())`); err == nil {
		t.Fatal("dimension constraint accepted malformed vector")
	}
}

func TestDatabaseAgentConversationCASAndPagination(t *testing.T) {
	ctx := context.Background()
	store := newConversationTestStore(t, ctx)
	owner := "owner"
	for _, id := range []string{"a", "b", "c"} {
		if _, _, err := store.CreateConversation(ctx, owner, id, id, "create-"+id, agentmemory.KnowledgeDigest(id, id, nil)); err != nil {
			t.Fatal(err)
		}
	}
	first, cursor, err := store.ListAgentConversations(ctx, owner, 2, "")
	if err != nil || len(first) != 2 || cursor == "" || first[0].ID != "c" || first[1].ID != "b" {
		t.Fatalf("first page = %#v, %q, %v", first, cursor, err)
	}
	second, next, err := store.ListAgentConversations(ctx, owner, 2, cursor)
	if err != nil || next != "" || len(second) != 1 || second[0].ID != "a" {
		t.Fatalf("second page = %#v, %q, %v", second, next, err)
	}
	if _, _, err := store.ListAgentConversations(ctx, owner, 2, "not-a-cursor"); !errors.Is(err, agentmemory.ErrInvalidCursor) {
		t.Fatalf("malformed conversation cursor = %v, want ErrInvalidCursor", err)
	}

	created, _, err := store.CreateConversation(ctx, owner, "messages", "messages", "messages-create", agentmemory.KnowledgeDigest("messages", "messages", nil))
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 5; n++ {
		if err := store.AppendConversationMessages(ctx, owner, "messages", "", []agentmemory.StoredMessage{{MessageID: fmt.Sprintf("m%d", n), Role: "user", Content: fmt.Sprintf("message %d", n)}}); err != nil {
			t.Fatal(err)
		}
	}
	_, messages, messageCursor, err := store.GetConversation(ctx, owner, "messages", 2, "")
	if err != nil || messageCursor != "4" || messageSeqs(messages) != "4,5" {
		t.Fatalf("first messages = %#v, %q, %v", messages, messageCursor, err)
	}
	_, messages, messageCursor, err = store.GetConversation(ctx, owner, "messages", 2, messageCursor)
	if err != nil || messageCursor != "2" || messageSeqs(messages) != "2,3" {
		t.Fatalf("second messages = %#v, %q, %v", messages, messageCursor, err)
	}
	for _, token := range []string{"0", "-1", "malformed"} {
		if _, _, _, err := store.GetConversation(ctx, owner, "messages", 2, token); !errors.Is(err, agentmemory.ErrInvalidCursor) {
			t.Fatalf("message cursor %q = %v, want ErrInvalidCursor", token, err)
		}
	}

	if _, _, err := store.RenameConversation(ctx, owner, "messages", "renamed", created.Revision+1, "rename-miss", agentmemory.KnowledgeDigest("renamed", "miss", nil)); !errors.Is(err, agentmemory.ErrRevisionConflict) {
		t.Fatalf("rename CAS miss = %v, want ErrRevisionConflict", err)
	}
	deleted, _, err := store.DeleteConversation(ctx, owner, "messages", created.Revision, "delete", agentmemory.KnowledgeDigest("messages", "delete", nil))
	if err != nil || !deleted.Deleted {
		t.Fatalf("delete = %#v, %v", deleted, err)
	}
	if _, _, err := store.DeleteConversation(ctx, owner, "messages", deleted.Revision, "delete-again", agentmemory.KnowledgeDigest("messages", "delete-again", nil)); !errors.Is(err, agentmemory.ErrDeleted) {
		t.Fatalf("re-delete = %v, want ErrDeleted", err)
	}
	if _, _, err := store.DeleteConversation(ctx, owner, "missing", 1, "delete-missing", agentmemory.KnowledgeDigest("missing", "delete", nil)); !errors.Is(err, agentmemory.ErrNotFound) {
		t.Fatalf("missing delete = %v, want ErrNotFound", err)
	}
}

func TestDatabaseAgentSummaryAndKnowledgeAtomicity(t *testing.T) {
	ctx := context.Background()
	store := newConversationTestStore(t, ctx)
	owner, conversation := "owner", "summary"
	if _, _, err := store.CreateConversation(ctx, owner, conversation, "summary", "summary-create", agentmemory.KnowledgeDigest("summary", "create", nil)); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		if err := store.AppendConversationMessages(ctx, owner, conversation, "", []agentmemory.StoredMessage{{MessageID: fmt.Sprintf("summary-%d", n), Role: "user", Content: "message"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveConversationSummary(ctx, owner, conversation, "new", 3); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConversationSummary(ctx, owner, conversation, "stale", 2); err != nil {
		t.Fatal(err)
	}
	var summary string
	var through int64
	if err := store.DB().QueryRowContext(ctx, `SELECT summary,summary_through_seq FROM p2p_native_agent_conversations WHERE owner_id=$1 AND conversation_id=$2`, owner, conversation).Scan(&summary, &through); err != nil || summary != "new" || through != 3 {
		t.Fatalf("summary after stale write = %q, %d, %v", summary, through, err)
	}

	digest := agentmemory.KnowledgeDigest("same", "content", []string{"tag"})
	start := make(chan struct{})
	type result struct {
		replay bool
		err    error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, replay, err := store.CreateKnowledgeMemory(ctx, owner, "same", "content", []string{"tag"}, "same-key", digest)
			results <- result{replay, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	created, replayed := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.replay {
			replayed++
		} else {
			created++
		}
	}
	if created != 1 || replayed != 1 {
		t.Fatalf("same-key concurrent results created=%d replayed=%d", created, replayed)
	}
	if _, replay, err := store.CreateKnowledgeMemory(ctx, owner, "same", "content", []string{"tag"}, "other-key", digest); err != nil || replay {
		t.Fatalf("same content with another key = replay=%v err=%v", replay, err)
	}
	if _, _, err := store.CreateKnowledgeMemory(ctx, owner, "third", "content", nil, "third-key", agentmemory.KnowledgeDigest("third", "content", nil)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateKnowledgeMemory(ctx, "other-owner", "other", "content", nil, "other-owner-key", agentmemory.KnowledgeDigest("other", "content", nil)); err != nil {
		t.Fatal(err)
	}
	first, cursor, err := store.SearchKnowledgeMemory(ctx, owner, "", 2, "")
	if err != nil || len(first) != 2 || cursor == "" {
		t.Fatalf("first knowledge page = %#v, %q, %v", first, cursor, err)
	}
	second, next, err := store.SearchKnowledgeMemory(ctx, owner, "", 2, cursor)
	if err != nil || next != "" || len(second) != 1 || first[0].ID == second[0].ID || first[1].ID == second[0].ID {
		t.Fatalf("second knowledge page = %#v, %q, %v", second, next, err)
	}
	if _, _, err := store.SearchKnowledgeMemory(ctx, owner, "", 2, "bad-cursor"); !errors.Is(err, agentmemory.ErrInvalidCursor) {
		t.Fatalf("malformed knowledge cursor = %v, want ErrInvalidCursor", err)
	}
	if _, _, err := store.CreateKnowledgeMemory(ctx, owner, "changed", "content", nil, "same-key", agentmemory.KnowledgeDigest("changed", "content", nil)); !errors.Is(err, agentmemory.ErrIdempotencyConflict) {
		t.Fatalf("same key different digest = %v, want ErrIdempotencyConflict", err)
	}
}

func messageSeqs(messages []agentmemory.StoredMessage) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, fmt.Sprintf("%d", message.Seq))
	}
	return strings.Join(parts, ",")
}
