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
	if dbProperties.ConnectionString.IsSQLite() {
		return nil, fmt.Errorf("SQLite is not supported for P2P product state; configure PostgreSQL")
	}
	db, writer, err := cm.Connection(dbProperties)
	if err != nil {
		return nil, err
	}
	store := &DatabaseStore{db: db, writer: writer}
	if err := store.migrate(ctx); err != nil {
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
