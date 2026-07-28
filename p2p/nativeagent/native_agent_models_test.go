package nativeagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type modelListProfileResolver struct {
	profile ServerModelProfile
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (r *modelListProfileResolver) ResolveModelProfile(context.Context, string) (ServerModelProfile, error) {
	return r.profile, nil
}

func TestModelsListServerProfileUsesRequestBaseURLAndStoredKey(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"Qwen/Qwen3-32B"}]}`))
	}))
	defer server.Close()

	resolver := &modelListProfileResolver{profile: ServerModelProfile{
		Provider: "openai_compatible",
		BaseURL:  server.URL + "/v1",
		APIKey:   "stored-siliconflow-key",
	}}
	original := resolver.profile
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent"), ModelProfiles: resolver})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"model_profile_id": "profile-siliconflow",
		"provider":         "OPENAI_COMPATIBLE",
		"base_url":         server.URL + "/v1/edited-siliconflow",
	})
	if err != nil {
		t.Fatalf("agent.models.list: %v", err)
	}
	if gotPath != "/v1/edited-siliconflow/models" || gotAuth != "Bearer stored-siliconflow-key" {
		t.Fatalf("unexpected request path=%q authorization=%q", gotPath, gotAuth)
	}
	if models, ok := result["models"].([]map[string]any); !ok || len(models) != 1 || models[0]["id"] != "Qwen/Qwen3-32B" {
		t.Fatalf("unexpected models: %#v", result["models"])
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "stored-siliconflow-key") {
		t.Fatalf("models response must not echo stored API key: %s", encoded)
	}
	if !reflect.DeepEqual(resolver.profile, original) {
		t.Fatalf("models.list must not mutate stored profile: before=%#v after=%#v", original, resolver.profile)
	}
}

func TestModelsListServerProfileRejectsCrossOriginOverrideWithoutRequest(t *testing.T) {
	var hostileRequests int
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostileRequests++
	}))
	defer hostile.Close()
	stored := httptest.NewServer(http.NotFoundHandler())
	defer stored.Close()

	runtime := New(Config{ModelProfiles: &modelListProfileResolver{profile: ServerModelProfile{
		Provider: "openai_compatible",
		BaseURL:  stored.URL + "/v1",
		APIKey:   "stored-key-never-forwarded",
	}}})
	_, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"model_profile_id": "profile-siliconflow",
		"base_url":         hostile.URL + "/v1",
	})
	if err == nil || !strings.Contains(err.Error(), "stored model profile origin") {
		t.Fatalf("expected cross-origin rejection, got %v", err)
	}
	if hostileRequests != 0 {
		t.Fatalf("cross-origin override must not receive a request: %d", hostileRequests)
	}
}

func TestModelsListServerProfileRejectsMalformedOverride(t *testing.T) {
	runtime := New(Config{ModelProfiles: &modelListProfileResolver{profile: ServerModelProfile{
		Provider: "openai_compatible",
		BaseURL:  "https://api.siliconflow.cn/v1",
		APIKey:   "stored-key",
	}}})
	for _, override := range []string{"not a URL", "https://user:pass@api.siliconflow.cn/v1", "https://api.siliconflow.cn:bad/v1"} {
		_, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
			"model_profile_id": "profile-siliconflow",
			"base_url":         override,
		})
		if err == nil || !strings.Contains(err.Error(), "base_url override is invalid") {
			t.Errorf("override %q error = %v, want malformed URL rejection", override, err)
		}
	}
}

func TestModelsListOpenRouterEmbeddingUsesDedicatedEndpoint(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	var genericRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			genericRequests++
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-should-not-appear"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/text-embedding-3-small","name":"Embedding","architecture":{"output_modalities":["embeddings"]}},{"id":"openai/gpt-4o","architecture":{"output_modalities":["text"]}}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider":   "openrouter",
		"base_url":   server.URL + "/v1",
		"api_key":    "openrouter-key",
		"model_kind": "EMBEDDING",
	})
	if err != nil {
		t.Fatalf("agent.models.list embedding: %v", err)
	}
	if gotPath != "/v1/embeddings/models" || gotQuery != "" || gotAuth != "Bearer openrouter-key" {
		t.Fatalf("unexpected embedding request path=%q query=%q authorization=%q", gotPath, gotQuery, gotAuth)
	}
	if genericRequests != 0 {
		t.Fatalf("embedding lookup must not call generic models endpoint")
	}
	models := result["models"].([]map[string]any)
	if len(models) != 1 || models[0]["id"] != "openai/text-embedding-3-small" {
		t.Fatalf("unexpected embedding models: %#v", models)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "openrouter-key") {
		t.Fatalf("models response must not echo API key: %s", encoded)
	}
}

func TestModelsListSiliconFlowConversationUsesChatFilter(t *testing.T) {
	var gotPath, gotQuery string
	var gotAuth string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotPath, gotQuery = req.URL.Path, req.URL.RawQuery
		gotAuth = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"text-embedding-3-small"},{"id":"rerank-v1"},{"id":"tts-1"},{"id":"whisper-1"},{"id":"dall-e-3"},{"id":"Qwen/Qwen3-32B"},{"id":"deepseek-chat"}]}`)),
		}, nil
	})}
	runtime := New(Config{HTTPClient: client})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "openai_compatible",
		"base_url": "https://api.siliconflow.cn/v1",
		"api_key":  "siliconflow-key",
	})
	if err != nil {
		t.Fatalf("agent.models.list: %v", err)
	}
	if gotPath != "/v1/models" || gotQuery != "sub_type=chat&type=text" {
		t.Fatalf("unexpected SiliconFlow request URL path=%q query=%q", gotPath, gotQuery)
	}
	if gotAuth != "Bearer siliconflow-key" {
		t.Fatalf("unexpected SiliconFlow authorization: %q", gotAuth)
	}
	models := result["models"].([]map[string]any)
	if len(models) != 2 || models[0]["id"] != "Qwen/Qwen3-32B" || models[1]["id"] != "deepseek-chat" {
		t.Fatalf("unexpected SiliconFlow conversation models: %#v", models)
	}
}

func TestModelsListOpenRouterConversationFiltersNonTextModels(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/text-embedding-3-small","architecture":{"output_modalities":["embedding"]}},{"id":"openai/gpt-4o","architecture":{"output_modalities":["text"]}}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider":   "openrouter",
		"base_url":   server.URL + "/v1",
		"api_key":    "openrouter-key",
		"model_kind": "conversation",
	})
	if err != nil {
		t.Fatalf("agent.models.list conversation: %v", err)
	}
	if gotQuery != "output_modalities=text" {
		t.Fatalf("expected OpenRouter text-output filter query, got %q", gotQuery)
	}
	models := result["models"].([]map[string]any)
	if len(models) != 1 || models[0]["id"] != "openai/gpt-4o" {
		t.Fatalf("unexpected conversation models: %#v", models)
	}
}

func TestModelsListRejectsUnsupportedKindsBeforeProviderFetch(t *testing.T) {
	for _, testCase := range []struct {
		provider string
		kind     string
	}{
		{provider: "anthropic", kind: "embedding"},
		{provider: "gemini", kind: "embedding"},
		{provider: "anthropic", kind: "speech"},
		{provider: "gemini", kind: "speech"},
	} {
		t.Run(testCase.provider+"/"+testCase.kind, func(t *testing.T) {
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
			}))
			defer server.Close()
			runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
			_, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
				"provider":   testCase.provider,
				"base_url":   server.URL,
				"api_key":    "test-key",
				"model_kind": testCase.kind,
			})
			if err == nil || !strings.Contains(err.Error(), "model list kind") {
				t.Fatalf("expected unsupported kind error, got %v", err)
			}
			if requests != 0 {
				t.Fatalf("unsupported kind must not fetch provider models: %d requests", requests)
			}
		})
	}
}

func TestModelsListServerProfileRejectsProviderMismatch(t *testing.T) {
	runtime := New(Config{ModelProfiles: &modelListProfileResolver{profile: ServerModelProfile{
		Provider: "openai_compatible",
		BaseURL:  "https://api.siliconflow.cn/v1",
		APIKey:   "stored-key",
	}}})
	_, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"model_profile_id": "profile-siliconflow",
		"provider":         "anthropic",
	})
	if err == nil || !strings.Contains(err.Error(), "does not match model profile provider") {
		t.Fatalf("expected provider mismatch error, got %v", err)
	}
}

func TestModelsListServerProfileRejectsRequestAPIKey(t *testing.T) {
	runtime := New(Config{ModelProfiles: &modelListProfileResolver{profile: ServerModelProfile{
		Provider: "openai_compatible",
		BaseURL:  "https://api.siliconflow.cn/v1",
		APIKey:   "stored-key",
	}}})
	_, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"model_profile_id": "profile-siliconflow",
		"api_key":          "attacker-key",
	})
	if err == nil || !strings.Contains(err.Error(), "api_key must not be provided") {
		t.Fatalf("expected request api_key rejection, got %v", err)
	}
}

func TestModelsListFetchesOpenAICompatibleProvider(t *testing.T) {
	var gotPath string
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"provider/model-a","name":"Model A","context_length":131072,"api_key":"test-key","authorization":"Bearer test-key","token":"token-value","secret":"secret-value","metadata":{"api_key":"nested-key"}},{"id":"provider/model-b"}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "openai_compatible",
		"base_url": server.URL,
		"api_key":  "test-key",
	})
	if err != nil {
		t.Fatalf("agent.models.list: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("expected /v1/models request, got %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("expected bearer auth, got %q", gotAuth)
	}
	models, ok := result["models"].([]map[string]any)
	if !ok || len(models) != 2 {
		t.Fatalf("expected two models, got %#v", result["models"])
	}
	if models[0]["id"] != "provider/model-a" || models[0]["name"] != "Model A" || models[0]["context_length"] == nil {
		t.Fatalf("unexpected first model: %#v", models[0])
	}
	if _, ok := models[0]["temperature"]; ok {
		t.Fatalf("models.list must not invent temperature defaults: %#v", models[0])
	}
	if _, ok := models[0]["top_p"]; ok {
		t.Fatalf("models.list must not invent top_p defaults: %#v", models[0])
	}
	for _, key := range []string{"api_key", "authorization", "token", "secret", "metadata"} {
		if _, ok := models[0][key]; ok {
			t.Fatalf("models.list must not expose upstream %s: %#v", key, models[0])
		}
	}
	data, _ := json.Marshal(result)
	for _, secret := range []string{"test-key", "token-value", "secret-value", "nested-key"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("models response must not echo upstream credentials: %s", data)
		}
	}
}

func TestModelsListConversationFiltersExplicitNonConversationKinds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"embed-model","type":"embedding"},{"id":"chat-model","type":"chat"},{"id":"unknown-model"}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider":   "openai_compatible",
		"base_url":   server.URL,
		"api_key":    "test-key",
		"model_kind": "conversation",
	})
	if err != nil {
		t.Fatalf("agent.models.list: %v", err)
	}
	models := result["models"].([]map[string]any)
	if len(models) != 2 || models[0]["id"] != "chat-model" || models[1]["id"] != "unknown-model" {
		t.Fatalf("unexpected conversation models: %#v", models)
	}
}

func TestModelsListDoesNotInventOpenAIMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5"}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "openai",
		"base_url": server.URL,
		"api_key":  "test-key",
	})
	if err != nil {
		t.Fatalf("agent.models.list: %v", err)
	}
	models, ok := result["models"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("expected one model, got %#v", result["models"])
	}
	for _, key := range []string{"temperature", "top_p", "context_length", "max_output_tokens", "reasoning_modes", "reasoning_mode"} {
		if _, ok := models[0][key]; ok {
			t.Fatalf("models.list must not invent %s: %#v", key, models[0])
		}
	}
}

func TestModelsListFetchesAnthropicProvider(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-5","display_name":"Claude Sonnet 4.5","max_input_tokens":200000,"max_tokens":64000}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "anthropic",
		"base_url": server.URL + "/v1",
		"api_key":  "test-key",
	})
	if err != nil {
		t.Fatalf("agent.models.list: %v", err)
	}
	if gotPath != "/v1/models" || gotAPIKey != "test-key" || gotVersion != anthropicVersion {
		t.Fatalf("unexpected Anthropic request path=%q api_key=%q version=%q", gotPath, gotAPIKey, gotVersion)
	}
	models := result["models"].([]map[string]any)
	if len(models) != 1 || models[0]["id"] != "claude-sonnet-4-5" || models[0]["name"] != "Claude Sonnet 4.5" || models[0]["max_input_tokens"] != float64(200000) {
		t.Fatalf("unexpected Anthropic models: %#v", models)
	}
}

func TestModelsListFetchesGeminiNativeProvider(t *testing.T) {
	var gotPath, gotAPIKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-3.6-flash","displayName":"Gemini 3.6 Flash"}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "gemini",
		"base_url": server.URL + "/v1beta",
		"api_key":  "test-key",
	})
	if err != nil {
		t.Fatalf("agent.models.list: %v", err)
	}
	if gotPath != "/v1beta/models" || gotAPIKey != "test-key" {
		t.Fatalf("unexpected Gemini request path=%q api_key=%q", gotPath, gotAPIKey)
	}
	models := result["models"].([]map[string]any)
	if len(models) != 1 || models[0]["id"] != "gemini-3.6-flash" || models[0]["name"] != "Gemini 3.6 Flash" {
		t.Fatalf("unexpected Gemini models: %#v", models)
	}
}

func TestModelsListFetchesGeminiNativeProviderFromCustomHost(t *testing.T) {
	var gotPath, gotGoogleKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotGoogleKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-custom","displayName":"Gemini Custom"}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "gemini",
		"base_url": server.URL,
		"api_key":  "test-key",
	})
	if err != nil {
		t.Fatalf("agent.models.list: %v", err)
	}
	if gotPath != "/v1beta/models" || gotGoogleKey != "test-key" {
		t.Fatalf("unexpected custom Gemini request path=%q google_key=%q", gotPath, gotGoogleKey)
	}
	models := result["models"].([]map[string]any)
	if len(models) != 1 || models[0]["id"] != "gemini-custom" {
		t.Fatalf("unexpected custom Gemini models: %#v", models)
	}
}

func TestGeminiModelProfileDoesNotDefaultRequiredFields(t *testing.T) {
	profile := New(Config{}).resolveModelProfile(map[string]any{
		"model_profile": map[string]any{"provider": "gemini"},
	})
	if profile.Model != "" || profile.BaseURL != "" {
		t.Fatalf("Gemini profile must not receive server defaults: %#v", profile)
	}
}

func TestModelsListFetchesXAIProvider(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.5","context_length":256000,"owned_by":"xai"}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "xai",
		"base_url": server.URL + "/v1",
		"api_key":  "test-key",
	})
	if err != nil {
		t.Fatalf("agent.models.list: %v", err)
	}
	if gotPath != "/v1/models" || gotAuth != "Bearer test-key" {
		t.Fatalf("unexpected xAI request path=%q authorization=%q", gotPath, gotAuth)
	}
	models := result["models"].([]map[string]any)
	if len(models) != 1 || models[0]["id"] != "grok-4.5" || models[0]["name"] != "grok-4.5" || models[0]["context_length"] != float64(256000) {
		t.Fatalf("unexpected xAI models: %#v", models)
	}
}

func TestXAIModelProfileDoesNotDefaultRequiredFields(t *testing.T) {
	profile := New(Config{}).resolveModelProfile(map[string]any{
		"model_profile": map[string]any{"provider": "xai"},
	})
	if profile.Model != "" || profile.BaseURL != "" {
		t.Fatalf("xAI profile must not receive server defaults: %#v", profile)
	}
}

func TestChatRequiresExplicitModelProfileFields(t *testing.T) {
	testCases := []struct {
		name    string
		profile map[string]any
		want    string
	}{
		{name: "provider", profile: map[string]any{"model": "model-a", "base_url": "https://models.example/v1", "api_key": "test-key"}, want: "model_profile.provider is required"},
		{name: "model", profile: map[string]any{"provider": "openai_compatible", "base_url": "https://models.example/v1", "api_key": "test-key"}, want: "model_profile.model is required; select a model"},
		{name: "base url", profile: map[string]any{"provider": "openai_compatible", "model": "model-a", "api_key": "test-key"}, want: "model_profile.base_url is required; configure the model API address"},
		{name: "api key", profile: map[string]any{"provider": "openai_compatible", "model": "model-a", "base_url": "https://models.example/v1"}, want: "model_profile.api_key is required"},
	}
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
				"prompt":        "hello",
				"model_profile": testCase.profile,
			})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("agent.chat error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestRemovedModelProvidersAreRejected(t *testing.T) {
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	for _, provider := range []string{"litellm", "vertex"} {
		t.Run(provider, func(t *testing.T) {
			profile := map[string]any{
				"provider": provider,
				"model":    "test-model",
				"base_url": "https://models.example/v1",
				"api_key":  "test-key",
			}
			_, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
				"prompt":        "hello",
				"model_profile": profile,
			})
			if err == nil || err.Error() != "model_profile.provider is not supported" {
				t.Fatalf("agent.chat provider %q error = %v", provider, err)
			}
			_, err = runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
				"provider": provider,
				"base_url": profile["base_url"],
				"api_key":  profile["api_key"],
			})
			if err == nil || !strings.Contains(err.Error(), "model list is not supported") {
				t.Fatalf("agent.models.list provider %q error = %v", provider, err)
			}
		})
	}
}

func TestChatStreamRequiresExplicitModelAndBaseURL(t *testing.T) {
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	var events []Event
	err := runtime.Stream(context.Background(), "agent.chat.stream", map[string]any{
		"prompt": "hello",
		"model_profile": map[string]any{
			"provider": "xai",
			"api_key":  "test-key",
		},
	}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("agent.chat.stream: %v", err)
	}
	if len(events) != 2 || events[0].Event != "error" || events[0].Data["error"] != "model_profile.model is required; select a model" || events[1].Event != "done" || events[1].Data["ok"] != false {
		t.Fatalf("agent.chat.stream events = %#v", events)
	}
}

func TestModelsListRequiresAPIKeyForDynamicFetch(t *testing.T) {
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	_, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
	})
	if err == nil || !strings.Contains(err.Error(), "api_key is required") {
		t.Fatalf("expected api_key required error, got %v", err)
	}
}

func TestModelsListRequiresExplicitBaseURL(t *testing.T) {
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	_, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "xai",
		"api_key":  "test-key",
	})
	if err == nil || !strings.Contains(err.Error(), "base_url is required") {
		t.Fatalf("expected base_url required error, got %v", err)
	}
}

func TestModelsListWithoutProviderReturnsMetadataOnly(t *testing.T) {
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.models.list", map[string]any{})
	if err != nil {
		t.Fatalf("agent.models.list metadata: %v", err)
	}
	models, ok := result["models"].([]map[string]any)
	if !ok || len(models) != 0 {
		t.Fatalf("expected empty models without provider, got %#v", result["models"])
	}
	providers, ok := result["providers"].([]map[string]any)
	if !ok || len(providers) == 0 {
		t.Fatalf("expected provider metadata, got %#v", result["providers"])
	}
	for _, provider := range providers {
		if provider["provider"] == "litellm" || provider["provider"] == "vertex" {
			t.Fatalf("removed provider leaked through metadata: %#v", provider)
		}
	}
}
