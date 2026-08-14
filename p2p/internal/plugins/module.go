// Package plugins owns the non-Agent plugin ProductCore actions, persistence
// orchestration, invocation validation, and Docker runner boundary.
package plugins

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkplugin"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

const (
	LegacyAgentPluginID = "io.dirextalk.agent"
	OpsPluginID         = "io.dirextalk.ops"

	StatusInstalled = "installed"
	StatusEnabled   = "enabled"
	StatusDisabled  = "disabled"
	StatusRemoved   = "uninstalled"

	jobStatusSucceeded = "succeeded"
	jobStatusFailed    = "failed"

	actionCatalogList   = "plugins.catalog.list"
	actionInstalledList = "plugins.installed.list"
	actionInstall       = "plugins.install"
	actionEnable        = "plugins.enable"
	actionDisable       = "plugins.disable"
	actionUninstall     = "plugins.uninstall"
	actionConfigGet     = "plugins.config.get"
	actionConfigUpdate  = "plugins.config.update"
	actionJobGet        = "plugins.job.get"
	actionHealth        = "plugins.health"
	actionLogsTail      = "plugins.logs.tail"
	actionInvoke        = "plugins.invoke"
)

// Store is the durable plugin repository used by Module.
type Store interface {
	UpsertPlugin(context.Context, dirextalkplugin.Instance) error
	ListPlugins(context.Context) ([]dirextalkplugin.Instance, error)
	GetPlugin(context.Context, string) (dirextalkplugin.Instance, bool, error)
	UpsertPluginJob(context.Context, dirextalkplugin.Job) error
	GetPluginJob(context.Context, string) (dirextalkplugin.Job, bool, error)
	UpsertPluginSecret(context.Context, dirextalkplugin.Secret) error
	GetPluginSecret(context.Context, string, string) (dirextalkplugin.Secret, bool, error)
}

// Config contains stable service inputs while plugin state remains Store-owned.
type Config struct {
	Homeserver string
	Now        func() time.Time
	NewJobID   func() string
}

type Module struct {
	store  Store
	runner Runner
	config Config
}

func New(store Store, runner Runner, cfg Config) *Module {
	if runner == nil {
		runner = NoopRunner{}
	}
	return &Module{store: store, runner: runner, config: cfg}
}

// Handlers returns the exact ProductCore action surface owned by the module.
func (m *Module) Handlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{
		actionCatalogList:   m.catalogList,
		actionInstalledList: m.installedList,
		actionInstall:       m.install,
		actionEnable:        m.enable,
		actionDisable:       m.disable,
		actionUninstall:     m.uninstall,
		actionConfigGet:     m.configGet,
		actionConfigUpdate:  m.configUpdate,
		actionJobGet:        m.jobGet,
		actionHealth:        m.health,
		actionLogsTail:      m.logsTail,
		actionInvoke:        m.invoke,
	}
}

// CheckStore preserves the persisted-service constructor's startup read check
// without rebuilding an in-memory shadow of durable plugin state.
func (m *Module) CheckStore(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}
	_, err := m.store.ListPlugins(ctx)
	return err
}

func (m *Module) catalogList(context.Context, map[string]any) (any, *actionbase.Error) {
	entries := availableCatalog(m.runner)
	return map[string]any{
		"plugins":        entries,
		"enabled":        len(entries) > 0,
		"docker_enabled": RunnerEnabled(m.runner),
	}, nil
}

func (m *Module) installedList(ctx context.Context, _ map[string]any) (any, *actionbase.Error) {
	plugins, err := m.listInstances(ctx)
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	return map[string]any{"plugins": plugins}, nil
}

func (m *Module) jobGet(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	jobID := actionbase.String(params["job_id"])
	if jobID == "" {
		return nil, actionbase.BadRequest("job_id is required")
	}
	job, ok, err := m.getJob(ctx, jobID)
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	if !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "plugin job not found")
	}
	return job, nil
}

func (m *Module) health(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	plugin, apiErr := m.requirePlugin(ctx, params)
	if apiErr != nil {
		return nil, apiErr
	}
	return map[string]any{
		"plugin_id": plugin.ID,
		"status":    plugin.Status,
		"enabled":   plugin.Enabled,
		"ok":        plugin.Enabled && plugin.Status == StatusEnabled,
	}, nil
}

func (m *Module) logsTail(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	plugin, apiErr := m.requirePlugin(ctx, params)
	if apiErr != nil {
		return nil, apiErr
	}
	return map[string]any{
		"plugin_id": plugin.ID,
		"logs":      []string{},
		"message":   "plugin log tail is not available from the first-version runner",
	}, nil
}

func (m *Module) invoke(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	req, clientAction, apiErr := m.prepareInvoke(ctx, params, false)
	if apiErr != nil {
		return nil, apiErr
	}
	result, err := m.runner.InvokePlugin(ctx, req)
	if err != nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, err.Error())
	}
	return map[string]any{
		"plugin_id": req.PluginID,
		"action":    clientAction,
		"result":    result,
	}, nil
}

// PreparedStream is validated synchronously before the WS adapter starts a
// goroutine. The runner request stays opaque so callers cannot bypass module
// validation while retaining the existing frame metadata.
type PreparedStream struct {
	PluginID string
	Action   string
	request  InvokeRequest
}

func (m *Module) PrepareStream(ctx context.Context, params map[string]any) (PreparedStream, *actionbase.Error) {
	req, clientAction, apiErr := m.prepareInvoke(ctx, params, true)
	if apiErr != nil {
		return PreparedStream{}, apiErr
	}
	return PreparedStream{PluginID: req.PluginID, Action: clientAction, request: req}, nil
}

func (m *Module) RunStream(ctx context.Context, prepared PreparedStream, emit func(StreamEvent) error) error {
	return m.runner.StreamPlugin(ctx, prepared.request, emit)
}

func (m *Module) prepareInvoke(ctx context.Context, params map[string]any, stream bool) (InvokeRequest, string, *actionbase.Error) {
	pluginID := actionbase.String(params["plugin_id"])
	if pluginID == "" {
		return InvokeRequest{}, "", actionbase.BadRequest("plugin_id is required")
	}
	entry, ok := FindOfficialPlugin(pluginID)
	if !ok {
		return InvokeRequest{}, "", actionbase.BadRequest("unknown official plugin")
	}
	plugin, exists, err := m.get(ctx, pluginID)
	if err != nil {
		return InvokeRequest{}, "", actionbase.InternalError(err)
	}
	if !exists {
		return InvokeRequest{}, "", actionbase.StatusError(http.StatusNotFound, "plugin is not installed")
	}
	if !plugin.Enabled || plugin.Status != StatusEnabled {
		return InvokeRequest{}, "", actionbase.StatusError(http.StatusConflict, "plugin is not enabled")
	}
	clientAction := actionbase.String(params["action"])
	if clientAction == "" {
		return InvokeRequest{}, "", actionbase.BadRequest("action is required")
	}
	runnerAction := clientAction
	if stream && !strings.HasSuffix(clientAction, ".stream") {
		runnerAction += ".stream"
	}
	if !ActionAllowed(entry, runnerAction) {
		return InvokeRequest{}, "", actionbase.BadRequest("plugin action is not allowed")
	}
	if !stream && strings.HasSuffix(runnerAction, ".stream") {
		return InvokeRequest{}, "", actionbase.BadRequest("stream plugin actions are not available through Product HTTP")
	}
	invokeParams := map[string]any{}
	if rawParams, ok := params["params"].(map[string]any); ok {
		invokeParams = cloneMap(rawParams)
	} else if params["params"] != nil {
		return InvokeRequest{}, "", actionbase.BadRequest("params must be an object")
	}
	return InvokeRequest{
		PluginID:      plugin.ID,
		ContainerName: ContainerName(plugin.ID),
		Action:        runnerAction,
		Params:        invokeParams,
	}, strings.TrimSuffix(clientAction, ".stream"), nil
}

func officialCatalog() []dirextalkplugin.CatalogEntry {
	return []dirextalkplugin.CatalogEntry{{
		ID:             OpsPluginID,
		Name:           "Dirextalk Ops",
		Version:        "0.1.0",
		Description:    "Official operations plugin for server status, backups, migration exports, and safe cleanup planning.",
		Image:          "docker.io/dirextalk/ops-plugin:latest",
		Digest:         "",
		MinBaseVersion: "0.1.0",
		Permissions: []string{
			"ops.status.read",
			"ops.backup.write",
			"ops.cleanup.plan",
			"ops.cleanup.run",
			"ops.migration.export",
		},
		Actions: []string{
			"ops.status.get",
			"ops.containers.list",
			"ops.logs.tail",
			"ops.backups.list",
			"ops.backup.create",
			"ops.backup.status",
			"ops.backup.download_chunk",
			"ops.backup.delete",
			"ops.cleanup.plan",
			"ops.cleanup.run",
			"ops.rooms.cleanup.plan",
			"ops.rooms.cleanup.run",
			"ops.media.orphans.plan",
			"ops.migration.export",
			"ops.restore.plan",
			"ops.restore.run",
		},
		ConfigSchema: map[string]any{
			"backup_root":          "string",
			"max_backups":          "number",
			"cleanup_requires_dry": true,
			"destructive_confirm":  "string",
		},
	}}
}

func availableCatalog(runner Runner) []dirextalkplugin.CatalogEntry {
	entries := officialCatalog()
	if !RunnerEnabled(runner) {
		return []dirextalkplugin.CatalogEntry{}
	}
	return entries
}

func FindOfficialPlugin(pluginID string) (dirextalkplugin.CatalogEntry, bool) {
	pluginID = strings.TrimSpace(pluginID)
	for _, entry := range officialCatalog() {
		if entry.ID == pluginID {
			return entry, true
		}
	}
	return dirextalkplugin.CatalogEntry{}, false
}

func ActionAllowed(entry dirextalkplugin.CatalogEntry, action string) bool {
	action = strings.TrimSpace(action)
	for _, allowed := range entry.Actions {
		if strings.TrimSpace(allowed) == action {
			return true
		}
	}
	return false
}

func (m *Module) listInstances(ctx context.Context) ([]dirextalkplugin.Instance, error) {
	if m == nil || m.store == nil {
		return []dirextalkplugin.Instance{}, nil
	}
	plugins, err := m.store.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]dirextalkplugin.Instance, 0, len(plugins))
	for i := range plugins {
		plugin := NormalizeInstance(plugins[i])
		if plugin.ID == LegacyAgentPluginID {
			continue
		}
		filtered = append(filtered, plugin)
	}
	return filtered, nil
}

func (m *Module) get(ctx context.Context, pluginID string) (dirextalkplugin.Instance, bool, error) {
	if m == nil || m.store == nil {
		return dirextalkplugin.Instance{}, false, nil
	}
	plugin, ok, err := m.store.GetPlugin(ctx, pluginID)
	if err != nil || !ok {
		return plugin, ok, err
	}
	return NormalizeInstance(plugin), true, nil
}

func (m *Module) save(ctx context.Context, plugin dirextalkplugin.Instance) error {
	if m == nil || m.store == nil {
		return errors.New("plugin store is not configured")
	}
	return m.store.UpsertPlugin(ctx, NormalizeInstance(plugin))
}

func (m *Module) saveJob(ctx context.Context, job dirextalkplugin.Job) error {
	if m == nil || m.store == nil {
		return errors.New("plugin store is not configured")
	}
	return m.store.UpsertPluginJob(ctx, job)
}

func (m *Module) getJob(ctx context.Context, jobID string) (dirextalkplugin.Job, bool, error) {
	if m == nil || m.store == nil {
		return dirextalkplugin.Job{}, false, nil
	}
	return m.store.GetPluginJob(ctx, jobID)
}
