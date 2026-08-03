package sqlutil_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

var dummyMigrations = []sqlutil.Migration{
	{
		Version: "init",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			_, err := txn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS dummy ( test TEXT );")
			return err
		},
	},
	{
		Version: "v2",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			_, err := txn.ExecContext(ctx, "ALTER TABLE dummy ADD COLUMN test2 TEXT;")
			return err
		},
	},
	{
		Version: "multiple execs",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			_, err := txn.ExecContext(ctx, "ALTER TABLE dummy ADD COLUMN test3 TEXT;")
			if err != nil {
				return err
			}
			_, err = txn.ExecContext(ctx, "ALTER TABLE dummy ADD COLUMN test4 TEXT;")
			return err
		},
	},
}

var failMigration = sqlutil.Migration{
	Version: "iFail",
	Up: func(ctx context.Context, txn *sql.Tx) error {
		return fmt.Errorf("iFail")
	},
	Down: nil,
}

func Test_migrations_Up(t *testing.T) {
	withFail := append(dummyMigrations, failMigration)

	tests := []struct {
		name       string
		migrations []sqlutil.Migration
		wantResult map[string]struct{}
		wantErr    bool
	}{
		{
			name:       "dummy migration",
			migrations: dummyMigrations,
			wantResult: map[string]struct{}{
				"init":           {},
				"v2":             {},
				"multiple execs": {},
			},
		},
		{
			name:       "with fail",
			migrations: withFail,
			wantErr:    true,
		},
	}

	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		conStr, close := test.PrepareDBConnectionString(t, dbType)
		defer close()

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				db, err := sql.Open("postgres", conStr)
				if err != nil {
					t.Errorf("unable to open database: %v", err)
				}
				m := sqlutil.NewMigrator(db)
				m.AddMigrations(tt.migrations...)
				if err = m.Up(ctx); (err != nil) != tt.wantErr {
					t.Errorf("Up() error = %v, wantErr %v", err, tt.wantErr)
				}
				result, err := m.ExecutedMigrations(ctx)
				if err != nil {
					t.Errorf("unable to get executed migrations: %v", err)
				}
				if !tt.wantErr && !reflect.DeepEqual(result, tt.wantResult) {
					t.Errorf("expected: %+v, got %v", tt.wantResult, result)
				}
			})
		}
	})
}

func Test_insertMigration(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		conStr, close := test.PrepareDBConnectionString(t, dbType)
		defer close()

		db, err := sql.Open("postgres", conStr)
		if err != nil {
			t.Errorf("unable to open database: %v", err)
		}

		if err := sqlutil.InsertMigration(context.Background(), db, "testing"); err != nil {
			t.Fatalf("unable to insert migration: %s", err)
		}
		// Second insert should not return an error, as it was already executed.
		if err := sqlutil.InsertMigration(context.Background(), db, "testing"); err != nil {
			t.Fatalf("unable to insert migration: %s", err)
		}
	})
}

func TestMigratorRejectsDuplicateVersionsBeforeOpeningDatabase(t *testing.T) {
	m := sqlutil.NewMigrator(nil)
	m.AddMigrations(
		sqlutil.Migration{Version: "duplicate"},
		sqlutil.Migration{Version: "duplicate"},
	)
	err := m.Up(context.Background())
	if err == nil || err.Error() != `duplicate database migration version "duplicate"` {
		t.Fatalf("Up() error = %v, want duplicate-version error", err)
	}
}

func TestMigratorUpToStopsAtKnownTargetAndRejectsUnknown(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ran []string
	m := sqlutil.NewMigrator(db)
	for _, version := range []string{"v1", "v2", "v3"} {
		version := version
		m.AddMigrations(sqlutil.Migration{Version: version, Up: func(context.Context, *sql.Tx) error {
			ran = append(ran, version)
			return nil
		}})
	}
	if err := m.UpTo(ctx, "v2"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ran, ","); got != "v1,v2" {
		t.Fatalf("UpTo execution = %q", got)
	}
	if err := m.UpTo(ctx, "v2"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ran, ","); got != "v1,v2" {
		t.Fatalf("repeat UpTo crossed target = %q", got)
	}
	if err := m.UpTo(ctx, "missing"); err == nil {
		t.Fatal("UpTo accepted unknown target")
	}
}
