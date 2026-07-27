package storage

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/google/uuid"
)

var (
	ErrModelProfileNotFound       = errors.New("model profile not found")
	ErrModelProfileRevision       = errors.New("model profile revision conflict")
	ErrModelProfileInvalid        = errors.New("invalid model profile")
	ErrModelProfileKeyUnavailable = errors.New("model profile key unavailable")
	ErrModelProfileIdempotency    = errors.New("model profile idempotency conflict")
)

const defaultModelProfilePageSize = 25

// ModelProfile is the server-owned, redacted profile projection. APIKey is
// populated only for internal model construction and is never serialized by
// ProductCore action handlers.
type ModelProfile struct {
	ProfileID, ClientProfileID string
	DisplayName, Provider      string
	BaseURL, Model             string
	SystemPrompt               string
	APIKey                     string `json:"-"`
	APIKeyConfigured           bool
	Temperature, TopP          *float64
	MaxOutputTokens            int
	ContextWindow              int
	ReasoningEffort            string
	Revision                   int64
	CredentialVersion          int64
	Deleted                    bool
	CreatedAt, UpdatedAt       time.Time
}

type ModelProfileSyncEntry struct {
	ClientProfileID                                     string
	ExpectedRevision                                    *int64
	DisplayName, Provider, BaseURL, Model, SystemPrompt string
	APIKey                                              *string
	Temperature, TopP                                   *float64
	MaxOutputTokens, ContextWindow                      int
	ReasoningEffort                                     string
}

type ModelProfileSyncResult struct {
	Profiles               []ModelProfile
	DefaultClientProfileID string
}

type ModelProfileListResult struct {
	Profiles               []ModelProfile
	NextPageToken          string
	DefaultClientProfileID string
}

type ModelProfileStore interface {
	SyncModelProfiles(context.Context, string, string, string, []ModelProfileSyncEntry) (ModelProfileSyncResult, error)
	ListModelProfiles(context.Context, string, int, string) (ModelProfileListResult, error)
	GetModelProfile(context.Context, string, string) (ModelProfile, bool, error)
	DeleteModelProfile(context.Context, string, string, string, *int64) error
	ResolveModelProfile(context.Context, string, string) (ModelProfile, error)
	ResolveModelProfileVersion(context.Context, string, string, int64) (ModelProfile, error)
	ResolveModelProfilePinned(context.Context, string, string, int64, int64) (ModelProfile, error)
	ModelProfileStoreReady() bool
}

type encryptedModelProfileStore struct {
	db     *sql.DB
	writer sqlutil.Writer
	key    []byte
	ready  bool
	now    func() time.Time
}

func NewEncryptedModelProfileStore(ctx context.Context, db *sql.DB, writer sqlutil.Writer, keyFile string) (ModelProfileStore, error) {
	if db == nil {
		return nil, fmt.Errorf("model profile database is unavailable")
	}
	rows, err := db.QueryContext(ctx, `SELECT COUNT(*) FROM p2p_agent_model_profiles WHERE octet_length(api_key_ciphertext) > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var encryptedCount int64
	if !rows.Next() || rows.Scan(&encryptedCount) != nil {
		return nil, fmt.Errorf("model profile key state is unavailable")
	}
	key, err := loadOrCreateModelProfileKey(keyFile, encryptedCount > 0)
	if err != nil {
		return nil, err
	}
	store := &encryptedModelProfileStore{db: db, writer: writer, key: key, ready: true, now: time.Now}
	if err := store.validateEncryptedRows(ctx); err != nil {
		return nil, ErrModelProfileKeyUnavailable
	}
	return store, nil
}

func (s *encryptedModelProfileStore) validateEncryptedRows(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT profile_id, provider, api_key_nonce, api_key_ciphertext FROM p2p_agent_model_profiles WHERE api_key_ciphertext <> ''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var profileID, provider string
		var nonce, ciphertext []byte
		if err := rows.Scan(&profileID, &provider, &nonce, &ciphertext); err != nil {
			return err
		}
		if _, err := s.decrypt(profileID, provider, nonce, ciphertext); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	history, err := s.db.QueryContext(ctx, `SELECT profile_id, provider, api_key_nonce, api_key_ciphertext FROM p2p_agent_model_profile_credentials`)
	if err != nil {
		return err
	}
	defer history.Close()
	for history.Next() {
		var profileID, provider string
		var nonce, ciphertext []byte
		if err := history.Scan(&profileID, &provider, &nonce, &ciphertext); err != nil {
			return err
		}
		if _, err := s.decrypt(profileID, provider, nonce, ciphertext); err != nil {
			return err
		}
	}
	return history.Err()
}

func NewDatabaseModelProfileStore(ctx context.Context, store *DatabaseStore, keyFile string) (ModelProfileStore, error) {
	if store == nil {
		return nil, fmt.Errorf("model profile database is unavailable")
	}
	return NewEncryptedModelProfileStore(ctx, store.db, store.writer, keyFile)
}

func (s *encryptedModelProfileStore) ModelProfileStoreReady() bool { return s != nil && s.ready }

func loadOrCreateModelProfileKey(path string, encryptedRows bool) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ErrModelProfileKeyUnavailable
	}
	if raw, err := os.ReadFile(path); err == nil {
		if len(raw) != 32 {
			return nil, ErrModelProfileKeyUnavailable
		}
		if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0600 {
			return nil, ErrModelProfileKeyUnavailable
		}
		return append([]byte(nil), raw...), nil
	} else if encryptedRows {
		return nil, ErrModelProfileKeyUnavailable
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, ErrModelProfileKeyUnavailable
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, ErrModelProfileKeyUnavailable
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, ErrModelProfileKeyUnavailable
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return loadOrCreateModelProfileKey(path, encryptedRows)
		}
		return nil, ErrModelProfileKeyUnavailable
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, ErrModelProfileKeyUnavailable
	}
	var writeErr error
	if _, writeErr = file.Write(key); writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return nil, ErrModelProfileKeyUnavailable
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, ErrModelProfileKeyUnavailable
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return nil, ErrModelProfileKeyUnavailable
	}
	if err := dir.Close(); err != nil {
		return nil, ErrModelProfileKeyUnavailable
	}
	return key, nil
}

func (s *encryptedModelProfileStore) SyncModelProfiles(ctx context.Context, ownerID, idempotencyKey, defaultClientID string, entries []ModelProfileSyncEntry) (ModelProfileSyncResult, error) {
	ownerID, idempotencyKey = strings.TrimSpace(ownerID), strings.TrimSpace(idempotencyKey)
	if ownerID == "" || idempotencyKey == "" {
		return ModelProfileSyncResult{}, ErrModelProfileInvalid
	}
	digest := profileSyncDigest(defaultClientID, entries)
	var result ModelProfileSyncResult
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var storedDigest []byte
		var storedJSON string
		claimed := false
		err := tx.QueryRowContext(ctx, `INSERT INTO p2p_agent_model_profile_syncs(owner_id,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,'{}'::jsonb,$4) ON CONFLICT DO NOTHING RETURNING request_digest,response_json`, ownerID, idempotencyKey, digest[:], s.now()).Scan(&storedDigest, &storedJSON)
		if err == nil {
			claimed = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !claimed {
			if err := tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM p2p_agent_model_profile_syncs WHERE owner_id=$1 AND idempotency_key=$2 FOR UPDATE`, ownerID, idempotencyKey).Scan(&storedDigest, &storedJSON); err != nil {
				return err
			}
			if string(storedDigest) != string(digest[:]) {
				return ErrModelProfileIdempotency
			}
			if storedJSON != "{}" {
				return json.Unmarshal([]byte(storedJSON), &result)
			}
		}
		for _, entry := range entries {
			if strings.TrimSpace(entry.ClientProfileID) == "" || strings.TrimSpace(entry.Provider) == "" || (entry.APIKey != nil && strings.TrimSpace(*entry.APIKey) == "") {
				return ErrModelProfileInvalid
			}
		}
		for _, entry := range entries {
			if err := s.upsertProfileTx(ctx, tx, ownerID, entry); err != nil {
				return err
			}
		}
		if defaultClientID != "" {
			var profileID string
			if err := tx.QueryRowContext(ctx, `SELECT profile_id FROM p2p_agent_model_profiles WHERE owner_id=$1 AND client_profile_id=$2`, ownerID, defaultClientID).Scan(&profileID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrModelProfileNotFound
				}
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO p2p_agent_model_profile_defaults(owner_id, profile_id, client_profile_id) VALUES($1,$2,$3) ON CONFLICT(owner_id) DO UPDATE SET profile_id=EXCLUDED.profile_id, client_profile_id=EXCLUDED.client_profile_id`, ownerID, profileID, defaultClientID); err != nil {
				return err
			}
		} else {
			_ = tx.QueryRowContext(ctx, `SELECT client_profile_id FROM p2p_agent_model_profile_defaults WHERE owner_id=$1`, ownerID).Scan(&defaultClientID)
		}
		profiles, err := s.listProfilesTx(ctx, tx, ownerID, 0, "")
		if err != nil {
			return err
		}
		result = ModelProfileSyncResult{Profiles: profiles, DefaultClientProfileID: defaultClientID}
		payload, err := json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE p2p_agent_model_profile_syncs SET response_json=$3 WHERE owner_id=$1 AND idempotency_key=$2`, ownerID, idempotencyKey, string(payload))
		return err
	})
	return result, err
}

func (s *encryptedModelProfileStore) upsertProfileTx(ctx context.Context, tx *sql.Tx, ownerID string, entry ModelProfileSyncEntry) error {
	var profile ModelProfile
	var deletedAt sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT profile_id,revision,provider,credential_version,deleted_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND client_profile_id=$2 FOR UPDATE`, ownerID, entry.ClientProfileID).Scan(&profile.ProfileID, &profile.Revision, &profile.Provider, &profile.CredentialVersion, &deletedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	isNew := errors.Is(err, sql.ErrNoRows)
	if !isNew && entry.ExpectedRevision != nil && *entry.ExpectedRevision != profile.Revision {
		return ErrModelProfileRevision
	}
	if isNew {
		profile.ProfileID = uuid.NewString()
		profile.Revision = 0
		profile.CredentialVersion = 0
	}
	profile.Deleted = deletedAt.Valid
	revision := profile.Revision + 1
	provider := strings.ToLower(strings.TrimSpace(entry.Provider))
	if provider == "" {
		return ErrModelProfileInvalid
	}
	var nonce, ciphertext []byte
	credentialRotated := entry.APIKey != nil
	if isNew || entry.APIKey != nil {
		if entry.APIKey != nil {
			var err error
			nonce, ciphertext, err = s.encrypt(profile.ProfileID, provider, []byte(*entry.APIKey))
			if err != nil {
				return err
			}
		} else if !isNew {
			if err := tx.QueryRowContext(ctx, `SELECT api_key_nonce,api_key_ciphertext FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2`, ownerID, profile.ProfileID).Scan(&nonce, &ciphertext); err != nil {
				return err
			}
		}
	} else {
		if err := tx.QueryRowContext(ctx, `SELECT api_key_nonce,api_key_ciphertext FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2`, ownerID, profile.ProfileID).Scan(&nonce, &ciphertext); err != nil {
			return err
		}
	}
	if !isNew && entry.APIKey == nil && len(ciphertext) > 0 && profile.Provider != provider {
		apiKey, err := s.decrypt(profile.ProfileID, profile.Provider, nonce, ciphertext)
		if err != nil {
			return err
		}
		nonce, ciphertext, err = s.encrypt(profile.ProfileID, provider, []byte(apiKey))
		if err != nil {
			return err
		}
		credentialRotated = true
	}
	if credentialRotated {
		profile.CredentialVersion++
		if profile.CredentialVersion <= 0 {
			profile.CredentialVersion = 1
		}
	}
	args := []any{ownerID, profile.ProfileID, entry.ClientProfileID, strings.TrimSpace(entry.DisplayName), provider, strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/"), strings.TrimSpace(entry.Model), strings.TrimSpace(entry.SystemPrompt), nullableFloat(entry.Temperature), nullableFloat(entry.TopP), entry.MaxOutputTokens, entry.ContextWindow, strings.TrimSpace(entry.ReasoningEffort), revision}
	if isNew {
		_, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_model_profiles(owner_id,profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,revision,api_key_version,api_key_nonce,api_key_ciphertext,credential_version,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,1,$15,$16,$17,$18,$18)`, append(args, nonce, ciphertext, profile.CredentialVersion, s.now())...)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE p2p_agent_model_profiles SET display_name=$4,provider=$5,base_url=$6,model=$7,system_prompt=$8,temperature=$9,top_p=$10,max_output_tokens=$11,context_window=$12,reasoning_effort=$13,revision=$14,api_key_version=1,api_key_nonce=$15,api_key_ciphertext=$16,credential_version=$17,deleted_at=NULL,updated_at=$18 WHERE owner_id=$1 AND profile_id=$2 AND client_profile_id=$3`, append(args, nonce, ciphertext, profile.CredentialVersion, s.now())...)
	}
	if err == nil && credentialRotated {
		_, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_model_profile_credentials(owner_id,profile_id,credential_version,provider,api_key_nonce,api_key_ciphertext,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, ownerID, profile.ProfileID, profile.CredentialVersion, provider, nonce, ciphertext, s.now())
	}
	if err == nil {
		err = s.snapshotProfileTx(ctx, tx, ownerID, profile.ProfileID, revision)
	}
	return err
}

func (s *encryptedModelProfileStore) snapshotProfileTx(ctx context.Context, tx *sql.Tx, ownerID, profileID string, revision int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO p2p_agent_model_profile_revisions(owner_id,profile_id,profile_revision,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,credential_version,deleted_at,created_at) SELECT owner_id,profile_id,revision,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,credential_version,deleted_at,updated_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2 AND revision=$3 ON CONFLICT DO NOTHING`, ownerID, profileID, revision)
	return err
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func (s *encryptedModelProfileStore) encrypt(profileID, provider string, plaintext []byte) ([]byte, []byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, nil, ErrModelProfileKeyUnavailable
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, ErrModelProfileKeyUnavailable
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, ErrModelProfileKeyUnavailable
	}
	return nonce, aead.Seal(nil, nonce, plaintext, []byte(profileID+"\x00"+provider)), nil
}

func (s *encryptedModelProfileStore) decrypt(profileID, provider string, nonce, ciphertext []byte) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", ErrModelProfileKeyUnavailable
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != aead.NonceSize() {
		return "", ErrModelProfileKeyUnavailable
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(profileID+"\x00"+provider))
	if err != nil {
		return "", ErrModelProfileKeyUnavailable
	}
	return string(plaintext), nil
}

func profileSyncDigest(defaultClientID string, entries []ModelProfileSyncEntry) [32]byte {
	data, _ := json.Marshal(struct {
		Default string
		Entries []ModelProfileSyncEntry
	}{defaultClientID, entries})
	return sha256.Sum256(data)
}

func profileDeleteDigest(profileID string, expected *int64) []byte {
	payload := struct {
		ProfileID string
		Expected  *int64
	}{profileID, expected}
	sum := sha256.Sum256([]byte(encodeModelProfileDigest(payload)))
	return sum[:]
}

func encodeModelProfileDigest(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func (s *encryptedModelProfileStore) ListModelProfiles(ctx context.Context, ownerID string, pageSize int, pageToken string) (ModelProfileListResult, error) {
	return s.listProfiles(ctx, ownerID, pageSize, pageToken)
}

func (s *encryptedModelProfileStore) listProfiles(ctx context.Context, ownerID string, pageSize int, pageToken string) (ModelProfileListResult, error) {
	if pageSize < 0 || pageSize > 100 {
		return ModelProfileListResult{}, ErrModelProfileInvalid
	}
	if pageSize == 0 {
		pageSize = defaultModelProfilePageSize
	}
	var cursorClient, cursorProfile string
	if pageToken != "" {
		var decodeErr error
		cursorClient, cursorProfile, decodeErr = decodeModelProfilePageToken(pageToken)
		if decodeErr != nil || cursorClient == "" || cursorProfile == "" {
			return ModelProfileListResult{}, ErrModelProfileInvalid
		}
		var exists bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM p2p_agent_model_profiles WHERE owner_id=$1 AND client_profile_id=$2 AND profile_id=$3 AND deleted_at IS NULL)`, ownerID, cursorClient, cursorProfile).Scan(&exists); err != nil {
			return ModelProfileListResult{}, err
		}
		if !exists {
			return ModelProfileListResult{}, ErrModelProfileInvalid
		}
	}
	query := `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,revision,credential_version,api_key_nonce,api_key_ciphertext,created_at,updated_at,deleted_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND deleted_at IS NULL`
	args := []any{ownerID}
	if pageToken != "" {
		query += ` AND (client_profile_id,profile_id) > ($2,$3)`
		args = append(args, cursorClient, cursorProfile)
	}
	query += ` ORDER BY client_profile_id,profile_id LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, pageSize+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ModelProfileListResult{}, err
	}
	defer rows.Close()
	profiles := make([]ModelProfile, 0, pageSize+1)
	for rows.Next() {
		profile, scanErr := s.scanProfile(rows)
		if scanErr != nil {
			return ModelProfileListResult{}, scanErr
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return ModelProfileListResult{}, err
	}
	next := ""
	if len(profiles) > pageSize {
		next = encodeModelProfilePageToken(profiles[pageSize-1].ClientProfileID, profiles[pageSize-1].ProfileID)
		profiles = profiles[:pageSize]
	}
	var defaultID string
	_ = s.db.QueryRowContext(ctx, `SELECT client_profile_id FROM p2p_agent_model_profile_defaults WHERE owner_id=$1`, ownerID).Scan(&defaultID)
	return ModelProfileListResult{Profiles: profiles, NextPageToken: next, DefaultClientProfileID: defaultID}, nil
}

func encodeModelProfilePageToken(clientProfileID, profileID string) string {
	return hex.EncodeToString([]byte(clientProfileID + "\x00" + profileID))
}

func decodeModelProfilePageToken(token string) (string, string, error) {
	decoded, err := hex.DecodeString(token)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(string(decoded), "\x00")
	if len(parts) != 2 {
		return "", "", ErrModelProfileInvalid
	}
	return parts[0], parts[1], nil
}

func (s *encryptedModelProfileStore) listProfilesTx(ctx context.Context, tx *sql.Tx, ownerID string, pageSize int, pageToken string) ([]ModelProfile, error) {
	rows, err := tx.QueryContext(ctx, `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,revision,credential_version,api_key_nonce,api_key_ciphertext,created_at,updated_at,deleted_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND deleted_at IS NULL ORDER BY client_profile_id,profile_id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := make([]ModelProfile, 0)
	for rows.Next() {
		p, err := s.scanProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

type modelProfileScanner interface{ Scan(...any) error }

func (s *encryptedModelProfileStore) scanProfile(row modelProfileScanner) (ModelProfile, error) {
	var p ModelProfile
	var nonce, ciphertext []byte
	var temperature, topP sql.NullFloat64
	var deletedAt sql.NullTime
	err := row.Scan(&p.ProfileID, &p.ClientProfileID, &p.DisplayName, &p.Provider, &p.BaseURL, &p.Model, &p.SystemPrompt, &temperature, &topP, &p.MaxOutputTokens, &p.ContextWindow, &p.ReasoningEffort, &p.Revision, &p.CredentialVersion, &nonce, &ciphertext, &p.CreatedAt, &p.UpdatedAt, &deletedAt)
	if err != nil {
		return p, err
	}
	if temperature.Valid {
		p.Temperature = &temperature.Float64
	}
	if topP.Valid {
		p.TopP = &topP.Float64
	}
	p.Deleted = deletedAt.Valid
	if len(ciphertext) > 0 {
		p.APIKey, err = s.decrypt(p.ProfileID, p.Provider, nonce, ciphertext)
		if err != nil {
			return ModelProfile{}, err
		}
		p.APIKeyConfigured = true
	}
	return p, nil
}

func (s *encryptedModelProfileStore) GetModelProfile(ctx context.Context, ownerID, profileID string) (ModelProfile, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,revision,credential_version,api_key_nonce,api_key_ciphertext,created_at,updated_at,deleted_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2 AND deleted_at IS NULL`, ownerID, profileID)
	p, err := s.scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelProfile{}, false, nil
	}
	return p, err == nil, err
}
func (s *encryptedModelProfileStore) ResolveModelProfile(ctx context.Context, ownerID, profileID string) (ModelProfile, error) {
	p, ok, err := s.GetModelProfile(ctx, ownerID, profileID)
	if err != nil {
		return ModelProfile{}, err
	}
	if !ok {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	return p, nil
}

func (s *encryptedModelProfileStore) ResolveModelProfileVersion(ctx context.Context, ownerID, profileID string, credentialVersion int64) (ModelProfile, error) {
	profile, err := s.ResolveModelProfile(ctx, ownerID, profileID)
	if err != nil || credentialVersion <= 0 || credentialVersion == profile.CredentialVersion {
		return profile, err
	}
	var provider string
	var nonce, ciphertext []byte
	err = s.db.QueryRowContext(ctx, `SELECT provider,api_key_nonce,api_key_ciphertext FROM p2p_agent_model_profile_credentials WHERE owner_id=$1 AND profile_id=$2 AND credential_version=$3`, ownerID, profileID, credentialVersion).Scan(&provider, &nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	if err != nil {
		return ModelProfile{}, err
	}
	apiKey, err := s.decrypt(profileID, provider, nonce, ciphertext)
	if err != nil {
		return ModelProfile{}, err
	}
	profile.Provider, profile.APIKey, profile.APIKeyConfigured, profile.CredentialVersion = provider, apiKey, true, credentialVersion
	return profile, nil
}

func (s *encryptedModelProfileStore) ResolveModelProfilePinned(ctx context.Context, ownerID, profileID string, profileRevision, credentialVersion int64) (ModelProfile, error) {
	if profileRevision <= 0 {
		return s.ResolveModelProfileVersion(ctx, ownerID, profileID, credentialVersion)
	}
	var profile ModelProfile
	var temperature, topP sql.NullFloat64
	var deletedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,profile_revision,credential_version,deleted_at FROM p2p_agent_model_profile_revisions WHERE owner_id=$1 AND profile_id=$2 AND profile_revision=$3`, ownerID, profileID, profileRevision).Scan(&profile.ProfileID, &profile.ClientProfileID, &profile.DisplayName, &profile.Provider, &profile.BaseURL, &profile.Model, &profile.SystemPrompt, &temperature, &topP, &profile.MaxOutputTokens, &profile.ContextWindow, &profile.ReasoningEffort, &profile.Revision, &profile.CredentialVersion, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	if err != nil {
		return ModelProfile{}, err
	}
	if temperature.Valid {
		profile.Temperature = &temperature.Float64
	}
	if topP.Valid {
		profile.TopP = &topP.Float64
	}
	profile.Deleted = deletedAt.Valid
	if credentialVersion <= 0 {
		credentialVersion = profile.CredentialVersion
	}
	if credentialVersion > 0 {
		var provider string
		var nonce, ciphertext []byte
		if err := s.db.QueryRowContext(ctx, `SELECT provider,api_key_nonce,api_key_ciphertext FROM p2p_agent_model_profile_credentials WHERE owner_id=$1 AND profile_id=$2 AND credential_version=$3`, ownerID, profileID, credentialVersion).Scan(&provider, &nonce, &ciphertext); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ModelProfile{}, ErrModelProfileNotFound
			}
			return ModelProfile{}, err
		}
		apiKey, err := s.decrypt(profileID, provider, nonce, ciphertext)
		if err != nil {
			return ModelProfile{}, err
		}
		profile.Provider, profile.APIKey, profile.APIKeyConfigured, profile.CredentialVersion = provider, apiKey, true, credentialVersion
	}
	return profile, nil
}
func (s *encryptedModelProfileStore) DeleteModelProfile(ctx context.Context, ownerID, idempotencyKey, profileID string, expected *int64) error {
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		digest := profileDeleteDigest(profileID, expected)
		var storedProfile string
		var storedDigest []byte
		var storedResponse string
		claimed := false
		err := tx.QueryRowContext(ctx, `INSERT INTO p2p_agent_model_profile_deletes(owner_id,idempotency_key,profile_id,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,'{}'::jsonb,$5) ON CONFLICT DO NOTHING RETURNING profile_id,request_digest,response_json`, ownerID, idempotencyKey, profileID, digest, s.now()).Scan(&storedProfile, &storedDigest, &storedResponse)
		if err == nil {
			claimed = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !claimed {
			if err := tx.QueryRowContext(ctx, `SELECT profile_id,request_digest,response_json FROM p2p_agent_model_profile_deletes WHERE owner_id=$1 AND idempotency_key=$2 FOR UPDATE`, ownerID, idempotencyKey).Scan(&storedProfile, &storedDigest, &storedResponse); err != nil {
				return err
			}
			if storedProfile != profileID || (storedDigest != nil && !bytes.Equal(storedDigest, digest)) {
				return ErrModelProfileIdempotency
			}
			if storedResponse != "{}" {
				return nil
			}
		}
		var rev int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2 AND deleted_at IS NULL FOR UPDATE`, ownerID, profileID).Scan(&rev); errors.Is(err, sql.ErrNoRows) {
			return ErrModelProfileNotFound
		} else if err != nil {
			return err
		}
		if expected != nil && *expected != rev {
			return ErrModelProfileRevision
		}
		if _, err := tx.ExecContext(ctx, `UPDATE p2p_agent_model_profiles SET deleted_at=$3,revision=revision+1,updated_at=$3 WHERE owner_id=$1 AND profile_id=$2 AND deleted_at IS NULL`, ownerID, profileID, s.now()); err != nil {
			return err
		}
		if err := s.snapshotProfileTx(ctx, tx, ownerID, profileID, rev+1); err != nil {
			return err
		}
		_, _ = tx.ExecContext(ctx, `DELETE FROM p2p_agent_model_profile_defaults WHERE owner_id=$1 AND profile_id=$2`, ownerID, profileID)
		response, marshalErr := json.Marshal(map[string]any{"deleted": true, "profile_id": profileID})
		if marshalErr != nil {
			return marshalErr
		}
		_, err = tx.ExecContext(ctx, `UPDATE p2p_agent_model_profile_deletes SET request_digest=$3,response_json=$4::jsonb WHERE owner_id=$1 AND idempotency_key=$2`, ownerID, idempotencyKey, digest, string(response))
		return err
	})
}

// ModelProfileStoreKeyDigest is useful in diagnostics without exposing the key.
func ModelProfileStoreKeyDigest(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}
