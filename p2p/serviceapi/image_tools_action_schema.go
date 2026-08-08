package serviceapi

func imageToolUploadBeginSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key":  {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"image_request_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"name":             {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "basename;utf8_bytes_1_to_255"}},
			"mime_type":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:image/jpeg|image/png|image/webp"}},
			"declared_size":    {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_1_to_8388608"}},
			"content_sha256":   {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256"}},
		},
		Response: imageToolUploadProjectionSchema(),
	}
}

func imageToolUploadAppendSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key":   {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"upload_id":         {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"expected_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
			"ordinal":           {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "non_negative_integer"}},
			"offset_bytes":      {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "non_negative_integer"}},
			"data_base64":       {Type: "string", Required: true, WriteOnly: true, Presence: &ActionPresenceSchema{Present: "canonical_standard_base64_decoding_to_1_to_1048576_bytes"}},
			"chunk_sha256":      {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256_matching_decoded_data"}},
		},
		Response: imageToolUploadProjectionSchema(),
	}
}

func imageToolUploadCommitSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key":   {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"upload_id":         {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"expected_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
			"content_sha256":    {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256_matching_declared_upload"}},
		},
		Response: map[string]ActionFieldSchema{
			"source_id":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"source_revision":  {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "exactly_1"}},
			"image_request_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"name":             {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "basename;utf8_bytes_1_to_255"}},
			"mime_type":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:image/jpeg|image/png|image/webp"}},
			"size_bytes":       {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_1_to_8388608"}},
			"sha256":           {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256_matching_committed_content"}},
			"status":           {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exactly:committed"}},
			"expires_at":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "rfc3339"}},
		},
	}
}

func imageToolUploadProjectionSchema() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"upload_id":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"source_id":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"image_request_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"status":           {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:receiving|committed|consumed"}},
		"received_size":    {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_0_to_8388608"}},
		"max_chunk_bytes":  {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "exactly_1048576"}},
		"revision":         {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"expires_at":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "rfc3339"}},
	}
}

func imageToolExecuteSchema(translate bool) *ActionSchema {
	request := map[string]ActionFieldSchema{
		"idempotency_key": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"source_id":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"source_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "exactly_1"}},
	}
	response := map[string]ActionFieldSchema{
		"idempotency_key": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid_matching_request"}},
		"source_id":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid_matching_request"}},
		"source_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "exactly_1_matching_request"}},
		"text":            {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "utf8_bytes_0_to_65536"}},
	}
	if translate {
		request["target_locale"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_bcp47"}}
		response["target_locale"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_bcp47_matching_request"}}
	}
	return &ActionSchema{Request: request, Response: response}
}
