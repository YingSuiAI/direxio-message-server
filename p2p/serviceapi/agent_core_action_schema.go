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
func coreExtensionDiscoverSchema() *ActionSchema {
	request := corePageFields()
	request["query"] = ActionFieldSchema{Type: "string"}
	request["source"] = ActionFieldSchema{Type: "string"}
	return coreActionSchema(request, map[string]ActionFieldSchema{"candidates": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
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
	req["candidate"] = ActionFieldSchema{Type: "object", Required: true}
	req["secret_inputs"] = ActionFieldSchema{Type: "array", WriteOnly: true, Items: &ActionFieldSchema{Type: "object"}}
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
	return coreActionSchema(request, map[string]ActionFieldSchema{"task_id": {Type: "string"}})
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

func coreWorkloadPlanSchema() *ActionSchema {
	req := coreRequired("idempotency_key", "summary", "artifact", "source", "target_kind", "expires_at", "typed_target")
	identity := map[string]ActionFieldSchema{"kind": {Type: "string", Required: true}, "core_runner_service": {Type: "string"}, "image_digest": {Type: "string"}, "aws_account_id": {Type: "string"}, "aws_region": {Type: "string"}, "instance_id": {Type: "string"}, "cluster": {Type: "string"}, "service": {Type: "string"}, "task_definition_revision": {Type: "string"}, "desired_count": {Type: "integer"}, "endpoint": {Type: "string"}, "core_runner_id": {Type: "string"}, "aws_ec2_document_version": {Type: "string"}, "aws_ec2_systemd_service": {Type: "string"}, "aws_ec2_required_instance_tags": {Type: "object"}, "aws_ecs_cluster_arn": {Type: "string"}, "aws_ecs_service_name": {Type: "string"}, "aws_ecs_task_family": {Type: "string"}, "aws_ecs_platform_version": {Type: "string"}, "aws_ecs_subnet_ids": {Type: "array", Items: &ActionFieldSchema{Type: "string"}}, "aws_ecs_security_group_ids": {Type: "array", Items: &ActionFieldSchema{Type: "string"}}, "aws_ecs_assign_public_ip": {Type: "boolean"}, "aws_ecs_target_group_arn": {Type: "string"}, "aws_ecs_target_group_port": {Type: "integer"}, "aws_ecs_task_role_arn": {Type: "string"}, "aws_ecs_execution_role_arn": {Type: "string"}, "aws_ecs_desired_count": {Type: "integer"}, "aws_ecs_image_uri": {Type: "string"}}
	targetProps := map[string]ActionFieldSchema{"identity": {Type: "object", Required: true, Properties: identity}, "ports": {Type: "array", Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"port": {Type: "integer", Required: true}}}}, "network_grants": {Type: "array", Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"reference_id": {Type: "string", Required: true}, "kind": {Type: "string", Required: true}}}}, "labels": {Type: "object"}}
	req["typed_target"] = ActionFieldSchema{Type: "object", Required: true, Properties: targetProps}
	req["command_steps"] = ActionFieldSchema{Type: "array", Items: &ActionFieldSchema{Type: "string"}}
	req["image_digest"] = ActionFieldSchema{Type: "string"}
	req["image_uri"] = ActionFieldSchema{Type: "string"}
	req["typed_resource_limits"] = ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"cpu": {Type: "integer"}, "memory_mb": {Type: "integer"}, "processes": {Type: "integer"}, "disk_mb": {Type: "integer"}, "timeout_seconds": {Type: "integer"}, "output_mb": {Type: "integer"}}}
	req["typed_secret_grants"] = ActionFieldSchema{Type: "array", WriteOnly: true, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"reference_id": {Type: "string", Required: true}, "purpose": {Type: "string", Required: true}, "binding_digest": {Type: "string", Required: true}}}}
	return coreActionSchema(req, coreObjectResponse("plan"))
}
func coreWorkloadGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("plan_id"), coreObjectResponse("plan"))
}
func coreWorkloadListSchema() *ActionSchema {
	return coreActionSchema(corePageFields(), map[string]ActionFieldSchema{"plans": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
}
func coreWorkloadQuoteSchema() *ActionSchema {
	return coreActionSchema(coreRequired("plan_id"), coreObjectResponse("quote"))
}
func coreWorkloadApplySchema() *ActionSchema {
	req := coreRequired("idempotency_key", "plan_id")
	req["workload_id"] = ActionFieldSchema{Type: "string"}
	return coreActionSchema(req, map[string]ActionFieldSchema{"operation": {Type: "object"}, "confirmation": {Type: "object"}, "task_id": {Type: "string"}})
}
func coreWorkloadDestroySchema() *ActionSchema { return coreWorkloadApplySchema() }
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
