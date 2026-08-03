package nativeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearchUsesRequestScopedTavilyCredentials(t *testing.T) {
	var receivedAuthorization string
	var requestContainedKey bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode Tavily request: %v", err)
		}
		receivedAuthorization = request.Header.Get("Authorization")
		_, requestContainedKey = payload["api_key"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"connected","results":[{"title":"Dirextalk","url":"https://example.com","content":"ok","score":0.9}]}`))
	}))
	defer server.Close()

	runtime := New(Config{WebSearchEndpoint: server.URL, HTTPClient: server.Client()})
	params := map[string]any{
		"tool_credentials": map[string]any{
			"web_search": map[string]any{
				"enabled":  true,
				"provider": "tavily",
				"api_key":  "tvly-request-only",
			},
		},
	}
	result, err := runtime.Invoke(context.Background(), "agent.web_search.test", params)
	if err != nil {
		t.Fatalf("test web search: %v", err)
	}
	if result["ok"] != true || receivedAuthorization != "Bearer tvly-request-only" {
		t.Fatalf("unexpected web search result=%#v authorization=%q", result, receivedAuthorization)
	}
	if requestContainedKey {
		t.Fatal("Tavily API key must not be included in the JSON request body")
	}
	if strings.Contains(jsonValue(result), "tvly-request-only") {
		t.Fatalf("web search response leaked the API key: %#v", result)
	}
	if tools := runtime.requestScopedWebSearchTool(map[string]any{}); len(tools) != 0 {
		t.Fatalf("web search tool must not exist without request credentials: %#v", tools)
	}
	if _, err := runtime.searchTavily(context.Background(), webSearchCredentials{
		Enabled: true, Provider: "tavily", APIKey: "tvly-request-only",
	}, map[string]any{"query": strings.Repeat("a", maxWebSearchQueryRunes+1)}); err == nil || !strings.Contains(err.Error(), "1000") {
		t.Fatalf("expected oversized search query rejection, got %v", err)
	}
}

func TestWebSearchMapsProviderFailuresWithoutLeakingSecrets(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantError  string
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, wantError: "API key was rejected"},
		{name: "forbidden", statusCode: http.StatusForbidden, wantError: "API key was rejected"},
		{name: "rate limited", statusCode: http.StatusTooManyRequests, wantError: "rate limit"},
		{name: "provider failure", statusCode: http.StatusInternalServerError, wantError: "HTTP 500"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(`{"detail":"provider-body-must-not-leak"}`))
			}))
			defer server.Close()

			runtime := New(Config{WebSearchEndpoint: server.URL, HTTPClient: server.Client()})
			_, err := runtime.searchTavily(context.Background(), webSearchCredentials{
				Enabled: true, Provider: "tavily", APIKey: "tvly-must-not-leak",
			}, map[string]any{"query": "connection test"})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected error containing %q, got %v", test.wantError, err)
			}
			for _, secret := range []string{"tvly-must-not-leak", "provider-body-must-not-leak"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("web search error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestWebSearchCredentialSanitizersDropRequestSecrets(t *testing.T) {
	input := map[string]any{
		"tool_credentials": map[string]any{
			"web_search": map[string]any{
				"enabled":  true,
				"provider": "tavily",
				"api_key":  "tvly-secret",
			},
		},
		"model_profiles": []any{map[string]any{"api_key": "model-secret", "model": "gpt"}},
	}
	sanitized := sanitizeConfig(input)
	serialized := jsonValue(sanitized)
	for _, secret := range []string{"tvly-secret", "model-secret"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("sanitized config leaked %q: %#v", secret, sanitized)
		}
	}
	if _, exists := sanitized["tool_credentials"]; exists {
		t.Fatalf("request-scoped credentials must not be persisted: %#v", sanitized)
	}
	if !strings.Contains(serialized, "gpt") {
		t.Fatalf("sanitizer removed non-secret model metadata: %#v", sanitized)
	}
}

func TestWebSearchRejectsRedirectsAndCapsProviderResults(t *testing.T) {
	redirectFollowed := false
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectFollowed = true
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer redirectTarget.Close()
	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, redirectTarget.URL, http.StatusFound)
	}))
	defer redirectSource.Close()

	runtime := New(Config{WebSearchEndpoint: redirectSource.URL, HTTPClient: redirectSource.Client()})
	_, err := runtime.searchTavily(context.Background(), webSearchCredentials{
		Enabled: true, Provider: "tavily", APIKey: "tvly-no-redirect",
	}, map[string]any{"query": "redirect"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("expected redirect rejection, got %v", err)
	}
	if redirectFollowed {
		t.Fatal("web search followed a redirect with request credentials")
	}

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		results := make([]map[string]any, 12)
		for index := range results {
			results[index] = map[string]any{
				"title": "result", "url": fmt.Sprintf("https://example.com/%d", index), "content": "ok",
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer provider.Close()
	runtime = New(Config{WebSearchEndpoint: provider.URL, HTTPClient: provider.Client()})
	result, err := runtime.searchTavily(context.Background(), webSearchCredentials{
		Enabled: true, Provider: "tavily", APIKey: "tvly-cap-results",
	}, map[string]any{"query": "bounded", "max_results": 2})
	if err != nil {
		t.Fatalf("bounded search: %v", err)
	}
	items, _ := result["results"].([]map[string]any)
	if len(items) != 2 {
		t.Fatalf("provider results were not capped: %#v", items)
	}
}
