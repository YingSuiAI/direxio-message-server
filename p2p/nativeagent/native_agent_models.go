package nativeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (r *Runtime) modelsList(ctx context.Context, params map[string]any) (map[string]any, error) {
	if trimString(params["model_profile_id"]) != "" || trimString(params["client_model_profile_id"]) != "" {
		if _, present := params["api_key"]; present {
			return nil, fmt.Errorf("api_key must not be provided with a model profile ID")
		}
		profileParams := cloneAnyMap(params)
		// An ID always selects the server-owned encrypted profile. Do not let a
		// legacy inline model_profile shadow that lookup for this read action.
		delete(profileParams, "model_profile")
		profile, err := r.resolveModelProfileForRequest(ctx, profileParams)
		if err != nil {
			return nil, err
		}
		requestProvider := strings.ToLower(trimString(params["provider"]))
		if requestProvider != "" && requestProvider != profile.Provider {
			return nil, fmt.Errorf("provider %q does not match model profile provider %q", requestProvider, profile.Provider)
		}
		baseURL := trimString(params["base_url"])
		if strings.TrimSpace(profile.BaseURL) == "" {
			if baseURL != "" {
				return nil, fmt.Errorf("model profile base_url is required before an override can be used")
			}
		} else {
			storedOrigin, err := modelListBaseURLOrigin(profile.BaseURL)
			if err != nil {
				return nil, fmt.Errorf("stored model profile base_url is invalid: %w", err)
			}
			if baseURL != "" {
				overrideOrigin, err := modelListBaseURLOrigin(baseURL)
				if err != nil {
					return nil, fmt.Errorf("base_url override is invalid: %w", err)
				}
				if overrideOrigin != storedOrigin {
					return nil, fmt.Errorf("base_url override must use the stored model profile origin")
				}
			}
		}
		params = cloneAnyMap(params)
		params["provider"], params["base_url"], params["api_key"] = profile.Provider, fallbackString(baseURL, profile.BaseURL), profile.APIKey
	}
	provider := strings.ToLower(trimString(params["provider"]))
	modelKind := normalizeModelListKind(params["model_kind"])
	if modelKind == "" {
		return nil, fmt.Errorf("model list kind %q is not supported", trimString(params["model_kind"]))
	}
	result := map[string]any{
		"models":    []map[string]any{},
		"providers": modelProviderDefaults(),
	}
	if provider == "" {
		if modelKind == "embedding" || modelKind == "speech" {
			return nil, fmt.Errorf("provider is required for model list kind %q", modelKind)
		}
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
	if modelKind == "speech" || (modelKind == "embedding" && provider != "openrouter") {
		return nil, fmt.Errorf("model list kind %q is not supported for provider %q", modelKind, provider)
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
		if modelKind == "embedding" {
			if provider != "openrouter" {
				return nil, fmt.Errorf("model list kind %q is not supported for provider %q", modelKind, provider)
			}
			models, err = r.fetchOpenRouterEmbeddingModels(ctx, baseURL, apiKey)
		} else if modelKind == "speech" {
			return nil, fmt.Errorf("model list kind %q is not supported for provider %q", modelKind, provider)
		} else {
			models, err = r.fetchOpenAICompatibleModels(ctx, provider, baseURL, apiKey)
		}
	default:
		return nil, fmt.Errorf("model list is not supported for provider %q", provider)
	}
	if err != nil {
		return nil, err
	}
	result["models"] = models
	return result, nil
}

func normalizeModelListKind(value any) string {
	switch strings.ToLower(trimString(value)) {
	case "", "conversation":
		return "conversation"
	case "embedding", "speech":
		return strings.ToLower(trimString(value))
	default:
		return ""
	}
}

func modelListBaseURLOrigin(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("base_url is empty")
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse URL: %w", err)
	}
	if u.IsAbs() == false || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("URL must include an absolute HTTP(S) origin")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("URL userinfo, query, and fragment are not allowed")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("URL scheme must be http or https")
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("URL port is empty")
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else {
		n, parseErr := strconv.Atoi(port)
		if parseErr != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("URL port is invalid")
		}
		port = strconv.Itoa(n)
	}
	host := strings.ToLower(u.Hostname())
	return scheme + "://" + host + ":" + port, nil
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
	models := normalizeModelList("anthropic", filterModelListForKind("anthropic", "conversation", payload.Data))
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
	models := normalizeModelList("gemini", filterModelListForKind("gemini", "conversation", payload.Models))
	if len(models) == 0 {
		return nil, fmt.Errorf("fetch gemini models returned no models")
	}
	return models, nil
}

func (r *Runtime) fetchOpenAICompatibleModels(ctx context.Context, provider, baseURL, apiKey string) ([]map[string]any, error) {
	modelsURL, err := openAICompatibleModelsURL(provider, baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse %s model URL: %w", provider, err)
	}
	if modelsURL == "" {
		return nil, fmt.Errorf("base_url is required to fetch %s models", provider)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
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
	rawModels = filterModelListForKind(provider, "conversation", rawModels)
	models := normalizeModelList(provider, rawModels)
	if len(models) == 0 {
		return nil, fmt.Errorf("fetch %s models returned no models", provider)
	}
	return models, nil
}

func openAICompatibleModelsURL(provider, baseURL string) (string, error) {
	base := strings.TrimRight(openAICompatibleModelsBaseURL(provider, baseURL), "/")
	if base == "" {
		return "", nil
	}
	endpoint, err := url.Parse(base + "/models")
	if err != nil {
		return "", err
	}
	query := endpoint.Query()
	switch {
	case provider == "openrouter":
		query.Set("output_modalities", "text")
	case isSiliconFlowBaseURL(base):
		query.Set("type", "text")
		query.Set("sub_type", "chat")
	}
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func isSiliconFlowBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "api.siliconflow.cn" || host == "api.siliconflow.com"
}

func (r *Runtime) fetchOpenRouterEmbeddingModels(ctx context.Context, baseURL, apiKey string) ([]map[string]any, error) {
	url := strings.TrimRight(openAICompatibleModelsBaseURL("openrouter", baseURL), "/")
	if url == "" {
		return nil, fmt.Errorf("base_url is required to fetch openrouter models")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/embeddings/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch openrouter embedding models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch openrouter embedding models failed: %s", resp.Status)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode openrouter embedding models: %w", err)
	}
	models := normalizeModelList("openrouter", filterModelListForKind("openrouter", "embedding", payload.Data))
	if len(models) == 0 {
		return nil, fmt.Errorf("fetch openrouter embedding models returned no models")
	}
	return models, nil
}

func filterModelListForKind(provider, kind string, rawModels []map[string]any) []map[string]any {
	filtered := make([]map[string]any, 0, len(rawModels))
	for _, raw := range rawModels {
		if kind == "embedding" {
			// The dedicated OpenRouter embedding endpoint is authoritative. If it
			// still reports output modalities, retain only embedding models.
			if modalities, present := modelOutputModalities(raw); present && !containsString(modalities, "embedding") && !containsString(modalities, "embeddings") {
				continue
			}
		} else if modelIsNonConversation(raw) {
			continue
		}
		filtered = append(filtered, raw)
	}
	return filtered
}

func modelIsNonConversation(raw map[string]any) bool {
	for _, key := range []string{"type", "model_type", "kind"} {
		value := strings.ToLower(trimString(raw[key]))
		switch value {
		case "embedding", "embeddings", "rerank", "speech", "transcription", "image_generation", "moderation":
			return true
		}
	}
	if methods, present := stringListField(raw["supportedGenerationMethods"]); present {
		for _, method := range methods {
			if method == "generatecontent" || method == "generate_content" {
				return false
			}
		}
		return true
	}
	if modalities, present := modelOutputModalities(raw); present {
		return !containsString(modalities, "text")
	}
	if architecture, ok := raw["architecture"].(map[string]any); ok {
		modality := strings.ToLower(trimString(architecture["modality"]))
		for _, marker := range []string{"->embedding", "->rerank", "->image", "->audio", "->speech", "->moderation"} {
			if strings.Contains(modality, marker) {
				return true
			}
		}
	}
	return modelIDLooksNonConversation(raw)
}

func modelIDLooksNonConversation(raw map[string]any) bool {
	id := strings.ToLower(fallbackString(trimString(raw["id"]), trimString(raw["name"])))
	if id == "" {
		id = strings.ToLower(trimString(raw["model"]))
	}
	for _, marker := range []string{
		"text-embedding", "embedding-", "/embedding", "rerank", "tts", "speech", "whisper", "transcrib",
		"dall-e", "gpt-image", "stable-diffusion", "flux", "moderation",
	} {
		if strings.Contains(id, marker) {
			return true
		}
	}
	return false
}

func modelOutputModalities(raw map[string]any) ([]string, bool) {
	if modalities, present := stringListField(raw["output_modalities"]); present {
		return modalities, true
	}
	architecture, ok := raw["architecture"].(map[string]any)
	if !ok {
		return nil, false
	}
	modalities, present := stringListField(architecture["output_modalities"])
	return modalities, present
}

func modelInputModalities(raw map[string]any) ([]string, bool) {
	if modalities, present := stringListField(raw["input_modalities"]); present {
		return normalizeModelInputModalities(modalities), true
	}
	architecture, ok := raw["architecture"].(map[string]any)
	if !ok {
		return nil, false
	}
	modalities, present := stringListField(architecture["input_modalities"])
	if !present {
		return nil, false
	}
	return normalizeModelInputModalities(modalities), true
}

func normalizeModelInputModalities(modalities []string) []string {
	known := map[string]struct{}{"text": {}, "image": {}}
	result := make([]string, 0, len(modalities))
	seen := make(map[string]struct{}, len(modalities))
	for _, modality := range modalities {
		modality = strings.ToLower(strings.TrimSpace(modality))
		if modality == "" {
			continue
		}
		if _, ok := known[modality]; !ok {
			continue
		}
		if _, ok := seen[modality]; ok {
			continue
		}
		seen[modality] = struct{}{}
		result = append(result, modality)
	}
	return result
}

func stringListField(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok {
		if stringsValue, ok := value.([]string); ok {
			items = make([]any, 0, len(stringsValue))
			for _, item := range stringsValue {
				items = append(items, item)
			}
		} else {
			return nil, false
		}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		value := strings.ToLower(trimString(item))
		if value != "" {
			result = append(result, value)
		}
	}
	return result, true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
		if inputModalities, present := modelInputModalities(raw); present && len(inputModalities) > 0 {
			model["input_modalities"] = inputModalities
		}
		models = append(models, model)
	}
	return models
}
