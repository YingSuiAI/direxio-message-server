package consumers

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/caching"
	"github.com/YingSuiAI/dirextalk-message-server/internal/pushgateway"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/types"
	"github.com/YingSuiAI/dirextalk-message-server/setup/jetstream"
	"github.com/YingSuiAI/dirextalk-message-server/test/testrig"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"golang.org/x/crypto/bcrypt"

	"github.com/YingSuiAI/dirextalk-message-server/internal/pushrules"
	rsapi "github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
	"github.com/YingSuiAI/dirextalk-message-server/userapi/api"
	"github.com/YingSuiAI/dirextalk-message-server/userapi/producers"
	"github.com/YingSuiAI/dirextalk-message-server/userapi/storage"
	"github.com/YingSuiAI/dirextalk-message-server/userapi/storage/tables"
	userAPITypes "github.com/YingSuiAI/dirextalk-message-server/userapi/types"
)

const (
	dirextalkIOSAppID     = "com.direxio.app"
	dirextalkAndroidAppID = "com.dirextalk.app"
	dirextalkAPNsPushKey  = "apns-device-token"
	dirextalkFCMPushKey   = "fcm-device-token"
)

type recordingPushGateway struct {
	url  string
	req  *pushgateway.NotifyRequest
	resp pushgateway.NotifyResponse
}

func (g *recordingPushGateway) Notify(ctx context.Context, url string, req *pushgateway.NotifyRequest, resp *pushgateway.NotifyResponse) error {
	g.url = url
	g.req = req
	*resp = g.resp
	return nil
}

type noopPublisher struct{}

func (noopPublisher) PublishMsg(msg *nats.Msg, opts ...nats.PubOpt) (*nats.PubAck, error) {
	return &nats.PubAck{}, nil
}

type currentStateRoomserver struct {
	FakeUserRoomserverAPI
	state map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent
}

func (r *currentStateRoomserver) QueryCurrentState(ctx context.Context, req *rsapi.QueryCurrentStateRequest, res *rsapi.QueryCurrentStateResponse) error {
	res.StateEvents = map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{}
	for _, tuple := range req.StateTuples {
		if ev := r.state[tuple]; ev != nil {
			res.StateEvents[tuple] = ev
		}
	}
	return nil
}

func mustCreateDatabase(t *testing.T, dbType test.DBType) (storage.UserDatabase, func()) {
	t.Helper()
	connStr, close := test.PrepareDBConnectionString(t, dbType)
	cm := sqlutil.NewConnectionManager(nil, config.DatabaseOptions{})
	db, err := storage.NewUserDatabase(context.Background(), cm, &config.DatabaseOptions{
		ConnectionString: config.DataSource(connStr),
	}, "", 4, 0, 0, "")
	if err != nil {
		t.Fatalf("failed to create new user db: %v", err)
	}
	return db, func() {
		close()
	}
}

func mustCreateEvent(t *testing.T, content string) *types.HeaderedEvent {
	t.Helper()
	ev, err := gomatrixserverlib.MustGetRoomVersion(gomatrixserverlib.RoomVersionV10).NewEventFromTrustedJSON([]byte(content), false)
	if err != nil {
		t.Fatalf("failed to create event: %v", err)
	}
	return &types.HeaderedEvent{PDU: ev}
}

type FakeUserRoomserverAPI struct{ rsapi.UserRoomserverAPI }

func (f *FakeUserRoomserverAPI) QueryUserIDForSender(ctx context.Context, roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
	return spec.NewUserID(string(senderID), true)
}

func TestPushNotificationMetadataUsesDirextalkRoomStateForMessagePayload(t *testing.T) {
	ctx := context.Background()
	event := mustCreateEvent(t, `{
		"type":"m.room.message",
		"room_id":"!room:example.com",
		"sender":"@alice:example.com",
		"content":{"body":"hello","msgtype":"m.text"}
	}`)
	consumer := OutputRoomEventConsumer{rsAPI: &currentStateRoomserver{
		state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
			dirextalkRoomProfileTuple: mustCreateEvent(t, `{
				"type":"io.dirextalk.room.profile",
				"state_key":"",
				"room_id":"!room:example.com",
				"sender":"@alice:example.com",
				"content":{
					"room_type":"io.dirextalk.room.group",
					"name":"Engineering"
				}
			}`),
		},
	}}

	metadata, err := consumer.pushNotificationMetadata(ctx, event, "")
	if err != nil {
		t.Fatalf("pushNotificationMetadata returned error: %v", err)
	}
	if metadata.SuppressGateway {
		t.Fatal("expected group message push to be sent")
	}
	if metadata.Title != "Engineering" {
		t.Fatalf("unexpected title: %q", metadata.Title)
	}
	if metadata.PushType != "message" {
		t.Fatalf("unexpected push_type: %q", metadata.PushType)
	}
	if metadata.RoomType != "group" {
		t.Fatalf("unexpected room_type: %q", metadata.RoomType)
	}
}

func TestPushNotificationMetadataSuppressesAllChannelMessages(t *testing.T) {
	ctx := context.Background()
	event := mustCreateEvent(t, `{
		"type":"m.room.message",
		"room_id":"!posts:example.com",
		"sender":"@alice:example.com",
		"content":{"body":"ordinary chat","msgtype":"m.text"}
	}`)
	consumer := OutputRoomEventConsumer{rsAPI: &currentStateRoomserver{
		state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
			dirextalkRoomProfileTuple: mustCreateEvent(t, `{
				"type":"io.dirextalk.room.profile",
				"state_key":"",
				"room_id":"!posts:example.com",
				"sender":"@alice:example.com",
				"content":{
					"room_type":"io.dirextalk.room.channel",
					"channel_type":"chat",
					"name":"Announcements"
				}
			}`),
		},
	}}

	metadata, err := consumer.pushNotificationMetadata(ctx, event, "")
	if err != nil {
		t.Fatalf("pushNotificationMetadata returned error: %v", err)
	}
	if !metadata.SuppressGateway {
		t.Fatal("expected every channel message push to be suppressed")
	}
}

func TestPushNotificationMetadataUsesCallInviteContent(t *testing.T) {
	ctx := context.Background()
	event := mustCreateEvent(t, `{
		"type":"m.call.invite",
		"room_id":"!callroom:example.com",
		"sender":"@alice:example.com",
		"content":{"call_id":"call-123","lifetime":60000,"version":1}
	}`)
	consumer := OutputRoomEventConsumer{rsAPI: &currentStateRoomserver{
		state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
			dirextalkRoomProfileTuple: mustCreateEvent(t, `{
				"type":"io.dirextalk.room.profile",
				"state_key":"",
				"room_id":"!callroom:example.com",
				"sender":"@alice:example.com",
				"content":{"room_type":"io.dirextalk.room.direct","name":"Alice"}
			}`),
		},
	}}

	metadata, err := consumer.pushNotificationMetadata(ctx, event, "")
	if err != nil {
		t.Fatalf("pushNotificationMetadata returned error: %v", err)
	}
	if metadata.PushType != "call" {
		t.Fatalf("unexpected push_type: %q", metadata.PushType)
	}
	if metadata.RoomType != "direct" {
		t.Fatalf("unexpected room_type: %q", metadata.RoomType)
	}
	if metadata.CallID != "call-123" {
		t.Fatalf("unexpected call_id: %q", metadata.CallID)
	}
	if metadata.CallKind != "voice" {
		t.Fatalf("unexpected call_kind: %q", metadata.CallKind)
	}
}

func TestNotifyHTTPEventIDOnlySendsDirextalkIOSAPNsPusherAndReturnsRejectedDevice(t *testing.T) {
	ctx := context.Background()
	gateway := &recordingPushGateway{
		resp: pushgateway.NotifyResponse{Rejected: []string{dirextalkAPNsPushKey}},
	}
	consumer := OutputRoomEventConsumer{pgClient: gateway}
	event := mustCreateEvent(t, `{
		"type":"m.room.message",
		"room_id":"!room:example.com",
		"sender":"@alice:example.com",
		"content":{"body":"hello","msgtype":"m.text"}
	}`)
	devices := []*pushgateway.Device{
		{
			AppID:   dirextalkIOSAppID,
			PushKey: dirextalkAPNsPushKey,
			Data: map[string]interface{}{
				"format":   "event_id_only",
				"provider": "apns",
				"platform": "ios",
			},
		},
	}

	metadata := pushNotificationMetadata{
		Title:    "Dirextalk",
		RoomType: "direct",
		PushType: "message",
	}
	rejected, err := consumer.notifyHTTP(ctx, event, "https://push.dirextalk.ai/_matrix/push/v1/notify", "event_id_only", devices, "alice", "Dirextalk", int(7), metadata)
	if err != nil {
		t.Fatalf("notifyHTTP returned error: %v", err)
	}
	if gateway.url != "https://push.dirextalk.ai/_matrix/push/v1/notify" {
		t.Fatalf("unexpected push gateway URL: %q", gateway.url)
	}
	if gateway.req == nil {
		t.Fatal("expected push gateway request")
	}
	notification := gateway.req.Notification
	if notification.EventID != event.EventID() {
		t.Fatalf("unexpected event_id: %q", notification.EventID)
	}
	if notification.RoomID != event.RoomID().String() {
		t.Fatalf("unexpected room_id: %q", notification.RoomID)
	}
	if notification.Counts == nil || notification.Counts.Unread != 7 {
		t.Fatalf("unexpected notification counts: %#v", notification.Counts)
	}
	if notification.Title != "Dirextalk" {
		t.Fatalf("unexpected notification title: %q", notification.Title)
	}
	if notification.RoomType != "direct" {
		t.Fatalf("unexpected notification room_type: %q", notification.RoomType)
	}
	if notification.PushType != "message" {
		t.Fatalf("unexpected notification push_type: %q", notification.PushType)
	}
	if len(notification.Devices) != 1 {
		t.Fatalf("expected one device, got %d", len(notification.Devices))
	}
	device := notification.Devices[0]
	if device.AppID != dirextalkIOSAppID || device.PushKey != dirextalkAPNsPushKey {
		t.Fatalf("unexpected iOS APNs device: %#v", device)
	}
	wantData := map[string]interface{}{
		"format":   "event_id_only",
		"provider": "apns",
		"platform": "ios",
	}
	if !reflect.DeepEqual(device.Data, wantData) {
		t.Fatalf("unexpected iOS APNs device data:\n got: %#v\nwant: %#v", device.Data, wantData)
	}
	if len(rejected) != 1 || rejected[0].AppID != dirextalkIOSAppID || rejected[0].PushKey != dirextalkAPNsPushKey {
		t.Fatalf("unexpected rejected devices: %#v", rejected)
	}
}

func TestNotifyLocalOnlySuppressesFreshFocusedForegroundRoom(t *testing.T) {
	ctx := context.Background()
	localpart := "test"
	serverName := spec.ServerName("localhost")

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateDatabase(t, dbType)
		defer close()
		consumer := OutputRoomEventConsumer{
			db:           db,
			rsAPI:        &currentStateRoomserver{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{}},
			syncProducer: producers.NewSyncAPI(db, noopPublisher{}, "client_data", "notification_data"),
		}
		mem := &localMembership{
			UserID:    "@test:localhost",
			Localpart: localpart,
			Domain:    serverName,
		}
		contextData := json.RawMessage(`{
			"foreground": true,
			"expires_at_ms": 4102444800000
		}`)
		if err := db.SaveAccountData(ctx, localpart, serverName, "", "io.dirextalk.push.context", contextData); err != nil {
			t.Fatal(err)
		}

		foregroundEvent := mustCreateEvent(t, `{
			"type":"m.room.message",
			"room_id":"!other:example.com",
			"sender":"@alice:example.com",
			"content":{"body":"visible","msgtype":"m.text"}
		}`)
		if err := consumer.notifyLocal(ctx, foregroundEvent, mem, 2, "Foreground", 100); err != nil {
			t.Fatalf("notifyLocal returned error for foreground app: %v", err)
		}
		count, err := db.GetNotificationCount(ctx, localpart, serverName, tables.AllNotifications)
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("foreground app context without focused room must create a notification, got %d", count)
		}

		focusedData := json.RawMessage(`{
			"foreground": true,
			"expires_at_ms": 4102444800000,
			"current_room_id": "!focused:example.com"
		}`)
		if err := db.SaveAccountData(ctx, localpart, serverName, "", "io.dirextalk.push.context", focusedData); err != nil {
			t.Fatal(err)
		}
		focusedEvent := mustCreateEvent(t, `{
			"type":"m.room.message",
			"room_id":"!focused:example.com",
			"sender":"@alice:example.com",
			"content":{"body":"focused","msgtype":"m.text"}
		}`)
		if err := consumer.notifyLocal(ctx, focusedEvent, mem, 2, "Focused", 101); err != nil {
			t.Fatalf("notifyLocal returned error for focused foreground room: %v", err)
		}
		count, err = db.GetNotificationCount(ctx, localpart, serverName, tables.AllNotifications)
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("focused foreground room must not create another notification, got %d", count)
		}

		backgroundData := json.RawMessage(`{"foreground": false}`)
		if err := db.SaveAccountData(ctx, localpart, serverName, "", "io.dirextalk.push.context", backgroundData); err != nil {
			t.Fatal(err)
		}
		backgroundEvent := mustCreateEvent(t, `{
			"type":"m.room.message",
			"room_id":"!background:example.com",
			"sender":"@alice:example.com",
			"content":{"body":"background","msgtype":"m.text"}
		}`)
		if err := consumer.notifyLocal(ctx, backgroundEvent, mem, 2, "Background", 102); err != nil {
			t.Fatalf("notifyLocal returned error for background app: %v", err)
		}
		count, err = db.GetNotificationCount(ctx, localpart, serverName, tables.AllNotifications)
		if err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Fatalf("background app context must create a notification, got %d", count)
		}
	})
}

func TestDeleteRejectedPushersRemovesRejectedPusherOnlyForCurrentUser(t *testing.T) {
	ctx := context.Background()
	localpart := "alice"
	otherLocalpart := "bob"
	serverName := spec.ServerName("localhost")

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateDatabase(t, dbType)
		defer close()
		consumer := OutputRoomEventConsumer{db: db}

		if err := db.UpsertPusher(ctx, api.Pusher{
			Kind:    api.HTTPKind,
			AppID:   dirextalkIOSAppID,
			PushKey: dirextalkAPNsPushKey,
			Data: map[string]interface{}{
				"format":   "event_id_only",
				"url":      "https://push.dirextalk.ai/_matrix/push/v1/notify",
				"provider": "apns",
				"platform": "ios",
			},
		}, localpart, serverName); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertPusher(ctx, api.Pusher{
			Kind:    api.HTTPKind,
			AppID:   dirextalkAndroidAppID,
			PushKey: dirextalkFCMPushKey,
			Data: map[string]interface{}{
				"format":   "event_id_only",
				"url":      "https://push.dirextalk.ai/_matrix/push/v1/notify",
				"provider": "fcm",
				"platform": "android",
			},
		}, otherLocalpart, serverName); err != nil {
			t.Fatal(err)
		}

		consumer.deleteRejectedPushers(ctx, []*pushgateway.Device{{
			AppID:   dirextalkIOSAppID,
			PushKey: dirextalkAPNsPushKey,
		}}, localpart, serverName)

		pushers, err := db.GetPushers(ctx, localpart, serverName)
		if err != nil {
			t.Fatal(err)
		}
		if len(pushers) != 0 {
			t.Fatalf("expected rejected pusher to be removed, got %#v", pushers)
		}

		otherPushers, err := db.GetPushers(ctx, otherLocalpart, serverName)
		if err != nil {
			t.Fatal(err)
		}
		if len(otherPushers) != 1 || otherPushers[0].AppID != dirextalkAndroidAppID || otherPushers[0].PushKey != dirextalkFCMPushKey {
			t.Fatalf("expected other user's Android pusher to remain, got %#v", otherPushers)
		}
	})
}

func Test_evaluatePushRules(t *testing.T) {
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateDatabase(t, dbType)
		defer close()
		consumer := OutputRoomEventConsumer{db: db, rsAPI: &FakeUserRoomserverAPI{}}

		testCases := []struct {
			name         string
			eventContent string
			roomSize     int
			wantAction   pushrules.ActionKind
			wantActions  []*pushrules.Action
			wantNotify   bool
		}{
			{
				name:         "m.receipt doesn't notify",
				eventContent: `{"type":"m.receipt","room_id":"!room:example.com"}`,
				wantAction:   pushrules.UnknownAction,
				wantActions:  nil,
			},
			{
				name:         "m.reaction doesn't notify",
				eventContent: `{"type":"m.reaction","room_id":"!room:example.com"}`,
				wantAction:   pushrules.UnknownAction,
				wantActions:  []*pushrules.Action{},
			},
			{
				name:         "group plaintext message notifies with sound",
				eventContent: `{"type":"m.room.message","room_id":"!room:example.com"}`,
				roomSize:     10,
				wantNotify:   true,
				wantAction:   pushrules.NotifyAction,
				wantActions: []*pushrules.Action{
					{Kind: pushrules.NotifyAction},
					{Kind: pushrules.SetTweakAction, Tweak: pushrules.SoundTweak, Value: "default"},
				},
			},
			{
				name:         "direct plaintext message keeps sound",
				eventContent: `{"type":"m.room.message","room_id":"!room:example.com"}`,
				roomSize:     2,
				wantNotify:   true,
				wantAction:   pushrules.NotifyAction,
				wantActions: []*pushrules.Action{
					{Kind: pushrules.NotifyAction},
					{Kind: pushrules.SetTweakAction, Tweak: pushrules.SoundTweak, Value: "default"},
				},
			},
			{
				name:         "group encrypted message notifies with sound",
				eventContent: `{"type":"m.room.encrypted","room_id":"!room:example.com"}`,
				roomSize:     10,
				wantNotify:   true,
				wantAction:   pushrules.NotifyAction,
				wantActions: []*pushrules.Action{
					{Kind: pushrules.NotifyAction},
					{Kind: pushrules.SetTweakAction, Tweak: pushrules.SoundTweak, Value: "default"},
				},
			},
			{
				name:         "direct encrypted message keeps sound",
				eventContent: `{"type":"m.room.encrypted","room_id":"!room:example.com"}`,
				roomSize:     2,
				wantNotify:   true,
				wantAction:   pushrules.NotifyAction,
				wantActions: []*pushrules.Action{
					{Kind: pushrules.NotifyAction},
					{Kind: pushrules.SetTweakAction, Tweak: pushrules.SoundTweak, Value: "default"},
				},
			},
			{
				name:         "Matrix call invite keeps ring sound",
				eventContent: `{"type":"m.call.invite","room_id":"!room:example.com","content":{"call_id":"call-1"}}`,
				roomSize:     2,
				wantNotify:   true,
				wantAction:   pushrules.NotifyAction,
				wantActions: []*pushrules.Action{
					{Kind: pushrules.NotifyAction},
					{Kind: pushrules.SetTweakAction, Tweak: pushrules.SoundTweak, Value: "ring"},
				},
			},
			{
				name:         "m.room.message highlights",
				eventContent: `{"type":"m.room.message", "content": {"body": "test"},"room_id":"!room:example.com"}`,
				wantNotify:   true,
				wantAction:   pushrules.NotifyAction,
				wantActions: []*pushrules.Action{
					{Kind: pushrules.NotifyAction},
					{
						Kind:  pushrules.SetTweakAction,
						Tweak: pushrules.SoundTweak,
						Value: "default",
					},
					{
						Kind:  pushrules.SetTweakAction,
						Tweak: pushrules.HighlightTweak,
					},
				},
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				roomSize := tc.roomSize
				if roomSize == 0 {
					roomSize = 10
				}
				actions, err := consumer.evaluatePushRules(ctx, mustCreateEvent(t, tc.eventContent), &localMembership{
					UserID:    "@test:localhost",
					Localpart: "test",
					Domain:    "localhost",
				}, roomSize)
				if err != nil {
					t.Fatalf("failed to evaluate push rules: %v", err)
				}
				assert.Equal(t, tc.wantActions, actions)
				gotAction, _, err := pushrules.ActionsToTweaks(actions)
				if err != nil {
					t.Fatalf("failed to get actions: %v", err)
				}
				if gotAction != tc.wantAction {
					t.Fatalf("expected action to be '%s', got '%s'", tc.wantAction, gotAction)
				}
				// this is taken from `notifyLocal`
				if tc.wantNotify && gotAction != pushrules.NotifyAction {
					t.Fatalf("expected to notify but didn't")
				}
			})

		}
	})
}

func TestLocalRoomMembers(t *testing.T) {
	alice := test.NewUser(t)
	_, sk, err := ed25519.GenerateKey(nil)
	assert.NoError(t, err)
	bob := test.NewUser(t, test.WithSigningServer("notlocalhost", "ed25519:abc", sk))
	charlie := test.NewUser(t, test.WithSigningServer("notlocalhost", "ed25519:abc", sk))

	room := test.NewRoom(t, alice)
	room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]string{"membership": spec.Join}, test.WithStateKey(bob.ID))
	room.CreateAndInsert(t, charlie, spec.MRoomMember, map[string]string{"membership": spec.Join}, test.WithStateKey(charlie.ID))

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, close := testrig.CreateConfig(t, dbType)
		defer close()

		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		natsInstance := &jetstream.NATSInstance{}
		caches := caching.NewRistrettoCache(8*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		db, err := storage.NewUserDatabase(processCtx.Context(), cm, &cfg.UserAPI.AccountDatabase, cfg.Global.ServerName, bcrypt.MinCost, 1000, 1000, "")
		assert.NoError(t, err)

		err = rsapi.SendEvents(processCtx.Context(), rsAPI, rsapi.KindNew, room.Events(), "", "test", "test", nil, false)
		assert.NoError(t, err)

		consumer := OutputRoomEventConsumer{db: db, rsAPI: rsAPI, serverName: "test", cfg: &cfg.UserAPI}
		members, count, err := consumer.localRoomMembers(processCtx.Context(), room.ID)
		assert.NoError(t, err)
		assert.Equal(t, 3, count)
		expectedLocalMember := &localMembership{UserID: alice.ID, Localpart: alice.Localpart, Domain: "test", MemberContent: gomatrixserverlib.MemberContent{Membership: spec.Join}}
		assert.Equal(t, expectedLocalMember, members[0])
	})

}

func TestMessageStats(t *testing.T) {
	type args struct {
		eventType   string
		eventSender string
		roomID      string
	}
	tests := []struct {
		name           string
		args           args
		ourServer      spec.ServerName
		lastUpdate     time.Time
		initRoomCounts map[spec.ServerName]map[string]bool
		wantStats      userAPITypes.MessageStats
	}{
		{
			name:      "m.room.create does not count as a message",
			ourServer: "localhost",
			args: args{
				eventType:   "m.room.create",
				eventSender: "@alice:localhost",
			},
		},
		{
			name:      "our server - message",
			ourServer: "localhost",
			args: args{
				eventType:   "m.room.message",
				eventSender: "@alice:localhost",
				roomID:      "normalRoom",
			},
			wantStats: userAPITypes.MessageStats{Messages: 1, SentMessages: 1},
		},
		{
			name:      "our server - E2EE message",
			ourServer: "localhost",
			args: args{
				eventType:   "m.room.encrypted",
				eventSender: "@alice:localhost",
				roomID:      "encryptedRoom",
			},
			wantStats: userAPITypes.MessageStats{Messages: 1, SentMessages: 1, MessagesE2EE: 1, SentMessagesE2EE: 1},
		},

		{
			name:      "remote server - message",
			ourServer: "localhost",
			args: args{
				eventType:   "m.room.message",
				eventSender: "@alice:remote",
				roomID:      "normalRoom",
			},
			wantStats: userAPITypes.MessageStats{Messages: 2, SentMessages: 1, MessagesE2EE: 1, SentMessagesE2EE: 1},
		},
		{
			name:      "remote server - E2EE message",
			ourServer: "localhost",
			args: args{
				eventType:   "m.room.encrypted",
				eventSender: "@alice:remote",
				roomID:      "encryptedRoom",
			},
			wantStats: userAPITypes.MessageStats{Messages: 2, SentMessages: 1, MessagesE2EE: 2, SentMessagesE2EE: 1},
		},
		{
			name:       "day change creates a new room map",
			ourServer:  "localhost",
			lastUpdate: time.Now().Add(-time.Hour * 24),
			initRoomCounts: map[spec.ServerName]map[string]bool{
				"localhost": {"encryptedRoom": true},
			},
			args: args{
				eventType:   "m.room.encrypted",
				eventSender: "@alice:remote",
				roomID:      "someOtherRoom",
			},
			wantStats: userAPITypes.MessageStats{Messages: 2, SentMessages: 1, MessagesE2EE: 3, SentMessagesE2EE: 1},
		},
	}

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, close := mustCreateDatabase(t, dbType)
		defer close()

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if tt.lastUpdate.IsZero() {
					tt.lastUpdate = time.Now()
				}
				if tt.initRoomCounts == nil {
					tt.initRoomCounts = map[spec.ServerName]map[string]bool{}
				}
				s := &OutputRoomEventConsumer{
					db:         db,
					msgCounts:  map[spec.ServerName]userAPITypes.MessageStats{},
					roomCounts: tt.initRoomCounts,
					countsLock: sync.Mutex{},
					lastUpdate: tt.lastUpdate,
					serverName: tt.ourServer,
				}
				s.storeMessageStats(context.Background(), tt.args.eventType, tt.args.eventSender, tt.args.roomID)
				t.Logf("%+v", s.roomCounts)
				gotStats, activeRooms, activeE2EERooms, err := db.DailyRoomsMessages(context.Background(), tt.ourServer)
				if err != nil {
					t.Fatalf("unexpected error: %s", err)
				}
				if !reflect.DeepEqual(gotStats, tt.wantStats) {
					t.Fatalf("expected %+v, got %+v", tt.wantStats, gotStats)
				}
				if tt.args.eventType == "m.room.encrypted" && activeE2EERooms != 1 {
					t.Fatalf("expected room to be activeE2EE")
				}
				if tt.args.eventType == "m.room.message" && activeRooms != 1 {
					t.Fatalf("expected room to be active")
				}
			})
		}
	})
}

func BenchmarkLocalRoomMembers(b *testing.B) {
	t := &testing.T{}

	cfg, processCtx, close := testrig.CreateConfig(t, test.DBTypePostgres)
	defer close()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	natsInstance := &jetstream.NATSInstance{}
	caches := caching.NewRistrettoCache(8*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	db, err := storage.NewUserDatabase(processCtx.Context(), cm, &cfg.UserAPI.AccountDatabase, cfg.Global.ServerName, bcrypt.MinCost, 1000, 1000, "")
	assert.NoError(b, err)

	consumer := OutputRoomEventConsumer{db: db, rsAPI: rsAPI, serverName: "test", cfg: &cfg.UserAPI}
	_, sk, err := ed25519.GenerateKey(nil)
	assert.NoError(b, err)

	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)

	for i := 0; i < 100; i++ {
		user := test.NewUser(t, test.WithSigningServer("notlocalhost", "ed25519:abc", sk))
		room.CreateAndInsert(t, user, spec.MRoomMember, map[string]string{"membership": spec.Join}, test.WithStateKey(user.ID))
	}

	err = rsapi.SendEvents(processCtx.Context(), rsAPI, rsapi.KindNew, room.Events(), "", "test", "test", nil, false)
	assert.NoError(b, err)

	expectedLocalMember := &localMembership{UserID: alice.ID, Localpart: alice.Localpart, Domain: "test", MemberContent: gomatrixserverlib.MemberContent{Membership: spec.Join}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		members, count, err := consumer.localRoomMembers(processCtx.Context(), room.ID)
		assert.NoError(b, err)
		assert.Equal(b, 101, count)
		assert.Equal(b, expectedLocalMember, members[0])
	}
}
