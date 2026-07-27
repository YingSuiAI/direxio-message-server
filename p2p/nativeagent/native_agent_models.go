package nativeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func (r *Runtime) modelsList(ctx context.Context, params map[string]any) (map[string]any, error) {
	if trimString(params["model_profile_id"]) != "" || trimString(params["client_model_profile_id"]) != "" {
		profile, err := r.resolveModelProfileForRequest(ctx, params)
		if err != nil {
			return nil, err
		}
		params = cloneAnyMap(params)
		params["provider"], params["base_url"], params["api_key"] = profile.Provider, profile.BaseURL, profile.APIKey
	}
	provider := strings.ToLower(trimString(params["provider"]))
	result := map[string]any{
		"models":    []map[string]any{},
		"providers": modelProviderDefaults(),
	}
	if provider == "" {
		return result, nil
	}
	if !supportsNativeModelProvider(provider) {
		return nil, fmt.Errorf("model list is not supported for provider %q", provider)
	}
	baseURL := trimString(params["base_url"])
	if baseURL == "" {
		return nil, fmt.Errorf("base_url is required to fetch %s models; configure the model API address", provider)
	}
	apiKey := trimString(params["api_key"])
	if apiKey == "" {
		return nil, fmt.Errorf("api_key is required to fetch %s models", provider)
	}
	var (
		models []map[string]any
		err    error
	)
	switch provider {
	case "anthropic":
		models, err = r.fetchAnthropicModels(ctx, baseURL, apiKey)
	case "gemini":
		models, err = r.fetchGeminiModels(ctx, baseURL, apiKey)
	case "openai", "deepseek", "xai", "openai_compatible", "openrouter":
		models, err = r.fetchOpenAICompatibleModels(ctx, provider, baseURL, apiKey)
	default:
		return nil, fmt.Errorf("model list is not supported for provider %q", provider)
	}
	if err != nil {
		return nil, err
	}
	result["models"] = models
	return result, nil
}

func modelProviderDefaults() []map[string]any {
	return []map[string]any{
		{"provider": "openai", "default_base_url": defaultBaseURLForProvider("openai"), "requires_api_key": true, "dynamic_models": true},
		{"provider": "anthropic", "default_base_url": defaultBaseURLForProvider("anthropic"), "requires_api_key": true, "dynamic_models": true},
		{"provider": "deepseek", "default_base_url": defaultBaseURLForProvider("deepseek"), "requires_api_key": true, "dynamic_models": true},
		{"provider": "gemini", "default_base_url": defaultBaseURLForProvider("gemini"), "requires_api_key": true, "dynamic_models": true},
		{"provider": "xai", "default_base_url": defaultBaseURLForProvider("xai"), "requires_api_key": true, "dynamic_models": true},
		{"provider": "openai_compatible", "default_base_url": defaultBaseURLForProvider("openai_compatible"), "requires_api_key": true, "dynamic_models": true},
		{"provider": "openrouter", "default_base_url": defaultBaseURLForProvider("openrouter"), "requires_api_key": true, "dynamic_models": true},
	}
}

func (r *Runtime) fetchAnthropicModels(ctx context.Context, baseURL, apiKey string) ([]map[string]any, error) {
	baseURL = anthropicV1BaseURL(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("base_url is required to fetch anthropic models")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch anthropic models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch anthropic models failed: %s", resp.Status)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode anthropic models: %w", err)
	}
	models := normalizeModelList("anthropic", payload.Data)
	if len(models) == 0 {
		return nil, fmt.Errorf("fetch anthropic models returned no models")
	}
	return models, nil
}

func (r *Runtime) fetchGeminiModels(ctx context.Context, baseURL, apiKey string) ([]map[string]any, error) {
	baseURL = geminiV1BetaBaseURL(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("base_url is required to fetch gemini models")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch gemini models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch gemini models failed: %s", resp.Status)
	}
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode gemini models: %w", err)
	}
	models := normalizeModelList("gemini", payload.Models)
	if len(models) == 0 {
		return nil, fmt.Errorf("fetch gemini models returned no models")
	}
	return models, nil
}

func (r *Runtime) fetchOpenAICompatibleModels(ctx context.Context, provider, baseURL, apiKey string) ([]map[string]any, error) {
	url := strings.TrimRight(openAICompatibleModelsBaseURL(provider, baseURL), "/")
	if url == "" {
		return nil, fmt.Errorf("base_url is required to fetch %s models", provider)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s models: %w", provider, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s models failed: %s", provider, resp.Status)
	}
	var payload struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode %s models: %w", provider, err)
	}
	rawModels := payload.Data
	if len(rawModels) == 0 {
		rawModels = payload.Models
	}
	models := normalizeModelList(provider, rawModels)
	if len(models) == 0 {
		return nil, fmt.Errorf("fetch %s models returned no models", provider)
	}
	return models, nil
}

func openAICompatibleModelsBaseURL(provider, baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	if provider == "deepseek" {
		return baseURL
	}
	return normalizedOpenAIBaseURL(nativeModelProfile{Provider: provider, BaseURL: baseURL})
}

func normalizeModelList(provider string, rawModels []map[string]any) []map[string]any {
	seen := make(map[string]struct{}, len(rawModels))
	models := make([]map[string]any, 0, len(rawModels))
	for _, raw := range rawModels {
		id := fallbackString(trimString(raw["id"]), trimString(raw["name"]))
		if id == "" {
			id = trimString(raw["model"])
		}
		if provider == "gemini" {
			id = strings.TrimPrefix(id, "models/")
		}
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		model := map[string]any{
			"id": id,
			"name": fallbackString(
				fallbackString(trimString(raw["display_name"]), trimString(raw["displayName"])),
				fallbackString(trimString(raw["name"]), id),
			),
			"provider": provider,
		}
		for key, value := range raw {
			switch key {
			case "object", "created", "created_at", "owned_by", "type",
				"context_length", "max_input_tokens", "max_output_tokens",
				"max_tokens", "input_token_limit", "output_token_limit":
				model[key] = value
			}
		}
		models = append(models, model)
	}
	return models
}
