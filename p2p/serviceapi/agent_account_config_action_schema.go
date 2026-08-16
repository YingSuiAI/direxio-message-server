package serviceapi

func agentAccountConfigSchema(update bool) *ActionSchema {
	identity := ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"display_name": {Type: "string"},
		"avatar_url":   {Type: "string"},
	}}
	fields := map[string]ActionFieldSchema{
		"online_agent_identity": identity,
		"enabled":               {Type: "boolean"},
		"mcp_blocked_room_ids":  {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
	}
	if !update {
		fields = nil
	}
	return &ActionSchema{
		Request: fields,
		Response: map[string]ActionFieldSchema{
			"online_agent_identity": {Type: "object", Required: true, Properties: identity.Properties},
			"enabled":               {Type: "boolean", Required: true},
			"mcp_blocked_room_ids":  {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}},
		},
	}
}
