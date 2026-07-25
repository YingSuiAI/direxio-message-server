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
	return coreActionSchema(coreRequired("idempotency_key", "name", "task_template", "trigger"), coreObjectResponse("schedule"))
}
func coreScheduleGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("schedule_id"), coreObjectResponse("schedule"))
}
func coreScheduleListSchema() *ActionSchema {
	return coreActionSchema(corePageFields(), map[string]ActionFieldSchema{"schedules": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_page_token": {Type: "string"}})
}
func coreScheduleUpdateSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("schedule_id"), coreObjectResponse("schedule"))
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
func coreExtensionRemoveSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("installation_id"), map[string]ActionFieldSchema{"installation": {Type: "object"}, "confirmation_id": {Type: "string"}, "task_id": {Type: "string"}})
}
func coreExtensionExecuteSchema() *ActionSchema {
	request := coreMutationFields("installation_id")
	request["input"] = ActionFieldSchema{Type: "object"}
	request["tool_name"] = ActionFieldSchema{Type: "string", Presence: &ActionPresenceSchema{Omitted: "skill_execution", Present: "MCP_tool_name"}}
	return coreActionSchema(request, map[string]ActionFieldSchema{"task_id": {Type: "string"}})
}
func coreMCPToolsSchema() *ActionSchema {
	return coreActionSchema(coreMutationFields("installation_id"), map[string]ActionFieldSchema{"tools": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}})
}
