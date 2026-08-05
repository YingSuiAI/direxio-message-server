// Package agentcompletion relays verified Agent Team completion reports into
// the durable ProductCore event stream.
package agentcompletion

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	CursorSource  = "native-agent-task-events/v1"
	EventType     = "agent.team.execution.completed"
	SchemaVersion = "dirextalk.product.agent-team-execution-completed/v1"
)

var (
	errAgentEventStreamEnded = errors.New("Agent event stream ended")
	uuidPattern              = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
	)
	digestPattern       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	artifactNamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,127}$`)
	roleIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	actionIDPattern     = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	conversationPattern = regexp.MustCompile(
		`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`,
	)
)

type Completion struct {
	SourceEventID  string
	ConversationID string
	ExecutionID    string
	OwnerID        string
	TaskID         string
	PlanID         string
	PlanRevision   int64
	ReportDigest   string
	GeneratedAt    time.Time
	Execution      map[string]any
}

type SourceEvent struct {
	Seq        int64
	Completion *Completion
}

type Source interface {
	WatchTeamCompletionEvents(
		context.Context,
		int64,
		func(SourceEvent) error,
	) error
}

type CursorStore interface {
	LoadAgentEventCursor(context.Context, string) (int64, error)
	SaveAgentEventCursor(context.Context, string, int64) error
}

type EventAppender interface {
	Append(context.Context, dirextalkdomain.Event) error
}

type Relay struct {
	source           Source
	store            CursorStore
	events           EventAppender
	retryDelay       time.Duration
	retryLogInterval time.Duration
	logRetry         func(RetryNotice)
	now              func() time.Time
	cursorSource     string
}

type RetryNotice struct {
	Cursor      int64
	FailureCode string
	RetryDelay  time.Duration
}

type Config struct {
	RetryDelay       time.Duration
	RetryLogInterval time.Duration
	LogRetry         func(RetryNotice)
	Now              func() time.Time
}

func New(
	source Source,
	store CursorStore,
	events EventAppender,
	config Config,
) *Relay {
	retryDelay := config.RetryDelay
	if retryDelay <= 0 {
		retryDelay = time.Second
	}
	retryLogInterval := config.RetryLogInterval
	if retryLogInterval <= 0 {
		retryLogInterval = time.Minute
	}
	logRetry := config.LogRetry
	if logRetry == nil {
		logRetry = func(notice RetryNotice) {
			logrus.WithFields(logrus.Fields{
				"cursor":         notice.Cursor,
				"failure_code":   notice.FailureCode,
				"retry_delay_ms": notice.RetryDelay.Milliseconds(),
			}).Warn("P2P Agent completion relay stream failed; retrying")
		}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Relay{
		source:           source,
		store:            store,
		events:           events,
		retryDelay:       retryDelay,
		retryLogInterval: retryLogInterval,
		logRetry:         logRetry,
		now:              now,
		cursorSource:     CursorSource,
	}
}

func (relay *Relay) Run(ctx context.Context) error {
	if relay == nil || relay.source == nil || relay.store == nil ||
		relay.events == nil {
		return errors.New("Agent completion relay is unavailable")
	}
	cursor, err := relay.store.LoadAgentEventCursor(
		ctx,
		relay.cursorSource,
	)
	if err != nil {
		return fmt.Errorf("load Agent event cursor: %w", err)
	}
	if cursor < 0 {
		return errors.New("Agent event cursor is invalid")
	}
	var lastRetryLog time.Time
	for ctx.Err() == nil {
		watchErr := relay.source.WatchTeamCompletionEvents(
			ctx,
			cursor,
			func(event SourceEvent) error {
				if event.Seq <= cursor {
					return errors.New(
						"Agent event sequence did not advance",
					)
				}
				if event.Completion != nil {
					if err := validateCompletion(*event.Completion); err != nil {
						return err
					}
					if err := relay.events.Append(
						ctx,
						completionProductEvent(
							event.Seq,
							*event.Completion,
							relay.now().UTC(),
						),
					); err != nil {
						return fmt.Errorf(
							"append Agent completion event: %w",
							err,
						)
					}
				}
				if err := relay.store.SaveAgentEventCursor(
					ctx,
					relay.cursorSource,
					event.Seq,
				); err != nil {
					return fmt.Errorf("save Agent event cursor: %w", err)
				}
				cursor = event.Seq
				return nil
			},
		)
		if ctx.Err() != nil {
			return nil
		}
		if watchErr == nil {
			watchErr = errAgentEventStreamEnded
		}
		retryLogAt := relay.now().UTC()
		if lastRetryLog.IsZero() ||
			!retryLogAt.Before(lastRetryLog.Add(relay.retryLogInterval)) {
			relay.logRetry(RetryNotice{
				Cursor:      cursor,
				FailureCode: retryFailureCode(watchErr),
				RetryDelay:  relay.retryDelay,
			})
			lastRetryLog = retryLogAt
		}
		timer := time.NewTimer(relay.retryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func retryFailureCode(err error) string {
	if errors.Is(err, errAgentEventStreamEnded) {
		return "stream_ended"
	}
	switch status.Code(err) {
	case codes.Unauthenticated:
		return "unauthenticated"
	case codes.PermissionDenied:
		return "permission_denied"
	case codes.Unavailable:
		return "unavailable"
	case codes.DeadlineExceeded:
		return "deadline_exceeded"
	case codes.ResourceExhausted:
		return "resource_exhausted"
	default:
		return "relay_error"
	}
}

func validateCompletion(value Completion) error {
	if !uuidPattern.MatchString(value.ExecutionID) ||
		!uuidPattern.MatchString(value.TaskID) ||
		!uuidPattern.MatchString(value.PlanID) ||
		strings.TrimSpace(value.OwnerID) == "" ||
		len(value.OwnerID) > 255 ||
		value.OwnerID != strings.TrimSpace(value.OwnerID) ||
		!conversationPattern.MatchString(value.ConversationID) ||
		value.PlanRevision < 1 ||
		!digestPattern.MatchString(value.ReportDigest) ||
		value.GeneratedAt.IsZero() ||
		value.Execution == nil {
		return errors.New("Agent completion is invalid")
	}
	if textValue(value.Execution["execution_id"]) != value.ExecutionID ||
		textValue(value.Execution["task_id"]) != value.TaskID ||
		textValue(value.Execution["plan_id"]) != value.PlanID ||
		int64Value(value.Execution["plan_revision"]) != value.PlanRevision ||
		textValue(value.Execution["status"]) != "completed" ||
		value.Execution["cleanup_verified"] != true {
		return errors.New("Agent completion execution is unbound")
	}
	if err := validateCompletionArtifacts(value.Execution["artifacts"]); err != nil {
		return err
	}
	report, ok := value.Execution["report"].(map[string]any)
	if !ok || textValue(report["report_digest"]) != value.ReportDigest {
		return errors.New("Agent completion report is unbound")
	}
	reportGeneratedAt, err := time.Parse(
		time.RFC3339Nano,
		textValue(report["generated_at"]),
	)
	if err != nil || !reportGeneratedAt.Equal(value.GeneratedAt) {
		return errors.New("Agent completion report is unbound")
	}
	return nil
}

func validateCompletionArtifacts(raw any) error {
	artifacts, ok := raw.([]map[string]any)
	if !ok || len(artifacts) == 0 || len(artifacts) > 1024 {
		return errors.New("Agent completion artifacts are invalid")
	}
	seen := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if len(artifact) != 12 ||
			textValue(artifact["schema_version"]) !=
				"dirextalk.agent.team-artifact/v1" ||
			!uuidPattern.MatchString(textValue(artifact["artifact_id"])) ||
			!roleIDPattern.MatchString(textValue(artifact["role_id"])) ||
			!actionIDPattern.MatchString(textValue(artifact["action_id"])) ||
			!artifactNamePattern.MatchString(textValue(artifact["name"])) ||
			!validCompletionArtifactKind(
				textValue(artifact["kind"]),
				textValue(artifact["name"]),
			) ||
			!validCompletionArtifactMediaType(
				textValue(artifact["media_type"]),
			) ||
			int64Value(artifact["size_bytes"]) < 1 ||
			int64Value(artifact["size_bytes"]) > 8<<20 ||
			!digestPattern.MatchString(textValue(artifact["sha256"])) ||
			textValue(artifact["verification"]) != "passed" {
			return errors.New("Agent completion artifact is invalid")
		}
		createdAt, createdErr := time.Parse(
			time.RFC3339Nano,
			textValue(artifact["created_at"]),
		)
		expiresAt, expiresErr := time.Parse(
			time.RFC3339Nano,
			textValue(artifact["retention_expires_at"]),
		)
		if createdErr != nil || expiresErr != nil ||
			!expiresAt.After(createdAt) ||
			expiresAt.After(createdAt.Add(366*24*time.Hour)) {
			return errors.New("Agent completion artifact retention is invalid")
		}
		artifactID := textValue(artifact["artifact_id"])
		if _, duplicate := seen[artifactID]; duplicate {
			return errors.New("Agent completion artifact is duplicated")
		}
		seen[artifactID] = struct{}{}
	}
	return nil
}

func validCompletionArtifactKind(kind, name string) bool {
	switch name {
	case "final.json":
		return kind == "result"
	case "changes.patch":
		return kind == "patch"
	default:
		return kind == "file"
	}
}

func validCompletionArtifactMediaType(value string) bool {
	return value == "application/json" ||
		value == "text/plain; charset=utf-8"
}

func completionProductEvent(
	sourceSeq int64,
	completion Completion,
	createdAt time.Time,
) dirextalkdomain.Event {
	return dirextalkdomain.Event{
		Type: EventType,
		DedupeKey: fmt.Sprintf(
			"%s:%s:%s",
			EventType,
			completion.ExecutionID,
			completion.ReportDigest,
		),
		Payload: map[string]any{
			"schema_version":   SchemaVersion,
			"source_event_seq": sourceSeq,
			"conversation_id":  completion.ConversationID,
			"execution_id":     completion.ExecutionID,
			"task_id":          completion.TaskID,
			"plan_id":          completion.PlanID,
			"plan_revision":    completion.PlanRevision,
			"report_digest":    completion.ReportDigest,
			"generated_at":     completion.GeneratedAt.UTC().Format(time.RFC3339Nano),
			"execution":        completion.Execution,
		},
		CreatedAt: createdAt.Format(time.RFC3339Nano),
	}
}

func textValue(value any) string {
	text, _ := value.(string)
	return text
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case uint64:
		if typed <= uint64(^uint64(0)>>1) {
			return int64(typed)
		}
	case float64:
		converted := int64(typed)
		if float64(converted) == typed {
			return converted
		}
	}
	return 0
}
