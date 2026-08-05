// Package realtimews owns the ProductCore realtime WebSocket transport.
package realtimews

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/realtime"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	eventsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/events"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/httpapi"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/plugins"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	TicketTTL                = 120 * time.Second
	BatchLimit               = 100
	DefaultHeartbeatInterval = 25 * time.Second
)

// Ticket is the authenticated owner snapshot bound to a one-use upgrade token.
type Ticket struct {
	Role       string
	UserID     string
	DeviceID   string
	Generation uint64
	ExpiresAt  time.Time
}

// ActionPort invokes an authenticated ProductCore action.
type ActionPort interface {
	Handle(context.Context, Ticket, string, map[string]any) (any, *actionbase.Error)
}

// EventPort exposes the durable ProductCore event stream.
type EventPort interface {
	CursorStatus(context.Context, int64) (eventsmodule.CursorStatus, error)
	List(context.Context, int64, int) ([]dirextalkdomain.Event, error)
	Waiter() <-chan struct{}
}

// SessionPort tracks live client state shared with push suppression.
type SessionPort interface {
	Upsert(string, realtime.SessionState)
	Update(string, func(*realtime.SessionState))
	Remove(string)
	ShouldSuppressPush(string, string, time.Time) bool
}

// PluginStreamPort validates and invokes official plugin streams.
type PluginStreamPort interface {
	PrepareStream(context.Context, map[string]any) (plugins.PreparedStream, *actionbase.Error)
	RunStream(context.Context, plugins.PreparedStream, func(plugins.StreamEvent) error) error
}

// AgentStreamPort invokes Native Agent streaming actions.
type AgentStreamPort interface {
	Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error
}

type DurableAgentStreamPort interface {
	DurableStream(context.Context, string, string, map[string]any, func(agentstream.StreamEvent) error) error
}

type Dependencies struct {
	Actions  ActionPort
	Events   EventPort
	Sessions SessionPort
	Plugins  PluginStreamPort
	Agent    AgentStreamPort
	// TicketActive fences a consumed ticket against root account lifecycle
	// changes after the module releases its ticket lock.
	TicketActive func(Ticket) bool
}

type Config struct {
	Now               func() time.Time
	NewToken          func(string) string
	HeartbeatInterval time.Duration
}

type Module struct {
	actions      ActionPort
	events       EventPort
	sessions     SessionPort
	plugins      PluginStreamPort
	agent        AgentStreamPort
	ticketActive func(Ticket) bool
	now          func() time.Time
	newToken     func(string) string
	heartbeat    time.Duration

	ticketMu sync.Mutex
	tickets  map[string]Ticket
}

func New(deps Dependencies, cfg Config) *Module {
	heartbeat := cfg.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeatInterval
	}
	return &Module{
		actions:      deps.Actions,
		events:       deps.Events,
		sessions:     deps.Sessions,
		plugins:      deps.Plugins,
		agent:        deps.Agent,
		ticketActive: deps.TicketActive,
		now:          cfg.Now,
		newToken:     cfg.NewToken,
		heartbeat:    heartbeat,
		tickets:      map[string]Ticket{},
	}
}

// Handler returns the realtime WebSocket HTTP upgrade handler.
func (m *Module) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpapi.SetCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
		if _, err := m.LookupTicket(ticket); err != nil {
			httpapi.WriteError(w, actionbase.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN"))
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		// Native Agent image turns can be up to 8 MiB decoded (~11 MiB base64)
		// plus JSON framing. Keep a bounded frame ceiling for all realtime traffic.
		conn.SetReadLimit(16 << 20)
		record, err := m.ConsumeTicket(ticket)
		if err != nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "M_UNKNOWN_TOKEN")
			return
		}
		defer conn.Close(websocket.StatusInternalError, "connection closed")

		sessionID := m.token("ws_session")
		m.upsertSession(sessionID, realtime.SessionState{
			UserID:   record.UserID,
			Role:     record.Role,
			LastSeen: m.timeNow(),
		})
		defer m.removeSession(sessionID)

		ctx := r.Context()
		hello, err := readHello(ctx, conn)
		if err != nil {
			_ = wsjson.Write(ctx, conn, map[string]any{
				"type":  "server.error",
				"error": err.Error(),
			})
			return
		}
		since := actionbase.Int64(hello["since"])
		if since < 0 {
			since = 0
		}
		m.touchSession(sessionID)
		if err := wsjson.Write(ctx, conn, map[string]any{
			"type":                  "server.ready",
			"role":                  record.Role,
			"heartbeat_interval_ms": int64(m.heartbeat / time.Millisecond),
			"capabilities": map[string]any{
				"native_agent_turns": 1,
			},
		}); err != nil {
			return
		}

		client := newConnection(sessionID, record)
		defer client.cancelAllStreams()
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			m.readFrames(ctx, conn, client)
		}()
		m.streamEvents(ctx, conn, record.Role, since, readDone, client.outbound)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}

func readHello(ctx context.Context, conn *websocket.Conn) (map[string]any, error) {
	var frame map[string]any
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		return nil, err
	}
	if actionbase.String(frame["type"]) != "client.hello" {
		return nil, errors.New("client.hello is required")
	}
	return frame, nil
}

// IssueTicket stores a new one-use ticket for an already authenticated owner.
func (m *Module) IssueTicket(record Ticket) map[string]any {
	ticket := m.token("ws_ticket")
	record.ExpiresAt = m.timeNow().Add(TicketTTL)
	m.ticketMu.Lock()
	if m.tickets == nil {
		m.tickets = map[string]Ticket{}
	}
	m.tickets[ticket] = record
	m.ticketMu.Unlock()
	return map[string]any{
		"ticket":        ticket,
		"expires_in_ms": int64(TicketTTL / time.Millisecond),
	}
}

func (m *Module) LookupTicket(ticket string) (Ticket, error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return Ticket{}, errors.New("ticket is required")
	}
	if m == nil {
		return Ticket{}, errors.New("ticket invalid")
	}
	m.ticketMu.Lock()
	defer m.ticketMu.Unlock()
	record, ok := m.tickets[ticket]
	if !ok {
		return Ticket{}, errors.New("ticket invalid")
	}
	if m.timeNow().After(record.ExpiresAt) {
		return Ticket{}, errors.New("ticket expired")
	}
	return record, nil
}

func (m *Module) ConsumeTicket(ticket string) (Ticket, error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return Ticket{}, errors.New("ticket is required")
	}
	if m == nil {
		return Ticket{}, errors.New("ticket invalid")
	}
	m.ticketMu.Lock()
	record, ok := m.tickets[ticket]
	if !ok {
		m.ticketMu.Unlock()
		return Ticket{}, errors.New("ticket invalid")
	}
	delete(m.tickets, ticket)
	if m.timeNow().After(record.ExpiresAt) {
		m.ticketMu.Unlock()
		return Ticket{}, errors.New("ticket expired")
	}
	m.ticketMu.Unlock()
	if m.ticketActive != nil && !m.ticketActive(record) {
		return Ticket{}, errors.New("ticket invalid")
	}
	return record, nil
}

func (m *Module) ResetTickets() {
	if m == nil {
		return
	}
	m.ticketMu.Lock()
	m.tickets = map[string]Ticket{}
	m.ticketMu.Unlock()
}

func (m *Module) ShouldSuppressPush(userID, roomID string) bool {
	roomID = strings.TrimSpace(roomID)
	if m == nil || m.sessions == nil || roomID == "" {
		return false
	}
	return m.sessions.ShouldSuppressPush(userID, roomID, m.timeNow())
}

func (m *Module) upsertSession(sessionID string, state realtime.SessionState) {
	if m != nil && m.sessions != nil {
		m.sessions.Upsert(sessionID, state)
	}
}

func (m *Module) updateSession(sessionID string, update func(*realtime.SessionState)) {
	if m != nil && m.sessions != nil {
		m.sessions.Update(sessionID, update)
	}
}

func (m *Module) touchSession(sessionID string) {
	m.updateSession(sessionID, func(*realtime.SessionState) {})
}

func (m *Module) removeSession(sessionID string) {
	if m != nil && m.sessions != nil {
		m.sessions.Remove(sessionID)
	}
}

func (m *Module) timeNow() time.Time {
	if m != nil && m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

func (m *Module) token(prefix string) string {
	if m != nil && m.newToken != nil {
		return m.newToken(prefix)
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}
