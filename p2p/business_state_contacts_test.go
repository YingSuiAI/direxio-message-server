package p2p

import (
	"context"
	"net/http"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestContactAcceptRequiresPendingInboundContact(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)

	if _, apiErr := service.Handle(context.Background(), "contacts.requests.accept", map[string]any{
		"room_id":   "!missing:example.com",
		"peer_mxid": "@alice:remote.example",
	}); apiErr == nil || apiErr.Status != http.StatusNotFound {
		t.Fatalf("expected accept without pending inbound contact to return 404, got %#v", apiErr)
	}
}

func TestContactDecisionsResolvePeerWithoutCreatingMissingRequests(t *testing.T) {
	decisions := []struct {
		action     string
		wantStatus string
	}{
		{action: "contacts.requests.accept", wantStatus: "accepted"},
		{action: "contacts.requests.reject", wantStatus: "rejected"},
	}
	for _, decision := range decisions {
		t.Run(decision.action+" missing", func(t *testing.T) {
			service := NewService(Config{ServerName: "example.com"})
			bootstrapService(t, service)
			result, apiErr := service.Handle(context.Background(), decision.action, map[string]any{
				"peer_mxid": "@alice:remote.example",
			})
			if result != nil || apiErr == nil || apiErr.Status != http.StatusNotFound {
				t.Errorf("missing peer-only decision = (%#v, %#v), want structured 404", result, apiErr)
			}
			contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
			if len(contacts) != 0 {
				t.Errorf("missing peer-only decision created contact projection: %#v", contacts)
			}
			conversations, err := service.listConversations(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(conversations) != 0 {
				t.Errorf("missing peer-only decision created conversation projection: %#v", conversations)
			}
		})

		t.Run(decision.action+" existing", func(t *testing.T) {
			service := NewService(Config{ServerName: "example.com"})
			bootstrapService(t, service)
			if err := service.saveContact(context.Background(), contactRecord{
				RoomID: "!direct:example.com", PeerMXID: "@alice:remote.example",
				DisplayName: "Alice", Status: "pending_inbound", RequestID: "request-a",
			}); err != nil {
				t.Fatal(err)
			}
			resolved := mustHandle[contactRecord](t, service, decision.action, map[string]any{
				"peer_mxid": "@alice:remote.example",
			})
			if resolved.Status != decision.wantStatus || resolved.RoomID != "!direct:example.com" {
				t.Fatalf("peer-only decision did not resolve existing room: %#v", resolved)
			}
		})
	}
}

func TestContactListDeduplicatesPeerAndPrefersAcceptedContact(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    "@owner:peer.example",
		DisplayName: "owner",
		AvatarURL:   "mxc://peer.example/pending",
		Domain:      "peer.example",
		RoomID:      "!pending:example.com",
		Status:      "pending_inbound",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    "@owner:peer.example",
		DisplayName: "Bob Nickname",
		AvatarURL:   "mxc://peer.example/accepted",
		Domain:      "peer.example",
		RoomID:      "!accepted:example.com",
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustHandle[map[string]any](t, service, "contacts.list", nil)
	contacts := result["contacts"].([]contactRecord)
	if len(contacts) != 1 {
		t.Fatalf("expected one visible contact after peer dedupe, got %#v", contacts)
	}
	if contacts[0].RoomID != "!accepted:example.com" || contacts[0].DisplayName != "Bob Nickname" || contacts[0].AvatarURL != "mxc://peer.example/accepted" || contacts[0].Status != "accepted" {
		t.Fatalf("expected accepted contact with latest nickname to win, got %#v", contacts[0])
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	syncedContacts := bootstrap["contacts"].([]contactRecord)
	if len(syncedContacts) != 1 || syncedContacts[0].AvatarURL != "mxc://peer.example/accepted" {
		t.Fatalf("expected sync bootstrap contact to include avatar_url, got %#v", syncedContacts)
	}
}

func TestDeletedContactRequestRestoresOriginalRoom(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	contact := mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@alice:remote.example",
		"display_name": "Alice",
	})
	accepted := mustHandle[contactRecord](t, service, "contacts.requests.accept", map[string]any{
		"room_id":      contact.RoomID,
		"peer_mxid":    contact.PeerMXID,
		"display_name": contact.DisplayName,
		"domain":       contact.Domain,
	})
	if accepted.Status != "accepted" {
		t.Fatalf("expected accepted contact, got %#v", accepted)
	}
	mustHandle[map[string]any](t, service, "contacts.delete", map[string]any{
		"room_id": accepted.RoomID,
	})
	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if findContact(contacts, accepted.PeerMXID).PeerMXID != "" {
		t.Fatalf("expected deleted contact hidden from ordinary contact list, got %#v", contacts)
	}

	restored := mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         accepted.PeerMXID,
		"display_name": accepted.DisplayName,
	})
	if restored.Status != "accepted" || restored.RoomID != accepted.RoomID {
		t.Fatalf("expected deleted peer re-request to restore original room, got %#v", restored)
	}
	contacts = mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if findContact(contacts, accepted.PeerMXID).Status != "accepted" {
		t.Fatalf("expected restored contact visible as accepted, got %#v", contacts)
	}
}

func TestDeletedContactRequestRestoresOriginalRoomAfterReload(t *testing.T) {
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
	contact := mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@alice:remote.example",
		"display_name": "Alice",
	})
	accepted := mustHandle[contactRecord](t, service, "contacts.requests.accept", map[string]any{
		"room_id":      contact.RoomID,
		"peer_mxid":    contact.PeerMXID,
		"display_name": contact.DisplayName,
		"domain":       contact.Domain,
	})
	mustHandle[map[string]any](t, service, "contacts.delete", map[string]any{
		"room_id": accepted.RoomID,
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
	hidden := mustHandle[map[string]any](t, reloaded, "contacts.list", nil)["contacts"].([]contactRecord)
	if findContact(hidden, accepted.PeerMXID).PeerMXID != "" {
		t.Fatalf("expected deleted contact to remain hidden after reload, got %#v", hidden)
	}
	raw, err := reloaded.rawContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retained := findContact(raw, accepted.PeerMXID); retained.Status != "deleted" || retained.RoomID != accepted.RoomID {
		t.Fatalf("expected deleted contact identity to survive reload, got %#v", raw)
	}
	restored := mustHandle[contactRecord](t, reloaded, "contacts.request", map[string]any{
		"mxid":         accepted.PeerMXID,
		"display_name": accepted.DisplayName,
	})
	if restored.Status != "accepted" || restored.RoomID != accepted.RoomID {
		t.Fatalf("expected deleted peer re-request to restore original room after reload, got %#v", restored)
	}
	contacts := mustHandle[map[string]any](t, reloaded, "contacts.list", nil)["contacts"].([]contactRecord)
	if findContact(contacts, accepted.PeerMXID).Status != "accepted" {
		t.Fatalf("expected restored contact visible after reload, got %#v", contacts)
	}
}

func TestContactReplacementAfterReloadRemovesOldDirectConversation(t *testing.T) {
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
	const peerMXID = "@alice:remote.example"
	if err := service.saveContact(ctx, contactRecord{
		PeerMXID: peerMXID, RoomID: "!old:example.com", DisplayName: "Alice", Status: "accepted",
	}); err != nil {
		t.Fatal(err)
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
	if err := reloaded.saveContact(ctx, contactRecord{
		PeerMXID: peerMXID, RoomID: "!replacement:example.com", DisplayName: "Alice", Status: "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	conversations, err := reloaded.listConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var oldFound, replacementFound bool
	for _, conversation := range conversations {
		if conversation.Kind != conversationKindDirect {
			continue
		}
		switch conversation.MatrixRoomID {
		case "!old:example.com":
			oldFound = true
		case "!replacement:example.com":
			replacementFound = true
		}
	}
	if oldFound || !replacementFound {
		t.Fatalf("replacement conversations = %#v, want only replacement direct room", conversations)
	}
}

func TestContactRemarkUpdatePersistsAfterReload(t *testing.T) {
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
	contact := mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@alice:remote.example",
		"display_name": "Alice",
	})
	accepted := mustHandle[contactRecord](t, service, "contacts.requests.accept", map[string]any{
		"room_id":      contact.RoomID,
		"peer_mxid":    contact.PeerMXID,
		"display_name": contact.DisplayName,
		"domain":       contact.Domain,
	})
	updated := mustHandle[contactRecord](t, service, "contacts.update", map[string]any{
		"room_id":      accepted.RoomID,
		"display_name": "Alice Remark",
	})
	if updated.DisplayName != "Alice Remark" || !updated.DisplayNameOverride || updated.PeerMXID != accepted.PeerMXID || updated.Status != "accepted" {
		t.Fatalf("expected updated contact remark, got %#v", updated)
	}
	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if got := findContact(contacts, accepted.PeerMXID); got.DisplayName != "Alice Remark" || !got.DisplayNameOverride {
		t.Fatalf("expected updated remark in contacts list, got %#v", contacts)
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
	reloadedContacts := mustHandle[map[string]any](t, reloaded, "contacts.list", nil)["contacts"].([]contactRecord)
	if got := findContact(reloadedContacts, accepted.PeerMXID); got.DisplayName != "Alice Remark" || !got.DisplayNameOverride {
		t.Fatalf("expected updated remark after reload, got %#v", reloadedContacts)
	}
}

func TestContactRequestPreservesPeerDomainWithPort(t *testing.T) {
	service := NewService(Config{ServerName: "dendrite-a:8448"})

	contact := mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@owner:dendrite-b:8448",
		"display_name": "Owner B",
	})

	if contact.Domain != "dendrite-b:8448" {
		t.Fatalf("expected MXID domain with port to be preserved, got %#v", contact)
	}
}

func TestContactRequestRemarkIsReturnedInContactListAndPendingNotice(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)

	contact := mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@alice:remote.example",
		"display_name": "Alice",
		"remark":       "我是 Adam，请通过好友申请",
	})
	if contact.Remark != "我是 Adam，请通过好友申请" {
		t.Fatalf("expected contact request response to include remark, got %#v", contact)
	}
	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].Remark != "我是 Adam，请通过好友申请" {
		t.Fatalf("expected contacts.list to include request remark, got %#v", contacts)
	}
	contact.Status = "pending_inbound"
	if err := service.saveContact(context.Background(), contact); err != nil {
		t.Fatal(err)
	}
	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	pending := bootstrap["pending"].(map[string]any)
	friendRequests := pending["friend_requests"].([]map[string]any)
	if len(friendRequests) != 1 || friendRequests[0]["remark"] != "我是 Adam，请通过好友申请" {
		t.Fatalf("expected pending friend request to include remark, got %#v", friendRequests)
	}
}
