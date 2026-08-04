package p2p

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestServicePrunesP2PEventsWhenRetentionMaxRowsIsConfigured(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{
		ServerName:                    "example.com",
		P2PEventRetentionMaxRows:      3,
		P2PEventRetentionPruneOnWrite: true,
	}), store)
	if err != nil {
		t.Fatal(err)
	}

	for seq := int64(1); seq <= 5; seq++ {
		if err := service.appendP2PEvent(ctx, p2pEvent{
			Seq:     seq,
			Type:    "test.event",
			RoomID:  "!room:example.com",
			EventID: "$event",
		}); err != nil {
			t.Fatal(err)
		}
	}
	remaining, err := store.ListEvents(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 || remaining[0].Seq != 3 || remaining[2].Seq != 5 {
		t.Fatalf("expected retained events 3..5, got %#v", remaining)
	}
}

func TestDatabaseStorePreservesKickedChannelMemberAutoReject(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":  "moderated",
		"room_id":     "!moderated:example.com",
		"name":        "Moderated",
		"visibility":  "public",
		"join_policy": "approval",
	})
	mustHandle[map[string]any](t, service, "channels.invite", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"user_mxid":  "@kicked:remote.example",
	})
	mustHandle[map[string]any](t, service, "channels.member.remove", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"user_mxid":  "@kicked:remote.example",
	})

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	rejected := mustHandle[map[string]any](t, reloaded, "channels.public.join_request", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"user_mxid":  "@kicked:remote.example",
	})
	if rejected["status"] != "rejected" {
		t.Fatalf("expected kicked member auto reject after reload, got %#v", rejected)
	}
}

func TestDatabaseStoreRestoresDeletedContactRequest(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	contact := mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@alice:remote.example",
		"display_name": "Alice",
	})
	contact = mustHandle[contactRecord](t, service, "contacts.requests.accept", map[string]any{
		"room_id":      contact.RoomID,
		"peer_mxid":    contact.PeerMXID,
		"display_name": contact.DisplayName,
		"domain":       contact.Domain,
	})
	mustHandle[map[string]any](t, service, "contacts.delete", map[string]any{
		"room_id": contact.RoomID,
	})

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	restored := mustHandle[contactRecord](t, reloaded, "contacts.request", map[string]any{
		"mxid":         contact.PeerMXID,
		"display_name": contact.DisplayName,
	})
	if restored.Status != "accepted" || restored.RoomID != contact.RoomID {
		t.Fatalf("expected deleted contact request to restore original room after reload, got %#v", restored)
	}
}

func TestDatabaseStoreRestoresContactRequestRemarkAfterReload(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	contact := mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@alice:remote.example",
		"display_name": "Alice",
		"remark":       "我是 Adam，请通过好友申请",
	})
	if contact.Remark != "我是 Adam，请通过好友申请" {
		t.Fatalf("expected contact request response to include remark, got %#v", contact)
	}

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	contacts := mustHandle[map[string]any](t, reloaded, "contacts.list", nil)["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].Remark != "我是 Adam，请通过好友申请" {
		t.Fatalf("expected reloaded contacts.list to include remark, got %#v", contacts)
	}
}

func TestDatabaseStoreRecallChannelPostRemovesPostAfterReload(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	createdChannel := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "ch_delete",
		"room_id":    "!delete:example.com",
		"name":       "Delete",
	})
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": createdChannel.ChannelID,
		"room_id":    createdChannel.RoomID,
		"body":       "delete me",
	})

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	mustHandle[map[string]any](t, reloaded, "channels.posts.recall", map[string]any{
		"post_id": post.PostID,
		"room_id": createdChannel.RoomID,
	})

	againStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer againStore.Close()
	again, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), againStore)
	if err != nil {
		t.Fatal(err)
	}
	posts := mustHandle[map[string]any](t, again, "channels.posts.list", map[string]any{"channel_id": createdChannel.ChannelID})
	if got := posts["posts"].([]channelPostRecord); len(got) != 0 {
		t.Fatalf("expected recalled post to stay deleted after reload, got %#v", got)
	}
}
