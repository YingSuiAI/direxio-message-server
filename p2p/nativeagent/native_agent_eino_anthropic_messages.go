package nativeagent

import (
	"encoding/json"
	"strings"

	"github.com/cloudwego/eino/schema"
)

func anthropicDirectMessages(input []*schema.Message) (string, []map[string]any) {
	var system []string
	messages := make([]map[string]any, 0, len(input))
	for _, message := range input {
		if message == nil {
			continue
		}
		switch message.Role {
		case schema.System:
			if strings.TrimSpace(message.Content) != "" {
				system = append(system, strings.TrimSpace(message.Content))
			}
		case schema.Assistant:
			content := anthropicDirectAssistantContent(message)
			if len(content) > 0 {
				messages = append(messages, map[string]any{"role": "assistant", "content": content})
			}
		case schema.Tool:
			messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": message.ToolCallID,
				"content":     message.Content,
			}}})
		default:
			if len(message.UserInputMultiContent) > 0 {
				content := make([]map[string]any, 0, len(message.UserInputMultiContent))
				for _, part := range message.UserInputMultiContent {
					switch part.Type {
					case schema.ChatMessagePartTypeText:
						if part.Text != "" {
							content = append(content, map[string]any{"type": "text", "text": part.Text})
						}
					case schema.ChatMessagePartTypeImageURL:
						if part.Image != nil && part.Image.Base64Data != nil {
							content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": part.Image.MIMEType, "data": *part.Image.Base64Data}})
						}
					}
				}
				if len(content) > 0 {
					messages = append(messages, map[string]any{"role": "user", "content": content})
				}
			} else if strings.TrimSpace(message.Content) != "" {
				messages = append(messages, map[string]any{"role": "user", "content": message.Content})
			}
		}
	}
	return strings.Join(system, "\n\n"), messages
}

func anthropicDirectAssistantContent(message *schema.Message) []map[string]any {
	content := make([]map[string]any, 0, len(message.ToolCalls)+1)
	if strings.TrimSpace(message.Content) != "" {
		content = append(content, map[string]any{"type": "text", "text": message.Content})
	}
	for _, call := range message.ToolCalls {
		var input any = map[string]any{}
		if strings.TrimSpace(call.Function.Arguments) != "" {
			_ = json.Unmarshal([]byte(call.Function.Arguments), &input)
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Function.Name,
			"input": input,
		})
	}
	return content
}

func anthropicDirectTools(tools []*schema.ToolInfo) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		record := map[string]any{
			"name":        tool.Name,
			"description": tool.Desc,
		}
		if tool.ParamsOneOf != nil {
			if js, err := tool.ParamsOneOf.ToJSONSchema(); err == nil && js != nil {
				var schemaMap map[string]any
				if data, err := json.Marshal(js); err == nil {
					_ = json.Unmarshal(data, &schemaMap)
				}
				record["input_schema"] = schemaMap
			}
		}
		if record["input_schema"] == nil {
			record["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		result = append(result, record)
	}
	return result
}

func anthropicDirectMessageFromResponse(decoded map[string]any) *schema.Message {
	content, _ := decoded["content"].([]any)
	var parts []string
	var calls []schema.ToolCall
	for _, raw := range content {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch trimString(part["type"]) {
		case "text":
			if text := trimString(part["text"]); text != "" {
				parts = append(parts, text)
			}
		case "tool_use":
			args := part["input"]
			data, _ := json.Marshal(args)
			calls = append(calls, schema.ToolCall{
				ID:   trimString(part["id"]),
				Type: "function",
				Function: schema.FunctionCall{
					Name:      trimString(part["name"]),
					Arguments: string(data),
				},
			})
		}
	}
	return schema.AssistantMessage(strings.Join(parts, ""), calls)
}

func anthropicDirectMessageFromStreamEvent(data []byte) *schema.Message {
	return newAnthropicDirectStreamDecoder().decode(data)
}

// anthropicDirectStreamDecoder keeps the small amount of state needed to turn
// Anthropic's content-block SSE protocol into Eino's stream chunks. Anthropic
// announces a tool call in content_block_start and sends its JSON arguments in
// one or more input_json_delta events. Eino concatenates ToolCalls by Index, so
// the start and every fragment must carry the same index.
type anthropicDirectStreamDecoder struct {
	nextIndex  int
	activeTool *int
}

func newAnthropicDirectStreamDecoder() *anthropicDirectStreamDecoder {
	return &anthropicDirectStreamDecoder{}
}

func (d *anthropicDirectStreamDecoder) decode(data []byte) *schema.Message {
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		return nil
	}
	switch trimString(event["type"]) {
	case "content_block_delta":
		delta, _ := event["delta"].(map[string]any)
		switch trimString(delta["type"]) {
		case "text_delta":
			if text := anthropicEventString(delta["text"]); text != "" {
				return schema.AssistantMessage(text, nil)
			}
		case "input_json_delta":
			partial := anthropicEventString(delta["partial_json"])
			if partial == "" {
				return nil
			}
			index := d.eventIndex(event)
			if index == nil {
				index = d.activeTool
			}
			if index == nil {
				return nil
			}
			return schema.AssistantMessage("", []schema.ToolCall{{
				Index: index,
				Type:  "function",
				Function: schema.FunctionCall{
					Arguments: partial,
				},
			}})
		}
	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		switch trimString(block["type"]) {
		case "text":
			if text := anthropicEventString(block["text"]); text != "" {
				return schema.AssistantMessage(text, nil)
			}
		case "tool_use":
			index := d.eventIndex(event)
			if index == nil {
				value := d.nextIndex
				index = &value
			}
			d.activeTool = index
			if *index >= d.nextIndex {
				d.nextIndex = *index + 1
			}
			return schema.AssistantMessage("", []schema.ToolCall{{
				Index: index,
				ID:    trimString(block["id"]),
				Type:  "function",
				Function: schema.FunctionCall{
					Name:      trimString(block["name"]),
					Arguments: anthropicToolArguments(block["input"]),
				},
			}})
		}
	case "content_block_stop":
		if index := d.eventIndex(event); index != nil && d.activeTool != nil && *index == *d.activeTool {
			d.activeTool = nil
		}
	}
	return nil
}

func (d *anthropicDirectStreamDecoder) eventIndex(event map[string]any) *int {
	if index, ok := intFromJSONNumber(event["index"]); ok && index >= 0 {
		value := index
		return &value
	}
	return nil
}

func anthropicEventString(value any) string {
	valueString, _ := value.(string)
	return valueString
}

func anthropicToolArguments(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	data, err := json.Marshal(value)
	if err != nil || string(data) == "{}" {
		return ""
	}
	return string(data)
}
