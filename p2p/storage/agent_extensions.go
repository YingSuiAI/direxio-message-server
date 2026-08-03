package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// AgentExtensionDDL is consumed by the direct-final v78 migration. Every
// immutable digest is a column; secret material is intentionally absent.
const AgentExtensionDDL = `
CREATE TABLE IF NOT EXISTS p2p_agent_extensions (
 owner_id TEXT NOT NULL, installation_id TEXT NOT NULL, candidate_json JSONB NOT NULL,
 state TEXT NOT NULL, revision BIGINT NOT NULL, active_version_id TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(owner_id, installation_id)
);
CREATE TABLE IF NOT EXISTS p2p_agent_extension_versions (
 owner_id TEXT NOT NULL, installation_id TEXT NOT NULL, version_id TEXT NOT NULL,
 pin_json JSONB NOT NULL, content_digest TEXT NOT NULL, manifest_digest TEXT NOT NULL,
 execution_digest TEXT NOT NULL, network_digest TEXT NOT NULL, secret_digest TEXT NOT NULL,
 execution_json JSONB NOT NULL, network_grants_json JSONB NOT NULL, secret_grants_json JSONB NOT NULL,
 tools_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(owner_id, installation_id, version_id),
 FOREIGN KEY(owner_id, installation_id) REFERENCES p2p_agent_extensions(owner_id, installation_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS p2p_agent_extension_replays (
 owner_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, request_digest TEXT NOT NULL,
 response_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(owner_id, idempotency_key)
);
CREATE TABLE IF NOT EXISTS p2p_agent_extension_execution_receipts (
 owner_id TEXT NOT NULL, task_id UUID NOT NULL, installation_id TEXT NOT NULL,
 version_id TEXT NOT NULL, request_digest TEXT NOT NULL, status TEXT NOT NULL,
 result_digest TEXT NOT NULL DEFAULT '', error_code TEXT NOT NULL DEFAULT '',
 uncertain BOOLEAN NOT NULL DEFAULT FALSE, attempt INTEGER NOT NULL DEFAULT 1,
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(owner_id, task_id),
 FOREIGN KEY(owner_id, installation_id, version_id) REFERENCES p2p_agent_extension_versions(owner_id, installation_id, version_id),
 FOREIGN KEY(owner_id, task_id) REFERENCES agent_tasks(owner_id, task_id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS p2p_agent_extensions_owner_updated ON p2p_agent_extensions(owner_id, updated_at DESC);
`

type AgentExtensionRecord struct {
	OwnerID, InstallationID, State, ActiveVersionID string
	CandidateJSON, VersionsJSON                     []byte
	Revision                                        int64
	CreatedAt, UpdatedAt                            time.Time
}

var ErrAgentExtensionNotFound = errors.New("agent extension not found")

func (s *DatabaseStore) PutAgentExtension(ctx context.Context, r AgentExtensionRecord) error {
	if s == nil || s.db == nil || strings.TrimSpace(r.OwnerID) == "" || strings.TrimSpace(r.InstallationID) == "" || len(r.CandidateJSON) == 0 {
		return errors.New("invalid agent extension")
	}
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO p2p_agent_extensions(owner_id,installation_id,candidate_json,state,revision,active_version_id,created_at,updated_at) VALUES($1,$2,$3::jsonb,$4,$5,$6,$7,$8) ON CONFLICT(owner_id,installation_id) DO UPDATE SET candidate_json=EXCLUDED.candidate_json,state=EXCLUDED.state,revision=EXCLUDED.revision,active_version_id=EXCLUDED.active_version_id,updated_at=EXCLUDED.updated_at`, r.OwnerID, r.InstallationID, string(r.CandidateJSON), r.State, r.Revision, r.ActiveVersionID, r.CreatedAt, r.UpdatedAt)
		return e
	})
}
func (s *DatabaseStore) GetAgentExtension(ctx context.Context, owner, id string) (AgentExtensionRecord, error) {
	var r AgentExtensionRecord
	if s == nil || s.db == nil {
		return r, ErrAgentExtensionNotFound
	}
	e := s.db.QueryRowContext(ctx, `SELECT owner_id,installation_id,candidate_json,state,revision,active_version_id,created_at,updated_at FROM p2p_agent_extensions WHERE owner_id=$1 AND installation_id=$2`, owner, id).Scan(&r.OwnerID, &r.InstallationID, &r.CandidateJSON, &r.State, &r.Revision, &r.ActiveVersionID, &r.CreatedAt, &r.UpdatedAt)
	if e == sql.ErrNoRows {
		return r, ErrAgentExtensionNotFound
	}
	return r, e
}
func (s *DatabaseStore) PutAgentExtensionReplay(ctx context.Context, owner, key, digest string, response any) error {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(key) == "" || strings.TrimSpace(digest) == "" {
		return errors.New("invalid replay")
	}
	b, e := json.Marshal(response)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO p2p_agent_extension_replays(owner_id,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4::jsonb,$5) ON CONFLICT(owner_id,idempotency_key) DO UPDATE SET request_digest=EXCLUDED.request_digest,response_json=EXCLUDED.response_json`, owner, key, digest, string(b), time.Now().UTC())
	return e
}
