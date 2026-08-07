package projector

import (
	"context"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	channelsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/channels"
)

// EventSink is the durable product-event outbox used by projections.
type EventSink interface {
	Append(context.Context, dirextalkdomain.Event) error
}

// ConversationPort owns durable conversation projection records.
type ConversationPort interface {
	GetRecord(context.Context, string, string) (dirextalkdomain.ConversationRecord, bool, error)
	Save(context.Context, dirextalkdomain.ConversationRecord) error
	SetCreator(context.Context, string, string) error
}

// ChannelPort distinguishes a state projection upsert from a workflow save.
// Profile projection already writes its conversation first, while invite
// projection uses SaveWithConversation to preserve the existing orchestration.
type ChannelPort interface {
	ByIDOrRoom(context.Context, string, string) (dirextalkdomain.Channel, bool, error)
	UpsertProjection(context.Context, dirextalkdomain.Channel) error
	SaveWithConversation(context.Context, dirextalkdomain.Channel) error
	Delete(context.Context, string) error
}

type ChannelContentPort interface {
	ProjectPost(context.Context, channelsmodule.ProjectionEvent) error
	ProjectPostSettings(context.Context, channelsmodule.ProjectionEvent) error
	ProjectComment(context.Context, channelsmodule.ProjectionEvent) error
	ProjectReaction(context.Context, channelsmodule.ProjectionEvent) error
	RemoveProjectedEvent(context.Context, string) (bool, error)
}

type GroupPort interface {
	ByRoom(context.Context, string) (dirextalkdomain.GroupRecord, bool, error)
	Save(context.Context, dirextalkdomain.GroupRecord) error
	Delete(context.Context, string) error
}

// ContactPort keeps the peer-scoped serialization boundary around contact
// reads and writes that can race with ProductCore contact workflows.
type ContactPort interface {
	WithPeer(string, func() error) error
	ListRaw(context.Context) ([]dirextalkdomain.ContactRecord, error)
	LookupByRoom(context.Context, string) (dirextalkdomain.ContactRecord, bool, error)
	Save(context.Context, dirextalkdomain.ContactRecord) error
	SaveProjectionIfCurrent(context.Context, dirextalkdomain.ContactRecord, dirextalkdomain.ContactRecord) (bool, error)
}

// MemberPort preserves the root member-save orchestration, including its
// per-member lock, stored-field merge, and channel/group count refresh.
type MemberPort interface {
	Lookup(context.Context, string, string) (dirextalkdomain.MemberRecord, bool, error)
	Save(context.Context, dirextalkdomain.MemberRecord) error
	SaveProjectionIfAbsent(context.Context, dirextalkdomain.MemberRecord) (bool, error)
	SaveProjectionIfCurrent(context.Context, dirextalkdomain.MemberRecord, dirextalkdomain.MemberRecord) (bool, error)
}

type BlockPort interface {
	Exists(context.Context, string, ...string) (bool, error)
}

type IdentitySnapshot struct {
	OwnerMXID        string
	OwnerDisplayName string
	OwnerAvatarURL   string
	AgentRoomID      string
}

type ReinviteDisposition uint8

const (
	// ReinviteRetained stops replacement-room processing. This includes the
	// no-Matrix-transport behavior of the current in-process service.
	ReinviteRetained ReinviteDisposition = iota
	ReinviteReplacementRequired
)

// DirectRoomPort classifies Matrix-specific join/invite outcomes while the
// projector owns the contact state transition that follows.
type DirectRoomPort interface {
	ReinviteAcceptedContact(context.Context, dirextalkdomain.ContactRecord, IdentitySnapshot) (ReinviteDisposition, error)
	JoinReplacementRoom(context.Context, string, IdentitySnapshot) (string, error)
}

type Dependencies struct {
	Events         EventSink
	Conversations  ConversationPort
	Channels       ChannelPort
	ChannelContent ChannelContentPort
	Groups         GroupPort
	Contacts       ContactPort
	Members        MemberPort
	Blocks         BlockPort
	DirectRooms    DirectRoomPort
	Identity       func() IdentitySnapshot
}
