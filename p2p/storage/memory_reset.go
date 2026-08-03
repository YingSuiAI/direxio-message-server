package storage

import (
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/operations"
)

// ResetAccountState atomically clears the volatile product and portal state
// removed by the legacy no-database account-deletion path. Installed plugin
// state is intentionally retained to preserve that path's existing behavior.
// This optional capability is not part of the durable Store contract.
func (s *MemoryStore) ResetAccountState() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.portal = nil
	s.readMarks = make(map[string]readMarker)
	s.conversations = make(map[string]conversationRecord)
	s.channels = make(map[string]channel)
	s.inviteGrants = make(map[string]channelInviteGrant)
	s.posts = nil
	s.comments = nil
	s.contacts = make(map[string]contactRecord)
	s.blocks = make(map[string]blockRecord)
	s.groups = make(map[string]groupRecord)
	s.calls = make(map[string]callRecord)
	s.favorites = make(map[int64]favoriteRecord)
	s.follows = make(map[string]followRecord)
	s.reactions = make(map[string]reactionRecord)
	s.members = make(map[string]memberRecord)
	s.events = nil
	s.eventSeq = make(map[int64]struct{})
	s.eventDedupe = make(map[string]int64)
	s.reports = make(map[string]reportRecord)
	s.operations = make(map[string]operations.Record)
}
