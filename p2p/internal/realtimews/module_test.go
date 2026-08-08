package realtimews

import (
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
