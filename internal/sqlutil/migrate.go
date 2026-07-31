// Copyright 2024 New Vector Ltd.
// Copyright 2022 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlutil

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/YingSuiAI/dirextalk-message-server/internal"
)

const createDBMigrationsSQL = "" +
	"CREATE TABLE IF NOT EXISTS db_migrations (" +
	" version TEXT PRIMARY KEY NOT NULL," +
	" time TEXT NOT NULL," +
	" dendrite_version TEXT NOT NULL" +
	");"

const insertVersionSQL = "" +
	"INSERT INTO db_migrations (version, time, dendrite_version)" +
	" VALUES ($1, $2, $3)"

const selectDBMigrationsSQL = "SELECT version FROM db_migrations"

// Migration defines a migration to be run.
type Migration struct {
	// Version is a simple description/name of this migration.
	Version string
	// Up defines the function to execute for an upgrade.
	Up func(ctx context.Context, txn *sql.Tx) error
	// Down defines the function to execute for a downgrade (not implemented yet).
	Down func(ctx context.Context, txn *sql.Tx) error
}

// Migrator contains fields required to run migrations.
type Migrator struct {
	db              *sql.DB
	migrations      []Migration
	knownMigrations map[string]struct{}
	addErr          error
	mutex           *sync.Mutex
	insertStmt      *sql.Stmt
}

// NewMigrator creates a new DB migrator.
func NewMigrator(db *sql.DB) *Migrator {
	return &Migrator{
		db:              db,
		migrations:      []Migration{},
		knownMigrations: make(map[string]struct{}),
		mutex:           &sync.Mutex{},
	}
}

// AddMigrations appends migrations to the list of migrations. Migrations are
// executed in the order they are added. Duplicate versions are retained as a
// configuration error and make Up fail before executing any migration; silently
// dropping one can leave a database only partially upgraded.
func (m *Migrator) AddMigrations(migrations ...Migration) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for _, mig := range migrations {
		if _, ok := m.knownMigrations[mig.Version]; ok {
			if m.addErr == nil {
				m.addErr = fmt.Errorf("duplicate database migration version %q", mig.Version)
			}
			continue
		}
		m.migrations = append(m.migrations, mig)
		m.knownMigrations[mig.Version] = struct{}{}
	}
}

// Up executes all migrations in order they were added.
func (m *Migrator) Up(ctx context.Context) error {
	return m.up(ctx, "")
}

// UpTo executes migrations in registration order through the named version.
// The target must be known and is included in the transaction.
func (m *Migrator) UpTo(ctx context.Context, target string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("unknown migration version %q", target)
	}
	return m.up(ctx, target)
}

func (m *Migrator) up(ctx context.Context, target string) error {
	m.mutex.Lock()
	addErr := m.addErr
	known := target == ""
	if target != "" {
		for _, migration := range m.migrations {
			if migration.Version == target {
				known = true
				break
			}
		}
	}
	m.mutex.Unlock()
	if addErr != nil {
		return addErr
	}
	if !known {
		return fmt.Errorf("unknown migration version %q", target)
	}
	// ensure there is a table for known migrations
	executedMigrations, err := m.ExecutedMigrations(ctx)
	if err != nil {
		return fmt.Errorf("unable to create/get migrations: %w", err)
	}
	// ensure we close the insert statement, as it's not needed anymore
	defer m.close()
	return WithTransaction(m.db, func(txn *sql.Tx) error {
		for i := range m.migrations {
			migration := m.migrations[i]
			// Skip migration if it was already executed
			if _, ok := executedMigrations[migration.Version]; ok {
				if target != "" && migration.Version == target {
					break
				}
				continue
			}
			logrus.Debugf("Executing database migration '%s'", migration.Version)

			if err = migration.Up(ctx, txn); err != nil {
				return fmt.Errorf("unable to execute migration '%s': %w", migration.Version, err)
			}
			if err = m.insertMigration(ctx, txn, migration.Version); err != nil {
				return fmt.Errorf("unable to insert executed migrations: %w", err)
			}
			if target != "" && migration.Version == target {
				break
			}
		}
		return nil
	})
}

func (m *Migrator) insertMigration(ctx context.Context, txn *sql.Tx, migrationName string) error {
	if m.insertStmt == nil {
		var stmt *sql.Stmt
		var err error
		if txn == nil {
			stmt, err = m.db.PrepareContext(ctx, insertVersionSQL)
		} else {
			stmt, err = txn.PrepareContext(ctx, insertVersionSQL)
		}
		if err != nil {
			return fmt.Errorf("unable to prepare insert statement: %w", err)
		}
		m.insertStmt = stmt
	}
	stmt := TxStmtContext(ctx, txn, m.insertStmt)
	_, err := stmt.ExecContext(ctx,
		migrationName,
		time.Now().Format(time.RFC3339),
		internal.VersionString(),
	)
	return err
}

// ExecutedMigrations returns a map with already executed migrations in addition to creating the
// migrations table, if it doesn't exist.
func (m *Migrator) ExecutedMigrations(ctx context.Context) (map[string]struct{}, error) {
	result := make(map[string]struct{})
	_, err := m.db.ExecContext(ctx, createDBMigrationsSQL)
	if err != nil {
		return nil, fmt.Errorf("unable to create db_migrations: %w", err)
	}
	rows, err := m.db.QueryContext(ctx, selectDBMigrationsSQL)
	if err != nil {
		return nil, fmt.Errorf("unable to query db_migrations: %w", err)
	}
	defer internal.CloseAndLogIfError(ctx, rows, "ExecutedMigrations: rows.close() failed")
	var version string
	for rows.Next() {
		if err = rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("unable to scan version: %w", err)
		}
		result[version] = struct{}{}
	}

	return result, rows.Err()
}

// InsertMigration creates the migrations table if it doesn't exist and
// inserts a migration given their name to the database.
// This should only be used when manually inserting migrations.
func InsertMigration(ctx context.Context, db *sql.DB, migrationName string) error {
	m := NewMigrator(db)
	defer m.close()
	existingMigrations, err := m.ExecutedMigrations(ctx)
	if err != nil {
		return err
	}
	if _, ok := existingMigrations[migrationName]; ok {
		return nil
	}
	return m.insertMigration(ctx, nil, migrationName)
}

func (m *Migrator) close() {
	if m.insertStmt != nil {
		internal.CloseAndLogIfError(context.Background(), m.insertStmt, "unable to close insert statement")
	}
}
