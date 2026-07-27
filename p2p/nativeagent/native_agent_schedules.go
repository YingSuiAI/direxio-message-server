package nativeagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// scheduledToolNames is deliberately independent of interactive selection.
// These are the actual compiled MCP tool names, and exclude message access,
// Matrix writes, runtime/shell, skills, MCP management, and external MCP.
var scheduledToolNames = map[string]struct{}{
	"dirextalk_contacts_list":         {},
	"dirextalk_contacts_search":       {},
	"dirextalk_rooms_search":          {},
	"dirextalk_room_members_list":     {},
	"dirextalk_channel_posts_list":    {},
	"dirextalk_channel_comments_list": {},
}

// EmbeddedAllowedTools is the only code-owned tool request accepted by the
// scheduled runner. Callers cannot expand this set through configuration.
func EmbeddedAllowedTools() []string {
	return []string{"dirextalk_contacts_list", "dirextalk_contacts_search", "dirextalk_rooms_search", "dirextalk_room_members_list", "dirextalk_channel_posts_list", "dirextalk_channel_comments_list"}
}

// ScheduledRunner is a fresh, restricted Eino execution boundary. It shares
// only the direct model client implementation with interactive Agent; it does
// not load agent config, memory, skills, MCP clients, or runtime tools.
type ScheduledRunner struct {
	runtime *Runtime
	tools   map[string]Tool
}

func NewScheduledRunner(tools []Tool) (*ScheduledRunner, error) {
	selected := make(map[string]Tool, len(scheduledToolNames))
	for _, tool := range tools {
		if _, allowed := scheduledToolNames[tool.Name]; !allowed {
			continue
		}
		if tool.Write || tool.Handler == nil {
			return nil, fmt.Errorf("scheduled tool %q is not read-only", tool.Name)
		}
		selected[tool.Name] = tool
	}
	// A missing compiled read-only capability is safe: it is rejected at
	// execute time rather than silently replaced by an interactive tool.
	return &ScheduledRunner{runtime: New(Config{}), tools: selected}, nil
}

func (r *ScheduledRunner) ExecuteScheduled(ctx context.Context, prompt string, profile storage.ModelProfile, requested []string) (string, error) {
	if r == nil || r.runtime == nil {
		return "", fmt.Errorf("scheduled runner is unavailable")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", fmt.Errorf("scheduled prompt is required")
	}
	modelProfile := nativeModelProfile{Provider: strings.ToLower(strings.TrimSpace(profile.Provider)), Model: strings.TrimSpace(profile.Model), BaseURL: strings.TrimRight(strings.TrimSpace(profile.BaseURL), "/"), APIKey: profile.APIKey, SystemPrompt: strings.TrimSpace(profile.SystemPrompt), Temperature: profile.Temperature, TopP: profile.TopP, MaxOutputTokens: profile.MaxOutputTokens, ContextWindow: profile.ContextWindow, ReasoningMode: normalizedReasoningMode(profile.ReasoningEffort)}
	if err := validateModelProfile(modelProfile); err != nil {
		return "", err
	}
	tools := make([]Tool, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		if _, allowed := scheduledToolNames[name]; !allowed {
			return "", fmt.Errorf("scheduled tool %q is not allowed", name)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		tool, ok := r.tools[name]
		if !ok {
			return "", fmt.Errorf("scheduled tool %q is unavailable", name)
		}
		tools = append(tools, tool)
	}
	einoTools := make([]einotool.BaseTool, 0, len(tools))
	for _, tool := range tools {
		info, err := nativeToolInfo(tool)
		if err != nil {
			return "", err
		}
		einoTools = append(einoTools, &einoNativeTool{native: tool, info: info})
	}
	return r.execute(ctx, modelProfile, prompt, einoTools)
}

func (r *ScheduledRunner) execute(ctx context.Context, profile nativeModelProfile, prompt string, tools []einotool.BaseTool) (string, error) {
	// Avoid agent memory/session persistence: this is a one-prompt run.
	text, _, _, err := r.runtime.runEinoAgent(ctx, profile, []*schema.Message{schema.UserMessage(prompt)}, einoAgentSession{systemPrompt: profile.SystemPrompt, contextWindow: profile.ContextWindow}, tools, 24)
	return text, err
}
