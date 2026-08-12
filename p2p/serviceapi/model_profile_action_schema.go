package serviceapi

func modelProfileSyncSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key":                        {Type: "string", Required: true},
			"default_conversation_client_profile_id": {Type: "string"},
			"default_tool_client_profile_id":         {Type: "string"},
			"default_embedding_client_profile_id":    {Type: "string"},
			"default_speech_client_profile_id":       {Type: "string"},
			"entries":                                {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: modelProfileSyncEntryFields()}},
		},
		Response: map[string]ActionFieldSchema{
			"profiles":                               {Type: "array", Items: &ActionFieldSchema{Type: "object", Properties: modelProfileResponseFields()}},
			"default_conversation_client_profile_id": {Type: "string"},
			"default_tool_client_profile_id":         {Type: "string", Required: true},
			"default_embedding_client_profile_id":    {Type: "string"},
			"default_speech_client_profile_id":       {Type: "string"},
		},
	}
}

func modelProfileSyncEntryFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"client_profile_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact_nonempty_bytes", Empty: "rejected"}},
		"expected_revision": {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "no_revision_check", Present: "must_match_current_revision"}},
		"display_name":      {Type: "string"},
		"provider":          {Type: "string", Required: true},
		"base_url":          {Type: "string"},
		"model":             {Type: "string"},
		"system_prompt":     {Type: "string"},
		"api_key":           {Type: "string", WriteOnly: true, Presence: &ActionPresenceSchema{Omitted: "preserve_existing", Present: "rotate_write_only", Empty: "rejected"}},
		"temperature":       {Type: "number"},
		"top_p":             {Type: "number"},
		"max_output_tokens": {Type: "integer"},
		"context_window":    {Type: "integer"},
		"reasoning_effort":  {Type: "string"},
		"model_kind":        {Type: "string"},
		"input_modalities":  {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
		"provider_config": {Type: "object", Properties: map[string]ActionFieldSchema{
			"app_id": {Type: "string"}, "voice_chat_app_id": {Type: "string"}, "ai_user_id": {Type: "string"},
			"tts_speaker": {Type: "string"}, "tts_resource_id": {Type: "string"}, "tts_speech_rate": {Type: "string"},
			"tts_loudness_rate": {Type: "string"}, "tts_pitch": {Type: "string"},
		}},
		"provider_secrets": {Type: "object", WriteOnly: true, Properties: map[string]ActionFieldSchema{
			"rtc_app_key": {Type: "string", WriteOnly: true}, "access_key_id": {Type: "string", WriteOnly: true}, "secret_access_key": {Type: "string", WriteOnly: true},
		}},
	}
}

func modelProfileResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"profile_id":         {Type: "string"},
		"client_profile_id":  {Type: "string"},
		"display_name":       {Type: "string"},
		"provider":           {Type: "string"},
		"base_url":           {Type: "string"},
		"model":              {Type: "string"},
		"system_prompt":      {Type: "string"},
		"api_key_configured": {Type: "boolean"},
		"temperature":        {Type: "number"},
		"top_p":              {Type: "number"},
		"max_output_tokens":  {Type: "integer"},
		"context_window":     {Type: "integer"},
		"reasoning_effort":   {Type: "string"},
		"revision":           {Type: "integer"},
		"credential_version": {Type: "integer"},
		"model_kind":         {Type: "string"},
		"input_modalities":   {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
		"provider_config": {Type: "object", Properties: map[string]ActionFieldSchema{
			"app_id": {Type: "string"}, "voice_chat_app_id": {Type: "string"}, "ai_user_id": {Type: "string"},
			"tts_speaker": {Type: "string"}, "tts_resource_id": {Type: "string"}, "tts_speech_rate": {Type: "string"},
			"tts_loudness_rate": {Type: "string"}, "tts_pitch": {Type: "string"},
		}},
		"provider_secret_status": {Type: "object", Properties: map[string]ActionFieldSchema{
			"rtc_app_key": {Type: "boolean"}, "access_key_id": {Type: "boolean"}, "secret_access_key": {Type: "boolean"},
		}},
		"created_at": {Type: "string"},
		"updated_at": {Type: "string"},
	}
}

func modelProfileListSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"page_size":  {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "server_default"}},
			"page_token": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "first_page"}},
		},
		Response: map[string]ActionFieldSchema{
			"profiles":                               {Type: "array", Items: &ActionFieldSchema{Type: "object", Properties: modelProfileResponseFields()}},
			"next_page_token":                        {Type: "string"},
			"default_conversation_client_profile_id": {Type: "string"},
			"default_tool_client_profile_id":         {Type: "string", Required: true},
			"default_embedding_client_profile_id":    {Type: "string"},
			"default_speech_client_profile_id":       {Type: "string"},
		},
	}
}

func modelProfileGetSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"profile_id": {Type: "string", Required: true},
		},
		Response: map[string]ActionFieldSchema{
			"profile": {Type: "object", Properties: modelProfileResponseFields()},
		},
	}
}

func modelProfileTestSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key": {Type: "string", Required: true},
			"profile_id":      {Type: "string", Required: true},
		},
		Response: map[string]ActionFieldSchema{
			"reachable":  {Type: "boolean", Required: true},
			"error_code": {Type: "string", Required: true},
		},
	}
}

func modelProfileDeleteSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key":   {Type: "string", Required: true},
			"profile_id":        {Type: "string", Required: true},
			"expected_revision": {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "no_revision_check", Present: "must_match_current_revision"}},
		},
		Response: map[string]ActionFieldSchema{
			"deleted":    {Type: "boolean"},
			"profile_id": {Type: "string"},
		},
	}
}
