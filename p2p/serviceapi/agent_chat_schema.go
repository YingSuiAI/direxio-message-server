package serviceapi

func nativeAgentChatSchema() *ActionSchema {
	reference := &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"kind": {Type: "string", Required: true}, "room_id": {Type: "string"}, "room_type": {Type: "string"},
		"title": {Type: "string"}, "preview": {Type: "string"}, "channel_id": {Type: "string"}, "post_id": {Type: "string"},
		"confirmation_id": {Type: "string"}, "operation_id": {Type: "string"}, "workload_id": {Type: "string"},
		"task_id": {Type: "string"}, "plan_id": {Type: "string"}, "action": {Type: "string"},
		"revision": {Type: "integer"}, "expires_at": {Type: "string"}, "target_kind": {Type: "string"},
		"summary": {Type: "string"}, "plan_digest": {Type: "string"},
	}}
	return &ActionSchema{Request: map[string]ActionFieldSchema{
		"prompt": {Type: "string"}, "message": {Type: "string"}, "messages": {Type: "array"},
		"conversation_id": {Type: "string"}, "turn_id": {Type: "string"}, "after_seq": {Type: "integer"},
		"model_profile_id": {Type: "string"}, "client_model_profile_id": {Type: "string"},
		"attachments": {Type: "array", Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
			"type": {Type: "string"}, "name": {Type: "string"}, "mime_type": {Type: "string", Required: true}, "data_base64": {Type: "string", Required: true, WriteOnly: true},
		}}},
	}, Response: map[string]ActionFieldSchema{
		"text": {Type: "string"}, "tool_calls": {Type: "array"},
		"references": {Type: "array", Items: reference, Presence: &ActionPresenceSchema{
			Omitted: "No room, channel, or pending confirmation references were produced.",
			Present: "References are immutable projections. Pending confirmation references are actionable only after the client reads the authoritative confirmation/operation state.",
		}},
	}}
}
