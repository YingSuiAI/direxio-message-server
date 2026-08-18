package members

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

var rebuildGenerationPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ValidRebuildGeneration reports whether value is safe to use as the durable
// fence for one explicit retained-room rebuild.
func ValidRebuildGeneration(value string) bool {
	value = strings.TrimSpace(value)
	return rebuildGenerationPattern.MatchString(value)
}

// RoomReactivate records the local owner's retained room invitation. Matrix
// re-invitation remains in the root protocol adapter that calls this action.
func (m *Module) RoomReactivate(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	if m.config.OwnerMXID == nil || m.config.LookupMember == nil || m.config.NewMember == nil ||
		m.config.SaveMember == nil || m.config.ApplyLocalProfile == nil ||
		m.config.SaveRetainedMetadata == nil || m.config.Conversation == nil {
		return nil, actionbase.InternalError(errors.New("room reactivation dependencies are not configured"))
	}
	params := actionbase.Params(raw)
	roomID := params.String("room_id")
	scope := strings.ToLower(params.String("room_type"))
	if scope == "" {
		scope = strings.ToLower(params.String("scope"))
	}
	if roomID == "" || (scope != "group" && scope != "channel") {
		return nil, actionbase.BadRequest("room_id and room_type are required")
	}

	ownerMXID := m.config.OwnerMXID()
	userID := firstMemberID(params)
	if userID == "" {
		userID = ownerMXID
	}
	if userID != ownerMXID {
		return nil, actionbase.StatusError(http.StatusForbidden, "room reactivation user must be local owner")
	}
	channelID := params.String("channel_id")
	rebuildGeneration := params.String("rebuild_generation")
	if rebuildGeneration != "" && !ValidRebuildGeneration(rebuildGeneration) {
		return nil, actionbase.BadRequest("rebuild_generation is invalid")
	}
	member, found, err := m.config.LookupMember(ctx, roomID, userID)
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	if !found {
		member = m.config.NewMember(roomID, channelID, userID)
	} else if channelID != "" {
		member.ChannelID = channelID
	}
	if rebuildGeneration != "" {
		if m.config.MatrixJoined == nil {
			return nil, actionbase.InternalError(errors.New("matrix member lookup is not configured"))
		}
		joined, joinedErr := m.config.MatrixJoined(ctx, roomID, userID)
		if joinedErr != nil {
			return nil, actionbase.InternalError(joinedErr)
		}
		if joined {
			member.Membership = "join"
			if member.JoinedAt == 0 {
				member.JoinedAt = m.now().UTC().UnixMilli()
			}
			if scope == "group" {
				member.ChannelID = ""
			}
			ApplyMemberProfile(&member, params)
			m.config.ApplyLocalProfile(&member)
			if err := m.config.SaveMember(ctx, member); err != nil {
				return nil, actionbase.InternalError(err)
			}
			if actionErr := m.config.SaveRetainedMetadata(ctx, scope, member, raw); actionErr != nil {
				return nil, actionErr
			}
			result := map[string]any{
				"status": "joined", "room_id": member.RoomID, "member": member,
				"needs_rebuild": false, "rebuild_generation": rebuildGeneration,
			}
			if scope == "channel" {
				if m.config.ChannelSnapshot == nil {
					return nil, actionbase.InternalError(errors.New("channel snapshot is not configured"))
				}
				result["channel"] = m.config.ChannelSnapshot(ctx, member.ChannelID)
			}
			return m.attachOperation(ctx, result, scope+"s.reactivate", "joined", member.RoomID)
		}
		if found && member.RequestID != "" && member.RequestID != rebuildGeneration &&
			!rebuildGenerationMaySupersede(member.Membership) {
			return nil, actionbase.StatusError(http.StatusConflict, "rebuild generation conflicts with current room recovery")
		}
		member.RequestID = rebuildGeneration
	}
	member.Membership = "invite"
	if scope == "group" {
		member.ChannelID = ""
	}
	ApplyMemberProfile(&member, params)
	m.config.ApplyLocalProfile(&member)
	if err := m.config.SaveMember(ctx, member); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if actionErr := m.config.SaveRetainedMetadata(ctx, scope, member, raw); actionErr != nil {
		return nil, actionErr
	}

	result := map[string]any{"status": "invite", "room_id": member.RoomID, "member": member}
	if rebuildGeneration != "" {
		result["needs_rebuild"] = true
		result["rebuild_generation"] = rebuildGeneration
	}
	if scope == "channel" {
		if m.config.ChannelSnapshot == nil {
			return nil, actionbase.InternalError(errors.New("channel snapshot is not configured"))
		}
		result["channel"] = m.config.ChannelSnapshot(ctx, member.ChannelID)
	}
	return m.attachOperation(ctx, result, scope+"s.reactivate", "invite", member.RoomID)
}

func rebuildGenerationMaySupersede(membership string) bool {
	switch strings.ToLower(strings.TrimSpace(membership)) {
	case "pending", "approved", "joining", "join_failed":
		return false
	default:
		// Matrix has already been checked and is not joined. Invite, stale
		// joined, and terminal projections are safe to fence with the latest
		// explicit recovery generation. This also prevents an unauthenticated
		// first writer from permanently poisoning the public callback.
		return true
	}
}
