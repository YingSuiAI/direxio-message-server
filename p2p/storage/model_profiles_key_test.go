package storage

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestModelProfileMasterKeyCreatesWithRestrictedPermissionsAndFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "model-profile.key")
	key, err := loadOrCreateModelProfileKey(path, false)
	if err != nil || len(key) != 32 {
		t.Fatalf("create key len=%d err=%v", len(key), err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions = %v", info.Mode().Perm())
	}
	reloaded, err := loadOrCreateModelProfileKey(path, true)
	if err != nil || string(reloaded) != string(key) {
		t.Fatalf("reloaded key mismatch: err=%v", err)
	}
	if _, err := loadOrCreateModelProfileKey(filepath.Join(filepath.Dir(path), "missing"), true); err == nil {
		t.Fatal("missing key accepted with encrypted rows")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "wrong"), []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateModelProfileKey(filepath.Join(filepath.Dir(path), "wrong"), false); err == nil {
		t.Fatal("wrong key length accepted")
	}
}

func TestMemoryModelProfileSyncIdempotencyIsConcurrentSafe(t *testing.T) {
	store := NewMemoryStore()
	entry := []ModelProfileSyncEntry{{ClientProfileID: "client-1", Provider: "deepseek", Model: "deepseek-chat", APIKey: stringPtr("secret")}}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.SyncModelProfiles(context.Background(), "owner", "same", "client-1", entry)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	profiles, err := store.ListModelProfiles(context.Background(), "owner", 0, "")
	if err != nil || len(profiles.Profiles) != 1 || profiles.Profiles[0].Revision != 1 {
		t.Fatalf("idempotent profiles = %#v, err=%v", profiles, err)
	}
}

func TestMemoryModelProfilePinnedRevisionSurvivesLogicalDelete(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	first, err := store.SyncModelProfiles(ctx, "owner", "create", "client-1", []ModelProfileSyncEntry{{ClientProfileID: "client-1", Provider: "deepseek", Model: "deepseek-chat", APIKey: stringPtr("secret-v1")}})
	if err != nil || len(first.Profiles) != 1 {
		t.Fatalf("create: %#v, %v", first, err)
	}
	profile := first.Profiles[0]
	if _, err := store.SyncModelProfiles(ctx, "owner", "rotate", "client-1", []ModelProfileSyncEntry{{ClientProfileID: "client-1", Provider: "deepseek", Model: "deepseek-reasoner", APIKey: stringPtr("secret-v2"), ExpectedRevision: int64Ptr(profile.Revision)}}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	current, _, _ := store.GetModelProfile(ctx, "owner", profile.ProfileID)
	if err := store.DeleteModelProfile(ctx, "owner", "delete", profile.ProfileID, int64Ptr(current.Revision)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.ResolveModelProfile(ctx, "owner", profile.ProfileID); err != ErrModelProfileNotFound {
		t.Fatalf("deleted profile resolved: %v", err)
	}
	pinned, err := store.ResolveModelProfilePinned(ctx, "owner", profile.ProfileID, profile.Revision, profile.CredentialVersion)
	if err != nil || pinned.APIKey != "secret-v1" || pinned.Model != "deepseek-chat" {
		t.Fatalf("pinned historical profile = %#v, err=%v", pinned, err)
	}
}

func TestMemoryModelProfileReactivationProviderRotationMatchesPinnedHistory(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	created, err := store.SyncModelProfiles(ctx, "owner", "create-reactivate", "", []ModelProfileSyncEntry{{
		ClientProfileID: "client-1", Provider: "deepseek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat", APIKey: stringPtr("secret-v1"),
	}})
	if err != nil || len(created.Profiles) != 1 {
		t.Fatalf("create: %#v, %v", created, err)
	}
	initial := created.Profiles[0]
	if err := store.DeleteModelProfile(ctx, "owner", "delete-reactivate", initial.ProfileID, int64Ptr(initial.Revision)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.SyncModelProfiles(ctx, "owner", "reactivate", "", []ModelProfileSyncEntry{{
		ClientProfileID: "client-1", Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o", ExpectedRevision: int64Ptr(initial.Revision + 1),
	}}); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	current, err := store.ResolveModelProfile(ctx, "owner", initial.ProfileID)
	if err != nil {
		t.Fatalf("current resolve: %v", err)
	}
	if current.Revision != initial.Revision+2 || current.CredentialVersion != initial.CredentialVersion+1 || current.Provider != "openai" || current.Model != "gpt-4o" || current.APIKey != "secret-v1" {
		t.Fatalf("current reactivated profile = %#v", current)
	}
	old, err := store.ResolveModelProfilePinned(ctx, "owner", initial.ProfileID, initial.Revision, initial.CredentialVersion)
	if err != nil || old.Provider != "deepseek" || old.Model != "deepseek-chat" || old.APIKey != "secret-v1" {
		t.Fatalf("old pinned profile = %#v, err=%v", old, err)
	}
	latest, err := store.ResolveModelProfilePinned(ctx, "owner", initial.ProfileID, current.Revision, current.CredentialVersion)
	if err != nil || latest.Provider != "openai" || latest.Model != "gpt-4o" || latest.APIKey != "secret-v1" {
		t.Fatalf("latest pinned profile = %#v, err=%v", latest, err)
	}
}

func TestMemoryModelProfileCursorPaginationMatchesDatabaseSemantics(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	for i, clientID := range []string{"client-a", "client-b"} {
		if _, err := store.SyncModelProfiles(ctx, "owner", "cursor-"+clientID, "", []ModelProfileSyncEntry{{ClientProfileID: clientID, Provider: "deepseek", Model: "model", APIKey: agentTestStringPtr("secret-" + string(rune('a'+i)))}}); err != nil {
			t.Fatalf("sync %s: %v", clientID, err)
		}
	}
	page, err := store.ListModelProfiles(ctx, "owner", 1, "")
	if err != nil || len(page.Profiles) != 1 || page.Profiles[0].ClientProfileID != "client-a" || page.NextPageToken == "" {
		t.Fatalf("first memory page = %#v, err=%v", page, err)
	}
	page2, err := store.ListModelProfiles(ctx, "owner", 1, page.NextPageToken)
	if err != nil || len(page2.Profiles) != 1 || page2.Profiles[0].ClientProfileID != "client-b" || page2.NextPageToken != "" {
		t.Fatalf("second memory page = %#v, err=%v", page2, err)
	}
	if _, err := store.ListModelProfiles(ctx, "owner", 1, "deadbeef"); err != ErrModelProfileInvalid {
		t.Fatalf("invalid memory cursor err=%v", err)
	}
}

func agentTestStringPtr(value string) *string { return &value }

func TestMemoryModelProfileSyncCommitsAtomicallyAfterValidation(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	created, err := store.SyncModelProfiles(ctx, "owner", "atomic-create", "", []ModelProfileSyncEntry{{ClientProfileID: "client-1", Provider: "deepseek", Model: "old", APIKey: agentTestStringPtr("secret")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncModelProfiles(ctx, "owner", "atomic-later-error", "", []ModelProfileSyncEntry{
		{ClientProfileID: "client-1", ExpectedRevision: agentInt64StoragePtr(created.Profiles[0].Revision), Provider: "deepseek", Model: "new"},
		{ClientProfileID: "client-2", ExpectedRevision: agentInt64StoragePtr(99), Provider: "deepseek", Model: "never"},
	}); err != ErrModelProfileRevision {
		t.Fatalf("later entry error = %v", err)
	}
	unchanged, err := store.ResolveModelProfile(ctx, "owner", created.Profiles[0].ProfileID)
	if err != nil || unchanged.Revision != created.Profiles[0].Revision || unchanged.Model != "old" {
		t.Fatalf("partial later-entry commit = %#v err=%v", unchanged, err)
	}
	if _, err := store.SyncModelProfiles(ctx, "owner", "atomic-default-error", "missing-default", []ModelProfileSyncEntry{{ClientProfileID: "client-2", Provider: "deepseek", Model: "never"}}); err != ErrModelProfileNotFound {
		t.Fatalf("default error = %v", err)
	}
	profiles, err := store.ListModelProfiles(ctx, "owner", 0, "")
	if err != nil || len(profiles.Profiles) != 1 || profiles.Profiles[0].ClientProfileID != "client-1" {
		t.Fatalf("partial default commit = %#v err=%v", profiles, err)
	}
}

func agentInt64StoragePtr(value int64) *int64 { return &value }
