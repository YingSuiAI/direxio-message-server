package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

// Get/Put/Replay methods make DatabaseStore an extensions.Store.  The
// extension package remains independent of storage, while this adapter keeps
// all versions immutable and all replay keys owner-scoped.
func (s *DatabaseStore) Get(ctx context.Context, owner, id string) (ext.Installation, error) {
	r, err := s.GetAgentExtension(ctx, owner, id)
	if err != nil {
		return ext.Installation{}, ext.ErrNotFound
	}
	var i ext.Installation
	if json.Unmarshal(r.CandidateJSON, &i.Candidate) != nil {
		return ext.Installation{}, ext.ErrConflict
	}
	i.ID, i.OwnerID, i.State, i.Revision, i.ActiveVersionID, i.CreatedAt, i.UpdatedAt = r.InstallationID, r.OwnerID, r.State, r.Revision, r.ActiveVersionID, r.CreatedAt, r.UpdatedAt
	rows, err := s.db.QueryContext(ctx, `SELECT version_id,pin_json,content_digest,manifest_digest,execution_digest,network_digest,secret_digest,execution_json,network_grants_json,secret_grants_json,tools_json,created_at FROM p2p_agent_extension_versions WHERE owner_id=$1 AND installation_id=$2 ORDER BY created_at,version_id`, owner, id)
	if err != nil {
		return ext.Installation{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var v ext.Version
		var pin, execution, network, secrets, tools []byte
		if err := rows.Scan(&v.VersionID, &pin, &v.ContentDigest, &v.ManifestDigest, &v.ExecutionDigest, &v.NetworkDigest, &v.SecretDigest, &execution, &network, &secrets, &tools, &v.CreatedAt); err != nil {
			return ext.Installation{}, err
		}
		if json.Unmarshal(pin, &v.Pin) != nil || json.Unmarshal(execution, &v.Execution) != nil || json.Unmarshal(network, &v.NetworkGrants) != nil || json.Unmarshal(secrets, &v.SecretGrants) != nil || json.Unmarshal(tools, &v.Tools) != nil {
			return ext.Installation{}, ext.ErrConflict
		}
		i.Versions = append(i.Versions, v)
	}
	return i, rows.Err()
}

func (s *DatabaseStore) Put(ctx context.Context, i ext.Installation) error {
	if i.Validate() != nil {
		return ext.ErrInvalid
	}
	candidate, _ := json.Marshal(i.Candidate)
	now := i.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		return s.putAgentExtensionTx(ctx, tx, i, candidate, now)
	})
}

func (s *DatabaseStore) putAgentExtensionTx(ctx context.Context, tx *sql.Tx, i ext.Installation, candidate []byte, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO p2p_agent_extensions(owner_id,installation_id,candidate_json,state,revision,active_version_id,created_at,updated_at) VALUES($1,$2,$3::jsonb,$4,$5,$6,$7,$8) ON CONFLICT(owner_id,installation_id) DO UPDATE SET candidate_json=EXCLUDED.candidate_json,state=EXCLUDED.state,revision=EXCLUDED.revision,active_version_id=EXCLUDED.active_version_id,updated_at=EXCLUDED.updated_at`, i.OwnerID, i.ID, string(candidate), i.State, i.Revision, i.ActiveVersionID, i.CreatedAt, now)
	if err != nil {
		return err
	}
	for _, v := range i.Versions {
		pin, _ := json.Marshal(v.Pin)
		execution, _ := json.Marshal(v.Execution)
		network, _ := json.Marshal(v.NetworkGrants)
		secrets, _ := json.Marshal(v.SecretGrants)
		tools, _ := json.Marshal(v.Tools)
		var old string
		er := tx.QueryRowContext(ctx, `SELECT content_digest FROM p2p_agent_extension_versions WHERE owner_id=$1 AND installation_id=$2 AND version_id=$3`, i.OwnerID, i.ID, v.VersionID).Scan(&old)
		if er == nil && old != v.ContentDigest {
			return ext.ErrConflict
		}
		if er != nil && er != sql.ErrNoRows {
			return er
		}
		if _, er = tx.ExecContext(ctx, `INSERT INTO p2p_agent_extension_versions(owner_id,installation_id,version_id,pin_json,content_digest,manifest_digest,execution_digest,network_digest,secret_digest,execution_json,network_grants_json,secret_grants_json,tools_json,created_at) VALUES($1,$2,$3,$4::jsonb,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb,$12::jsonb,$13::jsonb,$14) ON CONFLICT(owner_id,installation_id,version_id) DO NOTHING`, i.OwnerID, i.ID, v.VersionID, string(pin), v.ContentDigest, v.ManifestDigest, v.ExecutionDigest, v.NetworkDigest, v.SecretDigest, string(execution), string(network), string(secrets), string(tools), v.CreatedAt); er != nil {
			return er
		}
	}
	return nil
}

func (s *DatabaseStore) PutCAS(ctx context.Context, i ext.Installation, expected int64) error {
	if expected <= 0 {
		return s.Put(ctx, i)
	}
	if i.Validate() != nil {
		return ext.ErrInvalid
	}
	candidate, _ := json.Marshal(i.Candidate)
	now := i.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var current int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM p2p_agent_extensions WHERE owner_id=$1 AND installation_id=$2 FOR UPDATE`, i.OwnerID, i.ID).Scan(&current)
		if err == sql.ErrNoRows || current != expected {
			return ext.ErrRevisionConflict
		}
		if err != nil {
			return err
		}
		return s.putAgentExtensionTx(ctx, tx, i, candidate, now)
	})
}

func (s *DatabaseStore) Replay(ctx context.Context, owner, key, digest string) (ext.LifecycleResult, bool, error) {
	var raw, old string
	err := s.db.QueryRowContext(ctx, `SELECT request_digest,response_json FROM p2p_agent_extension_replays WHERE owner_id=$1 AND idempotency_key=$2`, owner, key).Scan(&old, &raw)
	if err == sql.ErrNoRows {
		return ext.LifecycleResult{}, false, nil
	}
	if err != nil {
		return ext.LifecycleResult{}, false, err
	}
	if old != digest {
		return ext.LifecycleResult{}, true, ext.ErrIdempotencyConflict
	}
	var out ext.LifecycleResult
	if json.Unmarshal([]byte(raw), &out) != nil {
		return ext.LifecycleResult{}, true, ext.ErrConflict
	}
	return out, true, nil
}
func (s *DatabaseStore) SaveReplay(ctx context.Context, owner, key, digest string, result ext.LifecycleResult) error {
	raw, e := json.Marshal(result)
	if e != nil {
		return e
	}
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var old string
		er := tx.QueryRowContext(ctx, `SELECT request_digest FROM p2p_agent_extension_replays WHERE owner_id=$1 AND idempotency_key=$2 FOR UPDATE`, owner, key).Scan(&old)
		if er == nil {
			if old != digest {
				return ext.ErrIdempotencyConflict
			}
			return nil
		}
		if er != sql.ErrNoRows {
			return er
		}
		_, er = tx.ExecContext(ctx, `INSERT INTO p2p_agent_extension_replays(owner_id,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4::jsonb,$5)`, owner, key, digest, string(raw), time.Now().UTC())
		return er
	})
}

var _ ext.Store = (*DatabaseStore)(nil)

func (s *DatabaseStore) List(ctx context.Context, owner string, limit int, token string) ([]ext.Installation, string, error) {
	if limit <= 0 || limit > 100 || owner == "" {
		return nil, "", ext.ErrInvalid
	}
	token = strings.TrimSpace(token)
	rows, err := s.db.QueryContext(ctx, `SELECT installation_id FROM p2p_agent_extensions WHERE owner_id=$1 AND installation_id>$2 ORDER BY installation_id LIMIT $3`, owner, token, limit+1)
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
		i, e := s.Get(ctx, owner, id)
		if e != nil {
			return nil, "", e
		}
		out = append(out, i)
	}
	return out, next, nil
}

type AgentExtensionExecutionReceipt struct {
	OwnerID, TaskID, InstallationID, VersionID     string
	RequestDigest, ResultDigest, ErrorCode, Status string
	Attempt                                        uint32
	Uncertain                                      bool
	CreatedAt, UpdatedAt                           time.Time
}

// PutAgentExtensionExecutionReceipt is an idempotent execution fence. A
// different request digest for the same owner/task is never overwritten.
func (s *DatabaseStore) PutAgentExtensionExecutionReceipt(ctx context.Context, r AgentExtensionExecutionReceipt) error {
	if r.OwnerID == "" || r.TaskID == "" || r.InstallationID == "" || r.VersionID == "" || r.RequestDigest == "" {
		return ext.ErrInvalid
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var old string
		err := tx.QueryRowContext(ctx, `SELECT request_digest FROM p2p_agent_extension_execution_receipts WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, r.OwnerID, r.TaskID).Scan(&old)
		if err == nil {
			if old != r.RequestDigest {
				return ext.ErrIdempotencyConflict
			}
			_, err = tx.ExecContext(ctx, `UPDATE p2p_agent_extension_execution_receipts SET status=$3,result_digest=$4,error_code=$5,uncertain=$6,attempt=$7,updated_at=$8 WHERE owner_id=$1 AND task_id=$2`, r.OwnerID, r.TaskID, r.Status, r.ResultDigest, r.ErrorCode, r.Uncertain, r.Attempt, r.UpdatedAt)
			return err
		}
		if err != sql.ErrNoRows {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_extension_execution_receipts(owner_id,task_id,installation_id,version_id,request_digest,status,result_digest,error_code,uncertain,attempt,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, r.OwnerID, r.TaskID, r.InstallationID, r.VersionID, r.RequestDigest, r.Status, r.ResultDigest, r.ErrorCode, r.Uncertain, r.Attempt, r.CreatedAt, r.UpdatedAt)
		return err
	})
}

func (s *DatabaseStore) MarkAgentExtensionExecutionUncertain(ctx context.Context, owner, taskID, code string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE p2p_agent_extension_execution_receipts SET uncertain=TRUE,status='uncertain',error_code=$3,updated_at=$4 WHERE owner_id=$1 AND task_id=$2`, owner, taskID, code, time.Now().UTC())
	return err
}
