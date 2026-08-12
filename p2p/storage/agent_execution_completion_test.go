package storage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	testdb "github.com/YingSuiAI/dirextalk-message-server/test"
)

func storageCompletionReceipt() agentExecutionCompletionReceipt {
	return agentExecutionCompletionReceipt{
		EventID:           "00000000-0000-4000-8000-000000000001",
		ExecutionID:       "00000000-0000-4000-8000-000000000002",
		RunID:             "00000000-0000-4000-8000-000000000002",
		ConversationID:    "00000000-0000-4000-8000-000000000003",
		TurnID:            "00000000-0000-4000-8000-000000000004",
		TerminalState:     "succeeded",
		CompletedAt:       "2026-08-07T01:02:03.123456789Z",
		PayloadDigest:     "2d0ca6e1e63d3ef71d036a2c28c943376fa8e157e640bc7b701043fe86f7b850",
		OwnerID:           "@owner:example.test",
		AccountGeneration: 7,
	}
}

func storageCompletionEvent(receipt agentExecutionCompletionReceipt) p2pEvent {
	return p2pEvent{
		Seq: 100, Type: "agent.execution.v2.completed", EventID: receipt.EventID,
		DedupeKey: "agent.execution.v2.completed:" + receipt.EventID,
		Payload:   receipt.PublicPayload(), CreatedAt: receipt.CompletedAt,
	}
}

func TestMemoryCompletionConcurrentReplayAndLazyFixtureInitialization(t *testing.T) {
	store := &MemoryStore{}
	receipt := storageCompletionReceipt()
	event := storageCompletionEvent(receipt)
	var inserted atomic.Int64
	sequences := make(chan int64, 64)
	errorsOut := make(chan error, 64)
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, sequence, err := store.RecordAgentExecutionCompletion(context.Background(), receipt, event)
			if created {
				inserted.Add(1)
			}
			sequences <- sequence
			errorsOut <- err
		}()
	}
	wg.Wait()
	close(sequences)
	close(errorsOut)
	if inserted.Load() != 1 {
		t.Fatalf("completion inserts=%d", inserted.Load())
	}
	for err := range errorsOut {
		if err != nil {
			t.Fatalf("concurrent exact replay failed: %v", err)
		}
	}
	for sequence := range sequences {
		if sequence != 100 {
			t.Fatalf("completion sequence=%d want 100", sequence)
		}
	}
	events, err := store.ListEvents(context.Background(), 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("completion events=%#v err=%v", events, err)
	}
}

func TestMemoryCompletionRejectsEitherIdentityCollision(t *testing.T) {
	store := NewMemoryStore()
	receipt := storageCompletionReceipt()
	if inserted, _, err := store.RecordAgentExecutionCompletion(context.Background(), receipt, storageCompletionEvent(receipt)); err != nil || !inserted {
		t.Fatalf("seed completion inserted=%t err=%v", inserted, err)
	}
	for name, mutate := range map[string]func(*agentExecutionCompletionReceipt){
		"event": func(value *agentExecutionCompletionReceipt) {
			value.ExecutionID = "00000000-0000-4000-8000-000000000009"
			value.RunID = value.ExecutionID
		},
		"execution": func(value *agentExecutionCompletionReceipt) {
			value.EventID = "00000000-0000-4000-8000-000000000009"
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := receipt
			mutate(&candidate)
			_, _, err := store.RecordAgentExecutionCompletion(context.Background(), candidate, storageCompletionEvent(candidate))
			if !errors.Is(err, dirextalkdomain.ErrAgentExecutionCompletionConflict) {
				t.Fatalf("identity collision error=%v", err)
			}
		})
	}
}

func TestPostgresCompletionConcurrentDedupeAndRestartReplay(t *testing.T) {
	ctx := context.Background()
	connectionString, closeDatabase := testdb.PrepareDBConnectionString(t, testdb.DBTypePostgres)
	defer closeDatabase()
	options := config.DatabaseOptions{ConnectionString: config.DataSource(connectionString)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, options), &options)
	if err != nil {
		t.Fatal(err)
	}
	receipt := storageCompletionReceipt()
	event := storageCompletionEvent(receipt)
	var inserted atomic.Int64
	sequences := make(chan int64, 16)
	errorsOut := make(chan error, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, sequence, recordErr := store.RecordAgentExecutionCompletion(ctx, receipt, event)
			if created {
				inserted.Add(1)
			}
			sequences <- sequence
			errorsOut <- recordErr
		}()
	}
	wg.Wait()
	close(sequences)
	close(errorsOut)
	if inserted.Load() != 1 {
		t.Fatalf("postgres completion inserts=%d", inserted.Load())
	}
	var sequence int64
	for err := range errorsOut {
		if err != nil {
			t.Fatalf("postgres concurrent completion: %v", err)
		}
	}
	for current := range sequences {
		if sequence == 0 {
			sequence = current
		}
		if current != sequence || current <= 0 {
			t.Fatalf("postgres completion sequence=%d want %d", current, sequence)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, options), &options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	created, replaySequence, err := reopened.RecordAgentExecutionCompletion(ctx, receipt, event)
	if err != nil || created || replaySequence != sequence {
		t.Fatalf("restart replay created=%t sequence=%d err=%v", created, replaySequence, err)
	}
	conflict := receipt
	conflict.TerminalState = "failed"
	conflict.PayloadDigest, _ = dirextalkdomain.CanonicalAgentExecutionCompletionDigest(conflict)
	if _, _, err := reopened.RecordAgentExecutionCompletion(ctx, conflict, storageCompletionEvent(conflict)); !errors.Is(err, dirextalkdomain.ErrAgentExecutionCompletionConflict) {
		t.Fatalf("postgres completion conflict error=%v", err)
	}
	for table, want := range map[string]int{"p2p_agent_execution_completion_receipts": 1, "p2p_events": 1} {
		var count int
		if err := reopened.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
}
