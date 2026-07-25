package nativeagent

import "context"

// managementTools is intentionally read-only. Embedded Native Agent has no
// mutable extension registry, runtime binaries, or external MCP clients.
func (r *Runtime) managementTools() []Tool {
	return []Tool{
		{Name: "native_agent_runtime_inspect", Description: "Inspect embedded Native Agent capabilities.", Parameters: objectSchema(nil), Handler: func(ctx context.Context, _ map[string]any) (any, error) { return r.runtimeInspect(ctx) }},
		{Name: "native_agent_skills_list", Description: "List immutable skills shipped with embedded Native Agent.", Parameters: objectSchema(nil), Handler: func(ctx context.Context, _ map[string]any) (any, error) { return r.skillsList(ctx) }},
		{Name: "native_agent_mcp_servers_list", Description: "List embedded MCP servers.", Parameters: objectSchema(nil), Handler: func(ctx context.Context, _ map[string]any) (any, error) { return r.mcpServersList(ctx) }},
	}
}
