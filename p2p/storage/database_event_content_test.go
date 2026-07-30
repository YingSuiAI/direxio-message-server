package storage

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestDatabaseStorePrunesP2PEventsBeforeSeq(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for seq := int64(1); seq <= 5; seq++ {
		if _, err := store.InsertEvent(ctx, p2pEvent{
			Seq:       seq,
			Type:      "test.event",
			RoomID:    "!room:example.com",
			EventID:   "$event",
			CreatedAt: "2026-06-29T00:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	bounds, err := store.EventBounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bounds.MinSeq != 1 || bounds.MaxSeq != 5 || bounds.Count != 5 {
		t.Fatalf("expected initial event bounds 1..5 count 5, got %#v", bounds)
	}
	deleted, err := store.PruneEventsBefore(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("expected 3 deleted events, got %d", deleted)
	}
	remaining, err := store.ListEvents(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].Seq != 4 || remaining[1].Seq != 5 {
		t.Fatalf("expected events 4 and 5 after prune, got %#v", remaining)
	}
}

func TestDatabaseStoreSkipsDuplicateP2PEventsByDedupeKey(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	inserted, err := store.InsertEvent(ctx, p2pEvent{
		Seq:       1,
		Type:      "room.member.projected",
		RoomID:    "!room:example.com",
		EventID:   "$event",
		DedupeKey: "room.member.projected:$event:@owner:example.com",
		CreatedAt: "2026-06-29T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatalf("expected first event insert to report inserted")
	}
	inserted, err = store.InsertEvent(ctx, p2pEvent{
		Seq:       2,
		Type:      "room.member.projected",
		RoomID:    "!room:example.com",
		EventID:   "$event",
		DedupeKey: "room.member.projected:$event:@owner:example.com",
		CreatedAt: "2026-06-29T00:00:01Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatalf("expected duplicate dedupe key to be skipped")
	}
	events, err := store.ListEvents(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("expected only first deduped event, got %#v", events)
	}
}

func TestDatabaseStoreDeleteChannelContentReportsDeletedRows(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.InsertChannelPost(ctx, channelPostRecord{
		PostID:         "post_1",
		ChannelID:      "ch_1",
		RoomID:         "!room:example.com",
		EventID:        "$post-event",
		AuthorMXID:     "@owner:example.com",
		OriginServerTS: 1,
	}); err != nil {
		t.Fatal(err)
	}
	postDeleted, err := store.DeleteChannelPost(ctx, "$post-event")
	if err != nil {
		t.Fatal(err)
	}
	if !postDeleted {
		t.Fatalf("expected post delete by event id to report a deleted row")
	}
	postDeletedAgain, err := store.DeleteChannelPost(ctx, "$post-event")
	if err != nil {
		t.Fatal(err)
	}
	if postDeletedAgain {
		t.Fatalf("expected second post delete to report no deleted row")
	}

	if err := store.InsertChannelComment(ctx, channelCommentRecord{
		CommentID:      "comment_1",
		PostID:         "post_1",
		ChannelID:      "ch_1",
		EventID:        "$comment-event",
		AuthorMXID:     "@owner:example.com",
		OriginServerTS: 2,
	}); err != nil {
		t.Fatal(err)
	}
	commentDeleted, err := store.DeleteChannelComment(ctx, "$comment-event")
	if err != nil {
		t.Fatal(err)
	}
	if !commentDeleted {
		t.Fatalf("expected comment delete by event id to report a deleted row")
	}
	commentDeletedAgain, err := store.DeleteChannelComment(ctx, "$comment-event")
	if err != nil {
		t.Fatal(err)
	}
	if commentDeletedAgain {
		t.Fatalf("expected second comment delete to report no deleted row")
	}
}

func TestDatabaseStoreGetsChannelContentByIDAndEventID(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.InsertChannelPost(ctx, channelPostRecord{
		PostID:         "post_lookup",
		ChannelID:      "ch_lookup",
		RoomID:         "!room:example.com",
		EventID:        "$post-lookup-event",
		AuthorMXID:     "@owner:example.com",
		Body:           "post lookup",
		OriginServerTS: 1,
	}); err != nil {
		t.Fatal(err)
	}
	postByID, ok, err := store.GetChannelPostByID(ctx, "post_lookup", "ch_lookup")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || postByID.EventID != "$post-lookup-event" {
		t.Fatalf("expected post lookup by id, got ok=%v post=%#v", ok, postByID)
	}
	if _, ok, err = store.GetChannelPostByID(ctx, "post_lookup", "other_channel"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("expected channel-scoped post lookup to reject another channel")
	}
	postByEvent, ok, err := store.GetChannelPostByEventID(ctx, "$post-lookup-event", "")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || postByEvent.PostID != "post_lookup" {
		t.Fatalf("expected post lookup by event id, got ok=%v post=%#v", ok, postByEvent)
	}

	if err := store.InsertChannelComment(ctx, channelCommentRecord{
		CommentID:      "comment_lookup",
		PostID:         "post_lookup",
		ChannelID:      "ch_lookup",
		EventID:        "$comment-lookup-event",
		AuthorMXID:     "@owner:example.com",
		Body:           "comment lookup",
		OriginServerTS: 2,
	}); err != nil {
		t.Fatal(err)
	}
	commentByID, ok, err := store.GetChannelCommentByID(ctx, "comment_lookup", "post_lookup")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || commentByID.EventID != "$comment-lookup-event" {
		t.Fatalf("expected comment lookup by id, got ok=%v comment=%#v", ok, commentByID)
	}
	if _, ok, err = store.GetChannelCommentByID(ctx, "comment_lookup", "other_post"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("expected post-scoped comment lookup to reject another post")
	}
	commentByEvent, ok, err := store.GetChannelCommentByEventID(ctx, "$comment-lookup-event", "ch_lookup")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || commentByEvent.CommentID != "comment_lookup" {
		t.Fatalf("expected comment lookup by event id, got ok=%v comment=%#v", ok, commentByEvent)
	}
}

func TestDatabaseStoreListsChannelPostsByVisibilityPage(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, post := range []channelPostRecord{
		{PostID: "public_old", ChannelID: "ch", Visibility: "public", OriginServerTS: 10},
		{PostID: "public_new", ChannelID: "ch", Visibility: "public", OriginServerTS: 20},
		{PostID: "private_newest", ChannelID: "ch", Visibility: "private", OriginServerTS: 30},
		{PostID: "other_public", ChannelID: "other", Visibility: "public", OriginServerTS: 40},
	} {
		if err := store.InsertChannelPost(ctx, post); err != nil {
			t.Fatalf("InsertChannelPost(%s): %v", post.PostID, err)
		}
	}

	first, more, err := store.ListChannelPostsByVisibilityPage(ctx, "ch", "public", 0, 1)
	if err != nil || !more || len(first) != 1 || first[0].PostID != "public_new" {
		t.Fatalf("first public page = %#v more=%v err=%v", first, more, err)
	}
	second, more, err := store.ListChannelPostsByVisibilityPage(ctx, "ch", "public", 1, 1)
	if err != nil || more || len(second) != 1 || second[0].PostID != "public_old" {
		t.Fatalf("second public page = %#v more=%v err=%v", second, more, err)
	}
}
