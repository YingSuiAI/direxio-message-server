package groups

import (
	"context"
	"errors"
	"net/http"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

func (m *Module) Create(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	roomID := params.String("room_id")
	needsStatePublish := roomID != ""
	name := params.FirstString("name", "group_name")
	group := View{
		RoomID:       roomID,
		Name:         fallback(name, "Group"),
		Topic:        params.String("topic"),
		AvatarURL:    params.String("avatar_url"),
		MemberCount:  1,
		InvitePolicy: fallback(params.String("invite_policy"), "member"),
	}
	if roomID == "" {
		if m.config.CreateRoom == nil {
			return nil, actionbase.InternalError(errors.New("group room creator is not configured"))
		}
		var actionErr *actionbase.Error
		roomID, actionErr = m.config.CreateRoom(ctx, group)
		if actionErr != nil {
			return nil, actionErr
		}
	}
	group.RoomID = roomID
	if group.Name == "" || group.Name == "Group" && name == "" {
		group.Name = fallback(name, roomID)
	}
	if err := m.Save(ctx, group); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if m.config.SaveOwnerMember == nil {
		return nil, actionbase.InternalError(errors.New("group owner member writer is not configured"))
	}
	if err := m.config.SaveOwnerMember(ctx, group.RoomID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if needsStatePublish {
		if err := m.publishState(ctx, group, false); err != nil {
			return nil, actionbase.InternalError(err)
		}
	}
	result, err := m.WithOperation(ctx, group, actionCreate, "ok")
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	return result, nil
}

func (m *Module) Update(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	roomID := params.String("room_id")
	if roomID == "" {
		return nil, actionbase.BadRequest("room_id is required")
	}
	group, ok, actionErr := m.lookup(ctx, roomID)
	if actionErr != nil || !ok {
		return nil, actionErr
	}
	if m.config.RequireOwner == nil {
		return nil, actionbase.InternalError(errors.New("group owner policy is not configured"))
	}
	if actionErr := m.config.RequireOwner(ctx, group.RoomID); actionErr != nil {
		return nil, actionErr
	}
	if name := params.FirstString("name", "group_name"); name != "" {
		group.Name = name
	}
	if _, exists := raw["topic"]; exists {
		group.Topic = params.String("topic")
	}
	if _, exists := raw["avatar_url"]; exists {
		group.AvatarURL = params.String("avatar_url")
	}
	if policy := params.String("invite_policy"); policy != "" {
		group.InvitePolicy = policy
	}
	if _, exists := raw["muted"]; exists {
		group.Muted = params.Bool("muted")
	}
	if err := m.Save(ctx, group); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if err := m.publishState(ctx, group, false); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return group, nil
}

func (m *Module) handleList(ctx context.Context, _ map[string]any) (any, *actionbase.Error) {
	ownerMXID := ""
	if m.config.OwnerMXID != nil {
		ownerMXID = m.config.OwnerMXID()
	}
	groups, err := m.ListJoined(ctx, ownerMXID)
	if err != nil {
		return map[string]any{"groups": []View{}}, nil
	}
	return map[string]any{"groups": groups}, nil
}

func (m *Module) policyHandler(action string) actionbase.Handler {
	return func(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
		params := actionbase.Params(raw)
		roomID := params.String("room_id")
		if roomID == "" {
			return nil, actionbase.BadRequest("room_id is required")
		}
		group, ok, actionErr := m.lookup(ctx, roomID)
		if actionErr != nil || !ok {
			return nil, actionErr
		}
		switch action {
		case actionMute:
			group.Muted = true
		case actionUnmute:
			group.Muted = false
		case actionInvitePolicyUpdate:
			if policy := params.String("invite_policy"); policy != "" {
				group.InvitePolicy = policy
			}
		}
		if err := m.Save(ctx, group); err != nil {
			return nil, actionbase.InternalError(err)
		}
		if action == actionInvitePolicyUpdate {
			if err := m.publishState(ctx, group, false); err != nil {
				return nil, actionbase.InternalError(err)
			}
			return group, nil
		}
		if m.config.SetMemberMute != nil {
			if actionErr := m.config.SetMemberMute(ctx, roomID, group.Muted); actionErr != nil {
				return nil, actionErr
			}
		}
		return map[string]any{
			"status":  "ok",
			"room_id": group.RoomID,
			"muted":   group.Muted,
			"group":   group,
		}, nil
	}
}

func (m *Module) Dissolve(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	roomID := actionbase.Params(raw).String("room_id")
	if roomID == "" {
		return nil, actionbase.BadRequest("room_id is required")
	}
	group, ok, actionErr := m.lookup(ctx, roomID)
	if actionErr != nil || !ok {
		return nil, actionErr
	}
	if m.config.RequireOwner == nil {
		return nil, actionbase.InternalError(errors.New("group owner policy is not configured"))
	}
	if actionErr := m.config.RequireOwner(ctx, group.RoomID); actionErr != nil {
		return nil, actionErr
	}
	if err := m.publishState(ctx, group, true); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if err := m.Delete(ctx, group.RoomID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return map[string]any{"status": "ok", "group": group}, nil
}

func (m *Module) lookup(ctx context.Context, roomID string) (View, bool, *actionbase.Error) {
	group, ok, err := m.ByRoom(ctx, roomID)
	if err != nil {
		return View{}, false, actionbase.InternalError(err)
	}
	if !ok {
		return View{}, false, actionbase.StatusError(http.StatusNotFound, "group not found")
	}
	return group, true, nil
}

func (m *Module) publishState(ctx context.Context, group View, dissolved bool) error {
	if m.config.PublishState == nil || strings.TrimSpace(group.RoomID) == "" {
		return nil
	}
	return m.config.PublishState(ctx, group, dissolved)
}

func fallback(value, fallbackValue string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallbackValue
}
