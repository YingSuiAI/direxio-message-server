// agent-secretctl manages only the local agent secret keyring. It never
// accepts key material or database credentials on its command line.
package main

import (
	"context"
	"database/sql"
	"errors"
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
		dsn := strings.TrimSpace(os.Getenv("P2P_AGENT_SECRET_DATABASE_DSN"))
		if err := requireAgentSecretDatabaseDSN(dsn); err != nil {
			fail()
		}
		db := openDatabase(dsn)
		defer db.Close()
		if err := verifyAgentSecretDatabase(context.Background(), path, dsn, db); err != nil {
			fail()
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
	case "upgrade":
		dsn := strings.TrimSpace(os.Getenv("P2P_AGENT_SECRET_DATABASE_DSN"))
		if dsn == "" {
			fail()
		}
		db := openDatabase(dsn)
		defer db.Close()
		if err := storage.UpgradeLegacyModelSecrets(context.Background(), db, rotationOptions(path)); err != nil {
			fail()
		}
	default:
		fail()
	}
}

var errAgentSecretDatabaseDSNRequired = errors.New("agent secret database dsn is required")

// verifyAgentSecretDatabase is deliberately the only verify path. A keyring
// check without the database scan is not a successful verification: callers
// must provide the database DSN and every Agent secret row must pass the
// storage-layer validation.
func verifyAgentSecretDatabase(ctx context.Context, path, dsn string, db *sql.DB) error {
	if err := requireAgentSecretDatabaseDSN(dsn); err != nil {
		return err
	}
	if _, err := storage.LoadAgentSecretKeyring(path); err != nil {
		return err
	}
	if db == nil {
		return errors.New("agent secret database is unavailable")
	}
	return storage.VerifyAgentSecretDatabase(ctx, db, rotationOptions(path))
}

func requireAgentSecretDatabaseDSN(dsn string) error {
	if strings.TrimSpace(dsn) == "" {
		return errAgentSecretDatabaseDSNRequired
	}
	return nil
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
