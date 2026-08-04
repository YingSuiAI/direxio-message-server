package p2p

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestPortalCredentialsFileIsWrittenAndUpdated(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	credentialsPath := filepath.Join(t.TempDir(), "ops", "bootstrap.json")
	t.Setenv("P2P_PORTAL_CREDENTIALS_FILE", credentialsPath)

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}

	initial := readCredentialsFile(t, credentialsPath)
	requireEightDigitPassword(t, initial.Password)
	if initial.AccessToken == "" || initial.AgentToken == "" || initial.DeviceID != "P2P_PORTAL" {
		t.Fatalf("expected default credentials file with tokens, got %#v", initial)
	}
	session := mustHandle[map[string]any](t, service, "portal.bootstrap", map[string]any{"password": initial.Password})
	accessToken := session["access_token"].(string)
	if accessToken == "" {
		t.Fatalf("expected access token from bootstrap, got %#v", session)
	}
	rotated := mustHandle[map[string]any](t, service, "portal.password", map[string]any{
		"old_password": initial.Password,
		"new_password": "new-secret",
	})
	nextAccessToken := rotated["access_token"].(string)
	if nextAccessToken == "" || nextAccessToken == accessToken {
		t.Fatalf("expected rotated access token, got %#v", rotated)
	}
	updated := readCredentialsFile(t, credentialsPath)
	if updated.Password != "new-secret" || updated.AccessToken != nextAccessToken {
		t.Fatalf("expected credentials file to update after password rotation, got %#v", updated)
	}
}

func readCredentialsFile(t *testing.T, path string) portalCredentialsFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var credentials portalCredentialsFile
	if err := json.Unmarshal(data, &credentials); err != nil {
		t.Fatal(err)
	}
	return credentials
}
