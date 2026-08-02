package nativeagent

import (
	"fmt"
	"strings"
)

// webSearchCredentials is intentionally request-scoped. It is never part of
// Runtime, model profiles, conversation memory, or the durable turn record.
type webSearchCredentials struct {
	Enabled  bool
	Provider string
	APIKey   string
}

func toolCredentialsFromParams(params map[string]any) webSearchCredentials {
	raw := nestedAnyMap(params["tool_credentials"])
	web := nestedAnyMap(raw["web_search"])
	return webSearchCredentials{
		Enabled:  boolParam(web["enabled"]),
		Provider: strings.ToLower(fallbackString(trimString(web["provider"]), "tavily")),
		APIKey:   strings.TrimSpace(trimString(web["api_key"])),
	}
}

func (c webSearchCredentials) validate() error {
	if !c.Enabled {
		return fmt.Errorf("web search is disabled")
	}
	if c.Provider != "tavily" {
		return fmt.Errorf("web search provider is not supported")
	}
	if c.APIKey == "" {
		return fmt.Errorf("web search API key is required")
	}
	return nil
}

func nestedAnyMap(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case map[string]string:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = item
		}
		return out
	default:
		return map[string]any{}
	}
}
