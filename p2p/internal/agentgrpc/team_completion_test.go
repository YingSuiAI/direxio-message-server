package agentgrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcompletion"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRunnerStreamsReportBoundTeamCompletion(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	server.team.mu.Lock()
	execution := server.team.execution
	report := execution.GetReport()
	report.GetRoles()[0].GetFinals()[0].Risks = nil
	server.team.mu.Unlock()
	generatedAt := report.GetGeneratedAt().AsTime()
	summary, err := json.Marshal(map[string]any{
		"schema_version":      1,
		"execution_id":        execution.GetExecutionId(),
		"owner_id":            execution.GetOwnerId(),
		"task_id":             execution.GetTaskId(),
		"plan_id":             execution.GetPlanId(),
		"plan_revision":       execution.GetPlanRevision(),
		"plan_digest":         execution.GetPlanDigest(),
		"status":              "completed",
		"record_revision":     execution.GetRecordRevision(),
		"conversation_id":     "agent-chat-11111111-2222-4333-8444-555555555555",
		"report_digest":       report.GetReportDigest(),
		"report_generated_at": generatedAt,
		"cleanup_verified":    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.tasks.mu.Lock()
	server.tasks.events = []*agentv1.Event{
		{
			Seq:           40,
			EventId:       "irrelevant-event",
			EventType:     "task.changed",
			AggregateType: "task",
			AggregateId:   testTeamTaskID,
			Revision:      1,
			SummaryJson:   []byte(`{}`),
			OccurredAt:    timestamppb.New(generatedAt),
		},
		{
			Seq:           41,
			EventId:       "completion-event",
			EventType:     "team.execution.completed",
			AggregateType: "team_execution",
			AggregateId:   execution.GetExecutionId(),
			Revision:      execution.GetRecordRevision(),
			SummaryJson:   summary,
			OccurredAt:    timestamppb.New(generatedAt.Add(time.Second)),
		},
	}
	server.tasks.mu.Unlock()
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var received []agentcompletion.SourceEvent
	err = runner.WatchTeamCompletionEvents(
		ctx,
		39,
		func(event agentcompletion.SourceEvent) error {
			received = append(received, event)
			if event.Seq == 41 {
				cancel()
			}
			return nil
		},
	)
	if err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
	if len(received) != 2 || received[0].Seq != 40 ||
		received[0].Completion != nil ||
		received[1].Completion == nil {
		t.Fatalf("events=%#v", received)
	}
	completion := received[1].Completion
	if completion.ConversationID !=
		"agent-chat-11111111-2222-4333-8444-555555555555" ||
		completion.ExecutionID != testTeamExecutionID ||
		completion.ReportDigest != report.GetReportDigest() ||
		completion.Execution["cleanup_verified"] != true {
		t.Fatalf("completion=%#v", completion)
	}
	encodedExecution, err := json.Marshal(completion.Execution)
	if err != nil || !bytes.Contains(encodedExecution, []byte(`"risks":[]`)) ||
		bytes.Contains(encodedExecution, []byte(`"risks":null`)) {
		t.Fatalf("completion execution JSON=%s error=%v", encodedExecution, err)
	}
}

func TestRunnerRejectsCompletionWithMismatchedReportTimestamp(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	server.team.mu.Lock()
	execution := server.team.execution
	report := execution.GetReport()
	server.team.mu.Unlock()
	generatedAt := report.GetGeneratedAt().AsTime()
	summary, err := json.Marshal(map[string]any{
		"schema_version":      1,
		"execution_id":        execution.GetExecutionId(),
		"owner_id":            execution.GetOwnerId(),
		"task_id":             execution.GetTaskId(),
		"plan_id":             execution.GetPlanId(),
		"plan_revision":       execution.GetPlanRevision(),
		"plan_digest":         execution.GetPlanDigest(),
		"status":              "completed",
		"record_revision":     execution.GetRecordRevision(),
		"conversation_id":     "agent-chat-11111111-2222-4333-8444-555555555555",
		"report_digest":       report.GetReportDigest(),
		"report_generated_at": generatedAt.Add(time.Second),
		"cleanup_verified":    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})
	completion, err := runner.teamCompletionFromEvent(
		context.Background(),
		&agentv1.Event{
			Seq:           41,
			EventId:       "completion-event",
			EventType:     "team.execution.completed",
			AggregateType: "team_execution",
			AggregateId:   execution.GetExecutionId(),
			Revision:      execution.GetRecordRevision(),
			SummaryJson:   summary,
			OccurredAt:    timestamppb.New(generatedAt.Add(2 * time.Second)),
		},
	)
	if err == nil || completion != nil {
		t.Fatalf("completion=%#v err=%v", completion, err)
	}
}
