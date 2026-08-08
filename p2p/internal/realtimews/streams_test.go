package realtimews

import (
	"context"
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

type sequencedDurableAgent struct {
	params chan map[string]any
}

func (sequencedDurableAgent) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	return nil
}

func (a sequencedDurableAgent) DurableStream(_ context.Context, _ string, _ string, params map[string]any, emit func(agentstream.StreamEvent) error) error {
	if a.params != nil {
		a.params <- cloneMap(params)
	}
	startID := actionbase.String(params["idempotency_key"])
	conversationID := actionbase.String(params["conversation_id"])
	turnID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	turn := agentstream.Turn{State: agentstream.StateAccepted, IdempotencyKey: startID, TurnID: turnID, ConversationID: conversationID, Revision: 1}
	if err := emit(agentstream.StreamEvent{Kind: agentstream.EventAccepted, Turn: turn, IdempotencyKey: startID, TurnID: turnID, ConversationID: conversationID, Revision: 1, Seq: 41, Event: "accepted"}); err != nil {
		return err
	}
	turn.State = agentstream.StateSucceeded
	return emit(agentstream.StreamEvent{Kind: agentstream.EventRuntime, Turn: turn, IdempotencyKey: startID, TurnID: turnID, ConversationID: conversationID, Revision: 1, Seq: 42, Event: "done", Data: map[string]any{"done": true}})
}

func (agentStreamPortStub) DurableStream(
	_ context.Context,
	ownerID string,
	_ string,
	params map[string]any,
	emit func(agentstream.StreamEvent) error,
) error {
	startID := actionbase.String(params["idempotency_key"])
	conversationID := actionbase.String(params["conversation_id"])
	turnID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	revision := int64(1)
	identity := map[string]any{
		"idempotency_key": startID, "conversation_id": conversationID,
		"turn_id": turnID, "revision": float64(revision),
	}
	turn := agentstream.Turn{OwnerID: ownerID, TurnID: turnID, IdempotencyKey: startID, ConversationID: conversationID, Revision: revision}
	accepted := cloneMap(identity)
	accepted["kind"] = "accepted"
	if err := emit(agentstream.StreamEvent{Kind: agentstream.EventAccepted, Turn: turn, TurnID: turnID, IdempotencyKey: startID, ConversationID: conversationID, Revision: revision, Seq: 1, Event: "accepted", Data: accepted}); err != nil {
		return err
	}
	delta := cloneMap(identity)
	delta["kind"], delta["text"] = "delta", "agent"
	if err := emit(agentstream.StreamEvent{Kind: agentstream.EventRuntime, Turn: turn, TurnID: turnID, IdempotencyKey: startID, ConversationID: conversationID, Revision: revision, Seq: 2, Event: "delta", Data: delta}); err != nil {
		return err
	}
	done := cloneMap(identity)
	done["kind"] = "done"
	return emit(agentstream.StreamEvent{Kind: agentstream.EventRuntime, Turn: turn, TurnID: turnID, IdempotencyKey: startID, ConversationID: conversationID, Revision: revision, Seq: 3, Event: "done", Data: done})
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
		"id": "agent-happy", "action": "agent.chat", "params": map[string]any{
			"idempotency_key": "11111111-1111-4111-8111-111111111111",
			"conversation_id": "22222222-2222-4222-8222-222222222222",
			"message":         "hello", "model_profile_id": "profile-id",
			"model_profile_revision": int64(1), "credential_version": int64(1),
		},
	})
	agentAccepted := nextOutbound(t, connection)
	agentDelta := nextOutbound(t, connection)
	agentDone := nextOutbound(t, connection)
	if agentAccepted["type"] != "server.native_agent_stream.accepted" ||
		agentAccepted["idempotency_key"] != "11111111-1111-4111-8111-111111111111" ||
		agentAccepted["turn_id"] != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" ||
		agentAccepted["revision"] != int64(1) {
		t.Fatalf("agent accepted frame did not preserve distinct identities: %#v", agentAccepted)
	}
	if agentDelta["type"] != "server.native_agent_stream.event" || agentDelta["event"] != "delta" || agentDelta["action"] != "agent.chat" || agentDone["event"] != "done" || agentDone["action"] != "agent.chat" {
		t.Fatalf("agent frames = %#v / %#v", agentDelta, agentDone)
	}
	for _, frame := range []map[string]any{agentDelta, agentDone} {
		if frame["idempotency_key"] != "11111111-1111-4111-8111-111111111111" || frame["turn_id"] != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || frame["revision"] != int64(1) {
			t.Fatalf("agent event frame identity drifted: %#v", frame)
		}
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
	if frame["type"] != "server.native_agent_stream.error" || frame["status"] != http.StatusBadRequest {
		t.Fatalf("sensitive stream frame = %#v, want HTTP 400 error", frame)
	}
	if strings.Contains(fmt.Sprint(frame), "stream-secret-canary") {
		t.Fatalf("sensitive stream frame leaked value: %#v", frame)
	}
	if runner.streamCalls != 0 {
		t.Fatalf("sensitive request reached runner %d time(s)", runner.streamCalls)
	}
}

func TestNativeAgentStreamValidatesImmediatelyAndDetachesDurableTurn(t *testing.T) {
	agent := &cancelTrackingAgent{started: make(chan struct{})}
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
		"idempotency_key":        "33333333-3333-4333-8333-333333333333",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
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
	if cancelled["type"] != "server.native_agent_stream.cancelled" || cancelled["ok"] != true || cancelled["execution_continues"] != true {
		t.Fatalf("valid stream cancel frame = %#v", cancelled)
	}
	if agent.cancelCalls != 0 {
		t.Fatalf("attachment detach stopped durable turn: params=%#v calls=%d", agent.cancelParams, agent.cancelCalls)
	}
}

func TestNativeAgentDurableFramesExposeSequenceCursor(t *testing.T) {
	receivedParams := make(chan map[string]any, 1)
	module := New(Dependencies{Agent: sequencedDurableAgent{params: receivedParams}}, Config{})
	connection := newConnection("session", Ticket{Role: "owner", UserID: "@owner:example.test"}, MaxInFlightRequests)
	module.startNativeAgentStream(context.Background(), connection, map[string]any{
		"id": "sequenced", "action": "agent.chat", "params": map[string]any{
			"idempotency_key": "11111111-1111-4111-8111-111111111111",
			"message":         "hello", "model_profile_id": "profile-id",
			"model_profile_revision": int64(1), "credential_version": int64(1),
			"conversation_id": "22222222-2222-4222-8222-222222222222", "after_seq": int64(40),
		},
	})
	params := <-receivedParams
	if params["after_seq"] != int64(40) {
		t.Fatalf("durable stream after_seq = %#v, want 40", params["after_seq"])
	}

	accepted := nextOutbound(t, connection)
	if accepted["type"] != "server.native_agent_stream.accepted" || accepted["seq"] != int64(41) || accepted["idempotency_key"] != "11111111-1111-4111-8111-111111111111" || accepted["revision"] != int64(1) {
		t.Fatalf("accepted frame = %#v, want seq 41", accepted)
	}
	done := nextOutbound(t, connection)
	if done["type"] != "server.native_agent_stream.event" || done["event"] != "done" || done["seq"] != int64(42) {
		t.Fatalf("done frame = %#v, want seq 42", done)
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
