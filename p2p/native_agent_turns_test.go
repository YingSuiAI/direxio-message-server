package p2p

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/coder/websocket"
)

func TestNativeAgentDurableTurnDeduplicatesAndReplays(t *testing.T) {
	runner := &durableTurnRunner{params: make(chan map[string]any, 1)}
	service := NewService(Config{ServerName: "example.com", NativeAgentRunner: runner})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	ready := readRealtimeFrame(t, conn)
	if ready["native_agent_turns"] != float64(1) {
		t.Fatalf("ready capability = %#v", ready)
	}
	params := map[string]any{
		"turn_id": "turn-replay", "conversation_id": "conversation-replay", "prompt": "hello",
		"model_profile": map[string]any{"provider": "openai", "model": "test", "api_key": "must-not-persist"},
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.native_agent_stream", "id": "durable-1", "action": "agent.chat", "params": params,
	})
	accepted := readRealtimeFrame(t, conn)
	assertDurableAcceptedFrame(t, accepted, "durable-1", "turn-replay", "conversation-replay")
	delta := readRealtimeFrame(t, conn)
	done := readRealtimeFrame(t, conn)
	assertDurableEventFrame(t, delta, "delta", float64(1))
	assertDurableEventFrame(t, done, "done", float64(2))
	runtimeParams := <-runner.params
	if runtimeParams["turn_id"] != nil || runtimeParams["after_seq"] != nil {
		t.Fatalf("coordinator-only params reached runtime: %#v", runtimeParams)
	}
	if runtimeParams["prompt"] != "hello" || runtimeParams["model_profile"] == nil {
		t.Fatalf("runtime params lost chat inputs: %#v", runtimeParams)
	}

	replayParams := cloneTestMap(params)
	replayParams["after_seq"] = float64(1)
	replayParams["model_profile"] = map[string]any{"provider": "openai", "model": "test", "api_key": "replacement-secret"}
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.native_agent_stream", "id": "durable-2", "action": "agent.chat", "params": replayParams,
	})
	accepted = readRealtimeFrame(t, conn)
	assertDurableAcceptedFrame(t, accepted, "durable-2", "turn-replay", "conversation-replay")
	if accepted["state"] != "succeeded" {
		t.Fatalf("replay state = %#v", accepted)
	}
	done = readRealtimeFrame(t, conn)
	assertDurableEventFrame(t, done, "done", float64(2))
	if runner.executions.Load() != 1 {
		t.Fatalf("runner executions = %d, want 1", runner.executions.Load())
	}

	mismatch := cloneTestMap(params)
	mismatch["prompt"] = "different"
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.native_agent_stream", "id": "durable-mismatch", "action": "agent.chat", "params": mismatch,
	})
	conflict := readRealtimeFrame(t, conn)
	if conflict["type"] != "server.native_agent_stream.error" || conflict["status"] != float64(http.StatusConflict) || conflict["code"] != "M_TURN_ID_REUSED" {
		t.Fatalf("turn mismatch = %#v", conflict)
	}
}

func TestNativeAgentDurableTurnRepairsStaleClientConversationRevision(t *testing.T) {
	runner := &durableTurnRunner{params: make(chan map[string]any, 1)}
	authority := &durableConversationStateReader{
		revision: 29,
		requests: make(chan string, 1),
	}
	service := NewService(Config{
		ServerName: "example.com", NativeAgentRunner: runner,
		AgentConversationStateReader: authority,
	})
	previousDigest, err := agentturns.RequestDigest("agent.chat.stream", map[string]any{"prompt": "previous"})
	if err != nil {
		t.Fatal(err)
	}
	ownerID := service.OwnerMXID()
	if _, err = service.store.ReserveAgentTurn(context.Background(), agentturns.Candidate{
		OwnerID: ownerID, TurnID: "previous-turn", ConversationID: "conversation-revision", Action: "agent.chat.stream", Digest: previousDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := service.store.MarkAgentTurnRunning(context.Background(), ownerID, "previous-turn"); err != nil || !changed {
		t.Fatalf("mark previous turn running = (%v, %v)", changed, err)
	}
	if _, _, changed, err := service.store.FinishAgentTurn(
		context.Background(), ownerID, "previous-turn", agentturns.StateSucceeded,
		"runtime", "done", map[string]any{"conversation_revision": int64(24)}, "",
	); err != nil || !changed {
		t.Fatalf("finish previous turn = (%v, %v)", changed, err)
	}

	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, conn)
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.native_agent_stream", "id": "stale-revision", "action": "agent.chat",
		"params": map[string]any{
			"turn_id": "new-turn", "conversation_id": "conversation-revision", "prompt": "continue",
			"expected_conversation_revision": 23,
		},
	})
	assertDurableAcceptedFrame(t, readRealtimeFrame(t, conn), "stale-revision", "new-turn", "conversation-revision")
	_ = readRealtimeFrame(t, conn)
	_ = readRealtimeFrame(t, conn)
	runtimeParams := <-runner.params
	if runtimeParams["expected_conversation_revision"] != int64(29) {
		t.Fatalf("runtime revision = %#v, want 29", runtimeParams["expected_conversation_revision"])
	}
	if conversationID := <-authority.requests; conversationID != "conversation-revision" {
		t.Fatalf("authoritative conversation request=%q", conversationID)
	}
}

func TestNativeAgentDurableTurnSurvivesWebSocketDisconnectAndReattaches(t *testing.T) {
	runner := &disconnectTurnRunner{
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		cancelled: make(chan struct{}),
	}
	service := NewService(Config{ServerName: "example.com", NativeAgentRunner: runner})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()

	first := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	writeRealtimeFrame(t, first, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, first)
	params := map[string]any{
		"turn_id": "turn-disconnect", "conversation_id": "conversation-disconnect", "prompt": "finish after page close",
	}
	writeRealtimeFrame(t, first, map[string]any{
		"type": "client.native_agent_stream", "id": "before-disconnect", "action": "agent.chat", "params": params,
	})
	assertDurableAcceptedFrame(t, readRealtimeFrame(t, first), "before-disconnect", "turn-disconnect", "conversation-disconnect")
	<-runner.started
	if err := first.Close(websocket.StatusNormalClosure, "page closed"); err != nil {
		t.Fatalf("close first websocket: %v", err)
	}
	select {
	case <-runner.cancelled:
		t.Fatal("websocket disconnect cancelled durable execution")
	case <-time.After(50 * time.Millisecond):
	}
	close(runner.release)
	waitServiceTurnState(t, service, "turn-disconnect", "succeeded")

	second := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer second.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, second, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, second)
	replayParams := cloneTestMap(params)
	replayParams["after_seq"] = float64(0)
	writeRealtimeFrame(t, second, map[string]any{
		"type": "client.native_agent_stream", "id": "after-disconnect", "action": "agent.chat", "params": replayParams,
	})
	accepted := readRealtimeFrame(t, second)
	assertDurableAcceptedFrame(t, accepted, "after-disconnect", "turn-disconnect", "conversation-disconnect")
	if accepted["state"] != "succeeded" {
		t.Fatalf("reattached turn state = %#v", accepted)
	}
	delta := readRealtimeFrame(t, second)
	done := readRealtimeFrame(t, second)
	if delta["type"] != "server.native_agent_stream.event" || delta["event"] != "delta" || delta["turn_id"] != "turn-disconnect" || delta["conversation_id"] != "conversation-disconnect" || delta["seq"] != float64(1) {
		t.Fatalf("reattached delta = %#v", delta)
	}
	if done["type"] != "server.native_agent_stream.event" || done["event"] != "done" || done["turn_id"] != "turn-disconnect" || done["conversation_id"] != "conversation-disconnect" || done["seq"] != float64(2) {
		t.Fatalf("reattached done = %#v", done)
	}
	if data, _ := done["data"].(map[string]any); data["text"] != "completed after disconnect" {
		t.Fatalf("reattached terminal assistant reply = %#v", done)
	}
	if runner.executions.Load() != 1 {
		t.Fatalf("runner executions = %d, want 1", runner.executions.Load())
	}
}

func TestNativeAgentDurableTurnRuntimeErrorFailsAndReplaysWithoutDone(t *testing.T) {
	service := NewService(Config{ServerName: "example.com", NativeAgentDataDir: t.TempDir()})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	params := map[string]any{
		"turn_id": "turn-missing-api-key", "conversation_id": "conversation-missing-api-key", "prompt": "hello",
	}

	first := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer first.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, first, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, first)
	writeRealtimeFrame(t, first, map[string]any{
		"type": "client.native_agent_stream", "id": "missing-api-key", "action": "agent.chat", "params": params,
	})
	assertDurableAcceptedFrame(t, readRealtimeFrame(t, first), "missing-api-key", "turn-missing-api-key", "conversation-missing-api-key")
	failed := readRealtimeFrame(t, first)
	if failed["type"] != "server.native_agent_stream.error" || failed["event"] != "failed" || failed["seq"] != float64(1) || failed["error"] != "model_profile.api_key is required" {
		t.Fatalf("missing-api-key failure frame = %#v", failed)
	}
	waitServiceTurnState(t, service, "turn-missing-api-key", "failed")
	events, err := service.store.ListAgentTurnEvents(context.Background(), service.OwnerMXID(), "turn-missing-api-key", 0)
	if err != nil || len(events) != 1 || events[0].Event != "failed" {
		t.Fatalf("terminal events = (%#v, %v), want one failed event", events, err)
	}

	second := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer second.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, second, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, second)
	replayParams := cloneTestMap(params)
	replayParams["after_seq"] = float64(0)
	writeRealtimeFrame(t, second, map[string]any{
		"type": "client.native_agent_stream", "id": "missing-api-key-replay", "action": "agent.chat", "params": replayParams,
	})
	accepted := readRealtimeFrame(t, second)
	assertDurableAcceptedFrame(t, accepted, "missing-api-key-replay", "turn-missing-api-key", "conversation-missing-api-key")
	if accepted["state"] != "failed" {
		t.Fatalf("replayed failure state = %#v", accepted)
	}
	replayed := readRealtimeFrame(t, second)
	if replayed["type"] != "server.native_agent_stream.error" || replayed["event"] != "failed" || replayed["seq"] != float64(1) || replayed["error"] != "model_profile.api_key is required" {
		t.Fatalf("replayed missing-api-key failure = %#v", replayed)
	}
}

func TestNativeAgentDurableTurnDetachContinuesAndExplicitStopCancels(t *testing.T) {
	runner := &durableTurnRunner{started: make(chan struct{}), release: make(chan struct{})}
	service := NewService(Config{ServerName: "example.com", NativeAgentRunner: runner})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, conn)
	params := map[string]any{"turn_id": "turn-detach", "conversation_id": "conversation", "prompt": "hold"}
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.native_agent_stream", "id": "detach", "action": "agent.chat", "params": params,
	})
	assertDurableAcceptedFrame(t, readRealtimeFrame(t, conn), "detach", "turn-detach", "conversation")
	<-runner.started
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.native_agent_stream.cancel", "id": "detach"})
	cancelled := readRealtimeFrame(t, conn)
	if cancelled["type"] != "server.native_agent_stream.cancelled" || cancelled["execution_continues"] != true {
		t.Fatalf("durable detach = %#v", cancelled)
	}
	close(runner.release)
	waitServiceTurnState(t, service, "turn-detach", "succeeded")

	stopRunner := &durableTurnRunner{started: make(chan struct{}), waitForCancel: true}
	stopService := NewService(Config{ServerName: "example.com", NativeAgentRunner: stopRunner})
	stopRouter := newP2PTestRouter(stopService)
	stopServer := httptest.NewServer(stopRouter)
	defer stopServer.Close()
	stopConn := dialRealtimeWS(t, stopServer.URL, mustCreateRealtimeWSTicket(t, stopRouter, stopService.AccessToken()))
	defer stopConn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, stopConn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, stopConn)
	stopParams := map[string]any{"turn_id": "turn-stop", "conversation_id": "conversation", "prompt": "stop"}
	writeRealtimeFrame(t, stopConn, map[string]any{
		"type": "client.native_agent_stream", "id": "stop", "action": "agent.chat", "params": stopParams,
	})
	assertDurableAcceptedFrame(t, readRealtimeFrame(t, stopConn), "stop", "turn-stop", "conversation")
	<-stopRunner.started
	result, apiErr := stopService.Handle(context.Background(), "agent.chat.turn.stop", map[string]any{"turn_id": "turn-stop"})
	if apiErr != nil || result.(map[string]any)["state"] != "stopped" || result.(map[string]any)["changed"] != true {
		t.Fatalf("stop result = (%#v, %#v)", result, apiErr)
	}
	stopped := readRealtimeFrame(t, stopConn)
	if stopped["type"] != "server.native_agent_stream.error" || stopped["event"] != "stopped" || stopped["seq"] != float64(1) {
		t.Fatalf("stopped event = %#v", stopped)
	}
	result, apiErr = stopService.Handle(context.Background(), "agent.chat.turn.stop", map[string]any{"turn_id": "turn-stop"})
	if apiErr != nil || result.(map[string]any)["changed"] != false {
		t.Fatalf("idempotent stop result = (%#v, %#v)", result, apiErr)
	}
}

type durableTurnRunner struct {
	executions    atomic.Int32
	started       chan struct{}
	release       chan struct{}
	params        chan map[string]any
	waitForCancel bool
	startOnce     sync.Once
}

type durableConversationStateReader struct {
	revision int64
	requests chan string
}

func (reader *durableConversationStateReader) GetConversationState(
	_ context.Context,
	conversationID string,
) (int64, bool, error) {
	reader.requests <- conversationID
	return reader.revision, true, nil
}

type disconnectTurnRunner struct {
	executions atomic.Int32
	started    chan struct{}
	release    chan struct{}
	cancelled  chan struct{}
}

func (r *disconnectTurnRunner) Apply(context.Context, string) error { return nil }

func (r *disconnectTurnRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (r *disconnectTurnRunner) Stream(ctx context.Context, _ string, _ map[string]any, emit func(nativeagent.Event) error) error {
	r.executions.Add(1)
	close(r.started)
	select {
	case <-r.release:
	case <-ctx.Done():
		close(r.cancelled)
		return ctx.Err()
	}
	if err := emit(nativeagent.Event{Event: "delta", Data: map[string]any{"text": "completed"}}); err != nil {
		return err
	}
	return emit(nativeagent.Event{Event: "done", Data: map[string]any{"text": "completed after disconnect"}})
}

func (r *durableTurnRunner) Apply(context.Context, string) error { return nil }

func (r *durableTurnRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (r *durableTurnRunner) Stream(ctx context.Context, _ string, params map[string]any, emit func(nativeagent.Event) error) error {
	r.executions.Add(1)
	if r.params != nil {
		r.params <- cloneTestMap(params)
	}
	if r.started != nil {
		r.startOnce.Do(func() { close(r.started) })
	}
	if r.waitForCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := emit(nativeagent.Event{Event: "delta", Data: map[string]any{"text": "hel"}}); err != nil {
		return err
	}
	return emit(nativeagent.Event{Event: "done", Data: map[string]any{"text": "hello"}})
}

func assertDurableAcceptedFrame(t *testing.T, frame map[string]any, id, turnID, conversationID string) {
	t.Helper()
	if frame["type"] != "server.native_agent_stream.accepted" || frame["id"] != id || frame["turn_id"] != turnID || frame["conversation_id"] != conversationID {
		t.Fatalf("accepted frame = %#v", frame)
	}
}

func assertDurableEventFrame(t *testing.T, frame map[string]any, event string, seq float64) {
	t.Helper()
	if frame["type"] != "server.native_agent_stream.event" || frame["event"] != event || frame["seq"] != seq || frame["turn_id"] != "turn-replay" || frame["conversation_id"] != "conversation-replay" {
		t.Fatalf("durable event frame = %#v", frame)
	}
}

func waitServiceTurnState(t *testing.T, service *Service, turnID, state string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		result, apiErr := service.Handle(context.Background(), "agent.chat.turns.list", map[string]any{})
		if apiErr == nil {
			for _, item := range result.(map[string]any)["turns"].([]map[string]any) {
				if item["turn_id"] == turnID && item["state"] == state {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("turn %s did not reach %s", turnID, state)
}

func cloneTestMap(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
