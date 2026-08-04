package realtimews

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
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
		"id": "agent-happy", "action": "agent.chat", "params": map[string]any{"prompt": "hello"},
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
	module.startNativeAgentStream(ctx, connection, map[string]any{"id": "shared", "action": "agent.chat"})
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
