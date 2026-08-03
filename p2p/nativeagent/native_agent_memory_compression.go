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
	profile := nativeModelProfile{}
	if r.modelProfiles != nil || hasModelProfile(params) {
		var resolveErr error
		profile, resolveErr = r.resolveModelProfileForRequest(ctx, params)
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
	memory.Summary = boundedNativeAgentSummary(summary)
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
	memory.Summary = boundedNativeAgentSummary(summary)
	memory.UpdatedAt = time.Now().UTC().UnixMilli()
	return memory
}

func boundedNativeAgentSummary(summary string) string {
	runes := []rune(strings.TrimSpace(SanitizeScheduledText(summary, "")))
	if len(runes) > 4000 {
		runes = runes[len(runes)-4000:]
	}
	return strings.TrimSpace(string(runes))
}

func (r *Runtime) summarizeEinoMemory(ctx context.Context, profile nativeModelProfile, previousSummary string, overflow []*schema.Message) (string, error) {
	summaryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	profile.MaxOutputTokens = 512
	chatModel, err := r.newEinoChatModel(summaryCtx, profile)
	if err != nil {
		return "", err
	}
	previousSummary, overflowText := boundedMemorySummaryInputs(profile, previousSummary, einoMessagesToSummary(overflow))
	prompt := "Existing summary:\n" + fallbackString(previousSummary, "(empty)") +
		"\n\nNew conversation messages to merge:\n" + overflowText
	message, err := chatModel.Generate(summaryCtx, []*schema.Message{
		schema.SystemMessage("You compress Dirextalk Agent conversation memory. Preserve user preferences, decisions, room/contact names, tool outcomes, and unresolved tasks. Return a concise Chinese summary only."),
		schema.UserMessage(prompt),
	})
	if err != nil {
		return "", err
	}
	return message.Content, nil
}

func boundedMemorySummaryInputs(profile nativeModelProfile, previousSummary, overflow string) (string, string) {
	budget := nativeAgentInputTokenBudget(profile.ContextWindow, profile.MaxOutputTokens)
	if budget <= 0 {
		budget = 12000
	}
	// Leave room for the compression instructions and provider framing. Favor
	// newer overflow facts while retaining a smaller slice of the old summary.
	budget = budget * 3 / 4
	previousBudget := budget / 3
	overflowBudget := budget - previousBudget
	return tailRunesWithinTokenBudget(strings.TrimSpace(previousSummary), previousBudget), tailRunesWithinTokenBudget(strings.TrimSpace(overflow), overflowBudget)
}

func tailRunesWithinTokenBudget(value string, budget int) string {
	if budget <= 0 || value == "" {
		return ""
	}
	runes := []rune(value)
	used, start := 0, len(runes)
	for start > 0 {
		cost := 1
		if runes[start-1] < 128 {
			cost = 1 // conservative for short mixed-language summaries
		}
		if used+cost > budget {
			break
		}
		used += cost
		start--
	}
	return strings.TrimSpace(string(runes[start:]))
}

func memoryCompressionUsesModel(config map[string]any, params map[string]any) bool {
	mode := strings.ToLower(fallbackString(trimString(params["memory_compression"]), trimString(config["memory_compression"])))
	if mode == "" {
		mode = strings.ToLower(fallbackString(trimString(params["context_compression"]), trimString(config["context_compression"])))
	}
	if mode == "text" || mode == "deterministic" || mode == "off" || mode == "disabled" {
		return false
	}
	// Model-backed summarization is the default whenever the active profile has
	// credentials. Callers can explicitly select deterministic text compaction.
	return mode == "" || mode == "model" || mode == "llm" || mode == "eino_model" || boolParam(params["model_memory_compression"])
}

func compressionLabel(profile nativeModelProfile) string {
	if profile.APIKey != "" {
		return "eino_model"
	}
	return "text"
}
