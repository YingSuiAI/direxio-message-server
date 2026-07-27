package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
)

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
