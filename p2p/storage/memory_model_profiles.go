package storage

import (
	"bytes"
	"context"
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

func (s *MemoryStore) SyncModelProfiles(_ context.Context, ownerID, idempotencyKey string, defaultClientID string, entries []ModelProfileSyncEntry) (ModelProfileSyncResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return ModelProfileSyncResult{}, ErrModelProfileInvalid
	}
	digest := profileSyncDigest(defaultClientID, entries)
	syncKey := ownerID + "\x00" + idempotencyKey
	if prior, ok := s.modelProfileSyncs[syncKey]; ok {
		if prior.Digest != digest {
			return ModelProfileSyncResult{}, ErrModelProfileIdempotency
		}
		return prior.Result, nil
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.ClientProfileID) == "" || strings.TrimSpace(entry.Provider) == "" || (entry.APIKey != nil && strings.TrimSpace(*entry.APIKey) == "") {
			return ModelProfileSyncResult{}, ErrModelProfileInvalid
		}
	}
	profiles := cloneModelProfiles(s.modelProfiles)
	revisions := cloneModelProfiles(s.modelProfileRevisions)
	credentials := cloneModelProfileCredentials(s.modelProfileCredentials)
	defaults := cloneModelProfileDefaults(s.modelProfileDefaults)
	for _, entry := range entries {
		key := ownerID + "\x00" + entry.ClientProfileID
		profile, ok := profiles[key]
		if !ok {
			profile = ModelProfile{ProfileID: uuid.NewString(), ClientProfileID: entry.ClientProfileID, Revision: 0, CreatedAt: time.Now().UTC()}
		}
		if entry.ExpectedRevision != nil && *entry.ExpectedRevision != profile.Revision {
			return ModelProfileSyncResult{}, ErrModelProfileRevision
		}
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		credentialRotated := entry.APIKey != nil || (profile.Provider != "" && profile.Provider != provider && profile.APIKeyConfigured)
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
		profile.BaseURL, profile.Model, profile.SystemPrompt = strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/"), strings.TrimSpace(entry.Model), strings.TrimSpace(entry.SystemPrompt)
		profile.Temperature, profile.TopP = entry.Temperature, entry.TopP
		profile.MaxOutputTokens, profile.ContextWindow, profile.ReasoningEffort = entry.MaxOutputTokens, entry.ContextWindow, strings.TrimSpace(entry.ReasoningEffort)
		profile.Revision++
		profile.Deleted = false
		profile.UpdatedAt = time.Now().UTC()
		profiles[key] = profile
		revisions[memoryModelProfileRevisionKey(ownerID, profile.ProfileID, profile.Revision)] = profile
	}
	if defaultClientID != "" {
		if profile, ok := profiles[ownerID+"\x00"+defaultClientID]; !ok || profile.Deleted {
			return ModelProfileSyncResult{}, ErrModelProfileNotFound
		}
		defaults[ownerID] = defaultClientID
	}
	result := listMemoryProfiles(profiles, defaults, ownerID)
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

func listMemoryProfiles(profilesByKey map[string]ModelProfile, defaults map[string]string, ownerID string) ModelProfileSyncResult {
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
	return ModelProfileSyncResult{Profiles: profiles, DefaultClientProfileID: defaults[ownerID]}
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

func cloneModelProfileDefaults(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for ownerID, clientProfileID := range source {
		copy[ownerID] = clientProfileID
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
		return ModelProfileListResult{Profiles: result.Profiles[:pageSize], NextPageToken: encodeModelProfilePageToken(last.ClientProfileID, last.ProfileID), DefaultClientProfileID: result.DefaultClientProfileID}, nil
	}
	return ModelProfileListResult{Profiles: result.Profiles, DefaultClientProfileID: result.DefaultClientProfileID}, nil
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
			if s.modelProfileDefaults[ownerID] == p.ClientProfileID {
				delete(s.modelProfileDefaults, ownerID)
			}
			s.modelProfileDeletes[deleteKey] = memoryModelProfileDelete{ProfileID: profileID, Digest: profileDeleteDigest(profileID, expected)}
			return nil
		}
	}
	return ErrModelProfileNotFound
}
