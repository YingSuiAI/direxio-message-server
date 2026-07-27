package nativeagent

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/schema"
)

const nativeAgentDefaultMemoryWindow = 12

type nativeAgentMemory struct {
	ConversationID string                    `json:"conversation_id"`
	Summary        string                    `json:"summary,omitempty"`
	Messages       []*schema.Message         `json:"messages,omitempty"`
	UpdatedAt      int64                     `json:"updated_at"`
	Metadata       map[string]map[string]any `json:"metadata,omitempty"`
}

type nativeAgentRunContext struct {
	conversationID string
	memory         nativeAgentMemory
	inputMessages  []*schema.Message
	memoryMessages []*schema.Message
	session        einoAgentSession
	maxSteps       int
	memoryDisabled bool
}

func (r *Runtime) prepareEinoRun(ctx context.Context, config map[string]any, params map[string]any, profile nativeModelProfile) (nativeAgentRunContext, error) {
	run := nativeAgentRunContext{
		conversationID: nativeAgentConversationKey(params),
		memoryDisabled: boolParam(params["memory_disabled"]) || boolParam(config["memory_disabled"]),
		maxSteps:       nativeAgentMaxSteps(config, params),
	}
	requestMessages := requestEinoMessages(params)
	systemPrompt := r.agentSystemPrompt(ctx, config, params, "")
	systemPrompt = appendPromptBlock(systemPrompt, profile.SystemPrompt)
	if run.memoryDisabled || run.conversationID == "" {
		run.inputMessages = requestMessages
		run.memoryMessages = memoryMessagesFromRequest(params, requestMessages)
		run.session = einoAgentSession{systemPrompt: systemPrompt, contextWindow: profile.ContextWindow}
		return run, nil
	}
	memory, err := r.loadMemory(ctx, run.conversationID)
	if err != nil {
		return run, err
	}
	run.memory = memory
	if strings.TrimSpace(memory.Summary) != "" {
		systemPrompt = appendPromptBlock(systemPrompt, "Conversation memory summary:\n"+strings.TrimSpace(memory.Summary))
	}
	if !hasExplicitRequestMessages(params) {
		run.inputMessages = append(run.inputMessages, cloneEinoMessages(memory.Messages)...)
	}
	run.inputMessages = append(run.inputMessages, requestMessages...)
	run.memoryMessages = memoryMessagesFromRequest(params, requestMessages)
	run.session = einoAgentSession{systemPrompt: systemPrompt, contextWindow: profile.ContextWindow}
	return run, nil
}

func (r *Runtime) rememberEinoMessages(ctx context.Context, config map[string]any, params map[string]any, profile nativeModelProfile, run nativeAgentRunContext, produced []*schema.Message) error {
	if run.memoryDisabled || run.conversationID == "" {
		return nil
	}
	memory := run.memory
	if memory.ConversationID == "" {
		memory.ConversationID = run.conversationID
	}
	memory.Messages = append(memory.Messages, compactEinoMessagesForMemory(run.memoryMessages)...)
	memory.Messages = append(memory.Messages, compactEinoMessagesForMemory(produced)...)
	window := int(int64Param(params["memory_window"]))
	if window <= 0 {
		window = int(int64Param(config["memory_window"]))
	}
	if window <= 0 {
		window = nativeAgentDefaultMemoryWindow
	}
	if memoryCompressionUsesModel(config, params) && profile.APIKey != "" {
		if compacted, err := r.compactNativeAgentMemoryWithModel(ctx, memory, window, profile); err == nil {
			memory = compacted
		} else {
			memory = compactNativeAgentMemory(memory, window)
		}
	} else {
		memory = compactNativeAgentMemory(memory, window)
	}
	return r.saveMemory(ctx, memory)
}

func memoryMessagesFromRequest(params map[string]any, requestMessages []*schema.Message) []*schema.Message {
	prompt := fallbackString(trimString(params["prompt"]), trimString(params["message"]))
	if prompt != "" {
		return []*schema.Message{schema.UserMessage(prompt)}
	}
	if !hasExplicitRequestMessages(params) {
		return cloneEinoMessages(requestMessages)
	}
	return nil
}

func nativeAgentConversationKey(params map[string]any) string {
	for _, key := range []string{"conversation_id", "thread_id", "room_id", "memory_key"} {
		if value := sanitizeNativeID(trimString(params[key])); value != "" {
			return value
		}
	}
	return "default"
}

// ConversationID returns the canonical runtime memory key for a chat request.
// Durable turn serialization must use this same value so aliases cannot write
// one memory context concurrently through different request spellings.
func ConversationID(params map[string]any) string {
	return nativeAgentConversationKey(params)
}
