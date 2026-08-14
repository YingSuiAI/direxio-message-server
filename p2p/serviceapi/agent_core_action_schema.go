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
	revision := req["expected_revision"]
	revision.Required = true
	req["expected_revision"] = revision
	req["name"] = ActionFieldSchema{Type: "string"}
	req["task_template"] = ActionFieldSchema{Type: "object"}
	req["trigger"] = ActionFieldSchema{Type: "object"}
	return coreActionSchema(req, coreObjectResponse("schedule"))
}
func coreScheduleMutationSchema() *ActionSchema {
	req := coreMutationFields("schedule_id")
	revision := req["expected_revision"]
	revision.Required = true
	req["expected_revision"] = revision
	return coreActionSchema(req, coreObjectResponse("schedule"))
}
func coreScheduleTriggerSchema() *ActionSchema {
	return coreActionSchema(coreRequired("idempotency_key", "schedule_id"), map[string]ActionFieldSchema{"schedule": {Type: "object"}, "occurrence_id": {Type: "string"}, "task_id": {Type: "string"}})
}
func coreScheduleDeleteSchema() *ActionSchema {
	req := coreMutationFields("schedule_id")
	revision := req["expected_revision"]
	revision.Required = true
	req["expected_revision"] = revision
	return coreActionSchema(req, map[string]ActionFieldSchema{"deleted": {Type: "boolean"}, "schedule_id": {Type: "string"}})
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
// confirmation projection used by confirmation actions and control-plane
// requests. Cloud Worker confirmations use identity/revision and live quote
// facts only; other domains retain their existing immutable binding fields.
func coreConfirmationResponseFields() map[string]ActionFieldSchema {
	secretGrant := &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"reference_id": {Type: "string", Presence: &ActionPresenceSchema{
			Omitted: "cloud_worker.execute exposes purpose only", Present: "required_for_non_cloud_worker_confirmation",
		}},
		"purpose": {Type: "string", Required: true},
		"binding_digest": {Type: "string", Presence: &ActionPresenceSchema{
			Omitted: "cloud_worker.execute exposes purpose only", Present: "required_for_non_cloud_worker_confirmation",
		}},
	}}
	cloudOnlyString := func(present string) ActionFieldSchema {
		return ActionFieldSchema{Type: "string", Presence: &ActionPresenceSchema{Omitted: "non_cloud_worker_confirmation", Present: present}}
	}
	cloudOnlyInteger := func(present string) ActionFieldSchema {
		return ActionFieldSchema{Type: "integer", Presence: &ActionPresenceSchema{Omitted: "non_cloud_worker_confirmation", Present: present}}
	}
	cloudOnlyObject := func(present string, properties map[string]ActionFieldSchema) ActionFieldSchema {
		return ActionFieldSchema{Type: "object", Presence: &ActionPresenceSchema{Omitted: "non_cloud_worker_confirmation", Present: present}, Properties: properties}
	}
	nonCloudOnly := func(kind string) ActionFieldSchema {
		return ActionFieldSchema{Type: kind, Presence: &ActionPresenceSchema{Omitted: "cloud_worker.execute", Present: "required_for_non_cloud_worker_confirmation"}}
	}
	binding := map[string]ActionFieldSchema{
		"owner_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "authenticated_owner_id"}},
		"account_generation": {Type: "integer", Presence: &ActionPresenceSchema{
			Omitted: "confirmation_domain_without_owner_generation_fence",
			Present: "positive_integer_required_for_cloud_worker.execute|execution_v2.run|extension.execute",
		}},
		"operation_domain":    {Type: "string", Required: true},
		"target_id":           {Type: "string", Required: true},
		"target_revision":     {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"target_kind":         {Type: "string"},
		"source_version":      {Type: "string"},
		"source_commit":       {Type: "string"},
		"content_digest":      nonCloudOnly("string"),
		"manifest_digest":     {Type: "string"},
		"execution_digest":    {Type: "string"},
		"permission_digest":   {Type: "string"},
		"parameter_digest":    nonCloudOnly("string"),
		"network_digest":      nonCloudOnly("string"),
		"secret_grant_digest": nonCloudOnly("string"),
		"selected_tool":       {Type: "string"},
		"selected_command":    {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
		"network_grants":      {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
		"secret_grants":       {Type: "array", Items: secretGrant, Presence: &ActionPresenceSchema{Omitted: "cloud_worker.execute", Present: "required_for_non_cloud_worker_confirmation"}},
		"execution_id":        cloudOnlyString("canonical_uuid_required_for_cloud_worker.execute"),
		"plan_id":             cloudOnlyString("canonical_uuid_required_for_cloud_worker.execute"),
		"plan_revision":       cloudOnlyInteger("positive_integer_required_for_cloud_worker.execute"),
		"quote": cloudOnlyObject("required_for_cloud_worker.execute;live_quote_display_and_authority", map[string]ActionFieldSchema{
			"amount_micros":                  cloudWorkerNested("integer", "nonnegative_integer"),
			"compute_micros_per_hour":        cloudWorkerNested("integer", "positive_integer"),
			"currency":                       cloudWorkerNested("string", "exact:USD"),
			"source_time":                    cloudWorkerNested("string", "rfc3339_nano"),
			"expires_at":                     cloudWorkerNested("string", "rfc3339_nano"),
			"maximum_authorized_cost_micros": cloudWorkerNested("integer", "integer_greater_than_or_equal_to_amount"),
		}),
	}
	return map[string]ActionFieldSchema{
		"confirmation_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"owner_id":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "authenticated_owner_id"}},
		"task_id":         {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"state":           awsEnumField("pending|confirmed|consumed|rejected|expired", true),
		"revision":        {Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}},
		"created_at":      {Type: "string", Required: true},
		"updated_at":      {Type: "string", Required: true},
		"expires_at":      {Type: "string", Required: true},
		"terminal_reason": {Type: "string"},
		"terminal_code":   {Type: "string"},
		"terminal_note":   {Type: "string"},
		"binding":         {Type: "object", Required: true, Properties: binding},
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

const (
	coreMCPKind        = "mcp"
	coreMCPSources     = "builtin|official_registry|smithery|glama|github|npm"
	coreMCPTransports  = "stdio_static|streamable_http|stdio_node"
	coreSkillKind      = "skill"
	coreSkillSources   = "builtin|skills_sh|github"
	coreSkillTransport = "skill_static"
)

func coreMCPDiscoverSchema() *ActionSchema {
	return coreExtensionDiscoverSchema(coreMCPKind, coreMCPSources, coreMCPTransports)
}

func coreSkillDiscoverSchema() *ActionSchema {
	return coreExtensionDiscoverSchema(coreSkillKind, coreSkillSources, coreSkillTransport)
}

func coreExtensionDiscoverSchema(kind, sources, transports string) *ActionSchema {
	request := corePageFields()
	request["query"] = ActionFieldSchema{Type: "string"}
	request["source"] = coreExtensionEnumField(sources, false)
	candidate := coreExtensionCandidateSchema(kind, sources, transports)
	return coreActionSchema(request, map[string]ActionFieldSchema{"candidates": {Type: "array", Items: &candidate}, "next_page_token": {Type: "string"}})
}

func coreMCPInspectSchema() *ActionSchema {
	return coreExtensionInspectSchema(coreMCPKind, coreMCPSources, coreMCPTransports)
}

func coreSkillInspectSchema() *ActionSchema {
	return coreExtensionInspectSchema(coreSkillKind, coreSkillSources, coreSkillTransport)
}

func coreExtensionInspectSchema(kind, sources, transports string) *ActionSchema {
	candidate := coreExtensionCandidateSchema(kind, sources, transports)
	inspection := coreExtensionInspectionFields(candidate, coreExtensionExecutionSchema(kind))
	return coreActionSchema(map[string]ActionFieldSchema{"candidate": candidate}, map[string]ActionFieldSchema{"inspection": {Type: "object", Required: true, Properties: inspection}})
}

func coreExtensionCandidateSchema(kind, sources, transports string) ActionFieldSchema {
	pin := map[string]ActionFieldSchema{
		"registry_version": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "git_source", Present: "required_with_registry_sha256;exact_immutable_version", Empty: "rejected"}},
		"registry_sha256":  {Type: "string", Presence: &ActionPresenceSchema{Omitted: "git_source", Present: "required_with_registry_version;lowercase_sha256", Empty: "rejected"}},
		"git_commit":       {Type: "string", Presence: &ActionPresenceSchema{Omitted: "registry_source", Present: "required_with_git_sha256;lowercase_40_hex", Empty: "rejected"}},
		"git_sha256":       {Type: "string", Presence: &ActionPresenceSchema{Omitted: "registry_source", Present: "required_with_git_commit;lowercase_sha256", Empty: "rejected"}},
	}
	return ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
		"id":          {Type: "string", Required: true},
		"kind":        coreExtensionEnumField(kind, true),
		"source":      coreExtensionEnumField(sources, true),
		"name":        {Type: "string", Required: true},
		"description": {Type: "string"},
		"pin":         {Type: "object", Required: true, Properties: pin},
		"transport":   coreExtensionTransportField(kind, transports),
	}}
}

func coreExtensionExecutionSchema(kind string) map[string]ActionFieldSchema {
	if kind == coreSkillKind {
		return map[string]ActionFieldSchema{
			"skill": {Type: "object", Properties: map[string]ActionFieldSchema{
				"relative_path": {Type: "string", Required: true},
				"digest":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256"}},
				"executable":    {Type: "boolean"},
				"argv":          {Type: "array", Items: &ActionFieldSchema{Type: "string"}},
			}},
		}
	}
	return map[string]ActionFieldSchema{
		"stdio": {Type: "object", Properties: map[string]ActionFieldSchema{
			"relative_path": {Type: "string", Required: true},
			"digest":        {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "lowercase_sha256"}},
			"argv":          {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}},
			"runtime":       {Type: "string", Presence: &ActionPresenceSchema{Omitted: "stdio_static", Present: "node_required_for_stdio_node", Empty: "rejected"}},
		}},
		"remote": {Type: "object", Properties: map[string]ActionFieldSchema{
			"url": {Type: "string", Required: true}, "credential_reference_id": {Type: "string"},
		}},
	}
}

func coreExtensionInspectionFields(candidate ActionFieldSchema, execution map[string]ActionFieldSchema) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"candidate": candidate, "content_digest": {Type: "string", Required: true}, "manifest_digest": {Type: "string", Required: true}, "execution_digest": {Type: "string", Required: true}, "network_schema_digest": {Type: "string", Required: true}, "secret_schema_digest": {Type: "string", Required: true},
		"execution":      {Type: "object", Required: true, Properties: execution},
		"network_grants": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"scheme": {Type: "string", Required: true}, "host": {Type: "string", Required: true}, "port": {Type: "integer", Required: true}, "path_prefix": {Type: "string", Required: true}, "digest": {Type: "string", Required: true}}}},
		"secret_grants":  {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"reference_id": {Type: "string", Required: true}, "purpose": {Type: "string", Required: true}, "binding_digest": {Type: "string", Required: true}, "configured": {Type: "boolean", Required: true}}}},
	}
}
func coreMCPGetSchema() *ActionSchema {
	return coreExtensionGetSchema(coreMCPKind, coreMCPSources, coreMCPTransports)
}

func coreSkillGetSchema() *ActionSchema {
	return coreExtensionGetSchema(coreSkillKind, coreSkillSources, coreSkillTransport)
}

func coreExtensionGetSchema(kind, sources, transports string) *ActionSchema {
	return coreActionSchema(coreRequired("installation_id"), map[string]ActionFieldSchema{"installation": coreExtensionInstallationSchema(kind, sources, transports)})
}

func coreMCPListSchema() *ActionSchema {
	return coreExtensionListSchema(coreMCPKind, coreMCPSources, coreMCPTransports)
}

func coreSkillListSchema() *ActionSchema {
	return coreExtensionListSchema(coreSkillKind, coreSkillSources, coreSkillTransport)
}

func coreExtensionListSchema(kind, sources, transports string) *ActionSchema {
	request := corePageFields()
	request["source"] = coreExtensionEnumField(sources, false)
	request["state"] = coreExtensionEnumField("draft|installing|installed|updating|uninstalling|removed|failed", false)
	installation := coreExtensionInstallationSchema(kind, sources, transports)
	return coreActionSchema(request, map[string]ActionFieldSchema{"installations": {Type: "array", Items: &installation}, "next_page_token": {Type: "string"}})
}
func coreMCPInstallSchema() *ActionSchema {
	return coreExtensionInstallSchema(coreMCPKind, coreMCPSources, coreMCPTransports)
}

func coreSkillInstallSchema() *ActionSchema {
	return coreExtensionInstallSchema(coreSkillKind, coreSkillSources, coreSkillTransport)
}

func coreExtensionInstallSchema(kind, sources, transports string) *ActionSchema {
	req := coreRequired("idempotency_key")
	candidate := coreExtensionCandidateSchema(kind, sources, transports)
	req["candidate"] = candidate
	req["inspection"] = ActionFieldSchema{Type: "object", Required: true, Properties: coreExtensionInspectionFields(candidate, coreExtensionExecutionSchema(kind))}
	req["secret_inputs"] = ActionFieldSchema{Type: "array", WriteOnly: true, Items: &ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{"reference_id": {Type: "string", Required: true}, "purpose": {Type: "string", Required: true}, "secret_value": {Type: "string", Required: true, WriteOnly: true}}}}
	return coreActionSchema(req, map[string]ActionFieldSchema{"installation": coreExtensionInstallationSchema(kind, sources, transports), "confirmation_id": {Type: "string"}, "task_id": {Type: "string"}})
}
func coreMCPUpdateSchema() *ActionSchema {
	return coreExtensionUpdateSchema(coreMCPKind, coreMCPSources, coreMCPTransports)
}

func coreSkillUpdateSchema() *ActionSchema {
	return coreExtensionUpdateSchema(coreSkillKind, coreSkillSources, coreSkillTransport)
}

func coreExtensionUpdateSchema(kind, sources, transports string) *ActionSchema {
	s := coreExtensionInstallSchema(kind, sources, transports)
	s.Request["installation_id"] = ActionFieldSchema{Type: "string", Required: true}
	s.Request["expected_revision"] = ActionFieldSchema{Type: "integer", Required: true}
	return s
}
func coreMCPRemoveSchema() *ActionSchema {
	return coreExtensionRemoveSchema(coreMCPKind, coreMCPSources, coreMCPTransports)
}

func coreSkillRemoveSchema() *ActionSchema {
	return coreExtensionRemoveSchema(coreSkillKind, coreSkillSources, coreSkillTransport)
}

func coreExtensionRemoveSchema(kind, sources, transports string) *ActionSchema {
	request := coreMutationFields("installation_id")
	expected := request["expected_revision"]
	expected.Required = true
	request["expected_revision"] = expected
	return coreActionSchema(request, map[string]ActionFieldSchema{"installation": coreExtensionInstallationSchema(kind, sources, transports), "confirmation_id": {Type: "string"}, "task_id": {Type: "string"}})
}

func coreExtensionInstallationSchema(kind, sources, transports string) ActionFieldSchema {
	nodeReceipt := ActionFieldSchema{Type: "object", Presence: &ActionPresenceSchema{Omitted: "non_node_or_unpublished_version", Present: "published_stdio_node_version"}, Properties: map[string]ActionFieldSchema{
		"package_name":               {Type: "string", Required: true},
		"package_version":            {Type: "string", Required: true},
		"artifact_bytes":             {Type: "integer", Required: true},
		"file_count":                 {Type: "integer", Required: true},
		"node_version":               {Type: "string", Required: true},
		"npm_version":                {Type: "string", Required: true},
		"lifecycle_scripts_disabled": {Type: "boolean", Required: true},
		"native_addons_absent":       {Type: "boolean", Required: true},
	}}
	candidate := coreExtensionCandidateSchema(kind, sources, transports)
	inspection := coreExtensionInspectionFields(candidate, coreExtensionExecutionSchema(kind))
	version := ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"version_id":     {Type: "string", Required: true},
		"pin":            candidate.Properties["pin"],
		"content_digest": {Type: "string", Required: true}, "manifest_digest": {Type: "string", Required: true}, "execution_digest": {Type: "string", Required: true},
		"network_schema_digest": {Type: "string", Required: true}, "secret_schema_digest": {Type: "string", Required: true},
		"execution": inspection["execution"], "created_at": {Type: "string", Required: true},
		"network_grants": inspection["network_grants"], "secret_grants": inspection["secret_grants"],
	}}
	if kind == coreMCPKind {
		version.Properties["node_artifact"] = nodeReceipt
	}
	return ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"versions": {Type: "array", Items: &version},
	}}
}

func coreExtensionEnumField(values string, required bool) ActionFieldSchema {
	return ActionFieldSchema{Type: "string", Required: required, Presence: &ActionPresenceSchema{Present: "one_of:" + values, Empty: "rejected"}}
}

func coreExtensionTransportField(kind, transports string) ActionFieldSchema {
	field := coreExtensionEnumField(transports, true)
	if kind == coreMCPKind {
		field.Presence.Present += ";npm_requires_stdio_node;stdio_node_requires_npm_or_github"
	}
	return field
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

func awsEnumField(values string, required bool) ActionFieldSchema {
	p := &ActionPresenceSchema{Present: "one_of:" + values, Empty: "rejected"}
	if !required {
		p.Omitted = "all_values"
	}
	return ActionFieldSchema{Type: "string", Required: required, Presence: p}
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
	request := coreMutationFields("credential_id")
	request["expected_revision"] = ActionFieldSchema{Type: "integer", Required: true}
	return coreActionSchema(request, map[string]ActionFieldSchema{"deleted": {Type: "boolean"}, "credential_id": {Type: "string"}})
}
func coreAWSCredentialListSchema() *ActionSchema {
	return coreActionSchema(corePageFields(), map[string]ActionFieldSchema{"credentials": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: awsCredentialResponseFields()}}, "next_page_token": {Type: "string"}})
}
func coreAWSCredentialTestSchema() *ActionSchema {
	request := coreMutationFields("credential_id")
	request["expected_revision"] = ActionFieldSchema{Type: "integer", Required: true}
	return coreActionSchema(request, map[string]ActionFieldSchema{"credential_id": {Type: "string"}, "account_id": {Type: "string"}, "user_arn": {Type: "string"}, "principal_id": {Type: "string"}, "credential_revision": {Type: "integer"}, "tested_at": {Type: "string"}})
}
