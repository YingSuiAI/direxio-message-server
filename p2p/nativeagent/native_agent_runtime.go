package nativeagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const embeddedExtensionsForbiddenMessage = "embedded native agent extensions are forbidden"

var errEmbeddedExtensionsForbidden = errors.New(embeddedExtensionsForbiddenMessage)

func embeddedExtensionsForbidden() error { return errEmbeddedExtensionsForbidden }

// IsEmbeddedExtensionsForbidden identifies the stable compatibility error for
// runtime extension actions that embedded Native Agent no longer supports.
func IsEmbeddedExtensionsForbidden(err error) bool {
	return errors.Is(err, errEmbeddedExtensionsForbidden)
}

const (
	defaultNativeAgentDataDir = "/var/dirextalk-message-server/agent"
	nativeAgentHTTPTimeout    = 90 * time.Second
	nativeAgentToolCallLimit  = 48
)

type Config struct {
	DataDir       string
	Store         ConfigStore
	Tools         []Tool
	HTTPClient    *http.Client
	ModelProfiles ModelProfileResolver
}

// ServerModelProfile is a redacted profile projection with the key retained
// only in memory for the outbound provider request.
type ServerModelProfile struct {
	ProfileID, ClientProfileID string
	DisplayName, Provider      string
	BaseURL, Model             string
	SystemPrompt               string
	APIKey                     string
	APIKeyConfigured           bool
	Temperature, TopP          *float64
	MaxOutputTokens            int
	ContextWindow              int
	ReasoningEffort            string
	Revision                   int64
	CredentialVersion          int64
}

type ModelProfileResolver interface {
	ResolveModelProfile(context.Context, string) (ServerModelProfile, error)
}

type ConfigStore interface {
	Load(ctx context.Context) (map[string]any, bool, error)
	Save(ctx context.Context, config map[string]any) error
}

type Event struct {
	Event string
	Data  map[string]any
}

type Runtime struct {
	store         ConfigStore
	dataDir       string
	client        *http.Client
	tools         []Tool
	modelProfiles ModelProfileResolver
}

func New(config Config) *Runtime {
	dataDir := strings.TrimSpace(config.DataDir)
	if dataDir == "" {
		dataDir = strings.TrimSpace(os.Getenv("P2P_NATIVE_AGENT_DATA_DIR"))
	}
	if dataDir == "" {
		dataDir = defaultNativeAgentDataDir
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: nativeAgentHTTPTimeout}
	}
	return &Runtime{
		store:         config.Store,
		dataDir:       filepath.Clean(dataDir),
		client:        client,
		tools:         append([]Tool{}, config.Tools...),
		modelProfiles: config.ModelProfiles,
	}
}

func (r *Runtime) Apply(ctx context.Context, action string) error {
	switch strings.TrimSpace(action) {
	case "install", "enable":
		return r.ensureDataDirs()
	case "disable", "uninstall":
		return nil
	default:
		return fmt.Errorf("native agent action %q is not supported", action)
	}
}

func (r *Runtime) Invoke(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	action = strings.TrimSpace(action)
	switch action {
	case "agent.chat":
		return r.chat(ctx, params)
	case "agent.models.list":
		return r.modelsList(ctx, params)
	case "agent.runtime.inspect":
		return r.runtimeInspect(ctx)
	case "agent.runtime.install":
		return nil, embeddedExtensionsForbidden()
	case "agent.runtime.which":
		return nil, embeddedExtensionsForbidden()
	case "agent.runtime.run":
		return nil, embeddedExtensionsForbidden()
	case "agent.skills.list":
		return r.skillsList(ctx)
	case "agent.skills.install":
		return nil, embeddedExtensionsForbidden()
	case "agent.skills.enable":
		return nil, embeddedExtensionsForbidden()
	case "agent.skills.disable":
		return nil, embeddedExtensionsForbidden()
	case "agent.skills.uninstall":
		return nil, embeddedExtensionsForbidden()
	case "agent.skills.registry.search":
		return nil, embeddedExtensionsForbidden()
	case "agent.mcp.servers.list":
		return r.mcpServersList(ctx)
	case "agent.mcp.servers.install":
		return nil, embeddedExtensionsForbidden()
	case "agent.mcp.servers.enable":
		return nil, embeddedExtensionsForbidden()
	case "agent.mcp.servers.disable":
		return nil, embeddedExtensionsForbidden()
	case "agent.mcp.servers.uninstall":
		return nil, embeddedExtensionsForbidden()
	case "agent.mcp.registry.search":
		return nil, embeddedExtensionsForbidden()
	case "agent.knowledge.config.get", "agent.knowledge.config.update", "agent.knowledge.sources.list",
		"agent.knowledge.sources.delete", "agent.knowledge.upload.start", "agent.knowledge.upload.chunk",
		"agent.knowledge.upload.finish", "agent.knowledge.memory.create", "agent.knowledge.search",
		"agent.knowledge.status":
		return map[string]any{"supported": false, "status": "unsupported"}, nil
	case "agent.context.compress":
		return r.compressMemory(ctx, params)
	case "agent.config.propose_patch":
		return map[string]any{"ok": true, "patch": map[string]any{}}, nil
	default:
		if strings.HasPrefix(action, "agent.") {
			return r.invokeDirectTool(ctx, action, params)
		}
		return nil, fmt.Errorf("native agent action %q is not implemented", action)
	}
}

func (r *Runtime) Stream(ctx context.Context, action string, params map[string]any, emit func(Event) error) error {
	if strings.TrimSpace(action) != "agent.chat.stream" {
		return fmt.Errorf("native agent stream action %q is not implemented", action)
	}
	config, _, err := r.agentConfig(ctx)
	if err != nil {
		return emitNativeAgentStreamFailure(emit, err)
	}
	profile, resolveErr := r.resolveModelProfileForRequest(ctx, params)
	if resolveErr != nil {
		if emitErr := emit(Event{Event: "error", Data: map[string]any{"error": resolveErr.Error()}}); emitErr != nil {
			return emitErr
		}
		return emit(Event{Event: "done", Data: map[string]any{"ok": false, "native": true, "framework": "eino", "model_ready": false}})
	}
	if err := validateModelProfile(profile); err != nil {
		if emitErr := emit(Event{Event: "error", Data: map[string]any{"error": err.Error()}}); emitErr != nil {
			return emitErr
		}
		return emit(Event{Event: "done", Data: map[string]any{
			"ok":          false,
			"native":      true,
			"framework":   "eino",
			"model_ready": false,
		}})
	}
	run, err := r.prepareEinoRun(ctx, config, params, profile)
	if err != nil {
		return emitNativeAgentStreamFailure(emit, err, profile.APIKey)
	}
	tools, cleanup, err := r.enabledEinoTools(ctx, config, params)
	if err != nil {
		return emitNativeAgentStreamFailure(emit, err, profile.APIKey)
	}
	defer cleanup()
	text, reasoning, toolCalls, produced, err := r.streamEinoAgent(ctx, profile, run.inputMessages, run.session, tools, emit, run.maxSteps)
	if err != nil {
		return emitNativeAgentStreamFailure(emit, err, profile.APIKey)
	}
	if err := r.rememberEinoMessages(ctx, config, params, profile, run, produced); err != nil {
		return emitNativeAgentStreamFailure(emit, err, profile.APIKey)
	}
	trace := buildAgentTrace(run, produced, toolCalls, text)
	if err := emit(Event{Event: "trace", Data: trace}); err != nil {
		return err
	}
	done := map[string]any{
		"ok":         true,
		"native":     true,
		"framework":  "eino",
		"provider":   profile.Provider,
		"model":      profile.Model,
		"text":       text,
		"tool_calls": toolCalls,
		"steps":      trace["steps"],
		"trace":      trace,
	}
	if references := nativeAgentReferences(produced); len(references) > 0 {
		done["references"] = references
	}
	if reasoning != "" {
		done["reasoning_content"] = reasoning
	}
	return emit(Event{Event: "done", Data: done})
}

func emitNativeAgentStreamFailure(emit func(Event) error, err error, secrets ...string) error {
	message := strings.TrimSpace(err.Error())
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if message == "" {
		message = "native agent turn failed"
	}
	return emit(Event{Event: "error", Data: map[string]any{"error": message}})
}

func (r *Runtime) ensureDataDirs() error {
	for _, dir := range []string{
		r.dataDir,
		filepath.Join(r.dataDir, "skills"),
		filepath.Join(r.dataDir, "mcp"),
		filepath.Join(r.dataDir, "runtime"),
		filepath.Join(r.dataDir, "runtime", "bin"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) runtimeInspect(ctx context.Context) (map[string]any, error) {
	_, exists, err := r.agentConfig(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"ok":            true,
		"native":        true,
		"framework":     "eino",
		"configured":    exists,
		"data_dir":      r.dataDir,
		"skills":        []map[string]any{},
		"mcp_servers":   []map[string]any{},
		"runtime_tools": []map[string]any{},
		"time":          time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func (r *Runtime) agentConfig(ctx context.Context) (map[string]any, bool, error) {
	if r.store == nil {
		return map[string]any{}, false, nil
	}
	config, exists, err := r.store.Load(ctx)
	return cloneAnyMap(config), exists, err
}

func (r *Runtime) updateAgentConfig(ctx context.Context, mutate func(map[string]any)) error {
	if r.store == nil {
		return fmt.Errorf("config store is unavailable")
	}
	config, _, err := r.agentConfig(ctx)
	if err != nil {
		return err
	}
	mutate(config)
	return r.store.Save(ctx, sanitizeConfig(config))
}
