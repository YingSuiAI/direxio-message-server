package storage

import (
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkplugin"
)

type portalState = dirextalkdomain.PortalState
type clientBuild = dirextalkdomain.ClientBuild
type readMarker = dirextalkdomain.ReadMarker
type channel = dirextalkdomain.Channel
type channelInviteGrant = dirextalkdomain.ChannelInviteGrant
type channelPostRecord = dirextalkdomain.ChannelPostRecord
type channelCommentRecord = dirextalkdomain.ChannelCommentRecord
type contactRecord = dirextalkdomain.ContactRecord
type groupRecord = dirextalkdomain.GroupRecord
type callRecord = dirextalkdomain.CallRecord
type favoriteRecord = dirextalkdomain.FavoriteRecord
type followRecord = dirextalkdomain.FollowRecord
type reactionRecord = dirextalkdomain.ReactionRecord
type memberRecord = dirextalkdomain.MemberRecord
type p2pEvent = dirextalkdomain.Event
type eventBounds = dirextalkdomain.EventBounds
type blockRecord = dirextalkdomain.BlockRecord
type reportRecord = dirextalkdomain.ReportRecord
type pluginInstance = dirextalkplugin.Instance
type pluginJob = dirextalkplugin.Job
type pluginSecret = dirextalkplugin.Secret
type conversationKind = dirextalkdomain.ConversationKind
type conversationRecord = dirextalkdomain.ConversationRecord

const (
	conversationKindDirect       = dirextalkdomain.ConversationKindDirect
	conversationKindGroup        = dirextalkdomain.ConversationKindGroup
	conversationKindChannel      = dirextalkdomain.ConversationKindChannel
	conversationKindSystem       = dirextalkdomain.ConversationKindSystem
	conversationLifecycleActive  = dirextalkdomain.ConversationLifecycleActive
	conversationLifecycleDeleted = dirextalkdomain.ConversationLifecycleDeleted
	conversationProjectionReady  = dirextalkdomain.ConversationProjectionReady
)

func normalizeConversationRecord(record conversationRecord) conversationRecord {
	return dirextalkdomain.NormalizeConversationRecord(record)
}

func normalizeMemberRecord(record memberRecord) memberRecord {
	record.Membership = dirextalkdomain.NormalizeMemberMembership(record.Membership)
	return record
}

func normalizeChannelPostRecord(record channelPostRecord) channelPostRecord {
	record.Visibility = dirextalkdomain.NormalizeChannelPostVisibility(record.Visibility)
	if !record.CommentsEnabledSet {
		record.CommentsEnabled = true
		record.CommentsEnabledSet = true
	}
	return record
}

func conversationIDForRoomID(roomID string) string {
	return dirextalkdomain.ConversationIDForRoomID(roomID)
}

func conversationFromContact(contact contactRecord) conversationRecord {
	return dirextalkdomain.ConversationFromContact(contact)
}

func conversationFromGroup(group groupRecord) conversationRecord {
	return dirextalkdomain.ConversationFromGroup(group)
}

func conversationFromChannel(ch channel) conversationRecord {
	return dirextalkdomain.ConversationFromChannel(ch)
}

func contactDeleted(status string) bool {
	return dirextalkdomain.ContactDeleted(status)
}

func fallbackString(value, fallback string) string {
	return dirextalkdomain.FallbackString(value, fallback)
}
