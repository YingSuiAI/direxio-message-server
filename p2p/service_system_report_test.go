package p2p

import (
	"context"
	"net/http"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestReportSubmitWithoutMatrixTransportFailsClosed(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	service.systemRoomID = "!system:example.com"
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com", "name": "Reported Group",
	})

	_, apiErr := service.Handle(context.Background(), "reports.submit", map[string]any{
		"target_type": "group", "room_id": group.RoomID,
	})
	if apiErr == nil || apiErr.Status != http.StatusInternalServerError {
		t.Fatalf("reports.submit without Matrix transport = %#v", apiErr)
	}
}

func TestReportSubmitPersistsReportAndSystemConversationAfterReload(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	transport := &recordingTransport{roomID: "!system:example.com", eventID: "$report:example.com", ts: 1783433640000}
	service, err := NewServiceWithStoreAndTransport(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store, transport)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Reported Group",
	})
	report := mustHandle[reportRecord](t, service, "reports.submit", map[string]any{
		"target_type":           "group",
		"room_id":               group.RoomID,
		"reason":                "Spam / Advertisement",
		"body":                  "Suspicious advertisement",
		"reporter_mxid":         "@alice:remote.example",
		"reporter_display_name": "Alice",
		"image_urls":            []any{"mxc://example.com/evidence"},
	})
	if report.EventID != "$report:example.com" || report.SystemRoomID != "!system:example.com" {
		t.Fatalf("expected report to use system message result, got %#v", report)
	}
	if len(transport.messages) != 1 || transport.messages[0].RoomID != "!system:example.com" || transport.messages[0].Content["msg_type"] != "report" {
		t.Fatalf("expected one Matrix system report notice, got %#v", transport.messages)
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
	reports, err := reloadedStore.ListReports(ctx, group.RoomID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 ||
		reports[0].TargetRoomID != group.RoomID ||
		reports[0].TargetName != "Reported Group" ||
		reports[0].ReporterMXID != "@alice:remote.example" ||
		reports[0].EventID != "$report:example.com" ||
		reports[0].SystemRoomID != "!system:example.com" ||
		len(reports[0].ImageURLs) != 1 ||
		reports[0].ImageURLs[0] != "mxc://example.com/evidence" {
		t.Fatalf("expected submitted report to survive reload, got %#v", reports)
	}
	conversations := mustHandle[map[string]any](t, reloaded, "conversations.list", nil)["conversations"].([]conversationView)
	var systemConversation conversationView
	for _, conversation := range conversations {
		if conversation.Kind == conversationKindSystem {
			systemConversation = conversation
			break
		}
	}
	if systemConversation.MatrixRoomID != "!system:example.com" ||
		systemConversation.LastEventID != "$report:example.com" ||
		systemConversation.LastMessage != "Suspicious advertisement" ||
		systemConversation.LastActivityAt != 1783433640000 {
		t.Fatalf("expected system conversation to survive reload, got %#v", conversations)
	}
}
