package nativeagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
)

type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Write       bool
	// Available is evaluated whenever the tool registry is built. It is used
	// for server capabilities that can become ready after startup; nil means
	// available for backwards-compatible first-party/test tools.
	Available func() bool
	Handler   func(context.Context, map[string]any) (any, error)
}

func (r *Runtime) enabledTools(ctx context.Context, config map[string]any, params map[string]any) []Tool {
	selected := stringSliceParam(params["enabled_tools"])
	if len(selected) == 0 {
		selected = stringSliceParam(config["enabled_tools"])
	}
	availableTools := r.availableTools()
	byName := make(map[string]Tool, len(availableTools))
	for _, tool := range availableTools {
		byName[tool.Name] = tool
	}
	enabled := map[string]bool{}
	enable := func(tool Tool) {
		enabled[tool.Name] = true
	}
	if len(selected) == 0 {
		for _, tool := range availableTools {
			enable(tool)
		}
	} else {
		for _, value := range selected {
			if strings.EqualFold(value, "all") {
				for _, tool := range availableTools {
					enable(tool)
				}
				break
			}
			if name := nativeToolAlias(value); name != "" {
				if tool, ok := byName[name]; ok {
					enable(tool)
				}
			}
		}
	}
	// Durable knowledge tools are a compiled core capability. Keep them
	// available after upgrades even when an older enabled_tools list omits them.
	for _, tool := range availableTools {
		if nativeAgentMemoryToolName(tool.Name) != "" {
			enable(tool)
		}
	}
	tools := make([]Tool, 0, len(availableTools))
	for _, tool := range availableTools {
		if enabled[tool.Name] {
			tools = append(tools, tool)
		}
	}
	return tools
}

func enableNativeAgentManagementTools(enabled map[string]bool, availableTools []Tool) {
	for _, tool := range availableTools {
		if nativeAgentManagementTool(tool.Name) {
			enabled[tool.Name] = true
		}
	}
}

func nativeAgentManagementTool(name string) bool {
	name = strings.TrimSpace(name)
	return name == "native_agent_runtime_inspect" ||
		strings.HasPrefix(name, "native_agent_skills_") ||
		strings.HasPrefix(name, "native_agent_mcp_servers_")
}

func nativeToolAlias(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, ".", "_")
	value = strings.ReplaceAll(value, "-", "_")
	aliases := map[string]string{
		"agent_schedules_list":                                   "native_agent_schedules_list",
		"agent_schedules_get":                                    "native_agent_schedules_get",
		"agent_schedule_runs_list":                               "native_agent_schedule_runs_list",
		"agent_schedule_runs_get":                                "native_agent_schedule_runs_get",
		"agent_core_aws_ec2_provisions_plan":                     "native_agent_aws_ec2_provisions_plan",
		"agent_core_aws_ec2_provisions_get":                      "native_agent_aws_ec2_provisions_get",
		"agent_core_aws_ec2_provisions_list":                     "native_agent_aws_ec2_provisions_list",
		"agent_core_aws_ec2_provisions_events":                   "native_agent_aws_ec2_provisions_events",
		"agent_core_aws_ec2_provisions_create_request":           "native_agent_aws_ec2_provisions_create_request",
		"agent_core_aws_ec2_provisions_destroy_request":          "native_agent_aws_ec2_provisions_destroy_request",
		"agent_core_aws_ec2_provisions_geolibre_install_plan":    "native_agent_aws_ec2_geolibre_install_plan",
		"agent_core_aws_ec2_provisions_geolibre_install_request": "native_agent_aws_ec2_geolibre_install_request",
		"agent_core_workloads_actual_get":                        "native_agent_workload_actual_get",
		"contacts_list":                                          "dirextalk_contacts_list",
		"search_contacts":                                        "dirextalk_contacts_search",
		"contacts_search":                                        "dirextalk_contacts_search",
		"rooms_search":                                           "dirextalk_rooms_search",
		"search_rooms":                                           "dirextalk_rooms_search",
		"messages_list":                                          "dirextalk_messages_list",
		"list_messages":                                          "dirextalk_messages_list",
		"messages_send":                                          "dirextalk_messages_send",
		"send_message":                                           "dirextalk_messages_send",
		"room_members_list":                                      "dirextalk_room_members_list",
		"channel_posts_list":                                     "dirextalk_channel_posts_list",
		"channel_comments_list":                                  "dirextalk_channel_comments_list",
		"channel_comments_create":                                "dirextalk_channel_comments_create",
		"summarize":                                              "dirextalk_summarize",
		"summarize_conversation":                                 "dirextalk_summarize",
		"agent_contacts_list":                                    "dirextalk_contacts_list",
		"agent_contacts_search":                                  "dirextalk_contacts_search",
		"agent_rooms_search":                                     "dirextalk_rooms_search",
		"agent_messages_list":                                    "dirextalk_messages_list",
		"agent_messages_send":                                    "dirextalk_messages_send",
		"agent_room_members_list":                                "dirextalk_room_members_list",
		"agent_channel_posts_list":                               "dirextalk_channel_posts_list",
		"agent_channel_comments_list":                            "dirextalk_channel_comments_list",
		"agent_channel_comments_create":                          "dirextalk_channel_comments_create",
		"agent_summarize":                                        "dirextalk_summarize",
		"memory_remember":                                        "native_agent_memory_remember",
		"remember":                                               "native_agent_memory_remember",
		"memory_search":                                          "native_agent_memory_search",
		"recall":                                                 "native_agent_memory_search",
		"skills_list":                                            "native_agent_skills_list",
		"skills_install":                                         "native_agent_skills_install",
		"skills_enable":                                          "native_agent_skills_enable",
		"skills_disable":                                         "native_agent_skills_disable",
		"skills_uninstall":                                       "native_agent_skills_uninstall",
		"install_skill":                                          "native_agent_skills_install",
		"enable_skill":                                           "native_agent_skills_enable",
		"disable_skill":                                          "native_agent_skills_disable",
		"uninstall_skill":                                        "native_agent_skills_uninstall",
		"agent_skills_list":                                      "native_agent_skills_list",
		"agent_skills_install":                                   "native_agent_skills_install",
		"agent_skills_enable":                                    "native_agent_skills_enable",
		"agent_skills_disable":                                   "native_agent_skills_disable",
		"agent_skills_uninstall":                                 "native_agent_skills_uninstall",
		"mcp_servers_list":                                       "native_agent_mcp_servers_list",
		"mcp_servers_install":                                    "native_agent_mcp_servers_install",
		"mcp_servers_enable":                                     "native_agent_mcp_servers_enable",
		"mcp_servers_disable":                                    "native_agent_mcp_servers_disable",
		"mcp_servers_uninstall":                                  "native_agent_mcp_servers_uninstall",
		"install_mcp_server":                                     "native_agent_mcp_servers_install",
		"enable_mcp_server":                                      "native_agent_mcp_servers_enable",
		"disable_mcp_server":                                     "native_agent_mcp_servers_disable",
		"uninstall_mcp_server":                                   "native_agent_mcp_servers_uninstall",
		"agent_mcp_servers_list":                                 "native_agent_mcp_servers_list",
		"agent_mcp_servers_install":                              "native_agent_mcp_servers_install",
		"agent_mcp_servers_enable":                               "native_agent_mcp_servers_enable",
		"agent_mcp_servers_disable":                              "native_agent_mcp_servers_disable",
		"agent_mcp_servers_uninstall":                            "native_agent_mcp_servers_uninstall",
	}
	if strings.HasPrefix(value, "dirextalk_") {
		return value
	}
	if strings.HasPrefix(value, "native_agent_") {
		return value
	}
	return aliases[value]
}

func (r *Runtime) availableTools() []Tool {
	tools := make([]Tool, 0, len(r.tools)+2)
	seen := map[string]bool{}
	for _, tool := range r.tools {
		if tool.Available != nil && !tool.Available() {
			continue
		}
		// These names are reserved for the compiled, owner-scoped handlers below.
		if nativeAgentMemoryToolName(tool.Name) != "" {
			continue
		}
		if embeddedDirextalkTool(tool.Name) {
			tools = append(tools, tool)
			seen[tool.Name] = true
		}
	}
	if r.persistentMemoryReady && r.knowledge != nil {
		for _, tool := range r.knowledgeEinoTools() {
			if tool.Available != nil && !tool.Available() {
				continue
			}
			if !seen[tool.Name] {
				tools = append(tools, tool)
			}
		}
	}
	return tools
}

// embeddedDirextalkTool is a positive allowlist anchored to the compiled MCP
// capability registry. Configuration may select from it, but cannot add tools.
func embeddedDirextalkTool(name string) bool {
	if name == "dirextalk_summarize" {
		return true
	}
	// Generic workload plan projections may contain command/image material;
	// they are deliberately unavailable to Native Agent even if the shared
	// MCP registry happens to know the legacy names.
	if name == "native_agent_workloads_list" || name == "native_agent_workloads_get" || name == "native_agent_workloads_quote" {
		return false
	}
	if nativeAgentMemoryToolName(name) != "" {
		return true
	}
	// Control and schedule names are a closed set. Prefix checks would publish
	// future confirm/create/delete actions merely because they share a prefix.
	switch name {
	case "native_agent_schedules_list", "native_agent_schedules_get",
		"native_agent_schedule_runs_list", "native_agent_schedule_runs_get",
		"native_agent_aws_credentials_list", "native_agent_aws_credentials_test",
		"native_agent_workloads_list",
		"native_agent_workloads_get", "native_agent_workloads_quote",
		"native_agent_workload_operations_get", "native_agent_workload_operations_events",
		"native_agent_workload_actual_get",
		"native_agent_deployments_list", "native_agent_deployments_get",
		"native_agent_deployments_events",
		"native_agent_aws_ec2_provisions_plan", "native_agent_aws_ec2_provisions_get",
		"native_agent_aws_ec2_provisions_list", "native_agent_aws_ec2_provisions_events",
		"native_agent_aws_ec2_provisions_create_request", "native_agent_aws_ec2_provisions_destroy_request",
		"native_agent_aws_ec2_geolibre_install_plan", "native_agent_aws_ec2_geolibre_install_request":
		return true
	}
	_, ok := dirextalkmcp.NativeToolAction(name)
	return ok
}

func nativeAgentMemoryToolName(name string) string {
	switch strings.TrimSpace(name) {
	case "native_agent_memory_remember", "native_agent_memory_search":
		return strings.TrimSpace(name)
	default:
		return ""
	}
}

func (r *Runtime) invokeDirectTool(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	toolName := nativeToolAlias(action)
	if toolName == "" {
		return nil, fmt.Errorf("unknown native agent tool action %q", action)
	}
	result, err := r.callTool(ctx, r.availableTools(), toolName, params)
	if err != nil {
		return nil, err
	}
	return anyToMap(result)
}

func (r *Runtime) summarize(ctx context.Context, params map[string]any) (map[string]any, error) {
	text := trimString(params["text"])
	roomID := trimString(params["room_id"])
	if text == "" && roomID != "" {
		result, err := r.invokeDirectTool(ctx, "agent.messages.list", params)
		if err != nil {
			return nil, err
		}
		text = jsonValue(result["messages"])
	}
	if text == "" {
		return map[string]any{"summary": "", "message": "no content"}, nil
	}
	runes := []rune(strings.Join(strings.Fields(text), " "))
	limit := 500
	if len(runes) < limit {
		limit = len(runes)
	}
	summary := string(runes[:limit])
	if len(runes) > limit {
		summary += "..."
	}
	return map[string]any{"summary": summary, "source_chars": len([]rune(text))}, nil
}

func (r *Runtime) callTool(ctx context.Context, tools []Tool, name string, args map[string]any) (any, error) {
	for _, tool := range tools {
		if tool.Name == name {
			return tool.Handler(ctx, args)
		}
	}
	return nil, fmt.Errorf("tool %q is not available", name)
}

func objectSchema(properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties}
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }
func boolSchema() map[string]any   { return map[string]any{"type": "boolean"} }
