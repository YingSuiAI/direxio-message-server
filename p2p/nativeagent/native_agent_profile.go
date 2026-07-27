package nativeagent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

type nativeModelProfile struct {
	Provider        string
	Model           string
	BaseURL         string
	APIKey          string
	SystemPrompt    string
	Temperature     *float64
	TopP            *float64
	MaxOutputTokens int
	ContextWindow   int
	ReasoningMode   string
}

func (r *Runtime) resolveModelProfile(params map[string]any) nativeModelProfile {
	raw, _ := params["model_profile"].(map[string]any)
	return nativeModelProfile{
		Provider:        strings.ToLower(pluginConfigString(raw, "provider")),
		Model:           pluginConfigString(raw, "model"),
		BaseURL:         strings.TrimRight(pluginConfigString(raw, "base_url"), "/"),
		APIKey:          trimString(raw["api_key"]),
		SystemPrompt:    pluginConfigString(raw, "system_prompt"),
		Temperature:     optionalFloat(raw["temperature"]),
		TopP:            optionalFloat(raw["top_p"]),
		MaxOutputTokens: int(int64Param(raw["max_output_tokens"])),
		ContextWindow:   int(int64Param(raw["context_window"])),
		ReasoningMode:   normalizedReasoningMode(raw["reasoning_mode"]),
	}
}

func (r *Runtime) resolveModelProfileForRequest(ctx context.Context, params map[string]any) (nativeModelProfile, error) {
	// During the rolling upgrade window old Flutter sends the client-local
	// model_profile_id together with the complete inline profile and key. The
	// inline shape remains authoritative until that client path is retired.
	if _, present := params["model_profile"]; present {
		return r.resolveModelProfile(params), nil
	}
	serverID := trimString(params["model_profile_id"])
	legacyID := trimString(params["client_model_profile_id"])
	if serverID != "" && legacyID != "" && serverID != legacyID {
		return nativeModelProfile{}, errors.New("model_profile_id and client_model_profile_id are ambiguous")
	}
	if serverID == "" {
		serverID = legacyID
	}
	if serverID == "" {
		return r.resolveModelProfile(params), nil
	}
	if r == nil || r.modelProfiles == nil {
		return nativeModelProfile{}, errors.New("server model profiles are unavailable")
	}
	profile, err := r.modelProfiles.ResolveModelProfile(ctx, serverID)
	if err != nil {
		return nativeModelProfile{}, err
	}
	return nativeModelProfile{
		Provider:        strings.ToLower(strings.TrimSpace(profile.Provider)),
		Model:           strings.TrimSpace(profile.Model),
		BaseURL:         strings.TrimRight(strings.TrimSpace(profile.BaseURL), "/"),
		APIKey:          profile.APIKey,
		SystemPrompt:    strings.TrimSpace(profile.SystemPrompt),
		Temperature:     profile.Temperature,
		TopP:            profile.TopP,
		MaxOutputTokens: profile.MaxOutputTokens,
		ContextWindow:   profile.ContextWindow,
		ReasoningMode:   normalizedReasoningMode(profile.ReasoningEffort),
	}, nil
}

func validateModelProfile(profile nativeModelProfile) error {
	if profile.Provider == "" {
		return errors.New("model_profile.provider is required; select a model provider")
	}
	if !supportsNativeModelProvider(profile.Provider) {
		return errors.New("model_profile.provider is not supported")
	}
	if profile.Model == "" {
		return errors.New("model_profile.model is required; select a model")
	}
	if profile.BaseURL == "" {
		return errors.New("model_profile.base_url is required; configure the model API address")
	}
	if profile.APIKey == "" {
		return errors.New("model_profile.api_key is required")
	}
	return nil
}

func supportsNativeModelProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai", "anthropic", "deepseek", "gemini", "xai", "openai_compatible", "openrouter":
		return true
	default:
		return false
	}
}

func hasModelProfile(params map[string]any) bool {
	_, ok := params["model_profile"]
	return ok
}

func normalizedReasoningMode(value any) string {
	mode := strings.ToLower(strings.TrimSpace(trimString(value)))
	switch mode {
	case "", "none", "off":
		return ""
	case "minimal", "low", "medium", "high", "xhigh", "auto", "fast", "deep":
		return mode
	default:
		return mode
	}
}

func defaultBaseURLForProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openai":
		return "https://api.openai.com/v1"
	case "anthropic":
		return "https://api.anthropic.com/v1"
	case "deepseek":
		return "https://api.deepseek.com/v1"
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta"
	case "xai":
		return "https://api.x.ai/v1"
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	case "openai_compatible":
		return "http://localhost:4000/v1"
	default:
		return ""
	}
}

func optionalFloat(value any) *float64 {
	switch v := value.(type) {
	case float64:
		return &v
	case float32:
		n := float64(v)
		return &n
	case int:
		n := float64(v)
		return &n
	case int64:
		n := float64(v)
		return &n
	case json.Number:
		if n, err := v.Float64(); err == nil {
			return &n
		}
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return &n
		}
	}
	return nil
}
