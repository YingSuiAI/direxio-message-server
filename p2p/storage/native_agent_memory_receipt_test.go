package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
)

func TestDatabaseConversationMutationReceiptsReplayStoredResponse(t *testing.T) {
	ctx := context.Background()
	store := newConversationTestStore(t, ctx)
	owner, id := "owner", "conversation"
	createDigest := agentmemory.KnowledgeDigest(id, "first", nil)
	created, replayed, err := store.CreateConversation(ctx, owner, id, "first", "create-key", createDigest)
	if err != nil || replayed {
		t.Fatalf("CreateConversation = (%#v, %v, %v)", created, replayed, err)
	}
	renamed, replayed, err := store.RenameConversation(ctx, owner, id, "second", created.Revision, "rename-key", agentmemory.KnowledgeDigest(id, "second", nil))
	if err != nil || replayed {
		t.Fatalf("RenameConversation = (%#v, %v, %v)", renamed, replayed, err)
	}
	createdReplay, replayed, err := store.CreateConversation(ctx, owner, id, "first", "create-key", createDigest)
	if err != nil || !replayed || !equalConversationResponse(createdReplay, created) {
		t.Fatalf("create replay = (%#v, %v, %v), want original %#v", createdReplay, replayed, err, created)
	}
	deleted, replayed, err := store.DeleteConversation(ctx, owner, id, renamed.Revision, "delete-key", agentmemory.KnowledgeDigest(id, "delete", nil))
	if err != nil || replayed || !deleted.Deleted {
		t.Fatalf("DeleteConversation = (%#v, %v, %v)", deleted, replayed, err)
	}
	renamedReplay, replayed, err := store.RenameConversation(ctx, owner, id, "second", created.Revision, "rename-key", agentmemory.KnowledgeDigest(id, "second", nil))
	if err != nil || !replayed || !equalConversationResponse(renamedReplay, renamed) {
		t.Fatalf("rename replay = (%#v, %v, %v), want original %#v", renamedReplay, replayed, err, renamed)
	}
	deletedReplay, replayed, err := store.DeleteConversation(ctx, owner, id, renamed.Revision, "delete-key", agentmemory.KnowledgeDigest(id, "delete", nil))
	if err != nil || !replayed || !equalConversationResponse(deletedReplay, deleted) {
		t.Fatalf("delete replay = (%#v, %v, %v), want original %#v", deletedReplay, replayed, err, deleted)
	}
	_, _, err = store.DeleteConversation(ctx, owner, id, renamed.Revision, "delete-key", agentmemory.KnowledgeDigest(id, "other", nil))
	if !errors.Is(err, agentmemory.ErrIdempotencyConflict) {
		t.Fatalf("different digest error = %v, want ErrIdempotencyConflict", err)
	}

	var response []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT response_json FROM p2p_native_agent_conversation_mutations WHERE owner_id=$1 AND action='delete' AND idempotency_key='delete-key'`, owner).Scan(&response); err != nil {
		t.Fatal(err)
	}
	decoded, err := conversationMutationResponse(response)
	if err != nil || !equalConversationResponse(decoded, deleted) {
		t.Fatalf("stored response decode = (%#v, %v), want %#v", decoded, err, deleted)
	}
}

func equalConversationResponse(got, want agentmemory.Conversation) bool {
	got.CreatedAt, got.UpdatedAt = got.CreatedAt.UTC(), got.UpdatedAt.UTC()
	want.CreatedAt, want.UpdatedAt = want.CreatedAt.UTC(), want.UpdatedAt.UTC()
	return reflect.DeepEqual(got, want)
}

func TestDatabaseConversationMutationReceiptRejectsInvalidResponse(t *testing.T) {
	ctx := context.Background()
	store := newConversationTestStore(t, ctx)
	digest := agentmemory.KnowledgeDigest("conversation", "title", nil)
	if _, _, err := store.CreateConversation(ctx, "owner", "conversation", "title", "key", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE p2p_native_agent_conversation_mutations SET response_json='{}'::jsonb WHERE owner_id='owner' AND action='create' AND idempotency_key='key'`); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := store.CreateConversation(ctx, "owner", "conversation", "title", "key", digest); err == nil || !replayed {
		t.Fatalf("invalid receipt replay = (replayed=%v, err=%v), want decode error", replayed, err)
	}
}
