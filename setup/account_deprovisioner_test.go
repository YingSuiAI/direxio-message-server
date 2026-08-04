package setup

import (
	"context"
	"database/sql"
	"testing"

	serverTest "github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestResetPostgresDatabasePreservesInfrastructureMetadata(t *testing.T) {
	ctx := context.Background()
	connectionString, closeDatabase := serverTest.PrepareDBConnectionString(
		t,
		serverTest.DBTypePostgres,
	)
	defer closeDatabase()

	db, err := sql.Open("postgres", connectionString)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, statement := range []string{
		`CREATE TABLE db_migrations (version TEXT PRIMARY KEY, time TEXT NOT NULL, dendrite_version TEXT NOT NULL)`,
		`CREATE TABLE p2p_account_data (id BIGSERIAL PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE agent_owned_data (id BIGSERIAL PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE core_owned_data (id BIGSERIAL PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO db_migrations (version, time, dendrite_version) VALUES ('p2p: fresh ProductCore baseline v1', 'now', 'test')`,
		`INSERT INTO p2p_account_data (value) VALUES ('delete me')`,
		`INSERT INTO agent_owned_data (value) VALUES ('agent keeps ownership')`,
		`INSERT INTO core_owned_data (value) VALUES ('core keeps ownership')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare deprovision database: %v", err)
		}
	}

	txn, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resetPostgresDatabase(ctx, txn); err != nil {
		_ = txn.Rollback()
		t.Fatalf("reset account database: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}

	assertTableCount(t, db, "p2p_account_data", 0)
	assertTableCount(t, db, "db_migrations", 1)
	assertTableCount(t, db, "agent_owned_data", 1)
	assertTableCount(t, db, "core_owned_data", 1)
}

func assertTableCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(
		context.Background(),
		`SELECT count(*) FROM `+table,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s rows = %d, want %d", table, got, want)
	}
}
