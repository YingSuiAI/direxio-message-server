package realtimews

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/plugins"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
)

type connection struct {
	sessionID      string
	record         Ticket
	outbound       chan map[string]any
	requestMu      sync.Mutex
	requestLimit   int
	requestCancels map[string]context.CancelFunc
	streamMu       sync.Mutex
	streamCancels  map[string]context.CancelFunc
}

func newConnection(sessionID string, record Ticket, requestLimit int) *connection {
	return &connection{
		sessionID:    sessionID,
		record:       record,
		outbound:     make(chan map[string]any, 32),
		requestLimit: requestLimit,
	}
}

func (c *connection) startRequest(id string, cancel context.CancelFunc) (started, duplicate bool) {
	id = strings.TrimSpace(id)
	if c == nil || id == "" || cancel == nil {
		return false, false
	}
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	if c.requestCancels == nil {
		c.requestCancels = map[string]context.CancelFunc{}
	}
	if _, exists := c.requestCancels[id]; exists {
		return false, true
	}
	if c.requestLimit <= 0 || len(c.requestCancels) >= c.requestLimit {
		return false, false
	}
	c.requestCancels[id] = cancel
	return true, false
}

func (c *connection) finishRequest(id string) {
	if c == nil {
		return
	}
	c.requestMu.Lock()
	delete(c.requestCancels, strings.TrimSpace(id))
	c.requestMu.Unlock()
}

func (c *connection) cancelAllRequests() {
	if c == nil {
		return
	}
	c.requestMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.requestCancels))
	for _, cancel := range c.requestCancels {
		cancels = append(cancels, cancel)
	}
	c.requestCancels = map[string]context.CancelFunc{}
	c.requestMu.Unlock()
	for _, cancel := range cancels {
		cancel()
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
}

func (c *connection) cancelStream(id string) bool {
	if c == nil {
		return false
	}
	c.streamMu.Lock()
	cancel, ok := c.streamCancels[strings.TrimSpace(id)]
	if ok {
		delete(c.streamCancels, strings.TrimSpace(id))
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
	if runnerAction == "agent.chat.stream" {
		client.send(nativeAgentStreamError(id, action, http.StatusBadRequest, "text turns use the HTTP/SSE transport"))
		return
	}
	spec, ok := serviceapi.ActionSpecFor(runnerAction)
	if !ok || spec.Transport != serviceapi.ActionTransportWSStreamOnly || !strings.HasPrefix(runnerAction, "agent.") {
		client.send(nativeAgentStreamError(id, action, http.StatusBadRequest, "action is not a native agent stream action"))
		return
	}
	if err := agentgateway.ValidateActionRequest(runnerAction, params); err != nil {
		client.send(nativeAgentStreamError(id, action, http.StatusBadRequest, err.Error()))
		return
	}
	if m.agent == nil {
		client.send(nativeAgentStreamError(id, action, http.StatusBadGateway, "native agent runtime is not configured"))
		return
	}
	if m.nativeAgentReadiness != nil && m.nativeAgentReadiness() != nil {
		client.send(nativeAgentStreamError(id, action, http.StatusServiceUnavailable, "native agent capability catalog is not ready"))
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	if !client.startStream(id, cancel) {
		cancel()
		client.send(nativeAgentStreamError(id, action, http.StatusConflict, "stream id is already active"))
		return
	}
	go func() {
		defer client.finishStream(id)
		wireAction := nativeAgentStreamAction(runnerAction)
		doneSent := false
		terminalErrorSent := false
		err := m.agent.Stream(streamCtx, runnerAction, params, func(event agentstream.Event) error {
			eventName := strings.TrimSpace(event.Event)
			if eventName == "" {
				eventName = "message"
			}
			if eventName == "done" {
				doneSent = true
			} else if eventName == "error" || eventName == "cancelled" {
				terminalErrorSent = true
			}
			data := event.Data
			if data == nil {
				data = map[string]any{}
			}
			return client.sendBlocking(streamCtx, map[string]any{
				"type":   "server.native_agent_stream.event",
				"id":     id,
				"action": wireAction,
				"event":  eventName,
				"data":   data,
			})
		})
		if err != nil {
			if streamCtx.Err() != nil || terminalErrorSent {
				return
			}
			status := http.StatusBadGateway
			if errors.Is(err, agentgateway.ErrInvalidActionRequest) {
				status = http.StatusBadRequest
			}
			_ = client.sendBlocking(ctx, nativeAgentStreamError(id, action, status, err.Error()))
			return
		}
		if !doneSent {
			_ = client.sendBlocking(ctx, map[string]any{
				"type":   "server.native_agent_stream.event",
				"id":     id,
				"action": wireAction,
				"event":  "done",
				"data":   map[string]any{},
			})
		}
	}()
}

func nativeAgentStreamAction(action string) string {
	if action == "agent.voice.session.stream" {
		return action
	}
	return strings.TrimSuffix(action, ".stream")
}

func (m *Module) cancelNativeAgentStream(client *connection, frame map[string]any) {
	id := actionbase.String(frame["id"])
	if id == "" {
		client.send(nativeAgentStreamError(id, "", http.StatusBadRequest, "id is required"))
		return
	}
	if !client.cancelStream(id) {
		client.send(nativeAgentStreamError(id, "", http.StatusNotFound, "stream is not active"))
		return
	}
	client.send(map[string]any{
		"type": "server.native_agent_stream.cancelled",
		"id":   id,
		"ok":   true,
	})
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
