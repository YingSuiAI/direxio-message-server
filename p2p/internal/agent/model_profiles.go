package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func (m *Module) modelProfileSync(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.modelProfiles == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "server model profiles are unavailable")
	}
	for key := range params {
		if key != "idempotency_key" && key != "default_client_profile_id" && key != "default_conversation_client_profile_id" && key != "default_embedding_client_profile_id" && key != "default_speech_client_profile_id" && key != "entries" {
			return nil, actionbase.BadRequest("unknown model profile sync field: " + key)
		}
	}
	idempotency, err := requiredProfileString(params, "idempotency_key")
	if err != nil {
		return nil, err
	}
	legacyDefault, err := optionalProfileString(params, "default_client_profile_id")
	if err != nil {
		return nil, err
	}
	conversationDefault, err := optionalProfileString(params, "default_conversation_client_profile_id")
	if err != nil {
		return nil, err
	}
	if legacyDefault != "" && conversationDefault != "" && legacyDefault != conversationDefault {
		return nil, actionbase.BadRequest("default_client_profile_id conflicts with default_conversation_client_profile_id")
	}
	if conversationDefault == "" {
		conversationDefault = legacyDefault
	}
	embeddingDefault, err := optionalProfileString(params, "default_embedding_client_profile_id")
	if err != nil {
		return nil, err
	}
	speechDefault, err := optionalProfileString(params, "default_speech_client_profile_id")
	if err != nil {
		return nil, err
	}
	raw, ok := params["entries"].([]any)
	if !ok {
		if typed, ok2 := params["entries"].([]map[string]any); ok2 {
			raw = make([]any, len(typed))
			for i := range typed {
				raw[i] = typed[i]
			}
			ok = true
		}
	}
	if !ok {
		return nil, actionbase.BadRequest("entries must be an array")
	}
	entries := make([]storage.ModelProfileSyncEntry, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, actionbase.BadRequest("entries must contain objects")
		}
		entry, e := parseProfileEntry(m)
		if e != nil {
			return nil, e
		}
		entries = append(entries, entry)
	}
	result, storeErr := m.modelProfiles.SyncModelProfilesWithDefaults(ctx, m.currentOwnerID(), idempotency, storage.ModelProfileDefaults{ConversationClientProfileID: conversationDefault, EmbeddingClientProfileID: embeddingDefault, SpeechClientProfileID: speechDefault}, entries)
	if storeErr != nil {
		return nil, modelProfileStoreError(storeErr)
	}
	return map[string]any{"profiles": profileMaps(result.Profiles), "default_client_profile_id": result.DefaultClientProfileID, "default_conversation_client_profile_id": result.Defaults.ConversationClientProfileID, "default_embedding_client_profile_id": result.Defaults.EmbeddingClientProfileID, "default_speech_client_profile_id": result.Defaults.SpeechClientProfileID}, nil
}

func (m *Module) modelProfileList(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.modelProfiles == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "server model profiles are unavailable")
	}
	for key := range params {
		if key != "page_size" && key != "page_token" {
			return nil, actionbase.BadRequest("unknown model profile list field: " + key)
		}
	}
	pageSize := 0
	if raw, ok := params["page_size"]; ok {
		value, parseErr := strictProfileInt64(raw)
		if parseErr != nil || value < 0 || value > int64(^uint(0)>>1) {
			return nil, actionbase.BadRequest("page_size must be a non-negative integer")
		}
		pageSize = int(value)
	}
	token, err := optionalProfileValue(params, "page_token")
	if err != nil {
		return nil, err
	}
	result, storeErr := m.modelProfiles.ListModelProfiles(ctx, m.currentOwnerID(), pageSize, token)
	if storeErr != nil {
		return nil, modelProfileStoreError(storeErr)
	}
	return map[string]any{"profiles": profileMaps(result.Profiles), "next_page_token": result.NextPageToken, "default_client_profile_id": result.DefaultClientProfileID, "default_conversation_client_profile_id": result.Defaults.ConversationClientProfileID, "default_embedding_client_profile_id": result.Defaults.EmbeddingClientProfileID, "default_speech_client_profile_id": result.Defaults.SpeechClientProfileID}, nil
}
func (m *Module) modelProfileGet(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.modelProfiles == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "server model profiles are unavailable")
	}
	for key := range params {
		if key != "profile_id" {
			return nil, actionbase.BadRequest("unknown model profile get field: " + key)
		}
	}
	id, e := requiredProfileString(params, "profile_id")
	if e != nil {
		return nil, e
	}
	p, ok, err := m.modelProfiles.GetModelProfile(ctx, m.currentOwnerID(), id)
	if err != nil {
		return nil, modelProfileStoreError(err)
	}
	if !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "model profile not found")
	}
	return map[string]any{"profile": profileMap(p)}, nil
}
func (m *Module) modelProfileDelete(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.modelProfiles == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "server model profiles are unavailable")
	}
	for key := range params {
		if key != "idempotency_key" && key != "profile_id" && key != "expected_revision" {
			return nil, actionbase.BadRequest("unknown model profile delete field: " + key)
		}
	}
	idempotency, e := requiredProfileString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredProfileString(params, "profile_id")
	if e != nil {
		return nil, e
	}
	var rev *int64
	if _, ok := params["expected_revision"]; ok {
		v, parseErr := strictProfileInt64(params["expected_revision"])
		if parseErr != nil || v < 0 {
			return nil, actionbase.BadRequest("expected_revision must be an integer")
		}
		rev = &v
	}
	if err := m.modelProfiles.DeleteModelProfile(ctx, m.currentOwnerID(), idempotency, id, rev); err != nil {
		return nil, modelProfileStoreError(err)
	}
	return map[string]any{"deleted": true, "profile_id": id}, nil
}

func requiredProfileString(params map[string]any, key string) (string, *actionbase.Error) {
	value, err := requiredProfileValue(params, key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", actionbase.BadRequest(key + " is required")
	}
	if value != strings.TrimSpace(value) {
		return "", actionbase.BadRequest(key + " must not have surrounding whitespace")
	}
	return value, nil
}
func optionalProfileString(params map[string]any, key string) (string, *actionbase.Error) {
	value, ok := params[key]
	if !ok {
		return "", nil
	}
	s, ok := value.(string)
	if !ok || strings.TrimSpace(s) == "" || s != strings.TrimSpace(s) {
		return "", actionbase.BadRequest(key + " must be a non-empty canonical string")
	}
	return s, nil
}

// optionalProfileValue preserves the existing missing/empty semantics while
// rejecting JSON values that are not strings.  action.Params.String is
// intentionally permissive for legacy actions, but model-profile DTOs must
// not coerce a malformed value into an empty field and clear stored data.
func optionalProfileValue(params map[string]any, key string) (string, *actionbase.Error) {
	value, ok := params[key]
	if !ok {
		return "", nil
	}
	s, ok := value.(string)
	if !ok {
		return "", actionbase.BadRequest(key + " must be a string")
	}
	return strings.TrimSpace(s), nil
}

func requiredProfileValue(params map[string]any, key string) (string, *actionbase.Error) {
	value, ok := params[key]
	if !ok {
		return "", actionbase.BadRequest(key + " is required")
	}
	s, ok := value.(string)
	if !ok {
		return "", actionbase.BadRequest(key + " must be a string")
	}
	return s, nil
}
func parseProfileEntry(raw map[string]any) (storage.ModelProfileSyncEntry, *actionbase.Error) {
	known := map[string]bool{"client_profile_id": true, "expected_revision": true, "display_name": true, "provider": true, "base_url": true, "model": true, "system_prompt": true, "api_key": true, "temperature": true, "top_p": true, "max_output_tokens": true, "context_window": true, "reasoning_effort": true, "model_kind": true, "input_modalities": true, "provider_config": true, "provider_secrets": true}
	for key := range raw {
		if !known[key] {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("unknown model profile field: " + key)
		}
	}
	client, e := requiredProfileString(raw, "client_profile_id")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	provider, e := requiredProfileString(raw, "provider")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	provider = strings.ToLower(provider)
	switch provider {
	case "openai", "anthropic", "deepseek", "gemini", "xai", "openai_compatible", "openrouter", "volc_voice":
	default:
		return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("model profile provider is not supported")
	}
	displayName, e := optionalProfileValue(raw, "display_name")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	baseURL, e := optionalProfileValue(raw, "base_url")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	model, e := optionalProfileValue(raw, "model")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	systemPrompt, e := optionalProfileValue(raw, "system_prompt")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	reasoningEffort, e := optionalProfileValue(raw, "reasoning_effort")
	if e != nil {
		return storage.ModelProfileSyncEntry{}, e
	}
	entry := storage.ModelProfileSyncEntry{ClientProfileID: client, Provider: provider, DisplayName: displayName, BaseURL: baseURL, Model: model, SystemPrompt: systemPrompt, ReasoningEffort: reasoningEffort}
	if rawKind, ok := raw["model_kind"]; ok {
		kind, ok := rawKind.(string)
		if !ok {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("model_kind must be a string")
		}
		entry.ModelKind = strings.TrimSpace(kind)
	}
	if rawModalities, ok := raw["input_modalities"]; ok {
		values, ok := rawModalities.([]any)
		if typed, typedOK := rawModalities.([]string); typedOK {
			values = make([]any, len(typed))
			for i := range typed {
				values[i] = typed[i]
			}
			ok = true
		}
		if !ok {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("input_modalities must be an array")
		}
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("input_modalities must contain strings")
			}
			entry.InputModalities = append(entry.InputModalities, text)
		}
	}
	if rawConfig, ok := raw["provider_config"]; ok {
		config, ok := rawConfig.(map[string]any)
		if !ok {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("provider_config must be an object")
		}
		entry.ProviderConfig = config
	}
	if rawSecrets, ok := raw["provider_secrets"]; ok {
		secrets, ok := rawSecrets.(map[string]any)
		if !ok {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("provider_secrets must be an object")
		}
		entry.ProviderSecrets = map[string]string{}
		for key, value := range secrets {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("provider_secrets values must be non-empty strings")
			}
			entry.ProviderSecrets[key] = text
		}
	}
	if provider == "volc_voice" && entry.ModelKind == "" {
		entry.ModelKind = storage.ModelKindSpeech
	}
	if provider == "volc_voice" {
		for _, key := range []string{"api_key", "base_url", "model", "system_prompt", "temperature", "top_p", "max_output_tokens", "context_window", "reasoning_effort"} {
			if _, present := raw[key]; present {
				return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("speech profiles do not accept " + key)
			}
		}
	}
	if v, ok := raw["max_output_tokens"]; ok {
		n, parseErr := strictProfileInt64(v)
		if parseErr != nil || n < 0 {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("max_output_tokens must be an integer")
		}
		entry.MaxOutputTokens = int(n)
	}
	if v, ok := raw["context_window"]; ok {
		n, parseErr := strictProfileInt64(v)
		if parseErr != nil || n < 0 {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("context_window must be an integer")
		}
		entry.ContextWindow = int(n)
	}
	if v, ok := raw["api_key"]; ok {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("api_key must be non-empty when present")
		}
		entry.APIKey = &s
	}
	if v, ok := raw["expected_revision"]; ok {
		x, parseErr := strictProfileInt64(v)
		if parseErr != nil || x < 0 {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("expected_revision must be an integer")
		}
		entry.ExpectedRevision = &x
	}
	if v, ok := raw["temperature"]; ok {
		x, parseErr := strictProfileFloat64(v)
		if parseErr != nil {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("temperature must be a finite number")
		}
		entry.Temperature = &x
	}
	if v, ok := raw["top_p"]; ok {
		x, parseErr := strictProfileFloat64(v)
		if parseErr != nil {
			return storage.ModelProfileSyncEntry{}, actionbase.BadRequest("top_p must be a finite number")
		}
		entry.TopP = &x
	}
	return entry, nil
}

func strictProfileInt64(value any) (int64, error) {
	switch v := value.(type) {
	case int:
		return int64(v), nil
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case uint:
		if uint64(v) > math.MaxInt64 {
			return 0, errors.New("integer overflow")
		}
		return int64(v), nil
	case uint8:
		return int64(v), nil
	case uint16:
		return int64(v), nil
	case uint32:
		return int64(v), nil
	case uint64:
		if v > math.MaxInt64 {
			return 0, errors.New("integer overflow")
		}
		return int64(v), nil
	case json.Number:
		n, err := v.Int64()
		return n, err
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < math.MinInt64 || v > math.MaxInt64 {
			return 0, errors.New("invalid integer")
		}
		return int64(v), nil
	case float32:
		return strictProfileInt64(float64(v))
	default:
		return 0, errors.New("invalid integer type")
	}
}

func strictProfileFloat64(value any) (float64, error) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, errors.New("non-finite number")
		}
		return v, nil
	case float32:
		return strictProfileFloat64(float64(v))
	case int:
		return float64(v), nil
	case int8:
		return float64(v), nil
	case int16:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case uint:
		return float64(v), nil
	case uint8:
		return float64(v), nil
	case uint16:
		return float64(v), nil
	case uint32:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	case json.Number:
		n, err := v.Float64()
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, errors.New("non-finite number")
		}
		return n, nil
	default:
		return 0, errors.New("invalid number type")
	}
}
func profileMap(p storage.ModelProfile) map[string]any {
	return map[string]any{"profile_id": p.ProfileID, "client_profile_id": p.ClientProfileID, "display_name": p.DisplayName, "provider": p.Provider, "model_kind": p.ModelKind, "input_modalities": p.InputModalities, "provider_config": p.ProviderConfig, "provider_secret_status": p.ProviderSecretStatus, "base_url": p.BaseURL, "model": p.Model, "system_prompt": p.SystemPrompt, "api_key_configured": p.APIKeyConfigured, "temperature": p.Temperature, "top_p": p.TopP, "max_output_tokens": p.MaxOutputTokens, "context_window": p.ContextWindow, "reasoning_effort": p.ReasoningEffort, "revision": p.Revision, "credential_version": p.CredentialVersion, "created_at": p.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), "updated_at": p.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")}
}
func profileMaps(profiles []storage.ModelProfile) []map[string]any {
	out := make([]map[string]any, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, profileMap(p))
	}
	return out
}
func modelProfileStoreError(err error) *actionbase.Error {
	switch {
	case errors.Is(err, storage.ErrModelProfileNotFound):
		return actionbase.StatusError(http.StatusNotFound, "model profile not found")
	case errors.Is(err, storage.ErrModelProfileRevision), errors.Is(err, storage.ErrModelProfileIdempotency):
		return actionbase.StatusError(http.StatusConflict, "model profile revision conflict")
	case errors.Is(err, storage.ErrModelProfileInvalid):
		return actionbase.BadRequest("invalid model profile")
	case errors.Is(err, storage.ErrModelProfileKeyUnavailable):
		return actionbase.StatusError(http.StatusServiceUnavailable, "server model profiles are unavailable")
	default:
		return actionbase.StatusError(http.StatusServiceUnavailable, "server model profiles are unavailable")
	}
}
