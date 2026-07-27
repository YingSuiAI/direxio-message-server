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

const (
	ModelKindConversation = "conversation"
	ModelKindEmbedding    = "embedding"
	ModelKindSpeech       = "speech"
)

// ModelProfile is the server-owned, redacted profile projection. APIKey is
// populated only for internal model construction and is never serialized by
// ProductCore action handlers.
type ModelProfile struct {
	ProfileID, ClientProfileID string
	DisplayName, Provider      string
	ModelKind                  string
	InputModalities            []string
	ProviderConfig             map[string]any
	ProviderSecretStatus       map[string]bool
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
	ModelKind                                           string
	InputModalities                                     []string
	ProviderConfig                                      map[string]any
	ProviderSecrets                                     map[string]string
}

type ModelProfileDefaults struct {
	ConversationClientProfileID string
	EmbeddingClientProfileID    string
	SpeechClientProfileID       string
}

type ModelProfileSyncResult struct {
	Profiles               []ModelProfile
	DefaultClientProfileID string
	Defaults               ModelProfileDefaults
}

type ModelProfileListResult struct {
	Profiles               []ModelProfile
	NextPageToken          string
	DefaultClientProfileID string
	Defaults               ModelProfileDefaults
}

type ModelProfileStore interface {
	SyncModelProfiles(context.Context, string, string, string, []ModelProfileSyncEntry) (ModelProfileSyncResult, error)
	SyncModelProfilesWithDefaults(context.Context, string, string, ModelProfileDefaults, []ModelProfileSyncEntry) (ModelProfileSyncResult, error)
	ListModelProfiles(context.Context, string, int, string) (ModelProfileListResult, error)
	GetModelProfile(context.Context, string, string) (ModelProfile, bool, error)
	DeleteModelProfile(context.Context, string, string, string, *int64) error
	ResolveModelProfile(context.Context, string, string) (ModelProfile, error)
	// ResolveModelProfilePin returns active owner-scoped profile metadata for
	// durable callers. It never reads or decrypts credential material.
	ResolveModelProfilePin(context.Context, string, string) (ModelProfile, error)
	ResolveModelProfileVersion(context.Context, string, string, int64) (ModelProfile, error)
	ResolveModelProfilePinned(context.Context, string, string, int64, int64) (ModelProfile, error)
	ResolveDefaultModelProfile(context.Context, string, string) (ModelProfile, error)
	ResolveDefaultModelProfilePin(context.Context, string, string) (ModelProfile, error)
	ModelProfileStoreReady() bool
}

func (s *encryptedModelProfileStore) ResolveDefaultModelProfile(ctx context.Context, ownerID, kind string) (ModelProfile, error) {
	column := "profile_id"
	switch strings.TrimSpace(kind) {
	case ModelKindEmbedding:
		column = "embedding_profile_id"
	case ModelKindSpeech:
		column = "speech_profile_id"
	case ModelKindConversation:
		column = "profile_id"
	default:
		return ModelProfile{}, ErrModelProfileInvalid
	}
	var id sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT `+column+` FROM p2p_agent_model_profile_defaults WHERE owner_id=$1`, strings.TrimSpace(ownerID)).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ModelProfile{}, ErrModelProfileNotFound
		}
		return ModelProfile{}, err
	}
	if !id.Valid || strings.TrimSpace(id.String) == "" {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	profile, err := s.ResolveModelProfile(ctx, strings.TrimSpace(ownerID), id.String)
	if err != nil {
		return ModelProfile{}, err
	}
	if profile.ModelKind == "" {
		profile.ModelKind = ModelKindConversation
	}
	if profile.ModelKind != kind {
		return ModelProfile{}, ErrModelProfileInvalid
	}
	return profile, nil
}

func (s *encryptedModelProfileStore) ResolveDefaultModelProfilePin(ctx context.Context, ownerID, kind string) (ModelProfile, error) {
	column := "profile_id"
	switch strings.TrimSpace(kind) {
	case ModelKindEmbedding:
		column = "embedding_profile_id"
	case ModelKindSpeech:
		column = "speech_profile_id"
	case ModelKindConversation:
	default:
		return ModelProfile{}, ErrModelProfileInvalid
	}
	var id sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT `+column+` FROM p2p_agent_model_profile_defaults WHERE owner_id=$1`, strings.TrimSpace(ownerID)).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ModelProfile{}, ErrModelProfileNotFound
		}
		return ModelProfile{}, err
	}
	if !id.Valid || strings.TrimSpace(id.String) == "" {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,revision,credential_version,created_at,updated_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2 AND deleted_at IS NULL`, strings.TrimSpace(ownerID), id.String)
	profile, err := scanProfilePin(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	if err != nil {
		return ModelProfile{}, err
	}
	if profile.ModelKind == "" {
		profile.ModelKind = ModelKindConversation
	}
	if profile.ModelKind != kind {
		return ModelProfile{}, ErrModelProfileInvalid
	}
	return profile, nil
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
	return s.SyncModelProfilesWithDefaults(ctx, ownerID, idempotencyKey, ModelProfileDefaults{ConversationClientProfileID: defaultClientID}, entries)
}

func (s *encryptedModelProfileStore) SyncModelProfilesWithDefaults(ctx context.Context, ownerID, idempotencyKey string, defaults ModelProfileDefaults, entries []ModelProfileSyncEntry) (ModelProfileSyncResult, error) {
	ownerID, idempotencyKey = strings.TrimSpace(ownerID), strings.TrimSpace(idempotencyKey)
	if ownerID == "" || idempotencyKey == "" {
		return ModelProfileSyncResult{}, ErrModelProfileInvalid
	}
	normalizedEntries := make([]ModelProfileSyncEntry, len(entries))
	for i, entry := range entries {
		normalizedEntries[i] = entry
		if err := normalizeModelProfileEntry(&normalizedEntries[i]); err != nil {
			return ModelProfileSyncResult{}, err
		}
	}
	defaults = normalizeModelProfileDefaults(defaults)
	digest := profileSyncDigest(defaults, normalizedEntries)
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
		for _, entry := range normalizedEntries {
			if strings.TrimSpace(entry.ClientProfileID) == "" || strings.TrimSpace(entry.Provider) == "" || (entry.APIKey != nil && strings.TrimSpace(*entry.APIKey) == "") {
				return ErrModelProfileInvalid
			}
		}
		for _, entry := range normalizedEntries {
			if err := s.upsertProfileTx(ctx, tx, ownerID, entry); err != nil {
				return err
			}
		}
		if err := s.syncProfileDefaultsTx(ctx, tx, ownerID, defaults); err != nil {
			return err
		}
		profiles, err := s.listProfilesTx(ctx, tx, ownerID, 0, "")
		if err != nil {
			return err
		}
		storedDefaults := ModelProfileDefaults{}
		_ = tx.QueryRowContext(ctx, `SELECT client_profile_id, embedding_client_profile_id, speech_client_profile_id FROM p2p_agent_model_profile_defaults WHERE owner_id=$1`, ownerID).Scan(&storedDefaults.ConversationClientProfileID, &storedDefaults.EmbeddingClientProfileID, &storedDefaults.SpeechClientProfileID)
		result = ModelProfileSyncResult{Profiles: profiles, DefaultClientProfileID: storedDefaults.ConversationClientProfileID, Defaults: storedDefaults}
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
	err := tx.QueryRowContext(ctx, `SELECT profile_id,revision,provider,credential_version,model_kind,deleted_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND client_profile_id=$2 FOR UPDATE`, ownerID, entry.ClientProfileID).Scan(&profile.ProfileID, &profile.Revision, &profile.Provider, &profile.CredentialVersion, &profile.ModelKind, &deletedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	isNew := errors.Is(err, sql.ErrNoRows)
	if !isNew {
		if err := validateModelProfileCredentialTransition(profile.ModelKind, entry.ModelKind, entry.APIKey, entry.ProviderSecrets); err != nil {
			return err
		}
	}
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
	if entry.ModelKind == ModelKindSpeech && (profile.Revision == 0 || len(entry.ProviderSecrets) > 0) {
		for _, key := range []string{"rtc_app_key", "access_key_id", "secret_access_key"} {
			if strings.TrimSpace(entry.ProviderSecrets[key]) == "" {
				return ErrModelProfileInvalid
			}
		}
	}
	var nonce, ciphertext []byte
	credentialRotated := entry.APIKey != nil
	if entry.ModelKind == ModelKindSpeech {
		credentialRotated = len(entry.ProviderSecrets) > 0
	}
	if isNew || entry.APIKey != nil || len(entry.ProviderSecrets) > 0 {
		if entry.ModelKind == ModelKindSpeech && len(entry.ProviderSecrets) > 0 {
			encoded, encodeErr := json.Marshal(entry.ProviderSecrets)
			if encodeErr != nil {
				return ErrModelProfileInvalid
			}
			nonce, ciphertext, err = s.encrypt(profile.ProfileID, provider, encoded)
			if err != nil {
				return err
			}
		} else if entry.APIKey != nil {
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
	if !isNew && entry.APIKey == nil && len(entry.ProviderSecrets) == 0 && len(ciphertext) > 0 && profile.Provider != provider {
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
	modalitiesJSON, _ := json.Marshal(entry.InputModalities)
	providerConfigJSON, _ := json.Marshal(entry.ProviderConfig)
	args := []any{ownerID, profile.ProfileID, entry.ClientProfileID, strings.TrimSpace(entry.DisplayName), provider, strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/"), strings.TrimSpace(entry.Model), strings.TrimSpace(entry.SystemPrompt), nullableFloat(entry.Temperature), nullableFloat(entry.TopP), entry.MaxOutputTokens, entry.ContextWindow, strings.TrimSpace(entry.ReasoningEffort), entry.ModelKind, string(modalitiesJSON), string(providerConfigJSON), revision}
	if isNew {
		_, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_model_profiles(owner_id,profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,revision,api_key_version,api_key_nonce,api_key_ciphertext,credential_version,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15::jsonb,$16::jsonb,$17,1,$18,$19,$20,$21,$21)`, append(args, nonce, ciphertext, profile.CredentialVersion, s.now())...)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE p2p_agent_model_profiles SET display_name=$4,provider=$5,base_url=$6,model=$7,system_prompt=$8,temperature=$9,top_p=$10,max_output_tokens=$11,context_window=$12,reasoning_effort=$13,model_kind=$14,input_modalities=$15::jsonb,provider_config=$16::jsonb,revision=$17,api_key_version=1,api_key_nonce=$18,api_key_ciphertext=$19,credential_version=$20,deleted_at=NULL,updated_at=$21 WHERE owner_id=$1 AND profile_id=$2 AND client_profile_id=$3`, append(args, nonce, ciphertext, profile.CredentialVersion, s.now())...)
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
	_, err := tx.ExecContext(ctx, `INSERT INTO p2p_agent_model_profile_revisions(owner_id,profile_id,profile_revision,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,credential_version,deleted_at,created_at) SELECT owner_id,profile_id,revision,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,credential_version,deleted_at,updated_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2 AND revision=$3 ON CONFLICT DO NOTHING`, ownerID, profileID, revision)
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

func profileSyncDigest(defaultsInput any, entries []ModelProfileSyncEntry) [32]byte {
	defaults := ModelProfileDefaults{}
	switch value := defaultsInput.(type) {
	case string:
		defaults.ConversationClientProfileID = value
	case ModelProfileDefaults:
		defaults = value
	}
	data, _ := json.Marshal(struct {
		Defaults ModelProfileDefaults
		Entries  []ModelProfileSyncEntry
	}{normalizeModelProfileDefaults(defaults), entries})
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
	query := `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,revision,credential_version,api_key_nonce,api_key_ciphertext,created_at,updated_at,deleted_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND deleted_at IS NULL`
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
	var defaults ModelProfileDefaults
	_ = s.db.QueryRowContext(ctx, `SELECT client_profile_id,embedding_client_profile_id,speech_client_profile_id FROM p2p_agent_model_profile_defaults WHERE owner_id=$1`, ownerID).Scan(&defaults.ConversationClientProfileID, &defaults.EmbeddingClientProfileID, &defaults.SpeechClientProfileID)
	return ModelProfileListResult{Profiles: profiles, NextPageToken: next, DefaultClientProfileID: defaults.ConversationClientProfileID, Defaults: defaults}, nil
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
	rows, err := tx.QueryContext(ctx, `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,revision,credential_version,api_key_nonce,api_key_ciphertext,created_at,updated_at,deleted_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND deleted_at IS NULL ORDER BY client_profile_id,profile_id`, ownerID)
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
	var modalitiesJSON, providerConfigJSON []byte
	err := row.Scan(&p.ProfileID, &p.ClientProfileID, &p.DisplayName, &p.Provider, &p.BaseURL, &p.Model, &p.SystemPrompt, &temperature, &topP, &p.MaxOutputTokens, &p.ContextWindow, &p.ReasoningEffort, &p.ModelKind, &modalitiesJSON, &providerConfigJSON, &p.Revision, &p.CredentialVersion, &nonce, &ciphertext, &p.CreatedAt, &p.UpdatedAt, &deletedAt)
	if err != nil {
		return p, err
	}
	if temperature.Valid {
		p.Temperature = &temperature.Float64
	}
	if topP.Valid {
		p.TopP = &topP.Float64
	}
	_ = json.Unmarshal(modalitiesJSON, &p.InputModalities)
	_ = json.Unmarshal(providerConfigJSON, &p.ProviderConfig)
	if p.ModelKind == "" {
		p.ModelKind = ModelKindConversation
	}
	if p.ModelKind == ModelKindSpeech && p.CredentialVersion > 0 {
		p.ProviderSecretStatus = map[string]bool{"rtc_app_key": true, "access_key_id": true, "secret_access_key": true}
	}
	p.Deleted = deletedAt.Valid
	if len(ciphertext) > 0 {
		p.APIKey, err = s.decrypt(p.ProfileID, p.Provider, nonce, ciphertext)
		if err != nil {
			return ModelProfile{}, err
		}
		p.APIKeyConfigured = true
		if p.ModelKind == ModelKindSpeech {
			var secrets map[string]string
			if json.Unmarshal([]byte(p.APIKey), &secrets) == nil {
				p.ProviderSecretStatus = map[string]bool{}
				for _, key := range []string{"rtc_app_key", "access_key_id", "secret_access_key"} {
					p.ProviderSecretStatus[key] = strings.TrimSpace(secrets[key]) != ""
				}
			}
		}
	}
	return p, nil
}

func (s *encryptedModelProfileStore) GetModelProfile(ctx context.Context, ownerID, profileID string) (ModelProfile, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,revision,credential_version,api_key_nonce,api_key_ciphertext,created_at,updated_at,deleted_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2 AND deleted_at IS NULL`, ownerID, profileID)
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

// scanProfilePin deliberately has no credential columns. Keep it separate
// from scanProfile so adding a caller to the durable pin path cannot begin
// decrypting a key by accident.
func scanProfilePin(row modelProfileScanner) (ModelProfile, error) {
	var p ModelProfile
	var temperature, topP sql.NullFloat64
	var modalitiesJSON, providerConfigJSON []byte
	err := row.Scan(&p.ProfileID, &p.ClientProfileID, &p.DisplayName, &p.Provider, &p.BaseURL, &p.Model, &p.SystemPrompt, &temperature, &topP, &p.MaxOutputTokens, &p.ContextWindow, &p.ReasoningEffort, &p.ModelKind, &modalitiesJSON, &providerConfigJSON, &p.Revision, &p.CredentialVersion, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	if temperature.Valid {
		p.Temperature = &temperature.Float64
	}
	if topP.Valid {
		p.TopP = &topP.Float64
	}
	_ = json.Unmarshal(modalitiesJSON, &p.InputModalities)
	_ = json.Unmarshal(providerConfigJSON, &p.ProviderConfig)
	if p.ModelKind == "" {
		p.ModelKind = ModelKindConversation
	}
	if p.ModelKind == ModelKindSpeech && p.CredentialVersion > 0 {
		p.ProviderSecretStatus = map[string]bool{"rtc_app_key": true, "access_key_id": true, "secret_access_key": true}
	}
	return p, nil
}

func (s *encryptedModelProfileStore) ResolveModelProfilePin(ctx context.Context, ownerID, profileID string) (ModelProfile, error) {
	row := s.db.QueryRowContext(ctx, `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,revision,credential_version,created_at,updated_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2 AND deleted_at IS NULL`, ownerID, profileID)
	p, err := scanProfilePin(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	return p, err
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
	var modalitiesJSON, providerConfigJSON []byte
	err := s.db.QueryRowContext(ctx, `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,profile_revision,credential_version,deleted_at FROM p2p_agent_model_profile_revisions WHERE owner_id=$1 AND profile_id=$2 AND profile_revision=$3`, ownerID, profileID, profileRevision).Scan(&profile.ProfileID, &profile.ClientProfileID, &profile.DisplayName, &profile.Provider, &profile.BaseURL, &profile.Model, &profile.SystemPrompt, &temperature, &topP, &profile.MaxOutputTokens, &profile.ContextWindow, &profile.ReasoningEffort, &profile.ModelKind, &modalitiesJSON, &providerConfigJSON, &profile.Revision, &profile.CredentialVersion, &deletedAt)
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
	_ = json.Unmarshal(modalitiesJSON, &profile.InputModalities)
	_ = json.Unmarshal(providerConfigJSON, &profile.ProviderConfig)
	if profile.ModelKind == "" {
		profile.ModelKind = ModelKindConversation
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
		var clientProfileID string
		if err := tx.QueryRowContext(ctx, `SELECT revision,client_profile_id FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2 AND deleted_at IS NULL FOR UPDATE`, ownerID, profileID).Scan(&rev, &clientProfileID); errors.Is(err, sql.ErrNoRows) {
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
		_, err = tx.ExecContext(ctx, `UPDATE p2p_agent_model_profile_defaults SET profile_id=CASE WHEN profile_id=$2 OR client_profile_id=$3 THEN NULL ELSE profile_id END,client_profile_id=CASE WHEN client_profile_id=$3 THEN '' ELSE client_profile_id END,embedding_profile_id=CASE WHEN embedding_profile_id=$2 OR embedding_client_profile_id=$3 THEN NULL ELSE embedding_profile_id END,embedding_client_profile_id=CASE WHEN embedding_client_profile_id=$3 THEN '' ELSE embedding_client_profile_id END,speech_profile_id=CASE WHEN speech_profile_id=$2 OR speech_client_profile_id=$3 THEN NULL ELSE speech_profile_id END,speech_client_profile_id=CASE WHEN speech_client_profile_id=$3 THEN '' ELSE speech_client_profile_id END WHERE owner_id=$1`, ownerID, profileID, clientProfileID)
		if err != nil {
			return err
		}
		response, marshalErr := json.Marshal(map[string]any{"deleted": true, "profile_id": profileID})
		if marshalErr != nil {
			return marshalErr
		}
		_, err = tx.ExecContext(ctx, `UPDATE p2p_agent_model_profile_deletes SET request_digest=$3,response_json=$4::jsonb WHERE owner_id=$1 AND idempotency_key=$2`, ownerID, idempotencyKey, digest, string(response))
		return err
	})
}

func normalizeModelProfileDefaults(defaults ModelProfileDefaults) ModelProfileDefaults {
	defaults.ConversationClientProfileID = strings.TrimSpace(defaults.ConversationClientProfileID)
	defaults.EmbeddingClientProfileID = strings.TrimSpace(defaults.EmbeddingClientProfileID)
	defaults.SpeechClientProfileID = strings.TrimSpace(defaults.SpeechClientProfileID)
	return defaults
}

func normalizeModelProfileEntry(entry *ModelProfileSyncEntry) error {
	if entry == nil {
		return ErrModelProfileInvalid
	}
	entry.ModelKind = strings.ToLower(strings.TrimSpace(entry.ModelKind))
	if entry.ModelKind == "" {
		entry.ModelKind = ModelKindConversation
	}
	modalities := make([]string, 0, len(entry.InputModalities))
	seen := map[string]bool{}
	for _, raw := range entry.InputModalities {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" || seen[value] {
			return ErrModelProfileInvalid
		}
		seen[value] = true
		modalities = append(modalities, value)
	}
	switch entry.ModelKind {
	case ModelKindConversation:
		if len(modalities) == 0 {
			modalities = []string{"text"}
			seen["text"] = true
		}
		for _, modality := range modalities {
			if modality != "text" && modality != "image" {
				return ErrModelProfileInvalid
			}
		}
		if !seen["text"] {
			return ErrModelProfileInvalid
		}
	case ModelKindEmbedding:
		if len(modalities) == 0 {
			modalities = []string{"text"}
			seen["text"] = true
		}
		if len(modalities) != 1 || modalities[0] != "text" {
			return ErrModelProfileInvalid
		}
	case ModelKindSpeech:
		if entry.APIKey != nil {
			return ErrModelProfileInvalid
		}
		if len(modalities) == 0 {
			modalities = []string{"audio"}
			seen["audio"] = true
		}
		if len(modalities) != 1 || modalities[0] != "audio" {
			return ErrModelProfileInvalid
		}
	default:
		return ErrModelProfileInvalid
	}
	entry.InputModalities = modalities
	if entry.ModelKind == ModelKindSpeech {
		if strings.ToLower(strings.TrimSpace(entry.Provider)) != "volc_voice" {
			return ErrModelProfileInvalid
		}
		if entry.ProviderConfig == nil {
			entry.ProviderConfig = map[string]any{}
		}
		allowed := map[string]bool{"app_id": true, "voice_chat_app_id": true, "ai_user_id": true, "tts_speaker": true, "tts_resource_id": true, "tts_speech_rate": true, "tts_loudness_rate": true, "tts_pitch": true}
		for key, value := range entry.ProviderConfig {
			if !allowed[key] {
				return ErrModelProfileInvalid
			}
			switch typed := value.(type) {
			case string:
				if strings.TrimSpace(typed) == "" {
					return ErrModelProfileInvalid
				}
			default:
				return ErrModelProfileInvalid
			}
		}
		allowedSecrets := map[string]bool{"rtc_app_key": true, "access_key_id": true, "secret_access_key": true}
		for key, value := range entry.ProviderSecrets {
			if !allowedSecrets[key] || strings.TrimSpace(value) == "" {
				return ErrModelProfileInvalid
			}
		}
	} else if len(entry.ProviderConfig) > 0 || len(entry.ProviderSecrets) > 0 {
		return ErrModelProfileInvalid
	}
	return nil
}

func validateModelProfileCredentialTransition(existingKind, requestedKind string, apiKey *string, providerSecrets map[string]string) error {
	if existingKind == "" {
		existingKind = ModelKindConversation
	}
	if existingKind == requestedKind {
		return nil
	}
	if existingKind == ModelKindSpeech {
		// A voice secret bundle must never be reused as a generic API key.
		if apiKey == nil || len(providerSecrets) != 0 {
			return ErrModelProfileInvalid
		}
		return nil
	}
	if requestedKind == ModelKindSpeech {
		// A generic API key must never be reinterpreted as voice credentials.
		if apiKey != nil || len(providerSecrets) == 0 {
			return ErrModelProfileInvalid
		}
	}
	return nil
}

func validateDefaultKinds(defaults ModelProfileDefaults, profiles map[string]ModelProfile) error {
	defaults = normalizeModelProfileDefaults(defaults)
	for _, item := range []struct {
		clientID string
		kind     string
		required bool
	}{
		{defaults.ConversationClientProfileID, ModelKindConversation, true},
		{defaults.EmbeddingClientProfileID, ModelKindEmbedding, false},
		{defaults.SpeechClientProfileID, ModelKindSpeech, false},
	} {
		if item.clientID == "" {
			if item.required {
				// Preserve legacy behavior: an omitted conversation default leaves
				// the current default unchanged and is valid for existing stores.
			}
			continue
		}
		profile, ok := profiles[item.clientID]
		if !ok || profile.Deleted {
			return ErrModelProfileNotFound
		}
		if profile.ModelKind != item.kind {
			return ErrModelProfileInvalid
		}
	}
	return nil
}

func (s *encryptedModelProfileStore) syncProfileDefaultsTx(ctx context.Context, tx *sql.Tx, ownerID string, requested ModelProfileDefaults) error {
	requested = normalizeModelProfileDefaults(requested)
	var current ModelProfileDefaults
	_ = tx.QueryRowContext(ctx, `SELECT client_profile_id, embedding_client_profile_id, speech_client_profile_id FROM p2p_agent_model_profile_defaults WHERE owner_id=$1`, ownerID).Scan(&current.ConversationClientProfileID, &current.EmbeddingClientProfileID, &current.SpeechClientProfileID)
	if requested.ConversationClientProfileID == "" {
		requested.ConversationClientProfileID = current.ConversationClientProfileID
	}
	if requested.EmbeddingClientProfileID == "" {
		requested.EmbeddingClientProfileID = current.EmbeddingClientProfileID
	}
	if requested.SpeechClientProfileID == "" {
		requested.SpeechClientProfileID = current.SpeechClientProfileID
	}
	rows, err := tx.QueryContext(ctx, `SELECT client_profile_id, model_kind FROM p2p_agent_model_profiles WHERE owner_id=$1 AND deleted_at IS NULL`, ownerID)
	if err != nil {
		return err
	}
	profiles := map[string]ModelProfile{}
	for rows.Next() {
		var clientID, kind string
		if err := rows.Scan(&clientID, &kind); err != nil {
			rows.Close()
			return err
		}
		profiles[clientID] = ModelProfile{ClientProfileID: clientID, ModelKind: kind}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := validateDefaultKinds(requested, profiles); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_model_profile_defaults(owner_id, profile_id, client_profile_id, embedding_profile_id, embedding_client_profile_id, speech_profile_id, speech_client_profile_id) VALUES($1, NULLIF((SELECT profile_id FROM p2p_agent_model_profiles WHERE owner_id=$1 AND client_profile_id=$2),''), $2, NULLIF((SELECT profile_id FROM p2p_agent_model_profiles WHERE owner_id=$1 AND client_profile_id=$3),''), $3, NULLIF((SELECT profile_id FROM p2p_agent_model_profiles WHERE owner_id=$1 AND client_profile_id=$4),''), $4) ON CONFLICT(owner_id) DO UPDATE SET profile_id=EXCLUDED.profile_id,client_profile_id=EXCLUDED.client_profile_id,embedding_profile_id=EXCLUDED.embedding_profile_id,embedding_client_profile_id=EXCLUDED.embedding_client_profile_id,speech_profile_id=EXCLUDED.speech_profile_id,speech_client_profile_id=EXCLUDED.speech_client_profile_id`, ownerID, requested.ConversationClientProfileID, requested.EmbeddingClientProfileID, requested.SpeechClientProfileID)
	return err
}

// ModelProfileStoreKeyDigest is useful in diagnostics without exposing the key.
func ModelProfileStoreKeyDigest(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}
