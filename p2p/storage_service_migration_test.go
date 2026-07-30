package p2p

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	pluginsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/plugins"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestDatabaseStoreMigratesLegacyAgentPluginConfigToNativePortalConfig(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertPlugin(ctx, pluginInstance{
		ID:      pluginsmodule.LegacyAgentPluginID,
		Name:    "Legacy Agent",
		Status:  pluginsmodule.StatusEnabled,
		Enabled: true,
		Config: map[string]any{
			"display_name":         "Migrated Agent",
			"avatar_url":           "mxc://example.com/migrated-agent",
			"context_window":       float64(48),
			"enabled":              true,
			"model":                "legacy-model",
			"system_prompt":        "legacy prompt",
			"mcp_blocked_room_ids": []any{"!blocked:example.com"},
			"skills": []any{
				map[string]any{"id": "legacy-skill", "enabled": true},
			},
			"mcp_servers": []any{
				map[string]any{"id": "legacy-mcp", "enabled": true, "transport": "stdio"},
			},
			"runtime_tools": []any{
				map[string]any{"id": "legacy-tool", "enabled": true},
			},
			"model_profiles": []any{
				map[string]any{"id": "deepseek", "provider": "deepseek", "model": "deepseek-chat", "api_key": "sk-legacy", "api_key_ref": "legacy-ref"},
			},
			"api_key":     "sk-root",
			"api_key_ref": "root-ref",
		},
	}); err != nil {
		t.Fatal(err)
	}

	service, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, store)
	if err != nil {
		t.Fatal(err)
	}
	agentConfig := mustHandle[map[string]any](t, service, "agent.config.get", nil)
	if agentConfig["display_name"] != "Migrated Agent" ||
		agentConfig["avatar_url"] != "mxc://example.com/migrated-agent" ||
		agentConfig["model"] != "legacy-model" ||
		agentConfig["system_prompt"] != "legacy prompt" ||
		int64Param(agentConfig["context_window"]) != 48 {
		t.Fatalf("expected legacy shared config to migrate to native agent config, got %#v", agentConfig)
	}
	blockedRooms := agentConfig["mcp_blocked_room_ids"].([]string)
	if len(blockedRooms) != 1 || blockedRooms[0] != "!blocked:example.com" {
		t.Fatalf("expected migrated blocked rooms, got %#v", agentConfig["mcp_blocked_room_ids"])
	}

	skills := mustHandle[map[string]any](t, service, "agent.skills.list", nil)["skills"].([]map[string]any)
	if len(skills) != 1 || skills[0]["id"] != "legacy-skill" {
		t.Fatalf("expected legacy skills in native config storage, got %#v", skills)
	}
	servers := mustHandle[map[string]any](t, service, "agent.mcp.servers.list", nil)["servers"].([]map[string]any)
	if len(servers) != 1 || servers[0]["id"] != "legacy-mcp" {
		t.Fatalf("expected legacy MCP servers in native config storage, got %#v", servers)
	}
	loaded, exists, err := (nativeAgentConfigStore{service: service}).Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("expected migrated native config to exist")
	}
	if hasNestedKey(loaded, "api_key") || hasNestedKey(loaded, "api_key_ref") {
		t.Fatalf("migrated native config must not persist model API keys, got %#v", loaded)
	}

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	reloadedSkills := mustHandle[map[string]any](t, reloaded, "agent.skills.list", nil)["skills"].([]map[string]any)
	if len(reloadedSkills) != 1 || reloadedSkills[0]["id"] != "legacy-skill" {
		t.Fatalf("expected migrated skills to survive restart, got %#v", reloadedSkills)
	}
	secondReload, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := (nativeAgentConfigStore{service: secondReload}).Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	againSkills := again["skills"].([]any)
	if len(againSkills) != 1 {
		t.Fatalf("expected idempotent migration without duplicated skills, got %#v", againSkills)
	}
}

func TestDatabaseStoreRestoresPortalAndBusinessState(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	cm := sqlutil.NewConnectionManager(nil, dbOpts)
	store, err := NewDatabaseStore(ctx, cm, &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, store)
	if err != nil {
		t.Fatal(err)
	}

	defaultPassword := service.password
	session := bootstrapService(t, service)
	accessToken, _ := session["access_token"].(string)
	if accessToken == "" {
		t.Fatalf("expected access token in session: %#v", session)
	}
	mustHandle[ownerProfile](t, service, "profile.update", map[string]any{"display_name": "Owner Name", "email": "owner@example.com"})
	mustHandle[contactRecord](t, service, "contacts.request", map[string]any{"mxid": "@alice:remote.example", "display_name": "Alice", "avatar_url": "mxc://remote.example/alice"})
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{"room_id": "!group:example.com", "name": "Group"})
	mustHandle[callRecord](t, service, "calls.create", map[string]any{"call_id": "call_1", "room_id": "!room:example.com"})
	mustHandle[callRecord](t, service, "calls.event", map[string]any{
		"call_id":        "call_1",
		"event":          "connected",
		"answered_at_ms": int64(1767225600000),
	})
	mustHandle[callRecord](t, service, "calls.event", map[string]any{
		"call_id":       "call_1",
		"event":         "ended",
		"ended_at_ms":   int64(1767225605000),
		"ended_by_mxid": "@alice:remote.example",
		"reason":        "remote_hangup",
		"duration_ms":   int64(5000),
	})
	favorite := mustHandle[favoriteRecord](t, service, "favorites.add", map[string]any{"event_id": "$event", "room_id": "!room:example.com", "content": "fav", "message_type": "text"})
	mustHandle[followRecord](t, service, "follows.add", map[string]any{"domain": "remote.example"})
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{"channel_id": "ch_news", "name": "News", "channel_type": "post"})
	mustHandle[groupRecord](t, service, "groups.invite_policy.update", map[string]any{"room_id": group.RoomID, "invite_policy": "owner"})
	mustHandle[map[string]any](t, service, "channels.mute", map[string]any{"channel_id": ch.ChannelID})
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{"channel_id": ch.ChannelID, "body": "post body"})
	mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{"channel_id": ch.ChannelID, "post_id": post.PostID, "body": "comment body"})
	mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"display_name": "Storage Legacy Agent",
		"avatar_url":   "mxc://example.com/storage-legacy-agent",
		"native_agent_identity": map[string]any{
			"display_name": "Storage Ying",
			"avatar_url":   "mxc://example.com/storage-ying",
		},
		"online_agent_identity": map[string]any{
			"display_name": "Storage Online",
			"avatar_url":   "mxc://example.com/storage-online",
		},
		"context_window":       float64(96),
		"enabled":              true,
		"model":                "storage-model",
		"system_prompt":        "stored prompt",
		"mcp_blocked_room_ids": []any{"!secret:example.com", ch.RoomID},
	})
	mustReportClientVersion(t, service, map[string]any{
		"client_version": "3.4.5",
		"build_number":   "345",
		"platform":       "android",
	})
	service.systemRoomID = "!system:example.com"
	if err := store.SavePortal(ctx, service.portalStateLocked()); err != nil {
		t.Fatal(err)
	}
	report := reportRecord{
		ReportID:            "report_1",
		TargetType:          "channel",
		TargetRoomID:        ch.RoomID,
		TargetChannelID:     ch.ChannelID,
		TargetName:          ch.Name,
		ReporterMXID:        "@alice:remote.example",
		ReporterDisplayName: "Alice",
		Reason:              "Spam / Advertisement",
		Body:                "ads",
		ImageURLs:           []string{"mxc://example.com/evidence"},
		SystemRoomID:        "!system:example.com",
		EventID:             "$report:example.com",
		OriginServerTS:      1783433640000,
		CreatedAt:           "2026-07-07T10:14:00Z",
	}
	if err := store.InsertReport(ctx, report); err != nil {
		t.Fatal(err)
	}

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, reloadedStore)
	if err != nil {
		t.Fatal(err)
	}

	reloadedSession := mustHandle[map[string]any](t, reloaded, "portal.auth", map[string]any{"password": defaultPassword})
	if reloadedSession["access_token"] != accessToken {
		t.Fatalf("expected access token to survive reload, got %#v want %q", reloadedSession, accessToken)
	}
	if reloadedSession["system_room_id"] != "!system:example.com" {
		t.Fatalf("expected system room id to survive reload, got %#v", reloadedSession)
	}
	if reloaded.clientBuild.Version != "v3.4.5" || reloaded.clientBuild.BuildNumber != "345" || reloaded.clientBuild.Platform != "android" || reloaded.clientBuild.ReportedAt == "" {
		t.Fatalf("expected portal client build to survive reload, got %#v", reloaded.clientBuild)
	}
	profile := mustHandle[ownerProfile](t, reloaded, "profile.get", nil)
	if profile.DisplayName != "Owner Name" || profile.Email != "owner@example.com" {
		t.Fatalf("expected profile to survive reload, got %#v", profile)
	}
	agentConfig := mustHandle[map[string]any](t, reloaded, "agent.config.get", nil)
	blockedRooms := agentConfig["mcp_blocked_room_ids"].([]string)
	nativeIdentity := agentConfig["native_agent_identity"].(map[string]any)
	onlineIdentity := agentConfig["online_agent_identity"].(map[string]any)
	if agentConfig["display_name"] != "Storage Ying" ||
		agentConfig["avatar_url"] != "mxc://example.com/storage-ying" ||
		nativeIdentity["display_name"] != "Storage Ying" ||
		nativeIdentity["avatar_url"] != "mxc://example.com/storage-ying" ||
		onlineIdentity["display_name"] != "Storage Online" ||
		onlineIdentity["avatar_url"] != "mxc://example.com/storage-online" ||
		agentConfig["model"] != "storage-model" ||
		agentConfig["system_prompt"] != "stored prompt" ||
		int64Param(agentConfig["context_window"]) != 96 ||
		len(blockedRooms) != 2 ||
		blockedRooms[0] != "!secret:example.com" ||
		blockedRooms[1] != ch.RoomID {
		t.Fatalf("expected agent config to survive reload, got %#v", agentConfig)
	}
	channels := mustHandle[map[string]any](t, reloaded, "channels.list", nil)
	if got, ok := channels["channels"].([]channel); !ok || len(got) != 1 || got[0].Name != "News" || !got[0].Muted {
		t.Fatalf("expected restored channel, got %#v", channels)
	}
	bootstrap := mustHandle[map[string]any](t, reloaded, "sync.bootstrap", nil)
	if got, ok := bootstrap["contacts"].([]contactRecord); !ok || len(got) != 1 || got[0].PeerMXID != "@alice:remote.example" || got[0].AvatarURL != "mxc://remote.example/alice" {
		t.Fatalf("expected restored contacts in sync bootstrap, got %#v", bootstrap)
	}
	if got, ok := bootstrap["groups"].([]groupRecord); !ok || len(got) != 1 || got[0].RoomID != "!group:example.com" || got[0].InvitePolicy != "owner" {
		t.Fatalf("expected restored groups in sync bootstrap, got %#v", bootstrap)
	}
	calls := mustHandle[map[string]any](t, reloaded, "calls.list", nil)
	if got, ok := calls["calls"].([]callRecord); !ok || len(got) != 1 || got[0].CallID != "call_1" || got[0].State != "ended" || got[0].AnsweredAt == "" || got[0].EndedAt == "" || got[0].EndedByMXID != "@alice:remote.example" || got[0].EndReason != "remote_hangup" || got[0].DurationMS != 5000 {
		t.Fatalf("expected restored call, got %#v", calls)
	}
	favorites := mustHandle[map[string]any](t, reloaded, "favorites.list", map[string]any{"message_type": "text"})
	if got, ok := favorites["favorites"].([]favoriteRecord); !ok || len(got) != 1 || got[0].ID != favorite.ID {
		t.Fatalf("expected restored favorite, got %#v", favorites)
	}
	follows := mustHandle[map[string]any](t, reloaded, "follows.list", nil)
	if got, ok := follows["follows"].([]followRecord); !ok || len(got) != 1 || got[0].Domain != "remote.example" {
		t.Fatalf("expected restored follow, got %#v", follows)
	}
	posts := mustHandle[map[string]any](t, reloaded, "channels.posts.list", map[string]any{"channel_id": ch.ChannelID})
	if got, ok := posts["posts"].([]channelPostRecord); !ok || len(got) != 1 || got[0].Body != "post body" {
		t.Fatalf("expected restored post, got %#v", posts)
	}
	comments := mustHandle[map[string]any](t, reloaded, "channels.comments.list", map[string]any{"post_id": post.PostID})
	if got, ok := comments["comments"].([]channelCommentRecord); !ok || len(got) != 1 || got[0].Body != "comment body" {
		t.Fatalf("expected restored comment, got %#v", comments)
	}
	reports, err := reloadedStore.ListReports(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 ||
		reports[0].ReportID != "report_1" ||
		reports[0].TargetChannelID != ch.ChannelID ||
		reports[0].ReporterMXID != "@alice:remote.example" ||
		len(reports[0].ImageURLs) != 1 ||
		reports[0].ImageURLs[0] != "mxc://example.com/evidence" {
		t.Fatalf("expected restored report, got %#v", reports)
	}
}
