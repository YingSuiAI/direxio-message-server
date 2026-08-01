package agentembedded

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func (m *Module) backendsGet(_ context.Context, _ map[string]any) (any, *actionbase.Error) {
	capabilities := []string{"planning.skills"}
	if m != nil && m.cfg.ModelProfiles != nil && m.cfg.ModelProfiles.ModelProfileStoreReady() && m.capabilityReady("model_profiles.server", true) {
		capabilities = append(capabilities, "model_profiles.server", "model_roles.server")
	}
	if m != nil && m.cfg.CapabilityReady != nil && m.capabilityReady("memory.server", true) {
		capabilities = append(capabilities, "memory.server")
	}
	if m != nil && m.cfg.CapabilityReady != nil && m.capabilityReady("voice.server", true) {
		capabilities = append(capabilities, "voice.server")
	}
	if m != nil && m.capabilityReady("schedules.server", m.cfg.Schedules != nil && m.cfg.ScheduleTrigger != nil) {
		capabilities = append(capabilities, "schedules.server")
	}
	if m != nil && m.capabilityReady("task", m.cfg.Tasks != nil) {
		capabilities = append(capabilities, "task")
	}
	if m != nil && m.capabilityReady("confirmation", m.cfg.Confirmations != nil) {
		capabilities = append(capabilities, "confirmation")
	}
	if m != nil && m.capabilityReady("mcp", m.cfg.MCP != nil) {
		capabilities = append(capabilities, "mcp")
	}
	if m != nil && m.capabilityReady("aws.control", m.cfg.AWS != nil) {
		capabilities = append(capabilities, "aws.control")
	}
	if m != nil && m.capabilityReady("execution.v2", m.cfg.ExecutionV2 != nil) {
		capabilities = append(capabilities, "execution.v2")
		for _, token := range []string{
			"execution.v2.plan",
			"execution.v2.secrets",
			"execution.v2.observe",
			"execution.v2.run",
			"execution.v2.provision",
			"execution.v2.bindings",
			"execution.v2.transport.aws_ssm",
			"execution.v2.transport.http_api",
		} {
			dependency := true
			if token == "execution.v2.plan" {
				dependency = m.cfg.ExecutionV2PlanReady
			}
			if m.capabilityReady(token, dependency) {
				capabilities = append(capabilities, token)
			}
		}
	}
	return map[string]any{
		"embedded": map[string]any{"available": true, "configured": true, "status": "ready", "capabilities": capabilities},
		"core":     map[string]any{"configured": false, "status": "not_configured", "instance_id": "", "api_version": "", "capabilities": []string{}, "supported_model_providers": []string{}},
	}, nil
}

func (m *Module) statusGet(_ context.Context, _ map[string]any) (any, *actionbase.Error) {
	return map[string]any{"configured": false, "status": "not_configured", "instance_id": "", "api_version": "", "capabilities": []string{}, "supported_model_providers": []string{}}, nil
}

func (m *Module) modelHandler(action string) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		if _, e := m.requireCapability(ctx, params, "model_profiles.server", m.cfg.ModelProfiles != nil); e != nil {
			return nil, e
		}
		if m == nil || m.cfg.ModelProfiles == nil {
			return unavailable(ctx, params)
		}
		owner := m.owner()
		switch action[strings.LastIndex(action, ".")+1:] {
		case "sync":
			return m.modelSync(ctx, owner, params)
		case "list":
			size, token, e := page(params)
			if e != nil {
				return nil, e
			}
			out, err := m.cfg.ModelProfiles.ListModelProfiles(ctx, owner, size, token)
			if err != nil {
				return nil, modelError(err)
			}
			items := make([]any, 0, len(out.Profiles))
			for _, p := range out.Profiles {
				items = append(items, profileMap(p))
			}
			return modelProfileResult(items, out.NextPageToken, out.DefaultClientProfileID, out.Defaults), nil
		case "get":
			id, e := requiredString(params, "profile_id")
			if e != nil {
				return nil, e
			}
			p, found, err := m.cfg.ModelProfiles.GetModelProfile(ctx, owner, id)
			if err != nil {
				return nil, modelError(err)
			}
			if !found {
				return nil, actionbase.CodedError(http.StatusNotFound, "model_profile_not_found", "model profile was not found")
			}
			return map[string]any{"profile": profileMap(p)}, nil
		case "delete":
			id, e := requiredString(params, "profile_id")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(params, "idempotency_key")
			if e != nil {
				return nil, e
			}
			rev, e := optionalInt64(params, "expected_revision")
			if e != nil {
				return nil, e
			}
			if err := m.cfg.ModelProfiles.DeleteModelProfile(ctx, owner, key, id, ptrInt64(rev, params, "expected_revision")); err != nil {
				return nil, modelError(err)
			}
			return map[string]any{"deleted": true, "profile_id": id}, nil
		default:
			return unavailable(ctx, params)
		}
	}
}

func (m *Module) modelSync(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	for key := range params {
		switch key {
		case "idempotency_key", "default_client_profile_id", "default_conversation_client_profile_id",
			"default_embedding_client_profile_id", "default_speech_client_profile_id", "entries":
		default:
			return nil, actionbase.BadRequest("unknown model profile sync field: " + key)
		}
	}
	idempotency, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	raw, ok := params["entries"].([]any)
	if !ok {
		if raw2, ok2 := params["profiles"].([]any); ok2 {
			raw = raw2
		} else if raw2, ok2 := params["model_profiles"].([]any); ok2 {
			raw = raw2
		} else if typed, ok2 := params["entries"].([]map[string]any); ok2 {
			raw = make([]any, len(typed))
			for i := range typed {
				raw[i] = typed[i]
			}
		} else {
			return nil, actionbase.BadRequest("profiles must be an array")
		}
	}
	entries := make([]storage.ModelProfileSyncEntry, 0, len(raw))
	for _, item := range raw {
		v, ok := item.(map[string]any)
		if !ok {
			return nil, actionbase.BadRequest("profiles must contain objects")
		}
		entry, e := parseModelProfileEntry(v)
		if e != nil {
			return nil, e
		}
		entries = append(entries, entry)
	}
	legacyDefault, e := optionalString(params, "default_client_profile_id")
	if e != nil {
		return nil, e
	}
	conversationDefault, e := optionalString(params, "default_conversation_client_profile_id")
	if e != nil {
		return nil, e
	}
	if legacyDefault != "" && conversationDefault != "" && legacyDefault != conversationDefault {
		return nil, actionbase.BadRequest("default_client_profile_id conflicts with default_conversation_client_profile_id")
	}
	if conversationDefault == "" {
		conversationDefault = legacyDefault
	}
	embeddingDefault, e := optionalString(params, "default_embedding_client_profile_id")
	if e != nil {
		return nil, e
	}
	speechDefault, e := optionalString(params, "default_speech_client_profile_id")
	if e != nil {
		return nil, e
	}
	defaults := storage.ModelProfileDefaults{
		ConversationClientProfileID: conversationDefault,
		EmbeddingClientProfileID:    embeddingDefault,
		SpeechClientProfileID:       speechDefault,
	}
	result, err := m.cfg.ModelProfiles.SyncModelProfilesWithDefaults(ctx, owner, idempotency, defaults, entries)
	if err != nil {
		return nil, modelError(err)
	}
	profiles := make([]any, 0, len(result.Profiles))
	for _, p := range result.Profiles {
		profiles = append(profiles, profileMap(p))
	}
	return modelProfileResult(profiles, "", result.DefaultClientProfileID, result.Defaults), nil
}

func profileMap(p storage.ModelProfile) map[string]any {
	hint := p.APIKeyHint
	if hint == "" && p.APIKey != "" {
		hint = storage.ModelProfileAPIKeyHint(p.ModelKind, p.APIKey)
	}
	result := map[string]any{
		"profile_id": p.ProfileID, "client_profile_id": p.ClientProfileID,
		"display_name": p.DisplayName, "provider": p.Provider,
		"model_kind": p.ModelKind, "input_modalities": p.InputModalities,
		"provider_config": p.ProviderConfig, "provider_secret_status": p.ProviderSecretStatus,
		"base_url": p.BaseURL, "model": p.Model, "system_prompt": p.SystemPrompt,
		"api_key_configured": p.APIKeyConfigured, "temperature": p.Temperature,
		"top_p": p.TopP, "max_output_tokens": p.MaxOutputTokens,
		"context_window": p.ContextWindow, "reasoning_effort": p.ReasoningEffort,
		"revision": p.Revision, "credential_version": p.CredentialVersion,
		"created_at": p.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		"updated_at": p.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
	if hint != "" {
		result["api_key_hint"] = hint
	}
	return result
}

func modelProfileResult(profiles []any, nextPageToken, legacyDefault string, defaults storage.ModelProfileDefaults) map[string]any {
	return map[string]any{
		"profiles": profiles, "next_page_token": nextPageToken,
		"default_client_profile_id":              legacyDefault,
		"default_conversation_client_profile_id": defaults.ConversationClientProfileID,
		"default_embedding_client_profile_id":    defaults.EmbeddingClientProfileID,
		"default_speech_client_profile_id":       defaults.SpeechClientProfileID,
	}
}

func parseModelProfileEntry(v map[string]any) (storage.ModelProfileSyncEntry, *actionbase.Error) {
	known := map[string]bool{
		"client_profile_id": true, "expected_revision": true, "display_name": true,
		"provider": true, "base_url": true, "model": true, "system_prompt": true,
		"api_key": true, "temperature": true, "top_p": true, "max_output_tokens": true,
		"context_window": true, "reasoning_effort": true, "model_kind": true,
		"input_modalities": true, "provider_config": true, "provider_secrets": true,
	}
	for key := range v {
		if !known[key] {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("unknown model profile field: " + key)
		}
	}
	clientID, e := requiredString(v, "client_profile_id")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	provider, e := requiredString(v, "provider")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	entry := storage.ModelProfileSyncEntry{ClientProfileID: clientID, Provider: strings.ToLower(provider)}
	switch entry.Provider {
	case "openai", "anthropic", "deepseek", "gemini", "xai", "openai_compatible", "openrouter", "volc_voice":
	default:
		return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("model profile provider is not supported")
	}
	for key, target := range map[string]*string{
		"display_name": &entry.DisplayName, "base_url": &entry.BaseURL,
		"model": &entry.Model, "system_prompt": &entry.SystemPrompt,
		"reasoning_effort": &entry.ReasoningEffort, "model_kind": &entry.ModelKind,
	} {
		*target, e = optionalString(v, key)
		if e != nil {
			return storage.ModelProfileSyncEntry{}, e
		}
	}
	entry.ModelKind = strings.TrimSpace(entry.ModelKind)
	if entry.Provider == "volc_voice" && entry.ModelKind == "" {
		entry.ModelKind = storage.ModelKindSpeech
	}
	if entry.Provider == "volc_voice" {
		for _, key := range []string{"api_key", "base_url", "model", "system_prompt", "temperature", "top_p", "max_output_tokens", "context_window", "reasoning_effort"} {
			if _, present := v[key]; present {
				return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("speech profiles do not accept " + key)
			}
		}
	}
	if raw, ok := v["input_modalities"]; ok {
		values, valid := modelStringSlice(raw)
		if !valid {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("input_modalities must contain strings")
		}
		entry.InputModalities = values
	}
	if raw, ok := v["provider_config"]; ok {
		config, ok := raw.(map[string]any)
		if !ok {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("provider_config must be an object")
		}
		entry.ProviderConfig = config
	}
	if raw, ok := v["provider_secrets"]; ok {
		secrets, ok := raw.(map[string]any)
		if !ok {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("provider_secrets must be an object")
		}
		entry.ProviderSecrets = map[string]string{}
		for key, rawValue := range secrets {
			value, ok := rawValue.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("provider_secrets values must be non-empty strings")
			}
			entry.ProviderSecrets[key] = strings.TrimSpace(value)
		}
	}
	for key, target := range map[string]*int{"max_output_tokens": &entry.MaxOutputTokens, "context_window": &entry.ContextWindow} {
		if _, ok := v[key]; ok {
			value, intErr := optionalInt64(v, key)
			if intErr != nil || value < 0 {
				return storage.ModelProfileSyncEntry{}, actionbase.BadRequest(key + " must be a non-negative integer")
			}
			*target = int(value)
		}
	}
	if raw, ok := v["api_key"]; ok {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("api_key must be non-empty when present")
		}
		value = strings.TrimSpace(value)
		entry.APIKey = &value
	}
	if _, ok := v["expected_revision"]; ok {
		value, intErr := optionalInt64(v, "expected_revision")
		if intErr != nil || value < 0 {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("expected_revision must be a non-negative integer")
		}
		entry.ExpectedRevision = &value
	}
	entry.Temperature, e = optionalModelFloat(v, "temperature")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	entry.TopP, e = optionalModelFloat(v, "top_p")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	return entry, nil
}

func modelStringSlice(raw any) ([]string, bool) {
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...), true
	case []any:
		result := make([]string, 0, len(values))
		for _, rawValue := range values {
			value, ok := rawValue.(string)
			if !ok {
				return nil, false
			}
			result = append(result, value)
		}
		return result, true
	default:
		return nil, false
	}
}

func optionalModelFloat(values map[string]any, key string) (*float64, *actionbase.Error) {
	raw, ok := values[key]
	if !ok {
		return nil, nil
	}
	var value float64
	switch number := raw.(type) {
	case float64:
		value = number
	case float32:
		value = float64(number)
	case int:
		value = float64(number)
	case int64:
		value = float64(number)
	case json.Number:
		parsed, err := number.Float64()
		if err != nil {
			return nil, actionbase.BadRequest(key + " must be a finite number")
		}
		value = parsed
	default:
		return nil, actionbase.BadRequest(key + " must be a finite number")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, actionbase.BadRequest(key + " must be a finite number")
	}
	return &value, nil
}

func modelError(err error) *actionbase.Error {
	if errorsIs(err, storage.ErrModelProfileNotFound) {
		return actionbase.CodedError(http.StatusNotFound, "model_profile_not_found", "model profile was not found")
	}
	if errorsIs(err, storage.ErrModelProfileRevision) {
		return actionbase.CodedError(http.StatusConflict, "model_profile_revision_conflict", "model profile revision conflict")
	}
	return actionbase.InternalError(err)
}
