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
	Title          string                    `json:"-"`
	Summary        string                    `json:"summary,omitempty"`
	Messages       []*schema.Message         `json:"messages,omitempty"`
	LastMessageSeq int64                     `json:"-"`
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
	if r.persistentMemoryReady && run.conversationID != "" && hasExplicitRequestMessages(params) {
		return run, validationErrorf("messages is not accepted when server conversation memory is enabled; send only the current prompt")
	}
	if _, explicit := selectedSkillIDs(config, params); explicit {
		if _, err := r.selectPlanningSkills(ctx, config, params); err != nil {
			return run, fmt.Errorf("invalid planning skill selection: %w", err)
		}
	}
	requestMessages := requestEinoMessages(params)
	systemPrompt := r.agentSystemPrompt(ctx, config, params, "")
	systemPrompt = appendPromptBlock(systemPrompt, profile.SystemPrompt)
	if run.memoryDisabled || run.conversationID == "" {
		run.inputMessages = requestMessages
		run.memoryMessages = memoryMessagesFromRequest(params, requestMessages)
		run.session = einoAgentSession{systemPrompt: systemPrompt, contextWindow: profile.ContextWindow, maxOutputTokens: profile.MaxOutputTokens}
		return run, nil
	}
	memory, err := r.loadMemory(ctx, run.conversationID)
	if err != nil {
		return run, err
	}
	compacted := false
	window := nativeAgentMemoryWindow(config, params)
	if contextWindow := nativeAgentMemoryContextWindow(memory, systemPrompt, requestMessages, profile); contextWindow > 0 && contextWindow < window {
		window = contextWindow
	}
	if len(memory.Messages) > window {
		if r.persistentMemoryReady && memoryCompressionUsesModel(config, params) && profile.APIKey != "" {
			if modelCompacted, compactErr := r.compactNativeAgentMemoryWithModel(ctx, memory, window, profile); compactErr == nil {
				memory = modelCompacted
			} else {
				memory = compactNativeAgentMemory(memory, window)
			}
		} else {
			memory = compactNativeAgentMemory(memory, window)
		}
		compacted = true
	}
	run.memory = memory
	if compacted {
		_ = r.saveMemory(ctx, memory)
	}
	if strings.TrimSpace(memory.Summary) != "" {
		systemPrompt = appendPromptBlock(systemPrompt, "Conversation memory summary:\n"+strings.TrimSpace(memory.Summary))
	}
	if !hasExplicitRequestMessages(params) {
		run.inputMessages = append(run.inputMessages, cloneEinoMessages(memory.Messages)...)
	}
	run.inputMessages = append(run.inputMessages, requestMessages...)
	run.memoryMessages = memoryMessagesFromRequest(params, requestMessages)
	run.session = einoAgentSession{systemPrompt: systemPrompt, contextWindow: profile.ContextWindow, maxOutputTokens: profile.MaxOutputTokens}
	return run, nil
}

func nativeAgentMemoryContextWindow(memory nativeAgentMemory, systemPrompt string, requestMessages []*schema.Message, profile nativeModelProfile) int {
	if profile.ContextWindow <= 0 || len(memory.Messages) <= 1 {
		return len(memory.Messages)
	}
	remaining := nativeAgentInputTokenBudget(profile.ContextWindow, profile.MaxOutputTokens)
	remaining -= estimateNativeAgentTextTokens(systemPrompt)
	remaining -= estimateNativeAgentTextTokens(memory.Summary)
	remaining -= estimateEinoMessagesTokens(requestMessages)
	if remaining <= 0 {
		return 1
	}
	used, keep := 0, 0
	for i := len(memory.Messages) - 1; i >= 0; i-- {
		cost := estimateEinoMessageTokens(memory.Messages[i])
		if keep > 0 && used+cost > remaining {
			break
		}
		used += cost
		keep++
	}
	if keep <= 0 {
		return 1
	}
	return keep
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
	r.maybeSetAutomaticConversationTitle(ctx, profile, run, produced)
	memory.Messages = append(memory.Messages, newMessages...)
	window := nativeAgentMemoryWindow(config, params)
	if r.persistentMemoryReady && memoryCompressionUsesModel(config, params) && profile.APIKey != "" {
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

func nativeAgentMemoryWindow(config, params map[string]any) int {
	window := int(int64Param(params["memory_window"]))
	if window <= 0 {
		window = int(int64Param(config["memory_window"]))
	}
	if window <= 0 {
		window = nativeAgentDefaultMemoryWindow
	}
	return window
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
