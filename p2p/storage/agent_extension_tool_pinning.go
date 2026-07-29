package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

func clonePinnedTools(tools []ext.Tool) []ext.Tool {
	out := make([]ext.Tool, len(tools))
	for n, tool := range tools {
		out[n] = tool
		out[n].InputSchema = append([]byte(nil), tool.InputSchema...)
	}
	return out
}

func validatePinRequest(ctx context.Context, owner, installationID, versionID string, expectedRevision int64, tools []ext.Tool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(installationID) == "" || strings.TrimSpace(versionID) == "" || expectedRevision <= 0 {
		return ext.ErrInvalid
	}
	return ext.ValidatePinnedTools(tools)
}

// PinTools is the PostgreSQL implementation of ToolPinner. Both installation
// and version rows are locked in one transaction; the final update retains an
// empty-value CAS so a future schema change cannot silently replace a pin.
func (s *DatabaseStore) PinTools(ctx context.Context, owner, installationID, versionID string, expectedRevision int64, tools []ext.Tool) ([]ext.Tool, error) {
	if err := validatePinRequest(ctx, owner, installationID, versionID, expectedRevision, tools); err != nil {
		return nil, err
	}
	if s == nil || s.db == nil || s.writer == nil {
		return nil, ext.ErrUnavailable
	}
	var pinned []ext.Tool
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var revision int64
		var state, active string
		err := tx.QueryRowContext(ctx, `SELECT revision,state,active_version_id FROM p2p_agent_extensions WHERE owner_id=$1 AND installation_id=$2 FOR UPDATE`, owner, installationID).Scan(&revision, &state, &active)
		if err == sql.ErrNoRows {
			return ext.ErrNotFound
		}
		if err != nil {
			return err
		}
		if revision != expectedRevision {
			return ext.ErrRevisionConflict
		}
		if state != "installed" || active != versionID {
			return ext.ErrConflict
		}

		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT tools_json FROM p2p_agent_extension_versions WHERE owner_id=$1 AND installation_id=$2 AND version_id=$3 FOR UPDATE`, owner, installationID, versionID).Scan(&raw)
		if err == sql.ErrNoRows {
			return ext.ErrNotFound
		}
		if err != nil {
			return err
		}
		var existing []ext.Tool
		if len(raw) > 0 && string(raw) != "null" {
			if json.Unmarshal(raw, &existing) != nil {
				return ext.ErrConflict
			}
		}
		if len(existing) > 0 {
			if ext.ValidatePinnedTools(existing) != nil {
				return ext.ErrConflict
			}
			if !ext.PinnedToolsEqual(existing, tools) {
				return ext.ErrConflict
			}
			pinned = clonePinnedTools(existing)
			return nil
		}

		encoded, marshalErr := json.Marshal(tools)
		if marshalErr != nil {
			return ext.ErrInvalid
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE p2p_agent_extension_versions SET tools_json=$1::jsonb WHERE owner_id=$2 AND installation_id=$3 AND version_id=$4 AND (tools_json IS NULL OR tools_json='null'::jsonb OR tools_json='[]'::jsonb)`, string(encoded), owner, installationID, versionID)
		if updateErr != nil {
			return updateErr
		}
		n, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if n != 1 {
			return ext.ErrConflict
		}
		pinned = clonePinnedTools(tools)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pinned, nil
}

// AtomicExtensionStore is the runtime Store used by the embedded MCP action
// port. Keep pinning on this wrapper so callers need only assert extensions.ToolPinner.
func (s *AtomicExtensionStore) PinTools(ctx context.Context, owner, installationID, versionID string, expectedRevision int64, tools []ext.Tool) ([]ext.Tool, error) {
	if s == nil || s.DB == nil {
		return nil, ext.ErrUnavailable
	}
	return s.DB.PinTools(ctx, owner, installationID, versionID, expectedRevision, tools)
}

var (
	_ ext.ToolPinner = (*DatabaseStore)(nil)
	_ ext.ToolPinner = (*AtomicExtensionStore)(nil)
)
