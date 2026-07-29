package storage

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

var ErrAgentSecretRotation = errors.New("agent secret rotation failed")

const (
	defaultAgentSecretRotationBatchSize = 100
	defaultAgentSecretRotationLeaseTTL  = 30 * time.Second
)

// AgentSecretRotationOptions contains locations and operational fencing only.
// Key material is always read from KeyringFile and is never accepted inline.
type AgentSecretRotationOptions struct {
	KeyringFile               string
	LegacyModelProfileKeyFile string
	LeaseOwner                string
	BatchSize                 int
	LeaseTTL                  time.Duration
	Now                       func() time.Time
}

type agentSecretRotation struct {
	ID, ActiveKeyID, LeaseOwner string
	OldKeyIDs                   []string
	LeaseEpoch                  int64
}

type genericSecretRotationRow struct {
	Domain, OwnerID, EntityID, Purpose, Reference, KeyID string
	Revision                                             int64
	BindingDigest, Nonce, Ciphertext                     []byte
	EnvelopeVersion, AADVersion                          int
}

type modelSecretRotationRow struct {
	OwnerID, ProfileID, Provider, KeyID string
	ProfileRevision, CredentialVersion  int64
	Nonce, Ciphertext                   []byte
}

func (o AgentSecretRotationOptions) normalized() (AgentSecretRotationOptions, error) {
	o.KeyringFile = strings.TrimSpace(o.KeyringFile)
	o.LegacyModelProfileKeyFile = strings.TrimSpace(o.LegacyModelProfileKeyFile)
	o.LeaseOwner = strings.TrimSpace(o.LeaseOwner)
	if o.KeyringFile == "" {
		return AgentSecretRotationOptions{}, ErrAgentSecretRotation
	}
	if o.LeaseOwner == "" {
		o.LeaseOwner = uuid.NewString()
	}
	if o.BatchSize == 0 {
		o.BatchSize = defaultAgentSecretRotationBatchSize
	}
	if o.BatchSize < 1 || o.BatchSize > 1000 {
		return AgentSecretRotationOptions{}, ErrAgentSecretRotation
	}
	if o.LeaseTTL == 0 {
		o.LeaseTTL = defaultAgentSecretRotationLeaseTTL
	}
	if o.LeaseTTL < 5*time.Second || o.LeaseTTL > 5*time.Minute {
		return AgentSecretRotationOptions{}, ErrAgentSecretRotation
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o, nil
}

// VerifyAgentSecretDatabase validates all generic and model-profile envelopes.
// Legacy model rows are accepted only when the configured legacy key can open
// them. Unknown keys, malformed bindings and authentication failures fail the
// complete verification.
func VerifyAgentSecretDatabase(ctx context.Context, db *sql.DB, options AgentSecretRotationOptions) error {
	if db == nil {
		return ErrAgentSecretRotation
	}
	options, err := options.normalized()
	if err != nil {
		return err
	}
	keyring, err := LoadAgentSecretKeyring(options.KeyringFile)
	if err != nil {
		return ErrAgentSecretRotation
	}
	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		return ErrAgentSecretRotation
	}
	legacyKey, err := loadLegacyModelProfileKey(options.LegacyModelProfileKeyFile)
	if err != nil {
		return err
	}
	if err := verifyGenericSecretRows(ctx, db, enveloper); err != nil {
		return ErrAgentSecretRotation
	}
	if err := verifyCurrentModelSecretRows(ctx, db, enveloper, legacyKey); err != nil {
		return ErrAgentSecretRotation
	}
	if err := verifyHistoricalModelSecretRows(ctx, db, enveloper, legacyKey); err != nil {
		return ErrAgentSecretRotation
	}
	return nil
}

// RotateAgentSecrets performs a resumable offline rotation. The service must
// be stopped while it runs. A crash after activating the new key is safe:
// decrypt-only keys remain in the keyring and the live database rotation row
// causes the next invocation to resume the same rewrap.
func RotateAgentSecrets(ctx context.Context, db *sql.DB, options AgentSecretRotationOptions) error {
	if db == nil {
		return ErrAgentSecretRotation
	}
	options, err := options.normalized()
	if err != nil {
		return err
	}
	// Fence before changing the on-disk keyring. A running server keeps the
	// shared side of this lock for its lifetime, so rotation cannot leave it
	// with a keyring/row generation it did not start with.
	maintenanceGuard, err := acquireAgentSecretMaintenanceGuard(ctx, db)
	if err != nil {
		return ErrAgentSecretRotation
	}
	defer releaseAgentSecretMaintenanceGuard(maintenanceGuard)
	file, keyring, oldKeyIDs, err := prepareAgentSecretKeyringRotation(options.KeyringFile)
	if err != nil || len(oldKeyIDs) == 0 {
		return ErrAgentSecretRotation
	}
	rotation, err := claimAgentSecretRotation(ctx, db, options, file.ActiveKeyID, oldKeyIDs)
	if err != nil {
		return ErrAgentSecretRotation
	}
	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		return ErrAgentSecretRotation
	}
	legacyKey, err := loadLegacyModelProfileKey(options.LegacyModelProfileKeyFile)
	if err != nil {
		return err
	}
	for {
		var processed int
		count, batchErr := rewrapGenericSecretBatch(ctx, db, options, rotation, enveloper)
		if batchErr != nil {
			_ = failAgentSecretRotation(ctx, db, rotation, options, "generic_rewrap_failed")
			return ErrAgentSecretRotation
		}
		processed += count
		count, batchErr = rewrapCurrentModelSecretBatch(ctx, db, options, rotation, enveloper, legacyKey)
		if batchErr != nil {
			_ = failAgentSecretRotation(ctx, db, rotation, options, "model_current_rewrap_failed")
			return ErrAgentSecretRotation
		}
		processed += count
		count, batchErr = rewrapHistoricalModelSecretBatch(ctx, db, options, rotation, enveloper, legacyKey)
		if batchErr != nil {
			_ = failAgentSecretRotation(ctx, db, rotation, options, "model_history_rewrap_failed")
			return ErrAgentSecretRotation
		}
		processed += count
		if processed == 0 {
			break
		}
	}
	if err := setAgentSecretRotationState(ctx, db, rotation, options, "verifying"); err != nil {
		return ErrAgentSecretRotation
	}
	if err := VerifyAgentSecretDatabase(ctx, db, options); err != nil {
		_ = failAgentSecretRotation(ctx, db, rotation, options, "verification_failed")
		return ErrAgentSecretRotation
	}
	if err := verifyAgentSecretKeysUnused(ctx, db, rotation.OldKeyIDs); err != nil {
		_ = failAgentSecretRotation(ctx, db, rotation, options, "old_key_still_used")
		return ErrAgentSecretRotation
	}
	if err := completeAgentSecretRotation(ctx, db, rotation, options); err != nil {
		return ErrAgentSecretRotation
	}
	if err := retireAgentSecretKeys(options.KeyringFile, rotation.ActiveKeyID, rotation.OldKeyIDs); err != nil {
		// Database rows are already safe under the new active key. Keeping old
		// decrypt-only keys is a recoverable state and the next run can retire
		// them after another full verification.
		return ErrAgentSecretRotation
	}
	return nil
}

func prepareAgentSecretKeyringRotation(path string) (agentSecretKeyringFile, *AgentSecretKeyring, []string, error) {
	file, err := readAgentSecretKeyringFile(path)
	if err != nil {
		return agentSecretKeyringFile{}, nil, nil, err
	}
	old := decryptOnlyAgentSecretKeyIDs(file)
	if len(old) == 0 {
		key := make([]byte, 32)
		id := make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return agentSecretKeyringFile{}, nil, nil, ErrAgentSecretRotation
		}
		if _, err := io.ReadFull(rand.Reader, id); err != nil {
			clear(key)
			return agentSecretKeyringFile{}, nil, nil, ErrAgentSecretRotation
		}
		for index := range file.Keys {
			file.Keys[index].DecryptOnly = true
		}
		file.ActiveKeyID = hex.EncodeToString(id)
		file.Keys = append(file.Keys, agentSecretKeyRecord{
			ID:  file.ActiveKeyID,
			Key: base64.RawStdEncoding.EncodeToString(key),
		})
		clear(key)
		if err := writeAgentSecretKeyringFileAtomic(path, file); err != nil {
			return agentSecretKeyringFile{}, nil, nil, err
		}
		old = decryptOnlyAgentSecretKeyIDs(file)
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return agentSecretKeyringFile{}, nil, nil, ErrAgentSecretRotation
	}
	keyring, err := parseAgentSecretKeyring(raw)
	if err != nil {
		return agentSecretKeyringFile{}, nil, nil, ErrAgentSecretRotation
	}
	return file, keyring, old, nil
}

func readAgentSecretKeyringFile(path string) (agentSecretKeyringFile, error) {
	if !agentSecretPrivatePath(path) {
		return agentSecretKeyringFile{}, ErrAgentSecretRotation
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return agentSecretKeyringFile{}, ErrAgentSecretRotation
	}
	var file agentSecretKeyringFile
	if json.Unmarshal(raw, &file) != nil {
		return agentSecretKeyringFile{}, ErrAgentSecretRotation
	}
	if _, err := parseAgentSecretKeyring(raw); err != nil {
		return agentSecretKeyringFile{}, ErrAgentSecretRotation
	}
	return file, nil
}

func decryptOnlyAgentSecretKeyIDs(file agentSecretKeyringFile) []string {
	out := make([]string, 0, len(file.Keys))
	for _, record := range file.Keys {
		if record.DecryptOnly {
			out = append(out, record.ID)
		}
	}
	sort.Strings(out)
	return out
}

func writeAgentSecretKeyringFileAtomic(path string, file agentSecretKeyringFile) error {
	raw, err := json.Marshal(file)
	if err != nil {
		return ErrAgentSecretRotation
	}
	if _, err := parseAgentSecretKeyring(raw); err != nil {
		return ErrAgentSecretRotation
	}
	dir := filepath.Dir(path)
	if !agentSecretPrivateDir(dir) {
		return ErrAgentSecretRotation
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return ErrAgentSecretRotation
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrAgentSecretRotation
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".rotate-"+uuid.NewString())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrAgentSecretRotation
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return ErrAgentSecretRotation
	}
	if _, err := f.Write(raw); err != nil {
		return ErrAgentSecretRotation
	}
	if err := f.Sync(); err != nil {
		return ErrAgentSecretRotation
	}
	if err := f.Close(); err != nil {
		return ErrAgentSecretRotation
	}
	if err := os.Rename(tmp, path); err != nil {
		return ErrAgentSecretRotation
	}
	ok = true
	d, err := os.Open(dir)
	if err != nil {
		return ErrAgentSecretRotation
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return ErrAgentSecretRotation
	}
	if err := d.Close(); err != nil {
		return ErrAgentSecretRotation
	}
	return nil
}

func retireAgentSecretKeys(path, active string, old []string) error {
	file, err := readAgentSecretKeyringFile(path)
	if err != nil || file.ActiveKeyID != active {
		return ErrAgentSecretRotation
	}
	remove := make(map[string]struct{}, len(old))
	for _, id := range old {
		remove[id] = struct{}{}
	}
	keys := make([]agentSecretKeyRecord, 0, len(file.Keys))
	for _, record := range file.Keys {
		if _, found := remove[record.ID]; found {
			if !record.DecryptOnly || record.ID == active {
				return ErrAgentSecretRotation
			}
			continue
		}
		keys = append(keys, record)
	}
	file.Keys = keys
	return writeAgentSecretKeyringFileAtomic(path, file)
}

func claimAgentSecretRotation(ctx context.Context, db *sql.DB, options AgentSecretRotationOptions, active string, old []string) (agentSecretRotation, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return agentSecretRotation{}, err
	}
	defer tx.Rollback()
	var out agentSecretRotation
	var state, leaseOwner string
	var leaseUntil sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT rotation_id::text,from_key_ids,to_key_id,state,lease_owner,lease_epoch,lease_expires_at
		FROM p2p_agent_secret_rotations
		WHERE state IN ('rewrapping','verifying')
		ORDER BY created_at
		LIMIT 1
		FOR UPDATE`).Scan(&out.ID, pq.Array(&out.OldKeyIDs), &out.ActiveKeyID, &state, &leaseOwner, &out.LeaseEpoch, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		out = agentSecretRotation{ID: uuid.NewString(), ActiveKeyID: active, OldKeyIDs: append([]string(nil), old...), LeaseOwner: options.LeaseOwner}
		_, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_secret_rotations(
			rotation_id,state,from_key_ids,to_key_id,lease_owner,lease_epoch,lease_expires_at,created_at,updated_at
		) VALUES($1,'rewrapping',$2,$3,$4,1,$5,$6,$6)`,
			out.ID, pq.Array(out.OldKeyIDs), active, options.LeaseOwner, options.Now().UTC().Add(options.LeaseTTL), options.Now().UTC())
		out.LeaseEpoch = 1
	} else if err == nil {
		sort.Strings(out.OldKeyIDs)
		expected := append([]string(nil), old...)
		sort.Strings(expected)
		if out.ActiveKeyID != active || !equalAgentSecretKeyIDs(out.OldKeyIDs, expected) {
			return agentSecretRotation{}, ErrAgentSecretRotation
		}
		now := options.Now().UTC()
		if leaseUntil.Valid && leaseUntil.Time.After(now) && leaseOwner != "" && leaseOwner != options.LeaseOwner {
			return agentSecretRotation{}, ErrAgentSecretRotation
		}
		err = tx.QueryRowContext(ctx, `UPDATE p2p_agent_secret_rotations
			SET state='rewrapping',lease_owner=$2,lease_epoch=lease_epoch+1,lease_expires_at=$3,error_code='',updated_at=$4
			WHERE rotation_id=$1
			RETURNING lease_epoch`, out.ID, options.LeaseOwner, now.Add(options.LeaseTTL), now).Scan(&out.LeaseEpoch)
		out.LeaseOwner = options.LeaseOwner
	}
	if err != nil {
		return agentSecretRotation{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentSecretRotation{}, err
	}
	return out, nil
}

func equalAgentSecretKeyIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func lockAgentSecretRotationTx(ctx context.Context, tx *sql.Tx, rotation agentSecretRotation, options AgentSecretRotationOptions) error {
	now := options.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE p2p_agent_secret_rotations
		SET lease_expires_at=$5,updated_at=$4
		WHERE rotation_id=$1 AND lease_owner=$2 AND lease_epoch=$3
			AND state='rewrapping' AND lease_expires_at>$4`,
		rotation.ID, rotation.LeaseOwner, rotation.LeaseEpoch, now, now.Add(options.LeaseTTL))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrAgentSecretRotation
	}
	return nil
}

func recordAgentSecretRotationBatchTx(ctx context.Context, tx *sql.Tx, rotation agentSecretRotation, options AgentSecretRotationOptions, count int, domain, owner, entity string, revision int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE p2p_agent_secret_rotations
		SET rewrapped_rows=rewrapped_rows+$4,cursor_domain=$5,cursor_owner_id=$6,cursor_entity_id=$7,cursor_revision=$8,
			lease_expires_at=$9,updated_at=$10
		WHERE rotation_id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND state='rewrapping'`,
		rotation.ID, rotation.LeaseOwner, rotation.LeaseEpoch, count, domain, owner, entity, revision,
		options.Now().UTC().Add(options.LeaseTTL), options.Now().UTC())
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrAgentSecretRotation
	}
	return nil
}

func rewrapGenericSecretBatch(ctx context.Context, db *sql.DB, options AgentSecretRotationOptions, rotation agentSecretRotation, enveloper *AgentSecretEnveloper) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockAgentSecretRotationTx(ctx, tx, rotation, options); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT secret_domain,owner_id,entity_id,secret_revision,purpose,reference,
			binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext
		FROM p2p_agent_secrets
		WHERE key_id=ANY($1)
		ORDER BY secret_domain,owner_id,entity_id,secret_revision,purpose,reference
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, pq.Array(rotation.OldKeyIDs), options.BatchSize)
	if err != nil {
		return 0, err
	}
	items := make([]genericSecretRotationRow, 0, options.BatchSize)
	for rows.Next() {
		var item genericSecretRotationRow
		if err := rows.Scan(&item.Domain, &item.OwnerID, &item.EntityID, &item.Revision, &item.Purpose, &item.Reference,
			&item.BindingDigest, &item.EnvelopeVersion, &item.AADVersion, &item.KeyID, &item.Nonce, &item.Ciphertext); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, item := range items {
		if item.EnvelopeVersion != 1 || item.AADVersion != 1 || len(item.BindingDigest) != sha256.Size {
			return 0, ErrAgentSecretRotation
		}
		var digest [sha256.Size]byte
		copy(digest[:], item.BindingDigest)
		binding := AgentSecretBinding{Domain: item.Domain, OwnerID: item.OwnerID, EntityID: item.EntityID, Revision: item.Revision, Purpose: item.Purpose, Reference: item.Reference, BindingDigest: digest}
		plaintext, err := enveloper.Open(binding, AgentSecretEnvelope{KeyID: item.KeyID, Nonce: item.Nonce, Ciphertext: item.Ciphertext})
		if err != nil {
			return 0, err
		}
		sealed, err := enveloper.Seal(binding, plaintext)
		clear(plaintext)
		if err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE p2p_agent_secrets
			SET key_id=$7,nonce=$8,ciphertext=$9
			WHERE secret_domain=$1 AND owner_id=$2 AND entity_id=$3 AND secret_revision=$4 AND purpose=$5 AND reference=$6
				AND key_id=$10 AND nonce=$11 AND ciphertext=$12`,
			item.Domain, item.OwnerID, item.EntityID, item.Revision, item.Purpose, item.Reference,
			sealed.KeyID, sealed.Nonce, sealed.Ciphertext, item.KeyID, item.Nonce, item.Ciphertext)
		if err != nil {
			return 0, err
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return 0, ErrAgentSecretRotation
		}
		if err := upsertAgentSecretUsageTx(ctx, tx, item.Domain, item.OwnerID, item.EntityID, item.Revision, item.Purpose, item.Reference, sealed); err != nil {
			return 0, err
		}
	}
	last := genericSecretRotationRow{}
	if len(items) > 0 {
		last = items[len(items)-1]
	}
	if err := recordAgentSecretRotationBatchTx(ctx, tx, rotation, options, len(items), last.Domain, last.OwnerID, last.EntityID, last.Revision); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(items), nil
}

func rewrapCurrentModelSecretBatch(ctx context.Context, db *sql.DB, options AgentSecretRotationOptions, rotation agentSecretRotation, enveloper *AgentSecretEnveloper, legacyKey []byte) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockAgentSecretRotationTx(ctx, tx, rotation, options); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT owner_id,profile_id,provider,
			CASE WHEN api_key_profile_revision>0 THEN api_key_profile_revision ELSE revision END,
			credential_version,api_key_key_id,api_key_nonce,api_key_ciphertext
		FROM p2p_agent_model_profiles
		WHERE api_key_ciphertext<>''::bytea AND (api_key_key_id='' OR api_key_key_id=ANY($1))
		ORDER BY owner_id,profile_id
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, pq.Array(rotation.OldKeyIDs), options.BatchSize)
	if err != nil {
		return 0, err
	}
	items, err := scanModelSecretRotationRows(rows)
	if err != nil {
		return 0, err
	}
	for _, item := range items {
		plaintext, err := openModelRotationSecret(enveloper, legacyKey, item)
		if err != nil {
			return 0, err
		}
		sealed, err := SealModelProfileCredential(enveloper, item.OwnerID, item.ProfileID, item.Provider, item.ProfileRevision, item.CredentialVersion, plaintext)
		clear(plaintext)
		if err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE p2p_agent_model_profiles
			SET api_key_version=2,api_key_key_id=$3,api_key_profile_revision=$4,api_key_nonce=$5,api_key_ciphertext=$6
			WHERE owner_id=$1 AND profile_id=$2 AND api_key_key_id=$7 AND api_key_nonce=$8 AND api_key_ciphertext=$9`,
			item.OwnerID, item.ProfileID, sealed.KeyID, item.ProfileRevision, sealed.Nonce, sealed.Ciphertext,
			item.KeyID, item.Nonce, item.Ciphertext)
		if err != nil {
			return 0, err
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return 0, ErrAgentSecretRotation
		}
		if err := upsertAgentSecretUsageTx(ctx, tx, "model_profile.current", item.OwnerID, item.ProfileID, item.ProfileRevision, "model_profile_credential", item.Provider, sealed.AgentSecretEnvelope); err != nil {
			return 0, err
		}
	}
	last := modelSecretRotationRow{}
	if len(items) > 0 {
		last = items[len(items)-1]
	}
	if err := recordAgentSecretRotationBatchTx(ctx, tx, rotation, options, len(items), "model_profile.current", last.OwnerID, last.ProfileID, last.ProfileRevision); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(items), nil
}

func rewrapHistoricalModelSecretBatch(ctx context.Context, db *sql.DB, options AgentSecretRotationOptions, rotation agentSecretRotation, enveloper *AgentSecretEnveloper, legacyKey []byte) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockAgentSecretRotationTx(ctx, tx, rotation, options); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.owner_id,c.profile_id,c.provider,
			COALESCE(NULLIF(c.profile_revision,0),(
				SELECT MIN(r.profile_revision)
				FROM p2p_agent_model_profile_revisions r
				WHERE r.owner_id=c.owner_id AND r.profile_id=c.profile_id AND r.credential_version=c.credential_version
			),1),
			c.credential_version,c.api_key_key_id,c.api_key_nonce,c.api_key_ciphertext
		FROM p2p_agent_model_profile_credentials c
		WHERE c.api_key_ciphertext<>''::bytea AND (c.api_key_key_id='' OR c.api_key_key_id=ANY($1))
		ORDER BY c.owner_id,c.profile_id,c.credential_version
		LIMIT $2
		FOR UPDATE OF c SKIP LOCKED`, pq.Array(rotation.OldKeyIDs), options.BatchSize)
	if err != nil {
		return 0, err
	}
	items, err := scanModelSecretRotationRows(rows)
	if err != nil {
		return 0, err
	}
	for _, item := range items {
		plaintext, err := openModelRotationSecret(enveloper, legacyKey, item)
		if err != nil {
			return 0, err
		}
		sealed, err := SealModelProfileCredential(enveloper, item.OwnerID, item.ProfileID, item.Provider, item.ProfileRevision, item.CredentialVersion, plaintext)
		clear(plaintext)
		if err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE p2p_agent_model_profile_credentials
			SET api_key_key_id=$4,profile_revision=$5,api_key_nonce=$6,api_key_ciphertext=$7
			WHERE owner_id=$1 AND profile_id=$2 AND credential_version=$3
				AND api_key_key_id=$8 AND api_key_nonce=$9 AND api_key_ciphertext=$10`,
			item.OwnerID, item.ProfileID, item.CredentialVersion, sealed.KeyID, item.ProfileRevision, sealed.Nonce, sealed.Ciphertext,
			item.KeyID, item.Nonce, item.Ciphertext)
		if err != nil {
			return 0, err
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return 0, ErrAgentSecretRotation
		}
		if err := upsertAgentSecretUsageTx(ctx, tx, "model_profile.history", item.OwnerID, item.ProfileID, item.ProfileRevision, "model_profile_credential", item.Provider, sealed.AgentSecretEnvelope); err != nil {
			return 0, err
		}
	}
	last := modelSecretRotationRow{}
	if len(items) > 0 {
		last = items[len(items)-1]
	}
	if err := recordAgentSecretRotationBatchTx(ctx, tx, rotation, options, len(items), "model_profile.history", last.OwnerID, last.ProfileID, last.ProfileRevision); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(items), nil
}

func scanModelSecretRotationRows(rows *sql.Rows) ([]modelSecretRotationRow, error) {
	defer rows.Close()
	items := []modelSecretRotationRow{}
	for rows.Next() {
		var item modelSecretRotationRow
		if err := rows.Scan(&item.OwnerID, &item.ProfileID, &item.Provider, &item.ProfileRevision,
			&item.CredentialVersion, &item.KeyID, &item.Nonce, &item.Ciphertext); err != nil {
			return nil, err
		}
		if item.ProfileRevision < 1 || item.CredentialVersion < 1 {
			return nil, ErrAgentSecretRotation
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func openModelRotationSecret(enveloper *AgentSecretEnveloper, legacyKey []byte, item modelSecretRotationRow) ([]byte, error) {
	if item.KeyID != "" {
		return OpenModelProfileCredential(enveloper, item.OwnerID, item.ProfileID, item.Provider, item.ProfileRevision,
			ModelProfileCredentialEnvelope{
				AgentSecretEnvelope: AgentSecretEnvelope{KeyID: item.KeyID, Nonce: item.Nonce, Ciphertext: item.Ciphertext},
				CredentialVersion:   item.CredentialVersion,
			}, nil)
	}
	return openLegacyModelProfileCredential(legacyKey, item.ProfileID, item.Provider, item.Nonce, item.Ciphertext)
}

func loadLegacyModelProfileKey(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, ErrAgentSecretRotation
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, ErrAgentSecretRotation
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != 32 {
		return nil, ErrAgentSecretRotation
	}
	return key, nil
}

func openLegacyModelProfileCredential(key []byte, profileID, provider string, nonce, ciphertext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrAgentSecretRotation
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrAgentSecretRotation
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != aead.NonceSize() || len(ciphertext) < aead.Overhead() {
		return nil, ErrAgentSecretRotation
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(profileID+"\x00"+provider))
	if err != nil {
		return nil, ErrAgentSecretRotation
	}
	return plaintext, nil
}

func verifyGenericSecretRows(ctx context.Context, db *sql.DB, enveloper *AgentSecretEnveloper) error {
	rows, err := db.QueryContext(ctx, `SELECT secret_domain,owner_id,entity_id,secret_revision,purpose,reference,
		binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext
		FROM p2p_agent_secrets
		ORDER BY secret_domain,owner_id,entity_id,secret_revision,purpose,reference`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item genericSecretRotationRow
		if err := rows.Scan(&item.Domain, &item.OwnerID, &item.EntityID, &item.Revision, &item.Purpose, &item.Reference,
			&item.BindingDigest, &item.EnvelopeVersion, &item.AADVersion, &item.KeyID, &item.Nonce, &item.Ciphertext); err != nil {
			return err
		}
		if item.EnvelopeVersion != 1 || item.AADVersion != 1 || len(item.BindingDigest) != sha256.Size {
			return ErrAgentSecretRotation
		}
		var digest [sha256.Size]byte
		copy(digest[:], item.BindingDigest)
		plaintext, err := enveloper.Open(
			AgentSecretBinding{Domain: item.Domain, OwnerID: item.OwnerID, EntityID: item.EntityID, Revision: item.Revision, Purpose: item.Purpose, Reference: item.Reference, BindingDigest: digest},
			AgentSecretEnvelope{KeyID: item.KeyID, Nonce: item.Nonce, Ciphertext: item.Ciphertext},
		)
		clear(plaintext)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

func verifyCurrentModelSecretRows(ctx context.Context, db *sql.DB, enveloper *AgentSecretEnveloper, legacyKey []byte) error {
	rows, err := db.QueryContext(ctx, `SELECT owner_id,profile_id,provider,
		CASE WHEN api_key_profile_revision>0 THEN api_key_profile_revision ELSE revision END,
		credential_version,api_key_key_id,api_key_nonce,api_key_ciphertext
		FROM p2p_agent_model_profiles
		WHERE api_key_ciphertext<>''::bytea
		ORDER BY owner_id,profile_id`)
	if err != nil {
		return err
	}
	items, err := scanModelSecretRotationRows(rows)
	if err != nil {
		return err
	}
	for _, item := range items {
		plaintext, err := openModelRotationSecret(enveloper, legacyKey, item)
		clear(plaintext)
		if err != nil {
			return err
		}
	}
	return nil
}

func verifyHistoricalModelSecretRows(ctx context.Context, db *sql.DB, enveloper *AgentSecretEnveloper, legacyKey []byte) error {
	rows, err := db.QueryContext(ctx, `SELECT c.owner_id,c.profile_id,c.provider,
		COALESCE(NULLIF(c.profile_revision,0),(
			SELECT MIN(r.profile_revision)
			FROM p2p_agent_model_profile_revisions r
			WHERE r.owner_id=c.owner_id AND r.profile_id=c.profile_id AND r.credential_version=c.credential_version
		),1),
		c.credential_version,c.api_key_key_id,c.api_key_nonce,c.api_key_ciphertext
		FROM p2p_agent_model_profile_credentials c
		ORDER BY c.owner_id,c.profile_id,c.credential_version`)
	if err != nil {
		return err
	}
	items, err := scanModelSecretRotationRows(rows)
	if err != nil {
		return err
	}
	for _, item := range items {
		plaintext, err := openModelRotationSecret(enveloper, legacyKey, item)
		clear(plaintext)
		if err != nil {
			return err
		}
	}
	return nil
}

func verifyAgentSecretKeysUnused(ctx context.Context, db *sql.DB, old []string) error {
	var count int64
	for _, query := range []string{
		`SELECT COUNT(*) FROM p2p_agent_secrets WHERE key_id=ANY($1)`,
		`SELECT COUNT(*) FROM p2p_agent_model_profiles WHERE api_key_ciphertext<>''::bytea AND (api_key_key_id='' OR api_key_key_id=ANY($1))`,
		`SELECT COUNT(*) FROM p2p_agent_model_profile_credentials WHERE api_key_ciphertext<>''::bytea AND (api_key_key_id='' OR api_key_key_id=ANY($1))`,
		`SELECT COUNT(*) FROM p2p_agent_secret_key_usage WHERE key_id=ANY($1)`,
	} {
		if err := db.QueryRowContext(ctx, query, pq.Array(old)).Scan(&count); err != nil || count != 0 {
			return ErrAgentSecretRotation
		}
	}
	return nil
}

func setAgentSecretRotationState(ctx context.Context, db *sql.DB, rotation agentSecretRotation, options AgentSecretRotationOptions, state string) error {
	result, err := db.ExecContext(ctx, `UPDATE p2p_agent_secret_rotations
		SET state=$4,lease_expires_at=$5,updated_at=$6
		WHERE rotation_id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND state='rewrapping'`,
		rotation.ID, rotation.LeaseOwner, rotation.LeaseEpoch, state, options.Now().UTC().Add(options.LeaseTTL), options.Now().UTC())
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrAgentSecretRotation
	}
	return nil
}

func completeAgentSecretRotation(ctx context.Context, db *sql.DB, rotation agentSecretRotation, options AgentSecretRotationOptions) error {
	now := options.Now().UTC()
	result, err := db.ExecContext(ctx, `UPDATE p2p_agent_secret_rotations
		SET state='complete',lease_owner='',lease_expires_at=NULL,verified_rows=rewrapped_rows,error_code='',updated_at=$4,completed_at=$4
		WHERE rotation_id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND state='verifying'`,
		rotation.ID, rotation.LeaseOwner, rotation.LeaseEpoch, now)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrAgentSecretRotation
	}
	return nil
}

func failAgentSecretRotation(ctx context.Context, db *sql.DB, rotation agentSecretRotation, options AgentSecretRotationOptions, code string) error {
	_, err := db.ExecContext(ctx, `UPDATE p2p_agent_secret_rotations
		SET state='failed',lease_owner='',lease_expires_at=NULL,error_code=$4,updated_at=$5
		WHERE rotation_id=$1 AND lease_owner=$2 AND lease_epoch=$3`,
		rotation.ID, rotation.LeaseOwner, rotation.LeaseEpoch, code, options.Now().UTC())
	return err
}

func upsertAgentSecretUsageTx(ctx context.Context, tx *sql.Tx, domain, owner, entity string, revision int64, purpose, reference string, envelope AgentSecretEnvelope) error {
	digest := agentSecretEnvelopeDigest(envelope)
	_, err := tx.ExecContext(ctx, `INSERT INTO p2p_agent_secret_key_usage(
		secret_domain,owner_id,entity_id,secret_revision,purpose,reference,key_id,envelope_digest,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NOW())
	ON CONFLICT(secret_domain,owner_id,entity_id,secret_revision,purpose,reference)
	DO UPDATE SET key_id=EXCLUDED.key_id,envelope_digest=EXCLUDED.envelope_digest,updated_at=EXCLUDED.updated_at`,
		domain, owner, entity, revision, purpose, reference, envelope.KeyID, digest[:])
	return err
}

func agentSecretEnvelopeDigest(envelope AgentSecretEnvelope) [sha256.Size]byte {
	var data bytes.Buffer
	data.WriteString(envelope.KeyID)
	data.WriteByte(0)
	data.Write(envelope.Nonce)
	data.Write(envelope.Ciphertext)
	return sha256.Sum256(data.Bytes())
}
