package storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	channelsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/channels"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestDatabaseStoreRejectsSQLiteConnectionString(t *testing.T) {
	ctx := context.Background()
	dbOpts := config.DatabaseOptions{ConnectionString: "file::memory:?cache=shared"}

	_, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)

	if err == nil || !strings.Contains(err.Error(), "SQLite") {
		t.Fatalf("expected SQLite connection string to be rejected, got %v", err)
	}
}

func TestDatabaseStoreCreatesBusinessIndexes(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	expected := []string{
		"p2p_channels_room_idx",
		"p2p_channels_type_visibility_idx",
		"p2p_channels_visibility_idx",
		"p2p_channel_posts_channel_idx",
		"p2p_channel_posts_event_idx",
		"p2p_channel_posts_author_idx",
		"p2p_channel_posts_channel_visibility_idx",
		"p2p_channel_comments_post_idx",
		"p2p_channel_comments_channel_idx",
		"p2p_channel_comments_event_idx",
		"p2p_contacts_peer_idx",
		"p2p_contacts_status_idx",
		"p2p_blocks_type_idx",
		"p2p_blocks_room_idx",
		"p2p_blocks_peer_idx",
		"p2p_reports_target_idx",
		"p2p_reports_reporter_idx",
		"p2p_calls_room_idx",
		"p2p_calls_state_idx",
		"p2p_favorites_type_idx",
		"p2p_favorites_event_idx",
		"p2p_reactions_user_idx",
		"p2p_reactions_target_idx",
		"p2p_reactions_event_idx",
		"p2p_members_channel_idx",
		"p2p_members_room_idx",
		"p2p_members_user_idx",
		"p2p_members_room_joined_idx",
		"p2p_members_channel_joined_idx",
		"p2p_members_user_room_idx",
		"p2p_members_user_channel_idx",
		"p2p_events_room_idx",
		"p2p_events_type_idx",
		"p2p_legacy_agent_invocations_state_updated_idx",
		"p2p_native_agent_turns_owner_conversation_idx",
		"p2p_native_agent_turns_active_idx",
	}
	for _, indexName := range expected {
		t.Run(indexName, func(t *testing.T) {
			var name string
			if err := store.DB().QueryRowContext(ctx, `SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, indexName).Scan(&name); err != nil {
				t.Fatalf("expected index %s to exist: %v", indexName, err)
			}
		})
	}
	var contactPeerIndex string
	if err := store.DB().QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'p2p_contacts_peer_idx'`).Scan(&contactPeerIndex); err != nil {
		t.Fatalf("expected contact peer index definition: %v", err)
	}
	if !strings.Contains(strings.ToUpper(contactPeerIndex), "UNIQUE") {
		t.Fatalf("expected p2p_contacts_peer_idx to be unique, got %s", contactPeerIndex)
	}
	var messageTableCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'p2p_messages'`).Scan(&messageTableCount); err != nil {
		t.Fatal(err)
	}
	if messageTableCount != 0 {
		t.Fatalf("p2p_messages table must not be created after Matrix-source migration")
	}
}

func TestDatabaseMembershipMigrationCanonicalizesJoinedToJoin(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO p2p_members (
			room_id, user_id, channel_id, display_name, avatar_url, domain,
			membership, role, muted, joined_at, requester_node_base_url, request_id
		) VALUES ($1, $2, '', '', '', 'example.com', ' JOINED ', 'member', 0, 1, '', '')
	`, "!room:example.com", "@alice:example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM db_migrations WHERE version = $1`, "p2p: canonical Matrix member membership v76"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var membership string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT membership FROM p2p_members WHERE room_id = $1 AND user_id = $2
	`, "!room:example.com", "@alice:example.com").Scan(&membership); err != nil {
		t.Fatal(err)
	}
	if membership != "join" {
		t.Fatalf("migrated membership = %q, want join", membership)
	}
}

func TestLegacyChannelFavoritesBackfillToOwnerReaction(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.SavePortal(ctx, portalState{OwnerMXID: "@owner:example.com"}); err != nil {
		t.Fatalf("save owner portal: %v", err)
	}
	if err := store.InsertChannelPost(ctx, channelPostRecord{
		PostID: "post_legacy", ChannelID: "channel_legacy", RoomID: "!channel:example.com", EventID: "$post_legacy",
	}); err != nil {
		t.Fatalf("insert channel post: %v", err)
	}
	if err := store.UpsertFavorite(ctx, favoriteRecord{
		ID: 1, EventID: "$post_legacy", RoomID: "!channel:example.com", MessageType: "m.image", CreatedAt: "2026-07-20T00:00:00Z",
	}); err != nil {
		t.Fatalf("insert legacy favorite: %v", err)
	}

	txn, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := backfillLegacyChannelFavorites(ctx, txn); err != nil {
		_ = txn.Rollback()
		t.Fatalf("backfill legacy favorite: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
	reaction, ok, err := store.GetReaction(ctx, "post", "post_legacy", "favorite", "@owner:example.com")
	if err != nil || !ok || !reaction.Active || reaction.ChannelID != "channel_legacy" {
		t.Fatalf("expected active owner favorite reaction, got %#v ok=%v err=%v", reaction, ok, err)
	}
	content := channelsmodule.NewContent(store, nil, nil, nil, channelsmodule.ContentConfig{
		Owner: func() channelsmodule.ContentOwner { return channelsmodule.ContentOwner{MXID: "@owner:example.com"} },
	})
	result, apiErr := content.Posts(ctx, map[string]any{"channel_id": "channel_legacy"})
	if apiErr != nil {
		t.Fatalf("list migrated post: %#v", apiErr)
	}
	posts := result.(map[string]any)["posts"].([]channelsmodule.Post)
	if len(posts) != 1 || posts[0].FavoriteCount != 1 || !posts[0].FavoritedByMe {
		t.Fatalf("owner view should expose migrated favorite state, got %#v", posts)
	}

	txn, err = store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := backfillLegacyChannelFavorites(ctx, txn); err != nil {
		_ = txn.Rollback()
		t.Fatalf("replay backfill: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
	count, err := store.CountActiveReactions(ctx, "post", "post_legacy", "favorite")
	if err != nil || count != 1 {
		t.Fatalf("backfill replay must remain idempotent, count=%d err=%v", count, err)
	}
}

func TestDatabaseReactionEventIdentityDeactivatesCurrentProjection(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertReaction(ctx, reactionRecord{
		EventID: "$favorite", TargetType: "post", TargetID: "post_1", ChannelID: "channel_1", PostID: "post_1",
		Reaction: "favorite", UserID: "@owner:example.com", Active: true,
	}); err != nil {
		t.Fatalf("store favorite reaction: %v", err)
	}
	removed, err := store.DeactivateReactionByEventID(ctx, "$favorite")
	if err != nil || !removed {
		t.Fatalf("deactivate projected reaction = (%t, %v), want (true, nil)", removed, err)
	}
	reaction, ok, err := store.GetReaction(ctx, "post", "post_1", "favorite", "@owner:example.com")
	if err != nil || !ok || reaction.Active || reaction.EventID != "$favorite" {
		t.Fatalf("expected stored reaction to remain identifiable and inactive, got %#v ok=%v err=%v", reaction, ok, err)
	}
}

func TestDatabaseStoreContactPeerUniqueMigrationDeduplicatesExistingRows(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	db, writer, err := sqlutil.NewConnectionManager(nil, dbOpts).Connection(&dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	store := NewUnmigratedDatabaseStore(db, writer)
	defer store.Close()

	if _, execErr := store.DB().ExecContext(ctx, `
		CREATE TABLE p2p_contacts (
			room_id TEXT PRIMARY KEY NOT NULL,
			peer_mxid TEXT NOT NULL,
			display_name TEXT NOT NULL,
			domain TEXT NOT NULL,
			status TEXT NOT NULL
		)
	`); execErr != nil {
		t.Fatal(execErr)
	}
	if _, execErr := store.DB().ExecContext(ctx, `CREATE INDEX p2p_contacts_peer_idx ON p2p_contacts(peer_mxid)`); execErr != nil {
		t.Fatal(execErr)
	}
	duplicates := []contactRecord{
		{RoomID: "!pending:example.com", PeerMXID: "@alice:remote.example", DisplayName: "Pending Alice", Domain: "remote.example", Status: "pending_outbound"},
		{RoomID: "!accepted:example.com", PeerMXID: "@alice:remote.example", DisplayName: "Accepted Alice", Domain: "remote.example", Status: "accepted"},
		{RoomID: "!deleted:example.com", PeerMXID: "@alice:remote.example", DisplayName: "Deleted Alice", Domain: "remote.example", Status: "deleted"},
		{RoomID: "!bob:example.com", PeerMXID: "@bob:remote.example", DisplayName: "Bob", Domain: "remote.example", Status: "pending_outbound"},
	}
	for _, contact := range duplicates {
		if _, execErr := store.DB().ExecContext(ctx, `
			INSERT INTO p2p_contacts (room_id, peer_mxid, display_name, domain, status)
			VALUES ($1, $2, $3, $4, $5)
		`, contact.RoomID, contact.PeerMXID, contact.DisplayName, contact.Domain, contact.Status); execErr != nil {
			t.Fatal(execErr)
		}
	}
	if migrationErr := markP2PMigrationsBeforeContactPeerUnique(ctx, store.DB()); migrationErr != nil {
		t.Fatal(migrationErr)
	}

	if migrationErr := store.Migrate(ctx); migrationErr != nil {
		t.Fatal(migrationErr)
	}

	contacts, err := store.ListContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(contacts) != 2 {
		t.Fatalf("expected duplicate peers to be compacted, got %#v", contacts)
	}
	alice := findStoredContact(contacts, "@alice:remote.example")
	if alice.RoomID != "!accepted:example.com" || alice.Status != "accepted" {
		t.Fatalf("expected migration to keep accepted contact for duplicate peer, got %#v", alice)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO p2p_contacts (room_id, peer_mxid, display_name, domain, status)
		VALUES ($1, $2, $3, $4, $5)
	`, "!new-alice:example.com", "@alice:remote.example", "Alice Duplicate", "remote.example", "pending_outbound"); err == nil {
		t.Fatalf("expected migrated contact peer index to reject duplicates")
	}
}

func findStoredContact(contacts []contactRecord, peerMXID string) contactRecord {
	for _, contact := range contacts {
		if contact.PeerMXID == peerMXID {
			return contact
		}
	}
	return contactRecord{}
}

func markP2PMigrationsBeforeContactPeerUnique(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS db_migrations (
			version TEXT PRIMARY KEY NOT NULL,
			time TEXT NOT NULL,
			dendrite_version TEXT NOT NULL
		)
	`); err != nil {
		return err
	}
	versions := []string{
		"p2p: integrated appservice tables v1",
		"p2p: integrated appservice tables v2",
		"p2p: integrated appservice tables v3",
		"p2p: integrated appservice tables v4 member avatars",
		"p2p: integrated appservice tables v5 product mute state",
		"p2p: integrated appservice tables v6 member join order",
		"p2p: integrated appservice tables v7 portal matrix device",
		"p2p: integrated appservice tables v11 channel comment replies",
		"p2p: integrated appservice tables v12 channel comment media",
		"p2p: integrated appservice tables v13 event outbox",
		"p2p: integrated appservice tables v14 channel invite grants",
		"p2p: drop legacy message mirror table v15",
	}
	for _, version := range versions {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO db_migrations (version, time, dendrite_version)
			VALUES ($1, $2, $3)
		`, version, "2026-06-21T00:00:00Z", "test"); err != nil {
			return err
		}
	}
	return nil
}
