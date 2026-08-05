package agentcompletion

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRelayPersistsCursorAndPublishesCompletionExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	digest := "sha256:" + strings.Repeat("a", 64)
	executionID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	completion := Completion{
		SourceEventID:  "event-2",
		ConversationID: "agent-chat-11111111-2222-4333-8444-555555555555",
		ExecutionID:    executionID,
		OwnerID:        "dirextalk-project:demo.example",
		TaskID:         "88888888-8888-4888-8888-888888888888",
		PlanID:         "99999999-9999-4999-8999-999999999999",
		PlanRevision:   1,
		ReportDigest:   digest,
		GeneratedAt:    time.Date(2026, 8, 3, 1, 2, 3, 0, time.UTC),
		Execution: map[string]any{
			"execution_id":     executionID,
			"task_id":          "88888888-8888-4888-8888-888888888888",
			"plan_id":          "99999999-9999-4999-8999-999999999999",
			"plan_revision":    int64(1),
			"status":           "completed",
			"cleanup_verified": true,
			"report": map[string]any{
				"report_digest": digest,
				"generated_at":  "2026-08-03T01:02:03Z",
			},
			"artifacts": []map[string]any{{
				"schema_version":       "dirextalk.agent.team-artifact/v1",
				"artifact_id":          "cccccccc-dddd-4eee-8fff-000000000001",
				"role_id":              "implementer",
				"action_id":            "implement",
				"name":                 "final.json",
				"kind":                 "result",
				"media_type":           "application/json",
				"size_bytes":           int64(256),
				"sha256":               "sha256:" + strings.Repeat("b", 64),
				"verification":         "passed",
				"created_at":           "2026-08-03T01:02:02Z",
				"retention_expires_at": "2026-11-01T01:02:02Z",
			}},
		},
	}
	store := &relayTestCursorStore{cancelAt: 2, cancel: cancel}
	sink := &relayTestEventSink{dedupe: make(map[string]struct{})}
	source := &relayTestSource{events: []SourceEvent{
		{Seq: 1},
		{Seq: 2, Completion: &completion},
	}}
	relay := New(source, store, sink, Config{
		RetryDelay: time.Millisecond,
		Now: func() time.Time {
			return time.Date(2026, 8, 3, 1, 2, 4, 0, time.UTC)
		},
	})
	if err := relay.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if store.cursor != 2 {
		t.Fatalf("cursor=%d, want 2", store.cursor)
	}
	if len(sink.events) != 1 {
		t.Fatalf("events=%#v", sink.events)
	}
	event := sink.events[0]
	if event.Type != EventType ||
		event.Payload["source_event_seq"] != int64(2) ||
		event.Payload["conversation_id"] != completion.ConversationID ||
		event.Payload["report_digest"] != digest ||
		event.DedupeKey == "" {
		t.Fatalf("completion event=%#v", event)
	}
}

func TestRelayReportsSanitizedWatchFailureAtBoundedRate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &relayTestFailingSource{
		err:      status.Error(codes.PermissionDenied, "credential detail"),
		cancel:   cancel,
		cancelAt: 3,
	}
	store := &relayTestCursorStore{cursor: 2607}
	sink := &relayTestEventSink{dedupe: make(map[string]struct{})}
	var notices []RetryNotice
	relay := New(source, store, sink, Config{
		RetryDelay:       time.Nanosecond,
		RetryLogInterval: time.Hour,
		LogRetry: func(notice RetryNotice) {
			notices = append(notices, notice)
		},
		Now: func() time.Time {
			return time.Date(2026, 8, 3, 6, 10, 0, 0, time.UTC)
		},
	})
	if err := relay.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 {
		t.Fatalf("retry notices=%#v", notices)
	}
	if notices[0].Cursor != 2607 ||
		notices[0].FailureCode != "permission_denied" ||
		notices[0].RetryDelay != time.Nanosecond {
		t.Fatalf("retry notice=%#v", notices[0])
	}
}

func TestRetryFailureCodeIsClosedAndStable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want string
	}{
		{errAgentEventStreamEnded, "stream_ended"},
		{status.Error(codes.Unauthenticated, "secret"), "unauthenticated"},
		{status.Error(codes.Unavailable, "upstream"), "unavailable"},
		{status.Error(codes.DeadlineExceeded, "timeout"), "deadline_exceeded"},
		{status.Error(codes.ResourceExhausted, "limit"), "resource_exhausted"},
		{errors.New("internal detail"), "relay_error"},
	}
	for _, test := range tests {
		if got := retryFailureCode(test.err); got != test.want {
			t.Fatalf("retryFailureCode(%v)=%q, want %q", test.err, got, test.want)
		}
	}
}

type relayTestSource struct {
	events []SourceEvent
}

type relayTestFailingSource struct {
	err      error
	cancel   context.CancelFunc
	calls    int
	cancelAt int
}

func (source *relayTestFailingSource) WatchTeamCompletionEvents(
	context.Context,
	int64,
	func(SourceEvent) error,
) error {
	source.calls++
	if source.calls >= source.cancelAt {
		source.cancel()
	}
	return source.err
}

func (source *relayTestSource) WatchTeamCompletionEvents(
	ctx context.Context,
	afterSeq int64,
	emit func(SourceEvent) error,
) error {
	for _, event := range source.events {
		if event.Seq <= afterSeq {
			continue
		}
		if err := emit(event); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

type relayTestCursorStore struct {
	mu       sync.Mutex
	cursor   int64
	cancelAt int64
	cancel   context.CancelFunc
}

func (store *relayTestCursorStore) LoadAgentEventCursor(
	context.Context,
	string,
) (int64, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.cursor, nil
}

func (store *relayTestCursorStore) SaveAgentEventCursor(
	_ context.Context,
	_ string,
	cursor int64,
) error {
	store.mu.Lock()
	if cursor > store.cursor {
		store.cursor = cursor
	}
	cancel := store.cancelAt > 0 && store.cursor >= store.cancelAt
	store.mu.Unlock()
	if cancel {
		store.cancel()
	}
	return nil
}

type relayTestEventSink struct {
	mu     sync.Mutex
	dedupe map[string]struct{}
	events []dirextalkdomain.Event
}

func (sink *relayTestEventSink) Append(
	_ context.Context,
	event dirextalkdomain.Event,
) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if _, exists := sink.dedupe[event.DedupeKey]; exists {
		return nil
	}
	sink.dedupe[event.DedupeKey] = struct{}{}
	sink.events = append(sink.events, event)
	return nil
}
