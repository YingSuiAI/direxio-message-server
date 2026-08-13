package realtimews

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

type terminalErrorAgent struct{}

func (terminalErrorAgent) Stream(_ context.Context, _ string, _ map[string]any, emit func(agentstream.Event) error) error {
	if err := emit(agentstream.Event{Event: "error", Data: map[string]any{"error": "gateway terminal error"}}); err != nil {
		return err
	}
	return errors.New("gateway terminal error")
}

type validatingNativeAgentRunner struct {
	streamCalls int
}

func (r *validatingNativeAgentRunner) Apply(context.Context, string) error { return nil }

func (r *validatingNativeAgentRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (r *validatingNativeAgentRunner) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	r.streamCalls++
	return nil
}

func TestPluginAndVoiceStreamsPreserveFramesAndSharedIDNamespace(t *testing.T) {
	pluginPort := &pluginStreamPortStub{started: make(chan struct{})}
	module := New(Dependencies{Plugins: pluginPort, Agent: agentStreamPortStub{}}, Config{})
	connection := newConnection("session", Ticket{Role: "owner"}, MaxInFlightRequests)
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
			"idempotency_key": "33333333-3333-4333-8333-333333333333",
			"conversation_id": "22222222-2222-4222-8222-222222222222",
			"message":         "hello", "model_profile_id": "profile-id",
			"model_profile_revision": int64(1), "credential_version": int64(1),
		},
	})
	conflict := nextOutbound(t, connection)
	if conflict["type"] != "server.native_agent_stream.error" || conflict["status"] != http.StatusBadRequest || conflict["error"] != "text turns use the HTTP/SSE transport" {
		t.Fatalf("retired text WS result = %#v", conflict)
	}
	module.cancelPluginStream(connection, map[string]any{"id": "shared"})
	cancelled := nextOutbound(t, connection)
	if cancelled["type"] != "server.plugin_stream.cancelled" || cancelled["ok"] != true {
		t.Fatalf("cancelled frame = %#v", cancelled)
	}
}

func TestNativeAgentStreamRejectsSensitiveKeysBeforeForwardAndReplay(t *testing.T) {
	runner := &validatingNativeAgentRunner{}
	agent := agentmodule.New(agentmodule.Config{Runner: runner})
	module := New(Dependencies{Agent: agent}, Config{})
	connection := newConnection("session", Ticket{Role: "owner", UserID: "@owner:example.test"}, MaxInFlightRequests)
	params := map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
		"metadata":               []any{map[string]any{"dbPass": "stream-secret-canary"}},
	}
	module.startNativeAgentStream(context.Background(), connection, map[string]any{
		"id": "sensitive", "action": "agent.chat", "params": params,
	})

	frame := nextOutbound(t, connection)
	if frame["type"] != "server.native_agent_stream.error" || frame["status"] != http.StatusBadRequest || frame["error"] != "text turns use the HTTP/SSE transport" {
		t.Fatalf("sensitive stream frame = %#v, want HTTP 400 error", frame)
	}
	if strings.Contains(fmt.Sprint(frame), "stream-secret-canary") {
		t.Fatalf("sensitive stream frame leaked value: %#v", frame)
	}
	if runner.streamCalls != 0 {
		t.Fatalf("sensitive request reached runner %d time(s)", runner.streamCalls)
	}
}

func TestNativeAgentTextStreamIsRetiredBeforeForward(t *testing.T) {
	agent := &validatingNativeAgentRunner{}
	module := New(Dependencies{Agent: agent}, Config{})
	connection := newConnection("session", Ticket{Role: "owner", UserID: "@owner:example.test"}, MaxInFlightRequests)
	invalid := map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
		"metadata":               map[string]any{"httpBasicAuth": "immediate-secret"},
	}
	module.startNativeAgentStream(context.Background(), connection, map[string]any{
		"id": "invalid", "action": "agent.chat", "params": invalid,
	})
	frame := nextOutbound(t, connection)
	if frame["type"] != "server.native_agent_stream.error" || frame["status"] != http.StatusBadRequest || frame["error"] != "text turns use the HTTP/SSE transport" {
		t.Fatalf("invalid immediate frame = %#v, want HTTP 400", frame)
	}
	if strings.Contains(fmt.Sprint(frame), "immediate-secret") {
		t.Fatalf("invalid immediate frame leaked secret: %#v", frame)
	}
	module.cancelNativeAgentStream(connection, map[string]any{"id": "invalid"})
	if cancelFrame := nextOutbound(t, connection); cancelFrame["status"] != http.StatusNotFound {
		t.Fatalf("invalid stream cancel frame = %#v, want not found", cancelFrame)
	}
	if agent.streamCalls != 0 {
		t.Fatalf("retired text WS reached agent %d time(s)", agent.streamCalls)
	}
}

func TestNativeAgentNonDurableTerminalErrorIsSentOnce(t *testing.T) {
	module := New(Dependencies{Agent: terminalErrorAgent{}}, Config{})
	connection := newConnection("session", Ticket{Role: "owner", UserID: "@owner:example.test"}, MaxInFlightRequests)
	module.startNativeAgentStream(context.Background(), connection, map[string]any{
		"id": "terminal-voice", "action": "agent.voice.session.stream", "params": map[string]any{},
	})
	frame := nextOutbound(t, connection)
	if frame["type"] != "server.native_agent_stream.event" || frame["event"] != "error" {
		t.Fatalf("non-durable terminal error frame = %#v", frame)
	}
	select {
	case duplicate := <-connection.outbound:
		t.Fatalf("non-durable terminal gateway error was duplicated: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
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
