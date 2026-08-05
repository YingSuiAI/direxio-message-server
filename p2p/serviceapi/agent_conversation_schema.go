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
	case "memories_list":
		s.Request = map[string]ActionFieldSchema{"page_size": {Type: "integer"}, "page_token": {Type: "string"}}
	case "memories_update":
		s.Request = map[string]ActionFieldSchema{"memory_id": {Type: "string", Required: true}, "title": {Type: "string"}, "content": {Type: "string", Required: true}, "tags": {Type: "array"}, "expected_revision": {Type: "integer", Required: true}, "idempotency_key": {Type: "string", Required: true}}
	case "memories_delete":
		s.Request = map[string]ActionFieldSchema{"memory_id": {Type: "string", Required: true}, "expected_revision": {Type: "integer", Required: true}, "idempotency_key": {Type: "string", Required: true}}
	}
	switch action {
	case "create":
		s.Response = map[string]ActionFieldSchema{"memory_id": {Type: "string"}, "title": {Type: "string"}, "content": {Type: "string"}, "tags": {Type: "array"}, "created_at": {Type: "string"}, "replayed": {Type: "boolean"}, "embedding_indexed": {Type: "boolean"}, "embedding_profile_id": {Type: "string"}, "embedding_profile_revision": {Type: "integer"}, "embedding_model": {Type: "string"}}
	case "search":
		s.Response = map[string]ActionFieldSchema{"items": {Type: "array"}, "next_cursor": {Type: "string"}, "search_mode": {Type: "string"}, "embedding_profile_id": {Type: "string"}, "embedding_profile_revision": {Type: "integer"}, "embedding_model": {Type: "string"}, "embedding_generation": {Type: "string"}, "collection_config_digest": {Type: "string"}}
	case "status":
		s.Response = map[string]ActionFieldSchema{"supported": {Type: "boolean"}, "count": {Type: "integer"}, "embedding_indexed": {Type: "integer"}, "embedding_stale": {Type: "integer"}, "embedding_profile_id": {Type: "string"}, "embedding_profile_revision": {Type: "integer"}, "embedding_model": {Type: "string"}}
	case "memories_list":
		s.Response = map[string]ActionFieldSchema{"items": {Type: "array"}, "next_page_token": {Type: "string"}}
	default:
		s.Response = map[string]ActionFieldSchema{"memory_id": {Type: "string"}, "title": {Type: "string"}, "content": {Type: "string"}, "tags": {Type: "array"}, "revision": {Type: "integer"}, "created_at": {Type: "string"}, "updated_at": {Type: "string"}, "replayed": {Type: "boolean"}}
	}
	return s
}

func knowledgeConfigSchema(action string) *ActionSchema {
	s := &ActionSchema{Response: map[string]ActionFieldSchema{
		"embedding_profile_id":       {Type: "string", Required: true},
		"embedding_profile_revision": {Type: "integer", Required: true},
		"embedding_model":            {Type: "string", Required: true},
		"dimension":                  {Type: "integer"},
		"collection":                 {Type: "string"},
		"collection_config_digest":   {Type: "string", Required: true},
		"revision":                   {Type: "integer", Required: true},
		"updated_at":                 {Type: "string"},
	}}
	if action == "update" {
		s.Request = map[string]ActionFieldSchema{
			"idempotency_key":          {Type: "string", Required: true},
			"expected_revision":        {Type: "integer", Required: true},
			"embedding_profile_id":     {Type: "string"},
			"profile_id":               {Type: "string"},
			"dimension":                {Type: "integer"},
			"collection":               {Type: "string"},
			"collection_config_digest": {Type: "string"},
		}
	}
	return s
}

func knowledgeSourceSchema(action string) *ActionSchema {
	s := &ActionSchema{}
	switch action {
	case "list":
		s.Request = map[string]ActionFieldSchema{"page_size": {Type: "integer"}, "page_token": {Type: "string"}}
		s.Response = map[string]ActionFieldSchema{"sources": {Type: "array"}, "next_page_token": {Type: "string"}}
	case "delete":
		s.Request = map[string]ActionFieldSchema{"source_id": {Type: "string", Required: true}, "expected_revision": {Type: "integer", Required: true}, "idempotency_key": {Type: "string", Required: true}}
		s.Response = map[string]ActionFieldSchema{"source": {Type: "object"}, "replayed": {Type: "boolean"}}
	case "upload_start":
		s.Request = map[string]ActionFieldSchema{"filename": {Type: "string", Required: true}, "mime_type": {Type: "string", Required: true}, "size": {Type: "integer", Required: true}, "content_sha256": {Type: "string", Required: true}, "idempotency_key": {Type: "string", Required: true}}
		s.Response = map[string]ActionFieldSchema{"upload_id": {Type: "string"}, "source_id": {Type: "string"}, "status": {Type: "string"}, "size": {Type: "integer"}, "received_size": {Type: "integer"}, "max_chunk_bytes": {Type: "integer"}, "progress": {Type: "number"}, "replayed": {Type: "boolean"}}
	case "upload_chunk":
		s.Request = map[string]ActionFieldSchema{"upload_id": {Type: "string", Required: true}, "offset": {Type: "integer", Required: true}, "data": {Type: "string", Required: true}, "chunk_sha256": {Type: "string", Required: true}, "idempotency_key": {Type: "string", Required: true}}
		s.Response = map[string]ActionFieldSchema{"upload_id": {Type: "string"}, "source_id": {Type: "string"}, "status": {Type: "string"}, "size": {Type: "integer"}, "received_size": {Type: "integer"}, "max_chunk_bytes": {Type: "integer"}, "progress": {Type: "number"}}
	case "upload_finish":
		s.Request = map[string]ActionFieldSchema{"upload_id": {Type: "string", Required: true}, "title": {Type: "string"}, "content_sha256": {Type: "string", Required: true}, "idempotency_key": {Type: "string", Required: true}}
		s.Response = map[string]ActionFieldSchema{"source": {Type: "object"}}
	}
	return s
}
