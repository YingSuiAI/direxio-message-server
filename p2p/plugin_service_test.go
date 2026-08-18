package p2p

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/plugins"
)

type recordingPluginRunner struct {
	operations []PluginRunnerOperation
	invokes    []PluginInvokeRequest
	streams    []PluginInvokeRequest
}

func (r *recordingPluginRunner) ApplyPlugin(ctx context.Context, op PluginRunnerOperation) error {
	r.operations = append(r.operations, op)
	return nil
}

func (r *recordingPluginRunner) InvokePlugin(ctx context.Context, req PluginInvokeRequest) (map[string]any, error) {
	r.invokes = append(r.invokes, req)
	return map[string]any{
		"ok":    true,
		"text":  "hello from plugin",
		"model": req.Params["model"],
	}, nil
}

func (r *recordingPluginRunner) StreamPlugin(ctx context.Context, req PluginInvokeRequest, emit func(PluginStreamEvent) error) error {
	r.streams = append(r.streams, req)
	if err := emit(PluginStreamEvent{Event: "delta", Data: map[string]any{"text": "hel"}}); err != nil {
		return err
	}
	return emit(PluginStreamEvent{Event: "done", Data: map[string]any{"text": "hello"}})
}

func TestPluginCatalogHidesOpsWhenRunnerDisabled(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})

	catalog := mustHandle[map[string]any](t, service, "plugins.catalog.list", nil)
	entries, ok := catalog["plugins"].([]pluginCatalogEntry)
	if !ok {
		t.Fatalf("expected typed plugin catalog entries, got %#v", catalog["plugins"])
	}
	if catalogHasPluginID(entries, pluginsmodule.OpsPluginID) {
		t.Fatalf("ops plugin requires docker runner and must be hidden when docker runner is disabled, got %#v", entries)
	}
}

func TestOpsInstallAndEnableUseOfficialDockerRunnerAndState(t *testing.T) {
	runner := &recordingPluginRunner{}
	service := NewService(Config{ServerName: "example.com", PluginRunner: runner})

	install := mustHandle[map[string]any](t, service, "plugins.install", map[string]any{
		"plugin_id": "io.dirextalk.ops",
	})
	if install["status"] != "installed" || install["job_id"] == "" {
		t.Fatalf("expected installed plugin job result, got %#v", install)
	}
	if len(runner.operations) != 1 || runner.operations[0].Action != "install" || runner.operations[0].PluginID != pluginsmodule.OpsPluginID || !pluginsmodule.OfficialImage(runner.operations[0].Image) {
		t.Fatalf("expected install runner operation for ops plugin, got %#v", runner.operations)
	}

	enable := mustHandle[map[string]any](t, service, "plugins.enable", map[string]any{
		"plugin_id": "io.dirextalk.ops",
	})
	if enable["status"] != "enabled" {
		t.Fatalf("expected enabled plugin result, got %#v", enable)
	}
	if len(runner.operations) != 2 || runner.operations[1].Action != "enable" || runner.operations[1].PluginID != "io.dirextalk.ops" {
		t.Fatalf("expected ops enable runner operation, got %#v", runner.operations)
	}

	disable := mustHandle[map[string]any](t, service, "plugins.disable", map[string]any{"plugin_id": pluginsmodule.OpsPluginID})
	uninstall := mustHandle[map[string]any](t, service, "plugins.uninstall", map[string]any{"plugin_id": pluginsmodule.OpsPluginID})
	if disable["status"] != pluginsmodule.StatusDisabled || uninstall["status"] != pluginsmodule.StatusRemoved {
		t.Fatalf("unexpected disable/uninstall results: disable=%#v uninstall=%#v", disable, uninstall)
	}
	if len(runner.operations) != 4 || runner.operations[2].Action != "disable" || runner.operations[3].Action != "uninstall" {
		t.Fatalf("plugin lifecycle runner order = %#v", runner.operations)
	}
}

func TestPluginConfigUpdateSeparatesSecretFromPublicConfig(t *testing.T) {
	service := NewService(Config{ServerName: "example.com", PluginRunner: &recordingPluginRunner{}})
	mustHandle[map[string]any](t, service, "plugins.install", map[string]any{"plugin_id": pluginsmodule.OpsPluginID})

	result := mustHandle[map[string]any](t, service, "plugins.config.update", map[string]any{
		"plugin_id": pluginsmodule.OpsPluginID,
		"config": map[string]any{
			"api_key":     "secret-value",
			"api_key_ref": "secret:api_key",
			"backup_root": "/backups",
			"model_profiles": []any{map[string]any{
				"id": "primary", "api_key": "profile-secret", "api_key_ref": "secret:model_profile_primary_api_key",
			}},
		},
	})
	config := result["config"].(map[string]any)
	if _, exposed := config["api_key"]; exposed || config["backup_root"] != "/backups" {
		t.Fatalf("public config leaked or lost fields: %#v", config)
	}
	secret, ok, err := service.store.GetPluginSecret(context.Background(), pluginsmodule.OpsPluginID, "api_key")
	if err != nil || !ok || secret.Value != "secret-value" {
		t.Fatalf("durable secret = %#v, ok=%v err=%v", secret, ok, err)
	}
	status := result["secret_status"].(map[string]any)["api_key"].(map[string]any)
	if status["configured"] != true {
		t.Fatalf("secret status = %#v", status)
	}
	profile := config["model_profiles"].([]any)[0].(map[string]any)
	if _, exposed := profile["api_key"]; exposed {
		t.Fatalf("model profile leaked api_key: %#v", profile)
	}
	profileSecret, ok, err := service.store.GetPluginSecret(context.Background(), pluginsmodule.OpsPluginID, "model_profile_primary_api_key")
	if err != nil || !ok || profileSecret.Value != "profile-secret" {
		t.Fatalf("durable model profile secret = %#v, ok=%v err=%v", profileSecret, ok, err)
	}
}

func TestPluginEnableBuildsOpsRuntimeConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		homeserver  string
		environment map[string]string
		wantEnv     map[string]string
		wantVolumes []string
	}{
		{
			name:       "configured ops environment",
			homeserver: "http://message-server:8008",
			environment: map[string]string{
				"P2P_OPS_BACKUP_VOLUME":            "dirextalk_ops_backups_test",
				"P2P_OPS_MAX_BACKUPS":              "9",
				"P2P_OPS_MESSAGE_SERVER_CONTAINER": "message-server-test",
				"P2P_OPS_POSTGRES_CONTAINER":       "postgres-test",
				"P2P_OPS_POSTGRES_USER":            "postgres-user-test",
				"P2P_OPS_POSTGRES_PASSWORD":        "postgres-password-test",
			},
			wantEnv: map[string]string{
				"DIREXTALK_BASE_URL":           "http://message-server:8008",
				"OPS_BACKUP_ROOT":              "/var/lib/dirextalk-ops/backups",
				"OPS_MAX_BACKUPS":              "9",
				"OPS_MESSAGE_SERVER_CONTAINER": "message-server-test",
				"OPS_POSTGRES_CONTAINER":       "postgres-test",
				"OPS_POSTGRES_USER":            "postgres-user-test",
				"OPS_POSTGRES_PASSWORD":        "postgres-password-test",
			},
			wantVolumes: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				"dirextalk_ops_backups_test:/var/lib/dirextalk-ops",
			},
		},
		{
			name:       "single node defaults",
			homeserver: "http://message-server:8008",
			wantEnv: map[string]string{
				"DIREXTALK_BASE_URL":           "http://message-server:8008",
				"OPS_BACKUP_ROOT":              "/var/lib/dirextalk-ops/backups",
				"OPS_MAX_BACKUPS":              "10",
				"OPS_MESSAGE_SERVER_CONTAINER": "dirextalk-p2p-message-server-1",
				"OPS_POSTGRES_CONTAINER":       "dirextalk-p2p-postgres-1",
				"OPS_POSTGRES_USER":            "dirextalk_message_server",
				"OPS_POSTGRES_PASSWORD":        "dirextalk_message_server",
			},
			wantVolumes: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				"p2p_ops_backups:/var/lib/dirextalk-ops",
			},
		},
		{
			name:       "auto homeserver backend override",
			homeserver: "http://auto",
			environment: map[string]string{
				"P2P_PLUGIN_BACKEND_BASE_URL": "http://message-server:8008",
			},
			wantEnv: map[string]string{"DIREXTALK_BASE_URL": "http://message-server:8008"},
		},
	}
	environmentKeys := []string{
		"P2P_PLUGIN_BACKEND_BASE_URL",
		"P2P_OPS_DOCKER_SOCKET",
		"P2P_OPS_BACKUP_VOLUME",
		"P2P_OPS_MAX_BACKUPS",
		"P2P_OPS_MESSAGE_SERVER_CONTAINER",
		"P2P_OPS_POSTGRES_CONTAINER",
		"P2P_OPS_POSTGRES_USER",
		"P2P_OPS_POSTGRES_PASSWORD",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range environmentKeys {
				t.Setenv(key, tc.environment[key])
			}
			runner := &recordingPluginRunner{}
			service := NewService(Config{ServerName: "example.com", Homeserver: tc.homeserver, PluginRunner: runner})
			mustHandle[map[string]any](t, service, "plugins.install", map[string]any{"plugin_id": "io.dirextalk.ops"})
			mustHandle[map[string]any](t, service, "plugins.enable", map[string]any{"plugin_id": "io.dirextalk.ops"})

			if len(runner.operations) != 2 {
				t.Fatalf("expected install and enable operations, got %#v", runner.operations)
			}
			op := runner.operations[1]
			for _, key := range []string{"DIREXTALK_AGENT_TOKEN", "DIREXTALK_AGENT_TOKEN_REF"} {
				if _, ok := op.Env[key]; ok {
					t.Fatalf("ops plugin must not receive %s, got %#v", key, op.Env)
				}
			}
			for key, want := range tc.wantEnv {
				if got := op.Env[key]; got != want {
					t.Fatalf("expected ops env %s=%q, got %q in %#v", key, want, got, op.Env)
				}
			}
			for _, want := range tc.wantVolumes {
				if !stringSliceContains(op.Volumes, want) {
					t.Fatalf("expected ops volume %q, got %#v", want, op.Volumes)
				}
			}
		})
	}
}

func TestPluginActionAllowlistIncludesOpsActions(t *testing.T) {
	entry, ok := pluginsmodule.FindOfficialPlugin(pluginsmodule.OpsPluginID)
	if !ok {
		t.Fatalf("expected ops plugin in official catalog")
	}
	for _, action := range []string{
		"ops.status.get",
		"ops.backup.create",
		"ops.backup.status",
		"ops.backup.download_chunk",
		"ops.cleanup.plan",
		"ops.cleanup.run",
		"ops.rooms.cleanup.plan",
		"ops.rooms.cleanup.run",
		"ops.media.orphans.plan",
		"ops.migration.export",
		"ops.restore.plan",
		"ops.restore.run",
	} {
		if !pluginsmodule.ActionAllowed(entry, action) {
			t.Fatalf("expected ops action %q to be allowed by catalog %#v", action, entry.Actions)
		}
	}
}

func catalogHasPluginID(entries []pluginCatalogEntry, pluginID string) bool {
	for _, entry := range entries {
		if entry.ID == pluginID {
			return true
		}
	}
	return false
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestPluginInvokeIsOwnerOnlyAndCallsEnabledOfficialPlugin(t *testing.T) {
	runner := &recordingPluginRunner{}
	service := NewService(Config{ServerName: "example.com", PluginRunner: runner})
	router := newP2PTestRouter(service)

	mustHandle[map[string]any](t, service, "plugins.install", map[string]any{
		"plugin_id": "io.dirextalk.ops",
	})
	mustHandle[map[string]any](t, service, "plugins.enable", map[string]any{
		"plugin_id": "io.dirextalk.ops",
	})

	agentReq := jsonRequest(t, "/_p2p/command", map[string]any{
		"action": "plugins.invoke",
		"params": map[string]any{
			"plugin_id": "io.dirextalk.ops",
			"action":    "ops.status.get",
			"params":    map[string]any{},
		},
	})
	agentReq.Header.Set("Authorization", "Bearer "+service.AgentToken())
	agentRec := httptest.NewRecorder()
	router.ServeHTTP(agentRec, agentReq)
	if agentRec.Code != http.StatusUnauthorized {
		t.Fatalf("agent token must not invoke plugins, got %d body=%s", agentRec.Code, agentRec.Body.String())
	}

	ownerReq := jsonRequest(t, "/_p2p/command", map[string]any{
		"action": "plugins.invoke",
		"params": map[string]any{
			"plugin_id": "io.dirextalk.ops",
			"action":    "ops.status.get",
			"params":    map[string]any{},
		},
	})
	ownerReq.Header.Set("Authorization", "Bearer "+service.AccessToken())
	ownerRec := httptest.NewRecorder()
	router.ServeHTTP(ownerRec, ownerReq)
	if ownerRec.Code != http.StatusOK {
		t.Fatalf("expected owner invoke to succeed, got %d body=%s", ownerRec.Code, ownerRec.Body.String())
	}
	if len(runner.invokes) != 1 || runner.invokes[0].Action != "ops.status.get" || runner.invokes[0].PluginID != "io.dirextalk.ops" {
		t.Fatalf("expected ops plugin invoke runner call, got %#v", runner.invokes)
	}
	result := decodeJSONMap(t, ownerRec.Body.String())
	if result["plugin_id"] != "io.dirextalk.ops" || result["action"] != "ops.status.get" {
		t.Fatalf("expected invoke envelope, got %#v", result)
	}
}

func TestPluginInstallRejectsUnknownPluginBeforeRunner(t *testing.T) {
	runner := &recordingPluginRunner{}
	service := NewService(Config{ServerName: "example.com", PluginRunner: runner})

	_, apiErr := service.Handle(context.Background(), "plugins.install", map[string]any{
		"plugin_id": "io.example.unknown",
	})
	if apiErr == nil || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("expected unknown plugin to be rejected with 400, got %#v", apiErr)
	}
	if len(runner.operations) != 0 {
		t.Fatalf("unknown plugin must not reach runner, got %#v", runner.operations)
	}
}

func decodeJSONMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode json body %q: %v", body, err)
	}
	return decoded
}
