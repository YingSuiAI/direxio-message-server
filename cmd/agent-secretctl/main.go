// agent-secretctl manages only the local agent secret keyring. It never
// accepts key material or database credentials on its command line.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	_ "github.com/lib/pq"
)

func main() {
	if len(os.Args) != 2 {
		fail()
	}
	path := strings.TrimSpace(os.Getenv("P2P_AGENT_SECRET_KEYRING_FILE"))
	if path == "" {
		fail()
	}
	switch os.Args[1] {
	case "init":
		dsn := strings.TrimSpace(os.Getenv("P2P_AGENT_SECRET_DATABASE_DSN"))
		if dsn == "" {
			fail()
		}
		db := openDatabase(dsn)
		defer db.Close()
		if _, err := storage.InitializeAgentSecretKeyringForDatabase(context.Background(), db, path); err != nil {
			fail()
		}
	case "verify":
		if _, err := storage.LoadAgentSecretKeyring(path); err != nil {
			fail()
		}
		dsn := strings.TrimSpace(os.Getenv("P2P_AGENT_SECRET_DATABASE_DSN"))
		if dsn != "" {
			db := openDatabase(dsn)
			defer db.Close()
			if err := storage.VerifyAgentSecretDatabase(context.Background(), db, rotationOptions(path)); err != nil {
				fail()
			}
		}
	case "rotate":
		dsn := strings.TrimSpace(os.Getenv("P2P_AGENT_SECRET_DATABASE_DSN"))
		if dsn == "" {
			fail()
		}
		db := openDatabase(dsn)
		defer db.Close()
		if err := storage.RotateAgentSecrets(context.Background(), db, rotationOptions(path)); err != nil {
			fail()
		}
	default:
		fail()
	}
}

func rotationOptions(path string) storage.AgentSecretRotationOptions {
	return storage.AgentSecretRotationOptions{
		KeyringFile:               path,
		LegacyModelProfileKeyFile: strings.TrimSpace(os.Getenv("P2P_AGENT_MODEL_PROFILE_KEY_FILE")),
		LeaseOwner:                strings.TrimSpace(os.Getenv("P2P_AGENT_SECRET_ROTATION_OWNER")),
	}
}

func openDatabase(dsn string) *sql.DB {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		fail()
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		fail()
	}
	return db
}

func fail() {
	fmt.Fprintln(os.Stderr, "agent secret keyring operation failed")
	os.Exit(1)
}
