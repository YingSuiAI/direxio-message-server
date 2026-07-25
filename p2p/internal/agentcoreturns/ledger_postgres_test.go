package agentcoreturns

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestLedgerReattachesAfterServerRebuildOnSameDatabase(t *testing.T) {
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE p2p_agent_core_turns (
		owner_id TEXT NOT NULL, client_turn_id TEXT NOT NULL, core_turn_id TEXT NOT NULL DEFAULT '', core_profile_id TEXT NOT NULL DEFAULT '', conversation_id TEXT NOT NULL DEFAULT '',
		request_digest BYTEA NOT NULL, status TEXT NOT NULL, last_sequence BIGINT NOT NULL DEFAULT 0, core_revision BIGINT NOT NULL DEFAULT 0, model_profile_revision BIGINT NOT NULL DEFAULT 0, last_event_kind TEXT NOT NULL DEFAULT '', terminal_code TEXT NOT NULL DEFAULT '', terminal_summary TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id, client_turn_id))`)
	if err != nil {
		t.Fatal(err)
	}
	first := New(db)
	digest := Digest(map[string]any{"message": "prompt", "api_key": "must-not-persist"})
	if _, replay, err := first.Reserve(context.Background(), Record{OwnerID: "owner-a", ClientTurnID: "turn", RequestDigest: digest, CoreTurnID: "core-turn", CoreProfileID: "core-profile", ConversationID: "conv"}); err != nil || replay {
		t.Fatalf("initial reserve replay=%v err=%v", replay, err)
	}
	if err := first.Update(context.Background(), "owner-a", "turn", "core-turn", "conv", "done", 3, 7, 2, "done", "", "complete"); err != nil {
		t.Fatal(err)
	}
	rebuilt := New(db)
	record, err := rebuilt.Get(context.Background(), "owner-a", "turn")
	if err != nil || record.CoreTurnID != "core-turn" || record.CoreProfileID != "core-profile" || record.Status != "done" || record.LastSequence != 3 || record.CoreRevision != 7 || record.ModelProfileRevision != 2 {
		t.Fatalf("rebuilt record=%#v err=%v", record, err)
	}
	if _, err := rebuilt.Get(context.Background(), "owner-b", "turn"); err != ErrNotFound {
		t.Fatalf("owner isolation err=%v", err)
	}
}

func TestLedgerPostgresConcurrentWatchersCannotRewriteTerminal(t *testing.T) {
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE p2p_agent_core_turns (owner_id TEXT NOT NULL, client_turn_id TEXT NOT NULL, core_turn_id TEXT NOT NULL DEFAULT '', core_profile_id TEXT NOT NULL DEFAULT '', conversation_id TEXT NOT NULL DEFAULT '', request_digest BYTEA NOT NULL, status TEXT NOT NULL, last_sequence BIGINT NOT NULL DEFAULT 0, core_revision BIGINT NOT NULL DEFAULT 0, model_profile_revision BIGINT NOT NULL DEFAULT 0, last_event_kind TEXT NOT NULL DEFAULT '', terminal_code TEXT NOT NULL DEFAULT '', terminal_summary TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id, client_turn_id))`)
	if err != nil {
		t.Fatal(err)
	}
	l := New(db)
	if _, _, err := l.Reserve(context.Background(), Record{OwnerID: "owner", ClientTurnID: "race", RequestDigest: Digest("race")}); err != nil {
		t.Fatal(err)
	}
	if err := l.Update(context.Background(), "owner", "race", "core-initial", "conversation-initial", "running", 2, 5, 3, "delta", "", ""); err != nil {
		t.Fatal(err)
	}
	initial, err := l.Get(context.Background(), "owner", "race")
	if err != nil || initial.Status != "running" || initial.CoreTurnID != "core-initial" || initial.ConversationID != "conversation-initial" || initial.LastSequence != 2 || initial.CoreRevision != 5 || initial.ModelProfileRevision != 3 || initial.LastEventKind != "delta" {
		t.Fatalf("nonterminal projection not persisted: %#v err=%v", initial, err)
	}
	if err := l.Update(context.Background(), "owner", "race", "stale-core", "stale-conversation", "failed", 1, 9, 4, "stale", "late", "late"); err != nil {
		t.Fatal(err)
	}
	unchanged, err := l.Get(context.Background(), "owner", "race")
	if err != nil || unchanged.CoreTurnID != "core-initial" || unchanged.LastSequence != 2 || unchanged.LastEventKind != "delta" {
		t.Fatalf("stale projection rewrote row: %#v err=%v", unchanged, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status := "running"
			seq := int64(i + 1)
			if i == 7 {
				status = "done"
				seq = 100
			}
			_ = l.Update(context.Background(), "owner", "race", "core", "conv", status, seq, seq, 1, "delta", "", "")
		}(i)
	}
	wg.Wait()
	record, err := l.Get(context.Background(), "owner", "race")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "done" {
		t.Fatalf("terminal status rewritten: %#v", record)
	}
	if err := l.Update(context.Background(), "owner", "race", "core-complete", "conversation-complete", "completed", 101, 200, 4, "completed", "done", "completed projection"); err != nil {
		t.Fatal(err)
	}
	if err := l.Update(context.Background(), "owner", "race", "late-core", "late-conversation", "failed", 102, 201, 5, "late", "late", "late"); err != nil {
		t.Fatal(err)
	}
	record, err = l.Get(context.Background(), "owner", "race")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "done" || record.CoreTurnID != "core-initial" || record.ConversationID != "conversation-initial" || record.LastSequence != 101 || record.CoreRevision != 200 || record.LastEventKind != "completed" || record.TerminalSummary != "completed projection" {
		t.Fatalf("same-terminal completion or late rewrite mismatch: %#v", record)
	}
}
