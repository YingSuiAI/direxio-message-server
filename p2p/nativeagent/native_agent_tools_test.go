package nativeagent

import (
	"context"
	"strings"
	"testing"
)

func TestEnabledToolsExposeManagementAndRuntimeToolsWithoutConfirmation(t *testing.T) {
	runtime := &Runtime{}
	for _, tool := range runtime.enabledTools(context.Background(), nil, nil) {
		if strings.HasPrefix(tool.Name, "native_agent_") || strings.HasPrefix(tool.Name, "runtime__") || strings.HasPrefix(tool.Name, "mcp__") {
			t.Fatalf("mutable extension tool leaked: %q", tool.Name)
		}
	}
	if tools := runtime.enabledRuntimeEinoTools(nil, nil); len(tools) != 0 {
		t.Fatalf("runtime Eino tools leaked: %#v", tools)
	}
}

func toolEnabled(tools []Tool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}
