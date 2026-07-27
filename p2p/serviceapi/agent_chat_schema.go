package serviceapi

func nativeAgentChatSchema() *ActionSchema {
	return &ActionSchema{Request: map[string]ActionFieldSchema{
		"prompt": {Type: "string"}, "message": {Type: "string"}, "messages": {Type: "array"},
		"conversation_id": {Type: "string"}, "turn_id": {Type: "string"}, "after_seq": {Type: "integer"},
		"model_profile_id": {Type: "string"}, "client_model_profile_id": {Type: "string"},
		"attachments": {Type: "array", Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
			"type": {Type: "string"}, "name": {Type: "string"}, "mime_type": {Type: "string", Required: true}, "data_base64": {Type: "string", Required: true, WriteOnly: true},
		}}},
	}, Response: map[string]ActionFieldSchema{"text": {Type: "string"}, "tool_calls": {Type: "array"}}}
}
