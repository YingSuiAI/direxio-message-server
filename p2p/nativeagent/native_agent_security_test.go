package nativeagent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
)

func TestEinoToolsExcludeRuntimeAndExtensionTools(t *testing.T) {
	runtime := New(Config{Tools: []Tool{{Name: "dirextalk_safe", Handler: func(context.Context, map[string]any) (any, error) { return map[string]any{}, nil }}}})
	tools, cleanup, err := runtime.enabledEinoTools(context.Background(), map[string]any{"runtime_tools": []any{map[string]any{"command": "must-not-run"}}}, map[string]any{"enabled_tools": []any{"all"}})
	if err != nil {
		t.Fatalf("construct Eino tools: %v", err)
	}
	defer cleanup()
	for name := range einoToolNames(t, tools) {
		if strings.HasPrefix(name, "runtime__") || strings.HasPrefix(name, "mcp__") || strings.HasPrefix(name, "native_agent_") {
			t.Fatalf("extension tool leaked: %q", name)
		}
	}
}

func TestRuntimeEnvironmentDoesNotInheritServiceSecrets(t *testing.T) {
	t.Setenv("DIREXTALK_AGENT_TOKEN", "agent-secret")
	t.Setenv("P2P_PORTAL_PASSWORD", "portal-secret")
	env := runtimeEnv(filepath.Join(t.TempDir(), "agent"))
	if envHasPrefix(env, "DIREXTALK_AGENT_TOKEN=") || envHasPrefix(env, "P2P_PORTAL_PASSWORD=") {
		t.Fatalf("runtime env must not inherit service secrets: %#v", env)
	}
}

func TestStdioMCPTransportIsForbidden(t *testing.T) {
	if _, err := New(Config{}).mcpTransport(map[string]any{"transport": "stdio", "command": "must-not-run"}); err == nil || err.Error() != embeddedExtensionsForbiddenMessage {
		t.Fatalf("stdio MCP transport = %v, want stable forbidden error", err)
	}
}

func einoToolNames(t *testing.T, tools []einotool.BaseTool) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, tool := range tools {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		names[info.Name] = true
	}
	return names
}

func envHasPrefix(env []string, prefix string) bool {
	for _, value := range env {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
