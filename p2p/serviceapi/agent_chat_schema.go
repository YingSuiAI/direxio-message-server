package serviceapi

func nativeAgentChatSchema(acceptsAttachments bool) *ActionSchema {
	reference := &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"kind": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:room|channel_post|execution_plan|execution_run|execution_confirmation|service_binding"}},
		// task_id is the only Cloud Worker discriminator. Without it the
		// original generic Execution V2 informational schemas remain in force.
		"task_id":            {Type: "string", Presence: &ActionPresenceSchema{Omitted: "generic_execution_v2_reference", Present: "canonical_uuid;cloud_worker_reference"}},
		"account_generation": {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "generic_execution_v2_reference", Present: "positive_integer_equal_to_prepared_permission_generation;required_with_task_id"}},
		"plan_id":            {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid;required_by_plan_linkage"}},
		"plan_revision":      {Type: "integer", Presence: &ActionPresenceSchema{Present: "positive_integer;required_by_plan_linkage"}},
		"plan_digest":        {Type: "string", Presence: &ActionPresenceSchema{Omitted: "cloud_worker_reference", Present: "lowercase_sha256;generic_execution_reference_only"}},
		"run_id":             {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid;required_by_run_linkage"}},
		"run_revision":       {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "non_run_reference", Present: "positive_integer;required_for_execution_run"}},
		"run_digest":         {Type: "string", Presence: &ActionPresenceSchema{Omitted: "cloud_worker_reference", Present: "lowercase_sha256;generic_execution_reference_only"}},
		"deployment_id":      {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid;generic_execution_run_or_service_binding_only"}},
		"execution_id":       {Type: "string", Presence: &ActionPresenceSchema{Omitted: "generic_execution_v2_reference_or_non_run_cloud_worker_reference", Present: "canonical_uuid;required_for_cloud_worker_execution_run"}},
		"worker_id":          {Type: "string", Presence: &ActionPresenceSchema{Omitted: "non_run_or_unassigned_worker_reference", Present: "canonical_uuid;cloud_worker_execution_run_only"}},
		"confirmation_id":    {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid;required_for_execution_confirmation"}},
		"confirmation_revision": {Type: "integer", Presence: &ActionPresenceSchema{
			Omitted: "non_confirmation_reference", Present: "positive_integer;required_for_execution_confirmation",
		}},
		"stage_id":         {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid;generic_execution_confirmation_only"}},
		"stage_revision":   {Type: "integer", Presence: &ActionPresenceSchema{Present: "positive_integer;generic_execution_confirmation_only"}},
		"stage_digest":     {Type: "string", Presence: &ActionPresenceSchema{Present: "lowercase_sha256;generic_execution_confirmation_only"}},
		"target_id":        {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid;generic_execution_confirmation_or_service_binding"}},
		"target_revision":  {Type: "integer", Presence: &ActionPresenceSchema{Present: "positive_integer;generic_execution_confirmation_or_service_binding"}},
		"target_digest":    {Type: "string", Presence: &ActionPresenceSchema{Present: "lowercase_sha256;generic_execution_confirmation_or_service_binding"}},
		"preview_digest":   {Type: "string", Presence: &ActionPresenceSchema{Present: "lowercase_sha256;full_generic_execution_confirmation_only"}},
		"binding_digest":   {Type: "string", Presence: &ActionPresenceSchema{Omitted: "cloud_worker_reference", Present: "lowercase_sha256;generic_binding_only"}},
		"quote_digest":     {Type: "string", Presence: &ActionPresenceSchema{Omitted: "cloud_worker_reference", Present: "lowercase_sha256;generic_reference_only"}},
		"execution_digest": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "cloud_worker_reference", Present: "lowercase_sha256;generic_reference_only"}},
		"risk_level":       {Type: "string", Presence: &ActionPresenceSchema{Present: "bounded_16;full_generic_execution_confirmation_only"}},
		"gate_type":        {Type: "string", Presence: &ActionPresenceSchema{Present: "bounded_128;full_generic_execution_confirmation_only"}},
		"binding_id":       {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid;service_binding_only"}},
		"binding_revision": {Type: "integer", Presence: &ActionPresenceSchema{Present: "positive_integer;service_binding_only"}},
		"project_id":       {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid;service_binding_only"}},
		"status":           {Type: "string", Presence: &ActionPresenceSchema{Present: "cloud_worker_execution_state_or_bounded_generic_run_status"}},
		"state":            {Type: "string", Presence: &ActionPresenceSchema{Present: "cloud_worker_confirmation_state_or_bounded_generic_confirmation_state"}},
		"room_id":          {Type: "string", Presence: &ActionPresenceSchema{Present: "required_for_room_or_channel_post"}},
		"room_type":        {Type: "string"},
		"channel_id":       {Type: "string", Presence: &ActionPresenceSchema{Present: "required_for_channel_post"}},
		"post_id":          {Type: "string", Presence: &ActionPresenceSchema{Present: "required_for_channel_post"}},
		"title":            {Type: "string"},
		"preview":          {Type: "string"},
	}}
	canonicalUUIDArray := &ActionFieldSchema{Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
	request := map[string]ActionFieldSchema{
		"idempotency_key":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"message":                {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "nonempty_utf8"}},
		"conversation_id":        {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"model_profile_id":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact_nonempty_bytes"}},
		"model_profile_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"credential_version":     {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
	}
	if acceptsAttachments {
		conversation := request["conversation_id"]
		conversation.Required = true
		request["conversation_id"] = conversation
		request["after_seq"] = ActionFieldSchema{
			Type: "integer", Presence: &ActionPresenceSchema{Omitted: "from_beginning", Present: "nonnegative_integer"},
		}
		request["accepted_attachment_ids"] = ActionFieldSchema{
			Type: "array", Items: canonicalUUIDArray,
			Presence: &ActionPresenceSchema{
				Omitted: "no_committed_turn_inputs",
				Present: "unique;at_most_4;all_owned_committed_unexpired_and_bound_to_turn_request_id;combined_size_at_most_8388608",
			},
		}
		extensionSelection := &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
			"kind":           {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exactly:mcp"}},
			"id":             {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid;installed_and_enabled"}},
			"pinned_version": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact_source_version_or_commit;utf8_bytes_1_to_256"}},
			"digest":         {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256_equal_to_active_content_digest"}},
			"allowed_tools": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}, Presence: &ActionPresenceSchema{
				Present: "unique;1_to_64;each_utf8_bytes_1_to_256;exactly_present_in_pinned_tool_catalog;no_core_intrinsics",
			}},
		}}
		request["extensions"] = ActionFieldSchema{Type: "array", Items: extensionSelection, Presence: &ActionPresenceSchema{
			Omitted: "no_local_extension_tools_exposed_to_the_model",
			Present: "unique_installation_ids;1_to_64;immutable_exact_selections_only",
		}}
	}
	return &ActionSchema{Request: request, Response: map[string]ActionFieldSchema{
		"text": {Type: "string"}, "tool_calls": {Type: "array"},
		"related_task_ids": {Type: "array", Items: canonicalUUIDArray, Presence: &ActionPresenceSchema{
			Omitted: "No CoreTask was related to the assistant result.",
			Present: "At most 32 server-authored canonical UUIDs; never synthesized from references.",
		}},
		"related_plan_ids": {Type: "array", Items: canonicalUUIDArray, Presence: &ActionPresenceSchema{
			Omitted: "No Execution V2 plan was related to the assistant result.",
			Present: "At most 32 server-authored canonical UUIDs; never synthesized from references.",
		}},
		"references": {Type: "array", Items: reference, Presence: &ActionPresenceSchema{
			Omitted: "No room, channel-post, or Execution V2 references were produced.",
			Present: "At most 32 strict server-authored references; Execution V2 references carry complete UUID/revision/digest linkage and grant no authority.",
		}},
	}}
}

func nativeAgentAttachmentBeginSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"turn_request_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"kind":            {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:image|file|workspace_archive"}},
			"name":            {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "basename;utf8_bytes_1_to_255"}},
			"mime_type":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "approved_media_type_matching_kind"}},
			"declared_size":   {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_1_to_8388608"}},
			"content_sha256":  {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256"}},
		},
		Response: nativeAgentAttachmentUploadProjectionSchema(),
	}
}

func nativeAgentAttachmentAppendSchema() *ActionSchema {
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
		Response: nativeAgentAttachmentUploadProjectionSchema(),
	}
}

func nativeAgentAttachmentCommitSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key":   {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"upload_id":         {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"expected_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
			"content_sha256":    {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256_matching_declared_upload"}},
		},
		Response: map[string]ActionFieldSchema{
			"attachment": {
				Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
					"source_id":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
					"revision":        {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "exactly_1"}},
					"turn_request_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
					"kind":            {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:image|file|workspace_archive"}},
					"name":            {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "basename;utf8_bytes_1_to_255"}},
					"mime_type":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "approved_media_type_matching_kind"}},
					"size_bytes":      {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_1_to_8388608"}},
					"sha256":          {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256_matching_committed_content"}},
					"status":          {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exactly:committed"}},
					"expires_at":      {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "rfc3339"}},
				},
			},
		},
	}
}

func nativeAgentAttachmentUploadProjectionSchema() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"upload_id":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"source_id":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"turn_request_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"status":          {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exactly:receiving"}},
		"received_size":   {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_0_to_8388608"}},
		"max_chunk_bytes": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "exactly_1048576"}},
		"revision":        {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"expires_at":      {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "rfc3339"}},
	}
}

func nativeAgentTurnStopSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key":   {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"turn_id":           {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"expected_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		},
		Response: nativeAgentTurnMetadataSchema(),
	}
}

func nativeAgentTurnSteerSchema() *ActionSchema {
	response := nativeAgentTurnMetadataSchema()
	response["steer_idempotency_key"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid;steer_mutation_receipt"}}
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"idempotency_key":   {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid;steer_mutation_identity"}},
			"turn_id":           {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid;agent_authored_internal_turn_identity"}},
			"expected_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
			"instruction":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "utf8_bytes_1_to_1048576;same_turn_guidance"}},
		},
		Response: response,
	}
}

func nativeAgentTurnsListSchema() *ActionSchema {
	turn := &ActionFieldSchema{Type: "object", Properties: nativeAgentTurnMetadataSchema()}
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"conversation_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"page_token":      {Type: "string", Presence: &ActionPresenceSchema{Omitted: "first_page", Present: "bounded_4096"}},
			"limit":           {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "server_default_50", Present: "integer_1_to_1000"}},
		},
		Response: map[string]ActionFieldSchema{
			"turns":       {Type: "array", Required: true, Items: turn},
			"next_cursor": {Type: "string", Required: true},
		},
	}
}

func nativeAgentTurnMetadataSchema() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"turn_id":          {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid;agent_authored_internal_turn_identity"}},
		"idempotency_key":  {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid;original_start_idempotency_key"}},
		"conversation_id":  {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"state":            {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:accepted|running|waiting_confirmation|completed|canceled|failed"}},
		"revision":         {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"last_sequence":    {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "nonnegative_integer"}},
		"terminal_code":    {Type: "string", Required: true},
		"terminal_summary": {Type: "string", Required: true},
		"created_at":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "rfc3339_nano"}},
		"updated_at":       {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "rfc3339_nano_not_before_created_at"}},
	}
}
