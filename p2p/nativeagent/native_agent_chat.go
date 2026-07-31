package nativeagent

import (
	"context"
	"strings"
)

const nativeAgentDefaultSystemPrompt = `You are Dirextalk Native Agent, an owner-authorized assistant embedded in Dirextalk Message Server.

Embedded capability rules:
- You can call only the compiled Dirextalk tools provided for this turn, plus immutable skills shipped with this release when present.
- Configuration can disable compiled tools but cannot add tools, skills, MCP servers, commands, packages, or binaries.
- Shell commands, runtime CLI execution, mutable Skill operations, MCP server operations, external MCP calls, and requests to self-call POST /mcp are unavailable in this embedded runtime. State that constraint plainly; never suggest an unapproved local-execution workaround. When the request needs execution outside the control plane, use the available Execution V2 analysis and planning tools instead.
- Use the available Dirextalk tools for product operations. Message sends and channel comment writes are user-visible and must reflect the user's request accurately.
- Execution V2 tools may analyze a request, discover exact imported/reserved target IDs, revisions, and capabilities, build an immutable plan, and request a run. A reservation target at revision 1 is non-executable; service deployment requires a compute.provision Plan/Run that references the executable target revision 2. Never confirm, consume, or claim completion on the owner's behalf; confirmation remains an explicit owner action.
- When durable memory tools are available, explicit requests such as “记住”“帮我记住”“记下来”“保存到记忆”“别忘了”, or “please remember/remember that/save this/store this/don't forget” require calling native_agent_memory_remember before confirming. Never silently store ordinary conversation, and never treat recall questions such as “你还记得”“回忆”“recall” or “do you remember” as a write request.
- For explicit recall requests, call native_agent_memory_search before answering. Do not claim that something was persisted or found unless the corresponding tool returned successfully; conversation memory is separate from durable knowledge memory.
- You can call configured model providers and compress local conversation context.`

func (r *Runtime) chat(ctx context.Context, params map[string]any) (map[string]any, error) {
	ctx = r.withRequestContext(ctx, params)
	attachments, err := ValidateNativeAgentChatParams(params)
	if err != nil {
		return nil, err
	}
	config, _, err := r.agentConfig(ctx)
	if err != nil {
		return nil, err
	}
	profile, err := r.resolveModelProfileForRequest(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := nativeAgentImageProfileAllowed(params, profile, len(attachments) > 0); err != nil {
		return nil, err
	}
	if err := validateModelProfile(profile); err != nil {
		return nil, err
	}
	run, err := r.prepareEinoRun(ctx, config, params, profile)
	if err != nil {
		return nil, err
	}
	tools, cleanup, err := r.enabledEinoTools(ctx, config, params)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	text, toolCalls, produced, err := r.runEinoAgent(ctx, profile, run.inputMessages, run.session, tools, run.maxSteps)
	if err != nil {
		return nil, err
	}
	if err := r.rememberEinoMessages(ctx, config, params, profile, run, produced); err != nil {
		return nil, err
	}
	trace := buildAgentTrace(run, produced, toolCalls, text)
	result := map[string]any{
		"ok":         true,
		"native":     true,
		"framework":  "eino",
		"provider":   profile.Provider,
		"model":      profile.Model,
		"text":       text,
		"tool_calls": toolCalls,
		"steps":      trace["steps"],
		"trace":      trace,
	}
	if references := nativeAgentReferences(produced); len(references) > 0 {
		result["references"] = references
	}
	return result, nil
}

func (r *Runtime) agentSystemPrompt(ctx context.Context, config map[string]any, params map[string]any, extra string) string {
	systemPrompt := nativeAgentDefaultSystemPrompt
	systemPrompt = appendPromptBlock(systemPrompt, pluginConfigString(config, "system_prompt"))
	if requestPrompt := trimString(params["system_prompt"]); requestPrompt != "" {
		systemPrompt = appendPromptBlock(systemPrompt, requestPrompt)
	}
	if skillsPrompt := r.enabledSkillsPromptForRequest(ctx, config, params); skillsPrompt != "" {
		systemPrompt = appendPromptBlock(systemPrompt, skillsPrompt)
	}
	if strings.TrimSpace(extra) != "" {
		systemPrompt = appendPromptBlock(systemPrompt, strings.TrimSpace(extra))
	}
	return systemPrompt
}

func appendPromptBlock(base, block string) string {
	block = strings.TrimSpace(block)
	if block == "" {
		return strings.TrimSpace(base)
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return block
	}
	return base + "\n\n" + block
}
