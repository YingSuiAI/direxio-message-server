package serviceapi

func coreRequired(names ...string) map[string]ActionFieldSchema {
	out := make(map[string]ActionFieldSchema, len(names))
	for _, name := range names {
		out[name] = ActionFieldSchema{Type: "string", Required: true}
	}
	return out
}

func corePageFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"page_size":  {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "server_default"}},
		"page_token": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "first_page"}},
	}
}

func coreMutationFields(names ...string) map[string]ActionFieldSchema {
	out := coreRequired(names...)
	out["idempotency_key"] = ActionFieldSchema{Type: "string", Required: true}
	out["expected_revision"] = ActionFieldSchema{Type: "integer", Presence: &ActionPresenceSchema{Omitted: "no_revision_check", Present: "must_match_current_revision"}}
	return out
}

func coreActionSchema(request map[string]ActionFieldSchema, response map[string]ActionFieldSchema) *ActionSchema {
	return &ActionSchema{Request: request, Response: response}
}

func coreObjectResponse(name string) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{name: {Type: "object"}}
}

func coreTaskGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("task_id"), coreObjectResponse("task"))
}
func coreTaskListSchema() *ActionSchema {
	request := corePageFields()
	request["status"] = ActionFieldSchema{Type: "string", Presence: &ActionPresenceSchema{Omitted: "all_statuses", Present: "normalized_core_task_status"}}
	return coreActionSchema(request, map[string]ActionFieldSchema{"tasks": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
}
func coreTaskMutationSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("task_id"), coreObjectResponse("task"))
}
func coreTaskEventsSchema() *ActionSchema {
	request := coreRequired("task_id")
	request["after_sequence"] = ActionFieldSchema{Type: "integer", Presence: &ActionPresenceSchema{Omitted: "from_beginning"}}
	request["limit"] = ActionFieldSchema{Type: "integer", Presence: &ActionPresenceSchema{Omitted: "bounded_default"}}
	return coreActionSchema(request, map[string]ActionFieldSchema{"events": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}})
}
func coreScheduleCreateSchema() *ActionSchema {
	req := coreRequired("idempotency_key", "name")
	req["task_template"] = ActionFieldSchema{Type: "object", Required: true}
	req["trigger"] = ActionFieldSchema{Type: "object", Required: true}
	return coreActionSchema(req, coreObjectResponse("schedule"))
}
func coreScheduleGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("schedule_id"), coreObjectResponse("schedule"))
}
func coreScheduleListSchema() *ActionSchema {
	return coreActionSchema(corePageFields(), map[string]ActionFieldSchema{"schedules": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
}
func coreScheduleUpdateSchema() *ActionSchema {
	req := coreMutationFields("schedule_id")
	req["name"] = ActionFieldSchema{Type: "string"}
	req["task_template"] = ActionFieldSchema{Type: "object"}
	req["trigger"] = ActionFieldSchema{Type: "object"}
	return coreActionSchema(req, coreObjectResponse("schedule"))
}
func coreScheduleMutationSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("schedule_id"), coreObjectResponse("schedule"))
}
func coreScheduleTriggerSchema() *ActionSchema {
	return coreActionSchema(coreRequired("idempotency_key", "schedule_id"), map[string]ActionFieldSchema{"schedule": {Type: "object"}, "occurrence_id": {Type: "string"}, "task_id": {Type: "string"}})
}
func coreScheduleDeleteSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("schedule_id"), map[string]ActionFieldSchema{"deleted": {Type: "boolean"}, "schedule_id": {Type: "string"}})
}
func coreConfirmationGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("confirmation_id"), coreObjectResponse("confirmation"))
}
func coreConfirmationListSchema() *ActionSchema {
	request := corePageFields()
	request["operation_domain"] = ActionFieldSchema{Type: "string"}
	request["target_id"] = ActionFieldSchema{Type: "string"}
	request["states"] = ActionFieldSchema{Type: "array", Items: &ActionFieldSchema{Type: "string"}}
	return coreActionSchema(request, map[string]ActionFieldSchema{"confirmations": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
}
func coreConfirmationMutationSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("confirmation_id"), coreObjectResponse("confirmation"))
}
func coreConfirmationExtensionUncertainAcknowledgeSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{
		"confirmation_id":                {Type: "string", Required: true},
		"task_id":                        {Type: "string", Required: true},
		"installation_id":                {Type: "string", Required: true},
		"expected_task_revision":         {Type: "integer", Required: true},
		"expected_confirmation_revision": {Type: "integer", Required: true},
		"resolution":                     {Type: "string", Required: true},
		"idempotency_key":                {Type: "string", Required: true},
	}, map[string]ActionFieldSchema{
		"confirmation": {Type: "object", Required: true}, "task": {Type: "object", Required: true},
		"resolution": {Type: "string", Required: true}, "reservation_released": {Type: "boolean", Required: true},
	})
}
func coreExtensionDiscoverSchema() *ActionSchema {
	request := corePageFields()
	request["query"] = ActionFieldSchema{Type: "string"}
	request["source"] = ActionFieldSchema{Type: "string"}
	candidate := coreExtensionCandidateSchema()
	return coreActionSchema(request, map[string]ActionFieldSchema{"candidates": {Type: "array", Items: &candidate}, "next_page_token": {Type: "string"}})
}
func coreExtensionInspectSchema() *ActionSchema {
	candidate := coreExtensionCandidateSchema()
	inspection := coreExtensionInspectionFields()
	return coreActionSchema(map[string]ActionFieldSchema{"candidate": candidate}, map[string]ActionFieldSchema{"inspection": {Type: "object", Required: true, Properties: inspection}})
}

func coreExtensionCandidateSchema() ActionFieldSchema {
	pin := map[string]ActionFieldSchema{"registry_version": {Type: "string", Required: true}, "registry_sha256": {Type: "string", Required: true}, "git_commit": {Type: "string", Required: true}, "git_sha256": {Type: "string", Required: true}}
	return ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{"id": {Type: "string", Required: true}, "kind": {Type: "string", Required: true}, "source": {Type: "string", Required: true}, "name": {Type: "string", Required: true}, "description": {Type: "string", Required: true}, "pin": {Type: "object", Required: true, Properties: pin}, "transport": {Type: "string", Required: true}}}
}

func coreExtensionInspectionFields() map[string]ActionFieldSchema {
	argv := ActionFieldSchema{Type: "array", Items: &ActionFieldSchema{Type: "string"}}
	execution := map[string]ActionFieldSchema{
		"stdio":  {Type: "object", Properties: map[string]ActionFieldSchema{"relative_path": {Type: "string", Required: true}, "digest": {Type: "string", Required: true}, "argv": argv}},
		"remote": {Type: "object", Properties: map[string]ActionFieldSchema{"url": {Type: "string", Required: true}, "credential_reference_id": {Type: "string"}}},
		"skill":  {Type: "object", Properties: map[string]ActionFieldSchema{"relative_path": {Type: "string", Required: true}, "digest": {Type: "string", Required: true}, "executable": {Type: "boolean", Required: true}, "argv": argv}},
	}
	return map[string]ActionFieldSchema{
		"candidate": coreExtensionCandidateSchema(), "content_digest": {Type: "string", Required: true}, "manifest_digest": {Type: "string", Required: true}, "execution_digest": {Type: "string", Required: true}, "network_schema_digest": {Type: "string", Required: true}, "secret_schema_digest": {Type: "string", Required: true},
		"execution":      {Type: "object", Required: true, Properties: execution},
		"network_grants": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"scheme": {Type: "string", Required: true}, "host": {Type: "string", Required: true}, "port": {Type: "integer", Required: true}, "path_prefix": {Type: "string"}, "digest": {Type: "string", Required: true}}}},
		"secret_grants":  {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"reference_id": {Type: "string", Required: true}, "purpose": {Type: "string", Required: true}, "binding_digest": {Type: "string", Required: true}, "configured": {Type: "boolean", Required: true}}}},
	}
}
func coreExtensionGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("installation_id"), coreObjectResponse("installation"))
}
func coreExtensionListSchema() *ActionSchema {
	request := corePageFields()
	request["source"] = ActionFieldSchema{Type: "string"}
	request["state"] = ActionFieldSchema{Type: "string"}
	return coreActionSchema(request, map[string]ActionFieldSchema{"installations": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
}
func coreExtensionMutationSchema() *ActionSchema {
	request := coreMutationFields()
	request["candidate"] = ActionFieldSchema{Type: "object", Required: true}
	request["secret_inputs"] = ActionFieldSchema{Type: "array", WriteOnly: true, Items: &ActionFieldSchema{Type: "object"}}
	return coreActionSchema(request, map[string]ActionFieldSchema{"installation": {Type: "object"}, "confirmation_id": {Type: "string"}, "task_id": {Type: "string"}})
}
func coreExtensionInstallSchema() *ActionSchema {
	req := coreMutationFields()
	req["candidate"] = coreExtensionCandidateSchema()
	req["inspection"] = ActionFieldSchema{Type: "object", Required: true, Properties: coreExtensionInspectionFields()}
	req["secret_inputs"] = ActionFieldSchema{Type: "array", WriteOnly: true, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"reference_id": {Type: "string", Required: true}, "purpose": {Type: "string", Required: true}, "secret_value": {Type: "string", Required: true, WriteOnly: true}}}}
	return coreActionSchema(req, map[string]ActionFieldSchema{"installation": {Type: "object"}, "confirmation_id": {Type: "string"}, "task_id": {Type: "string"}})
}
func coreExtensionUpdateSchema() *ActionSchema {
	s := coreExtensionInstallSchema()
	s.Request["installation_id"] = ActionFieldSchema{Type: "string", Required: true}
	return s
}
func coreExtensionRemoveSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("installation_id"), map[string]ActionFieldSchema{"installation": {Type: "object"}, "confirmation_id": {Type: "string"}, "task_id": {Type: "string"}})
}
func coreExtensionExecuteSchema() *ActionSchema {
	request := coreMutationFields("installation_id")
	request["input"] = ActionFieldSchema{Type: "object"}
	request["tool_name"] = ActionFieldSchema{Type: "string", Presence: &ActionPresenceSchema{Omitted: "skill_execution", Present: "MCP_tool_name"}}
	return coreActionSchema(request, map[string]ActionFieldSchema{"confirmation_id": {Type: "string", Required: true}, "task_id": {Type: "string", Required: true}})
}
func coreMCPExecuteSchema() *ActionSchema {
	s := coreExtensionExecuteSchema()
	s.Request["tool_name"] = ActionFieldSchema{Type: "string", Required: true}
	return s
}
func coreSkillExecuteSchema() *ActionSchema { return coreExtensionExecuteSchema() }
func coreMCPToolsSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("installation_id"), map[string]ActionFieldSchema{"tools": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}})
}

func workloadIdentityFields(required bool) map[string]ActionFieldSchema {
	f := map[string]ActionFieldSchema{"kind": {Type: "string", Required: true}, "core_runner_service": {Type: "string"}, "image_digest": {Type: "string"}, "aws_account_id": {Type: "string"}, "aws_region": {Type: "string"}, "instance_id": {Type: "string"}, "cluster": {Type: "string"}, "service": {Type: "string"}, "task_definition_revision": {Type: "string"}, "desired_count": {Type: "integer"}, "endpoint": {Type: "string"}, "core_runner_id": {Type: "string"}, "aws_ec2_document_version": {Type: "string"}, "aws_ec2_systemd_service": {Type: "string"}, "aws_ec2_required_instance_tags": {Type: "object"}, "aws_ecs_cluster_arn": {Type: "string"}, "aws_ecs_service_name": {Type: "string"}, "aws_ecs_task_family": {Type: "string"}, "aws_ecs_platform_version": {Type: "string"}, "aws_ecs_subnet_ids": {Type: "array", Items: &ActionFieldSchema{Type: "string"}}, "aws_ecs_security_group_ids": {Type: "array", Items: &ActionFieldSchema{Type: "string"}}, "aws_ecs_assign_public_ip": {Type: "boolean"}, "aws_ecs_target_group_arn": {Type: "string"}, "aws_ecs_target_group_port": {Type: "integer"}, "aws_ecs_task_role_arn": {Type: "string"}, "aws_ecs_execution_role_arn": {Type: "string"}, "aws_ecs_desired_count": {Type: "integer"}, "aws_ecs_image_uri": {Type: "string"}}
	if required {
		for key, field := range f {
			field.Required = true
			f[key] = field
		}
	}
	return f
}
func workloadTargetFields(required bool) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"identity": {Type: "object", Required: true, Properties: workloadIdentityFields(required)}, "ports": {Type: "array", Required: required, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"port": {Type: "integer", Required: true}}}}, "network_grants": {Type: "array", Required: required, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"reference_id": {Type: "string", Required: true}, "kind": {Type: "string", Required: true}}}}, "labels": {Type: "object", Required: required}}
}
func workloadLimitsFields(required bool) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"cpu": {Type: "integer", Required: required}, "memory_mb": {Type: "integer", Required: required}, "processes": {Type: "integer", Required: required}, "disk_mb": {Type: "integer", Required: required}, "timeout_seconds": {Type: "integer", Required: required}, "output_mb": {Type: "integer", Required: required}}
}
func workloadSecretGrantSchema() *ActionFieldSchema {
	return &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"reference_id": {Type: "string", Required: true}, "purpose": {Type: "string", Required: true}, "binding_digest": {Type: "string", Required: true}}}
}
func workloadPlanResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"plan_id": {Type: "string", Required: true}, "revision": {Type: "integer", Required: true}, "digest": {Type: "string", Required: true}, "summary": {Type: "string", Required: true}, "artifact": {Type: "string", Required: true}, "source": {Type: "string", Required: true}, "command_steps": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}}, "image_digest": {Type: "string", Required: true}, "image_uri": {Type: "string", Required: true}, "target_kind": {Type: "string", Required: true}, "expires_at": {Type: "string", Required: true}, "created_at": {Type: "string", Required: true}, "typed_target": {Type: "object", Required: true, Properties: workloadTargetFields(true)}, "typed_resource_limits": {Type: "object", Properties: workloadLimitsFields(true)}, "typed_secret_grants": {Type: "array", Required: true, Items: workloadSecretGrantSchema()}}
}
func workloadDesiredPlanFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"plan_id": {Type: "string", Required: true}, "plan_revision": {Type: "integer", Required: true}, "plan_digest": {Type: "string", Required: true}, "target": {Type: "object", Required: true, Properties: workloadTargetFields(true)}, "resource_limits": {Type: "object", Required: true, Properties: workloadLimitsFields(true)}, "secret_grants": {Type: "array", Required: true, Items: workloadSecretGrantSchema()}}
}
func workloadActualFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"workload_id": {Type: "string", Required: true}, "revision": {Type: "integer", Required: true}, "state": {Type: "string", Required: true}, "identity": {Type: "object", Required: true, Properties: workloadIdentityFields(true)}, "applied_plan_id": {Type: "string", Required: true}, "applied_plan_digest": {Type: "string", Required: true}, "readback_digest": {Type: "string", Required: true}, "provider_version": {Type: "string", Required: true}, "observed_at": {Type: "string", Required: true}, "updated_at": {Type: "string", Required: true}}
}

// Event readbacks are provider observations captured at event time. They are
// deliberately sparse and must not be presented as the current workload.
func workloadEventActualFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"workload_id": {Type: "string", Required: true}, "state": {Type: "string", Required: true}, "identity": {Type: "object", Required: true, Properties: workloadIdentityFields(true)}, "readback_digest": {Type: "string", Required: true}, "provider_version": {Type: "string", Required: true}, "observed_at": {Type: "string", Required: true}}
}
func workloadOperationFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"operation_id": {Type: "string", Required: true}, "workload_id": {Type: "string", Required: true}, "plan_id": {Type: "string", Required: true}, "kind": {Type: "string", Required: true}, "plan_revision": {Type: "integer", Required: true}, "plan_digest": {Type: "string", Required: true}, "target_kind": {Type: "string", Required: true}, "task_id": {Type: "string", Required: true}, "confirmation_id": {Type: "string", Required: true}, "status": {Type: "string", Required: true}, "revision": {Type: "integer", Required: true}, "failure_code": {Type: "string", Required: true}, "failure_summary": {Type: "string", Required: true}, "created_at": {Type: "string", Required: true}, "updated_at": {Type: "string", Required: true}, "desired_plan": {Type: "object", Required: true, Properties: workloadDesiredPlanFields()}, "actual": {Type: "object", Properties: workloadActualFields()}, "dispatch_epoch": {Type: "integer", Required: true}, "dispatch_lease_until": {Type: "string", Required: true}}
}

func coreWorkloadPlanSchema() *ActionSchema {
	req := coreRequired("idempotency_key", "summary", "artifact", "source", "target_kind", "expires_at", "typed_target")
	req["typed_target"] = ActionFieldSchema{Type: "object", Required: true, Properties: workloadTargetFields(false)}
	req["command_steps"] = ActionFieldSchema{Type: "array", Items: &ActionFieldSchema{Type: "string"}}
	req["image_digest"] = ActionFieldSchema{Type: "string"}
	req["image_uri"] = ActionFieldSchema{Type: "string"}
	req["typed_resource_limits"] = ActionFieldSchema{Type: "object", Properties: workloadLimitsFields(false)}
	req["typed_secret_grants"] = ActionFieldSchema{Type: "array", WriteOnly: true, Items: workloadSecretGrantSchema()}
	return coreActionSchema(req, map[string]ActionFieldSchema{"plan": {Type: "object", Required: true, Properties: workloadPlanResponseFields()}})
}
func coreWorkloadGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("plan_id"), map[string]ActionFieldSchema{"plan": {Type: "object", Required: true, Properties: workloadPlanResponseFields()}})
}
func coreWorkloadListSchema() *ActionSchema {
	return coreActionSchema(corePageFields(), map[string]ActionFieldSchema{"plans": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: workloadPlanResponseFields()}}, "next_page_token": {Type: "string"}})
}
func coreWorkloadQuoteSchema() *ActionSchema {
	return coreActionSchema(coreRequired("plan_id"), coreObjectResponse("quote"))
}
func coreWorkloadApplySchema() *ActionSchema {
	req := coreRequired("idempotency_key", "plan_id")
	req["workload_id"] = ActionFieldSchema{Type: "string"}
	return coreActionSchema(req, map[string]ActionFieldSchema{"operation": {Type: "object", Required: true, Properties: workloadOperationFields()}, "confirmation": {Type: "object", Required: true}, "task_id": {Type: "string", Required: true}})
}
func coreWorkloadDestroySchema() *ActionSchema { return coreWorkloadApplySchema() }
func coreWorkloadOperationGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("operation_id"), map[string]ActionFieldSchema{"operation": {Type: "object", Required: true, Properties: workloadOperationFields()}})
}
func coreWorkloadOperationEventsSchema() *ActionSchema {
	req := coreRequired("operation_id")
	req["after_sequence"] = ActionFieldSchema{Type: "integer", Presence: &ActionPresenceSchema{Omitted: "from_beginning", Present: "nonnegative_sequence"}}
	event := map[string]ActionFieldSchema{"operation_id": {Type: "string", Required: true}, "sequence": {Type: "integer", Required: true}, "kind": {Type: "string", Required: true}, "status": {Type: "string", Required: true}, "message": {Type: "string", Required: true}, "actual": {Type: "object", Properties: workloadEventActualFields(), Presence: &ActionPresenceSchema{Omitted: "no_event_readback", Present: "event_time_sparse_readback"}}, "at": {Type: "string", Required: true}}
	return coreActionSchema(req, map[string]ActionFieldSchema{"events": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: event}}})
}
func coreWorkloadActualGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("workload_id"), map[string]ActionFieldSchema{"workload": {Type: "object", Required: true, Properties: workloadActualFields()}})
}
func coreAWSCredentialCreateSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"idempotency_key": {Type: "string", Required: true}, "name": {Type: "string", Required: true}, "region": {Type: "string", Required: true}, "access_key_id": {Type: "string", Required: true, WriteOnly: true}, "secret_access_key": {Type: "string", Required: true, WriteOnly: true}, "session_token": {Type: "string", WriteOnly: true}}, coreObjectResponse("credential"))
}
func coreAWSCredentialUpdateSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"idempotency_key": {Type: "string", Required: true}, "credential_id": {Type: "string", Required: true}, "expected_revision": {Type: "integer", Required: true}, "name": {Type: "string"}, "region": {Type: "string"}, "access_key_id": {Type: "string", WriteOnly: true}, "secret_access_key": {Type: "string", WriteOnly: true}, "session_token": {Type: "string", WriteOnly: true}}, coreObjectResponse("credential"))
}
func coreAWSCredentialDeleteSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("credential_id"), map[string]ActionFieldSchema{"deleted": {Type: "boolean"}, "credential_id": {Type: "string"}})
}
func coreAWSCredentialListSchema() *ActionSchema {
	return coreActionSchema(corePageFields(), map[string]ActionFieldSchema{"credentials": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
}
func coreAWSCredentialTestSchema() *ActionSchema {
	return coreActionSchema(coreRequired("credential_id"), map[string]ActionFieldSchema{"credential_id": {Type: "string"}, "account_id": {Type: "string"}, "user_arn": {Type: "string"}, "principal_id": {Type: "string"}, "credential_revision": {Type: "integer"}, "tested_at": {Type: "string"}})
}
func coreAWSPlanReadSchema() *ActionSchema {
	return coreActionSchema(coreRequired("plan_id"), coreObjectResponse("plan"))
}
func coreAWSPlanListSchema() *ActionSchema {
	return coreActionSchema(corePageFields(), map[string]ActionFieldSchema{"plans": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
}
func coreAWSQuoteSchema() *ActionSchema {
	return coreActionSchema(coreRequired("plan_id"), coreObjectResponse("quote"))
}
func coreAWSChangeGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("change_id"), coreObjectResponse("change"))
}
func coreAWSChangeListSchema() *ActionSchema {
	req := corePageFields()
	req["plan_id"] = ActionFieldSchema{Type: "string"}
	return coreActionSchema(req, map[string]ActionFieldSchema{"changes": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
}
func coreAWSChangeStatusSchema() *ActionSchema {
	return coreActionSchema(coreRequired("change_id"), map[string]ActionFieldSchema{"change": {Type: "object"}, "status": {Type: "string"}, "stage": {Type: "string"}})
}
