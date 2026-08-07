package projector

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkstate"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	roomserverAPI "github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/types"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type Config struct {
	Now func() time.Time
}

// Module projects Matrix roomserver output into Dirextalk read models and the
// durable product-event outbox. Account lifecycle gating remains in the root
// Service facade so consumers cannot bypass deprovision synchronization.
type Module struct {
	dependencies Dependencies
	now          func() time.Time
}

func New(dependencies Dependencies, cfg Config) *Module {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Module{dependencies: dependencies, now: now}
}

func (m *Module) ProjectOutputEvent(ctx context.Context, output roomserverAPI.OutputEvent) error {
	if output.Type == roomserverAPI.OutputTypeRedactedEvent && output.RedactedEvent != nil {
		return m.removeProjectedEvent(ctx, output.RedactedEvent.RedactedEventID)
	}
	if output.Type == roomserverAPI.OutputTypeNewInviteEvent && output.NewInviteEvent != nil {
		return m.ProjectRoomEvent(ctx, output.NewInviteEvent.Event)
	}
	if output.Type != roomserverAPI.OutputTypeNewRoomEvent || output.NewRoomEvent == nil {
		return nil
	}
	return m.ProjectRoomEvent(ctx, output.NewRoomEvent.Event)
}

func (m *Module) ProjectRoomEvent(ctx context.Context, event *types.HeaderedEvent) error {
	if event == nil {
		return nil
	}
	switch event.Type() {
	case spec.MRoomCreate:
		return m.projectRoomCreate(ctx, event)
	case "m.room.message":
		return m.projectMessage(ctx, event)
	case "m.reaction":
		return m.projectReaction(ctx, event)
	case "m.room.member":
		if event.StateKey() != nil {
			return m.projectMember(ctx, event)
		}
	case dirextalkstate.RoomProfileEventType:
		if event.StateKey() != nil {
			return m.projectRoomProfileState(ctx, event)
		}
	case dirextalkstate.ChannelPostSettingsEventType:
		if event.StateKey() != nil {
			return m.projectChannelPostSettings(ctx, event)
		}
	case dirextalkstate.MemberPolicyEventType:
		if event.StateKey() != nil {
			return m.projectMemberPolicyState(ctx, event)
		}
	case dirextalkstate.JoinRequestEventType:
		if event.StateKey() != nil {
			return m.projectJoinRequestState(ctx, event)
		}
	}
	return nil
}

func (m *Module) appendEvent(ctx context.Context, event dirextalkdomain.Event) error {
	if m == nil || m.dependencies.Events == nil {
		return errors.New("projector event sink is not configured")
	}
	return m.dependencies.Events.Append(ctx, event)
}

func (m *Module) identity() IdentitySnapshot {
	if m == nil || m.dependencies.Identity == nil {
		return IdentitySnapshot{}
	}
	return m.dependencies.Identity()
}

func (m *Module) eventTime(event *types.HeaderedEvent) time.Time {
	if event != nil {
		if ts := int64(event.OriginServerTS()); ts > 0 {
			return time.UnixMilli(ts).UTC()
		}
	}
	return m.now().UTC()
}

func projectedEventDedupeKey(eventType, eventID, subject string) string {
	eventType = strings.TrimSpace(eventType)
	eventID = strings.TrimSpace(eventID)
	subject = strings.TrimSpace(subject)
	if eventType == "" || eventID == "" {
		return ""
	}
	if subject == "" {
		return eventType + ":" + eventID
	}
	return eventType + ":" + eventID + ":" + subject
}

func textValue(value any) string {
	return actionbase.String(value)
}

func boolValue(value any) bool {
	return actionbase.Bool(value)
}

func fallbackText(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func acceptedContact(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "accepted")
}

func deletedContact(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "deleted")
}
