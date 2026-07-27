package nativeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type openAICompatibleDirectChatModel struct {
	r       *Runtime
	profile nativeModelProfile
	tools   []*schema.ToolInfo
}

func newOpenAICompatibleDirectChatModel(r *Runtime, profile nativeModelProfile) model.ToolCallingChatModel {
	return &openAICompatibleDirectChatModel{r: r, profile: profile}
}

func (m *openAICompatibleDirectChatModel) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	payload, err := m.requestPayload(input, false)
	if err != nil {
		return nil, err
	}
	decoded, err := m.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	return openAICompatibleMessageFromResponse(decoded), nil
}

func (m *openAICompatibleDirectChatModel) Stream(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	payload, err := m.requestPayload(input, true)
	if err != nil {
		return nil, err
	}
	return streamDirectModel(ctx, m.r.client, m.newRequest, payload, openAICompatibleMessageFromStreamEvent)
}

func (m *openAICompatibleDirectChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	clone := *m
	clone.tools = append([]*schema.ToolInfo{}, tools...)
	return &clone, nil
}

func (m *openAICompatibleDirectChatModel) requestPayload(input []*schema.Message, stream bool) (map[string]any, error) {
	payload := map[string]any{
		"model":    m.profile.Model,
		"messages": openAICompatibleMessages(input),
	}
	if stream {
		payload["stream"] = true
	}
	if m.profile.MaxOutputTokens > 0 {
		payload["max_tokens"] = m.profile.MaxOutputTokens
	}
	if m.profile.Temperature != nil {
		payload["temperature"] = *m.profile.Temperature
	}
	if m.profile.TopP != nil {
		payload["top_p"] = *m.profile.TopP
	}
	if m.profile.ReasoningMode != "" {
		payload["reasoning_effort"] = m.profile.ReasoningMode
	}
	if len(m.tools) > 0 {
		payload["tools"] = openAICompatibleTools(m.tools)
	}
	return payload, nil
}

func (m *openAICompatibleDirectChatModel) post(ctx context.Context, payload map[string]any) (map[string]any, error) {
	return postDirectModel(ctx, m.r.client, m.newRequest, payload)
}

func (m *openAICompatibleDirectChatModel) newRequest(ctx context.Context, payload map[string]any) (*http.Request, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.chatCompletionsURL(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.profile.APIKey)
	return req, nil
}

func (m *openAICompatibleDirectChatModel) chatCompletionsURL() string {
	if m.profile.Provider == "deepseek" {
		base := strings.TrimRight(m.profile.BaseURL, "/")
		return base + "/chat/completions"
	}
	base := normalizedOpenAIBaseURL(m.profile)
	return strings.TrimRight(base, "/") + "/chat/completions"
}

func openAICompatibleMessages(input []*schema.Message) []map[string]any {
	messages := make([]map[string]any, 0, len(input))
	for _, message := range input {
		if message == nil {
			continue
		}
		record := map[string]any{
			"role": string(message.Role),
		}
		switch message.Role {
		case schema.Tool:
			record["content"] = message.Content
			record["tool_call_id"] = message.ToolCallID
			if message.ToolName != "" {
				record["name"] = message.ToolName
			}
		case schema.Assistant:
			record["content"] = message.Content
			if len(message.ToolCalls) > 0 {
				record["tool_calls"] = openAICompatibleToolCalls(message.ToolCalls)
			}
		default:
			if len(message.UserInputMultiContent) > 0 {
				record["content"] = openAICompatibleInputContent(message)
			} else {
				record["content"] = message.Content
			}
		}
		messages = append(messages, record)
	}
	return messages
}

func openAICompatibleInputContent(message *schema.Message) []map[string]any {
	parts := make([]map[string]any, 0, len(message.UserInputMultiContent))
	for _, part := range message.UserInputMultiContent {
		switch part.Type {
		case schema.ChatMessagePartTypeText:
			parts = append(parts, map[string]any{"type": "text", "text": part.Text})
		case schema.ChatMessagePartTypeImageURL:
			if part.Image == nil || part.Image.Base64Data == nil {
				continue
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + part.Image.MIMEType + ";base64," + *part.Image.Base64Data}})
		}
	}
	return parts
}

func openAICompatibleToolCalls(calls []schema.ToolCall) []map[string]any {
	result := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		callType := strings.TrimSpace(call.Type)
		if callType == "" {
			callType = "function"
		}
		result = append(result, map[string]any{
			"id":   call.ID,
			"type": callType,
			"function": map[string]any{
				"name":      call.Function.Name,
				"arguments": call.Function.Arguments,
			},
		})
	}
	return result
}

func openAICompatibleTools(tools []*schema.ToolInfo) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		parameters := map[string]any{"type": "object", "properties": map[string]any{}}
		if tool.ParamsOneOf != nil {
			if js, err := tool.ParamsOneOf.ToJSONSchema(); err == nil && js != nil {
				var schemaMap map[string]any
				if data, err := json.Marshal(js); err == nil {
					_ = json.Unmarshal(data, &schemaMap)
				}
				if len(schemaMap) > 0 {
					parameters = schemaMap
				}
			}
		}
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Desc,
				"parameters":  parameters,
			},
		})
	}
	return result
}

func openAICompatibleMessageFromResponse(decoded map[string]any) *schema.Message {
	choices, _ := decoded["choices"].([]any)
	if len(choices) == 0 {
		return schema.AssistantMessage("", nil)
	}
	choice, _ := choices[0].(map[string]any)
	rawMessage, _ := choice["message"].(map[string]any)
	message := schema.AssistantMessage(openAICompatibleText(rawMessage["content"]), openAICompatibleToolCallsFromAny(rawMessage["tool_calls"]))
	message.ReasoningContent = openAICompatibleText(rawMessage["reasoning_content"])
	return message
}

func openAICompatibleMessageFromStreamEvent(data []byte) *schema.Message {
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		return nil
	}
	choices, _ := event["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	content := openAICompatibleText(delta["content"])
	reasoningContent := openAICompatibleText(delta["reasoning_content"])
	calls := openAICompatibleToolCallsFromAny(delta["tool_calls"])
	if content == "" && reasoningContent == "" && len(calls) == 0 {
		return nil
	}
	message := schema.AssistantMessage(content, calls)
	message.ReasoningContent = reasoningContent
	return message
}

func openAICompatibleText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, raw := range v {
			part, _ := raw.(map[string]any)
			if text := trimString(part["text"]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

func openAICompatibleToolCallsFromAny(value any) []schema.ToolCall {
	rawCalls, _ := value.([]any)
	calls := make([]schema.ToolCall, 0, len(rawCalls))
	for _, raw := range rawCalls {
		record, _ := raw.(map[string]any)
		function, _ := record["function"].(map[string]any)
		var index *int
		if idx, ok := intFromJSONNumber(record["index"]); ok {
			index = &idx
		}
		calls = append(calls, schema.ToolCall{
			Index: index,
			ID:    trimString(record["id"]),
			Type:  fallbackString(trimString(record["type"]), "function"),
			Function: schema.FunctionCall{
				Name:      trimString(function["name"]),
				Arguments: trimString(function["arguments"]),
			},
		})
	}
	return calls
}

func intFromJSONNumber(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}
