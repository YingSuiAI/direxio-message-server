package nativeagent

import (
	"context"
	"strings"
	"testing"
)

func TestAgentSystemPromptPrependsNativeProductRules(t *testing.T) {
	runtime := New(Config{})
	prompt := runtime.agentSystemPrompt(
		context.Background(),
		map[string]any{"system_prompt": "User configured system prompt."},
		map[string]any{"system_prompt": "Request scoped system prompt."},
		"Extra prompt block.",
	)

	for _, marker := range []string{
		"Dirextalk Native Agent",
		"only the compiled Dirextalk tools",
		"Shell commands, runtime CLI execution, mutable Skill operations",
		"User configured system prompt.",
		"Request scoped system prompt.",
		"Extra prompt block.",
	} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("expected system prompt to contain %q, got %q", marker, prompt)
		}
	}
	if strings.Index(prompt, "Dirextalk Native Agent") > strings.Index(prompt, "User configured system prompt.") {
		t.Fatalf("native product rules must come before user system prompt, got %q", prompt)
	}
	for _, forbidden := range []string{"native_agent_skills_*", "npx skills add", "runtime shell/CLI tools", "manage MCP servers"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("locked-down system prompt must not advertise %q: %q", forbidden, prompt)
		}
	}
}
