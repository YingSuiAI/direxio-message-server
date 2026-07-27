package nativeagent

import (
	"context"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
)

func (r *Runtime) compressMemory(ctx context.Context, params map[string]any) (map[string]any, error) {
	key := nativeAgentConversationKey(params)
	if key == "" {
		return r.summarize(ctx, params)
	}
	memory, err := r.loadMemory(ctx, key)
	if err != nil {
		return nil, err
	}
	window := int(int64Param(params["memory_window"]))
	if window <= 0 {
		window = nativeAgentDefaultMemoryWindow
	}
	profile, resolveErr := r.resolveModelProfileForRequest(ctx, params)
	if hasModelProfile(params) || trimString(params["model_profile_id"]) != "" || trimString(params["client_model_profile_id"]) != "" {
		if resolveErr != nil {
			return nil, resolveErr
		}
		if err := validateModelProfile(profile); err != nil {
			return nil, err
		}
		memory, err = r.compactNativeAgentMemoryWithModel(ctx, memory, window, profile)
		if err != nil {
			return nil, err
		}
	} else {
		memory = compactNativeAgentMemory(memory, window)
	}
	if err := r.saveMemory(ctx, memory); err != nil {
		return nil, err
	}
	return map[string]any{
		"conversation_id": key,
		"summary":         memory.Summary,
		"messages":        memory.Messages,
		"updated_at":      memory.UpdatedAt,
		"compression":     compressionLabel(profile),
	}, nil
}

func (r *Runtime) compactNativeAgentMemoryWithModel(ctx context.Context, memory nativeAgentMemory, window int, profile nativeModelProfile) (nativeAgentMemory, error) {
	if window <= 0 {
		window = nativeAgentDefaultMemoryWindow
	}
	memory.Messages = compactEinoMessagesForMemory(memory.Messages)
	if len(memory.Messages) <= window {
		memory.UpdatedAt = time.Now().UTC().UnixMilli()
		return memory, nil
	}
	overflow := memory.Messages[:len(memory.Messages)-window]
	recent := append([]*schema.Message{}, memory.Messages[len(memory.Messages)-window:]...)
	summary, err := r.summarizeEinoMemory(ctx, profile, memory.Summary, overflow)
	if err != nil {
		return memory, err
	}
	memory.Summary = strings.TrimSpace(summary)
	memory.Messages = recent
	memory.UpdatedAt = time.Now().UTC().UnixMilli()
	return memory, nil
}

func compactNativeAgentMemory(memory nativeAgentMemory, window int) nativeAgentMemory {
	if window <= 0 {
		window = nativeAgentDefaultMemoryWindow
	}
	memory.Messages = compactEinoMessagesForMemory(memory.Messages)
	if len(memory.Messages) <= window {
		memory.UpdatedAt = time.Now().UTC().UnixMilli()
		return memory
	}
	overflow := memory.Messages[:len(memory.Messages)-window]
	memory.Messages = append([]*schema.Message{}, memory.Messages[len(memory.Messages)-window:]...)
	parts := make([]string, 0, 2)
	if strings.TrimSpace(memory.Summary) != "" {
		parts = append(parts, memory.Summary)
	}
	if overflowSummary := einoMessagesToSummary(overflow); strings.TrimSpace(overflowSummary) != "" {
		parts = append(parts, overflowSummary)
	}
	summary := strings.Join(parts, "\n")
	runes := []rune(summary)
	if len(runes) > 4000 {
		summary = string(runes[len(runes)-4000:])
	}
	memory.Summary = strings.TrimSpace(summary)
	memory.UpdatedAt = time.Now().UTC().UnixMilli()
	return memory
}

func (r *Runtime) summarizeEinoMemory(ctx context.Context, profile nativeModelProfile, previousSummary string, overflow []*schema.Message) (string, error) {
	chatModel, err := r.newEinoChatModel(ctx, profile)
	if err != nil {
		return "", err
	}
	prompt := "Existing summary:\n" + fallbackString(strings.TrimSpace(previousSummary), "(empty)") +
		"\n\nNew conversation messages to merge:\n" + einoMessagesToSummary(overflow)
	message, err := chatModel.Generate(ctx, []*schema.Message{
		schema.SystemMessage("You compress Dirextalk Agent conversation memory. Preserve user preferences, decisions, room/contact names, tool outcomes, and unresolved tasks. Return a concise Chinese summary only."),
		schema.UserMessage(prompt),
	})
	if err != nil {
		return "", err
	}
	return message.Content, nil
}

func memoryCompressionUsesModel(config map[string]any, params map[string]any) bool {
	mode := strings.ToLower(fallbackString(trimString(params["memory_compression"]), trimString(config["memory_compression"])))
	if mode == "" {
		mode = strings.ToLower(fallbackString(trimString(params["context_compression"]), trimString(config["context_compression"])))
	}
	return mode == "model" || mode == "llm" || mode == "eino_model" || boolParam(params["model_memory_compression"])
}

func compressionLabel(profile nativeModelProfile) string {
	if profile.APIKey != "" {
		return "eino_model"
	}
	return "text"
}
