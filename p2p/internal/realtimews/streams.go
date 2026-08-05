package realtimews

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/plugins"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
)

type connection struct {
	sessionID      string
	record         Ticket
	outbound       chan map[string]any
	streamMu       sync.Mutex
	streamCancels  map[string]context.CancelFunc
	durableStreams map[string]bool
}

func newConnection(sessionID string, record Ticket) *connection {
	return &connection{
		sessionID: sessionID,
		record:    record,
		outbound:  make(chan map[string]any, 32),
	}
}

func (c *connection) send(frame map[string]any) {
	if c == nil || frame == nil {
		return
	}
	select {
	case c.outbound <- frame:
	default:
	}
}

func (c *connection) sendBlocking(ctx context.Context, frame map[string]any) error {
	if c == nil || frame == nil {
		return nil
	}
	select {
	case c.outbound <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *connection) startStream(id string, cancel context.CancelFunc) bool {
	id = strings.TrimSpace(id)
	if c == nil || id == "" || cancel == nil {
		return false
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if c.streamCancels == nil {
		c.streamCancels = map[string]context.CancelFunc{}
	}
	if _, exists := c.streamCancels[id]; exists {
		return false
	}
	c.streamCancels[id] = cancel
	return true
}

func (c *connection) finishStream(id string) {
	if c == nil {
		return
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	delete(c.streamCancels, strings.TrimSpace(id))
	delete(c.durableStreams, strings.TrimSpace(id))
}

func (c *connection) markDurableStream(id string) {
	if c == nil {
		return
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if _, ok := c.streamCancels[strings.TrimSpace(id)]; ok {
		if c.durableStreams == nil {
			c.durableStreams = map[string]bool{}
		}
		c.durableStreams[strings.TrimSpace(id)] = true
	}
}

func (c *connection) durableStream(id string) bool {
	if c == nil {
		return false
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	return c.durableStreams[strings.TrimSpace(id)]
}

func (c *connection) cancelStream(id string) bool {
	if c == nil {
		return false
	}
	c.streamMu.Lock()
	cancel, ok := c.streamCancels[strings.TrimSpace(id)]
	if ok {
		delete(c.streamCancels, strings.TrimSpace(id))
		delete(c.durableStreams, strings.TrimSpace(id))
	}
	c.streamMu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

func (c *connection) cancelAllStreams() {
	if c == nil {
		return
	}
	c.streamMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.streamCancels))
	for _, cancel := range c.streamCancels {
		cancels = append(cancels, cancel)
	}
	c.streamCancels = map[string]context.CancelFunc{}
	c.durableStreams = map[string]bool{}
	c.streamMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (m *Module) startPluginStream(ctx context.Context, client *connection, frame map[string]any) {
	id := actionbase.String(frame["id"])
	action := actionbase.String(frame["action"])
	if id == "" {
		client.send(pluginStreamError(id, action, http.StatusBadRequest, "id is required"))
		return
	}
	if action == "" {
		client.send(pluginStreamError(id, action, http.StatusBadRequest, "action is required"))
		return
	}
	if client.record.Role != "owner" {
		client.send(pluginStreamError(id, action, http.StatusForbidden, "M_FORBIDDEN"))
		return
	}
	params := map[string]any{
		"plugin_id": frame["plugin_id"],
		"action":    action,
	}
	if rawParams, ok := frame["params"].(map[string]any); ok {
		params["params"] = rawParams
	} else if frame["params"] != nil {
		client.send(pluginStreamError(id, action, http.StatusBadRequest, "params must be an object"))
		return
	}
	prepared, apiErr := m.plugins.PrepareStream(ctx, params)
	if apiErr != nil {
		client.send(pluginStreamError(id, action, apiErr.Status, apiErr.Error))
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	if !client.startStream(id, cancel) {
		cancel()
		client.send(pluginStreamError(id, action, http.StatusConflict, "stream id is already active"))
		return
	}
	go func() {
		defer client.finishStream(id)
		doneSent := false
		err := m.plugins.RunStream(streamCtx, prepared, func(event plugins.StreamEvent) error {
			eventName := strings.TrimSpace(event.Event)
			if eventName == "" {
				eventName = "message"
			}
			if eventName == "done" {
				doneSent = true
			}
			data := event.Data
			if data == nil {
				data = map[string]any{}
			}
			return client.sendBlocking(streamCtx, map[string]any{
				"type":      "server.plugin_stream.event",
				"id":        id,
				"plugin_id": prepared.PluginID,
				"action":    prepared.Action,
				"event":     eventName,
				"data":      data,
			})
		})
		if err != nil {
			if streamCtx.Err() != nil {
				return
			}
			_ = client.sendBlocking(ctx, pluginStreamError(id, prepared.Action, http.StatusBadGateway, err.Error()))
			return
		}
		if !doneSent {
			_ = client.sendBlocking(ctx, map[string]any{
				"type":      "server.plugin_stream.event",
				"id":        id,
				"plugin_id": prepared.PluginID,
				"action":    prepared.Action,
				"event":     "done",
				"data":      map[string]any{},
			})
		}
	}()
}

func (m *Module) cancelPluginStream(client *connection, frame map[string]any) {
	id := actionbase.String(frame["id"])
	if id == "" {
		client.send(pluginStreamError(id, "", http.StatusBadRequest, "id is required"))
		return
	}
	if !client.cancelStream(id) {
		client.send(pluginStreamError(id, "", http.StatusNotFound, "stream is not active"))
		return
	}
	client.send(map[string]any{
		"type": "server.plugin_stream.cancelled",
		"id":   id,
		"ok":   true,
	})
}

func pluginStreamError(id, action string, status int, message string) map[string]any {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(message) == "" {
		message = "M_UNKNOWN"
	}
	return map[string]any{
		"type":   "server.plugin_stream.error",
		"id":     id,
		"action": action,
		"ok":     false,
		"status": status,
		"error":  message,
	}
}

func (m *Module) startNativeAgentStream(ctx context.Context, client *connection, frame map[string]any) {
	id := actionbase.String(frame["id"])
	action := actionbase.String(frame["action"])
	if id == "" {
		client.send(nativeAgentStreamError(id, action, http.StatusBadRequest, "id is required"))
		return
	}
	if action == "" {
		client.send(nativeAgentStreamError(id, action, http.StatusBadRequest, "action is required"))
		return
	}
	if client.record.Role != "owner" {
		client.send(nativeAgentStreamError(id, action, http.StatusForbidden, "M_FORBIDDEN"))
		return
	}
	params := map[string]any{}
	if rawParams, ok := frame["params"].(map[string]any); ok {
		params = cloneMap(rawParams)
	} else if frame["params"] != nil {
		client.send(nativeAgentStreamError(id, action, http.StatusBadRequest, "params must be an object"))
		return
	}
	runnerAction := action
	if !strings.HasSuffix(runnerAction, ".stream") {
		runnerAction += ".stream"
	}
	spec, ok := serviceapi.ActionSpecFor(runnerAction)
	if !ok || spec.Transport != serviceapi.ActionTransportWSStreamOnly || !strings.HasPrefix(runnerAction, "agent.") {
		client.send(nativeAgentStreamError(id, action, http.StatusBadRequest, "action is not a native agent stream action"))
		return
	}
	if m.agent == nil {
		client.send(nativeAgentStreamError(id, action, http.StatusBadGateway, "native agent runtime is not configured"))
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	if !client.startStream(id, cancel) {
		cancel()
		client.send(nativeAgentStreamError(id, action, http.StatusConflict, "stream id is already active"))
		return
	}
	if turnID := actionbase.String(params["turn_id"]); turnID != "" {
		client.markDurableStream(id)
		durable, ok := m.agent.(DurableAgentStreamPort)
		if !ok {
			client.finishStream(id)
			cancel()
			client.send(nativeAgentStreamError(id, action, http.StatusBadGateway, "native agent turn coordinator is not configured"))
			return
		}
		go func() {
			defer client.finishStream(id)
			err := durable.DurableStream(streamCtx, client.record.UserID, runnerAction, params, func(event agentturns.StreamEvent) error {
				switch event.Kind {
				case agentturns.EventAccepted:
					return client.sendBlocking(streamCtx, map[string]any{
						"type": "server.native_agent_stream.accepted", "id": id,
						"action": strings.TrimSuffix(action, ".stream"), "turn_id": event.TurnID,
						"conversation_id": event.ConversationID, "state": string(event.Turn.State),
					})
				case agentturns.EventError:
					message := actionbase.String(event.Data["error"])
					errorCode := actionbase.String(event.Data["error_code"])
					if message == "" {
						message = event.Event
					}
					status := nativeAgentTurnErrorStatus(event.Event, errorCode)
					frame := map[string]any{
						"type": "server.native_agent_stream.error", "id": id,
						"action": strings.TrimSuffix(action, ".stream"), "ok": false, "status": status,
						"error": message, "event": event.Event, "turn_id": event.TurnID,
						"conversation_id": event.ConversationID, "seq": event.Seq, "data": event.Data,
					}
					if errorCode != "" {
						frame["code"] = errorCode
						frame["error_code"] = errorCode
					}
					return client.sendBlocking(streamCtx, frame)
				default:
					return client.sendBlocking(streamCtx, map[string]any{
						"type": "server.native_agent_stream.event", "id": id,
						"action": strings.TrimSuffix(action, ".stream"), "event": event.Event,
						"data": event.Data, "turn_id": event.TurnID,
						"conversation_id": event.ConversationID, "seq": event.Seq,
					})
				}
			})
			if err == nil || streamCtx.Err() != nil {
				return
			}
			status := http.StatusBadGateway
			code := ""
			if errors.Is(err, agentturns.ErrTurnIDReused) {
				status = http.StatusConflict
				code = "M_TURN_ID_REUSED"
			} else if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "after_seq") {
				status = http.StatusBadRequest
			}
			frame := nativeAgentStreamError(id, action, status, err.Error())
			if code != "" {
				frame["code"] = code
				frame["error_code"] = code
			}
			frame["turn_id"] = turnID
			frame["conversation_id"] = durableStreamConversationID(params)
			frame["seq"] = int64(0)
			_ = client.sendBlocking(ctx, frame)
		}()
		return
	}
	if err := client.sendBlocking(streamCtx, map[string]any{
		"type":   "server.native_agent_stream.accepted",
		"id":     id,
		"action": strings.TrimSuffix(action, ".stream"),
		"state":  "running",
	}); err != nil {
		client.finishStream(id)
		cancel()
		return
	}
	go func() {
		defer client.finishStream(id)
		doneSent := false
		err := m.agent.Stream(streamCtx, runnerAction, params, func(event nativeagent.Event) error {
			eventName := strings.TrimSpace(event.Event)
			if eventName == "" {
				eventName = "message"
			}
			if eventName == "done" {
				doneSent = true
			}
			data := event.Data
			if data == nil {
				data = map[string]any{}
			}
			return client.sendBlocking(streamCtx, map[string]any{
				"type":   "server.native_agent_stream.event",
				"id":     id,
				"action": strings.TrimSuffix(action, ".stream"),
				"event":  eventName,
				"data":   data,
			})
		})
		if err != nil {
			if streamCtx.Err() != nil {
				return
			}
			_ = client.sendBlocking(ctx, nativeAgentStreamError(id, action, http.StatusBadGateway, err.Error()))
			return
		}
		if !doneSent {
			_ = client.sendBlocking(ctx, map[string]any{
				"type":   "server.native_agent_stream.event",
				"id":     id,
				"action": strings.TrimSuffix(action, ".stream"),
				"event":  "done",
				"data":   map[string]any{},
			})
		}
	}()
}

func nativeAgentTurnErrorStatus(event, errorCode string) int {
	switch strings.TrimSpace(event) {
	case "stopped":
		return http.StatusConflict
	case "interrupted":
		return http.StatusServiceUnavailable
	}
	switch strings.TrimSpace(errorCode) {
	case "M_AGENT_CONVERSATION_OUT_OF_DATE":
		return http.StatusConflict
	case "M_AGENT_MODEL_CREDENTIAL_INVALID":
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

func durableStreamConversationID(params map[string]any) string {
	return nativeagent.ConversationID(params)
}

func (m *Module) cancelNativeAgentStream(client *connection, frame map[string]any) {
	id := actionbase.String(frame["id"])
	if id == "" {
		client.send(nativeAgentStreamError(id, "", http.StatusBadRequest, "id is required"))
		return
	}
	durable := client.durableStream(id)
	if !client.cancelStream(id) {
		client.send(nativeAgentStreamError(id, "", http.StatusNotFound, "stream is not active"))
		return
	}
	response := map[string]any{
		"type": "server.native_agent_stream.cancelled",
		"id":   id,
		"ok":   true,
	}
	if durable {
		response["execution_continues"] = true
	}
	client.send(response)
}

func nativeAgentStreamError(id, action string, status int, message string) map[string]any {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(message) == "" {
		message = "M_UNKNOWN"
	}
	return map[string]any{
		"type":   "server.native_agent_stream.error",
		"id":     id,
		"action": action,
		"ok":     false,
		"status": status,
		"error":  message,
	}
}

func cloneMap(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
