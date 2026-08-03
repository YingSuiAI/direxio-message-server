package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestVerifyAgentSecretDatabaseRequiresDSN(t *testing.T) {
	err := verifyAgentSecretDatabase(context.Background(), filepath.Join(t.TempDir(), "missing.json"), "  ", nil)
	if !errors.Is(err, errAgentSecretDatabaseDSNRequired) {
		t.Fatalf("verify error = %v, want required DSN", err)
	}
}

func TestVerifyAgentSecretDatabaseRequiresKeyringAndDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring", "agent-secrets.json")
	if _, err := storage.LoadOrCreateAgentSecretKeyring(path); err != nil {
		t.Fatal(err)
	}
	if err := verifyAgentSecretDatabase(context.Background(), path, "postgres://placeholder", nil); err == nil {
		t.Fatal("verify unexpectedly succeeded without a database")
	}

	var db *sql.DB
	if err := verifyAgentSecretDatabase(context.Background(), filepath.Join(t.TempDir(), "missing.json"), "postgres://placeholder", db); err == nil {
		t.Fatal("verify unexpectedly succeeded without a keyring")
	}
}
