package nativeagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

type einoAgentSession struct {
	systemPrompt    string
	contextWindow   int
	maxOutputTokens int
}

func (s einoAgentSession) rewrite(_ context.Context, messages []*schema.Message) []*schema.Message {
	return compactEinoMessagesForContext(messages, s.contextWindow, s.maxOutputTokens)
}

func (s einoAgentSession) modify(_ context.Context, messages []*schema.Message) []*schema.Message {
	systemPrompt := strings.TrimSpace(s.systemPrompt)
	if systemPrompt == "" {
		return messages
	}
	result := make([]*schema.Message, 0, len(messages)+1)
	result = append(result, schema.SystemMessage(systemPrompt))
	result = append(result, messages...)
	return result
}

func requestEinoMessages(params map[string]any) []*schema.Message {
	result := make([]*schema.Message, 0, 8)
	if history, ok := params["messages"].([]any); ok {
		for _, raw := range history {
			message, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if msg := mapToEinoMessage(message); msg != nil {
				result = append(result, msg)
			}
		}
	}
	prompt := fallbackString(trimString(params["prompt"]), trimString(params["message"]))
	attachments, _ := parseNativeAgentAttachments(params)
	if prompt != "" || len(attachments) > 0 {
		if len(attachments) > 0 {
			result = append(result, &schema.Message{Role: schema.User, Content: prompt, UserInputMultiContent: nativeAgentAttachmentParts(prompt, attachments)})
		} else {
			result = append(result, schema.UserMessage(prompt))
		}
	}
	if len(result) == 0 {
		result = append(result, schema.UserMessage("你好"))
	}
	return sanitizeEinoMessagesForModel(result)
}

func mapToEinoMessage(message map[string]any) *schema.Message {
	content := trimString(message["content"])
	if content == "" {
		content = trimString(message["text"])
	}
	if content == "" {
		return nil
	}
	switch strings.ToLower(trimString(message["role"])) {
	case "system":
		return schema.SystemMessage(content)
	case "assistant":
		return schema.AssistantMessage(content, nil)
	case "tool":
		return schema.ToolMessage(content, trimString(message["tool_call_id"]), schema.WithToolName(trimString(message["name"])))
	default:
		return schema.UserMessage(content)
	}
}

func compactEinoMessages(messages []*schema.Message, contextWindow int) []*schema.Message {
	if contextWindow <= 0 {
		contextWindow = 48
	}
	if len(messages) <= contextWindow {
		return sanitizeEinoMessagesForModel(messages)
	}
	result := make([]*schema.Message, 0, contextWindow+1)
	if messages[0] != nil && messages[0].Role == schema.System {
		result = append(result, messages[0])
	}
	result = append(result, messages[len(messages)-contextWindow:]...)
	return sanitizeEinoMessagesForModel(result)
}

// compactEinoMessagesForContext treats model context_window as a token budget.
// The previous implementation treated it as a message count, so a 128k model
// effectively never compacted and could overflow on a few large tool results.
// This conservative estimator deliberately reserves space for output and tool
// schemas; the provider remains authoritative for exact tokenization.
func compactEinoMessagesForContext(messages []*schema.Message, contextWindow, maxOutputTokens int) []*schema.Message {
	messages = sanitizeEinoMessagesForModel(messages)
	if contextWindow <= 0 {
		return compactEinoMessages(messages, 48)
	}
	budget := nativeAgentInputTokenBudget(contextWindow, maxOutputTokens)
	if budget <= 0 || estimateEinoMessagesTokens(messages) <= budget {
		return messages
	}

	var system *schema.Message
	start := 0
	if len(messages) > 0 && messages[0] != nil && messages[0].Role == schema.System {
		system = messages[0]
		start = 1
	}
	used := estimateEinoMessageTokens(system)
	kept := make([]*schema.Message, 0, len(messages)-start)
	for i := len(messages) - 1; i >= start; i-- {
		cost := estimateEinoMessageTokens(messages[i])
		if len(kept) > 0 && used+cost > budget {
			break
		}
		kept = append(kept, messages[i])
		used += cost
	}
	for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
		kept[left], kept[right] = kept[right], kept[left]
	}
	result := make([]*schema.Message, 0, len(kept)+1)
	if system != nil {
		result = append(result, system)
	}
	result = append(result, kept...)
	return sanitizeEinoMessagesForModel(result)
}

func nativeAgentInputTokenBudget(contextWindow, maxOutputTokens int) int {
	if contextWindow <= 0 {
		return 0
	}
	outputReserve := maxOutputTokens
	if outputReserve <= 0 {
		outputReserve = contextWindow / 4
	}
	if outputReserve > contextWindow/2 {
		outputReserve = contextWindow / 2
	}
	toolReserve := contextWindow / 10
	if toolReserve > 4096 {
		toolReserve = 4096
	}
	budget := contextWindow - outputReserve - toolReserve
	if budget < contextWindow/4 {
		budget = contextWindow / 4
	}
	return budget
}

func estimateEinoMessagesTokens(messages []*schema.Message) int {
	total := 0
	for _, message := range messages {
		total += estimateEinoMessageTokens(message)
	}
	return total
}

func estimateEinoMessageTokens(message *schema.Message) int {
	if message == nil {
		return 0
	}
	total := 12 + estimateNativeAgentTextTokens(message.Content)
	for _, call := range message.ToolCalls {
		total += 16 + estimateNativeAgentTextTokens(call.Function.Name) + estimateNativeAgentTextTokens(call.Function.Arguments)
	}
	for _, part := range message.UserInputMultiContent {
		if part.Text != "" {
			total += estimateNativeAgentTextTokens(part.Text)
		} else {
			total += 256
		}
	}
	return total
}

func estimateNativeAgentTextTokens(value string) int {
	ascii, other := 0, 0
	for _, r := range value {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+2)/3 + other + 1
}

func sanitizeEinoMessagesForModel(messages []*schema.Message) []*schema.Message {
	result := make([]*schema.Message, 0, len(messages))
	pendingToolCalls := map[string]struct{}{}
	for _, message := range messages {
		if message == nil {
			continue
		}
		switch message.Role {
		case schema.Tool:
			if _, ok := pendingToolCalls[strings.TrimSpace(message.ToolCallID)]; !ok {
				continue
			}
			delete(pendingToolCalls, strings.TrimSpace(message.ToolCallID))
			result = append(result, message)
		default:
			pendingToolCalls = map[string]struct{}{}
			if message.Role == schema.Assistant && len(message.ToolCalls) > 0 {
				for _, call := range message.ToolCalls {
					if id := strings.TrimSpace(call.ID); id != "" {
						pendingToolCalls[id] = struct{}{}
					}
				}
			}
			result = append(result, message)
		}
	}
	return result
}

func cloneEinoMessages(messages []*schema.Message) []*schema.Message {
	if len(messages) == 0 {
		return nil
	}
	result := make([]*schema.Message, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		clone := *message
		if len(message.ToolCalls) > 0 {
			clone.ToolCalls = make([]schema.ToolCall, len(message.ToolCalls))
			for i, call := range message.ToolCalls {
				clone.ToolCalls[i] = call
				if call.Extra != nil {
					clone.ToolCalls[i].Extra = cloneAnyMap(call.Extra)
				}
			}
		}
		clone.UserInputMultiContent = cloneEinoInputParts(message.UserInputMultiContent)
		clone.MultiContent = cloneEinoChatParts(message.MultiContent)
		if len(message.Extra) > 0 {
			clone.Extra = cloneAnyMap(message.Extra)
		}
		result = append(result, &clone)
	}
	return result
}

func cloneEinoChatParts(parts []schema.ChatMessagePart) []schema.ChatMessagePart {
	if len(parts) == 0 {
		return nil
	}
	result := make([]schema.ChatMessagePart, len(parts))
	for i, part := range parts {
		result[i] = part
		if part.ImageURL != nil {
			copyPart := *part.ImageURL
			copyPart.Extra = cloneAnyMap(part.ImageURL.Extra)
			result[i].ImageURL = &copyPart
		}
		if part.AudioURL != nil {
			copyPart := *part.AudioURL
			copyPart.Extra = cloneAnyMap(part.AudioURL.Extra)
			result[i].AudioURL = &copyPart
		}
		if part.VideoURL != nil {
			copyPart := *part.VideoURL
			copyPart.Extra = cloneAnyMap(part.VideoURL.Extra)
			result[i].VideoURL = &copyPart
		}
		if part.FileURL != nil {
			copyPart := *part.FileURL
			copyPart.Extra = cloneAnyMap(part.FileURL.Extra)
			result[i].FileURL = &copyPart
		}
	}
	return result
}

func cloneEinoInputParts(parts []schema.MessageInputPart) []schema.MessageInputPart {
	if len(parts) == 0 {
		return nil
	}
	result := make([]schema.MessageInputPart, len(parts))
	for i, part := range parts {
		result[i] = part
		if part.Image != nil {
			image := *part.Image
			image.MessagePartCommon = part.Image.MessagePartCommon
			if part.Image.URL != nil {
				value := *part.Image.URL
				image.URL = &value
			}
			if part.Image.Base64Data != nil {
				value := *part.Image.Base64Data
				image.Base64Data = &value
			}
			image.Extra = cloneAnyMap(part.Image.Extra)
			result[i].Image = &image
		}
		if part.Extra != nil {
			result[i].Extra = cloneAnyMap(part.Extra)
		}
	}
	return result
}

func trimEinoMessageForMemory(message *schema.Message) *schema.Message {
	if message == nil {
		return nil
	}
	switch message.Role {
	case schema.System:
		return nil
	case schema.User, schema.Assistant, schema.Tool:
	default:
		return nil
	}
	clone := *message
	clone.ResponseMeta = nil
	clone.Extra = nil
	if len(clone.UserInputMultiContent) > 0 {
		marker := attachmentMemoryMarker(clone.UserInputMultiContent)
		if strings.TrimSpace(clone.Content) == "" {
			clone.Content = marker
		} else if marker != "" {
			clone.Content = strings.TrimSpace(clone.Content) + "\n" + marker
		}
		clone.UserInputMultiContent = nil
	}
	if clone.Role == schema.Assistant && len(clone.ToolCalls) == 0 && strings.TrimSpace(clone.Content) == "" {
		return nil
	}
	if clone.Role != schema.Assistant && strings.TrimSpace(clone.Content) == "" && len(clone.UserInputMultiContent) == 0 {
		return nil
	}
	if len(clone.ToolCalls) > 0 {
		clone.ToolCalls = append([]schema.ToolCall{}, clone.ToolCalls...)
	}
	return &clone
}

func attachmentMemoryMarker(parts []schema.MessageInputPart) string {
	count := 0
	for _, part := range parts {
		if part.Type == schema.ChatMessagePartTypeImageURL && part.Image != nil {
			count++
		}
	}
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("[attached %d image(s)]", count)
}

func compactEinoMessagesForMemory(messages []*schema.Message) []*schema.Message {
	result := make([]*schema.Message, 0, len(messages))
	for _, message := range messages {
		if msg := trimEinoMessageForMemory(message); msg != nil {
			result = append(result, msg)
		}
	}
	return result
}

func einoMessagesToSummary(messages []*schema.Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		content := strings.TrimSpace(message.Content)
		switch message.Role {
		case schema.Assistant:
			if len(message.ToolCalls) > 0 {
				names := make([]string, 0, len(message.ToolCalls))
				for _, call := range message.ToolCalls {
					names = append(names, call.Function.Name)
				}
				parts = append(parts, "assistant tool_call: "+strings.Join(names, ", "))
			}
			if content != "" {
				parts = append(parts, "assistant: "+content)
			}
		case schema.Tool:
			toolName := fallbackString(message.ToolName, "tool")
			if content != "" {
				parts = append(parts, toolName+": "+content)
			}
		case schema.User:
			if content != "" {
				parts = append(parts, "user: "+content)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func hasExplicitRequestMessages(params map[string]any) bool {
	values, ok := params["messages"].([]any)
	return ok && len(values) > 0
}
