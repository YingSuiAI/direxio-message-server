package realtimews

import (
	"context"
	"net/http"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/realtime"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func (m *Module) readFrames(ctx context.Context, conn *websocket.Conn, client *connection) {
	for {
		var frame map[string]any
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return
		}
		sessionID := client.sessionID
		switch actionbase.String(frame["type"]) {
		case "client.lifecycle":
			m.updateSession(sessionID, func(state *realtime.SessionState) {
				foreground := actionbase.Bool(frame["foreground"])
				hidden := actionbase.Bool(frame["hidden"])
				state.Foreground = foreground
				state.Hidden = hidden
				state.AppState = actionbase.String(frame["state"])
				updateSessionFlags(state, frame, map[string]bool{
					"foreground": foreground,
					"background": !foreground,
					"hidden":     hidden,
				})
			})
		case "client.focus":
			m.updateSession(sessionID, func(state *realtime.SessionState) {
				roomID := actionbase.String(frame["room_id"])
				state.FocusedRoomID = roomID
				updateSessionFlags(state, frame, map[string]bool{"focused": roomID != ""})
			})
		case "client.ack":
			m.updateSession(sessionID, func(state *realtime.SessionState) {
				if seq := actionbase.Int64(frame["seq"]); seq > state.LastAckSeq {
					state.LastAckSeq = seq
				}
			})
		case "client.ping":
			m.touchSession(sessionID)
			pong := map[string]any{"type": "server.pong"}
			if id := actionbase.String(frame["id"]); id != "" {
				pong["id"] = id
			}
			client.send(pong)
		case "client.request":
			client.send(m.HandleRequest(ctx, client.record, frame))
		case "client.command":
			client.send(responseError(
				actionbase.String(frame["id"]),
				actionbase.String(frame["action"]),
				http.StatusBadRequest,
				"unsupported frame type",
			))
		case "client.plugin_stream":
			m.startPluginStream(ctx, client, frame)
		case "client.plugin_stream.cancel":
			m.cancelPluginStream(client, frame)
		case "client.native_agent_stream":
			m.startNativeAgentStream(ctx, client, frame)
		case "client.native_agent_stream.cancel":
			m.cancelNativeAgentStream(client, frame)
		default:
			m.touchSession(sessionID)
		}
	}
}

// HandleRequest validates and invokes a ProductCore client.request frame.
func (m *Module) HandleRequest(ctx context.Context, ticket Ticket, frame map[string]any) map[string]any {
	id := actionbase.String(frame["id"])
	action := actionbase.String(frame["action"])
	if id == "" {
		return responseError(id, action, http.StatusBadRequest, "id is required")
	}
	if action == "" {
		return responseError(id, action, http.StatusBadRequest, "action is required")
	}
	params := map[string]any{}
	if rawParams, ok := frame["params"].(map[string]any); ok {
		params = rawParams
	} else if rawParams, ok := frame["params"].(map[string]interface{}); ok {
		params = rawParams
	} else if frame["params"] != nil {
		return responseError(id, action, http.StatusBadRequest, "params must be an object")
	}
	if action == serviceapi.RealtimeWSTicketAction {
		return responseError(id, action, http.StatusForbidden, "M_FORBIDDEN")
	}
	if message := ClientRequestBlockedMessage(action); message != "" {
		return responseError(id, action, http.StatusBadRequest, message)
	}
	if ticket.Role != "owner" {
		return responseError(id, action, http.StatusForbidden, "M_FORBIDDEN")
	}
	if m == nil || m.actions == nil {
		return responseError(id, action, http.StatusInternalServerError, "service is unavailable")
	}
	result, apiErr := m.actions.Handle(ctx, ticket, action, params)
	if apiErr != nil {
		response := responseError(id, action, apiErr.Status, apiErr.Error)
		if apiErr.Code != "" {
			response["code"] = apiErr.Code
			response["error_code"] = apiErr.Code
		}
		if apiErr.OperationID != "" {
			response["operation_id"] = apiErr.OperationID
		}
		if apiErr.CurrentRoomID != "" {
			response["current_room_id"] = apiErr.CurrentRoomID
		}
		return response
	}
	return map[string]any{
		"type":   "server.response",
		"id":     id,
		"action": action,
		"ok":     true,
		"result": result,
	}
}

func ClientRequestBlockedMessage(action string) string {
	if serviceapi.HTTPOnlyAction(action) {
		return "action requires http"
	}
	if serviceapi.WSStreamOnlyAction(action) {
		return "action requires websocket"
	}
	return ""
}

func updateSessionFlags(state *realtime.SessionState, frame map[string]any, defaults map[string]bool) {
	if state == nil {
		return
	}
	flags := make(map[string]bool, len(state.Flags)+len(defaults)+4)
	for key, value := range state.Flags {
		key = strings.TrimSpace(key)
		if key != "" {
			flags[key] = value
		}
	}
	for key, value := range defaults {
		key = strings.TrimSpace(key)
		if key != "" {
			flags[key] = value
		}
	}
	for key, value := range actionbase.BoolMap(frame["flags"]) {
		flags[key] = value
	}
	if len(flags) == 0 {
		state.Flags = nil
		return
	}
	state.Flags = flags
}

func responseError(id, action string, status int, message string) map[string]any {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(message) == "" {
		message = "M_UNKNOWN"
	}
	return map[string]any{
		"type":   "server.response",
		"id":     id,
		"action": action,
		"ok":     false,
		"status": status,
		"error":  message,
	}
}
