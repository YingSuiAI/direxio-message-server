package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
)

type DatabaseStore struct {
	db     *sql.DB
	writer sqlutil.Writer
}

func NewDatabaseStore(ctx context.Context, cm *sqlutil.Connections, dbProperties *config.DatabaseOptions) (*DatabaseStore, error) {
	return newDatabaseStore(ctx, cm, dbProperties, "")
}

// NewDatabaseStoreAtMigration opens a PostgreSQL store and runs the same
// production migration registry through target, for historical upgrade
// fixtures. Production startup should use NewDatabaseStore.
func NewDatabaseStoreAtMigration(ctx context.Context, cm *sqlutil.Connections, dbProperties *config.DatabaseOptions, target string) (*DatabaseStore, error) {
	return newDatabaseStore(ctx, cm, dbProperties, target)
}

func newDatabaseStore(ctx context.Context, cm *sqlutil.Connections, dbProperties *config.DatabaseOptions, target string) (*DatabaseStore, error) {
	if dbProperties.ConnectionString.IsSQLite() {
		return nil, fmt.Errorf("SQLite is not supported for P2P product state; configure PostgreSQL")
	}
	db, writer, err := cm.Connection(dbProperties)
	if err != nil {
		return nil, err
	}
	store := &DatabaseStore{db: db, writer: writer}
	if err := store.migrateTo(ctx, target); err != nil {
		return nil, err
	}
	return store, nil
}

func NewUnmigratedDatabaseStore(db *sql.DB, writer sqlutil.Writer) *DatabaseStore {
	return &DatabaseStore{db: db, writer: writer}
}

func (s *DatabaseStore) DB() *sql.DB {
	return s.db
}

func (s *DatabaseStore) Migrate(ctx context.Context) error {
	return s.migrate(ctx)
}

func (s *DatabaseStore) Close() error {
	return s.db.Close()
}
