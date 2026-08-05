package storage

import (
	"context"
	"testing"
)

func TestMemoryAgentEventCursorIsMonotonicAndResettable(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.SaveAgentEventCursor(ctx, "source", 10); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentEventCursor(ctx, "source", 9); err != nil {
		t.Fatal(err)
	}
	if cursor, err := store.LoadAgentEventCursor(ctx, "source"); err != nil || cursor != 10 {
		t.Fatalf("cursor=%d error=%v", cursor, err)
	}
	store.ResetAccountState()
	if cursor, err := store.LoadAgentEventCursor(ctx, "source"); err != nil || cursor != 0 {
		t.Fatalf("reset cursor=%d error=%v", cursor, err)
	}
}
