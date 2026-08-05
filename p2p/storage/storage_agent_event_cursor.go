package storage

import (
	"context"
	"database/sql"
	"errors"
)

func (s *DatabaseStore) LoadAgentEventCursor(
	ctx context.Context,
	source string,
) (int64, error) {
	var cursor int64
	err := s.db.QueryRowContext(ctx, `
		SELECT after_seq
		FROM p2p_agent_event_cursors
		WHERE source=$1`, source,
	).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return cursor, err
}

func (s *DatabaseStore) SaveAgentEventCursor(
	ctx context.Context,
	source string,
	afterSeq int64,
) error {
	return s.writer.Do(s.db, nil, func(txn *sql.Tx) error {
		_, err := txn.ExecContext(ctx, `
			INSERT INTO p2p_agent_event_cursors (
			    source, after_seq, updated_at
			)
			VALUES ($1,$2,CURRENT_TIMESTAMP)
			ON CONFLICT (source) DO UPDATE
			SET after_seq=GREATEST(
			        p2p_agent_event_cursors.after_seq,
			        EXCLUDED.after_seq
			    ),
			    updated_at=CASE
			        WHEN EXCLUDED.after_seq >
			             p2p_agent_event_cursors.after_seq
			        THEN CURRENT_TIMESTAMP
			        ELSE p2p_agent_event_cursors.updated_at
			    END`,
			source,
			afterSeq,
		)
		return err
	})
}
