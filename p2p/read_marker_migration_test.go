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

func TestReadMarkerV72RowMigratesReopensAndCanonicalizesMonotonically(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}

	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		DELETE FROM db_migrations
		WHERE version = 'p2p: authoritative read marker order v73'
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		ALTER TABLE p2p_read_markers
			DROP COLUMN topological_position,
			DROP COLUMN stream_position
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO p2p_read_markers (room_id, event_id, origin_server_ts)
		VALUES ($1, $2, $3)
	`, "!room:example.com", "$legacy-current", int64(999)); err != nil {
		t.Fatal(err)
	}
	assertReadMarkerPositionColumnCount(t, ctx, store, 0)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v72-compatible read marker row: %v", err)
	}
	assertReadMarkerPositionColumnCount(t, ctx, store, 2)
	var v73Count int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM db_migrations
		WHERE version = 'p2p: authoritative read marker order v73'
	`).Scan(&v73Count); err != nil || v73Count != 1 {
		t.Fatalf("v73 migration ledger count = %d, err = %v", v73Count, err)
	}
	var migratedEventID string
	var topology, stream int64
	if err := store.DB().QueryRowContext(ctx, `
		SELECT event_id, topological_position, stream_position
		FROM p2p_read_markers WHERE room_id = $1
	`, "!room:example.com").Scan(&migratedEventID, &topology, &stream); err != nil {
		t.Fatal(err)
	}
	if migratedEventID != "$legacy-current" || topology != 0 || stream != 0 {
		t.Fatalf("retained row after v73 = (%q, %d, %d), want legacy row with zero tuple", migratedEventID, topology, stream)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, reopened)
	if err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	service.SetReadMarkerPositionResolver(staticReadMarkerResolver{
		"$legacy-current": {
			roomID: "!room:example.com", topologicalPosition: 8, streamPosition: 20,
			originServerTS: 100,
		},
		"$older": {
			roomID: "!room:example.com", topologicalPosition: 7, streamPosition: 99,
			originServerTS: 9_999,
		},
		"$later": {
			roomID: "!room:example.com", topologicalPosition: 9, streamPosition: 21,
			originServerTS: 1,
		},
	})
	for _, eventID := range []string{"$older", "$later", "$older", "$later"} {
		mustHandle[map[string]any](t, service, "sync.read_marker", map[string]any{
			"room_id": "!room:example.com", "event_id": eventID,
		})
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	finalStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer finalStore.Close()
	marker, found, err := finalStore.GetReadMarker(ctx, "!room:example.com")
	if err != nil || !found {
		t.Fatalf("GetReadMarker after final reopen = (%#v, %v, %v)", marker, found, err)
	}
	if marker.EventID != "$later" || marker.OriginServerTS != 1 ||
		marker.TopologicalPosition != 9 || marker.StreamPosition != 21 {
		t.Fatalf("retained marker regressed after canonicalization/replay: %#v", marker)
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
