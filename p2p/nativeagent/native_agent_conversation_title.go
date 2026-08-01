package nativeagent

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"github.com/cloudwego/eino/schema"
)

const nativeAgentConversationTitleRunes = 32

func (r *Runtime) maybeSetAutomaticConversationTitle(ctx context.Context, profile nativeModelProfile, run nativeAgentRunContext, produced []*schema.Message) {
	if !r.persistentMemoryReady || run.conversationID == "" || run.memory.LastMessageSeq != 0 || strings.TrimSpace(run.memory.Title) != "" {
		return
	}
	store, ok := r.conversations.(agentmemory.AutomaticConversationTitleStore)
	if !ok {
		return
	}
	userText := firstConversationTitleText(run.memoryMessages, schema.User)
	if userText == "" {
		return
	}
	title := ""
	if profile.APIKey != "" {
		title = r.generateConversationTitle(ctx, profile, userText, firstConversationTitleText(produced, schema.Assistant))
	}
	if title == "" {
		title = conversationTitleFallback(userText, profile.APIKey)
	}
	if title == "" {
		return
	}
	// Title generation is a best-effort projection after the durable turn. A
	// provider or title update failure must never change the chat result.
	_, _ = store.SetAutomaticConversationTitle(ctx, r.effectiveOwner(ctx), run.conversationID, title)
}

func (r *Runtime) generateConversationTitle(ctx context.Context, profile nativeModelProfile, userText, assistantText string) string {
	titleCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	profile.MaxOutputTokens = 64
	chatModel, err := r.newEinoChatModel(titleCtx, profile)
	if err != nil {
		return ""
	}
	prompt := "User:\n" + truncateConversationTitleSource(userText, 1200)
	if assistantText = strings.TrimSpace(assistantText); assistantText != "" {
		prompt += "\n\nAssistant:\n" + truncateConversationTitleSource(assistantText, 1200)
	}
	message, err := chatModel.Generate(titleCtx, []*schema.Message{
		schema.SystemMessage("Generate one concise title for this conversation in the user's language. Return only the title, without quotes, markdown, punctuation suffixes, or explanation. Use at most 16 Chinese/Japanese characters or 8 English words. Never include credentials, tokens, URLs, UUIDs, or raw identifiers."),
		schema.UserMessage(prompt),
	})
	if err != nil || message == nil {
		return ""
	}
	return normalizeConversationTitle(message.Content, profile.APIKey)
}

func firstConversationTitleText(messages []*schema.Message, role schema.RoleType) string {
	for _, message := range messages {
		if message != nil && message.Role == role {
			if value := strings.TrimSpace(message.Content); value != "" {
				return value
			}
		}
	}
	return ""
}

func conversationTitleFallback(userText, apiKey string) string {
	value := SanitizeScheduledText(userText, apiKey)
	for index, r := range value {
		if index > 0 && (r == '\n' || r == '\r' || r == '。' || r == '！' || r == '？' || r == '.' || r == '!' || r == '?') {
			value = value[:index]
			break
		}
	}
	return normalizeConversationTitle(value, apiKey)
}

func normalizeConversationTitle(value, apiKey string) string {
	value = SanitizeScheduledText(value, apiKey)
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimSpace(strings.Trim(value, "`#*_\"'“”‘’「」『』【】[]()（）:：;；,.，。!?！？-—"))
	if value == "" {
		return ""
	}
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > nativeAgentConversationTitleRunes {
		runes = runes[:nativeAgentConversationTitleRunes]
	}
	for len(runes) > 0 && (unicode.IsPunct(runes[len(runes)-1]) || unicode.IsSpace(runes[len(runes)-1])) {
		runes = runes[:len(runes)-1]
	}
	return strings.TrimSpace(string(runes))
}

func truncateConversationTitleSource(value string, limit int) string {
	runes := []rune(SanitizeScheduledText(value, ""))
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
