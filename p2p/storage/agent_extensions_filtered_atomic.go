package storage

import (
	"context"

	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

// ListFiltered keeps the atomic lifecycle adapter interchangeable with the
// underlying PostgreSQL extension store for action handlers.
func (s *AtomicExtensionStore) ListFiltered(ctx context.Context, owner string, limit int, token, source, state string) ([]ext.Installation, string, error) {
	if s == nil || s.DB == nil {
		return nil, "", ext.ErrInvalid
	}
	return s.DB.ListFiltered(ctx, owner, limit, token, source, state)
}

var _ ext.FilteredStore = (*AtomicExtensionStore)(nil)
