package serviceapi

func agentConfigSchema(update bool) *ActionSchema {
	identity := ActionFieldSchema{
		Type: "object",
		Properties: map[string]ActionFieldSchema{
			"display_name": {Type: "string"},
			"avatar_url":   {Type: "string"},
		},
	}
	response := map[string]ActionFieldSchema{
		"revision":              {Type: "integer"},
		"native_agent_identity": identity,
		"online_agent_identity": identity,
		"enabled":               {Type: "boolean", Required: true},
		"mcp_blocked_room_ids":  {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}},
	}
	schema := &ActionSchema{Response: response}
	if update {
		schema.Request = map[string]ActionFieldSchema{
			"operation_id":          {Type: "string"},
			"expected_revision":     {Type: "integer"},
			"native_agent_identity": identity,
			"online_agent_identity": identity,
			"enabled":               {Type: "boolean"},
			"mcp_blocked_room_ids":  {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
		}
	}
	return schema
}
