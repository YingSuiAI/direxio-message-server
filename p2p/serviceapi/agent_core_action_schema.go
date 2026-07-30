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
	return coreActionSchema(coreRequired("confirmation_id"), map[string]ActionFieldSchema{"confirmation": {Type: "object", Required: true, Properties: coreConfirmationResponseFields()}})
}
func coreConfirmationListSchema() *ActionSchema {
	request := corePageFields()
	request["operation_domain"] = ActionFieldSchema{Type: "string"}
	request["target_id"] = ActionFieldSchema{Type: "string"}
	request["states"] = ActionFieldSchema{Type: "array", Items: &ActionFieldSchema{Type: "string"}}
	return coreActionSchema(request, map[string]ActionFieldSchema{"confirmations": {Type: "array", Items: &ActionFieldSchema{Type: "object", Properties: coreConfirmationResponseFields()}}, "next_page_token": {Type: "string"}})
}
func coreConfirmationMutationSchema() *ActionSchema {
	request := coreMutationFields("confirmation_id")
	request["expected_revision"] = ActionFieldSchema{Type: "integer", Required: true}
	return coreActionSchema(request, map[string]ActionFieldSchema{"confirmation": {Type: "object", Required: true, Properties: coreConfirmationResponseFields()}})
}

// coreConfirmationResponseFields mirrors confirmationMap, the single public
// confirmation projection used by confirmation actions and AWS/Workload
// change requests. It contains only immutable binding facts and digests; no
// credential or other secret values cross this wire boundary.
func coreConfirmationResponseFields() map[string]ActionFieldSchema {
	secretGrant := &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"reference_id":    {Type: "string", Required: true},
		"purpose":         {Type: "string", Required: true},
		"secret_revision": {Type: "integer", Presence: &ActionPresenceSchema{Present: "positive_integer", Empty: "omitted"}},
		"binding_digest":  {Type: "string", Required: true},
	}}
	binding := map[string]ActionFieldSchema{
		"owner_id":            {Type: "string"},
		"operation_domain":    {Type: "string", Required: true},
		"target_id":           {Type: "string", Required: true},
		"target_revision":     {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"target_kind":         {Type: "string"},
		"source_version":      {Type: "string"},
		"source_commit":       {Type: "string"},
		"content_digest":      {Type: "string", Required: true},
		"manifest_digest":     {Type: "string"},
		"execution_digest":    {Type: "string"},
		"permission_digest":   {Type: "string"},
		"parameter_digest":    {Type: "string", Required: true},
		"network_digest":      {Type: "string", Required: true},
		"secret_grant_digest": {Type: "string", Required: true},
		"selected_tool":       {Type: "string"},
		"selected_command":    {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
		"network_grants":      {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
		"secret_grants":       {Type: "array", Required: true, Items: secretGrant},
	}
	return map[string]ActionFieldSchema{
		"confirmation_id":            {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"expected_workload_revision": {Type: "integer", Presence: &ActionPresenceSchema{Present: "nonnegative_integer", Omitted: "when_no_workload_fence"}},
		"task_id":                    {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"state":                      awsEnumField("pending|confirmed|consumed|rejected|expired", true),
		"revision":                   {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"created_at":                 {Type: "string", Required: true},
		"updated_at":                 {Type: "string", Required: true},
		"expires_at":                 {Type: "string", Required: true},
		"terminal_reason":            {Type: "string"},
		"terminal_code":              {Type: "string"},
		"terminal_note":              {Type: "string"},
		"binding":                    {Type: "object", Required: true, Properties: binding},
	}
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
	execution := map[string]ActionFieldSchema{
		"remote": {Type: "object", Properties: map[string]ActionFieldSchema{"url": {Type: "string", Required: true}, "credential_reference_id": {Type: "string", Required: true}}},
	}
	return map[string]ActionFieldSchema{
		"candidate": coreExtensionCandidateSchema(), "content_digest": {Type: "string", Required: true}, "manifest_digest": {Type: "string", Required: true}, "execution_digest": {Type: "string", Required: true}, "network_schema_digest": {Type: "string", Required: true}, "secret_schema_digest": {Type: "string", Required: true},
		"execution":      {Type: "object", Required: true, Properties: execution},
		"network_grants": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"scheme": {Type: "string", Required: true}, "host": {Type: "string", Required: true}, "port": {Type: "integer", Required: true}, "path_prefix": {Type: "string", Required: true}, "digest": {Type: "string", Required: true}}}},
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
	expected := s.Request["expected_revision"]
	expected.Required = true
	s.Request["expected_revision"] = expected
	return s
}
func coreSkillExecuteSchema() *ActionSchema { return coreExtensionExecuteSchema() }
func coreMCPToolsSchema() *ActionSchema {
	request := coreMutationFields("installation_id")
	expected := request["expected_revision"]
	expected.Required = true
	request["expected_revision"] = expected
	return coreActionSchema(request, map[string]ActionFieldSchema{"tools": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}})
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
func workloadSecretGrantSchema(bindingDigestRequired bool) *ActionFieldSchema {
	bindingDigest := ActionFieldSchema{Type: "string", Required: bindingDigestRequired}
	if !bindingDigestRequired {
		bindingDigest.Presence = &ActionPresenceSchema{
			Omitted: "allowed_only_when_purpose_is_aws_credential; server_pins_the_owner_bound_encrypted_credential_revision",
			Present: "required_for_non_aws_grants; aws_credential_values_are_replaced_by_the_server_pinned_digest",
		}
	}
	return &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"reference_id": {Type: "string", Required: true}, "purpose": {Type: "string", Required: true}, "secret_revision": {Type: "integer"}, "binding_digest": bindingDigest}}
}
func workloadPlanResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"plan_id": {Type: "string", Required: true}, "revision": {Type: "integer", Required: true}, "digest": {Type: "string", Required: true}, "summary": {Type: "string", Required: true}, "artifact": {Type: "string", Required: true}, "source": {Type: "string", Required: true}, "command_steps": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}}, "image_digest": {Type: "string", Required: true}, "image_uri": {Type: "string", Required: true}, "target_kind": {Type: "string", Required: true}, "expires_at": {Type: "string", Required: true}, "created_at": {Type: "string", Required: true}, "typed_target": {Type: "object", Required: true, Properties: workloadTargetFields(true)}, "typed_resource_limits": {Type: "object", Properties: workloadLimitsFields(true)}, "typed_secret_grants": {Type: "array", Required: true, Items: workloadSecretGrantSchema(true)}}
}
func workloadDesiredPlanFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"plan_id": {Type: "string", Required: true}, "plan_revision": {Type: "integer", Required: true}, "plan_digest": {Type: "string", Required: true}, "target": {Type: "object", Required: true, Properties: workloadTargetFields(true)}, "resource_limits": {Type: "object", Required: true, Properties: workloadLimitsFields(true)}, "secret_grants": {Type: "array", Required: true, Items: workloadSecretGrantSchema(true)}}
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
	return map[string]ActionFieldSchema{
		"operation_id":               {Type: "string", Required: true},
		"workload_id":                {Type: "string", Required: true},
		"expected_workload_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "nonnegative_integer"}},
		"plan_id":                    {Type: "string", Required: true},
		"kind":                       {Type: "string", Required: true},
		"plan_revision":              {Type: "integer", Required: true},
		"plan_digest":                {Type: "string", Required: true},
		"summary":                    {Type: "string", Required: true},
		"target_kind":                {Type: "string", Required: true},
		"task_id":                    {Type: "string", Required: true},
		"confirmation_id":            {Type: "string", Required: true},
		"status":                     {Type: "string", Required: true},
		"revision":                   {Type: "integer", Required: true},
		"failure_code":               {Type: "string", Required: true},
		"failure_summary":            {Type: "string", Required: true},
		"created_at":                 {Type: "string", Required: true},
		"updated_at":                 {Type: "string", Required: true},
		"desired_plan":               {Type: "object", Required: true, Properties: workloadDesiredPlanFields()},
		// These fields are the server-derived immutable confirmation binding
		// projected on every workload operation response. They contain only
		// target identity, revisions, digests and non-secret network references.
		"target_id":            {Type: "string", Required: true},
		"target_revision":      {Type: "integer", Required: true},
		"content_digest":       {Type: "string", Required: true},
		"parameter_digest":     {Type: "string", Required: true},
		"network_digest":       {Type: "string", Required: true},
		"secret_grant_digest":  {Type: "string", Required: true},
		"network_grants":       {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}},
		"secret_grant_refs":    {Type: "array", Required: true, Items: workloadSecretGrantSchema(true)},
		"actual":               {Type: "object", Properties: workloadActualFields()},
		"dispatch_epoch":       {Type: "integer", Required: true},
		"dispatch_lease_until": {Type: "string", Required: true},
	}
}

func coreWorkloadPlanSchema() *ActionSchema {
	req := coreRequired("idempotency_key", "summary", "artifact", "source", "target_kind", "expires_at", "typed_target")
	req["typed_target"] = ActionFieldSchema{Type: "object", Required: true, Properties: workloadTargetFields(false)}
	req["command_steps"] = ActionFieldSchema{Type: "array", Items: &ActionFieldSchema{Type: "string"}}
	req["image_digest"] = ActionFieldSchema{Type: "string"}
	req["image_uri"] = ActionFieldSchema{Type: "string"}
	req["typed_resource_limits"] = ActionFieldSchema{Type: "object", Properties: workloadLimitsFields(false)}
	req["typed_secret_grants"] = ActionFieldSchema{Type: "array", WriteOnly: true, Items: workloadSecretGrantSchema(false)}
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
	return coreActionSchema(req, map[string]ActionFieldSchema{"operation": {Type: "object", Required: true, Properties: workloadOperationFields()}, "confirmation": {Type: "object", Required: true, Properties: coreConfirmationResponseFields()}, "task_id": {Type: "string", Required: true}, "expected_workload_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "nonnegative_integer"}}})
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
func coreWorkloadOperationReconcileSchema() *ActionSchema {
	return coreActionSchema(coreRequired("operation_id"), map[string]ActionFieldSchema{"operation": {Type: "object", Required: true, Properties: workloadOperationFields()}})
}
func coreWorkloadActualGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("workload_id"), map[string]ActionFieldSchema{"workload": {Type: "object", Required: true, Properties: workloadActualFields()}})
}

func coreDashboardGetSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"recent_limit": {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "bounded_default"}}}, map[string]ActionFieldSchema{"summary": {Type: "object", Required: true}, "deployments": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object"}}, "partial": {Type: "boolean", Required: true}, "warnings": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}}, "observed_at": {Type: "string", Required: true}})
}
func coreDeploymentsListSchema() *ActionSchema {
	req := corePageFields()
	req["status"] = ActionFieldSchema{Type: "string"}
	req["target_kind"] = ActionFieldSchema{Type: "string"}
	return coreActionSchema(req, map[string]ActionFieldSchema{"deployments": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: deploymentResponseFields()}}, "next_page_token": {Type: "string"}})
}

func deploymentResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"deployment_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"workload_id":   {Type: "string", Presence: &ActionPresenceSchema{Omitted: "when_deployment_has_no_workload_linkage", Present: "canonical_uuid"}},
	}
}
func coreDeploymentsGetSchema() *ActionSchema {
	deploymentID := awsCanonicalUUIDField(false)
	deploymentID.Presence.Omitted = "allowed_when_workload_id_present"
	workloadID := awsCanonicalUUIDField(false)
	workloadID.Presence.Omitted = "allowed_when_deployment_id_present"
	return coreActionSchema(map[string]ActionFieldSchema{"deployment_id": deploymentID, "workload_id": workloadID}, map[string]ActionFieldSchema{"deployment": {Type: "object", Required: true, Properties: deploymentResponseFields()}, "current_operation": {Type: "object"}, "actual": {Type: "object"}})
}
func coreDeploymentsEventsSchema() *ActionSchema {
	deploymentID := awsCanonicalUUIDField(false)
	deploymentID.Presence.Omitted = "allowed_when_workload_id_present"
	workloadID := awsCanonicalUUIDField(false)
	workloadID.Presence.Omitted = "allowed_when_deployment_id_present"
	req := map[string]ActionFieldSchema{"deployment_id": deploymentID, "workload_id": workloadID}
	req["after_sequence"] = ActionFieldSchema{Type: "integer"}
	req["page_size"] = ActionFieldSchema{Type: "integer"}
	event := map[string]ActionFieldSchema{"event_id": {Type: "string", Required: true}, "deployment_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}, "workload_id": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "omitted_when_deployment_has_no_workload_linkage", Present: "canonical_uuid"}}, "operation_id": {Type: "string", Required: true}, "sequence": {Type: "integer", Required: true}, "type": {Type: "string", Required: true}, "status": {Type: "string", Required: true}, "message": {Type: "string", Required: true}, "occurred_at": {Type: "string", Required: true}, "actual": {Type: "object"}}
	return coreActionSchema(req, map[string]ActionFieldSchema{"events": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: event}}, "next_after_sequence": {Type: "integer", Required: true}})
}

func awsCredentialResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"credential_id":                {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"name":                         {Type: "string", Required: true},
		"region":                       {Type: "string", Required: true},
		"account_id":                   {Type: "string", Required: true},
		"user_arn":                     {Type: "string", Required: true},
		"access_key_configured":        {Type: "boolean", Required: true},
		"secret_access_key_configured": {Type: "boolean", Required: true},
		"session_token_configured":     {Type: "boolean", Required: true},
		"verified_revision":            {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "nonnegative_integer"}},
		"revision":                     {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"created_at":                   {Type: "string", Required: true},
		"updated_at":                   {Type: "string", Required: true},
		"tested_at":                    {Type: "string", Presence: &ActionPresenceSchema{Omitted: "when_verified_revision_differs", Present: "when_verified_revision_matches_revision"}},
	}
}

func coreAWSCredentialCreateSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"idempotency_key": {Type: "string", Required: true}, "name": {Type: "string", Required: true}, "region": {Type: "string", Required: true}, "access_key_id": {Type: "string", Required: true, WriteOnly: true}, "secret_access_key": {Type: "string", Required: true, WriteOnly: true}, "session_token": {Type: "string", WriteOnly: true}}, map[string]ActionFieldSchema{"credential": {Type: "object", Required: true, Properties: awsCredentialResponseFields()}})
}
func coreAWSCredentialUpdateSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"idempotency_key": {Type: "string", Required: true}, "credential_id": {Type: "string", Required: true}, "expected_revision": {Type: "integer", Required: true}, "name": {Type: "string"}, "region": {Type: "string"}, "access_key_id": {Type: "string", WriteOnly: true}, "secret_access_key": {Type: "string", WriteOnly: true}, "session_token": {Type: "string", WriteOnly: true}}, map[string]ActionFieldSchema{"credential": {Type: "object", Required: true, Properties: awsCredentialResponseFields()}})
}
func coreAWSCredentialDeleteSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("credential_id"), map[string]ActionFieldSchema{"deleted": {Type: "boolean"}, "credential_id": {Type: "string"}})
}
func coreAWSCredentialListSchema() *ActionSchema {
	return coreActionSchema(corePageFields(), map[string]ActionFieldSchema{"credentials": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: awsCredentialResponseFields()}}, "next_page_token": {Type: "string"}})
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

// The EC2 provision actions intentionally expose a narrow typed boundary. In
// particular, callers cannot provide owner ids, CloudFormation templates,
// parameters, commands, images, provider overrides, or credential material.
// The owner is taken from the authenticated ProductCore request and the
// compiled provider pins the remaining deployment details.
func awsCanonicalUUIDField(required bool) ActionFieldSchema {
	p := &ActionPresenceSchema{Present: "canonical_uuid", Empty: "rejected"}
	if !required {
		p.Omitted = "omitted"
	}
	return ActionFieldSchema{Type: "string", Required: required, Presence: p}
}

func awsPositiveRevisionField(required bool) ActionFieldSchema {
	p := &ActionPresenceSchema{Present: "positive_integer", Empty: "rejected"}
	if !required {
		p.Omitted = "no_revision_filter"
	}
	return ActionFieldSchema{Type: "integer", Required: required, Presence: p}
}

func awsEnumField(values string, required bool) ActionFieldSchema {
	p := &ActionPresenceSchema{Present: "one_of:" + values, Empty: "rejected"}
	if !required {
		p.Omitted = "all_values"
	}
	return ActionFieldSchema{Type: "string", Required: required, Presence: p}
}

func awsEC2PlanResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"plan_id":                     {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"credential_id":               {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"credential_revision":         {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"region":                      {Type: "string", Required: true},
		"stack_name":                  {Type: "string", Required: true},
		"display_name":                {Type: "string", Required: true},
		"instance_type":               {Type: "string", Required: true},
		"volume_gib":                  {Type: "integer", Required: true},
		"public_http":                 {Type: "boolean", Required: true},
		"acknowledge_public_exposure": {Type: "boolean", Required: true},
		"operation":                   awsEnumField("create|delete", true),
		"template_sha256":             {Type: "string", Required: true},
		"plan_digest":                 {Type: "string", Required: true},
		"content_digest":              {Type: "string", Required: true},
		"parameter_digest":            {Type: "string", Required: true},
		"network_digest":              {Type: "string", Required: true},
		"secret_grant_digest":         {Type: "string", Required: true},
		"target_id":                   {Type: "string", Required: true},
		"revision":                    {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"created_at":                  {Type: "string", Required: true},
	}
}

func awsEC2QuoteResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"plan_id":               {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"operation":             awsEnumField("create|delete", true),
		"region":                {Type: "string", Required: true},
		"stack_name":            {Type: "string", Required: true},
		"resource_count":        {Type: "integer", Required: true},
		"parameter_count":       {Type: "integer", Required: true},
		"tag_count":             {Type: "integer", Required: true},
		"estimated_monthly_usd": {Type: "number", Required: true},
		"price_status":          {Type: "string", Required: true},
		"summary":               {Type: "string", Required: true},
		"plan_digest":           {Type: "string", Required: true},
	}
}

func awsEC2ReadbackResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"stack_id":          {Type: "string", Required: true},
		"instance_id":       {Type: "string", Required: true},
		"public_ip":         {Type: "string"},
		"security_group_id": {Type: "string", Required: true},
		"output_digest":     {Type: "string", Required: true},
		"observed_at":       {Type: "string", Required: true},
	}
}

func awsEC2ProvisionResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"provision_id":              {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"plan_id":                   {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"credential_id":             {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"credential_revision":       {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"plan_revision":             {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"region":                    {Type: "string", Required: true},
		"stack_name":                {Type: "string", Required: true},
		"profile":                   {Type: "string", Required: true},
		"template_sha256":           {Type: "string", Required: true},
		"plan_digest":               {Type: "string", Required: true},
		"credential_binding_digest": {Type: "string", Presence: &ActionPresenceSchema{Present: "immutable_digest", Empty: "omitted"}},
		"content_digest":            {Type: "string", Presence: &ActionPresenceSchema{Present: "immutable_digest", Empty: "omitted"}},
		"parameter_digest":          {Type: "string", Presence: &ActionPresenceSchema{Present: "immutable_digest", Empty: "omitted"}},
		"network_digest":            {Type: "string", Presence: &ActionPresenceSchema{Present: "immutable_digest", Empty: "omitted"}},
		"secret_grant_digest":       {Type: "string", Presence: &ActionPresenceSchema{Present: "immutable_digest", Empty: "omitted"}},
		"target_id":                 {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_aws_target", Empty: "omitted"}},
		"state":                     awsEnumField("planned|creating|active|destroying|destroyed|uncertain|failed", true),
		"revision":                  {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"create_change_id":          {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid", Empty: "omitted"}},
		"destroy_change_id":         {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid", Empty: "omitted"}},
		"active_change_id":          {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid", Empty: "omitted"}},
		"readback":                  {Type: "object", Properties: awsEC2ReadbackResponseFields(), Presence: &ActionPresenceSchema{Omitted: "empty_before_verified_readback", Present: "verified_readback_only", Empty: "empty_before_verified_readback"}},
		"reconciliation_required":   {Type: "boolean", Required: true},
		"error_code":                {Type: "string"},
		"error_summary":             {Type: "string"},
		"created_at":                {Type: "string", Required: true},
		"updated_at":                {Type: "string", Required: true},
	}
}

func awsEC2ChangeResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"change_id":               {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"plan_id":                 {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"provision_id":            {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid", Empty: "omitted"}},
		"credential_id":           {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"task_id":                 {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"confirmation_id":         {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"operation":               awsEnumField("create|delete", true),
		"status":                  awsEnumField("waiting_user|running|succeeded|failed|canceled", true),
		"stage":                   awsEnumField("requested|change_set_creating|change_set_ready|executing|reconciling|reconciliation_required|succeeded|failed|canceled", true),
		"change_set_id":           {Type: "string"},
		"provider_request_digest": {Type: "string"},
		"revision":                {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"error_code":              {Type: "string"},
		"error_summary":           {Type: "string"},
		"created_at":              {Type: "string", Required: true},
		"updated_at":              {Type: "string", Required: true},
	}
}

func awsEC2TaskResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"task_id":         {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"status":          {Type: "string", Required: true},
		"revision":        {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"plan_id":         {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"confirmation_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"failure_code":    {Type: "string"},
		"failure_summary": {Type: "string"},
	}
}

func awsEC2ConfirmationResponseFields() map[string]ActionFieldSchema {
	return coreConfirmationResponseFields()
}

func awsEC2ChangeEventResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"event_id":     {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"provision_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"sequence":     {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "monotonically_increasing_nonnegative_sequence"}},
		"change_id":    {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid", Empty: "omitted"}},
		"task_id":      {Type: "string", Presence: &ActionPresenceSchema{Present: "canonical_uuid", Empty: "omitted"}},
		"kind":         {Type: "string", Required: true},
		"revision":     {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"at":           {Type: "string", Required: true},
	}
}

func coreAWSEC2ProvisionPlanSchema() *ActionSchema {
	request := map[string]ActionFieldSchema{
		"credential_id":                awsCanonicalUUIDField(true),
		"expected_credential_revision": awsPositiveRevisionField(true),
		"region":                       {Type: "string", Required: true},
		"stack_name":                   {Type: "string", Required: true},
		"display_name":                 {Type: "string", Required: true},
		"instance_type":                {Type: "string", Required: true},
		"volume_gib":                   {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer", Empty: "rejected"}},
		"public_http":                  {Type: "boolean", Required: true},
		"acknowledge_public_exposure":  {Type: "boolean", Required: true, Presence: &ActionPresenceSchema{Present: "must_be_true", Empty: "rejected"}},
		"idempotency_key":              awsCanonicalUUIDField(true),
	}
	return coreActionSchema(request, map[string]ActionFieldSchema{
		"plan":      {Type: "object", Required: true, Properties: awsEC2PlanResponseFields()},
		"quote":     {Type: "object", Required: true, Properties: awsEC2QuoteResponseFields()},
		"provision": {Type: "object", Required: true, Properties: awsEC2ProvisionResponseFields()},
	})
}

func coreAWSEC2ProvisionGetSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"provision_id": awsCanonicalUUIDField(true)}, map[string]ActionFieldSchema{
		"provision": {Type: "object", Required: true, Properties: awsEC2ProvisionResponseFields()},
	})
}

func coreAWSEC2ProvisionListSchema() *ActionSchema {
	request := corePageFields()
	request["state"] = awsEnumField("planned|creating|active|destroying|destroyed|uncertain|failed", false)
	return coreActionSchema(request, map[string]ActionFieldSchema{
		"provisions":      {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: awsEC2ProvisionResponseFields()}},
		"next_page_token": {Type: "string"},
	})
}

func coreAWSEC2ProvisionEventsSchema() *ActionSchema {
	request := map[string]ActionFieldSchema{
		"provision_id":   awsCanonicalUUIDField(true),
		"after_sequence": {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "from_beginning", Present: "nonnegative_sequence", Empty: "rejected"}},
		"limit":          {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "server_default", Present: "positive_integer", Empty: "rejected"}},
	}
	return coreActionSchema(request, map[string]ActionFieldSchema{
		"events":              {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: awsEC2ChangeEventResponseFields()}},
		"next_after_sequence": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "nonnegative_sequence"}},
	})
}

func coreAWSEC2ProvisionChangeRequestSchema() *ActionSchema {
	request := map[string]ActionFieldSchema{
		"provision_id":      awsCanonicalUUIDField(true),
		"expected_revision": awsPositiveRevisionField(true),
		"idempotency_key":   awsCanonicalUUIDField(true),
	}
	return coreActionSchema(request, map[string]ActionFieldSchema{
		"provision":       {Type: "object", Required: true, Properties: awsEC2ProvisionResponseFields()},
		"change":          {Type: "object", Required: true, Properties: awsEC2ChangeResponseFields()},
		"task_id":         awsCanonicalUUIDField(true),
		"confirmation_id": awsCanonicalUUIDField(true),
		"task":            {Type: "object", Required: true, Properties: awsEC2TaskResponseFields()},
		"confirmation":    {Type: "object", Required: true, Properties: awsEC2ConfirmationResponseFields()},
	})
}

func coreAWSEC2ProvisionRetrySchema() *ActionSchema {
	request := map[string]ActionFieldSchema{
		"provision_id":      awsCanonicalUUIDField(true),
		"expected_revision": awsPositiveRevisionField(true),
		"idempotency_key":   awsCanonicalUUIDField(true),
	}
	return coreActionSchema(request, map[string]ActionFieldSchema{
		"provision": {Type: "object", Required: true, Properties: awsEC2ProvisionResponseFields()},
	})
}

func geoLibreTargetResponseFields() map[string]ActionFieldSchema {
	// geoLibrePlanMap publishes this deliberately flat, redacted target. Keep
	// this list exact: it is not the generic workload typed_target shape.
	return map[string]ActionFieldSchema{
		"provision_id":        {Type: "string", Required: true},
		"provision_revision":  {Type: "string", Required: true},
		"credential_id":       {Type: "string", Required: true},
		"credential_revision": {Type: "integer", Required: true},
		"account_id":          {Type: "string", Required: true},
		"region":              {Type: "string", Required: true},
		"instance_id":         {Type: "string", Required: true},
		"public_endpoint":     {Type: "string", Required: true},
		"service":             {Type: "string", Required: true},
		"port":                {Type: "integer", Required: true},
		"exposure":            {Type: "string", Required: true},
		"sidecar":             {Type: "string", Required: true},
	}
}

func geoLibrePlanResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"plan_id":               {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"revision":              {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"digest":                {Type: "string", Required: true},
		"summary":               {Type: "string", Required: true},
		"artifact":              {Type: "string", Required: true},
		"source":                {Type: "string", Required: true},
		"image_digest":          {Type: "string", Required: true},
		"target_kind":           {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "aws-ec2-ssm"}},
		"expires_at":            {Type: "string", Required: true},
		"created_at":            {Type: "string", Required: true},
		"typed_target":          {Type: "object", Required: true, Properties: geoLibreTargetResponseFields()},
		"typed_resource_limits": {Type: "object", Required: true, Properties: workloadLimitsFields(true)},
		"release": {Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
			"version": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "fixed_geolibre_release"}}, "commit": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "fixed_geolibre_commit"}}, "image_digest": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "fixed_geolibre_image_digest"}},
			"manifest_digest": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "fixed_geolibre_manifest_digest"}}, "command_digest": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "fixed_geolibre_command_digest"}}, "service": {Type: "string", Required: true},
			"port": {Type: "integer", Required: true}, "health_path": {Type: "string", Required: true},
		}},
	}
}

func geoLibreTaskResponseFields() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"task_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"goal":    {Type: "string"}, "conversation_id": {Type: "string"}, "model_profile_id": {Type: "string"},
		"attachment_refs": {Type: "array", Items: &ActionFieldSchema{Type: "string"}}, "knowledge_refs": {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
		"timeout_seconds": {Type: "integer"}, "status": {Type: "string", Required: true}, "attempt": {Type: "integer", Required: true},
		"lease_epoch": {Type: "integer", Required: true}, "available_at": {Type: "string"}, "retry_of_task_id": {Type: "string"},
		"failure_code": {Type: "string"}, "failure_summary": {Type: "string"}, "revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"expected_workload_revision": {Type: "integer", Presence: &ActionPresenceSchema{Present: "positive_integer", Omitted: "when_task_has_no_workload_fence"}},
		"kind":                       {Type: "string"}, "created_at": {Type: "string", Required: true}, "updated_at": {Type: "string", Required: true},
	}
}

func geoLibreOperationResponseFields() map[string]ActionFieldSchema {
	fields := workloadOperationFields()
	fields["summary"] = ActionFieldSchema{Type: "string", Required: true}
	return fields
}

func coreAWSEC2ProvisionGeoLibreInstallPlanSchema() *ActionSchema {
	request := map[string]ActionFieldSchema{
		"provision_id":      awsCanonicalUUIDField(true),
		"expected_revision": awsPositiveRevisionField(true),
		"expires_at":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact_utc_rfc3339", Empty: "rejected"}},
		"idempotency_key":   awsCanonicalUUIDField(true),
	}
	return coreActionSchema(request, map[string]ActionFieldSchema{
		"plan":               {Type: "object", Required: true, Properties: geoLibrePlanResponseFields()},
		"provision_id":       awsCanonicalUUIDField(true),
		"provision_revision": awsPositiveRevisionField(true),
		"expires_at":         {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact_utc_rfc3339"}},
	})
}

func coreAWSEC2ProvisionGeoLibreInstallRequestSchema() *ActionSchema {
	request := map[string]ActionFieldSchema{
		"provision_id":               awsCanonicalUUIDField(true),
		"expected_revision":          awsPositiveRevisionField(true),
		"plan_id":                    awsCanonicalUUIDField(true),
		"expires_at":                 {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact_utc_rfc3339", Empty: "rejected"}},
		"plan_revision":              awsPositiveRevisionField(true),
		"plan_digest":                {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "immutable_digest", Empty: "rejected"}},
		"idempotency_key":            awsCanonicalUUIDField(true),
		"workload_id":                awsCanonicalUUIDField(false),
		"expected_workload_revision": {Type: "integer", Presence: &ActionPresenceSchema{Present: "positive_integer_when_workload_id_present", Omitted: "omitted_when_workload_id_omitted", Empty: "rejected"}},
	}
	return coreActionSchema(request, map[string]ActionFieldSchema{
		"plan":                       {Type: "object", Required: true, Properties: geoLibrePlanResponseFields()},
		"provision_id":               awsCanonicalUUIDField(true),
		"provision_revision":         awsPositiveRevisionField(true),
		"expires_at":                 {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact_utc_rfc3339"}},
		"workload_id":                awsCanonicalUUIDField(true),
		"operation":                  {Type: "object", Required: true, Properties: geoLibreOperationResponseFields()},
		"task_id":                    awsCanonicalUUIDField(true),
		"task":                       {Type: "object", Required: true, Properties: geoLibreTaskResponseFields()},
		"confirmation_id":            awsCanonicalUUIDField(true),
		"confirmation":               {Type: "object", Required: true, Properties: coreConfirmationResponseFields()},
		"expected_workload_revision": {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "nonnegative_integer"}},
	})
}
