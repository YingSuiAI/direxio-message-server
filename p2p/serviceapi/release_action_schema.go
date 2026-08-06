package serviceapi

func releaseStatusSchema() *ActionSchema {
	return &ActionSchema{
		Response: map[string]ActionFieldSchema{
			"current_version": {Type: "string", Required: true},
			"client_version":  {Type: "string"},
			"agent": {
				Type: "object", Required: true,
				Properties: map[string]ActionFieldSchema{
					"available":              {Type: "boolean", Required: true},
					"current_version":        {Type: "string", Required: true},
					"latest_version":         {Type: "string", Required: true},
					"minimum_server_version": {Type: "string", Required: true},
					"update_available":       {Type: "boolean", Required: true},
					"compatibility":          {Type: "string", Required: true},
					"reasons":                {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}},
				},
			},
		},
	}
}

func releaseApplySchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"plan_token":      {Type: "string", Required: true, WriteOnly: true},
			"idempotency_key": {Type: "string", Required: true},
			"confirm":         {Type: "string", Required: true},
		},
		Response: map[string]ActionFieldSchema{
			"job_id":     {Type: "string", Required: true},
			"job_token":  {Type: "string", Required: true, WriteOnly: true},
			"status_url": {Type: "string", Required: true},
		},
	}
}
