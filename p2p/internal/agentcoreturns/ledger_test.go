package agentcoreturns

import (
	"context"
	"errors"
	"testing"
)

func TestLedgerBindsOwnerClientAndDigest(t *testing.T) {
	l := New(nil)
	d := Digest(map[string]any{"message": "secret body"})
	r := Record{OwnerID: "owner-a", ClientTurnID: "turn-1", RequestDigest: d, ConversationID: "conv"}
	got, replay, err := l.Reserve(context.Background(), r)
	if err != nil || replay || got.ClientTurnID != "turn-1" {
		t.Fatalf("reserve = %#v replay=%v err=%v", got, replay, err)
	}
	if _, replay, err = l.Reserve(context.Background(), r); err != nil || !replay {
		t.Fatalf("same request should replay: replay=%v err=%v", replay, err)
	}
	r.RequestDigest = Digest(map[string]any{"message": "other"})
	if _, _, err = l.Reserve(context.Background(), r); !errors.Is(err, ErrConflict) {
		t.Fatalf("digest mismatch err=%v", err)
	}
	if _, err = l.Get(context.Background(), "owner-b", "turn-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner isolation err=%v", err)
	}
}

func TestLedgerMemoryCASPersistsNonterminalProjectionAndRejectsStaleRewrite(t *testing.T) {
	l := New(nil)
	if _, _, err := l.Reserve(context.Background(), Record{OwnerID: "owner", ClientTurnID: "turn", RequestDigest: Digest("turn")}); err != nil {
		t.Fatal(err)
	}
	if err := l.Update(context.Background(), "owner", "turn", "core-1", "conversation-1", "running", 4, 9, 7, "delta", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Update(context.Background(), "owner", "turn", "stale-core", "stale-conversation", "failed", 3, 10, 8, "stale", "late", "late"); err != nil {
		t.Fatal(err)
	}
	record, err := l.Get(context.Background(), "owner", "turn")
	if err != nil {
		t.Fatal(err)
	}
	if record.CoreTurnID != "core-1" || record.ConversationID != "conversation-1" || record.Status != "running" || record.LastSequence != 4 || record.CoreRevision != 9 || record.ModelProfileRevision != 7 || record.LastEventKind != "delta" {
		t.Fatalf("stale update rewrote projection: %#v", record)
	}
	if err := l.Update(context.Background(), "owner", "turn", "core-1", "conversation-1", "completed", 5, 10, 7, "completed", "done", "finished"); err != nil {
		t.Fatal(err)
	}
	if err := l.Update(context.Background(), "owner", "turn", "core-1b", "conversation-1b", "done", 6, 11, 8, "completed", "done", "finished richer"); err != nil {
		t.Fatal(err)
	}
	if err := l.Update(context.Background(), "owner", "turn", "late-core", "late-conversation", "running", 6, 11, 8, "late", "late", "late"); err != nil {
		t.Fatal(err)
	}
	record, err = l.Get(context.Background(), "owner", "turn")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "completed" || record.CoreTurnID != "core-1" || record.ConversationID != "conversation-1" || record.LastSequence != 6 || record.CoreRevision != 11 || record.TerminalCode != "done" || record.TerminalSummary != "finished richer" {
		t.Fatalf("terminal projection rewritten: %#v", record)
	}
}
