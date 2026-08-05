package agentgrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcompletion"
)

var teamConversationIDPattern = regexp.MustCompile(
	`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`,
)

type teamExecutionCompletedSummary struct {
	SchemaVersion   int       `json:"schema_version"`
	ExecutionID     string    `json:"execution_id"`
	OwnerID         string    `json:"owner_id"`
	TaskID          string    `json:"task_id"`
	PlanID          string    `json:"plan_id"`
	PlanRevision    uint64    `json:"plan_revision"`
	PlanDigest      string    `json:"plan_digest"`
	Status          string    `json:"status"`
	RecordRevision  uint64    `json:"record_revision"`
	ConversationID  string    `json:"conversation_id"`
	ReportDigest    string    `json:"report_digest"`
	ReportGenerated time.Time `json:"report_generated_at"`
	CleanupVerified bool      `json:"cleanup_verified"`
}

func (runner *Runner) WatchTeamCompletionEvents(
	ctx context.Context,
	afterSeq int64,
	emit func(agentcompletion.SourceEvent) error,
) error {
	if runner == nil || runner.tasks == nil || runner.team == nil {
		return errors.New("agent service client is unavailable")
	}
	if afterSeq < 0 || emit == nil {
		return errors.New("Agent completion watch is invalid")
	}
	stream, err := runner.tasks.WatchEvents(
		ctx,
		&agentv1.WatchEventsRequest{AfterSeq: afterSeq},
	)
	if err != nil {
		return sanitizeRPCError(ctx, err)
	}
	cursor := afterSeq
	for {
		response, receiveErr := stream.Recv()
		if errors.Is(receiveErr, io.EOF) {
			return errors.New("agent service event stream ended")
		}
		if receiveErr != nil {
			return sanitizeRPCError(ctx, receiveErr)
		}
		remote := response.GetEvent()
		if remote == nil || remote.GetSeq() <= cursor {
			return errors.New("agent service returned an invalid event sequence")
		}
		cursor = remote.GetSeq()
		if remote.GetEventType() != "team.execution.completed" {
			if err := emit(agentcompletion.SourceEvent{Seq: cursor}); err != nil {
				return err
			}
			continue
		}
		completion, err := runner.teamCompletionFromEvent(ctx, remote)
		if err != nil {
			return err
		}
		if completion == nil {
			if err := emit(agentcompletion.SourceEvent{Seq: cursor}); err != nil {
				return err
			}
			continue
		}
		if err := emit(agentcompletion.SourceEvent{
			Seq:        cursor,
			Completion: completion,
		}); err != nil {
			return err
		}
	}
}

func (runner *Runner) teamCompletionFromEvent(
	ctx context.Context,
	remote *agentv1.Event,
) (*agentcompletion.Completion, error) {
	if remote == nil || remote.GetEventType() != "team.execution.completed" ||
		remote.GetAggregateType() != "team_execution" ||
		!canonicalUUID(remote.GetAggregateId()) ||
		remote.GetRevision() < 1 ||
		remote.GetOccurredAt() == nil ||
		remote.GetOccurredAt().CheckValid() != nil {
		return nil, errors.New("agent service returned an invalid Team completion event")
	}
	var summary teamExecutionCompletedSummary
	decoder := json.NewDecoder(bytes.NewReader(remote.GetSummaryJson()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&summary); err != nil {
		return nil, errors.New("agent service returned an invalid Team completion event")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("agent service returned an invalid Team completion event")
	}
	if summary.SchemaVersion != 1 ||
		summary.ExecutionID != remote.GetAggregateId() ||
		summary.OwnerID != runner.ownerID ||
		!canonicalUUID(summary.ExecutionID) ||
		!canonicalUUID(summary.TaskID) ||
		!canonicalUUID(summary.PlanID) ||
		summary.PlanRevision < 1 ||
		!teamDigestPattern.MatchString(summary.PlanDigest) ||
		summary.Status != "completed" ||
		summary.RecordRevision != uint64(remote.GetRevision()) ||
		!teamDigestPattern.MatchString(summary.ReportDigest) ||
		summary.ReportGenerated.IsZero() ||
		summary.ReportGenerated.After(remote.GetOccurredAt().AsTime()) ||
		!summary.CleanupVerified {
		return nil, errors.New("agent service returned an invalid Team completion event")
	}
	conversationID := strings.TrimSpace(summary.ConversationID)
	if conversationID == "" {
		return nil, nil
	}
	if conversationID != summary.ConversationID ||
		!teamConversationIDPattern.MatchString(conversationID) {
		return nil, errors.New("agent service returned an invalid Team completion conversation")
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.team.GetTeamExecutionV3(
		callContext,
		&agentv1.GetTeamExecutionV3Request{
			OwnerId:     runner.ownerID,
			ExecutionId: summary.ExecutionID,
		},
	)
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil || response.GetExecution() == nil {
		return nil, errors.New("agent service returned an invalid Team execution response")
	}
	execution, err := runner.mapTeamExecution(response.GetExecution())
	if err != nil {
		return nil, err
	}
	report, ok := execution["report"].(map[string]any)
	reportGeneratedText, _ := report["generated_at"].(string)
	reportGeneratedAt, reportGeneratedErr := time.Parse(
		time.RFC3339Nano,
		reportGeneratedText,
	)
	if execution["execution_id"] != summary.ExecutionID ||
		execution["task_id"] != summary.TaskID ||
		execution["plan_id"] != summary.PlanID ||
		execution["plan_revision"] != int64(summary.PlanRevision) ||
		execution["plan_digest"] != summary.PlanDigest ||
		execution["record_revision"] != int64(summary.RecordRevision) ||
		execution["status"] != "completed" ||
		execution["cleanup_verified"] != true ||
		!ok || report["report_digest"] != summary.ReportDigest ||
		reportGeneratedErr != nil ||
		!reportGeneratedAt.Equal(summary.ReportGenerated) {
		return nil, errors.New("agent service returned an unbound Team completion report")
	}
	return &agentcompletion.Completion{
		SourceEventID:  remote.GetEventId(),
		ConversationID: conversationID,
		ExecutionID:    summary.ExecutionID,
		OwnerID:        summary.OwnerID,
		TaskID:         summary.TaskID,
		PlanID:         summary.PlanID,
		PlanRevision:   int64(summary.PlanRevision),
		ReportDigest:   summary.ReportDigest,
		GeneratedAt:    summary.ReportGenerated.UTC(),
		Execution:      execution,
	}, nil
}
