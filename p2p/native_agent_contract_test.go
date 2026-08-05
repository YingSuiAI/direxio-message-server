package p2p

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	pluginsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/plugins"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/coder/websocket"
)

func TestNativeAgentActionsAreOwnerOnlyAndCallNativeRuntimeDirectly(t *testing.T) {
	dockerRunner := &recordingPluginRunner{}
	nativeRunner := &recordingNativeAgentRunner{}
	service := NewService(Config{
		ServerName:        "example.com",
		PluginRunner:      dockerRunner,
		NativeAgentRunner: nativeRunner,
	})
	router := newP2PTestRouter(service)

	if service.Authorize(service.AgentToken(), "agent.chat") {
		t.Fatal("agent_token must not authorize native agent owner actions")
	}

	agentReq := jsonRequest(t, "/_p2p/command", map[string]any{
		"action": "agent.chat",
		"params": map[string]any{"prompt": "hello"},
	})
	agentReq.Header.Set("Authorization", "Bearer "+service.AgentToken())
	agentRec := httptest.NewRecorder()
	router.ServeHTTP(agentRec, agentReq)
	if agentRec.Code != http.StatusUnauthorized {
		t.Fatalf("agent_token must be rejected for agent.chat, got %d body=%s", agentRec.Code, agentRec.Body.String())
	}

	ownerReq := jsonRequest(t, "/_p2p/command", map[string]any{
		"action": "agent.chat",
		"params": map[string]any{
			"prompt": "hello",
			"model_profile": map[string]any{
				"id":       "deepseek:chat",
				"provider": "deepseek",
				"model":    "deepseek-chat",
				"api_key":  "sk-client-only",
			},
		},
	})
	ownerReq.Header.Set("Authorization", "Bearer "+service.AccessToken())
	ownerRec := httptest.NewRecorder()
	router.ServeHTTP(ownerRec, ownerReq)
	if ownerRec.Code != http.StatusOK {
		t.Fatalf("owner native agent action must succeed, got %d body=%s", ownerRec.Code, ownerRec.Body.String())
	}
	if len(dockerRunner.invokes) != 0 {
		t.Fatalf("native agent action must not reach docker plugin runner, got %#v", dockerRunner.invokes)
	}
	if len(nativeRunner.invokes) != 1 {
		t.Fatalf("expected one direct native runtime invoke, got %#v", nativeRunner.invokes)
	}
	invoke := nativeRunner.invokes[0]
	if invoke.Action != "agent.chat" {
		t.Fatalf("native runtime must be called by native action name, got %#v", invoke)
	}
	profile, ok := invoke.Params["model_profile"].(map[string]any)
	if !ok || profile["api_key"] != "sk-client-only" {
		t.Fatalf("request-scoped model profile must pass through direct action, got %#v", invoke.Params)
	}
}

func TestDedicatedRemoteRunnerOwnsChatAndModelDiscovery(t *testing.T) {
	localRunner := &recordingNativeAgentRunner{}
	chatRunner := &recordingNativeAgentRunner{}
	service := NewService(Config{
		ServerName:            "example.com",
		NativeAgentRunner:     localRunner,
		NativeAgentChatRunner: chatRunner,
	})

	if _, apiErr := service.Handle(context.Background(), "agent.chat", map[string]any{"prompt": "hello"}); apiErr != nil {
		t.Fatalf("dedicated Chat Runner failed: %#v", apiErr)
	}
	if _, apiErr := service.Handle(context.Background(), "agent.models.list", nil); apiErr != nil {
		t.Fatalf("local runtime action failed: %#v", apiErr)
	}
	if len(chatRunner.invokes) != 2 || chatRunner.invokes[0].Action != "agent.chat" || chatRunner.invokes[1].Action != "agent.models.list" {
		t.Fatalf("Chat and model discovery must use the dedicated Runner, got %#v", chatRunner.invokes)
	}
	if len(localRunner.invokes) != 0 {
		t.Fatalf("model discovery must not expose credentials to the local runtime, got %#v", localRunner.invokes)
	}
}

func TestNativeAgentIsNotManagedAsPlugin(t *testing.T) {
	runner := &recordingPluginRunner{}
	service := NewService(Config{ServerName: "example.com", PluginRunner: runner, NativeAgentRunner: &recordingNativeAgentRunner{}})

	catalog := mustHandle[map[string]any](t, service, "plugins.catalog.list", nil)
	entries := catalog["plugins"].([]pluginCatalogEntry)
	if catalogHasPluginID(entries, pluginsmodule.LegacyAgentPluginID) {
		t.Fatalf("agent plugin must not be returned in catalog, got %#v", entries)
	}
	if !catalogHasPlugin(entries, pluginsmodule.OpsPluginID) {
		t.Fatalf("ops plugin must remain available when docker runner is enabled, got %#v", entries)
	}

	if err := service.store.UpsertPlugin(context.Background(), pluginInstance{ID: pluginsmodule.LegacyAgentPluginID, Name: "Hidden Agent", Status: pluginsmodule.StatusEnabled, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := service.store.UpsertPlugin(context.Background(), pluginInstance{ID: pluginsmodule.OpsPluginID, Name: "Dirextalk Ops", Status: pluginsmodule.StatusEnabled, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	installed := mustHandle[map[string]any](t, service, "plugins.installed.list", nil)
	plugins := installed["plugins"].([]pluginInstance)
	if pluginInstancesHaveID(plugins, pluginsmodule.LegacyAgentPluginID) {
		t.Fatalf("agent plugin must be hidden from installed plugin list, got %#v", plugins)
	}
	if !pluginInstancesHaveID(plugins, pluginsmodule.OpsPluginID) {
		t.Fatalf("ops plugin must remain visible in installed list, got %#v", plugins)
	}

	for _, action := range []string{"plugins.install", "plugins.enable", "plugins.disable", "plugins.uninstall", "plugins.config.get", "plugins.health", "plugins.logs.tail"} {
		_, apiErr := service.Handle(context.Background(), action, map[string]any{"plugin_id": pluginsmodule.LegacyAgentPluginID})
		if apiErr == nil || apiErr.Status < http.StatusBadRequest {
			t.Fatalf("expected %s to reject agent plugin, got %#v", action, apiErr)
		}
	}
	_, apiErr := service.Handle(context.Background(), "plugins.invoke", map[string]any{
		"plugin_id": pluginsmodule.LegacyAgentPluginID,
		"action":    "agent.chat",
		"params":    map[string]any{"prompt": "hello"},
	})
	if apiErr == nil || apiErr.Status < http.StatusBadRequest {
		t.Fatalf("plugins.invoke must reject agent plugin, got %#v", apiErr)
	}
}

func TestNativeAgentConfigStoreUsesPortalAgentConfigNotPluginRecords(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.agentConfig.Native = map[string]any{
		"skills": []any{
			map[string]any{"id": "portal-skill", "enabled": true},
		},
		"model_profiles": []any{
			map[string]any{"id": "portal-profile", "provider": "deepseek", "model": "deepseek-chat"},
		},
	}
	if err := service.store.UpsertPlugin(context.Background(), pluginInstance{
		ID:      pluginsmodule.LegacyAgentPluginID,
		Name:    "Legacy Agent",
		Enabled: true,
		Config: map[string]any{
			"skills": []any{
				map[string]any{"id": "legacy-skill", "enabled": true},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	store := nativeAgentConfigStore{service: service}
	loaded, exists, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("expected native portal agent config to exist")
	}
	skills, ok := loaded["skills"].([]any)
	if !ok || len(skills) != 1 {
		t.Fatalf("expected portal-backed skills, got %#v", loaded["skills"])
	}
	skill, ok := skills[0].(map[string]any)
	if !ok || skill["id"] != "portal-skill" {
		t.Fatalf("native config store must not read legacy plugin config, got %#v", loaded)
	}
	if hasNestedKey(loaded, "api_key") || hasNestedKey(loaded, "api_key_ref") {
		t.Fatalf("native config load must not expose model API keys, got %#v", loaded)
	}

	if err := store.Save(context.Background(), map[string]any{
		"display_name": "Saved Native Agent",
		"skills": []any{
			map[string]any{"id": "saved-skill", "enabled": true},
		},
		"model_profiles": []any{
			map[string]any{"id": "saved-profile", "api_key": "sk-save", "api_key_ref": "secret-ref", "model": "deepseek-chat"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := service.agentConfig.DisplayName; got != "Saved Native Agent" {
		t.Fatalf("expected Save to update portal agent config, got %q", got)
	}
	plugin, ok, err := service.store.GetPlugin(context.Background(), pluginsmodule.LegacyAgentPluginID)
	if err != nil || !ok {
		t.Fatalf("expected untouched legacy plugin to remain for migration compatibility, ok=%v err=%v", ok, err)
	}
	legacySkills := plugin.Config["skills"].([]any)
	legacySkill := legacySkills[0].(map[string]any)
	if legacySkill["id"] != "legacy-skill" {
		t.Fatalf("native config store must not write legacy plugin config, got %#v", plugin.Config)
	}
	loaded, _, err = store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hasNestedKey(loaded, "api_key") || hasNestedKey(loaded, "api_key_ref") {
		t.Fatalf("native config save must strip model API keys, got %#v", loaded)
	}
}

func TestNativeAgentToolsUseBlockedRoomsFromNativeConfigStore(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!visible:example.com",
		"name":    "Visible Group",
	})
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!blocked:example.com",
		"name":    "Blocked Group",
	})

	store := nativeAgentConfigStore{service: service}
	if err := store.Save(context.Background(), map[string]any{
		"mcp_blocked_room_ids": []any{"!blocked:example.com"},
	}); err != nil {
		t.Fatal(err)
	}

	capability := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomsSearch, map[string]any{"type": "group"})
	capabilityRooms := capability["rooms"].([]mcpRoomSummary)
	if len(capabilityRooms) != 1 || capabilityRooms[0].RoomID != "!visible:example.com" {
		t.Fatalf("MCP capability service must use native blocked-room config, got %#v", capabilityRooms)
	}

	var roomsTool *nativeagentToolForTest
	for _, tool := range agentmodule.Tools(service.dirextalkMCPService()) {
		if tool.Name == "dirextalk_rooms_search" {
			roomsTool = &nativeagentToolForTest{handler: tool.Handler}
			break
		}
	}
	if roomsTool == nil {
		t.Fatal("expected dirextalk_rooms_search native tool")
	}
	result, err := roomsTool.handler(context.Background(), map[string]any{"type": "group"})
	if err != nil {
		t.Fatal(err)
	}
	nativeRooms := result.(map[string]any)["rooms"].([]mcpRoomSummary)
	if len(nativeRooms) != 1 || nativeRooms[0].RoomID != "!visible:example.com" {
		t.Fatalf("native Agent tool must use same blocked-room config as MCP action, got %#v", nativeRooms)
	}
}

type nativeagentToolForTest struct {
	handler func(context.Context, map[string]any) (any, error)
}

func hasNestedKey(value any, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for k, v := range typed {
			if k == key || hasNestedKey(v, key) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if hasNestedKey(item, key) {
				return true
			}
		}
	}
	return false
}

func TestNativeAgentRealtimeStreamFramesUseDedicatedChatRunner(t *testing.T) {
	dockerRunner := &recordingPluginRunner{}
	localRunner := &recordingNativeAgentRunner{}
	chatRunner := &recordingNativeAgentRunner{}
	service := NewService(Config{
		ServerName: "example.com", PluginRunner: dockerRunner,
		NativeAgentRunner: localRunner, NativeAgentChatRunner: chatRunner,
	})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.native_agent_stream",
		"id":     "native-stream-1",
		"action": "agent.chat",
		"params": map[string]any{"prompt": "hello"},
	})
	accepted := readRealtimeFrame(t, conn)
	if accepted["type"] != "server.native_agent_stream.accepted" || accepted["id"] != "native-stream-1" || accepted["state"] != "running" {
		t.Fatalf("expected native stream accepted frame, got %#v", accepted)
	}
	delta := readRealtimeFrame(t, conn)
	if delta["type"] != "server.native_agent_stream.event" || delta["id"] != "native-stream-1" || delta["event"] != "delta" {
		t.Fatalf("expected native stream delta frame, got %#v", delta)
	}
	done := readRealtimeFrame(t, conn)
	if done["type"] != "server.native_agent_stream.event" || done["id"] != "native-stream-1" || done["event"] != "done" {
		t.Fatalf("expected native stream done frame, got %#v", done)
	}
	if len(dockerRunner.streams) != 0 {
		t.Fatalf("native stream must not reach docker plugin runner, got %#v", dockerRunner.streams)
	}
	if len(chatRunner.streams) != 1 || chatRunner.streams[0].Action != "agent.chat.stream" {
		t.Fatalf("expected dedicated Chat stream call, got %#v", chatRunner.streams)
	}
	if len(localRunner.streams) != 0 {
		t.Fatalf("remote Chat stream must not also execute locally, got %#v", localRunner.streams)
	}
}

func TestNativeAgentDurableStreamUsesDedicatedChatRunner(t *testing.T) {
	localRunner := &recordingNativeAgentRunner{}
	chatRunner := &recordingNativeAgentRunner{}
	service := NewService(Config{
		ServerName: "example.com", NativeAgentRunner: localRunner, NativeAgentChatRunner: chatRunner,
	})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.native_agent_stream", "id": "durable-native-stream", "action": "agent.chat",
		"params": map[string]any{
			"prompt": "hello", "turn_id": "turn-remote-1", "conversation_id": "conversation-remote-1",
		},
	})
	accepted := readRealtimeFrame(t, conn)
	if accepted["type"] != "server.native_agent_stream.accepted" || accepted["turn_id"] != "turn-remote-1" {
		t.Fatalf("expected durable stream acceptance, got %#v", accepted)
	}
	delta := readRealtimeFrame(t, conn)
	if delta["type"] != "server.native_agent_stream.event" || delta["event"] != "delta" {
		t.Fatalf("expected durable stream delta, got %#v", delta)
	}
	done := readRealtimeFrame(t, conn)
	if done["type"] != "server.native_agent_stream.event" || done["event"] != "done" {
		t.Fatalf("expected durable stream completion, got %#v", done)
	}
	if len(chatRunner.streams) != 1 || chatRunner.streams[0].Action != "agent.chat.stream" {
		t.Fatalf("durable stream must use dedicated Chat Runner, got %#v", chatRunner.streams)
	}
	if len(localRunner.streams) != 0 {
		t.Fatalf("durable remote Chat must not execute locally, got %#v", localRunner.streams)
	}
}

func TestNativeAgentRealtimeStreamRejectsNonStreamActionsBeforeRunner(t *testing.T) {
	nativeRunner := &recordingNativeAgentRunner{}
	service := NewService(Config{ServerName: "example.com", NativeAgentRunner: nativeRunner})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.native_agent_stream",
		"id":     "native-non-stream",
		"action": "agent.runtime.run",
		"params": map[string]any{"prompt": "hello"},
	})
	frame := readRealtimeFrame(t, conn)
	if frame["type"] != "server.native_agent_stream.error" || frame["id"] != "native-non-stream" || int(frame["status"].(float64)) != http.StatusBadRequest {
		t.Fatalf("expected non-stream action rejection, got %#v", frame)
	}
	if len(nativeRunner.streams) != 0 {
		t.Fatalf("non-stream action must not reach native runner, got %#v", nativeRunner.streams)
	}
}

type nativeAgentCall struct {
	Action string
	Params map[string]any
}

type recordingNativeAgentRunner struct {
	applies []string
	invokes []nativeAgentCall
	streams []nativeAgentCall
}

func (r *recordingNativeAgentRunner) Apply(ctx context.Context, action string) error {
	r.applies = append(r.applies, action)
	return nil
}

func (r *recordingNativeAgentRunner) Invoke(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	r.invokes = append(r.invokes, nativeAgentCall{Action: action, Params: params})
	return map[string]any{
		"ok":    true,
		"text":  "hello from native agent",
		"model": params["model"],
	}, nil
}

func (r *recordingNativeAgentRunner) Stream(ctx context.Context, action string, params map[string]any, emit func(nativeagent.Event) error) error {
	r.streams = append(r.streams, nativeAgentCall{Action: action, Params: params})
	if err := emit(nativeagent.Event{Event: "delta", Data: map[string]any{"text": "hel"}}); err != nil {
		return err
	}
	return emit(nativeagent.Event{Event: "done", Data: map[string]any{"text": "hello"}})
}

func TestNativeAgentRealtimeStreamCancelAndErrorFrames(t *testing.T) {
	cancelRunner := &blockingNativeAgentRunner{started: make(chan struct{})}
	service := NewService(Config{ServerName: "example.com", NativeAgentRunner: cancelRunner})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.native_agent_stream",
		"id":     "native-cancel",
		"action": "agent.chat.stream",
		"params": map[string]any{"prompt": "hold"},
	})
	accepted := readRealtimeFrame(t, conn)
	if accepted["type"] != "server.native_agent_stream.accepted" || accepted["id"] != "native-cancel" {
		t.Fatalf("expected native stream accepted frame, got %#v", accepted)
	}
	<-cancelRunner.started
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.native_agent_stream.cancel", "id": "native-cancel"})
	cancelled := readRealtimeFrame(t, conn)
	if cancelled["type"] != "server.native_agent_stream.cancelled" || cancelled["id"] != "native-cancel" || cancelled["ok"] != true {
		t.Fatalf("expected native stream cancelled frame, got %#v", cancelled)
	}
	if _, ok := cancelled["execution_continues"]; ok {
		t.Fatalf("legacy cancel frame must remain unchanged, got %#v", cancelled)
	}

	errorRunner := &erroringNativeAgentRunner{}
	errorService := NewService(Config{ServerName: "example.com", NativeAgentRunner: errorRunner})
	errorRouter := newP2PTestRouter(errorService)
	errorServer := httptest.NewServer(errorRouter)
	defer errorServer.Close()
	errorConn := dialRealtimeWS(t, errorServer.URL, mustCreateRealtimeWSTicket(t, errorRouter, errorService.AccessToken()))
	defer errorConn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, errorConn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, errorConn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, errorConn, map[string]any{
		"type":   "client.native_agent_stream",
		"id":     "native-error",
		"action": "agent.chat.stream",
		"params": map[string]any{"prompt": "boom"},
	})
	errorAccepted := readRealtimeFrame(t, errorConn)
	if errorAccepted["type"] != "server.native_agent_stream.accepted" || errorAccepted["id"] != "native-error" {
		t.Fatalf("expected native stream accepted frame, got %#v", errorAccepted)
	}
	frame := readRealtimeFrame(t, errorConn)
	if frame["type"] != "server.native_agent_stream.error" || frame["id"] != "native-error" || int(frame["status"].(float64)) != http.StatusBadGateway {
		t.Fatalf("expected native stream error frame, got %#v", frame)
	}
}

type blockingNativeAgentRunner struct {
	started chan struct{}
}

func (r *blockingNativeAgentRunner) Apply(context.Context, string) error {
	return nil
}

func (r *blockingNativeAgentRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (r *blockingNativeAgentRunner) Stream(ctx context.Context, action string, params map[string]any, emit func(nativeagent.Event) error) error {
	close(r.started)
	<-ctx.Done()
	return ctx.Err()
}

type erroringNativeAgentRunner struct{}

func (r *erroringNativeAgentRunner) Apply(context.Context, string) error {
	return nil
}

func (r *erroringNativeAgentRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return nil, errors.New("native boom")
}

func (r *erroringNativeAgentRunner) Stream(context.Context, string, map[string]any, func(nativeagent.Event) error) error {
	return errors.New("native boom")
}

func pluginInstancesHaveID(plugins []pluginInstance, pluginID string) bool {
	for _, plugin := range plugins {
		if plugin.ID == pluginID {
			return true
		}
	}
	return false
}
