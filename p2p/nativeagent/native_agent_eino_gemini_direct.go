package nativeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type geminiDirectChatModel struct {
	r       *Runtime
	profile nativeModelProfile
	tools   []*schema.ToolInfo
}

func newGeminiDirectChatModel(r *Runtime, profile nativeModelProfile) model.ToolCallingChatModel {
	return &geminiDirectChatModel{r: r, profile: profile}
}

func (m *geminiDirectChatModel) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	payload, err := m.requestPayload(input)
	if err != nil {
		return nil, err
	}
	decoded, err := postDirectModel(ctx, m.r.client, m.newGenerateRequest, payload)
	if err != nil {
		return nil, err
	}
	return geminiDirectMessageFromResponse(decoded), nil
}

func (m *geminiDirectChatModel) Stream(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	payload, err := m.requestPayload(input)
	if err != nil {
		return nil, err
	}
	return streamDirectModel(ctx, m.r.client, m.newStreamRequest, payload, geminiDirectMessageFromStreamEvent)
}

func (m *geminiDirectChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	clone := *m
	clone.tools = append([]*schema.ToolInfo{}, tools...)
	return &clone, nil
}

func (m *geminiDirectChatModel) requestPayload(input []*schema.Message) (map[string]any, error) {
	system, contents := geminiDirectContents(input)
	payload := map[string]any{"contents": contents}
	if system != "" {
		payload["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}
	generationConfig := map[string]any{}
	if m.profile.MaxOutputTokens > 0 {
		generationConfig["maxOutputTokens"] = m.profile.MaxOutputTokens
	}
	if m.profile.Temperature != nil {
		generationConfig["temperature"] = *m.profile.Temperature
	}
	if m.profile.TopP != nil {
		generationConfig["topP"] = *m.profile.TopP
	}
	if len(generationConfig) > 0 {
		payload["generationConfig"] = generationConfig
	}
	if len(m.tools) > 0 {
		payload["tools"] = []map[string]any{{
			"functionDeclarations": geminiDirectTools(m.tools),
		}}
	}
	return payload, nil
}

func (m *geminiDirectChatModel) newGenerateRequest(ctx context.Context, payload map[string]any) (*http.Request, error) {
	return m.newRequest(ctx, payload, "generateContent", false)
}

func (m *geminiDirectChatModel) newStreamRequest(ctx context.Context, payload map[string]any) (*http.Request, error) {
	return m.newRequest(ctx, payload, "streamGenerateContent", true)
}

func (m *geminiDirectChatModel) newRequest(ctx context.Context, payload map[string]any, action string, stream bool) (*http.Request, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	modelID := strings.TrimPrefix(strings.TrimSpace(m.profile.Model), "models/")
	endpoint := fmt.Sprintf(
		"%s/models/%s:%s",
		geminiV1BetaBaseURL(m.profile.BaseURL),
		url.PathEscape(modelID),
		action,
	)
	if stream {
		endpoint += "?alt=sse"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", m.profile.APIKey)
	return req, nil
}

func geminiV1BetaBaseURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Path != "" {
		return baseURL
	}
	return baseURL + "/v1beta"
}

func geminiDirectContents(input []*schema.Message) (string, []map[string]any) {
	var systemParts []string
	contents := make([]map[string]any, 0, len(input))
	toolNamesByCallID := map[string]string{}
	for _, message := range input {
		if message == nil {
			continue
		}
		if message.Role == schema.System {
			if text := strings.TrimSpace(message.Content); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		role := "user"
		parts := make([]map[string]any, 0, 1+len(message.ToolCalls))
		switch message.Role {
		case schema.Assistant:
			role = "model"
			if message.Content != "" {
				parts = append(parts, map[string]any{"text": message.Content})
			}
			for _, call := range message.ToolCalls {
				args := map[string]any{}
				if strings.TrimSpace(call.Function.Arguments) != "" {
					_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
				}
				if call.ID != "" && call.Function.Name != "" {
					toolNamesByCallID[call.ID] = call.Function.Name
				}
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{
						"name": call.Function.Name,
						"args": args,
					},
				})
			}
		case schema.Tool:
			response := map[string]any{"result": message.Content}
			if strings.TrimSpace(message.Content) != "" {
				var decoded any
				if json.Unmarshal([]byte(message.Content), &decoded) == nil {
					if decodedMap, ok := decoded.(map[string]any); ok {
						response = decodedMap
					} else {
						response = map[string]any{"result": decoded}
					}
				}
			}
			toolName := strings.TrimSpace(message.ToolName)
			if toolName == "" {
				toolName = toolNamesByCallID[message.ToolCallID]
			}
			parts = append(parts, map[string]any{
				"functionResponse": map[string]any{
					"name":     toolName,
					"response": response,
				},
			})
		default:
			if len(message.UserInputMultiContent) > 0 {
				for _, part := range message.UserInputMultiContent {
					switch part.Type {
					case schema.ChatMessagePartTypeText:
						if part.Text != "" {
							parts = append(parts, map[string]any{"text": part.Text})
						}
					case schema.ChatMessagePartTypeImageURL:
						if part.Image != nil && part.Image.Base64Data != nil {
							parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": part.Image.MIMEType, "data": *part.Image.Base64Data}})
						}
					}
				}
			} else if message.Content != "" {
				parts = append(parts, map[string]any{"text": message.Content})
			}
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	return strings.Join(systemParts, "\n\n"), contents
}

func geminiDirectTools(tools []*schema.ToolInfo) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		parameters := map[string]any{"type": "object", "properties": map[string]any{}}
		if tool.ParamsOneOf != nil {
			if js, err := tool.ParamsOneOf.ToJSONSchema(); err == nil && js != nil {
				if data, err := json.Marshal(js); err == nil {
					_ = json.Unmarshal(data, &parameters)
				}
			}
		}
		result = append(result, map[string]any{
			"name":        tool.Name,
			"description": tool.Desc,
			"parameters":  parameters,
		})
	}
	return result
}

func geminiDirectMessageFromResponse(decoded map[string]any) *schema.Message {
	candidates, _ := decoded["candidates"].([]any)
	if len(candidates) == 0 {
		return schema.AssistantMessage("", nil)
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	return geminiDirectMessageFromContent(content)
}

func geminiDirectMessageFromStreamEvent(data []byte) *schema.Message {
	var event map[string]any
	if json.Unmarshal(data, &event) != nil {
		return nil
	}
	message := geminiDirectMessageFromResponse(event)
	if message.Content == "" && len(message.ToolCalls) == 0 {
		return nil
	}
	return message
}

func geminiDirectMessageFromContent(content map[string]any) *schema.Message {
	rawParts, _ := content["parts"].([]any)
	var text strings.Builder
	var calls []schema.ToolCall
	for index, raw := range rawParts {
		part, _ := raw.(map[string]any)
		if value := trimString(part["text"]); value != "" {
			text.WriteString(value)
		}
		functionCall, _ := part["functionCall"].(map[string]any)
		name := trimString(functionCall["name"])
		if name == "" {
			continue
		}
		args, _ := json.Marshal(functionCall["args"])
		callIndex := index
		calls = append(calls, schema.ToolCall{
			Index: &callIndex,
			ID:    fmt.Sprintf("gemini-tool-call-%d", index),
			Type:  "function",
			Function: schema.FunctionCall{
				Name:      name,
				Arguments: string(args),
			},
		})
	}
	return schema.AssistantMessage(text.String(), calls)
}
