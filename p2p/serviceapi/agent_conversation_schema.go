package serviceapi

func conversationSchema(action string) *ActionSchema {
	str := func(req bool) ActionFieldSchema { return ActionFieldSchema{Type: "string", Required: req} }
	s := &ActionSchema{}
	switch action {
	case "create":
		s.Request = map[string]ActionFieldSchema{"conversation_id": str(true), "title": str(false), "idempotency_key": str(true)}
	case "list":
		s.Request = map[string]ActionFieldSchema{"page_size": {Type: "integer"}, "page_token": str(false)}
	case "get":
		s.Request = map[string]ActionFieldSchema{"conversation_id": str(true), "message_limit": {Type: "integer"}, "message_cursor": str(false)}
	case "delete":
		s.Request = map[string]ActionFieldSchema{"conversation_id": str(true), "expected_revision": {Type: "integer", Required: true}, "idempotency_key": str(true)}
	default:
		s.Request = map[string]ActionFieldSchema{"conversation_id": str(true), "expected_revision": {Type: "integer", Required: true}, "idempotency_key": str(true), "title": str(false)}
	}
	switch action {
	case "list":
		s.Response = map[string]ActionFieldSchema{"conversations": {Type: "array"}, "next_cursor": {Type: "string"}}
	case "get":
		s.Response = map[string]ActionFieldSchema{"conversation": {Type: "object"}, "messages": {Type: "array"}, "next_cursor": {Type: "string"}}
	default:
		s.Response = map[string]ActionFieldSchema{"conversation": {Type: "object"}, "replayed": {Type: "boolean"}}
	}
	return s
}
func knowledgeSchema(action string) *ActionSchema {
	s := &ActionSchema{}
	switch action {
	case "create":
		s.Request = map[string]ActionFieldSchema{"title": {Type: "string"}, "content": {Type: "string", Required: true}, "tags": {Type: "array"}, "idempotency_key": {Type: "string", Required: true}}
	case "search":
		s.Request = map[string]ActionFieldSchema{"query": {Type: "string"}, "page_size": {Type: "integer"}, "page_token": {Type: "string"}}
	case "status":
		s.Request = nil
	}
	switch action {
	case "create":
		s.Response = map[string]ActionFieldSchema{"memory_id": {Type: "string"}, "title": {Type: "string"}, "content": {Type: "string"}, "tags": {Type: "array"}, "created_at": {Type: "string"}, "replayed": {Type: "boolean"}}
	case "search":
		s.Response = map[string]ActionFieldSchema{"items": {Type: "array"}, "next_cursor": {Type: "string"}}
	case "status":
		s.Response = map[string]ActionFieldSchema{"supported": {Type: "boolean"}, "count": {Type: "integer"}}
	}
	return s
}
