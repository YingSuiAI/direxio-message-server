package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type memoryModelProfileSync struct {
	Digest [32]byte
	Result ModelProfileSyncResult
}

type memoryModelProfileCredential struct {
	Provider string
	APIKey   string
}

type memoryModelProfileDelete struct {
	ProfileID string
	Digest    []byte
}

func memoryModelProfileRevisionKey(ownerID, profileID string, revision int64) string {
	return ownerID + "\x00" + profileID + "\x00" + strconv.FormatInt(revision, 10)
}

func (s *MemoryStore) ModelProfileStoreReady() bool { return false }

func (s *MemoryStore) ResolveDefaultModelProfile(_ context.Context, ownerID, kind string) (ModelProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	defaults := s.modelProfileDefaults[strings.TrimSpace(ownerID)]
	clientID := defaults.ConversationClientProfileID
	switch kind {
	case ModelKindEmbedding:
		clientID = defaults.EmbeddingClientProfileID
	case ModelKindSpeech:
		clientID = defaults.SpeechClientProfileID
	case ModelKindConversation:
	default:
		return ModelProfile{}, ErrModelProfileInvalid
	}
	if strings.TrimSpace(clientID) == "" {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	profile, ok := s.modelProfiles[strings.TrimSpace(ownerID)+"\x00"+clientID]
	if !ok || profile.Deleted {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	if profile.ModelKind == "" {
		profile.ModelKind = ModelKindConversation
	}
	if profile.ModelKind != kind {
		return ModelProfile{}, ErrModelProfileInvalid
	}
	return profile, nil
}

func (s *MemoryStore) ResolveDefaultModelProfilePin(ctx context.Context, ownerID, kind string) (ModelProfile, error) {
	profile, err := s.ResolveDefaultModelProfile(ctx, ownerID, kind)
	if err != nil {
		return ModelProfile{}, err
	}
	profile.APIKey = ""
	return profile, nil
}

func (s *MemoryStore) SyncModelProfiles(_ context.Context, ownerID, idempotencyKey string, defaultClientID string, entries []ModelProfileSyncEntry) (ModelProfileSyncResult, error) {
	return s.SyncModelProfilesWithDefaults(context.Background(), ownerID, idempotencyKey, ModelProfileDefaults{ConversationClientProfileID: defaultClientID}, entries)
}

func (s *MemoryStore) SyncModelProfilesWithDefaults(_ context.Context, ownerID, idempotencyKey string, requestedDefaults ModelProfileDefaults, entries []ModelProfileSyncEntry) (ModelProfileSyncResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return ModelProfileSyncResult{}, ErrModelProfileInvalid
	}
	requestedDefaults = normalizeModelProfileDefaults(requestedDefaults)
	normalizedEntries := make([]ModelProfileSyncEntry, len(entries))
	for i, entry := range entries {
		normalizedEntries[i] = entry
		if err := normalizeModelProfileEntry(&normalizedEntries[i]); err != nil {
			return ModelProfileSyncResult{}, err
		}
	}
	digest := profileSyncDigest(requestedDefaults, normalizedEntries)
	syncKey := ownerID + "\x00" + idempotencyKey
	if prior, ok := s.modelProfileSyncs[syncKey]; ok {
		if prior.Digest != digest {
			return ModelProfileSyncResult{}, ErrModelProfileIdempotency
		}
		return prior.Result, nil
	}
	for _, entry := range normalizedEntries {
		if strings.TrimSpace(entry.ClientProfileID) == "" || strings.TrimSpace(entry.Provider) == "" || (entry.APIKey != nil && strings.TrimSpace(*entry.APIKey) == "") {
			return ModelProfileSyncResult{}, ErrModelProfileInvalid
		}
	}
	profiles := cloneModelProfiles(s.modelProfiles)
	revisions := cloneModelProfiles(s.modelProfileRevisions)
	credentials := cloneModelProfileCredentials(s.modelProfileCredentials)
	defaults := cloneModelProfileDefaults(s.modelProfileDefaults)
	for _, entry := range normalizedEntries {
		key := ownerID + "\x00" + entry.ClientProfileID
		profile, ok := profiles[key]
		if !ok {
			profile = ModelProfile{ProfileID: uuid.NewString(), ClientProfileID: entry.ClientProfileID, Revision: 0, CreatedAt: time.Now().UTC()}
		}
		if err := normalizeModelProfileEntry(&entry); err != nil {
			return ModelProfileSyncResult{}, err
		}
		if profile.Revision > 0 {
			if err := validateModelProfileCredentialTransition(profile.Provider, profile.ModelKind, entry.Provider, entry.ModelKind, entry.APIKey, entry.ProviderSecrets); err != nil {
				return ModelProfileSyncResult{}, err
			}
		}
		if entry.ExpectedRevision != nil && *entry.ExpectedRevision != profile.Revision {
			return ModelProfileSyncResult{}, ErrModelProfileRevision
		}
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "volc_voice" && entry.ModelKind == ModelKindSpeech && (profile.Revision == 0 || len(entry.ProviderSecrets) > 0) {
			for _, key := range []string{"rtc_app_key", "access_key_id", "secret_access_key"} {
				if strings.TrimSpace(entry.ProviderSecrets[key]) == "" {
					return ModelProfileSyncResult{}, ErrModelProfileInvalid
				}
			}
		}
		if profile.Revision == 0 && entry.ModelKind == ModelKindSpeech && provider != "volc_voice" && entry.APIKey == nil {
			return ModelProfileSyncResult{}, ErrModelProfileInvalid
		}
		credentialRotated := entry.APIKey != nil || len(entry.ProviderSecrets) > 0 || (profile.Provider != "" && profile.Provider != provider && profile.APIKeyConfigured)
		if entry.APIKey != nil {
			profile.APIKey = *entry.APIKey
			profile.APIKeyConfigured = profile.APIKey != ""
		}
		if credentialRotated {
			profile.CredentialVersion++
			if profile.CredentialVersion <= 0 {
				profile.CredentialVersion = 1
			}
			if credentials[profile.ProfileID] == nil {
				credentials[profile.ProfileID] = make(map[int64]memoryModelProfileCredential)
			}
			credentials[profile.ProfileID][profile.CredentialVersion] = memoryModelProfileCredential{Provider: provider, APIKey: profile.APIKey}
		}
		profile.DisplayName, profile.Provider = strings.TrimSpace(entry.DisplayName), provider
		profile.ModelKind = entry.ModelKind
		profile.InputModalities = append([]string(nil), entry.InputModalities...)
		profile.ProviderConfig = cloneAnyMap(entry.ProviderConfig)
		if provider == "volc_voice" && entry.ModelKind == ModelKindSpeech && len(entry.ProviderSecrets) > 0 {
			encoded, encodeErr := json.Marshal(entry.ProviderSecrets)
			if encodeErr != nil {
				return ModelProfileSyncResult{}, ErrModelProfileInvalid
			}
			profile.APIKey = string(encoded)
			profile.APIKeyConfigured = true
			if credentialRotated {
				credentials[profile.ProfileID][profile.CredentialVersion] = memoryModelProfileCredential{Provider: provider, APIKey: profile.APIKey}
			}
		}
		if provider == "volc_voice" && entry.ModelKind == ModelKindSpeech {
			var secrets map[string]string
			if json.Unmarshal([]byte(profile.APIKey), &secrets) == nil {
				profile.ProviderSecretStatus = map[string]bool{}
				for _, key := range []string{"rtc_app_key", "access_key_id", "secret_access_key"} {
					profile.ProviderSecretStatus[key] = strings.TrimSpace(secrets[key]) != ""
				}
			}
		} else {
			profile.ProviderSecretStatus = nil
		}
		profile.BaseURL, profile.Model, profile.SystemPrompt = strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/"), strings.TrimSpace(entry.Model), strings.TrimSpace(entry.SystemPrompt)
		profile.Temperature, profile.TopP = entry.Temperature, entry.TopP
		profile.MaxOutputTokens, profile.ContextWindow, profile.ReasoningEffort = entry.MaxOutputTokens, entry.ContextWindow, strings.TrimSpace(entry.ReasoningEffort)
		profile.Revision++
		profile.Deleted = false
		profile.UpdatedAt = time.Now().UTC()
		profiles[key] = profile
		revisions[memoryModelProfileRevisionKey(ownerID, profile.ProfileID, profile.Revision)] = profile
	}
	currentDefaults := defaults[ownerID]
	if requestedDefaults.ConversationClientProfileID == "" {
		requestedDefaults.ConversationClientProfileID = currentDefaults.ConversationClientProfileID
	}
	if requestedDefaults.EmbeddingClientProfileID == "" {
		requestedDefaults.EmbeddingClientProfileID = currentDefaults.EmbeddingClientProfileID
	}
	if requestedDefaults.SpeechClientProfileID == "" {
		requestedDefaults.SpeechClientProfileID = currentDefaults.SpeechClientProfileID
	}
	profileByClient := map[string]ModelProfile{}
	for key, profile := range profiles {
		if strings.HasPrefix(key, ownerID+"\x00") {
			profileByClient[profile.ClientProfileID] = profile
		}
	}
	if err := validateDefaultKinds(requestedDefaults, profileByClient); err != nil {
		return ModelProfileSyncResult{}, err
	}
	defaults[ownerID] = requestedDefaults
	result := listMemoryProfiles(profiles, defaults, ownerID)
	result.Defaults = requestedDefaults
	result.DefaultClientProfileID = requestedDefaults.ConversationClientProfileID
	s.modelProfiles = profiles
	s.modelProfileRevisions = revisions
	s.modelProfileCredentials = credentials
	s.modelProfileDefaults = defaults
	s.modelProfileSyncs[syncKey] = memoryModelProfileSync{Digest: digest, Result: result}
	return result, nil
}

func (s *MemoryStore) listMemoryProfilesLocked(ownerID string) ModelProfileSyncResult {
	return listMemoryProfiles(s.modelProfiles, s.modelProfileDefaults, ownerID)
}

func listMemoryProfiles(profilesByKey map[string]ModelProfile, defaults map[string]ModelProfileDefaults, ownerID string) ModelProfileSyncResult {
	profiles := make([]ModelProfile, 0)
	for key, profile := range profilesByKey {
		if strings.HasPrefix(key, ownerID+"\x00") && !profile.Deleted {
			profiles = append(profiles, profile)
		}
	}
	sort.Slice(profiles, func(i, j int) bool {
		if profiles[i].ClientProfileID == profiles[j].ClientProfileID {
			return profiles[i].ProfileID < profiles[j].ProfileID
		}
		return profiles[i].ClientProfileID < profiles[j].ClientProfileID
	})
	ownerDefaults := defaults[ownerID]
	return ModelProfileSyncResult{Profiles: profiles, DefaultClientProfileID: ownerDefaults.ConversationClientProfileID, Defaults: ownerDefaults}
}

func cloneModelProfiles(source map[string]ModelProfile) map[string]ModelProfile {
	copy := make(map[string]ModelProfile, len(source))
	for key, profile := range source {
		copy[key] = profile
	}
	return copy
}

func cloneModelProfileCredentials(source map[string]map[int64]memoryModelProfileCredential) map[string]map[int64]memoryModelProfileCredential {
	copy := make(map[string]map[int64]memoryModelProfileCredential, len(source))
	for profileID, versions := range source {
		copy[profileID] = make(map[int64]memoryModelProfileCredential, len(versions))
		for version, credential := range versions {
			copy[profileID][version] = credential
		}
	}
	return copy
}

func cloneModelProfileDefaults(source map[string]ModelProfileDefaults) map[string]ModelProfileDefaults {
	copy := make(map[string]ModelProfileDefaults, len(source))
	for ownerID, defaults := range source {
		copy[ownerID] = defaults
	}
	return copy
}

func (s *MemoryStore) ListModelProfiles(_ context.Context, ownerID string, pageSize int, pageToken string) (ModelProfileListResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := s.listMemoryProfilesLocked(ownerID)
	if pageSize < 0 || pageSize > 100 {
		return ModelProfileListResult{}, ErrModelProfileInvalid
	}
	if pageSize == 0 {
		pageSize = defaultModelProfilePageSize
	}
	start := 0
	if pageToken != "" {
		cursorClient, cursorProfile, decodeErr := decodeModelProfilePageToken(pageToken)
		if decodeErr != nil {
			return ModelProfileListResult{}, ErrModelProfileInvalid
		}
		for i, profile := range result.Profiles {
			if profile.ClientProfileID == cursorClient && profile.ProfileID == cursorProfile {
				start = i + 1
				break
			}
		}
		if start == 0 {
			return ModelProfileListResult{}, ErrModelProfileInvalid
		}
	}
	result.Profiles = result.Profiles[start:]
	if pageSize > 0 && len(result.Profiles) > pageSize {
		last := result.Profiles[pageSize-1]
		return ModelProfileListResult{Profiles: result.Profiles[:pageSize], NextPageToken: encodeModelProfilePageToken(last.ClientProfileID, last.ProfileID), DefaultClientProfileID: result.DefaultClientProfileID, Defaults: result.Defaults}, nil
	}
	return ModelProfileListResult{Profiles: result.Profiles, DefaultClientProfileID: result.DefaultClientProfileID, Defaults: result.Defaults}, nil
}
func (s *MemoryStore) GetModelProfile(_ context.Context, ownerID, profileID string) (ModelProfile, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, p := range s.modelProfiles {
		if strings.HasPrefix(key, ownerID+"\x00") && p.ProfileID == profileID && !p.Deleted {
			return p, true, nil
		}
	}
	return ModelProfile{}, false, nil
}
func (s *MemoryStore) ResolveModelProfile(ctx context.Context, ownerID, profileID string) (ModelProfile, error) {
	p, ok, err := s.GetModelProfile(ctx, ownerID, profileID)
	if err != nil {
		return ModelProfile{}, err
	}
	if !ok {
		return ModelProfile{}, ErrModelProfileNotFound
	}
	return p, nil
}
func (s *MemoryStore) ResolveModelProfilePin(ctx context.Context, ownerID, profileID string) (ModelProfile, error) {
	p, err := s.ResolveModelProfile(ctx, ownerID, profileID)
	if err != nil {
		return ModelProfile{}, err
	}
	// Mirror the database pin projection: no credential is observable on this
	// path, including in test-only memory storage.
	p.APIKey = ""
	p.APIKeyConfigured = false
	return p, nil
}
func (s *MemoryStore) ResolveModelProfileVersion(ctx context.Context, ownerID, profileID string, credentialVersion int64) (ModelProfile, error) {
	profile, err := s.ResolveModelProfile(ctx, ownerID, profileID)
	if err != nil {
		return profile, err
	}
	if credentialVersion > 0 && profile.CredentialVersion != credentialVersion {
		s.mu.RLock()
		credential, ok := s.modelProfileCredentials[profile.ProfileID][credentialVersion]
		s.mu.RUnlock()
		if !ok {
			return ModelProfile{}, ErrModelProfileNotFound
		}
		profile.Provider, profile.APIKey, profile.APIKeyConfigured, profile.CredentialVersion = credential.Provider, credential.APIKey, credential.APIKey != "", credentialVersion
	}
	return profile, nil
}
func (s *MemoryStore) ResolveModelProfilePinned(ctx context.Context, ownerID, profileID string, profileRevision, credentialVersion int64) (ModelProfile, error) {
	if profileRevision <= 0 {
		return s.ResolveModelProfileVersion(ctx, ownerID, profileID, credentialVersion)
	}
	s.mu.RLock()
	profile, ok := s.modelProfileRevisions[memoryModelProfileRevisionKey(ownerID, profileID, profileRevision)]
	if !ok {
		s.mu.RUnlock()
		return ModelProfile{}, ErrModelProfileNotFound
	}
	if credentialVersion > 0 && profile.CredentialVersion != credentialVersion {
		credential, exists := s.modelProfileCredentials[profile.ProfileID][credentialVersion]
		if !exists {
			s.mu.RUnlock()
			return ModelProfile{}, ErrModelProfileNotFound
		}
		profile.Provider, profile.APIKey, profile.APIKeyConfigured, profile.CredentialVersion = credential.Provider, credential.APIKey, credential.APIKey != "", credentialVersion
	}
	s.mu.RUnlock()
	return profile, nil
}
func (s *MemoryStore) DeleteModelProfile(_ context.Context, ownerID, idempotencyKey, profileID string, expected *int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleteKey := ownerID + "\x00" + idempotencyKey
	if prior, ok := s.modelProfileDeletes[deleteKey]; ok {
		if prior.ProfileID != profileID || !bytes.Equal(prior.Digest, profileDeleteDigest(profileID, expected)) {
			return ErrModelProfileIdempotency
		}
		return nil
	}
	for key, p := range s.modelProfiles {
		if strings.HasPrefix(key, ownerID+"\x00") && p.ProfileID == profileID && !p.Deleted {
			if expected != nil && *expected != p.Revision {
				return ErrModelProfileRevision
			}
			p.Deleted = true
			p.Revision++
			p.UpdatedAt = time.Now().UTC()
			s.modelProfiles[key] = p
			s.modelProfileRevisions[memoryModelProfileRevisionKey(ownerID, p.ProfileID, p.Revision)] = p
			defaults := s.modelProfileDefaults[ownerID]
			if defaults.ConversationClientProfileID == p.ClientProfileID {
				defaults.ConversationClientProfileID = ""
			}
			if defaults.EmbeddingClientProfileID == p.ClientProfileID {
				defaults.EmbeddingClientProfileID = ""
			}
			if defaults.SpeechClientProfileID == p.ClientProfileID {
				defaults.SpeechClientProfileID = ""
			}
			s.modelProfileDefaults[ownerID] = defaults
			s.modelProfileDeletes[deleteKey] = memoryModelProfileDelete{ProfileID: profileID, Digest: profileDeleteDigest(profileID, expected)}
			return nil
		}
	}
	return ErrModelProfileNotFound
}
