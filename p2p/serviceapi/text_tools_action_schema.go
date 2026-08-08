package serviceapi

func textToolFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"tool_id":       {Type: "string", Required: true},
		"name":          {Type: "string", Required: true},
		"system_prompt": {Type: "string", Required: true},
		"order":         {Type: "integer", Required: true},
		"enabled":       {Type: "boolean", Required: true},
	}
}

func textToolsConfigSchema(update bool) *ActionSchema {
	tool := ActionFieldSchema{Type: "object", Properties: textToolFields()}
	schema := &ActionSchema{Response: map[string]ActionFieldSchema{
		"enabled":    {Type: "boolean", Required: true},
		"revision":   {Type: "integer", Required: true},
		"tools":      {Type: "array", Required: true, Items: &tool},
		"updated_at": {Type: "string", Required: true},
	}}
	if update {
		schema.Request = map[string]ActionFieldSchema{
			"idempotency_key":   {Type: "string", Required: true},
			"expected_revision": {Type: "integer", Required: true},
			"enabled":           {Type: "boolean", Required: true},
			"tools":             {Type: "array", Required: true, Items: &tool},
		}
	}
	return schema
}

func textToolsExecuteSchema() *ActionSchema {
	source := ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"title":   {Type: "string", Required: true},
		"url":     {Type: "string", Required: true},
		"snippet": {Type: "string", Required: true},
	}}
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"tool_id":       {Type: "string", Required: true},
			"selected_text": {Type: "string", Required: true},
		},
		Response: map[string]ActionFieldSchema{
			"tool_id": {Type: "string", Required: true},
			"output":  {Type: "string", Required: true},
			"sources": {Type: "array", Required: true, Items: &source},
		},
	}
}
