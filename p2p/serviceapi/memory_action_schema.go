package serviceapi

func memoryConfigSchema(update bool) *ActionSchema {
	response := map[string]ActionFieldSchema{
		"enabled":              {Type: "boolean", Required: true},
		"embedding_configured": {Type: "boolean", Required: true},
		"embedding_profile_id": {Type: "string"},
		"embedding_model":      {Type: "string"},
		"revision":             {Type: "integer", Required: true},
		"updated_at":           {Type: "string"},
	}
	schema := &ActionSchema{Response: response}
	if update {
		schema.Request = map[string]ActionFieldSchema{
			"idempotency_key":   {Type: "string", Required: true},
			"expected_revision": {Type: "integer", Required: true},
			"enabled":           {Type: "boolean", Required: true},
		}
	}
	return schema
}

func memoryStatusSchema() *ActionSchema {
	schema := memoryConfigSchema(false)
	for key, field := range map[string]ActionFieldSchema{
		"active_fact_count":         {Type: "integer", Required: true},
		"timeline_event_count":      {Type: "integer", Required: true},
		"pending_observation_count": {Type: "integer", Required: true},
		"failed_observation_count":  {Type: "integer", Required: true},
		"facts":                     {Type: "array", Required: true},
		"timeline":                  {Type: "array", Required: true},
	} {
		schema.Response[key] = field
	}
	return schema
}

func memoryFactUpdateSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"fact_id":         {Type: "string", Required: true},
			"idempotency_key": {Type: "string", Required: true},
			"value":           {Type: "string", Required: true},
		},
		Response: memoryFactFields(),
	}
}

func memoryFactDeleteSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"fact_id":         {Type: "string", Required: true},
			"idempotency_key": {Type: "string", Required: true},
		},
		Response: map[string]ActionFieldSchema{
			"fact_id": {Type: "string", Required: true},
			"deleted": {Type: "boolean", Required: true},
		},
	}
}

func memoryFactFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"id":                {Type: "string", Required: true},
		"subject":           {Type: "string", Required: true},
		"predicate":         {Type: "string", Required: true},
		"value":             {Type: "string", Required: true},
		"kind":              {Type: "string", Required: true},
		"confidence":        {Type: "number", Required: true},
		"valid_from":        {Type: "string", Required: true},
		"last_confirmed_at": {Type: "string", Required: true},
	}
}
