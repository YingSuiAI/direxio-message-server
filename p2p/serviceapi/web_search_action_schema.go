package serviceapi

func webSearchConfigResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"enabled":            {Type: "boolean", Required: true},
		"provider":           {Type: "string", Required: true},
		"api_key_configured": {Type: "boolean", Required: true},
		"api_key_hint":       {Type: "string"},
		"revision":           {Type: "integer", Required: true},
		"tested_at":          {Type: "string"},
		"updated_at":         {Type: "string"},
	}
}

func webSearchConfigGetSchema() *ActionSchema {
	return &ActionSchema{Response: webSearchConfigResponseFields()}
}

func webSearchConfigUpdateSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key":   {Type: "string", Required: true},
			"expected_revision": {Type: "integer", Required: true},
			"enabled":           {Type: "boolean"},
			"provider":          {Type: "string", Presence: &ActionPresenceSchema{Omitted: "preserve_existing", Present: "tavily"}},
			"api_key":           {Type: "string", WriteOnly: true, Presence: &ActionPresenceSchema{Omitted: "preserve_existing", Present: "rotate_write_only", Empty: "rejected"}},
			"api_key_clear":     {Type: "boolean", Presence: &ActionPresenceSchema{Omitted: "preserve_existing", Present: "clear_when_true"}},
		},
		Response: webSearchConfigResponseFields(),
	}
}

func webSearchTestSchema() *ActionSchema {
	return &ActionSchema{Response: map[string]ActionFieldSchema{
		"ok":                 {Type: "boolean", Required: true},
		"provider":           {Type: "string", Required: true},
		"result_count":       {Type: "integer", Required: true},
		"tested_at":          {Type: "string", Required: true},
		"enabled":            {Type: "boolean", Required: true},
		"api_key_configured": {Type: "boolean", Required: true},
		"revision":           {Type: "integer", Required: true},
	}}
}
