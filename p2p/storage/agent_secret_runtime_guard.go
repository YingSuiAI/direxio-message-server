package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// agentSecretAdvisoryLockKey is a stable, cluster-wide PostgreSQL advisory
// lock namespace for the Agent secret subsystem.  A live server holds the
// shared variant for its process lifetime; maintenance commands use the
// exclusive variant before touching the keyring or encrypted rows.
//
// This value is deliberately a documented constant rather than a hash so
// independently deployed images fence each other identically.
const agentSecretAdvisoryLockKey int64 = 0x4454584147454E54 // "DTXAGENT"

var ErrAgentSecretRuntimeGuard = errors.New("agent secret runtime guard unavailable")

// AgentSecretRuntimeGuard keeps a PostgreSQL session-scoped shared advisory
// lock. Call Close during process shutdown. Closing the connection is also a
// fail-safe release if an explicit unlock cannot be sent.
type AgentSecretRuntimeGuard struct {
	mu   sync.Mutex
	conn *sql.Conn
}

// AcquireAgentSecretRuntimeGuard marks a process as actively serving Agent
// secrets. A concurrent rotation must fail rather than modifying its keyring.
func AcquireAgentSecretRuntimeGuard(ctx context.Context, db *sql.DB) (*AgentSecretRuntimeGuard, error) {
	if db == nil {
		return nil, ErrAgentSecretRuntimeGuard
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, ErrAgentSecretRuntimeGuard
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock_shared($1)", agentSecretAdvisoryLockKey).Scan(&acquired); err != nil || !acquired {
		_ = conn.Close()
		return nil, ErrAgentSecretRuntimeGuard
	}
	return &AgentSecretRuntimeGuard{conn: conn}, nil
}

func (g *AgentSecretRuntimeGuard) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if g.conn == nil {
		g.mu.Unlock()
		return nil
	}
	conn := g.conn
	g.conn = nil
	g.mu.Unlock()
	// Use Background: caller shutdown contexts are commonly already canceled,
	// but the connection close below still guarantees eventual release.
	var released bool
	err := conn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock_shared($1)", agentSecretAdvisoryLockKey).Scan(&released)
	closeErr := conn.Close()
	if err != nil || !released || closeErr != nil {
		return ErrAgentSecretRuntimeGuard
	}
	return nil
}

// acquireAgentSecretMaintenanceGuard obtains the exclusive side of the same
// session-scoped lock. It intentionally does not wait: a live service is a
// hard fail-closed condition for offline maintenance.
func acquireAgentSecretMaintenanceGuard(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	if db == nil {
		return nil, ErrAgentSecretRuntimeGuard
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, ErrAgentSecretRuntimeGuard
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", agentSecretAdvisoryLockKey).Scan(&acquired); err != nil || !acquired {
		_ = conn.Close()
		return nil, ErrAgentSecretRuntimeGuard
	}
	return conn, nil
}

func releaseAgentSecretMaintenanceGuard(conn *sql.Conn) {
	if conn == nil {
		return
	}
	var released bool
	_ = conn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1)", agentSecretAdvisoryLockKey).Scan(&released)
	_ = conn.Close()
}

// InitializeAgentSecretKeyringForDatabase creates an absent keyring only
// after proving that no ciphertext already bound to a keyring exists. Legacy
// model-profile rows with a blank key_id are intentionally excluded: they are
// opened by the separate legacy master key and must be upgraded after this
// new keyring is initialized. If keyring-bound ciphertext exists, operators
// must restore the matching keyring backup.
func InitializeAgentSecretKeyringForDatabase(ctx context.Context, db *sql.DB, path string) (*AgentSecretKeyring, error) {
	path = strings.TrimSpace(path)
	if db == nil || path == "" {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	if _, err := os.Stat(path); err == nil {
		return LoadAgentSecretKeyring(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	conn, err := acquireAgentSecretMaintenanceGuard(ctx, db)
	if err != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	defer releaseAgentSecretMaintenanceGuard(conn)
	// Recheck only after fencing a possible concurrent service/init command.
	if _, err := os.Stat(path); err == nil {
		return LoadAgentSecretKeyring(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	if err := agentSecretDatabaseHasCiphertext(ctx, conn); err != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	return LoadOrCreateAgentSecretKeyring(path)
}

type agentSecretCiphertextStore struct {
	table, column, keyIDColumn string
}

func agentSecretDatabaseHasCiphertext(ctx context.Context, conn *sql.Conn) error {
	for _, store := range []agentSecretCiphertextStore{
		{table: "p2p_agent_model_profiles", column: "api_key_ciphertext", keyIDColumn: "api_key_key_id"},
		{table: "p2p_agent_model_profile_credentials", column: "api_key_ciphertext", keyIDColumn: "api_key_key_id"},
		{table: "p2p_agent_secrets", column: "ciphertext"},
	} {
		present, err := agentSecretTableColumnExists(ctx, conn, store.table, store.column)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		// Both identifiers are compile-time constants above. Keeping this
		// dynamic only after catalog verification lets additive deployments
		// safely run before a later table migration exists.
		keyPredicate := ""
		if store.keyIDColumn != "" {
			keyPresent, err := agentSecretTableColumnExists(ctx, conn, store.table, store.keyIDColumn)
			if err != nil {
				return err
			}
			if !keyPresent {
				continue
			}
			keyPredicate = fmt.Sprintf(" AND %s<>''", store.keyIDColumn)
		}
		query := fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE %s IS NOT NULL AND octet_length(%s)>0%s)", store.table, store.column, store.column, keyPredicate)
		var found bool
		if err := conn.QueryRowContext(ctx, query).Scan(&found); err != nil {
			return err
		}
		if found {
			return ErrAgentSecretKeyringUnavailable
		}
	}
	return nil
}

func agentSecretTableColumnExists(ctx context.Context, conn *sql.Conn, table, column string) (bool, error) {
	var present bool
	err := conn.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM pg_catalog.pg_attribute a
		WHERE a.attrelid = to_regclass($1) AND a.attname = $2
			AND a.attnum > 0 AND NOT a.attisdropped
	)`, table, column).Scan(&present)
	return present, err
}
