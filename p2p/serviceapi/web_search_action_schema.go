package serviceapi

func webSearchCredentialsField(required bool) ActionFieldSchema {
	return ActionFieldSchema{
		Type:      "object",
		Required:  required,
		WriteOnly: true,
		Properties: map[string]ActionFieldSchema{
			"enabled":  {Type: "boolean", Required: true},
			"provider": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "tavily"}},
			"api_key":  {Type: "string", Required: true, WriteOnly: true},
		},
	}
}

func webSearchToolCredentialsField(required bool) ActionFieldSchema {
	return ActionFieldSchema{
		Type:      "object",
		Required:  required,
		WriteOnly: true,
		Properties: map[string]ActionFieldSchema{
			"web_search": webSearchCredentialsField(true),
		},
	}
}

func webSearchTestSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"tool_credentials": webSearchToolCredentialsField(true),
		},
		Response: map[string]ActionFieldSchema{
			"ok":           {Type: "boolean", Required: true},
			"provider":     {Type: "string", Required: true},
			"result_count": {Type: "integer", Required: true},
		},
	}
}
