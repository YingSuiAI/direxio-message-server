package serviceapi

import (
	"reflect"
	"strings"
	"testing"
)

func TestAgentConversationAndKnowledgeSchemasMatchHandlerResponses(t *testing.T) {
	byName := make(map[string]ActionSpec)
	for _, spec := range ActionSpecs() {
		byName[spec.Name] = spec
	}
	for _, test := range []struct {
		action string
		fields map[string]string
	}{
		{"agent.knowledge.config.get", map[string]string{"embedding_profile_id": "string", "embedding_profile_revision": "integer", "embedding_model": "string", "dimension": "integer", "collection": "string", "collection_config_digest": "string", "revision": "integer", "updated_at": "string"}},
		{"agent.knowledge.config.update", map[string]string{"embedding_profile_id": "string", "embedding_profile_revision": "integer", "embedding_model": "string", "dimension": "integer", "collection": "string", "collection_config_digest": "string", "revision": "integer", "updated_at": "string"}},
		{"agent.chat.conversations.create", map[string]string{"conversation": "object", "replayed": "boolean"}},
		{"agent.chat.conversations.list", map[string]string{"conversations": "array", "next_cursor": "string"}},
		{"agent.chat.conversations.get", map[string]string{"conversation": "object", "messages": "array", "next_cursor": "string"}},
		{"agent.chat.conversations.rename", map[string]string{"conversation": "object", "replayed": "boolean"}},
		{"agent.chat.conversations.delete", map[string]string{"conversation": "object", "replayed": "boolean"}},
		{"agent.knowledge.search", map[string]string{"items": "array", "next_cursor": "string", "search_mode": "string", "embedding_profile_id": "string", "embedding_profile_revision": "integer", "embedding_model": "string", "embedding_generation": "string", "collection_config_digest": "string"}},
		{"agent.knowledge.status", map[string]string{"supported": "boolean", "count": "integer", "embedding_indexed": "integer", "embedding_stale": "integer", "ready_count": "integer", "uploading_count": "integer", "indexing_count": "integer", "failed_count": "integer", "cleanup_pending_count": "integer", "checked_at": "string", "embedding_profile_id": "string", "embedding_profile_revision": "integer", "embedding_model": "string", "quota_used_bytes": "integer", "quota_limit_bytes": "integer", "quota_remaining_bytes": "integer", "max_source_bytes": "integer"}},
	} {
		schema := byName[test.action].Schema
		if schema == nil || len(schema.Response) != len(test.fields) {
			t.Fatalf("%s response schema = %#v", test.action, schema)
		}
		for name, want := range test.fields {
			if got := schema.Response[name].Type; got != want {
				t.Fatalf("%s response.%s = %q, want %q", test.action, name, got, want)
			}
		}
	}
}

func TestKnowledgeStatusSchemaRequiresCanonicalAgentFields(t *testing.T) {
	spec, ok := ActionSpecFor("agent.knowledge.status")
	if !ok || spec.Schema == nil {
		t.Fatal("agent.knowledge.status must publish a schema")
	}
	for _, field := range []string{"supported", "count", "embedding_indexed", "embedding_stale", "ready_count", "uploading_count", "indexing_count", "failed_count", "cleanup_pending_count", "checked_at", "quota_used_bytes", "quota_limit_bytes", "quota_remaining_bytes", "max_source_bytes"} {
		if !spec.Schema.Response[field].Required {
			t.Errorf("knowledge status response.%s must be required", field)
		}
	}
}

func TestKnowledgeActionSchemasPinCurrentUploadAndConfigContract(t *testing.T) {
	start, ok := ActionSpecFor("agent.knowledge.upload.start")
	if !ok || start.Schema == nil || !start.Schema.Request["content_sha256"].Required {
		t.Fatal("knowledge upload start must require content_sha256")
	}
	get, ok := ActionSpecFor("agent.knowledge.config.get")
	if !ok || get.Schema == nil || len(get.Schema.Request) != 0 {
		t.Fatalf("knowledge config get request schema = %#v", get)
	}
	update, ok := ActionSpecFor("agent.knowledge.config.update")
	if !ok || update.Schema == nil {
		t.Fatalf("knowledge config update schema = %#v", update)
	}
	for _, field := range []string{"idempotency_key", "expected_revision"} {
		if !update.Schema.Request[field].Required {
			t.Errorf("knowledge config update %s must be required", field)
		}
	}
	for _, field := range []string{"embedding_profile_id", "embedding_profile_revision", "embedding_model", "collection_config_digest", "revision"} {
		if !get.Schema.Response[field].Required || !update.Schema.Response[field].Required {
			t.Errorf("knowledge config %s must be required in both responses", field)
		}
	}
}

func TestKnowledgeSearchSchemaRequiresQueryAndResultEnvelope(t *testing.T) {
	spec, ok := ActionSpecFor("agent.knowledge.search")
	if !ok || spec.Schema == nil {
		t.Fatal("agent.knowledge.search must publish a schema")
	}
	if !spec.Schema.Request["query"].Required {
		t.Fatal("knowledge search query must be required")
	}
	for _, field := range []string{"items", "next_cursor", "search_mode"} {
		if !spec.Schema.Response[field].Required {
			t.Errorf("knowledge search response.%s must be required", field)
		}
	}
	for _, field := range []string{"embedding_profile_id", "embedding_profile_revision", "embedding_model", "embedding_generation", "collection_config_digest"} {
		if _, ok := spec.Schema.Response[field]; !ok {
			t.Errorf("knowledge search provenance field %s was removed", field)
		}
	}
}

func TestActionSpecsReturnsStableOrderedCopy(t *testing.T) {
	first := ActionSpecs()
	second := ActionSpecs()

	if len(first) == 0 {
		t.Fatal("ActionSpecs() returned no actions")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("ActionSpecs() did not preserve action order")
	}

	first[0] = ActionSpec{Name: "mutated"}
	if reflect.DeepEqual(first, ActionSpecs()) {
		t.Fatal("ActionSpecs() returned storage shared with its caller")
	}
	if got := ActionSpecs()[0].Name; got != "portal.bootstrap" {
		t.Fatalf("mutating returned specs changed registry: first action = %q", got)
	}
}

func TestReleaseV2SchemasPublishUnifiedComponentContract(t *testing.T) {
	status, ok := ActionSpecFor("release.v2.status")
	if !ok || status.Schema == nil || !status.Schema.Response["agent"].Required {
		t.Fatalf("release.v2.status schema = %#v", status.Schema)
	}
	agent := status.Schema.Response["agent"]
	for _, field := range []string{"available", "current_version", "latest_version", "minimum_server_version", "update_available", "compatibility", "reasons"} {
		if !agent.Properties[field].Required {
			t.Errorf("release.v2.status agent.%s must be required", field)
		}
	}
	if !status.Schema.Response["active_job"].Properties["component"].Required {
		t.Fatal("release.v2.status active_job.component must be required")
	}
	apply, ok := ActionSpecFor("release.v2.apply")
	if !ok || apply.Schema == nil || !apply.Schema.Request["component"].Required || !apply.Schema.Request["target_version"].Required || !apply.Schema.Response["job_token"].WriteOnly {
		t.Fatalf("release.v2.apply schema = %#v", apply.Schema)
	}
}

func TestActionSpecForFindsEveryRegisteredAction(t *testing.T) {
	for _, want := range ActionSpecs() {
		got, ok := ActionSpecFor(" \t" + want.Name + "\n")
		if !ok {
			t.Errorf("ActionSpecFor(%q) did not find registered action", want.Name)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ActionSpecFor(%q) = %#v, want %#v", want.Name, got, want)
		}
	}
}

func TestActionSpecForRejectsUnknownAndRetiredActions(t *testing.T) {
	for _, action := range []string{
		"", "   ", "portal.missing", "PORTAL.BOOTSTRAP",
		"portal.setup", "agent.status", "apis.list",
		"contacts.export", "contacts.download", "contacts.import",
		"rooms.send", "rooms.send_media", "rooms.messages.delete",
		"rooms.messages.delete_batch", "rooms.messages.delete_range", "rooms.messages.recall",
		"sync.messages", "sync.unread", "search",
	} {
		if got, ok := ActionSpecFor(action); ok {
			t.Errorf("ActionSpecFor(%q) = %#v, true; want zero, false", action, got)
		}
	}
}

func TestModelProfileActionSchemasDescribePresenceSensitiveFields(t *testing.T) {
	for _, action := range []string{"agent.model_profiles.sync", "agent.model_profiles.list", "agent.model_profiles.get", "agent.model_profiles.delete"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Schema == nil {
			t.Fatalf("%s must publish an action schema", action)
		}
	}
	syncSpec, _ := ActionSpecFor("agent.model_profiles.sync")
	if syncSpec.Schema.Request["default_tool_client_profile_id"].Type != "string" || !syncSpec.Schema.Response["default_tool_client_profile_id"].Required {
		t.Fatal("sync schema must expose the optional tool default and required readback")
	}
	entry := syncSpec.Schema.Request["entries"].Items
	if entry == nil || entry.Properties["client_profile_id"].Presence == nil || entry.Properties["api_key"].Presence == nil {
		t.Fatal("sync schema must expose client reference and API-key presence")
	}
	keyPresence := entry.Properties["api_key"].Presence
	if keyPresence.Omitted != "preserve_existing" || keyPresence.Present != "rotate_write_only" || keyPresence.Empty != "rejected" {
		t.Fatalf("api_key presence schema = %#v", keyPresence)
	}
	refPresence := entry.Properties["client_profile_id"].Presence
	if refPresence.Present != "exact_nonempty_bytes" || refPresence.Empty != "rejected" {
		t.Fatalf("client_profile_id presence schema = %#v", refPresence)
	}
}
func TestAgentCoreFamilySchemaDrift(t *testing.T) {
	c, _ := ActionSpecFor("agent.core.schedules.create")
	if c.Schema.Request["task_template"].Type != "object" || c.Schema.Request["trigger"].Type != "object" {
		t.Fatal("schedule create schema")
	}
	u, _ := ActionSpecFor("agent.core.mcp.update")
	if !u.Schema.Request["installation_id"].Required {
		t.Fatal("mcp update installation_id")
	}
	i, _ := ActionSpecFor("agent.core.mcp.install")
	if i.Schema.Request["installation_id"].Required {
		t.Fatal("mcp install installation_id")
	}
	m, _ := ActionSpecFor("agent.core.mcp.execute")
	if !m.Schema.Request["tool_name"].Required {
		t.Fatal("mcp tool_name")
	}
	s, _ := ActionSpecFor("agent.core.skills.execute")
	if s.Schema.Request["tool_name"].Required {
		t.Fatal("skill tool_name")
	}
	inspect, _ := ActionSpecFor("agent.core.mcp.inspect")
	candidate := inspect.Schema.Request["candidate"]
	if candidate.Type != "object" || !candidate.Required || !candidate.Properties["pin"].Required || candidate.Properties["pin"].Properties["git_sha256"].Type != "string" {
		t.Fatal("inspect candidate schema must publish the immutable pin")
	}
	if kind := candidate.Properties["kind"].Presence; kind == nil || kind.Present != "one_of:mcp" {
		t.Fatal("MCP candidate schema must publish only kind=mcp")
	}
	if source := candidate.Properties["source"].Presence; source == nil || source.Present != "one_of:builtin|official_registry|smithery|glama|github|npm" {
		t.Fatal("MCP candidate source schema drifted")
	}
	if transport := candidate.Properties["transport"].Presence; transport == nil || transport.Present != "one_of:stdio_static|streamable_http|stdio_node;npm_requires_stdio_node;stdio_node_requires_npm_or_github" {
		t.Fatal("MCP candidate transport schema drifted")
	}
	pin := candidate.Properties["pin"].Properties
	if pin["registry_version"].Required || pin["registry_sha256"].Required || pin["git_commit"].Required || pin["git_sha256"].Required {
		t.Fatal("immutable source pin must require exactly its registry or git pair, not both pairs")
	}
	installation := i.Schema.Request["inspection"]
	if installation.Type != "object" || !installation.Required || installation.Properties["execution"].Properties["remote"].Properties["url"].Type != "string" || installation.Properties["network_grants"].Items.Properties["port"].Type != "integer" || installation.Properties["secret_grants"].Items.Properties["configured"].Type != "boolean" {
		t.Fatal("extension inspection schema is incomplete")
	}
	stdio := installation.Properties["execution"].Properties["stdio"]
	if stdio.Properties["relative_path"].Type != "string" || stdio.Properties["argv"].Items == nil || stdio.Properties["runtime"].Presence == nil || !strings.Contains(stdio.Properties["runtime"].Presence.Present, "node") {
		t.Fatal("extension inspection schema must publish managed Node stdio execution")
	}
	if _, leaked := installation.Properties["execution"].Properties["skill"]; leaked {
		t.Fatal("MCP inspection schema published a Skill execution branch")
	}
	skillInspect, _ := ActionSpecFor("agent.core.skills.inspect")
	skillCandidate := skillInspect.Schema.Request["candidate"]
	if kind := skillCandidate.Properties["kind"].Presence; kind == nil || kind.Present != "one_of:skill" {
		t.Fatal("Skill candidate schema must publish only kind=skill")
	}
	if source := skillCandidate.Properties["source"].Presence; source == nil || source.Present != "one_of:builtin|skills_sh|github" {
		t.Fatal("Skill candidate source schema drifted")
	}
	if transport := skillCandidate.Properties["transport"].Presence; transport == nil || transport.Present != "one_of:skill_static" {
		t.Fatal("Skill candidate transport schema drifted")
	}
	skillExecution := skillInspect.Schema.Response["inspection"].Properties["execution"].Properties
	if skillExecution["skill"].Properties["relative_path"].Type != "string" {
		t.Fatal("Skill inspection schema is missing its static Skill entry")
	}
	if _, leaked := skillExecution["stdio"]; leaked {
		t.Fatal("Skill inspection schema published an MCP stdio branch")
	}
	if _, leaked := skillExecution["remote"]; leaked {
		t.Fatal("Skill inspection schema published an MCP remote branch")
	}
	if _, leaked := i.Schema.Request["expected_revision"]; leaked {
		t.Fatal("MCP install must not publish an update-only expected_revision")
	}
	get, _ := ActionSpecFor("agent.core.mcp.get")
	nodeReceipt := get.Schema.Response["installation"].Properties["versions"].Items.Properties["node_artifact"]
	if nodeReceipt.Type != "object" || len(nodeReceipt.Properties) != 8 || !nodeReceipt.Properties["package_name"].Required || !nodeReceipt.Properties["lifecycle_scripts_disabled"].Required || !nodeReceipt.Properties["native_addons_absent"].Required {
		t.Fatalf("published Node artifact receipt schema must expose exactly the proto fields: %#v", nodeReceipt)
	}
	if _, legacy := nodeReceipt.Properties["lifecycle_scripts_absent"]; legacy {
		t.Fatal("published Node artifact receipt schema must not preserve the superseded lifecycle_scripts_absent field")
	}
	for _, internal := range []string{"input_digest", "artifact_digest", "entry_path", "entry_sha256", "lock_sha256"} {
		if _, leaked := nodeReceipt.Properties[internal]; leaked {
			t.Fatalf("internal Node receipt field %q escaped public schema", internal)
		}
	}
	skillGet, _ := ActionSpecFor("agent.core.skills.get")
	if _, leaked := skillGet.Schema.Response["installation"].Properties["versions"].Items.Properties["node_artifact"]; leaked {
		t.Fatal("Skill installation schema published a managed Node receipt")
	}
	mcpDiscover, _ := ActionSpecFor("agent.core.mcp.discover")
	skillDiscover, _ := ActionSpecFor("agent.core.skills.discover")
	if mcpDiscover.Schema.Request["source"].Presence.Present != "one_of:builtin|official_registry|smithery|glama|github|npm" ||
		skillDiscover.Schema.Request["source"].Presence.Present != "one_of:builtin|skills_sh|github" {
		t.Fatal("extension discovery source schemas are not action-family specific")
	}
	secret := i.Schema.Request["secret_inputs"].Items.Properties["secret_value"]
	if !secret.Required || !secret.WriteOnly {
		t.Fatal("extension secret value must be write-only")
	}
	deleteCredential, ok := ActionSpecFor("agent.core.aws.credentials.delete")
	if !ok || deleteCredential.Schema == nil {
		t.Fatal("AWS credential delete schema must be published")
	}
	if !deleteCredential.Schema.Request["idempotency_key"].Required ||
		!deleteCredential.Schema.Request["credential_id"].Required ||
		!deleteCredential.Schema.Request["expected_revision"].Required {
		t.Fatal("AWS credential delete must require idempotency, identity, and exact revision")
	}
}

func TestBuildActionSpecIndexRejectsDuplicateNames(t *testing.T) {
	specs := []ActionSpec{
		{Name: "duplicate", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
		{Name: "duplicate", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	}

	index, err := buildActionSpecIndex(specs)
	if err == nil {
		t.Fatal("buildActionSpecIndex() accepted duplicate action names")
	}
	if index != nil {
		t.Fatalf("buildActionSpecIndex() returned partial index %#v on error", index)
	}
}

func TestNativeAgentReferenceSchemaMatchesStrictProducerShape(t *testing.T) {
	wantFields := map[string]bool{
		"kind": true, "account_generation": true, "task_id": true, "plan_id": true,
		"plan_revision": true, "plan_digest": true, "run_id": true, "run_revision": true,
		"run_digest": true, "deployment_id": true, "execution_id": true, "confirmation_id": true,
		"worker_id":             true,
		"confirmation_revision": true, "stage_id": true, "stage_revision": true,
		"stage_digest": true, "target_id": true, "target_revision": true,
		"target_digest": true, "preview_digest": true, "binding_digest": true,
		"quote_digest": true, "execution_digest": true, "risk_level": true,
		"gate_type": true, "binding_id": true, "binding_revision": true,
		"project_id": true, "status": true, "state": true, "room_id": true,
		"room_type": true, "title": true, "preview": true, "channel_id": true, "post_id": true,
		"record_kind": true, "artifact_id": true, "name": true, "media_type": true,
		"size_bytes": true, "sha256": true,
	}
	for _, action := range []string{"agent.chat"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Schema == nil {
			t.Fatalf("%s must publish a schema", action)
		}
		field := spec.Schema.Response["references"]
		if field.Type != "array" || field.Items == nil || field.Items.Type != "object" {
			t.Fatalf("%s references schema = %#v", action, field)
		}
		got := field.Items.Properties
		if len(got) != len(wantFields) {
			t.Fatalf("%s reference fields = %#v", action, got)
		}
		for name := range got {
			if !wantFields[name] {
				t.Errorf("%s publishes unsupported reference field %q", action, name)
			}
		}
		kind := field.Items.Properties["kind"]
		if !kind.Required || kind.Presence == nil || kind.Presence.Present != "one_of:room|channel_post|execution_plan|execution_run|execution_confirmation|execution_artifact|service_binding" {
			t.Errorf("%s reference kind schema = %#v", action, kind)
		}
		taskID := field.Items.Properties["task_id"]
		if taskID.Required || taskID.Presence == nil || taskID.Presence.Omitted != "generic_execution_v2_reference" {
			t.Errorf("%s task_id discriminator schema = %#v", action, taskID)
		}
		roomID := field.Items.Properties["room_id"]
		if roomID.Required || roomID.Presence == nil || roomID.Presence.Present != "required_for_room_or_channel_post" {
			t.Errorf("%s reference room_id must be conditionally required: %#v", action, roomID)
		}
		if field.Presence == nil || field.Presence.Omitted != "No room, channel-post, Execution V2, or execution-artifact references were produced." {
			t.Errorf("%s reference presence = %#v", action, field.Presence)
		}
		for _, name := range []string{"related_task_ids", "related_plan_ids"} {
			related := spec.Schema.Response[name]
			if related.Type != "array" || related.Items == nil || related.Items.Type != "string" || related.Items.Presence == nil || related.Items.Presence.Present != "canonical_uuid" {
				t.Errorf("%s %s schema = %#v", action, name, related)
			}
		}
	}
	history, _ := ActionSpecFor("agent.chat.conversations.get")
	messages := history.Schema.Response["messages"]
	if messages.Items == nil || messages.Items.Properties["references"].Items == nil {
		t.Fatalf("conversation history reference schema = %#v", messages)
	}
	if got := messages.Items.Properties["references"].Items.Properties["record_kind"]; got.Presence == nil || got.Presence.Present != "exact:local_sandbox;required_for_execution_artifact" {
		t.Fatalf("conversation history artifact record kind = %#v", got)
	}
}

func TestNativeAgentAttachmentsUseClosedUploadSchemas(t *testing.T) {
	unary, ok := ActionSpecFor("agent.chat")
	if !ok || unary.Schema == nil {
		t.Fatal("agent.chat must publish a schema")
	}
	if _, present := unary.Schema.Request["accepted_attachment_ids"]; present {
		t.Fatal("unary agent.chat must not advertise committed attachment IDs")
	}
	for _, action := range []string{
		"agent.chat.attachment.begin",
		"agent.chat.attachment.append",
		"agent.chat.attachment.commit",
	} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Schema == nil {
			t.Fatalf("%s must publish a schema", action)
		}
		for field, schema := range spec.Schema.Request {
			if !schema.Required {
				t.Errorf("%s request field %s must be required", action, field)
			}
		}
	}
	appendSpec, _ := ActionSpecFor("agent.chat.attachment.append")
	if !appendSpec.Schema.Request["data_base64"].WriteOnly {
		t.Fatal("attachment chunk bytes must be write-only")
	}
	begin, _ := ActionSpecFor("agent.chat.attachment.begin")
	if kind := begin.Schema.Request["kind"]; !kind.Required || kind.Presence == nil || kind.Presence.Present != "one_of:image|file|workspace_archive" {
		t.Fatalf("attachment kind schema = %#v", kind)
	}
	if size := begin.Schema.Request["declared_size"]; size.Presence == nil || size.Presence.Present != "integer_1_to_8388608" {
		t.Fatalf("attachment declared size schema = %#v", size)
	}
	commit, _ := ActionSpecFor("agent.chat.attachment.commit")
	if attachment := commit.Schema.Response["attachment"]; !attachment.Required || attachment.Type != "object" ||
		!attachment.Properties["source_id"].Required || attachment.Properties["kind"].Presence == nil ||
		attachment.Properties["kind"].Presence.Present != "one_of:image|file|workspace_archive" ||
		attachment.Properties["size_bytes"].Presence == nil || attachment.Properties["size_bytes"].Presence.Present != "integer_1_to_8388608" {
		t.Fatalf("commit attachment envelope schema = %#v", attachment)
	}
}

func TestNativeAgentChatPublishesOneClosedStartShape(t *testing.T) {
	for _, action := range []string{"agent.chat"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Schema == nil {
			t.Fatalf("%s must publish a schema", action)
		}
		request := spec.Schema.Request
		for _, field := range []string{"idempotency_key", "message", "model_profile_id", "model_profile_revision", "credential_version"} {
			if !request[field].Required {
				t.Errorf("%s request field %s must be required", action, field)
			}
		}
		for _, field := range []string{"prompt", "turn_id", "client_message_id", "request_id", "operation_id"} {
			if _, present := request[field]; present {
				t.Errorf("%s publishes unsupported start field %s", action, field)
			}
		}
	}
	unary, _ := ActionSpecFor("agent.chat")
	if unary.Schema.Request["conversation_id"].Required {
		t.Fatal("unary agent.chat must allow Agent to create a conversation")
	}
	if _, present := unary.Schema.Request["after_seq"]; present {
		t.Fatal("unary agent.chat must not advertise a stream cursor")
	}
}

func TestNativeAgentTurnControlPublishesDistinctStartAndInternalIdentities(t *testing.T) {
	stop, ok := ActionSpecFor("agent.chat.turn.stop")
	if !ok || stop.Schema == nil {
		t.Fatal("agent.chat.turn.stop must publish a schema")
	}
	if len(stop.Schema.Request) != 3 {
		t.Fatalf("turn stop request is not closed: %#v", stop.Schema.Request)
	}
	for _, field := range []string{"idempotency_key", "turn_id", "expected_revision"} {
		if !stop.Schema.Request[field].Required {
			t.Errorf("turn stop request field %s must be required", field)
		}
	}
	steer, ok := ActionSpecFor("agent.chat.turn.steer")
	if !ok || steer.Schema == nil || len(steer.Schema.Request) != 4 || len(steer.Schema.Response) != 11 {
		t.Fatalf("agent.chat.turn.steer schema = %#v", steer.Schema)
	}
	for _, field := range []string{"idempotency_key", "turn_id", "expected_revision", "instruction"} {
		if !steer.Schema.Request[field].Required {
			t.Errorf("turn steer request field %s must be required", field)
		}
	}
	if !steer.Schema.Response["steer_idempotency_key"].Required {
		t.Fatal("turn steer response must publish its mutation receipt")
	}
	for _, action := range []string{"agent.chat.turn.stop", "agent.chat.turns.list"} {
		spec, _ := ActionSpecFor(action)
		fields := spec.Schema.Response
		if action == "agent.chat.turns.list" {
			fields = spec.Schema.Response["turns"].Items.Properties
		}
		if len(fields) != 10 || !fields["turn_id"].Required || !fields["idempotency_key"].Required {
			t.Errorf("%s turn metadata schema = %#v", action, fields)
		}
	}
}

func TestNativeAgentWebSearchSchemasUseServerStoredCredential(t *testing.T) {
	getSpec, ok := ActionSpecFor("agent.web_search.config.get")
	if !ok || getSpec.Schema == nil {
		t.Fatal("agent.web_search.config.get must publish a schema")
	}
	updateSpec, ok := ActionSpecFor("agent.web_search.config.update")
	if !ok || updateSpec.Schema == nil {
		t.Fatal("agent.web_search.config.update must publish a schema")
	}
	if !updateSpec.Schema.Request["idempotency_key"].Required || !updateSpec.Schema.Request["expected_revision"].Required {
		t.Fatalf("web search update concurrency fields = %#v", updateSpec.Schema.Request)
	}
	if !updateSpec.Schema.Request["api_key"].WriteOnly {
		t.Fatal("web search update API key must be write-only")
	}
	for _, spec := range []ActionSpec{getSpec, updateSpec} {
		for _, field := range []string{"enabled", "provider", "api_key_configured", "revision"} {
			if !spec.Schema.Response[field].Required {
				t.Errorf("%s response field %s must be required", spec.Name, field)
			}
		}
		if _, exposed := spec.Schema.Response["api_key"]; exposed {
			t.Errorf("%s response must not publish an API key", spec.Name)
		}
	}
	testSpec, ok := ActionSpecFor("agent.web_search.test")
	if !ok || testSpec.Schema == nil || len(testSpec.Schema.Request) != 0 {
		t.Fatalf("agent.web_search.test must use the stored credential without request secrets: %#v", testSpec)
	}
	for _, action := range []string{"agent.chat"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Schema == nil {
			t.Fatalf("%s must publish a schema", action)
		}
		if _, legacy := spec.Schema.Request["tool_credentials"]; legacy {
			t.Fatalf("%s must not publish request-scoped web-search credentials", action)
		}
	}
}

func TestNativeAgentModelCatalogSchemaIsSeparateFromProfiles(t *testing.T) {
	spec, ok := ActionSpecFor("agent.models.list")
	if !ok || spec.Schema == nil {
		t.Fatal("agent.models.list must publish a provider catalog schema")
	}
	if !spec.Schema.Request["api_key"].WriteOnly {
		t.Fatal("provider catalog API key must remain write-only")
	}
	modelKind := spec.Schema.Request["model_kind"]
	if modelKind.Presence == nil || modelKind.Presence.Omitted != "conversation" {
		t.Fatalf("model_kind omission contract = %#v", modelKind.Presence)
	}
	if !spec.Schema.Response["models"].Required || !spec.Schema.Response["providers"].Required {
		t.Fatalf("provider catalog response = %#v", spec.Schema.Response)
	}
	if !spec.Schema.Response["models"].Items.Properties["id"].Required || !spec.Schema.Response["models"].Items.Properties["provider"].Required {
		t.Fatal("provider catalog model id and provider must be required")
	}
	if _, profiles := spec.Schema.Response["profiles"]; profiles {
		t.Fatal("provider catalog must not publish model profiles")
	}
}
