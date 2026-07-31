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

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentskills"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
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
	DataDir               string
	Store                 ConfigStore
	Tools                 []Tool
	HTTPClient            *http.Client
	ModelProfiles         ModelProfileResolver
	OwnerID               func() string
	Memory                ConversationMemoryStore
	Knowledge             KnowledgeStore
	KnowledgeSources      agentmemory.KnowledgeSourceStore
	Conversations         ConversationStore
	PersistentMemoryReady bool
	EmbeddingSession      agentmemory.KnowledgeEmbeddingSessionFunc
	PlanningSkills        *agentskills.Registry
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
	ModelKind                  string
	InputModalities            []string
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
	store                 ConfigStore
	dataDir               string
	client                *http.Client
	tools                 []Tool
	modelProfiles         ModelProfileResolver
	ownerID               func() string
	memory                ConversationMemoryStore
	knowledge             KnowledgeStore
	sources               agentmemory.KnowledgeSourceStore
	persistentMemoryReady bool
	embedding             agentmemory.KnowledgeEmbeddingSessionFunc
	conversations         ConversationStore
	planningSkills        *agentskills.Registry
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
	memory := config.Memory
	if memory == nil {
		// Volatile storage is reserved for direct unit tests. Production wiring
		// always supplies OwnerID and must provide a durable store explicitly.
		if config.OwnerID == nil {
			memory = NewInMemoryConversationMemoryStore()
		}
	}
	knowledge := config.Knowledge
	if knowledge == nil {
		if candidate, ok := memory.(KnowledgeStore); ok {
			knowledge = candidate
		}
	}
	sources := config.KnowledgeSources
	if sources == nil {
		if candidate, ok := memory.(agentmemory.KnowledgeSourceStore); ok {
			sources = candidate
		}
	}
	conversations := config.Conversations
	if conversations == nil {
		if candidate, ok := memory.(ConversationStore); ok {
			conversations = candidate
		}
	}
	planningSkills := config.PlanningSkills
	if planningSkills == nil {
		planningSkills, _ = agentskills.Builtin()
	}
	return &Runtime{
		store:                 config.Store,
		dataDir:               filepath.Clean(dataDir),
		client:                client,
		tools:                 append([]Tool{}, config.Tools...),
		modelProfiles:         config.ModelProfiles,
		ownerID:               config.OwnerID,
		memory:                memory,
		knowledge:             knowledge,
		sources:               sources,
		persistentMemoryReady: config.PersistentMemoryReady,
		embedding:             config.EmbeddingSession,
		conversations:         conversations,
		planningSkills:        planningSkills,
	}
}

type requestContextKey string

const (
	requestOwnerKey        requestContextKey = "nativeagent.owner"
	requestConversationKey requestContextKey = "nativeagent.conversation"
	requestIntentKey       requestContextKey = "nativeagent.intent"
	requestUserTextKey     requestContextKey = "nativeagent.current_user_text"
)

// RequestContext exposes server-injected, non-model-authored scope to built-in
// tools. Callers cannot supply these values as tool arguments.
func RequestContext(ctx context.Context) (owner, conversation, userText string) {
	owner, _ = ctx.Value(requestOwnerKey).(string)
	conversation, _ = ctx.Value(requestConversationKey).(string)
	userText, _ = ctx.Value(requestUserTextKey).(string)
	return
}

// WithRequestContext is used by server-owned adapters and focused tests; model
// tool arguments are never merged into this context.
func WithRequestContext(ctx context.Context, owner, conversation, userText string) context.Context {
	return WithRequestContextIntent(ctx, owner, conversation, "", userText)
}

// WithRequestContextIntent extends the server-owned scope with the stable
// turn/invocation intent used for write idempotency. It is never read from
// model-authored tool arguments.
func WithRequestContextIntent(ctx context.Context, owner, conversation, intent, userText string) context.Context {
	ctx = context.WithValue(ctx, requestOwnerKey, strings.TrimSpace(owner))
	ctx = context.WithValue(ctx, requestConversationKey, strings.TrimSpace(conversation))
	ctx = context.WithValue(ctx, requestIntentKey, strings.TrimSpace(intent))
	return context.WithValue(ctx, requestUserTextKey, userText)
}

func RequestIntent(ctx context.Context) string {
	value, _ := ctx.Value(requestIntentKey).(string)
	return strings.TrimSpace(value)
}

// RequestIssuedAt exposes the server-persisted durable turn creation time to
// built-in tools. A missing value means this is not a durable turn and any
// operation requiring replay-stable issuance must fail closed.
func RequestIssuedAt(ctx context.Context) (time.Time, bool) {
	return agentturns.IssuedAt(ctx)
}
func (r *Runtime) withRequestContext(ctx context.Context, params map[string]any) context.Context {
	owner := ""
	if r.ownerID != nil {
		owner = strings.TrimSpace(r.ownerID())
	}
	conversation := ConversationID(params)
	intent := trimString(params["turn_id"])
	userText := trimString(params["prompt"])
	if userText == "" {
		userText = trimString(params["message"])
	}
	ctx = context.WithValue(ctx, requestOwnerKey, owner)
	ctx = context.WithValue(ctx, requestConversationKey, conversation)
	ctx = context.WithValue(ctx, requestIntentKey, intent)
	return context.WithValue(ctx, requestUserTextKey, userText)
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
	ctx = r.withRequestContext(ctx, params)
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
	case "agent.knowledge.config.get", "agent.knowledge.config.update":
		return map[string]any{"supported": false, "status": "unsupported"}, nil
	case "agent.knowledge.sources.list":
		return r.listKnowledgeSources(ctx, params)
	case "agent.knowledge.sources.delete":
		return r.deleteKnowledgeSource(ctx, params)
	case "agent.knowledge.upload.start":
		return r.startKnowledgeUpload(ctx, params)
	case "agent.knowledge.upload.chunk":
		return r.chunkKnowledgeUpload(ctx, params)
	case "agent.knowledge.upload.finish":
		return r.finishKnowledgeUpload(ctx, params)
	case "agent.knowledge.memory.create":
		return r.createKnowledgeMemory(ctx, params)
	case "agent.knowledge.memories.list":
		return r.listKnowledgeMemories(ctx, params)
	case "agent.knowledge.memories.update":
		return r.updateKnowledgeMemory(ctx, params)
	case "agent.knowledge.memories.delete":
		return r.deleteKnowledgeMemory(ctx, params)
	case "agent.knowledge.search":
		return r.searchKnowledgeMemory(ctx, params)
	case "agent.knowledge.status":
		return r.knowledgeStatus(ctx)
	case "agent.chat.conversations.create":
		return r.createConversation(ctx, params)
	case "agent.chat.conversations.list":
		return r.listConversations(ctx, params)
	case "agent.chat.conversations.get":
		return r.getConversation(ctx, params)
	case "agent.chat.conversations.rename":
		return r.renameConversation(ctx, params)
	case "agent.chat.conversations.delete":
		return r.deleteConversation(ctx, params)
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
	ctx = r.withRequestContext(ctx, params)
	attachments, validationErr := ValidateNativeAgentChatParams(params)
	if validationErr != nil {
		return emitNativeAgentStreamFailure(emit, validationErr)
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
	if validationErr := nativeAgentImageProfileAllowed(params, profile, len(attachments) > 0); validationErr != nil {
		return emitNativeAgentStreamFailure(emit, validationErr)
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
		"capabilities": func() []string {
			if r.persistentMemoryReady {
				return []string{"memory.server"}
			}
			return []string{}
		}(),
		"time": time.Now().UTC().Format(time.RFC3339),
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
