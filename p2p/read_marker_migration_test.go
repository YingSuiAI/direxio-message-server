package p2p

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

// The message-server database is created from one fresh ProductCore baseline.
// Read-marker ordering is part of that baseline, not a replayable v72/v73
// upgrade path.
func TestFreshReadMarkerSchemaIsCanonicalAndDurable(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}

	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	assertReadMarkerPositionColumnCount(t, ctx, store, 2)

	var baselineCount int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM db_migrations
		WHERE version = 'p2p: fresh ProductCore baseline v1'
	`).Scan(&baselineCount); err != nil {
		t.Fatal(err)
	}
	if baselineCount != 1 {
		t.Fatalf("fresh ProductCore baseline rows = %d, want 1", baselineCount)
	}

	marker := readMarker{
		RoomID: "!room:example.com", EventID: "$current", OriginServerTS: 100,
		TopologicalPosition: 8, StreamPosition: 20,
	}
	if err := store.SaveReadMarker(ctx, marker); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertReadMarkerPositionColumnCount(t, ctx, reopened, 2)
	got, found, err := reopened.GetReadMarker(ctx, marker.RoomID)
	if err != nil || !found {
		t.Fatalf("GetReadMarker after fresh-baseline reopen = (%#v, %v, %v)", got, found, err)
	}
	if got != marker {
		t.Fatalf("durable canonical read marker = %#v, want %#v", got, marker)
	}
}

func assertReadMarkerPositionColumnCount(t *testing.T, ctx context.Context, store *DatabaseStore, want int) {
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
