package serviceapi

func nativeAgentChatSchema() *ActionSchema {
	reference := &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"kind": {Type: "string", Required: true}, "room_id": {Type: "string", Required: true}, "room_type": {Type: "string"},
		"title": {Type: "string"}, "preview": {Type: "string"}, "channel_id": {Type: "string"}, "post_id": {Type: "string"},
	}}
	return &ActionSchema{Request: map[string]ActionFieldSchema{
		"prompt": {Type: "string"}, "message": {Type: "string"},
		"conversation_id": {Type: "string"}, "turn_id": {Type: "string"}, "after_seq": {Type: "integer"},
		"model_profile_id":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact_nonempty_bytes"}},
		"model_profile_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"credential_version":     {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"attachments": {Type: "array", Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
			"type": {Type: "string"}, "name": {Type: "string"}, "mime_type": {Type: "string", Required: true}, "data_base64": {Type: "string", Required: true, WriteOnly: true},
		}}},
	}, Response: map[string]ActionFieldSchema{
		"text": {Type: "string"}, "tool_calls": {Type: "array"},
		"references": {Type: "array", Items: reference, Presence: &ActionPresenceSchema{
			Omitted: "No room or channel references were produced.",
			Present: "References are immutable projections derived from successful built-in Dirextalk tool results.",
		}},
	}}
}

func nativeAgentTurnStopSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"turn_id": {Type: "string", Required: true},
		},
		Response: map[string]ActionFieldSchema{
			"turn_id": {Type: "string", Required: true},
		},
	}
}

func nativeAgentTurnsListSchema() *ActionSchema {
	turn := &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"turn_id":          {Type: "string", Required: true},
		"conversation_id":  {Type: "string", Required: true},
		"state":            {Type: "string", Required: true},
		"revision":         {Type: "integer", Required: true},
		"last_sequence":    {Type: "integer", Required: true},
		"terminal_code":    {Type: "string", Required: true},
		"terminal_summary": {Type: "string", Required: true},
		"created_at":       {Type: "string", Required: true},
		"updated_at":       {Type: "string", Required: true},
	}}
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"conversation_id": {Type: "string", Required: true},
			"page_token":      {Type: "string"},
			"limit":           {Type: "integer"},
		},
		Response: map[string]ActionFieldSchema{
			"turns":       {Type: "array", Required: true, Items: turn},
			"next_cursor": {Type: "string", Required: true},
		},
	}
}
