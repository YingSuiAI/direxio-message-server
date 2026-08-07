package serviceapi

func releaseStatusSchema() *ActionSchema {
	return &ActionSchema{
		Response: map[string]ActionFieldSchema{
			"available":         {Type: "boolean", Required: true},
			"current_version":   {Type: "string", Required: true},
			"client_version":    {Type: "string"},
			"updater_available": {Type: "boolean", Required: true},
			"updater_ready":     {Type: "boolean", Required: true},
			"desired_state":     {Type: "string", Required: true},
			"active_job": {
				Type: "object",
				Properties: map[string]ActionFieldSchema{
					"job_id":            {Type: "string", Required: true},
					"component":         {Type: "string", Required: true},
					"status":            {Type: "string", Required: true},
					"current_version":   {Type: "string"},
					"target_version":    {Type: "string"},
					"service_available": {Type: "boolean", Required: true},
				},
			},
			"watchdog": {
				Type: "object", Required: true,
				Properties: map[string]ActionFieldSchema{
					"status":           {Type: "string", Required: true},
					"degraded":         {Type: "boolean", Required: true},
					"cooldown_until":   {Type: "string"},
					"last_observed_at": {Type: "string"},
					"error_code":       {Type: "string"},
				},
			},
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
			"component":       {Type: "string", Required: true},
			"target_version":  {Type: "string", Required: true},
			"idempotency_key": {Type: "string", Required: true},
			"confirm":         {Type: "string", Required: true},
		},
		Response: map[string]ActionFieldSchema{
			"job_id":     {Type: "string", Required: true},
			"job_token":  {Type: "string", Required: true, WriteOnly: true},
			"status_url": {Type: "string", Required: true},
			"status":     {Type: "string"},
		},
	}
}
