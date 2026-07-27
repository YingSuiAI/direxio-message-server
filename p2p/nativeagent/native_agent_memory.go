package nativeagent

import (
	"context"
	"fmt"
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
	if r.memory == nil {
		return fmt.Errorf("conversation memory store is not configured")
	}
	memory := run.memory
	if memory.ConversationID == "" {
		memory.ConversationID = run.conversationID
	}
	owner := r.effectiveOwner(ctx)
	if owner == "" {
		return fmt.Errorf("owner context is required")
	}
	turnID := trimString(params["turn_id"])
	newMessages := append(compactEinoMessagesForMemory(run.memoryMessages), compactEinoMessagesForMemory(produced)...)
	stored := make([]StoredMessage, 0, len(newMessages))
	roleIndex := map[string]int{}
	references := nativeAgentReferences(produced)
	for _, msg := range newMessages {
		if msg == nil {
			continue
		}
		item := StoredMessage{TurnID: turnID, Role: string(msg.Role), Content: msg.Content, Message: msg}
		if msg.Role == schema.Assistant && len(references) > 0 {
			item.References = references
		}
		if turnID != "" && (msg.Role == schema.User || msg.Role == schema.Assistant) {
			role := string(msg.Role)
			if idx, ok := roleIndex[role]; ok {
				stored[idx] = item
				continue
			}
			roleIndex[role] = len(stored)
		}
		stored = append(stored, item)
	}
	if len(stored) > 0 {
		if err := r.memory.AppendConversationMessages(ctx, owner, run.conversationID, turnID, stored); err != nil {
			return err
		}
	}
	memory.Messages = append(memory.Messages, newMessages...)
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
	// Message append is the durable turn boundary. Summary compaction is
	// best-effort: a failed summary update must not turn a successful model turn
	// into a terminal error or discard its messages.
	if err := r.saveMemory(ctx, memory); err != nil {
		return nil
	}
	return nil
}

func memoryMessagesFromRequest(params map[string]any, requestMessages []*schema.Message) []*schema.Message {
	prompt := fallbackString(trimString(params["prompt"]), trimString(params["message"]))
	attachments, _ := parseNativeAgentAttachments(params)
	if prompt != "" || len(attachments) > 0 {
		content := prompt
		if len(attachments) > 0 {
			marker := fmt.Sprintf("[attached %d image(s)]", len(attachments))
			if content == "" {
				content = marker
			} else {
				content += "\n" + marker
			}
		}
		return []*schema.Message{schema.UserMessage(content)}
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
