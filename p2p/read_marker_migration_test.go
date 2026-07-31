package p2p

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestFreshV78ReadMarkerSchemaIsCanonical(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}

	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertReadMarkerPositionColumnCount(t, ctx, store, 2)
	// The pure-V2 direct-final schema creates canonical read-marker columns in
	// one fresh migration; legacy v72/v73 upgrade replay is intentionally gone.
	var v78Count int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM db_migrations
		WHERE version = 'p2p: agent and execution v2 fresh schema v78'
	`).Scan(&v78Count); err != nil || v78Count != 1 {
		t.Fatalf("v78 migration ledger count = %d, err = %v", v78Count, err)
	}
}

func assertReadMarkerPositionColumnCount(
	t *testing.T, ctx context.Context, store *DatabaseStore, want int,
) {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'p2p_read_markers'
			AND column_name IN ('topological_position', 'stream_position')
	`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("read marker position column count = %d, want %d", count, want)
	}
}
