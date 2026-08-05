package realtimews

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/plugins"
)

type pluginStreamPortStub struct {
	started chan struct{}
	once    sync.Once
}

func (p *pluginStreamPortStub) PrepareStream(_ context.Context, params map[string]any) (plugins.PreparedStream, *actionbase.Error) {
	return plugins.PreparedStream{
		PluginID: actionbase.String(params["plugin_id"]),
		Action:   actionbase.String(params["action"]),
	}, nil
}

func (p *pluginStreamPortStub) RunStream(
	ctx context.Context,
	prepared plugins.PreparedStream,
	emit func(plugins.StreamEvent) error,
) error {
	if prepared.Action == "hold" {
		p.once.Do(func() { close(p.started) })
		<-ctx.Done()
		return ctx.Err()
	}
	return emit(plugins.StreamEvent{Event: "delta", Data: map[string]any{"text": "plugin"}})
}

type agentStreamPortStub struct{}

func (agentStreamPortStub) Stream(
	_ context.Context,
	_ string,
	_ map[string]any,
	emit func(agentstream.Event) error,
) error {
	return emit(agentstream.Event{Event: "delta", Data: map[string]any{"text": "agent"}})
}

type validatingNativeAgentRunner struct {
	streamCalls int
}

type cancelTrackingAgent struct {
	streamCalls        int
	durableStreamCalls int
	cancelCalls        int
	cancelParams       map[string]any
	started            chan struct{}
	startOnce          sync.Once
}

func (a *cancelTrackingAgent) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	a.streamCalls++
	return nil
}

func (a *cancelTrackingAgent) DurableStream(ctx context.Context, _ string, _ string, _ map[string]any, _ func(agentstream.StreamEvent) error) error {
	a.durableStreamCalls++
	if a.started != nil {
		a.startOnce.Do(func() { close(a.started) })
	}
	<-ctx.Done()
	return ctx.Err()
}

func (a *cancelTrackingAgent) CancelExternal(_ context.Context, _ string, params map[string]any) (map[string]any, error) {
	a.cancelCalls++
	a.cancelParams = cloneMap(params)
	return map[string]any{"ok": true}, nil
}

func (r *validatingNativeAgentRunner) Apply(context.Context, string) error { return nil }

func (r *validatingNativeAgentRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (r *validatingNativeAgentRunner) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	r.streamCalls++
	return nil
}

func TestPluginAndAgentStreamsPreserveFramesAndSharedIDNamespace(t *testing.T) {
	pluginPort := &pluginStreamPortStub{started: make(chan struct{})}
	module := New(Dependencies{Plugins: pluginPort, Agent: agentStreamPortStub{}}, Config{})
	connection := newConnection("session", Ticket{Role: "owner"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	module.startPluginStream(ctx, connection, map[string]any{
		"id": "plugin-happy", "plugin_id": "io.dirextalk.ops", "action": "status",
	})
	pluginDelta := nextOutbound(t, connection)
	pluginDone := nextOutbound(t, connection)
	if pluginDelta["type"] != "server.plugin_stream.event" || pluginDelta["event"] != "delta" || pluginDone["event"] != "done" {
		t.Fatalf("plugin frames = %#v / %#v", pluginDelta, pluginDone)
	}

	module.startNativeAgentStream(ctx, connection, map[string]any{
		"id": "agent-happy", "action": "agent.chat", "params": map[string]any{
			"prompt": "hello", "model_profile_id": "profile-id",
			"model_profile_revision": int64(1), "credential_version": int64(1),
		},
	})
	agentDelta := nextOutbound(t, connection)
	agentDone := nextOutbound(t, connection)
	if agentDelta["type"] != "server.native_agent_stream.event" || agentDelta["event"] != "delta" || agentDelta["action"] != "agent.chat" || agentDone["event"] != "done" || agentDone["action"] != "agent.chat" {
		t.Fatalf("agent frames = %#v / %#v", agentDelta, agentDone)
	}

	module.startNativeAgentStream(ctx, connection, map[string]any{
		"id": "agent-voice", "action": "agent.voice.session.stream",
	})
	voiceDelta := nextOutbound(t, connection)
	voiceDone := nextOutbound(t, connection)
	if voiceDelta["type"] != "server.native_agent_stream.event" || voiceDelta["event"] != "delta" || voiceDelta["action"] != "agent.voice.session.stream" || voiceDone["event"] != "done" || voiceDone["action"] != "agent.voice.session.stream" {
		t.Fatalf("voice frames must preserve canonical stream action = %#v / %#v", voiceDelta, voiceDone)
	}

	module.startPluginStream(ctx, connection, map[string]any{
		"id": "shared", "plugin_id": "io.dirextalk.ops", "action": "hold",
	})
	select {
	case <-pluginPort.started:
	case <-time.After(time.Second):
		t.Fatal("blocking plugin stream did not start")
	}
	module.startNativeAgentStream(ctx, connection, map[string]any{
		"id": "shared", "action": "agent.chat", "params": map[string]any{
			"prompt": "hello", "model_profile_id": "profile-id",
			"model_profile_revision": int64(1), "credential_version": int64(1),
		},
	})
	conflict := nextOutbound(t, connection)
	if conflict["type"] != "server.native_agent_stream.error" || conflict["status"] != http.StatusConflict {
		t.Fatalf("shared ID conflict = %#v", conflict)
	}
	module.cancelPluginStream(connection, map[string]any{"id": "shared"})
	cancelled := nextOutbound(t, connection)
	if cancelled["type"] != "server.plugin_stream.cancelled" || cancelled["ok"] != true {
		t.Fatalf("cancelled frame = %#v", cancelled)
	}
}

func TestNativeAgentStreamRejectsSensitiveKeysBeforeForwardAndReplay(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream", true: "durable-replay"}[durable], func(t *testing.T) {
			runner := &validatingNativeAgentRunner{}
			agent := agentmodule.New(agentmodule.Config{Runner: runner})
			module := New(Dependencies{Agent: agent}, Config{})
			connection := newConnection("session", Ticket{Role: "owner", UserID: "@owner:example.test"})
			params := map[string]any{
				"message":                "hello",
				"model_profile_id":       "profile-id",
				"model_profile_revision": int64(2),
				"credential_version":     int64(3),
				"metadata":               []any{map[string]any{"dbPass": "stream-secret-canary"}},
			}
			if durable {
				params["turn_id"] = "turn-1"
				params["conversation_id"] = "conversation-1"
			}
			module.startNativeAgentStream(context.Background(), connection, map[string]any{
				"id": "sensitive", "action": "agent.chat", "params": params,
			})

			frame := nextOutbound(t, connection)
			if frame["type"] != "server.native_agent_stream.error" || frame["status"] != http.StatusBadRequest {
				t.Fatalf("sensitive %s frame = %#v, want HTTP 400 error", map[bool]string{false: "stream", true: "durable-replay"}[durable], frame)
			}
			if strings.Contains(fmt.Sprint(frame), "stream-secret-canary") {
				t.Fatalf("sensitive %s frame leaked value: %#v", map[bool]string{false: "stream", true: "durable-replay"}[durable], frame)
			}
			if runner.streamCalls != 0 {
				t.Fatalf("sensitive %s request reached runner %d time(s)", map[bool]string{false: "stream", true: "durable-replay"}[durable], runner.streamCalls)
			}
		})
	}
}

func TestNativeAgentStreamValidatesImmediatelyAndCancelsOnlyTurnID(t *testing.T) {
	agent := &cancelTrackingAgent{started: make(chan struct{})}
	module := New(Dependencies{Agent: agent}, Config{})
	connection := newConnection("session", Ticket{Role: "owner", UserID: "@owner:example.test"})
	invalid := map[string]any{
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
		"turn_id":                "invalid-turn",
		"conversation_id":        "conversation-1",
		"metadata":               map[string]any{"httpBasicAuth": "immediate-secret"},
	}
	module.startNativeAgentStream(context.Background(), connection, map[string]any{
		"id": "invalid", "action": "agent.chat", "params": invalid,
	})
	frame := nextOutbound(t, connection)
	if frame["type"] != "server.native_agent_stream.error" || frame["status"] != http.StatusBadRequest {
		t.Fatalf("invalid immediate frame = %#v, want HTTP 400", frame)
	}
	if strings.Contains(fmt.Sprint(frame), "immediate-secret") {
		t.Fatalf("invalid immediate frame leaked secret: %#v", frame)
	}
	module.cancelNativeAgentStream(connection, map[string]any{"id": "invalid"})
	if cancelFrame := nextOutbound(t, connection); cancelFrame["status"] != http.StatusNotFound {
		t.Fatalf("invalid stream cancel frame = %#v, want not found", cancelFrame)
	}
	if agent.streamCalls != 0 || agent.durableStreamCalls != 0 || agent.cancelCalls != 0 {
		t.Fatalf("invalid request reached agent: stream=%d durable=%d cancel=%d", agent.streamCalls, agent.durableStreamCalls, agent.cancelCalls)
	}

	valid := map[string]any{
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
		"turn_id":                "valid-turn",
		"conversation_id":        "conversation-1",
		"metadata":               map[string]any{"ordinary": "value"},
	}
	module.startNativeAgentStream(context.Background(), connection, map[string]any{
		"id": "valid", "action": "agent.chat", "params": valid,
	})
	select {
	case <-agent.started:
	case <-time.After(time.Second):
		t.Fatal("valid durable stream did not reach agent")
	}
	module.cancelNativeAgentStream(connection, map[string]any{"id": "valid"})
	cancelled := nextOutbound(t, connection)
	if cancelled["type"] != "server.native_agent_stream.cancelled" || cancelled["ok"] != true {
		t.Fatalf("valid stream cancel frame = %#v", cancelled)
	}
	if agent.cancelCalls != 1 || !reflect.DeepEqual(agent.cancelParams, map[string]any{"turn_id": "valid-turn"}) {
		t.Fatalf("cancel forwarded params = %#v, calls=%d; want only turn_id", agent.cancelParams, agent.cancelCalls)
	}
}

func nextOutbound(t *testing.T, connection *connection) map[string]any {
	t.Helper()
	select {
	case frame := <-connection.outbound:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for outbound frame")
		return nil
	}
}
