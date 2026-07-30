package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestPostgresOpenRouterSpeechAPIKeyRoundTripAndCredentialShapeTransitions(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keyringDir := t.TempDir()
	if err := os.Chmod(keyringDir, 0700); err != nil {
		t.Fatal(err)
	}
	keyringPath := filepath.Join(keyringDir, "secret-keyring.json")
	if _, err := LoadOrCreateAgentSecretKeyring(keyringPath); err != nil {
		t.Fatal(err)
	}
	profiles, err := NewDatabaseModelProfileStoreWithKeyring(ctx, store, keyringPath, "")
	if err != nil {
		t.Fatal(err)
	}
	openRouterKey := "sk-openrouter-speech-1234"
	result, err := profiles.SyncModelProfiles(ctx, "owner", "openrouter-speech-create", "", []ModelProfileSyncEntry{{
		ClientProfileID: "speech", Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "provider/tts", ModelKind: ModelKindSpeech, APIKey: &openRouterKey,
	}})
	if err != nil || len(result.Profiles) != 1 {
		t.Fatalf("openrouter speech sync: %#v, %v", result, err)
	}
	created := result.Profiles[0]
	if created.Provider != "openrouter" || created.ModelKind != ModelKindSpeech || !created.APIKeyConfigured || created.CredentialVersion != 1 || created.APIKeyHint != "sk-********1234" || created.ProviderSecretStatus != nil {
		t.Fatalf("openrouter speech metadata = %#v", created)
	}
	var ciphertext []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT api_key_ciphertext FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2`, "owner", created.ProfileID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), openRouterKey) {
		t.Fatal("OpenRouter speech ciphertext contains plaintext API key")
	}
	listed, err := profiles.ListModelProfiles(ctx, "owner", 0, "")
	if err != nil || len(listed.Profiles) != 1 {
		t.Fatalf("openrouter speech list: %#v, %v", listed, err)
	}
	readback, err := profiles.ResolveModelProfile(ctx, "owner", created.ProfileID)
	if err != nil || readback.APIKey != openRouterKey || readback.APIKeyHint != "sk-********1234" || !readback.APIKeyConfigured || readback.CredentialVersion != 1 || readback.ProviderSecretStatus != nil {
		t.Fatalf("openrouter speech readback: %#v, %v", readback, err)
	}
	redacted, _ := json.Marshal(readback)
	if strings.Contains(string(redacted), openRouterKey) || strings.Contains(string(redacted), "provider_secret_status") {
		t.Fatalf("openrouter speech redaction = %s", redacted)
	}
	if _, err := profiles.SyncModelProfiles(ctx, "owner", "openrouter-to-volc-missing", "", []ModelProfileSyncEntry{{
		ClientProfileID: "speech", Provider: "volc_voice", ModelKind: ModelKindSpeech, ExpectedRevision: &created.Revision,
	}}); err != ErrModelProfileInvalid {
		t.Fatalf("OpenRouter to Volc without bundle err=%v", err)
	}
	volc, err := profiles.SyncModelProfiles(ctx, "owner", "openrouter-to-volc", "", []ModelProfileSyncEntry{{
		ClientProfileID: "speech", Provider: "volc_voice", ModelKind: ModelKindSpeech, ExpectedRevision: &created.Revision,
		ProviderConfig: map[string]any{"app_id": "app"}, ProviderSecrets: map[string]string{"rtc_app_key": "rtc", "access_key_id": "access", "secret_access_key": "secret"},
	}})
	if err != nil {
		t.Fatalf("OpenRouter to Volc transition: %v", err)
	}
	volcProfile := volc.Profiles[0]
	if volcProfile.CredentialVersion != created.CredentialVersion+1 || volcProfile.Provider != "volc_voice" {
		t.Fatalf("Volc transition metadata = %#v", volcProfile)
	}
	if _, err := profiles.SyncModelProfiles(ctx, "owner", "volc-to-openrouter-missing", "", []ModelProfileSyncEntry{{
		ClientProfileID: "speech", Provider: "openrouter", ModelKind: ModelKindSpeech, ExpectedRevision: &volcProfile.Revision,
	}}); err != ErrModelProfileInvalid {
		t.Fatalf("Volc to OpenRouter without API key err=%v", err)
	}
	newKey := "sk-openrouter-speech-5678"
	rotated, err := profiles.SyncModelProfiles(ctx, "owner", "volc-to-openrouter", "", []ModelProfileSyncEntry{{
		ClientProfileID: "speech", Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "provider/tts-v2", ModelKind: ModelKindSpeech, APIKey: &newKey, ExpectedRevision: &volcProfile.Revision,
	}})
	if err != nil {
		t.Fatalf("Volc to OpenRouter transition: %v", err)
	}
	final := rotated.Profiles[0]
	if final.CredentialVersion != volcProfile.CredentialVersion+1 || final.Provider != "openrouter" || final.APIKeyHint != "sk-********5678" || final.ProviderSecretStatus != nil {
		t.Fatalf("final OpenRouter speech metadata = %#v", final)
	}
}

func TestPostgresModelProfileSyncReadbackAndRestart(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	keyPath := filepath.Join(t.TempDir(), "model-profile.key")
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	var fkExists bool
	if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='p2p_agent_model_profile_credentials_owner_id_profile_id_fkey')`).Scan(&fkExists); err != nil || !fkExists {
		t.Fatalf("credential profile FK exists=%v err=%v", fkExists, err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO p2p_agent_model_profile_credentials(owner_id,profile_id,credential_version,provider,api_key_nonce,api_key_ciphertext,created_at) VALUES('orphan-owner','orphan-profile',1,'deepseek',decode('00','hex'),decode('00','hex'),$1)`, time.Now().UTC()); err == nil {
		t.Fatal("orphan credential insert unexpectedly succeeded")
	}
	profiles, err := NewDatabaseModelProfileStore(ctx, store, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := profiles.SyncModelProfiles(ctx, "owner", "sync-1", "client-1", []ModelProfileSyncEntry{{ClientProfileID: "client-1", Provider: "deepseek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat", APIKey: stringPtr("pg-secret")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Profiles) != 1 || !result.Profiles[0].APIKeyConfigured {
		t.Fatalf("sync result = %#v", result)
	}
	var ciphertext []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT api_key_ciphertext FROM p2p_agent_model_profiles WHERE owner_id='owner' AND client_profile_id='client-1'`).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "pg-secret") {
		t.Fatal("ciphertext contains plaintext API key")
	}
	profile, err := profiles.ResolveModelProfile(ctx, "owner", result.Profiles[0].ProfileID)
	if err != nil || profile.APIKey != "pg-secret" || profile.APIKeyHint != "********" {
		t.Fatalf("readback profile key configured=%v err=%v", profile.APIKeyConfigured, err)
	}
	if _, err := profiles.SyncModelProfiles(ctx, "owner", "sync-list-2", "", []ModelProfileSyncEntry{{ClientProfileID: "client-2", Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"}}); err != nil {
		t.Fatalf("second profile: %v", err)
	}
	page, err := profiles.ListModelProfiles(ctx, "owner", 1, "")
	if err != nil || len(page.Profiles) != 1 || page.NextPageToken == "" {
		t.Fatalf("first profile page = %#v, err=%v", page, err)
	}
	page2, err := profiles.ListModelProfiles(ctx, "owner", 1, page.NextPageToken)
	if err != nil || len(page2.Profiles) != 1 || page2.Profiles[0].ClientProfileID != "client-2" || page2.NextPageToken != "" {
		t.Fatalf("second profile page = %#v, err=%v", page2, err)
	}
	if _, err := profiles.ListModelProfiles(ctx, "owner", 1, "deadbeef"); err != ErrModelProfileInvalid {
		t.Fatalf("invalid profile cursor err=%v", err)
	}
	rotated, err := profiles.SyncModelProfiles(ctx, "owner", "sync-2", "client-1", []ModelProfileSyncEntry{{ClientProfileID: "client-1", ExpectedRevision: int64Ptr(profile.Revision), Provider: "deepseek", BaseURL: profile.BaseURL, Model: "deepseek-reasoner", APIKey: stringPtr("pg-secret-2")}})
	if err != nil {
		t.Fatal(err)
	}
	current := rotated.Profiles[0]
	if current.Revision != profile.Revision+1 || current.CredentialVersion != profile.CredentialVersion+1 {
		t.Fatalf("rotation versions = profile %d/%d, want %d/%d", current.Revision, current.CredentialVersion, profile.Revision+1, profile.CredentialVersion+1)
	}
	pinned, err := profiles.ResolveModelProfilePinned(ctx, "owner", current.ProfileID, profile.Revision, profile.CredentialVersion)
	if err != nil || pinned.APIKey != "pg-secret" || pinned.Model != profile.Model {
		t.Fatalf("old pinned profile = %#v, err=%v", pinned, err)
	}
	if err := profiles.DeleteModelProfile(ctx, "owner", "delete-1", current.ProfileID, int64Ptr(current.Revision)); err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.ResolveModelProfile(ctx, "owner", current.ProfileID); err != ErrModelProfileNotFound {
		t.Fatalf("deleted current profile err=%v", err)
	}
	pinned, err = profiles.ResolveModelProfilePinned(ctx, "owner", current.ProfileID, current.Revision+1, current.CredentialVersion)
	if err != nil || pinned.APIKey != "pg-secret-2" {
		t.Fatalf("deleted pinned profile = %#v, err=%v", pinned, err)
	}
	reactivated, err := profiles.SyncModelProfiles(ctx, "owner", "sync-reactivate", "client-1", []ModelProfileSyncEntry{{ClientProfileID: "client-1", ExpectedRevision: int64Ptr(current.Revision + 1), Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"}})
	if err != nil || len(reactivated.Profiles) < 1 {
		t.Fatalf("reactivation = %#v, err=%v", reactivated, err)
	}
	active := reactivated.Profiles[0]
	if active.Revision != current.Revision+2 || active.CredentialVersion != current.CredentialVersion+1 || active.Provider != "openai" || active.Model != "gpt-4o" || active.APIKey != "pg-secret-2" {
		t.Fatalf("reactivated current profile = %#v", active)
	}
	pinned, err = profiles.ResolveModelProfilePinned(ctx, "owner", active.ProfileID, current.Revision, current.CredentialVersion)
	if err != nil || pinned.Provider != "deepseek" || pinned.Model != "deepseek-reasoner" || pinned.APIKey != "pg-secret-2" {
		t.Fatalf("pre-delete pinned profile after reactivation = %#v, err=%v", pinned, err)
	}
	pinned, err = profiles.ResolveModelProfilePinned(ctx, "owner", active.ProfileID, active.Revision, active.CredentialVersion)
	if err != nil || pinned.Provider != "openai" || pinned.Model != "gpt-4o" || pinned.APIKey != "pg-secret-2" {
		t.Fatalf("reactivated pinned profile = %#v, err=%v", pinned, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- profiles.DeleteModelProfile(ctx, "owner", "delete-concurrent", active.ProfileID, int64Ptr(active.Revision))
		}()
	}
	wg.Wait()
	close(errs)
	for deleteErr := range errs {
		if deleteErr != nil {
			t.Fatalf("concurrent delete: %v", deleteErr)
		}
	}
	if err := profiles.DeleteModelProfile(ctx, "owner", "delete-concurrent", active.ProfileID, int64Ptr(active.Revision+1)); err != ErrModelProfileIdempotency {
		t.Fatalf("delete idempotency mismatch err=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := NewDatabaseModelProfileStore(ctx, reopened, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	profile, err = restarted.ResolveModelProfilePinned(ctx, "owner", current.ProfileID, active.Revision, active.CredentialVersion)
	if err != nil || profile.APIKey != "pg-secret-2" {
		t.Fatalf("restart pinned profile key configured=%v err=%v", profile.APIKeyConfigured, err)
	}
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key mode/info: %v", err)
	}
}

func TestPostgresModelProfileSpeechCredentialTransitionPins(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, constraint := range []string{"p2p_agent_model_profile_defaults_owner_profile_fkey", "p2p_agent_model_profile_defaults_owner_embedding_fkey", "p2p_agent_model_profile_defaults_owner_speech_fkey"} {
		var exists bool
		if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname=$1)`, constraint).Scan(&exists); err != nil || !exists {
			t.Fatalf("default constraint %s exists=%v err=%v", constraint, exists, err)
		}
	}
	profiles, err := NewDatabaseModelProfileStore(ctx, store, filepath.Join(t.TempDir(), "model-profile.key"))
	if err != nil {
		t.Fatal(err)
	}
	key := "generic-secret"
	first, err := profiles.SyncModelProfiles(ctx, "owner", "transition-create", "", []ModelProfileSyncEntry{{ClientProfileID: "client", Provider: "openai", Model: "gpt", APIKey: &key}})
	if err != nil {
		t.Fatal(err)
	}
	old := first.Profiles[0]
	rotated, err := profiles.SyncModelProfilesWithDefaults(ctx, "owner", "transition-speech", ModelProfileDefaults{}, []ModelProfileSyncEntry{{ClientProfileID: "client", Provider: "volc_voice", ModelKind: ModelKindSpeech, ProviderConfig: map[string]any{"app_id": "app"}, ProviderSecrets: map[string]string{"rtc_app_key": "rtc", "access_key_id": "access", "secret_access_key": "secret"}, ExpectedRevision: &old.Revision}})
	if err != nil {
		t.Fatal(err)
	}
	current := rotated.Profiles[0]
	if current.CredentialVersion != old.CredentialVersion+1 || current.ModelKind != ModelKindSpeech {
		t.Fatalf("transition versions = %#v", current)
	}
	oldPin, err := profiles.ResolveModelProfilePinned(ctx, "owner", current.ProfileID, old.Revision, old.CredentialVersion)
	if err != nil || oldPin.APIKey != key {
		t.Fatalf("old pin = %#v err=%v", oldPin, err)
	}
	newPin, err := profiles.ResolveModelProfilePinned(ctx, "owner", current.ProfileID, current.Revision, current.CredentialVersion)
	if err != nil || !strings.Contains(newPin.APIKey, "rtc_app_key") || strings.Contains(newPin.APIKey, key) {
		t.Fatalf("new speech pin = %#v err=%v", newPin, err)
	}
}

func stringPtr(value string) *string { return &value }
func int64Ptr(value int64) *int64    { return &value }
