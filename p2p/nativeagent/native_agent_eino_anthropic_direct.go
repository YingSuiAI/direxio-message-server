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

const (
	anthropicVersion           = "2023-06-01"
	anthropicRequiredMaxTokens = 65536
)

type anthropicDirectChatModel struct {
	r       *Runtime
	profile nativeModelProfile
	tools   []*schema.ToolInfo
}

func newAnthropicDirectChatModel(r *Runtime, profile nativeModelProfile) model.ToolCallingChatModel {
	return &anthropicDirectChatModel{r: r, profile: profile}
}

func (m *anthropicDirectChatModel) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	payload, err := m.requestPayload(input, false)
	if err != nil {
		return nil, err
	}
	decoded, err := m.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	return anthropicDirectMessageFromResponse(decoded), nil
}

func (m *anthropicDirectChatModel) Stream(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	payload, err := m.requestPayload(input, true)
	if err != nil {
		return nil, err
	}
	decoder := newAnthropicDirectStreamDecoder()
	return streamDirectModel(ctx, m.r.client, m.newRequest, payload, decoder.decode)
}

func (m *anthropicDirectChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	clone := *m
	clone.tools = append([]*schema.ToolInfo{}, tools...)
	return &clone, nil
}

func (m *anthropicDirectChatModel) requestPayload(input []*schema.Message, stream bool) (map[string]any, error) {
	system, messages := anthropicDirectMessages(input)
	payload := map[string]any{
		"model":      m.profile.Model,
		"messages":   messages,
		"max_tokens": fallbackInt(m.profile.MaxOutputTokens, anthropicRequiredMaxTokens),
	}
	if stream {
		payload["stream"] = true
	}
	if system != "" {
		payload["system"] = system
	}
	if m.profile.Temperature != nil {
		payload["temperature"] = *m.profile.Temperature
	}
	if m.profile.TopP != nil {
		payload["top_p"] = *m.profile.TopP
	}
	if len(m.tools) > 0 {
		payload["tools"] = anthropicDirectTools(m.tools)
	}
	return payload, nil
}

func (m *anthropicDirectChatModel) post(ctx context.Context, payload map[string]any) (map[string]any, error) {
	return postDirectModel(ctx, m.r.client, m.newRequest, payload)
}

func (m *anthropicDirectChatModel) newRequest(ctx context.Context, payload map[string]any) (*http.Request, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := anthropicV1BaseURL(m.profile.BaseURL) + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", m.profile.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	return req, nil
}

func anthropicV1BaseURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL
	}
	return baseURL + "/v1"
}
