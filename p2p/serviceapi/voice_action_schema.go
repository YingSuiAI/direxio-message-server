package serviceapi

func voiceSessionCreateSchema() *ActionSchema {
	return &ActionSchema{Request: map[string]ActionFieldSchema{
		"conversation_id":         {Type: "string", Required: true},
		"conversation_profile_id": {Type: "string"},
		"speech_profile_id":       {Type: "string"},
	}, Response: map[string]ActionFieldSchema{
		"ok": {Type: "boolean"}, "session_id": {Type: "string"}, "conversation_id": {Type: "string"},
		"token": {Type: "string"}, "app_id": {Type: "string"}, "voice_chat_app_id": {Type: "string"}, "room_id": {Type: "string"}, "user_id": {Type: "string"}, "ai_user_id": {Type: "string"}, "expires_at": {Type: "string"}, "conversation_profile_id": {Type: "string"}, "speech_profile_id": {Type: "string"}, "client_transcript_submit_enabled": {Type: "boolean"},
	}}
}

func voiceSessionMutationSchema() *ActionSchema {
	return &ActionSchema{Request: map[string]ActionFieldSchema{"session_id": {Type: "string", Required: true}}, Response: map[string]ActionFieldSchema{"ok": {Type: "boolean"}, "session_id": {Type: "string"}, "started": {Type: "boolean"}, "interrupted": {Type: "boolean"}, "ended": {Type: "boolean"}}}
}

func voiceSessionTranscriptSchema() *ActionSchema {
	return &ActionSchema{Request: map[string]ActionFieldSchema{"session_id": {Type: "string", Required: true}, "transcript_delta": {Type: "string"}, "transcript_final": {Type: "string"}}, Response: map[string]ActionFieldSchema{"ok": {Type: "boolean"}, "session_id": {Type: "string"}, "accepted": {Type: "boolean"}, "reason": {Type: "string"}}}
}

func voiceSessionStreamSchema() *ActionSchema {
	return &ActionSchema{Request: map[string]ActionFieldSchema{"session_id": {Type: "string", Required: true}}}
}
