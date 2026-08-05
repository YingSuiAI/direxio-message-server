package serviceapi

func modelCatalogSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"model_profile_id":        {Type: "string"},
			"client_model_profile_id": {Type: "string"},
			"provider":                {Type: "string"},
			"base_url":                {Type: "string"},
			"api_key":                 {Type: "string", WriteOnly: true},
			"model_kind": {
				Type:     "string",
				Presence: &ActionPresenceSchema{Omitted: "conversation", Present: "conversation_embedding_or_speech"},
			},
		},
		Response: map[string]ActionFieldSchema{
			"models": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
				"id":                {Type: "string", Required: true},
				"name":              {Type: "string"},
				"provider":          {Type: "string", Required: true},
				"context_length":    {Type: "integer"},
				"context_window":    {Type: "integer"},
				"max_output_tokens": {Type: "integer"},
				"input_modalities":  {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
				"output_modalities": {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
			}}},
			"providers": {
				Type:     "array",
				Required: true,
				Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
					"provider":         {Type: "string", Required: true},
					"default_base_url": {Type: "string"},
					"requires_api_key": {Type: "boolean", Required: true},
					"dynamic_models":   {Type: "boolean", Required: true},
				}},
			},
		},
	}
}
