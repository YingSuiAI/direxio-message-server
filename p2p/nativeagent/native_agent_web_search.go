package nativeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTavilySearchEndpoint = "https://api.tavily.com/search"
	maxWebSearchResponseBytes   = 2 << 20
	maxWebSearchQueryRunes      = 1000
	maxWebSearchResults         = 10
	webSearchTimeout            = 15 * time.Second
)

// requestScopedWebSearchTool publishes the built-in search tool only when a
// valid credential was supplied for this request. The returned closure keeps
// the credential in the request's call stack and never stores it on Runtime.
func (r *Runtime) requestScopedWebSearchTool(params map[string]any) []Tool {
	credentials := toolCredentialsFromParams(params)
	if credentials.validate() != nil {
		return nil
	}
	return []Tool{{
		Name:        "web_search",
		Description: "Search the public web for current information. Use this for recent facts, news, weather, schedules, prices, or sources that may have changed.",
		Parameters: map[string]any{
			"type":     "object",
			"required": []any{"query"},
			"properties": map[string]any{
				"query":       stringSchema(),
				"max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": maxWebSearchResults},
			},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return r.searchTavily(ctx, credentials, args)
		},
	}}
}

func (r *Runtime) testWebSearch(ctx context.Context, params map[string]any) (map[string]any, error) {
	credentials := toolCredentialsFromParams(params)
	if err := credentials.validate(); err != nil {
		return nil, err
	}
	result, err := r.searchTavily(ctx, credentials, map[string]any{
		"query":       "Dirextalk connection test",
		"max_results": 1,
	})
	if err != nil {
		return nil, err
	}
	results, _ := result["results"].([]map[string]any)
	return map[string]any{
		"ok":           true,
		"provider":     "tavily",
		"result_count": len(results),
	}, nil
}

// searchTavily performs one bounded Tavily request with request-scoped Bearer
// credentials. It returns sanitized snippets and never exposes provider
// response bodies or credentials through its result or errors.
func (r *Runtime) searchTavily(ctx context.Context, credentials webSearchCredentials, args map[string]any) (map[string]any, error) {
	if err := credentials.validate(); err != nil {
		return nil, err
	}
	fail := func(err error) (map[string]any, error) {
		return nil, redactWebSearchError(err, credentials.APIKey)
	}
	query := strings.TrimSpace(trimString(args["query"]))
	if query == "" {
		return fail(fmt.Errorf("web search query is required"))
	}
	if len([]rune(query)) > maxWebSearchQueryRunes {
		return fail(fmt.Errorf("web search query must be at most %d characters", maxWebSearchQueryRunes))
	}
	maxResults := int(int64Param(args["max_results"]))
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > maxWebSearchResults {
		maxResults = maxWebSearchResults
	}
	payload, err := json.Marshal(map[string]any{
		"query":        query,
		"search_depth": "basic",
		"max_results":  maxResults,
	})
	if err != nil {
		return fail(fmt.Errorf("build web search request: %w", err))
	}
	endpoint := strings.TrimSpace(r.webSearchEndpoint)
	if endpoint == "" {
		endpoint = defaultTavilySearchEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fail(fmt.Errorf("web search endpoint is invalid"))
	}
	requestCtx, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fail(fmt.Errorf("build web search request: %w", err))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(credentials.APIKey))
	client := r.client
	if client == nil {
		client = http.DefaultClient
	}
	boundedClient := *client
	boundedClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := boundedClient.Do(request)
	if err != nil {
		return fail(fmt.Errorf("web search request failed: %w", err))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxWebSearchResponseBytes+1))
	if err != nil {
		return fail(fmt.Errorf("read web search response: %w", err))
	}
	if len(body) > maxWebSearchResponseBytes {
		return fail(fmt.Errorf("web search response exceeded %d bytes", maxWebSearchResponseBytes))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fail(fmt.Errorf("web search API key was rejected"))
		case http.StatusTooManyRequests:
			return fail(fmt.Errorf("web search provider rate limit was exceeded"))
		default:
			return fail(fmt.Errorf("web search provider returned HTTP %d", response.StatusCode))
		}
	}
	var decoded struct {
		Answer  string `json:"answer"`
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fail(fmt.Errorf("decode web search response: %w", err))
	}
	results := make([]map[string]any, 0, len(decoded.Results))
	for _, item := range decoded.Results {
		if len(results) >= maxResults {
			break
		}
		if strings.TrimSpace(item.URL) == "" {
			continue
		}
		results = append(results, map[string]any{
			"title":   redactWebSearchText(previewText(item.Title, 300), credentials.APIKey),
			"url":     redactWebSearchText(strings.TrimSpace(item.URL), credentials.APIKey),
			"content": redactWebSearchText(previewText(item.Content, 2000), credentials.APIKey),
			"score":   item.Score,
		})
	}
	return map[string]any{
		"provider": "tavily",
		"query":    redactWebSearchText(query, credentials.APIKey),
		"answer":   redactWebSearchText(previewText(decoded.Answer, 3000), credentials.APIKey),
		"results":  results,
	}, nil
}

func redactWebSearchText(value, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[redacted]")
}

func redactWebSearchError(err error, secret string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	message = redactWebSearchText(message, secret)
	if strings.TrimSpace(message) == "" {
		message = "web search request failed"
	}
	return fmt.Errorf("%s", message)
}
