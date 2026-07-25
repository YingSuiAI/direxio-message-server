package nativeagent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedRuntimeRejectsMutableExtensions(t *testing.T) {
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	for _, action := range []string{
		"agent.runtime.install", "agent.runtime.which", "agent.runtime.run",
		"agent.skills.install", "agent.skills.enable", "agent.skills.disable", "agent.skills.uninstall", "agent.skills.registry.search",
		"agent.mcp.servers.install", "agent.mcp.servers.enable", "agent.mcp.servers.disable", "agent.mcp.servers.uninstall", "agent.mcp.registry.search",
	} {
		t.Run(action, func(t *testing.T) {
			_, err := runtime.Invoke(context.Background(), action, map[string]any{"command": "must-not-run"})
			if !errors.Is(err, embeddedExtensionsForbidden()) || err.Error() != embeddedExtensionsForbiddenMessage {
				t.Fatalf("%s error = %v, want stable forbidden error", action, err)
			}
		})
	}
	if _, err := runtime.mcpTransport(map[string]any{"transport": "stdio", "command": "must-not-run"}); !errors.Is(err, embeddedExtensionsForbidden()) {
		t.Fatalf("stdio MCP transport = %v, want forbidden", err)
	}
}

func TestEmbeddedEinoToolsIgnoreMutableConfiguration(t *testing.T) {
	compiledInvoked := false
	injectedInvoked := false
	runtime := New(Config{Tools: []Tool{{Name: "dirextalk_contacts_list", Handler: func(context.Context, map[string]any) (any, error) {
		compiledInvoked = true
		return map[string]any{"ok": true}, nil
	}}, {Name: "runtime__shell", Handler: func(context.Context, map[string]any) (any, error) {
		injectedInvoked = true
		return nil, nil
	}}, {Name: "native_agent_skills_install", Handler: func(context.Context, map[string]any) (any, error) {
		injectedInvoked = true
		return nil, nil
	}}, {Name: "unknown_tool", Handler: func(context.Context, map[string]any) (any, error) {
		injectedInvoked = true
		return nil, nil
	}}}})
	tools, cleanup, err := runtime.enabledEinoTools(context.Background(), map[string]any{
		"enabled_tools":         []any{"all"},
		"runtime_shell_enabled": true,
		"runtime_tools":         []any{map[string]any{"id": "must-not-run", "command": "must-not-run"}},
		"mcp_servers":           []any{map[string]any{"id": "external", "url": "http://127.0.0.1"}},
		"skills":                []any{map[string]any{"id": "mutable", "path": "/must-not-read/SKILL.md", "enabled": true}},
	}, nil)
	if err != nil {
		t.Fatalf("construct Eino tools: %v", err)
	}
	defer cleanup()
	names := einoToolNames(t, tools)
	if !names["dirextalk_contacts_list"] {
		t.Fatalf("compiled Dirextalk tool missing: %#v", names)
	}
	for name := range names {
		if strings.HasPrefix(name, "runtime__") || strings.HasPrefix(name, "mcp__") || strings.HasPrefix(name, "native_agent_skills_") || strings.HasPrefix(name, "native_agent_mcp_") {
			t.Fatalf("mutable extension tool leaked into Eino: %q", name)
		}
	}
	if _, err := runtime.invokeDirectTool(context.Background(), "agent.runtime__shell", nil); err == nil {
		t.Fatal("injected runtime tool became directly invokable")
	}
	if injectedInvoked || compiledInvoked {
		t.Fatalf("tool construction/direct rejection invoked injected=%v compiled=%v handlers", injectedInvoked, compiledInvoked)
	}
}

func TestManagementToolsAreReadOnly(t *testing.T) {
	runtime := New(Config{})
	for _, tool := range runtime.managementTools() {
		if strings.Contains(tool.Name, "install") || strings.Contains(tool.Name, "enable") || strings.Contains(tool.Name, "disable") || strings.Contains(tool.Name, "uninstall") {
			t.Fatalf("mutable management tool leaked: %q", tool.Name)
		}
		if _, err := tool.Handler(context.Background(), nil); err != nil {
			t.Fatalf("safe management tool %q failed: %v", tool.Name, err)
		}
	}
}
