package nativeagent

import (
	"context"
	"testing"
)

func TestDefaultEnabledToolsExposeWriteTools(t *testing.T) {
	runtime := New(Config{Tools: []Tool{
		{Name: "dirextalk_contacts_list", Description: "read", Handler: func(context.Context, map[string]any) (any, error) { return map[string]any{}, nil }},
		{Name: "dirextalk_messages_send", Description: "write", Write: true, Handler: func(context.Context, map[string]any) (any, error) { return map[string]any{}, nil }},
		{Name: "custom_unknown", Description: "unknown", Handler: func(context.Context, map[string]any) (any, error) { return map[string]any{}, nil }},
	}})

	tools := runtime.enabledTools(context.Background(), nil, nil)
	seen := map[string]bool{}
	for _, tool := range tools {
		seen[tool.Name] = true
	}
	if !seen["dirextalk_contacts_list"] {
		t.Fatalf("expected read tool to be enabled by default, got %#v", tools)
	}
	if !seen["dirextalk_messages_send"] {
		t.Fatalf("expected write tool to be enabled by default, got %#v", tools)
	}
	if seen["custom_unknown"] {
		t.Fatalf("unknown configured tool must not be enabled: %#v", tools)
	}
}
