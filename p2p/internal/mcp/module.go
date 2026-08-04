// Package mcp adapts Dirextalk product state and Matrix transport to the
// shared internal/dirextalkmcp capability service.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmatrix"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	channelsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/channels"
	contactsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/contacts"
	conversationmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/conversation"
	groupsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/groups"
	socialmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/social"
)

const (
	agentGatewayContentKey       = "io.dirextalk.agent_gateway"
	agentGatewaySourceContentKey = "io.dirextalk.gateway_source"
)

// MemberStore is the durable member read boundary required by MCP discovery.
type MemberStore interface {
	ListMembers(context.Context, string, string) ([]dirextalkdomain.MemberRecord, error)
}

// MatrixPort is the narrow Matrix read/write boundary required by MCP tools.
type MatrixPort interface {
	SendMessage(context.Context, dirextalktransport.SendMessageRequest) (dirextalktransport.SendMessageResult, error)
	ListRoomMembers(context.Context, string) ([]dirextalktransport.RoomMember, error)
}

// ProfileResolver resolves Matrix profile fallback data for MCP DTOs.
type ProfileResolver interface {
	ResolveMatrixProfile(context.Context, string) (dirextalkmatrix.Profile, error)
}

// Identity is the current owner/Agent state needed by MCP capabilities.
type Identity struct {
	OwnerMXID        string
	OwnerProfile     dirextalkdomain.OwnerProfile
	AgentMXID        string
	AgentDisplayName string
	AgentRoomID      string
	BlockedRoomIDs   []string
}

// Dependencies are the owning product modules and repositories adapted by MCP.
type Dependencies struct {
	Conversations  *conversationmodule.Module
	Contacts       *contactsmodule.Module
	Channels       *channelsmodule.Module
	ChannelContent *channelsmodule.ContentModule
	Groups         *groupsmodule.Module
	Members        MemberStore
	Social         *socialmodule.Module
	Matrix         MatrixPort
}

// Config contains dynamic service state and lifecycle callbacks. Dynamic
// readers are used because Matrix readers and profiles are wired after Service
// construction.
type Config struct {
	Identity              func() Identity
	MessageReader         func() dirextalkmcp.MessageReader
	ProfileResolver       func() ProfileResolver
	ResolveRoomOwner      func(context.Context, string) (string, error)
	BeginAccountOperation func(context.Context) (context.Context, func())
	AccountDeprovisioned  func() bool
	AgentRoomName         string
	PreparedMatrixStore   dirextalktransport.PreparedMatrixMutationStore
	Now                   func() time.Time
}

// Module implements all P2P-backed Dirextalk MCP capabilities.
type Module struct {
	conversations *conversationmodule.Module
	contacts      *contactsmodule.Module
	channels      *channelsmodule.Module
	content       *channelsmodule.ContentModule
	groups        *groupsmodule.Module
	members       MemberStore
	social        *socialmodule.Module
	matrix        MatrixPort
	preparedStore dirextalktransport.PreparedMatrixMutationStore
	config        Config
	service       *dirextalkmcp.Service
}

func New(deps Dependencies, cfg Config) *Module {
	module := &Module{
		conversations: deps.Conversations,
		contacts:      deps.Contacts,
		channels:      deps.Channels,
		content:       deps.ChannelContent,
		groups:        deps.Groups,
		members:       deps.Members,
		social:        deps.Social,
		matrix:        deps.Matrix,
		preparedStore: cfg.PreparedMatrixStore,
		config:        cfg,
	}
	module.service = dirextalkmcp.NewServiceWithConfig(dirextalkmcp.Config{
		Invoker:        module,
		RoomAuthorizer: module,
	})
	return module
}

// DurableMatrixMutationReady reports whether the optional crash-safe write
// port is wired. Product Capability registration uses this to omit write
// descriptors instead of advertising an unsafe SendMessage fallback.
func (m *Module) DurableMatrixMutationReady() bool {
	if m == nil || m.preparedStore == nil || m.matrix == nil {
		return false
	}
	_, ok := m.matrix.(dirextalktransport.PreparedMessagePort)
	return ok
}

// Service returns the shared registry/schema/invocation service used by both
// Native Agent tools and the standard Streamable HTTP endpoint.
func (m *Module) Service() *dirextalkmcp.Service {
	if m == nil || m.service == nil {
		return dirextalkmcp.NewService(nil)
	}
	return m.service
}

func (m *Module) InvokeCapability(ctx context.Context, action string, params map[string]any) (any, *dirextalkmcp.Error) {
	if m == nil {
		return nil, dirextalkmcp.StatusError(http.StatusInternalServerError, "Dirextalk MCP capability service is unavailable")
	}
	finish := func() {}
	if m.config.BeginAccountOperation != nil {
		ctx, finish = m.config.BeginAccountOperation(ctx)
	}
	defer finish()
	if m.config.AccountDeprovisioned != nil && m.config.AccountDeprovisioned() {
		return nil, dirextalkmcp.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	}

	switch action {
	case dirextalkmcp.ActionRoomsSearch:
		return m.roomsSearch(ctx, params)
	case dirextalkmcp.ActionContactsList:
		return m.contactsList(ctx, params)
	case dirextalkmcp.ActionContactsSearch:
		return m.contactsSearch(ctx, params)
	case dirextalkmcp.ActionMessagesSend:
		return m.messagesSend(ctx, params)
	case dirextalkmcp.ActionMessagesList:
		return m.messagesList(ctx, params)
	case dirextalkmcp.ActionRoomMembersList:
		return m.roomMembersList(ctx, params)
	case dirextalkmcp.ActionChannelPostsList:
		return m.channelPostsList(ctx, params)
	case dirextalkmcp.ActionChannelCommentsList:
		return m.channelCommentsList(ctx, params)
	case dirextalkmcp.ActionChannelCommentsCreate:
		return m.channelCommentCreate(ctx, params)
	default:
		return nil, dirextalkmcp.BadRequest("unknown MCP action")
	}
}

func (m *Module) MCPRoomBlocked(roomID string) bool {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return false
	}
	identity := m.identity()
	for _, blockedRoomID := range identity.BlockedRoomIDs {
		if roomID == strings.TrimSpace(blockedRoomID) {
			return true
		}
	}
	return false
}

func (m *Module) identity() Identity {
	if m == nil || m.config.Identity == nil {
		return Identity{}
	}
	return m.config.Identity()
}

func (m *Module) requireRoomAllowed(roomID string) *dirextalkmcp.Error {
	return m.Service().RequireRoomAllowed(roomID)
}

func (m *Module) messageReader() dirextalkmcp.MessageReader {
	if m == nil || m.config.MessageReader == nil {
		return nil
	}
	return m.config.MessageReader()
}

func (m *Module) profileResolver() ProfileResolver {
	if m == nil || m.config.ProfileResolver == nil {
		return nil
	}
	return m.config.ProfileResolver()
}

func (m *Module) now() time.Time {
	if m != nil && m.config.Now != nil {
		return m.config.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Module) agentRoomName() string {
	if name := strings.TrimSpace(m.config.AgentRoomName); name != "" {
		return name
	}
	return "Agents"
}

func internalError(err error) *dirextalkmcp.Error {
	if err == nil {
		return nil
	}
	return dirextalkmcp.StatusError(http.StatusInternalServerError, fmt.Sprintf("internal error: %s", err))
}

func transportWriteError(err error) *dirextalkmcp.Error {
	if err == nil {
		return nil
	}
	var policyErr *productpolicy.PolicyError
	if errors.As(err, &policyErr) {
		status := policyErr.Code
		if status <= 0 {
			status = http.StatusForbidden
		}
		return dirextalkmcp.StatusError(status, policyErr.Message)
	}
	return internalError(err)
}

func actionError(err *actionbase.Error) *dirextalkmcp.Error {
	if err == nil {
		return nil
	}
	return dirextalkmcp.StatusError(err.Status, err.Error)
}

func fallback(value, fallbackValue string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallbackValue
}
