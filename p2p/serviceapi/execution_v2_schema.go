package serviceapi

func executionV2Name(name string) string { return "agent.execution.v2." + name }

func executionV2PageRequest() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"page_size":  {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "server_default_100", Present: "integer_1_to_200"}},
		"page_token": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "first_page"}},
	}
}

func executionV2CloudWorkerRoute(request map[string]ActionFieldSchema) map[string]ActionFieldSchema {
	request["record_kind"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact:cloud_worker"}}
	return request
}

func cloudWorkerConditional(kind, rule string) ActionFieldSchema {
	return ActionFieldSchema{Type: kind, Presence: &ActionPresenceSchema{Present: rule}}
}

func cloudWorkerNested(kind, rule string) ActionFieldSchema {
	return ActionFieldSchema{Type: kind, Required: true, Presence: &ActionPresenceSchema{Present: rule}}
}

func cloudWorkerObject(rule string, properties map[string]ActionFieldSchema) ActionFieldSchema {
	field := cloudWorkerConditional("object", rule)
	field.Properties = properties
	return field
}

func cloudWorkerArray(rule string, item ActionFieldSchema) ActionFieldSchema {
	field := cloudWorkerConditional("array", rule)
	field.Items = &item
	return field
}

func cloudWorkerPlanProperties() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"owner_id":                cloudWorkerConditional("string", "exact_prepared_permission_owner"),
		"account_generation":      cloudWorkerConditional("integer", "positive_integer_equal_to_prepared_permission_generation"),
		"plan_id":                 cloudWorkerConditional("string", "canonical_uuid"),
		"revision":                cloudWorkerConditional("integer", "positive_integer"),
		"status":                  cloudWorkerConditional("string", "exact:waiting_user"),
		"execution_id":            cloudWorkerConditional("string", "canonical_uuid"),
		"task_id":                 cloudWorkerConditional("string", "canonical_uuid"),
		"confirmation_id":         cloudWorkerConditional("string", "canonical_uuid"),
		"conversation_id":         cloudWorkerConditional("string", "canonical_uuid"),
		"turn_id":                 cloudWorkerConditional("string", "canonical_uuid"),
		"objective_summary":       cloudWorkerConditional("string", "nonempty_redacted_summary"),
		"proposal_reason":         cloudWorkerConditional("string", "nonempty_redacted_reason"),
		"persistent_worker_reuse": cloudWorkerConditional("boolean", "authoritative_reuse_preference"),
		"workspace_mode":          cloudWorkerConditional("string", "one_of:none|read_only|write"),
		"aws": cloudWorkerObject("strict_aws_binding", map[string]ActionFieldSchema{
			"account_id": cloudWorkerNested("string", "aws_account_id"),
			"region":     cloudWorkerNested("string", "aws_region"),
		}),
		"compute": cloudWorkerObject("strict_worker_compute", map[string]ActionFieldSchema{
			"instance_type":         cloudWorkerNested("string", "nonempty"),
			"vcpu":                  cloudWorkerNested("integer", "positive_integer"),
			"memory_gib":            cloudWorkerNested("integer", "positive_integer"),
			"volume_gib":            cloudWorkerNested("integer", "positive_integer"),
			"volume_type":           cloudWorkerNested("string", "nonempty"),
			"volume_iops":           cloudWorkerNested("integer", "positive_integer"),
			"volume_throughput_mib": cloudWorkerNested("integer", "positive_integer"),
		}),
		"limits": cloudWorkerObject("strict_hard_limits", map[string]ActionFieldSchema{
			"max_runtime_seconds": cloudWorkerNested("integer", "positive_integer"),
		}),
		"quote": cloudWorkerObject("strict_quote_and_owner_hard_limit", map[string]ActionFieldSchema{
			"amount_micros":                  cloudWorkerNested("integer", "nonnegative_integer"),
			"compute_micros_per_hour":        cloudWorkerNested("integer", "positive_integer"),
			"currency":                       cloudWorkerNested("string", "exact:USD"),
			"source_time":                    cloudWorkerNested("string", "rfc3339_nano"),
			"expires_at":                     cloudWorkerNested("string", "rfc3339_nano_after_source_time"),
			"maximum_authorized_cost_micros": cloudWorkerNested("integer", "integer_greater_than_or_equal_to_amount"),
		}),
		"created_at": cloudWorkerConditional("string", "rfc3339_nano"),
		"updated_at": cloudWorkerConditional("string", "rfc3339_nano_not_before_created_at"),
	}
}

func cloudWorkerRunProperties() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"owner_id":           cloudWorkerConditional("string", "exact_prepared_permission_owner"),
		"account_generation": cloudWorkerConditional("integer", "positive_integer_equal_to_prepared_permission_generation"),
		"run_id":             cloudWorkerConditional("string", "canonical_uuid"),
		"execution_id":       cloudWorkerConditional("string", "canonical_uuid"),
		"plan_id":            cloudWorkerConditional("string", "canonical_uuid"),
		"plan_revision":      cloudWorkerConditional("integer", "positive_integer"),
		"task_id":            cloudWorkerConditional("string", "canonical_uuid"),
		"confirmation_id":    cloudWorkerConditional("string", "canonical_uuid"),
		"conversation_id":    cloudWorkerConditional("string", "canonical_uuid"),
		"turn_id":            cloudWorkerConditional("string", "canonical_uuid"),
		"status":             cloudWorkerConditional("string", "cloud_worker_execution_state"),
		"revision":           cloudWorkerConditional("integer", "positive_integer"),
		"worker_id":          cloudWorkerConditional("string", "empty_until_worker_assigned"),
		"persistent_worker":  cloudWorkerConditional("boolean", "authoritative_worker_lifecycle_flag"),
		"artifact_ids":       cloudWorkerArray("canonical_uuid_array", ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}),
		"failure_code":       cloudWorkerConditional("string", "redacted_failure_code_or_empty"),
		"failure_summary":    cloudWorkerConditional("string", "redacted_failure_summary_or_empty"),
		"created_at":         cloudWorkerConditional("string", "rfc3339_nano"),
		"updated_at":         cloudWorkerConditional("string", "rfc3339_nano_not_before_created_at"),
	}
}

func cloudWorkerArtifactProperties() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"owner_id":           cloudWorkerConditional("string", "exact_prepared_permission_owner"),
		"account_generation": cloudWorkerConditional("integer", "positive_integer_equal_to_prepared_permission_generation"),
		"artifact_id":        cloudWorkerConditional("string", "canonical_uuid"),
		"execution_id":       cloudWorkerConditional("string", "canonical_uuid"),
		"kind":               cloudWorkerConditional("string", "approved_artifact_kind"),
		"name":               cloudWorkerConditional("string", "safe_display_name"),
		"media_type":         cloudWorkerConditional("string", "nonempty_media_type"),
		"size_bytes":         cloudWorkerConditional("integer", "nonnegative_integer"),
		"sha256":             cloudWorkerConditional("string", "lowercase_sha256"),
		"status":             cloudWorkerConditional("string", "one_of:pending|verified|rejected"),
		"created_at":         cloudWorkerConditional("string", "rfc3339_nano"),
	}
}

func cloudWorkerArtifactDownloadProperties() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"owner_id":           cloudWorkerNested("string", "exact_prepared_permission_owner"),
		"account_generation": cloudWorkerNested("integer", "positive_integer_equal_to_prepared_permission_generation"),
		"artifact_id":        cloudWorkerNested("string", "canonical_uuid_equal_to_requested_artifact"),
		"execution_id":       cloudWorkerNested("string", "canonical_uuid"),
		"offset_bytes":       cloudWorkerNested("integer", "integer_0_to_8388607_equal_to_requested_offset"),
		"data_base64":        cloudWorkerNested("string", "canonical_standard_base64_for_1_to_524288_bytes"),
		"chunk_sha256":       cloudWorkerNested("string", "lowercase_sha256_of_decoded_chunk"),
		"artifact_sha256":    cloudWorkerNested("string", "lowercase_sha256_of_complete_artifact"),
		"size_bytes":         cloudWorkerNested("integer", "integer_1_to_8388608"),
		"next_offset_bytes":  cloudWorkerNested("integer", "offset_plus_decoded_chunk_length_strictly_advancing_not_above_size"),
		"eof":                cloudWorkerNested("boolean", "true_exactly_when_next_offset_equals_size"),
	}
}

func cloudWorkerEventProperties() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"event_id":           cloudWorkerConditional("string", "canonical_uuid"),
		"run_id":             cloudWorkerConditional("string", "canonical_uuid_equal_to_requested_run"),
		"owner_id":           cloudWorkerConditional("string", "exact_prepared_permission_owner"),
		"account_generation": cloudWorkerConditional("integer", "positive_integer_equal_to_prepared_permission_generation"),
		"revision":           cloudWorkerConditional("integer", "positive_integer"),
		"sequence":           cloudWorkerConditional("integer", "positive_contiguous_after_previous;first_equals_after_sequence_plus_one_unless_history_truncated"),
		"type":               cloudWorkerConditional("string", "nonempty_event_type"),
		"at":                 cloudWorkerConditional("string", "rfc3339_nano"),
		"payload_digest":     cloudWorkerConditional("string", "lowercase_sha256"),
		"status":             {Type: "string", Presence: &ActionPresenceSchema{Omitted: "event_has_no_state_transition", Present: "cloud_worker_execution_state"}},
	}
}

func executionV2CloudWorkerObjectResponse(name string, properties map[string]ActionFieldSchema) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{name: {Type: "object", Required: true, Properties: properties}}
}

func executionV2CloudWorkerPageResponse(name string, properties map[string]ActionFieldSchema) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		name:              {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: properties}},
		"next_page_token": {Type: "string", Required: true},
	}
}

func executionV2CloudWorkerEventsResponse() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"events":            {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: cloudWorkerEventProperties()}},
		"next_sequence":     {Type: "integer", Required: true},
		"history_truncated": {Type: "boolean", Required: true, Presence: &ActionPresenceSchema{Present: "true_when_after_sequence_precedes_retained_history"}},
	}
}

func executionV2MutationBase() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"idempotency_key": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
	}
}

func executionV2RevisionMutationBase() map[string]ActionFieldSchema {
	fields := executionV2MutationBase()
	fields["expected_revision"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}}
	return fields
}

func executionV2EventsRequest(id string) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		id:               {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"after_sequence": {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "from_beginning", Present: "nonnegative_integer"}},
		"limit":          {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "server_default_100", Present: "integer_1_to_200"}},
	}
}

func executionV2Schema(name string) *ActionSchema {
	switch name {
	case "plans.get":
		request := executionV2CloudWorkerRoute(map[string]ActionFieldSchema{"plan_id": {Type: "string", Required: true}, "revision": {Type: "integer"}})
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerObjectResponse("plan", cloudWorkerPlanProperties())}
	case "plans.list":
		request := executionV2CloudWorkerRoute(executionV2PageRequest())
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerPageResponse("plans", cloudWorkerPlanProperties())}
	case "runs.get":
		return &ActionSchema{Request: executionV2CloudWorkerRoute(map[string]ActionFieldSchema{"run_id": {Type: "string", Required: true}}), Response: executionV2CloudWorkerObjectResponse("run", cloudWorkerRunProperties())}
	case "runs.list":
		request := executionV2CloudWorkerRoute(executionV2PageRequest())
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerPageResponse("runs", cloudWorkerRunProperties())}
	case "runs.cancel":
		request := executionV2CloudWorkerRoute(executionV2RevisionMutationBase())
		request["run_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerObjectResponse("run", cloudWorkerRunProperties())}
	case "runs.events":
		return &ActionSchema{Request: executionV2CloudWorkerRoute(executionV2EventsRequest("run_id")), Response: executionV2CloudWorkerEventsResponse()}
	case "artifacts.get":
		request := executionV2CloudWorkerRoute(map[string]ActionFieldSchema{"artifact_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}})
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerObjectResponse("artifact", cloudWorkerArtifactProperties())}
	case "artifacts.download":
		request := map[string]ActionFieldSchema{
			"record_kind":     {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact:cloud_worker"}},
			"artifact_id":     {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"offset_bytes":    {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_0_to_8388607"}},
			"max_chunk_bytes": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_1_to_524288"}},
		}
		return &ActionSchema{Request: request, Response: cloudWorkerArtifactDownloadProperties()}
	default:
		return nil
	}
}
