package p2p

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/releasecontrol"
)

type recordingAccountDeactivator struct {
	users []string
	err   error
}

func (d *recordingAccountDeactivator) DeactivateAccount(ctx context.Context, localpart string) error {
	d.users = append(d.users, localpart)
	return d.err
}

type recordingAccountDeprovisioner struct {
	calls int
	err   error
}

type nthLeaveFailureTransport struct {
	recordingTransport
	failAt int
	calls  int
}

func (t *nthLeaveFailureTransport) LeaveRoom(ctx context.Context, req LeaveRoomRequest) error {
	t.calls++
	t.leaves = append(t.leaves, req.UserMXID+" from "+req.RoomID)
	if t.calls == t.failAt {
		return errors.New("leave failed")
	}
	return nil
}

func (d *recordingAccountDeprovisioner) DeprovisionAccount(ctx context.Context) error {
	d.calls++
	return d.err
}

func TestAccountDeleteRequiresExplicitConfirmation(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)

	if _, apiErr := service.Handle(context.Background(), "portal.account.delete", map[string]any{}); apiErr == nil || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("expected account deletion to require confirmation, got %#v", apiErr)
	}
}

func TestAccountDeleteLeavesContactsDissolvesOwnedRoomsAndDeprovisions(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "example.com", NativeAgentRunner: &externalDeprovisionRunner{}}, transport)
	deactivator := &recordingAccountDeactivator{}
	deprovisioner := &recordingAccountDeprovisioner{}
	releaseController := &recordingReleaseController{}
	service.releaseController = releaseController
	service.SetAccountDeactivator(deactivator)
	service.SetAccountDeprovisioner(deprovisioner)
	bootstrapService(t, service)

	mustSeedAccountDeleteState(t, service)
	mustHandle[callRecord](t, service, "calls.create", map[string]any{
		"call_id": "account-delete-call",
		"room_id": "!dm:remote.example",
	})

	result := mustHandle[map[string]any](t, service, "portal.account.delete", map[string]any{
		"confirm": "delete_account",
	})

	if result["status"] != "deprovisioned" {
		t.Fatalf("expected deprovisioned status, got %#v", result)
	}
	expectedLeaves := []string{
		"@owner:example.com from !dm:remote.example",
		"@owner:example.com from !member-group:example.com",
		"@owner:example.com from !member-channel:example.com",
	}
	if len(transport.leaves) != len(expectedLeaves) {
		t.Fatalf("expected leaves %#v, got %#v", expectedLeaves, transport.leaves)
	}
	for _, expected := range expectedLeaves {
		if !hasString(transport.leaves, expected) {
			t.Fatalf("expected leave %q, got %#v", expected, transport.leaves)
		}
	}
	if !hasDissolvedState(transport.stateEvents, "!owned-group:example.com", DirextalkRoomTypeGroup) {
		t.Fatalf("expected owned group dissolve state, got %#v", transport.stateEvents)
	}
	if !hasDissolvedState(transport.stateEvents, "!owned-channel:example.com", DirextalkRoomTypeChannel) {
		t.Fatalf("expected owned channel dissolve state, got %#v", transport.stateEvents)
	}
	if !hasDirectAccountDeletedState(transport.stateEvents, "!dm:remote.example") {
		t.Fatalf("expected direct account-deleted state, got %#v", transport.stateEvents)
	}
	if got, want := deactivator.users, []string{"owner", "agent"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("expected Matrix account deactivation %#v, got %#v", want, got)
	}
	if deprovisioner.calls != 1 {
		t.Fatalf("expected one deprovision call, got %d", deprovisioner.calls)
	}
	if len(releaseController.desiredStates) != 1 || releaseController.desiredStates[0] != releasecontrol.DesiredStateDeprovisioned {
		t.Fatalf("expected desired state deprovisioned before destructive delete, got %#v", releaseController.desiredStates)
	}
	if contacts, err := service.listContacts(context.Background()); err != nil || len(contacts) != 0 {
		t.Fatalf("volatile contacts survived account reset: contacts=%#v err=%v", contacts, err)
	}
	if groups, err := service.listGroups(context.Background()); err != nil || len(groups) != 0 {
		t.Fatalf("volatile groups survived account reset: groups=%#v err=%v", groups, err)
	}
	if channels, err := service.listChannels(context.Background()); err != nil || len(channels) != 0 {
		t.Fatalf("volatile channels survived account reset: channels=%#v err=%v", channels, err)
	}
	if calls, err := service.store.ListCalls(context.Background(), "", false); err != nil || len(calls) != 0 {
		t.Fatalf("volatile calls survived account reset: calls=%#v err=%v", calls, err)
	}
	if _, found, err := service.portalStore().LoadPortal(context.Background()); err != nil || found {
		t.Fatalf("volatile portal survived account reset: found=%v err=%v", found, err)
	}
}

func TestAccountDeleteKeepsMonotonicFenceWhenCriticalLeaveFails(t *testing.T) {
	ctx := context.Background()
	transport := &failingLeaveTransport{err: errors.New("leave failed")}
	service := NewServiceWithTransport(Config{ServerName: "example.com", NativeAgentRunner: &externalDeprovisionRunner{}}, transport)
	deprovisioner := &recordingAccountDeprovisioner{}
	releaseController := &recordingReleaseController{}
	service.releaseController = releaseController
	service.SetAccountDeprovisioner(deprovisioner)
	bootstrapService(t, service)
	mustSeedAccountDeleteState(t, service)

	if _, apiErr := service.Handle(ctx, "portal.account.delete", map[string]any{
		"confirm": "delete_account",
	}); apiErr == nil || apiErr.Status != http.StatusInternalServerError {
		t.Fatalf("expected leave failure before deprovision, got %#v", apiErr)
	}
	if deprovisioner.calls != 0 {
		t.Fatalf("deprovision must not run after critical leave failure")
	}
	assertDesiredStates(t, releaseController, releasecontrol.DesiredStateDeprovisioned)
}

func TestAccountDeleteNeverReopensAfterAnyDestructiveStageFails(t *testing.T) {
	tests := []struct {
		name      string
		transport Transport
		configure func(*testing.T, *Service)
	}{
		{name: "contact leave", transport: &nthLeaveFailureTransport{failAt: 1}},
		{name: "room leave", transport: &nthLeaveFailureTransport{failAt: 2}},
		{name: "account deactivate", transport: &recordingTransport{}, configure: func(t *testing.T, service *Service) {
			service.SetAccountDeactivator(&recordingAccountDeactivator{err: errors.New("deactivate failed")})
		}},
		{name: "credentials tombstone", transport: &recordingTransport{}, configure: func(t *testing.T, service *Service) {
			t.Setenv("P2P_PORTAL_CREDENTIALS_FILE", t.TempDir())
		}},
		{name: "database reset", transport: &recordingTransport{}, configure: func(t *testing.T, service *Service) {
			service.SetAccountDeprovisioner(&recordingAccountDeprovisioner{err: errors.New("reset failed")})
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			controller := &recordingReleaseController{}
			service := NewServiceWithTransport(Config{ServerName: "example.com", NativeAgentRunner: &externalDeprovisionRunner{}, ReleaseController: controller}, testCase.transport)
			service.SetAccountDeactivator(&recordingAccountDeactivator{})
			service.SetAccountDeprovisioner(&recordingAccountDeprovisioner{})
			bootstrapService(t, service)
			mustSeedAccountDeleteState(t, service)
			if testCase.configure != nil {
				testCase.configure(t, service)
			}

			if _, apiErr := service.Handle(context.Background(), "portal.account.delete", map[string]any{"confirm": "delete_account"}); apiErr == nil {
				t.Fatal("expected account deletion failure")
			}
			assertDesiredStates(t, controller, releasecontrol.DesiredStateDeprovisioned)
		})
	}
}

func TestAccountDeleteExplicitStandaloneModeAllowsMissingUpdater(t *testing.T) {
	controller := &recordingReleaseController{desiredErr: errors.New("updater unavailable")}
	service := NewServiceWithTransport(Config{
		ServerName:                       "example.com",
		NativeAgentRunner:                &externalDeprovisionRunner{},
		ReleaseController:                controller,
		AllowAccountDeleteWithoutUpdater: true,
	}, &recordingTransport{})
	deprovisioner := &recordingAccountDeprovisioner{}
	service.SetAccountDeactivator(&recordingAccountDeactivator{})
	service.SetAccountDeprovisioner(deprovisioner)
	bootstrapService(t, service)
	mustSeedAccountDeleteState(t, service)

	result, apiErr := service.Handle(context.Background(), "portal.account.delete", map[string]any{"confirm": "delete_account"})
	if apiErr != nil {
		t.Fatalf("standalone account deletion failed: %#v", apiErr)
	}
	value, ok := result.(map[string]any)
	if !ok || value["status"] != "deprovisioned" || deprovisioner.calls != 1 {
		t.Fatalf("standalone account deletion result=%#v calls=%d", result, deprovisioner.calls)
	}
	assertDesiredStates(t, controller, releasecontrol.DesiredStateDeprovisioned)
}

func TestAccountDeleteStopsBeforeDestructiveWorkWhenDesiredStateFails(t *testing.T) {
	transport := &recordingTransport{}
	controller := &recordingReleaseController{desiredErr: errors.New("updater unavailable with secret-token")}
	service := NewServiceWithTransport(Config{ServerName: "example.com", NativeAgentRunner: &externalDeprovisionRunner{}, ReleaseController: controller}, transport)
	deactivator := &recordingAccountDeactivator{}
	deprovisioner := &recordingAccountDeprovisioner{}
	service.SetAccountDeactivator(deactivator)
	service.SetAccountDeprovisioner(deprovisioner)
	bootstrapService(t, service)
	mustSeedAccountDeleteState(t, service)

	_, apiErr := service.Handle(context.Background(), "portal.account.delete", map[string]any{"confirm": "delete_account"})
	if apiErr == nil || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != updaterUnavailableCode {
		t.Fatalf("expected updater failure before destructive account work, got %#v", apiErr)
	}
	if len(transport.leaves) != 0 || len(deactivator.users) != 0 || deprovisioner.calls != 0 {
		t.Fatalf("destructive work ran after desired-state failure: leaves=%#v users=%#v deprovision=%d", transport.leaves, deactivator.users, deprovisioner.calls)
	}
}

func TestAccountDeleteDirectDissolveProjectsPeerContactDeleted(t *testing.T) {
	ctx := context.Background()
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	if err := service.saveContact(ctx, contactRecord{
		RoomID:      "!dm:remote.example",
		PeerMXID:    "@peer:remote.example",
		DisplayName: "Peer",
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	event := trustedStateEvent(t, "!dm:remote.example", "@peer:remote.example", DirextalkRoomProfileEventType, "", map[string]any{
		"room_type":       DirextalkRoomTypeDirect,
		"requester_mxid":  "@peer:remote.example",
		"target_mxid":     "@owner:example.com",
		"display_name":    "Peer",
		"dissolved":       true,
		"account_deleted": true,
		"deleted_mxid":    "@peer:remote.example",
	})
	if err := service.ProjectRoomEvent(ctx, event); err != nil {
		t.Fatal(err)
	}

	visible, err := service.listContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("expected deleted peer contact to be hidden, got %#v", visible)
	}
	raw, err := service.rawContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := findByRoom(raw, "!dm:remote.example"); got == nil || got.Status != "deleted" {
		t.Fatalf("expected raw contact to be marked deleted, got %#v", raw)
	}
}

func mustSeedAccountDeleteState(t *testing.T, service *Service) {
	t.Helper()
	ctx := context.Background()
	if err := service.saveContact(ctx, contactRecord{
		RoomID:   "!dm:remote.example",
		PeerMXID: "@peer:remote.example",
		Status:   "accepted",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(ctx, memberRecord{
		RoomID:     "!dm:remote.example",
		UserID:     "@owner:example.com",
		Membership: "join",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!owned-group:example.com",
		"name":    "Owned Group",
	})
	mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "owned-channel",
		"room_id":    "!owned-channel:example.com",
		"name":       "Owned Channel",
	})
	if err := service.saveGroup(ctx, groupRecord{
		RoomID: "!member-group:example.com",
		Name:   "Member Group",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(ctx, memberRecord{
		RoomID:     "!member-group:example.com",
		UserID:     "@owner:example.com",
		Membership: "join",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveChannel(ctx, channel{
		ChannelID: "member-channel",
		RoomID:    "!member-channel:example.com",
		Name:      "Member Channel",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(ctx, memberRecord{
		RoomID:     "!member-channel:example.com",
		ChannelID:  "member-channel",
		UserID:     "@owner:example.com",
		Membership: "join",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}
}

func hasDissolvedState(states []SendStateEventRequest, roomID, roomType string) bool {
	for _, state := range states {
		if state.RoomID != roomID || state.Event.Type != DirextalkRoomProfileEventType {
			continue
		}
		if state.Event.Content["room_type"] == roomType && state.Event.Content["dissolved"] == true {
			return true
		}
	}
	return false
}

func hasDirectAccountDeletedState(states []SendStateEventRequest, roomID string) bool {
	for _, state := range states {
		if state.RoomID != roomID || state.Event.Type != DirextalkRoomProfileEventType {
			continue
		}
		if state.Event.Content["room_type"] == DirextalkRoomTypeDirect &&
			state.Event.Content["dissolved"] == true &&
			state.Event.Content["account_deleted"] == true &&
			state.Event.Content["deleted_mxid"] == "@owner:example.com" {
			return true
		}
	}
	return false
}

func findByRoom(contacts []contactRecord, roomID string) *contactRecord {
	for i := range contacts {
		if contacts[i].RoomID == roomID {
			return &contacts[i]
		}
	}
	return nil
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertDesiredStates(t *testing.T, controller *recordingReleaseController, want ...releasecontrol.DesiredState) {
	t.Helper()
	if len(controller.desiredStates) != len(want) {
		t.Fatalf("desired state calls got %#v want %#v", controller.desiredStates, want)
	}
	for i := range want {
		if controller.desiredStates[i] != want[i] {
			t.Fatalf("desired state calls got %#v want %#v", controller.desiredStates, want)
		}
	}
}
