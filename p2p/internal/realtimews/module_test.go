package realtimews

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConsumeTicketRejectsInactiveAccount(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	module := New(Dependencies{
		TicketActive: func(Ticket) bool { return false },
	}, Config{
		Now:      func() time.Time { return now },
		NewToken: func(string) string { return "ticket" },
	})
	ticket := module.IssueTicket(Ticket{Role: "owner"})["ticket"].(string)
	if _, err := module.ConsumeTicket(ticket); err == nil {
		t.Fatal("inactive account ticket was accepted")
	}
}

func TestReadyCapabilitiesAreRevalidatedForEveryConnection(t *testing.T) {
	ready := false
	module := New(Dependencies{
		Agent: agentStreamPortStub{},
		NativeAgentReadiness: func() error {
			if ready {
				return nil
			}
			return errors.New("catalog unavailable")
		},
	}, Config{})
	if _, ok := module.readyCapabilities()["native_agent_turns"]; ok {
		t.Fatal("initially unready Agent capability was advertised")
	}
	ready = true
	if module.readyCapabilities()["native_agent_turns"] != 1 {
		t.Fatal("fresh catalog lease was not advertised")
	}
	ready = false
	if _, ok := module.readyCapabilities()["native_agent_turns"]; ok {
		t.Fatal("reconnect inherited an expired catalog lease")
	}
}

func TestPingProducesCorrelatedPong(t *testing.T) {
	if got := pongFrame(map[string]any{"type": "client.ping", "id": "heartbeat-7"}); got["type"] != "server.pong" || got["id"] != "heartbeat-7" {
		t.Fatalf("pong = %#v", got)
	}
	if got := pongFrame(map[string]any{"type": "client.ping"}); len(got) != 1 || got["type"] != "server.pong" {
		t.Fatalf("uncorrelated pong = %#v", got)
	}
}

func TestQueuedOutboundDrainsOneBoundedSnapshot(t *testing.T) {
	outbound := make(chan map[string]any, 4)
	for index := 1; index <= 3; index++ {
		outbound <- map[string]any{"index": index}
	}
	frames := queuedOutbound(outbound, 2)
	if len(frames) != 2 || frames[0]["index"] != 1 || frames[1]["index"] != 2 || len(outbound) != 1 {
		t.Fatalf("bounded outbound drain = %#v remaining=%d", frames, len(outbound))
	}
	frames = queuedOutbound(outbound, cap(outbound))
	if len(frames) != 1 || frames[0]["index"] != 3 || len(outbound) != 0 {
		t.Fatalf("remaining outbound drain = %#v remaining=%d", frames, len(outbound))
	}
}

func TestQueuedOutboundStopsWhenChannelIsClosed(t *testing.T) {
	outbound := make(chan map[string]any, 1)
	outbound <- map[string]any{"index": 1}
	close(outbound)
	frames := queuedOutbound(outbound, 4)
	if len(frames) != 1 || frames[0]["index"] != 1 {
		t.Fatalf("closed outbound drain = %#v", frames)
	}
}

func TestQueuedOutboundReleasesBlockedPong(t *testing.T) {
	connection := newConnection("session", Ticket{}, MaxInFlightRequests)
	for index := 0; index < cap(connection.outbound); index++ {
		connection.outbound <- map[string]any{"index": index}
	}
	done := make(chan error, 1)
	go func() {
		done <- connection.sendBlocking(context.Background(), map[string]any{"type": "server.pong"})
	}()

	frames := queuedOutbound(connection.outbound, cap(connection.outbound))
	if len(frames) != cap(connection.outbound) {
		t.Fatalf("drained frames = %d, want %d", len(frames), cap(connection.outbound))
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send pong: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pong remained blocked after outbound drain")
	}
	if frame := <-connection.outbound; frame["type"] != "server.pong" {
		t.Fatalf("outbound frame = %#v", frame)
	}
}
