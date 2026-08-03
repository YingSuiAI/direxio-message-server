package schedules

import (
	"context"
	"fmt"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
)

// Native Agent exposes only schedule reads. ProductCore owns schedule
// mutations and the generic task runtime owns occurrence execution.
var nativeScheduleReadActions = map[string]bool{
	"agent.schedules.list":     true,
	"agent.schedules.get":      true,
	"agent.schedule_runs.list": true,
	"agent.schedule_runs.get":  true,
}

func (m *Module) Tools() []nativeagent.Tool {
	if m == nil {
		return nil
	}
	obj := func(keys ...string) map[string]any {
		p := map[string]any{}
		for _, k := range keys {
			p[k] = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "object", "properties": p}
	}
	tool := func(name, desc, action string, schema map[string]any) nativeagent.Tool {
		return nativeagent.Tool{Name: name, Description: desc, Parameters: schema, Write: false, Handler: func(ctx context.Context, p map[string]any) (any, error) { return m.invokeChatTool(ctx, action, p) }}
	}
	return []nativeagent.Tool{
		tool("native_agent_schedules_list", "List embedded schedules.", "agent.schedules.list", obj("limit", "cursor")),
		tool("native_agent_schedules_get", "Get an embedded schedule.", "agent.schedules.get", obj("schedule_id")),
		tool("native_agent_schedule_runs_list", "List runs for an embedded schedule.", "agent.schedule_runs.list", obj("schedule_id", "limit", "cursor")),
		tool("native_agent_schedule_runs_get", "Get an embedded schedule run.", "agent.schedule_runs.get", obj("schedule_id", "run_id")),
	}
}

func (m *Module) invokeChatTool(ctx context.Context, action string, params map[string]any) (any, error) {
	if !nativeScheduleReadActions[action] {
		return nil, fmt.Errorf("schedule action %q is not exposed to Native Agent", action)
	}
	return m.InvokeTool(ctx, action, params)
}
