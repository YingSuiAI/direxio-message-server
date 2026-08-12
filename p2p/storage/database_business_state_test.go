package storage

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestDatabaseStoreUpsertContactIsUniqueByPeer(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if storeErr := store.UpsertContact(ctx, contactRecord{
		RoomID:      "!first:example.com",
		PeerMXID:    "@alice:remote.example",
		DisplayName: "Alice",
		AvatarURL:   "mxc://remote.example/alice-old",
		Remark:      "first request",
		Domain:      "remote.example",
		Status:      "pending_outbound",
	}); storeErr != nil {
		t.Fatal(storeErr)
	}
	if storeErr := store.UpsertContact(ctx, contactRecord{
		RoomID:      "!second:example.com",
		PeerMXID:    "@alice:remote.example",
		DisplayName: "Alice Updated",
		AvatarURL:   "mxc://remote.example/alice",
		Remark:      "updated request",
		Domain:      "remote.example",
		Status:      "accepted",
	}); storeErr != nil {
		t.Fatal(storeErr)
	}
	contacts, err := store.ListContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(contacts) != 1 || contacts[0].RoomID != "!second:example.com" || contacts[0].AvatarURL != "mxc://remote.example/alice" || contacts[0].Remark != "updated request" || contacts[0].Status != "accepted" {
		t.Fatalf("expected contact upsert to keep one row per peer, got %#v", contacts)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO p2p_contacts (room_id, peer_mxid, display_name, domain, status)
		VALUES ($1, $2, $3, $4, $5)
	`, "!third:example.com", "@alice:remote.example", "Alice Duplicate", "remote.example", "pending_outbound"); err == nil {
		t.Fatalf("expected raw duplicate contact insert to fail unique peer constraint")
	}
}

func TestDatabaseStoreCompareAndSwapContactGuardsRequestState(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	existing := contactRecord{
		RoomID: "!old:example.com", PeerMXID: "@peer:example.com",
		Status: "pending_inbound", RequestID: "request-a",
	}
	if err := store.UpsertContact(ctx, existing); err != nil {
		t.Fatal(err)
	}
	desired := existing
	desired.RoomID = "!replacement:example.com"
	desired.Status = "accepted"
	stale := existing
	stale.Status = "rejected"
	if saved, err := store.CompareAndSwapContact(ctx, desired, stale); err != nil || saved {
		t.Fatalf("stale contact state CAS = (%v, %v)", saved, err)
	}
	if saved, err := store.CompareAndSwapContact(ctx, desired, existing); err != nil || !saved {
		t.Fatalf("current contact state CAS = (%v, %v)", saved, err)
	}
	contacts, err := store.ListContacts(ctx)
	if err != nil || len(contacts) != 1 || contacts[0].RoomID != desired.RoomID || contacts[0].Status != "accepted" {
		t.Fatalf("contact after CAS = %#v, err=%v", contacts, err)
	}
}

func TestDatabaseStoreContactProjectionCASRollsBackConversationFailureAndReplays(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	existing := contactRecord{
		RoomID: "!projection:example.com", PeerMXID: "@peer:remote.example",
		Status: "joining", RequestID: "request-projection",
	}
	if err := store.UpsertContact(ctx, existing); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConversation(ctx, conversationFromContact(existing)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		CREATE FUNCTION p2p_test_fail_contact_projection_conversation() RETURNS trigger AS $$
		BEGIN
			IF NEW.matrix_room_id = '!projection:example.com' AND NEW.lifecycle = 'active' THEN
				RAISE EXCEPTION 'injected conversation projection failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		CREATE TRIGGER p2p_test_fail_contact_projection_conversation
		BEFORE INSERT OR UPDATE ON p2p_conversations
		FOR EACH ROW EXECUTE FUNCTION p2p_test_fail_contact_projection_conversation()
	`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.DB().ExecContext(context.Background(), `DROP TRIGGER IF EXISTS p2p_test_fail_contact_projection_conversation ON p2p_conversations`)
		_, _ = store.DB().ExecContext(context.Background(), `DROP FUNCTION IF EXISTS p2p_test_fail_contact_projection_conversation()`)
	}()

	accepted := existing
	accepted.Status = "accepted"
	accepted.Remark = ""
	if saved, err := store.CompareAndSwapContactProjection(ctx, accepted, existing); err == nil || saved {
		t.Fatalf("fault-injected contact projection CAS = (%v, %v), want rolled-back error", saved, err)
	}
	contacts, err := store.ListContacts(ctx)
	if err != nil || len(contacts) != 1 || contacts[0].Status != "joining" {
		t.Fatalf("failed contact projection committed partial contact: contacts=%#v err=%v", contacts, err)
	}
	conversation, found, err := store.GetConversationByRoomID(ctx, existing.RoomID)
	if err != nil || !found || conversation.Lifecycle != dirextalkdomain.ConversationLifecyclePending {
		t.Fatalf("failed contact projection changed conversation: conversation=%#v found=%v err=%v", conversation, found, err)
	}

	if _, err := store.DB().ExecContext(ctx, `DROP TRIGGER p2p_test_fail_contact_projection_conversation ON p2p_conversations`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `DROP FUNCTION p2p_test_fail_contact_projection_conversation()`); err != nil {
		t.Fatal(err)
	}
	if saved, err := store.CompareAndSwapContactProjection(ctx, accepted, existing); err != nil || !saved {
		t.Fatalf("replayed contact projection CAS = (%v, %v), want committed", saved, err)
	}
	contacts, err = store.ListContacts(ctx)
	if err != nil || len(contacts) != 1 || contacts[0].Status != "accepted" {
		t.Fatalf("replayed contact projection did not accept contact: contacts=%#v err=%v", contacts, err)
	}
	conversation, found, err = store.GetConversationByRoomID(ctx, existing.RoomID)
	if err != nil || !found || conversation.Lifecycle != dirextalkdomain.ConversationLifecycleActive {
		t.Fatalf("replayed contact projection did not activate conversation: conversation=%#v found=%v err=%v", conversation, found, err)
	}
}

func TestDatabaseStoreCompareAndSwapMemberGenerationGuardsState(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	existing := memberRecord{
		RoomID: "!channel:example.com", ChannelID: "channel-a", UserID: "@peer:example.com",
		Membership: "rejected", RequestID: "request-a", JoinedAt: 10,
	}
	if err := store.UpsertMember(ctx, existing); err != nil {
		t.Fatal(err)
	}
	desired := existing
	desired.Membership = "pending"
	desired.RequestID = "request-b"
	desired.JoinedAt = 20
	if saved, err := store.CompareAndSwapMemberGeneration(ctx, desired, existing.RequestID, "pending"); err != nil || saved {
		t.Fatalf("stale member state CAS = (%v, %v)", saved, err)
	}
	if saved, err := store.CompareAndSwapMemberGeneration(ctx, desired, existing.RequestID, existing.Membership); err != nil || !saved {
		t.Fatalf("current member state CAS = (%v, %v)", saved, err)
	}
	got, found, err := store.LookupMember(ctx, existing.RoomID, existing.UserID)
	if err != nil || !found || got.RequestID != desired.RequestID || got.JoinedAt != desired.JoinedAt || got.Membership != desired.Membership {
		t.Fatalf("member after CAS = (%#v, %v, %v)", got, found, err)
	}
}

func TestDatabaseStoreUpsertUsesJoinTransitionTimeAndPreservesJoinedTime(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	member := memberRecord{
		RoomID: "!join-time:example.com", UserID: "@peer:example.com",
		Membership: "invite", Role: "member", JoinedAt: 100,
	}
	if err := store.UpsertMember(ctx, member); err != nil {
		t.Fatal(err)
	}
	member.Membership = "join"
	member.JoinedAt = 200
	if err := store.UpsertMember(ctx, member); err != nil {
		t.Fatal(err)
	}
	member.JoinedAt = 300
	if err := store.UpsertMember(ctx, member); err != nil {
		t.Fatal(err)
	}

	got, found, err := store.LookupMember(ctx, member.RoomID, member.UserID)
	if err != nil || !found || got.JoinedAt != 200 {
		t.Fatalf("stored join timestamp = (%#v, %v, %v), want 200", got, found, err)
	}
}

func TestDatabaseStoreUpsertContactPersistsExplicitAvatarClearAcrossReopen(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	storeClosed := false
	defer func() {
		if !storeClosed {
			_ = store.Close()
		}
	}()

	contact := contactRecord{
		RoomID:      "!direct:example.com",
		PeerMXID:    "@alice:remote.example",
		DisplayName: "Alice",
		AvatarURL:   "mxc://remote.example/alice",
		Domain:      "remote.example",
		Status:      "accepted",
	}
	if err := store.UpsertContact(ctx, contact); err != nil {
		t.Fatal(err)
	}
	contact.AvatarURL = ""
	if err := store.UpsertContact(ctx, contact); err != nil {
		t.Fatal(err)
	}

	contacts, err := store.ListContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(contacts) != 1 || contacts[0].AvatarURL != "" {
		t.Fatalf("explicit avatar clear was not persisted: %#v", contacts)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	storeClosed = true

	reloaded, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	contacts, err = reloaded.ListContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(contacts) != 1 || contacts[0].AvatarURL != "" {
		t.Fatalf("explicit avatar clear did not survive reopen: %#v", contacts)
	}
}

func TestDatabaseStorePersistsBlocks(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	block := blockRecord{
		TargetType:  "channel",
		TargetID:    "!blocked:example.com",
		RoomID:      "!blocked:example.com",
		DisplayName: "Blocked Channel",
		AvatarURL:   "mxc://example.com/blocked",
		CreatedAt:   123,
	}
	if err := store.UpsertBlock(ctx, block); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlock(ctx, blockRecord{
		TargetType:  "channel",
		TargetID:    "!blocked:example.com",
		RoomID:      "!blocked:example.com",
		DisplayName: "Blocked Channel Updated",
		CreatedAt:   456,
	}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	blocks, err := reloaded.ListBlocks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].TargetID != "!blocked:example.com" || blocks[0].ChannelID != "" || blocks[0].DisplayName != "Blocked Channel Updated" || blocks[0].CreatedAt != 123 {
		t.Fatalf("expected one persisted block preserving original created_at, got %#v", blocks)
	}
	removed, err := reloaded.DeleteBlock(ctx, "channel", "!blocked:example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatalf("expected persisted block to be removed")
	}
	blocks, err = reloaded.ListBlocks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 {
		t.Fatalf("expected no blocks after removal, got %#v", blocks)
	}
}

func TestDatabaseStorePersistsPluginsAndJobs(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	plugin := pluginInstance{
		ID:        "io.dirextalk.ops",
		Name:      "Dirextalk Ops",
		Version:   "0.1.0",
		Image:     "docker.io/dirextalk/ops-plugin:latest",
		Digest:    "",
		Status:    "enabled",
		Enabled:   true,
		Config:    map[string]any{"backup_root": "/var/lib/dirextalk-ops/backups"},
		LastJobID: "job-install",
	}
	if err := store.UpsertPlugin(ctx, plugin); err != nil {
		t.Fatal(err)
	}
	job := pluginJob{
		JobID:    "job-install",
		PluginID: plugin.ID,
		Action:   "install",
		Status:   "succeeded",
		Message:  "installed",
	}
	if err := store.UpsertPluginJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPluginSecret(ctx, pluginSecret{
		PluginID:  plugin.ID,
		Name:      "ops_token",
		Value:     "ops-plugin-secret",
		UpdatedAt: 1710000000000,
	}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()

	plugins, err := reloaded.ListPlugins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 || plugins[0].ID != plugin.ID || !plugins[0].Enabled || plugins[0].Config["backup_root"] != "/var/lib/dirextalk-ops/backups" {
		t.Fatalf("expected persisted enabled plugin with config, got %#v", plugins)
	}
	gotJob, ok, err := reloaded.GetPluginJob(ctx, "job-install")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || gotJob.PluginID != plugin.ID || gotJob.Status != "succeeded" || gotJob.Message != "installed" {
		t.Fatalf("expected persisted plugin job, got ok=%v job=%#v", ok, gotJob)
	}
	gotSecret, ok, err := reloaded.GetPluginSecret(ctx, plugin.ID, "ops_token")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || gotSecret.Value != "ops-plugin-secret" || gotSecret.UpdatedAt != 1710000000000 {
		t.Fatalf("expected persisted plugin secret metadata, got ok=%v secret=%#v", ok, gotSecret)
	}
}

func TestDatabaseStoreListsJoinedGroupsAndChannelsForUser(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ownerMXID := "@owner:example.com"
	otherMXID := "@other:example.com"
	groups := []groupRecord{
		{RoomID: "!group-owner:example.com", Name: "Owner Group", InvitePolicy: "owner"},
		{RoomID: "!group-pending:example.com", Name: "Pending Group", InvitePolicy: "owner"},
		{RoomID: "!group-other:example.com", Name: "Other Group", InvitePolicy: "owner"},
	}
	for _, group := range groups {
		if err := store.UpsertGroup(ctx, group); err != nil {
			t.Fatal(err)
		}
	}
	channels := []channel{
		{ChannelID: "owner_channel", RoomID: "!channel-owner:example.com", Name: "Owner Channel", Visibility: "public", JoinPolicy: "open", ChannelType: "chat"},
		{ChannelID: "left_channel", RoomID: "!channel-left:example.com", Name: "Left Channel", Visibility: "public", JoinPolicy: "open", ChannelType: "chat"},
		{ChannelID: "other_channel", RoomID: "!channel-other:example.com", Name: "Other Channel", Visibility: "public", JoinPolicy: "open", ChannelType: "chat"},
	}
	for _, ch := range channels {
		if err := store.UpsertChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	members := []memberRecord{
		{RoomID: "!group-owner:example.com", UserID: ownerMXID, Domain: "example.com", Membership: "join", Role: "owner", JoinedAt: 10},
		{RoomID: "!group-pending:example.com", UserID: ownerMXID, Domain: "example.com", Membership: "pending", Role: "member", JoinedAt: 20},
		{RoomID: "!group-other:example.com", UserID: otherMXID, Domain: "example.com", Membership: "join", Role: "owner", JoinedAt: 30},
		{RoomID: "!channel-owner:example.com", ChannelID: "owner_channel", UserID: ownerMXID, Domain: "example.com", Membership: "join", Role: "owner", JoinedAt: 40},
		{RoomID: "!channel-left:example.com", ChannelID: "left_channel", UserID: ownerMXID, Domain: "example.com", Membership: "leave", Role: "member", JoinedAt: 50},
		{RoomID: "!channel-other:example.com", ChannelID: "other_channel", UserID: otherMXID, Domain: "example.com", Membership: "join", Role: "owner", JoinedAt: 60},
	}
	for _, member := range members {
		if err := store.UpsertMember(ctx, member); err != nil {
			t.Fatal(err)
		}
	}

	joinedGroups, err := store.ListJoinedGroupsForUser(ctx, ownerMXID)
	if err != nil {
		t.Fatal(err)
	}
	if len(joinedGroups) != 1 || joinedGroups[0].RoomID != "!group-owner:example.com" {
		t.Fatalf("expected only owner joined group, got %#v", joinedGroups)
	}
	joinedChannels, err := store.ListJoinedChannelsForUser(ctx, ownerMXID)
	if err != nil {
		t.Fatal(err)
	}
	if len(joinedChannels) != 1 || joinedChannels[0].ChannelID != "owner_channel" || !joinedChannels[0].IsOwned || joinedChannels[0].Role != "owner" || joinedChannels[0].MemberStatus != "join" {
		t.Fatalf("expected only owner joined channel with membership fields, got %#v", joinedChannels)
	}
	ownerMembers, err := store.ListMembersForUser(ctx, ownerMXID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerMembers) != 3 {
		t.Fatalf("expected owner visible memberships only, got %#v", ownerMembers)
	}
	for _, member := range ownerMembers {
		if member.UserID != ownerMXID || member.Membership == "leave" {
			t.Fatalf("expected owner non-left membership, got %#v", member)
		}
	}
}

func TestDatabaseStoreLooksUpGroupAndChannelWithoutFullList(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	group := groupRecord{RoomID: "!group:example.com", Name: "Group", Topic: "topic", InvitePolicy: "owner"}
	if err := store.UpsertGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	ch := channel{ChannelID: "channel_1", RoomID: "!channel:example.com", Name: "Channel", Visibility: "public", JoinPolicy: "open", ChannelType: "post", CommentsEnabled: true}
	if err := store.UpsertChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}

	gotGroup, ok, err := store.GetGroupByRoom(ctx, group.RoomID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || gotGroup.RoomID != group.RoomID || gotGroup.Name != group.Name {
		t.Fatalf("expected group lookup by room, got ok=%v group=%#v", ok, gotGroup)
	}
	gotByID, ok, err := store.GetChannelByIDOrRoom(ctx, ch.ChannelID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || gotByID.ChannelID != ch.ChannelID || !gotByID.CommentsEnabled {
		t.Fatalf("expected channel lookup by id, got ok=%v channel=%#v", ok, gotByID)
	}
	gotByRoom, ok, err := store.GetChannelByIDOrRoom(ctx, "", ch.RoomID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || gotByRoom.ChannelID != ch.ChannelID {
		t.Fatalf("expected channel lookup by room, got ok=%v channel=%#v", ok, gotByRoom)
	}
	missing, ok, err := store.GetChannelByIDOrRoom(ctx, "missing", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok || missing.ChannelID != "" {
		t.Fatalf("expected missing channel lookup, got ok=%v channel=%#v", ok, missing)
	}
}

func TestDatabaseStoreCountsJoinedMembers(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	members := []memberRecord{
		{RoomID: "!group:example.com", UserID: "@owner:example.com", Membership: "join", Role: "owner"},
		{RoomID: "!group:example.com", UserID: "@alice:example.com", Membership: "join", Role: "member"},
		{RoomID: "!group:example.com", UserID: "@left:example.com", Membership: "leave", Role: "member"},
		{RoomID: "!channel:example.com", ChannelID: "channel_1", UserID: "@owner:example.com", Membership: "join", Role: "owner"},
		{RoomID: "!channel:example.com", ChannelID: "channel_1", UserID: "@bob:example.com", Membership: "join", Role: "member"},
		{RoomID: "!channel:example.com", ChannelID: "channel_1", UserID: "@pending:example.com", Membership: "pending", Role: "member"},
	}
	for _, member := range members {
		if err := store.UpsertMember(ctx, member); err != nil {
			t.Fatal(err)
		}
	}

	groupCount, err := store.CountJoinedMembers(ctx, "!group:example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if groupCount != 2 {
		t.Fatalf("expected 2 joined group members, got %d", groupCount)
	}
	channelCount, err := store.CountJoinedMembers(ctx, "!channel:example.com", "channel_1")
	if err != nil {
		t.Fatal(err)
	}
	if channelCount != 2 {
		t.Fatalf("expected 2 joined channel members, got %d", channelCount)
	}
	joined, pending, err := store.CountProductMembers(ctx, "!channel:example.com", "channel_1")
	if err != nil {
		t.Fatal(err)
	}
	if joined != 2 || pending != 1 {
		t.Fatalf("expected channel joined=2 pending=1, got joined=%d pending=%d", joined, pending)
	}
}

func TestDatabaseStoreSearchesPublicChannelsAndListsOwnerPublicChannels(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ownerMXID := "@owner:example.com"
	channels := []channel{
		{ChannelID: "public_news", RoomID: "!public-news:example.com", Name: "Public News", Description: "daily updates", Visibility: "public", JoinPolicy: "open", ChannelType: "post"},
		{ChannelID: "public_other", RoomID: "!public-other:example.com", Name: "Public Other", Visibility: "public", JoinPolicy: "open", ChannelType: "chat"},
		{ChannelID: "private_news", RoomID: "!private-news:example.com", Name: "Private News", Visibility: "private", JoinPolicy: "invite", ChannelType: "chat"},
	}
	for _, ch := range channels {
		if err := store.UpsertChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	members := []memberRecord{
		{RoomID: "!public-news:example.com", ChannelID: "public_news", UserID: ownerMXID, Membership: "join", Role: "owner"},
		{RoomID: "!public-news:example.com", ChannelID: "public_news", UserID: "@alice:example.com", Membership: "join", Role: "member"},
		{RoomID: "!public-other:example.com", ChannelID: "public_other", UserID: "@other:example.com", Membership: "join", Role: "owner"},
		{RoomID: "!private-news:example.com", ChannelID: "private_news", UserID: ownerMXID, Membership: "join", Role: "owner"},
	}
	for _, member := range members {
		if err := store.UpsertMember(ctx, member); err != nil {
			t.Fatal(err)
		}
	}

	results, err := store.SearchPublicChannels(ctx, "news", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ChannelID != "public_news" {
		t.Fatalf("expected public news search result only, got %#v", results)
	}
	limited, err := store.SearchPublicChannels(ctx, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("expected search limit to apply, got %#v", limited)
	}
	owned, err := store.ListPublicChannelsForOwner(ctx, ownerMXID)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0].ChannelID != "public_news" {
		t.Fatalf("expected owner public channel only, got %#v", owned)
	}
}

func TestDatabaseStoreClientBuildUpdateUsesDeviceCASAndPreservesPortalFields(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state := portalState{
		Initialized:    true,
		Password:       "password",
		AccessToken:    "owner-token",
		MatrixDeviceID: "DEVICE_A",
		OwnerMXID:      "@owner:example.com",
		Profile:        dirextalkdomain.OwnerProfile{UserID: "@owner:example.com", DisplayName: "Initial"},
		AgentConfig: dirextalkdomain.AgentConfig{
			NativeAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Agent"},
			MCPBlockedRoomIDs:   []string{"!initial:example.com"},
		},
	}
	if err := store.SavePortal(ctx, state); err != nil {
		t.Fatal(err)
	}
	wrongDeviceUpdated, err := store.SaveClientBuild(ctx, "DEVICE_B", clientBuild{Version: "v9.9.9"})
	if err != nil || wrongDeviceUpdated {
		t.Fatalf("wrong device CAS updated=%v err=%v", wrongDeviceUpdated, err)
	}
	build := clientBuild{Version: "v2.3.4", BuildNumber: "42", Platform: "android", ReportedAt: "2026-07-10T12:00:00Z"}
	updated, err := store.SaveClientBuild(ctx, "DEVICE_A", build)
	if err != nil || !updated {
		t.Fatalf("current device CAS updated=%v err=%v", updated, err)
	}
	state.Profile.DisplayName = "Concurrent Profile"
	state.AgentConfig.MCPBlockedRoomIDs = []string{"!concurrent:example.com"}
	if err := store.SavePortal(ctx, state); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := store.LoadPortal(ctx)
	if err != nil || !ok {
		t.Fatalf("load portal ok=%v err=%v", ok, err)
	}
	if loaded.Profile.DisplayName != "Concurrent Profile" || len(loaded.AgentConfig.MCPBlockedRoomIDs) != 1 || loaded.AgentConfig.MCPBlockedRoomIDs[0] != "!concurrent:example.com" || loaded.ClientBuild != build {
		t.Fatalf("same-device portal write lost narrow client build or unrelated fields: %#v", loaded)
	}
	state.MatrixDeviceID = "DEVICE_B"
	state.ClientBuild = clientBuild{}
	if err := store.SavePortal(ctx, state); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err = store.LoadPortal(ctx)
	if err != nil || !ok || loaded.ClientBuild.Version != "" {
		t.Fatalf("device switch did not atomically clear client build: ok=%v err=%v state=%#v", ok, err, loaded)
	}
}

func TestDatabaseStorePersistsMemberRequesterNodeBaseURL(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	member := memberRecord{
		RoomID:               "!remote:c.example",
		ChannelID:            "remote_ch",
		UserID:               "@owner:b.example",
		Domain:               "b.example",
		Membership:           "pending",
		Role:                 "member",
		RequesterNodeBaseURL: "https://b.example/_p2p",
	}
	if err := store.UpsertMember(ctx, member); err != nil {
		t.Fatal(err)
	}

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, ok, err := reloadedStore.LookupMember(ctx, member.RoomID, member.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || reloaded.RequesterNodeBaseURL != member.RequesterNodeBaseURL {
		t.Fatalf("expected requester node base URL to survive reload, got ok=%v member=%#v", ok, reloaded)
	}
}
