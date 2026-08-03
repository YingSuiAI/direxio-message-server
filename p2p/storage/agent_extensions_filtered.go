package storage

import (
	"context"
	"strings"

	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

// ListFiltered returns owner-scoped extension installations filtered by the
// candidate source and lifecycle state. Filtering is performed in PostgreSQL;
// the page cursor is the stable installation_id ordering key.
func (s *DatabaseStore) ListFiltered(ctx context.Context, owner string, limit int, token, source, state string) ([]ext.Installation, string, error) {
	if s == nil || s.db == nil || limit <= 0 || limit > 100 || strings.TrimSpace(owner) == "" {
		return nil, "", ext.ErrInvalid
	}
	if err := ext.ValidateListFilter(source, state); err != nil {
		return nil, "", err
	}
	token, source, state = strings.TrimSpace(token), strings.TrimSpace(source), strings.TrimSpace(state)
	rows, err := s.db.QueryContext(ctx, `SELECT installation_id FROM p2p_agent_extensions WHERE owner_id=$1 AND installation_id>$2 AND ($3='' OR candidate_json->>'source'=$3) AND ($4='' OR state=$4) ORDER BY installation_id LIMIT $5`, owner, token, source, state, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	ids := make([]string, 0, limit+1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(ids) > limit {
		next = ids[limit-1]
		ids = ids[:limit]
	}
	out := make([]ext.Installation, 0, len(ids))
	for _, id := range ids {
		i, err := s.Get(ctx, owner, id)
		if err != nil {
			return nil, "", err
		}
		out = append(out, i)
	}
	return out, next, nil
}

var _ ext.FilteredStore = (*DatabaseStore)(nil)
