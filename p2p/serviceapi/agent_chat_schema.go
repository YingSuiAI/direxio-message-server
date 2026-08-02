package serviceapi

func nativeAgentChatSchema() *ActionSchema {
	reference := &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"kind": {Type: "string", Required: true}, "room_id": {Type: "string", Required: true}, "room_type": {Type: "string"},
		"title": {Type: "string"}, "preview": {Type: "string"}, "channel_id": {Type: "string"}, "post_id": {Type: "string"},
	}}
	return &ActionSchema{Request: map[string]ActionFieldSchema{
		"prompt": {Type: "string"}, "message": {Type: "string"}, "messages": {Type: "array"},
		"conversation_id": {Type: "string"}, "turn_id": {Type: "string"}, "after_seq": {Type: "integer"},
		"model_profile_id": {Type: "string"}, "client_model_profile_id": {Type: "string"},
		"tool_credentials": webSearchToolCredentialsField(false),
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
