package serviceapi

func embeddedScheduleCreateSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"name": {Type: "string", Required: true}, "prompt": {Type: "string", Required: true}, "model_profile_id": {Type: "string", Required: true}, "trigger": {Type: "object", Required: true}, "skip_if_running": {Type: "boolean"}, "idempotency_key": {Type: "string", Required: true}}, coreObjectResponse("schedule"))
}
func embeddedScheduleUpdateSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"schedule_id": {Type: "string", Required: true}, "name": {Type: "string"}, "prompt": {Type: "string"}, "model_profile_id": {Type: "string"}, "trigger": {Type: "object"}, "skip_if_running": {Type: "boolean"}, "idempotency_key": {Type: "string", Required: true}, "expected_revision": {Type: "integer", Required: true}}, coreObjectResponse("schedule"))
}
func embeddedScheduleGetSchema() *ActionSchema {
	return coreActionSchema(coreRequired("schedule_id"), coreObjectResponse("schedule"))
}
func embeddedScheduleListSchema() *ActionSchema {
	req := corePageFields()
	return coreActionSchema(req, map[string]ActionFieldSchema{"schedules": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_cursor": {Type: "string"}})
}
func embeddedScheduleMutationSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"schedule_id": {Type: "string", Required: true}, "idempotency_key": {Type: "string", Required: true}, "expected_revision": {Type: "integer", Required: true}}, map[string]ActionFieldSchema{"schedule": {Type: "object"}, "deleted": {Type: "boolean"}})
}
func embeddedScheduleRunsListSchema() *ActionSchema {
	req := map[string]ActionFieldSchema{"schedule_id": {Type: "string", Required: true}, "limit": {Type: "integer"}, "cursor": {Type: "string"}}
	return coreActionSchema(req, map[string]ActionFieldSchema{"runs": {Type: "array", Items: &ActionFieldSchema{Type: "object"}}, "next_cursor": {Type: "string"}})
}
func embeddedScheduleRunGetSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"schedule_id": {Type: "string", Required: true}, "run_id": {Type: "string", Required: true}}, map[string]ActionFieldSchema{"run": {Type: "object"}})
}

func embeddedScheduleRunNowSchema() *ActionSchema {
	return coreActionSchema(map[string]ActionFieldSchema{"schedule_id": {Type: "string", Required: true}, "idempotency_key": {Type: "string", Required: true}}, map[string]ActionFieldSchema{"run": {Type: "object"}})
}
