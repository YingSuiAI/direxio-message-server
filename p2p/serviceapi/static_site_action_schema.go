package serviceapi

func staticSiteListSchema() *ActionSchema {
	release := ActionFieldSchema{
		Type: "object",
		Properties: map[string]ActionFieldSchema{
			"site_id":         {Type: "string", Required: true},
			"release_id":      {Type: "string", Required: true},
			"conversation_id": {Type: "string", Required: true},
			"public_url":      {Type: "string", Required: true},
			"public_path":     {Type: "string", Required: true},
			"size_bytes":      {Type: "integer", Required: true},
			"created_at":      {Type: "string", Required: true},
		},
	}
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"page_size":  {Type: "integer"},
			"page_token": {Type: "string"},
		},
		Response: map[string]ActionFieldSchema{
			"releases":        {Type: "array", Required: true, Items: &release},
			"next_page_token": {Type: "string", Required: true},
		},
	}
}

func staticSiteDeleteSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"release_id":      {Type: "string", Required: true},
			"idempotency_key": {Type: "string", Required: true},
		},
		Response: map[string]ActionFieldSchema{
			"release_id": {Type: "string", Required: true},
			"deleted":    {Type: "boolean", Required: true},
			"replayed":   {Type: "boolean", Required: true},
		},
	}
}
