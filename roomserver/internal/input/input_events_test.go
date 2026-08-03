package input

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"

	federationAPI "github.com/YingSuiAI/dirextalk-message-server/federationapi/api"
	"github.com/YingSuiAI/dirextalk-message-server/internal/caching"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/internal/query"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/storage"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/types"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	"github.com/YingSuiAI/dirextalk-message-server/test"
	"github.com/YingSuiAI/dirextalk-message-server/test/signing"
)

func Test_EventAuth(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)

	// create two rooms, so we can craft "illegal" auth events
	room1 := test.NewRoom(t, alice)
	room2 := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))

	authEventIDs := make([]string, 0, 4)
	authEvents := []gomatrixserverlib.PDU{}

	// Add the legal auth events from room2
	for _, x := range room2.Events() {
		if x.Type() == spec.MRoomCreate {
			authEventIDs = append(authEventIDs, x.EventID())
			authEvents = append(authEvents, x.PDU)
		}
		if x.Type() == spec.MRoomPowerLevels {
			authEventIDs = append(authEventIDs, x.EventID())
			authEvents = append(authEvents, x.PDU)
		}
		if x.Type() == spec.MRoomJoinRules {
			authEventIDs = append(authEventIDs, x.EventID())
			authEvents = append(authEvents, x.PDU)
		}
	}

	// Add the illegal auth event from room1 (rooms are different)
	for _, x := range room1.Events() {
		if x.Type() == spec.MRoomMember {
			authEventIDs = append(authEventIDs, x.EventID())
			authEvents = append(authEvents, x.PDU)
		}
	}

	// Craft the illegal join event, with auth events from different rooms
	ev := room2.CreateEvent(t, bob, "m.room.member", map[string]interface{}{
		"membership": "join",
	}, test.WithStateKey(bob.ID), test.WithAuthIDs(authEventIDs))

	// Add the auth events to the allower
	allower, _ := gomatrixserverlib.NewAuthEvents(nil)
	for _, a := range authEvents {
		if err := allower.AddEvent(a); err != nil {
			t.Fatalf("allower.AddEvent failed: %v", err)
		}
	}

	// Finally check that the event is NOT allowed
	if err := gomatrixserverlib.Allowed(ev.PDU, allower, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return spec.NewUserID(string(senderID), true)
	}); err == nil {
		t.Fatalf("event should not be allowed, but it was")
	}
}

type eventAuthFederationAPI struct {
	federationAPI.RoomserverFederationAPI
	response fclient.RespEventAuth
	keyRing  *gomatrixserverlib.KeyRing
}

func (f *eventAuthFederationAPI) GetEventAuth(
	context.Context,
	spec.ServerName,
	spec.ServerName,
	gomatrixserverlib.RoomVersion,
	string,
	string,
) (fclient.RespEventAuth, error) {
	return f.response, nil
}

func (f *eventAuthFederationAPI) KeyRing() *gomatrixserverlib.KeyRing {
	return f.keyRing
}

func TestFetchAuthEventsStoresEachEventsOwnStateKey(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		connectionString, closeDatabase := test.PrepareDBConnectionString(t, dbType)
		defer closeDatabase()
		caches := caching.NewRistrettoCache(8*1024*1024, time.Hour, caching.DisableMetrics)
		connections := sqlutil.NewConnectionManager(nil, config.DatabaseOptions{})
		db, err := storage.Open(context.Background(), connections, &config.DatabaseOptions{
			ConnectionString: config.DataSource(connectionString),
		}, caches)
		if err != nil {
			t.Fatalf("open roomserver database: %v", err)
		}

		publicKey, privateKey, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("generate signing key: %v", err)
		}
		serverName := spec.ServerName(hex.EncodeToString(publicKey))
		owner := test.NewUser(t, test.WithSigningServer(serverName, gomatrixserverlib.KeyID(signing.KeyID), privateKey))
		joiner := test.NewUser(t, test.WithSigningServer(serverName, gomatrixserverlib.KeyID(signing.KeyID), privateKey))
		room := test.NewRoom(t, owner, test.RoomPreset(test.PresetPublicChat))
		joinEvent := room.CreateEvent(t, joiner, spec.MRoomMember, map[string]interface{}{
			"membership": spec.Join,
		}, test.WithStateKey(joiner.ID))

		authChain := make([]gomatrixserverlib.PDU, 0, len(room.Events()))
		expectedStateKeys := make(map[string]string)
		for _, event := range room.Events() {
			authChain = append(authChain, event.PDU)
			switch event.Type() {
			case spec.MRoomCreate, spec.MRoomPowerLevels, spec.MRoomJoinRules:
				expectedStateKeys[event.EventID()] = ""
			case spec.MRoomMember:
				if event.StateKeyEquals(owner.ID) {
					expectedStateKeys[event.EventID()] = owner.ID
				}
			}
		}
		if len(expectedStateKeys) != 4 {
			t.Fatalf("test auth chain does not contain the expected four state events: %#v", expectedStateKeys)
		}

		keyRing := (&signing.YggdrasilKeys{}).KeyRing()
		inputer := &Inputer{
			DB: db,
			FSAPI: &eventAuthFederationAPI{
				response: fclient.RespEventAuth{
					AuthEvents: gomatrixserverlib.NewEventJSONsFromEvents(authChain),
				},
				keyRing: keyRing,
			},
			Queryer: &query.Queryer{DB: db},
		}
		authEvents, err := gomatrixserverlib.NewAuthEvents(nil)
		if err != nil {
			t.Fatalf("create auth event set: %v", err)
		}
		if err = inputer.fetchAuthEvents(
			context.Background(),
			logrus.NewEntry(logrus.New()),
			nil,
			serverName,
			joinEvent,
			authEvents,
			make(map[string]*types.Event),
			[]spec.ServerName{serverName},
		); err != nil {
			t.Fatalf("fetch auth events: %v", err)
		}

		eventIDs := make([]string, 0, len(expectedStateKeys))
		for eventID := range expectedStateKeys {
			eventIDs = append(eventIDs, eventID)
		}
		metadata, err := db.EventNIDs(context.Background(), eventIDs)
		if err != nil {
			t.Fatalf("load stored event NIDs: %v", err)
		}
		entries, err := db.StateEntriesForEventIDs(context.Background(), eventIDs, false)
		if err != nil {
			t.Fatalf("load stored state entries: %v", err)
		}
		entriesByEventNID := make(map[types.EventNID]types.StateEntry, len(entries))
		stateKeyNIDs := make([]types.EventStateKeyNID, 0, len(entries))
		for _, entry := range entries {
			entriesByEventNID[entry.EventNID] = entry
			stateKeyNIDs = append(stateKeyNIDs, entry.EventStateKeyNID)
		}
		stateKeys, err := db.EventStateKeys(context.Background(), stateKeyNIDs)
		if err != nil {
			t.Fatalf("resolve stored event state keys: %v", err)
		}
		for eventID, wantStateKey := range expectedStateKeys {
			storedMetadata, ok := metadata[eventID]
			if !ok {
				t.Fatalf("auth event %s was not stored", eventID)
			}
			entry, ok := entriesByEventNID[storedMetadata.EventNID]
			if !ok {
				t.Fatalf("auth event %s has no stored state entry", eventID)
			}
			if gotStateKey := stateKeys[entry.EventStateKeyNID]; gotStateKey != wantStateKey {
				t.Fatalf("auth event %s stored state_key = %q, want %q (outer membership state_key is %q)", eventID, gotStateKey, wantStateKey, joiner.ID)
			}
		}
	})
}
