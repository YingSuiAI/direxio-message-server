package serviceapi

func executionV2Name(name string) string { return "agent.execution.v2." + name }

func executionV2Field(kind string, required bool) ActionFieldSchema {
	return ActionFieldSchema{Type: kind, Required: required}
}

func executionV2PageRequest() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"page_size":  {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "server_default_100", Present: "integer_1_to_200"}},
		"page_token": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "first_page"}},
	}
}

func executionV2PageResponse(item string) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		item:              {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object"}},
		"next_page_token": {Type: "string", Required: true},
	}
}

func executionV2CloudWorkerRoute(request map[string]ActionFieldSchema) map[string]ActionFieldSchema {
	request["record_kind"] = ActionFieldSchema{Type: "string", Presence: &ActionPresenceSchema{Omitted: "generic_execution_v2_authority", Present: "exact:cloud_worker"}}
	return request
}

func cloudWorkerConditional(kind, rule string) ActionFieldSchema {
	return ActionFieldSchema{Type: kind, Presence: &ActionPresenceSchema{
		Omitted: "allowed_only_for_generic_execution_v2;rejected_when_record_kind=cloud_worker",
		Present: "required_when_record_kind=cloud_worker;" + rule,
	}}
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
		"owner_id":                  cloudWorkerConditional("string", "exact_prepared_permission_owner"),
		"account_generation":        cloudWorkerConditional("integer", "positive_integer_equal_to_prepared_permission_generation"),
		"plan_id":                   cloudWorkerConditional("string", "canonical_uuid"),
		"revision":                  cloudWorkerConditional("integer", "positive_integer"),
		"status":                    cloudWorkerConditional("string", "exact:waiting_user"),
		"digest":                    cloudWorkerConditional("string", "lowercase_sha256"),
		"execution_id":              cloudWorkerConditional("string", "canonical_uuid"),
		"task_id":                   cloudWorkerConditional("string", "canonical_uuid"),
		"confirmation_id":           cloudWorkerConditional("string", "canonical_uuid"),
		"conversation_id":           cloudWorkerConditional("string", "canonical_uuid"),
		"turn_id":                   cloudWorkerConditional("string", "canonical_uuid"),
		"recipe_id":                 cloudWorkerConditional("string", "exact:ephemeral-pi-task"),
		"adapter":                   cloudWorkerConditional("string", "exact:pi_json_task_v1"),
		"objective_summary":         cloudWorkerConditional("string", "nonempty_redacted_summary"),
		"proposal_reason":           cloudWorkerConditional("string", "one_of:explicit_user_cloud|local_budget_exceeded"),
		"input_manifest_digest":     cloudWorkerConditional("string", "lowercase_sha256"),
		"input_manifest_item_count": cloudWorkerConditional("integer", "nonnegative_integer"),
		"workspace_mode":            cloudWorkerConditional("string", "one_of:none|read_only|write"),
		"model_authorization": cloudWorkerObject("strict_model_authorization", map[string]ActionFieldSchema{
			"model_profile_id":       cloudWorkerNested("string", "canonical_uuid"),
			"model_profile_revision": cloudWorkerNested("integer", "positive_integer"),
			"provider":               cloudWorkerNested("string", "nonempty"),
			"model":                  cloudWorkerNested("string", "nonempty"),
			"interface":              cloudWorkerNested("string", "nonempty"),
			"credential_version":     cloudWorkerNested("integer", "positive_integer"),
		}),
		"aws": cloudWorkerObject("strict_aws_binding", map[string]ActionFieldSchema{
			"account_id":          cloudWorkerNested("string", "aws_account_id"),
			"region":              cloudWorkerNested("string", "aws_region"),
			"credential_revision": cloudWorkerNested("integer", "positive_integer"),
		}),
		"compute": cloudWorkerObject("strict_single_worker_compute", map[string]ActionFieldSchema{
			"instance_type":              cloudWorkerNested("string", "nonempty"),
			"architecture":               cloudWorkerNested("string", "nonempty"),
			"root_device_name":           cloudWorkerNested("string", "nonempty"),
			"volume_gib":                 cloudWorkerNested("integer", "positive_integer"),
			"volume_type":                cloudWorkerNested("string", "nonempty"),
			"volume_iops":                cloudWorkerNested("integer", "positive_integer"),
			"volume_throughput_mib":      cloudWorkerNested("integer", "positive_integer"),
			"ami_id":                     cloudWorkerNested("string", "nonempty"),
			"ami_digest":                 cloudWorkerNested("string", "lowercase_sha256"),
			"worker_release_digest":      cloudWorkerNested("string", "lowercase_sha256"),
			"pi_runtime_digest":          cloudWorkerNested("string", "lowercase_sha256"),
			"host_network_policy_sha256": cloudWorkerNested("string", "lowercase_sha256"),
		}),
		"limits": cloudWorkerObject("strict_hard_limits", map[string]ActionFieldSchema{
			"max_runtime_seconds": cloudWorkerNested("integer", "positive_integer"),
			"max_tokens":          cloudWorkerNested("integer", "positive_integer"),
			"max_output_bytes":    cloudWorkerNested("integer", "positive_integer"),
		}),
		"network_grants": cloudWorkerArray("strict_approved_network_grants", ActionFieldSchema{Type: "string", Required: true}),
		"secret_grants": cloudWorkerArray("strict_purpose_only_secret_grants_without_references", ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
			"purpose": cloudWorkerNested("string", "nonempty_bounded_64"),
		}}),
		"artifact_retention_seconds": cloudWorkerConditional("integer", "positive_integer"),
		"quote": cloudWorkerObject("strict_quote_and_owner_hard_limit", map[string]ActionFieldSchema{
			"amount_micros":                  cloudWorkerNested("integer", "nonnegative_integer"),
			"currency":                       cloudWorkerNested("string", "exact:USD"),
			"source_time":                    cloudWorkerNested("string", "rfc3339_nano"),
			"expires_at":                     cloudWorkerNested("string", "rfc3339_nano_after_source_time"),
			"maximum_authorized_cost_micros": cloudWorkerNested("integer", "integer_greater_than_or_equal_to_amount"),
			"digest":                         cloudWorkerNested("string", "lowercase_sha256"),
		}),
		"execution_digest": cloudWorkerConditional("string", "lowercase_sha256"),
		"created_at":       cloudWorkerConditional("string", "rfc3339_nano"),
		"updated_at":       cloudWorkerConditional("string", "rfc3339_nano_not_before_created_at"),
	}
}

func cloudWorkerRunProperties() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"owner_id":               cloudWorkerConditional("string", "exact_prepared_permission_owner"),
		"account_generation":     cloudWorkerConditional("integer", "positive_integer_equal_to_prepared_permission_generation"),
		"run_id":                 cloudWorkerConditional("string", "canonical_uuid_equal_to_execution_id"),
		"execution_id":           cloudWorkerConditional("string", "canonical_uuid_equal_to_run_id"),
		"plan_id":                cloudWorkerConditional("string", "canonical_uuid"),
		"plan_revision":          cloudWorkerConditional("integer", "positive_integer"),
		"plan_digest":            cloudWorkerConditional("string", "lowercase_sha256"),
		"task_id":                cloudWorkerConditional("string", "canonical_uuid"),
		"confirmation_id":        cloudWorkerConditional("string", "canonical_uuid"),
		"conversation_id":        cloudWorkerConditional("string", "canonical_uuid"),
		"turn_id":                cloudWorkerConditional("string", "canonical_uuid"),
		"status":                 cloudWorkerConditional("string", "cloud_worker_execution_state"),
		"revision":               cloudWorkerConditional("integer", "positive_integer"),
		"digest":                 cloudWorkerConditional("string", "lowercase_sha256"),
		"workspace_mode":         cloudWorkerConditional("string", "one_of:none|read_only|write"),
		"quote_digest":           cloudWorkerConditional("string", "lowercase_sha256"),
		"execution_digest":       cloudWorkerConditional("string", "lowercase_sha256"),
		"cancellation_requested": cloudWorkerConditional("boolean", "authoritative_cancel_intent_flag"),
		"cleanup": cloudWorkerObject("strict_verified_cleanup_summary", map[string]ActionFieldSchema{
			"verified_destroyed":           cloudWorkerNested("boolean", "true_only_after_all_resources_destroyed"),
			"verified_at":                  {Type: "string", Presence: &ActionPresenceSchema{Omitted: "cleanup_not_verified", Present: "rfc3339_nano_when_verified"}},
			"resources_total":              cloudWorkerNested("integer", "nonnegative_integer"),
			"resources_verified_destroyed": cloudWorkerNested("integer", "between_zero_and_resources_total"),
		}),
		"artifact_ids":    cloudWorkerArray("canonical_uuid_array", ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}),
		"failure_code":    cloudWorkerConditional("string", "redacted_failure_code_or_empty"),
		"failure_summary": cloudWorkerConditional("string", "redacted_failure_summary_or_empty"),
		"created_at":      cloudWorkerConditional("string", "rfc3339_nano"),
		"updated_at":      cloudWorkerConditional("string", "rfc3339_nano_not_before_created_at"),
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
		"progress": {
			Type: "object",
			Presence: &ActionPresenceSchema{
				Omitted: "type_is_not_worker_progress",
				Present: "required_when_type_is_worker_progress;forbidden_otherwise;strict_secret_free_snapshot",
			},
			Properties: map[string]ActionFieldSchema{
				"phase":                   cloudWorkerNested("string", "one_of:claimed|preparing_inputs|running_pi|uploading_result|completing"),
				"elapsed_ms":              cloudWorkerNested("integer", "integer_0_to_86400000"),
				"last_activity_at":        cloudWorkerNested("string", "rfc3339_nano_not_after_event_at"),
				"cpu_time_ms":             cloudWorkerNested("integer", "integer_0_to_604800000"),
				"memory_high_water_bytes": cloudWorkerNested("integer", "integer_0_to_68719476736"),
				"invocation_count":        cloudWorkerNested("integer", "integer_0_to_1000000"),
				"uploaded_bytes":          cloudWorkerNested("integer", "integer_0_to_9437184"),
				"output_truncated":        cloudWorkerNested("boolean", "authoritative_runtime_output_truncation_flag"),
			},
		},
	}
}

func executionV2CloudWorkerObjectResponse(name string, properties map[string]ActionFieldSchema, genericOwnerRequired bool) map[string]ActionFieldSchema {
	if genericOwnerRequired {
		owner := properties["owner_id"]
		owner.Required = true
		properties["owner_id"] = owner
	}
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

func executionV2OwnedObject(name string) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{name: {
		Type:       "object",
		Required:   true,
		Properties: map[string]ActionFieldSchema{"owner_id": {Type: "string", Required: true}},
	}}
}

func executionV2OwnedPageResponse(item string) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		item: {
			Type:     "array",
			Required: true,
			Items: &ActionFieldSchema{
				Type:       "object",
				Properties: map[string]ActionFieldSchema{"owner_id": {Type: "string", Required: true}},
			},
		},
		"next_page_token": {Type: "string", Required: true},
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

func executionV2EventsResponse() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"events":        {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object"}},
		"next_sequence": {Type: "integer", Required: true},
	}
}

func executionV2PlanSelection(fields map[string]ActionFieldSchema) map[string]ActionFieldSchema {
	fields["intent"] = ActionFieldSchema{Type: "string", Required: true}
	fields["recipe_id"] = ActionFieldSchema{Type: "string", Required: true}
	fields["target_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
	fields["target_revision"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}}
	fields["purpose"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:service|job"}}
	fields["ai_configuration"] = ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"mode": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:api_key|auth_gate"}}, "provider": {Type: "string", Required: true},
		"secret_ref": {Type: "string"}, "secret_revision": {Type: "integer"}, "secret_purpose": {Type: "string"},
		"secret_binding_digest": {Type: "string"}, "status": {Type: "string"},
	}}
	return fields
}

func executionV2SecretResponse() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"secret": {Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
		"secret_ref": {Type: "string", Required: true}, "revision": {Type: "integer", Required: true},
		"purpose": {Type: "string", Required: true}, "provider": {Type: "string", Required: true},
		"binding_digest": {Type: "string", Required: true}, "status": {Type: "string", Required: true},
		"created_at": {Type: "string", Required: true}, "updated_at": {Type: "string", Required: true},
	}}}
}

// executionV2Schema is intentionally action-specific. ProductCore does not
// enforce ActionSchema at runtime, so this file and the strict boundary
// decoder are tested for exact key parity.
func executionV2Schema(name string) *ActionSchema {
	object := func(name string) map[string]ActionFieldSchema {
		return map[string]ActionFieldSchema{name: {Type: "object", Required: true}}
	}
	get := func(id, result string) *ActionSchema {
		return &ActionSchema{
			Request:  map[string]ActionFieldSchema{id: {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}},
			Response: object(result),
		}
	}

	switch name {
	case "projects.analyze":
		request := executionV2MutationBase()
		request["project_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
		request["source"] = ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
			"kind": {Type: "string", Required: true}, "location": {Type: "string"},
			"commit": {Type: "string"}, "artifact_id": {Type: "string"},
			"credential_ref": {Type: "string"}, "credential_revision": {Type: "integer"},
			"immutable": {Type: "boolean", Required: true},
		}}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("analysis")}
	case "analyses.get":
		return &ActionSchema{
			Request:  map[string]ActionFieldSchema{"analysis_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}},
			Response: executionV2OwnedObject("analysis"),
		}
	case "targets.list":
		return &ActionSchema{Request: executionV2PageRequest(), Response: executionV2OwnedPageResponse("targets")}
	case "targets.get":
		request := map[string]ActionFieldSchema{"target_id": {Type: "string", Required: true}, "revision": {Type: "integer"}}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("target")}
	case "targets.import":
		request := executionV2MutationBase()
		request["credential_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
		request["credential_revision"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}}
		request["instance_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "aws_ec2_instance_id"}}
		response := executionV2OwnedObject("target")
		response["observation_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
		response["observation"] = ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{"owner_id": {Type: "string", Required: true}}}
		return &ActionSchema{Request: request, Response: response}
	case "targets.reserve":
		request := executionV2MutationBase()
		request["credential_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
		request["credential_revision"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}}
		request["instance_type"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "aws_instance_type"}}
		request["volume_gib"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "range_8_16384"}}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("target")}
	case "targets.observe":
		request := executionV2MutationBase()
		request["target_id"] = ActionFieldSchema{Type: "string", Required: true}
		request["target_revision"] = ActionFieldSchema{Type: "integer", Required: true}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("observation")}
	case "plans.create":
		request := executionV2PlanSelection(executionV2MutationBase())
		request["project_id"] = ActionFieldSchema{Type: "string", Required: true}
		request["analysis_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("plan")}
	case "plans.revise":
		request := executionV2PlanSelection(executionV2RevisionMutationBase())
		request["plan_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("plan")}
	case "plans.get":
		request := executionV2CloudWorkerRoute(map[string]ActionFieldSchema{"plan_id": {Type: "string", Required: true}, "revision": {Type: "integer"}})
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerObjectResponse("plan", cloudWorkerPlanProperties(), true)}
	case "plans.list":
		request := executionV2CloudWorkerRoute(executionV2PageRequest())
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerPageResponse("plans", cloudWorkerPlanProperties())}
	case "deployments.list":
		request := executionV2PageRequest()
		request["project_id"] = ActionFieldSchema{Type: "string"}
		return &ActionSchema{Request: request, Response: executionV2PageResponse("deployments")}
	case "deployments.get":
		return get("deployment_id", "deployment")
	case "deployments.events":
		return &ActionSchema{Request: executionV2EventsRequest("deployment_id"), Response: executionV2EventsResponse()}
	case "runs.create":
		request := executionV2MutationBase()
		request["plan_id"] = ActionFieldSchema{Type: "string", Required: true}
		request["plan_revision"] = ActionFieldSchema{Type: "integer", Required: true}
		request["operation"] = ActionFieldSchema{Type: "string", Required: true}
		request["trigger_kind"] = ActionFieldSchema{Type: "string"}
		request["rollback_of_run_id"] = ActionFieldSchema{Type: "string"}
		response := object("run")
		response["stages"] = ActionFieldSchema{Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object"}}
		return &ActionSchema{Request: request, Response: response}
	case "runs.get":
		response := executionV2CloudWorkerObjectResponse("run", cloudWorkerRunProperties(), false)
		response["stages"] = ActionFieldSchema{Type: "array", Items: &ActionFieldSchema{Type: "object"}, Presence: &ActionPresenceSchema{Omitted: "record_kind=cloud_worker", Present: "generic_execution_v2_stage_projection"}}
		return &ActionSchema{Request: executionV2CloudWorkerRoute(map[string]ActionFieldSchema{"run_id": {Type: "string", Required: true}}), Response: response}
	case "runs.list":
		request := executionV2CloudWorkerRoute(executionV2PageRequest())
		request["project_id"] = ActionFieldSchema{Type: "string"}
		request["deployment_id"] = ActionFieldSchema{Type: "string"}
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerPageResponse("runs", cloudWorkerRunProperties())}
	case "runs.cancel":
		request := executionV2CloudWorkerRoute(executionV2RevisionMutationBase())
		request["run_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerObjectResponse("run", cloudWorkerRunProperties(), false)}
	case "runs.retry":
		request := executionV2RevisionMutationBase()
		request["run_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: object("run")}
	case "runs.events":
		return &ActionSchema{Request: executionV2CloudWorkerRoute(executionV2EventsRequest("run_id")), Response: executionV2CloudWorkerEventsResponse()}
	case "artifacts.get":
		request := executionV2CloudWorkerRoute(map[string]ActionFieldSchema{"artifact_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}})
		return &ActionSchema{Request: request, Response: executionV2CloudWorkerObjectResponse("artifact", cloudWorkerArtifactProperties(), false)}
	case "artifacts.download":
		request := map[string]ActionFieldSchema{
			"record_kind":     {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact:cloud_worker"}},
			"artifact_id":     {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"offset_bytes":    {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_0_to_8388607"}},
			"max_chunk_bytes": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "integer_1_to_524288"}},
		}
		return &ActionSchema{Request: request, Response: cloudWorkerArtifactDownloadProperties()}
	case "service_bindings.list":
		request := executionV2PageRequest()
		request["project_id"] = ActionFieldSchema{Type: "string"}
		return &ActionSchema{Request: request, Response: executionV2PageResponse("bindings")}
	case "service_bindings.get":
		return get("binding_id", "binding")
	case "service_bindings.invoke":
		request := executionV2RevisionMutationBase()
		request["binding_id"] = ActionFieldSchema{Type: "string", Required: true}
		request["operation"] = ActionFieldSchema{Type: "string", Required: true}
		request["input"] = ActionFieldSchema{Type: "object", Required: true}
		return &ActionSchema{Request: request, Response: object("result")}
	case "secrets.create":
		request := executionV2MutationBase()
		request["provider"] = ActionFieldSchema{Type: "string", Required: true}
		request["purpose"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact:ai_provider_api_key"}}
		request["value"] = ActionFieldSchema{Type: "string", Required: true, WriteOnly: true}
		return &ActionSchema{Request: request, Response: executionV2SecretResponse()}
	case "secrets.get":
		return &ActionSchema{Request: map[string]ActionFieldSchema{"secret_ref": {Type: "string", Required: true}, "revision": {Type: "integer"}}, Response: executionV2SecretResponse()}
	case "secrets.list":
		return &ActionSchema{Request: executionV2PageRequest(), Response: map[string]ActionFieldSchema{"secrets": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: executionV2SecretResponse()["secret"].Properties}}, "next_page_token": {Type: "string", Required: true}}}
	case "secrets.revoke":
		request := executionV2RevisionMutationBase()
		request["secret_ref"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: executionV2SecretResponse()}
	default:
		return nil
	}
}
