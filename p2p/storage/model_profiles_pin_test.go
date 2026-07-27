package storage

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMemoryModelProfilePinOmitsCredential(t *testing.T) {
	store := NewMemoryStore()
	key := "metadata-pin-canary"
	result, err := store.SyncModelProfiles(context.Background(), "owner", "pin-create", "client", []ModelProfileSyncEntry{{ClientProfileID: "client", DisplayName: "profile", Provider: "openai", BaseURL: "https://example.invalid", Model: "test", APIKey: &key}})
	if err != nil {
		t.Fatal(err)
	}
	pin, err := store.ResolveModelProfilePin(context.Background(), "owner", result.Profiles[0].ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if pin.APIKey != "" || pin.APIKeyConfigured {
		t.Fatalf("pin exposed credential: %#v", pin)
	}
	if pin.Revision != result.Profiles[0].Revision || pin.CredentialVersion != result.Profiles[0].CredentialVersion {
		t.Fatalf("pin did not preserve versions: %#v", pin)
	}
	resolved, err := store.ResolveModelProfilePinned(context.Background(), "owner", pin.ProfileID, pin.Revision, pin.CredentialVersion)
	if err != nil || resolved.APIKey != key {
		t.Fatalf("pinned resolve = %#v, %v", resolved, err)
	}
}

func TestDatabaseModelProfilePinUsesMetadataProjectionOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &encryptedModelProfileStore{db: db}
	query := `SELECT profile_id,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,model_kind,input_modalities,provider_config,revision,credential_version,created_at,updated_at FROM p2p_agent_model_profiles WHERE owner_id=$1 AND profile_id=$2 AND deleted_at IS NULL`
	// The expectation contains the exact dedicated projection; ciphertext and
	// nonce must never be selected on the schedule acceptance path.
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("owner", "profile").WillReturnRows(sqlmock.NewRows([]string{"profile_id", "client_profile_id", "display_name", "provider", "base_url", "model", "system_prompt", "temperature", "top_p", "max_output_tokens", "context_window", "reasoning_effort", "model_kind", "input_modalities", "provider_config", "revision", "credential_version", "created_at", "updated_at"}).AddRow("profile", "client", "name", "openai", "https://example.invalid", "m", "", nil, nil, 0, 0, "", "conversation", []byte(`["text"]`), []byte(`{}`), 7, 9, time.Now(), time.Now()))
	pin, err := store.ResolveModelProfilePin(context.Background(), "owner", "profile")
	if err != nil {
		t.Fatal(err)
	}
	if pin.APIKey != "" || pin.APIKeyConfigured || pin.Revision != 7 || pin.CredentialVersion != 9 {
		t.Fatalf("unsafe pin: %#v", pin)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
