// Package agent owns the Message Server account controls needed by the
// separately addressed Native Agent data plane.
package agent

import (
	"crypto/ed25519"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

type Config struct {
	Account           AccountPort
	OwnerID           func() string
	AccountGeneration int64
	TicketPrivateKey  []byte
	Now               func() time.Time
}

type Module struct {
	account           AccountPort
	ownerID           func() string
	accountGeneration int64
	ticketPrivateKey  ed25519.PrivateKey
	now               func() time.Time
}

func New(cfg Config) *Module {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Module{
		account:           cfg.Account,
		ownerID:           cfg.OwnerID,
		accountGeneration: cfg.AccountGeneration,
		ticketPrivateKey:  append(ed25519.PrivateKey(nil), cfg.TicketPrivateKey...),
		now:               now,
	}
}

func (m *Module) Handlers() map[string]actionbase.Handler {
	if m == nil {
		return nil
	}
	return map[string]actionbase.Handler{
		actionPassword:            m.accountPassword,
		actionMatrixSessionCreate: m.createMatrixSession,
		actionSessionCreate:       m.createAgentSession,
		actionConfigGet:           m.getAccountConfig,
		actionConfigUpdate:        m.updateAccountConfig,
	}
}

func (m *Module) currentOwnerID() string {
	if m != nil && m.ownerID != nil {
		return strings.TrimSpace(m.ownerID())
	}
	return "owner"
}
