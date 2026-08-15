package serviceapi

import "testing"

func TestExecutionV2ActionsAreOwnerHTTPAndWSWithStrictMutations(t *testing.T) {
	want := []string{"projects.analyze", "analyses.get", "targets.list", "targets.get", "targets.import", "targets.reserve", "targets.observe", "plans.create", "plans.revise", "plans.get", "plans.list", "deployments.list", "deployments.get", "deployments.events", "runs.create", "runs.get", "runs.list", "runs.cancel", "runs.retry", "runs.events", "artifacts.get", "artifacts.download", "deliverables.list", "service_bindings.list", "service_bindings.get", "service_bindings.invoke", "secrets.create", "secrets.get", "secrets.list", "secrets.revoke"}
	for _, bare := range want {
		name := executionV2Name(bare)
		s, ok := ActionSpecFor(name)
		if !ok || s.Auth != ActionAuthOwner || s.Transport != ActionTransportHTTPAndWS || s.Schema == nil {
			t.Fatalf("%s spec = %#v", name, s)
		}
	}
	if _, ok := ActionSpecFor("projects.analyze"); ok {
		t.Fatal("bare execution action alias registered")
	}
	for _, bare := range []string{"plans.revise", "runs.cancel", "runs.retry", "service_bindings.invoke", "secrets.revoke"} {
		name := executionV2Name(bare)
		s, _ := ActionSpecFor(name)
		if !s.Schema.Request["idempotency_key"].Required || !s.Schema.Request["expected_revision"].Required {
			t.Fatalf("%s must require idempotency and expected_revision", name)
		}
	}
	for _, bare := range []string{"runs.reconcile", "confirmations.get", "confirmations.list", "confirmations.confirm", "confirmations.reject"} {
		if _, ok := ActionSpecFor(executionV2Name(bare)); ok {
			t.Fatalf("superseded public action %s remains registered", bare)
		}
	}
	for _, bare := range []string{"plans.get", "plans.list", "runs.get", "runs.list", "runs.cancel", "runs.events", "artifacts.get"} {
		spec, _ := ActionSpecFor(executionV2Name(bare))
		filter := spec.Schema.Request["record_kind"]
		if filter.Required || filter.Type != "string" || filter.Presence == nil || filter.Presence.Omitted != "generic_execution_v2_authority" || filter.Presence.Present != "exact:cloud_worker" {
			t.Fatalf("%s record_kind filter=%#v", bare, filter)
		}
	}
	for _, bare := range []string{"runs.create", "runs.retry"} {
		spec, _ := ActionSpecFor(executionV2Name(bare))
		if _, ok := spec.Schema.Request["record_kind"]; ok {
			t.Fatalf("%s must remain generic-only; cloud workers are conversation-created", bare)
		}
	}
	download, _ := ActionSpecFor(executionV2Name("artifacts.download"))
	if len(download.Schema.Request) != 4 || len(download.Schema.Response) != 11 {
		t.Fatalf("artifact download schema is not closed: %#v", download.Schema)
	}
	for _, field := range []string{"record_kind", "artifact_id", "offset_bytes", "max_chunk_bytes"} {
		if !download.Schema.Request[field].Required {
			t.Errorf("artifact download request.%s must be required", field)
		}
	}
	if rule := download.Schema.Request["record_kind"].Presence; rule == nil || rule.Present != "exact:cloud_worker" {
		t.Fatalf("artifact download record kind = %#v", download.Schema.Request["record_kind"])
	}
	if rule := download.Schema.Request["max_chunk_bytes"].Presence; rule == nil || rule.Present != "integer_1_to_524288" {
		t.Fatalf("artifact download chunk bound = %#v", download.Schema.Request["max_chunk_bytes"])
	}
	if rule := download.Schema.Request["offset_bytes"].Presence; rule == nil || rule.Present != "integer_0_to_8388607" {
		t.Fatalf("artifact download offset bound = %#v", download.Schema.Request["offset_bytes"])
	}
	for _, field := range []string{"owner_id", "account_generation", "artifact_id", "execution_id", "offset_bytes", "data_base64", "chunk_sha256", "artifact_sha256", "size_bytes", "next_offset_bytes", "eof"} {
		value, ok := download.Schema.Response[field]
		if !ok || !value.Required || value.Presence == nil || value.Presence.Present == "" {
			t.Errorf("artifact download response.%s = %#v", field, value)
		}
	}
	deliverables, _ := ActionSpecFor(executionV2Name("deliverables.list"))
	if len(deliverables.Schema.Request) != 3 || len(deliverables.Schema.Response) != 2 {
		t.Fatalf("deliverables list schema is not closed: %#v", deliverables.Schema)
	}
	if kind := deliverables.Schema.Request["record_kind"]; !kind.Required || kind.Presence == nil || kind.Presence.Present != "exact:cloud_worker" {
		t.Fatalf("deliverables record kind = %#v", kind)
	}
	if size := deliverables.Schema.Request["page_size"]; size.Required || size.Presence == nil || size.Presence.Present != "integer_1_to_20" {
		t.Fatalf("deliverables page size = %#v", size)
	}
	item := deliverables.Schema.Response["deliverables"].Items
	if item == nil || len(item.Properties) != 22 {
		t.Fatalf("deliverables item schema = %#v", item)
	}
	for _, field := range []string{
		"owner_id", "account_generation", "artifact_id", "execution_id", "run_id", "plan_id", "task_id", "confirmation_id", "conversation_id", "turn_id",
		"objective_summary", "run_status", "name", "kind", "media_type", "size_bytes", "sha256", "artifact_status", "created_at", "completed_at", "retention_expires_at", "download",
	} {
		if value, ok := item.Properties[field]; !ok || !value.Required {
			t.Errorf("deliverables item.%s = %#v", field, value)
		}
	}
	downloadDescriptor := item.Properties["download"]
	if len(downloadDescriptor.Properties) != 5 || downloadDescriptor.Properties["action"].Presence.Present != "exact:agent.execution.v2.artifacts.download" || downloadDescriptor.Properties["max_chunk_bytes"].Presence.Present != "exact:524288" {
		t.Fatalf("deliverables download descriptor = %#v", downloadDescriptor)
	}
	runGet, _ := ActionSpecFor(executionV2Name("runs.get"))
	if stages := runGet.Schema.Response["stages"]; stages.Required || stages.Presence == nil || stages.Presence.Omitted != "record_kind=cloud_worker" {
		t.Fatalf("runs.get conditional stages=%#v", stages)
	}
	secretCreate, _ := ActionSpecFor(executionV2Name("secrets.create"))
	if !secretCreate.Schema.Request["value"].WriteOnly {
		t.Fatal("secrets.create value must be write-only")
	}
	if _, exposed := secretCreate.Schema.Response["secret"].Properties["value"]; exposed {
		t.Fatal("secret value must not appear in response schema")
	}
	importSpec, _ := ActionSpecFor(executionV2Name("targets.import"))
	if !importSpec.Schema.Request["idempotency_key"].Required ||
		!importSpec.Schema.Request["credential_id"].Required ||
		!importSpec.Schema.Request["credential_revision"].Required ||
		!importSpec.Schema.Request["instance_id"].Required {
		t.Fatal("targets.import must require an exact credential revision, instance and idempotency key")
	}
	for _, forbidden := range []string{"target_id", "target_revision", "target_digest", "account_id", "region", "capabilities", "observation_id", "observation_digest"} {
		if _, ok := importSpec.Schema.Request[forbidden]; ok {
			t.Fatalf("targets.import accepted caller authority field %q", forbidden)
		}
	}
	reserveSpec, _ := ActionSpecFor(executionV2Name("targets.reserve"))
	for _, required := range []string{"credential_id", "credential_revision", "instance_type", "volume_gib", "idempotency_key"} {
		if !reserveSpec.Schema.Request[required].Required {
			t.Fatalf("targets.reserve missing required %q", required)
		}
	}
	for _, forbidden := range []string{"target_id", "target_revision", "target_digest", "account_id", "region", "profile", "network", "capabilities", "cost_quote"} {
		if _, ok := reserveSpec.Schema.Request[forbidden]; ok {
			t.Fatalf("targets.reserve accepted caller authority field %q", forbidden)
		}
	}
	for _, name := range []string{"targets.get", "targets.reserve"} {
		spec, _ := ActionSpecFor(executionV2Name(name))
		if !spec.Schema.Response["target"].Properties["owner_id"].Required {
			t.Fatalf("%s target response must project authenticated owner_id", name)
		}
	}
	if !importSpec.Schema.Response["target"].Properties["owner_id"].Required || !importSpec.Schema.Response["observation"].Properties["owner_id"].Required {
		t.Fatal("targets.import response must project authenticated owner_id")
	}
	listSpec, _ := ActionSpecFor(executionV2Name("targets.list"))
	if listSpec.Schema.Response["targets"].Items == nil || !listSpec.Schema.Response["targets"].Items.Properties["owner_id"].Required {
		t.Fatal("targets.list items must project authenticated owner_id")
	}
	observeSpec, _ := ActionSpecFor(executionV2Name("targets.observe"))
	if !observeSpec.Schema.Response["observation"].Properties["owner_id"].Required {
		t.Fatal("targets.observe response must project authenticated owner_id")
	}
}

func TestExecutionV2CloudWorkerConditionalResponseSchemasPinStrictPublicProjection(t *testing.T) {
	planFields := []string{
		"owner_id", "account_generation", "plan_id", "revision", "status", "digest", "execution_id", "task_id", "confirmation_id", "conversation_id", "turn_id",
		"recipe_id", "adapter", "objective_summary", "proposal_reason", "input_manifest_digest", "input_manifest_item_count", "workspace_mode", "model_authorization", "aws", "compute", "limits",
		"network_grants", "secret_grants", "artifact_retention_seconds", "quote", "execution_digest", "created_at", "updated_at",
	}
	runFields := []string{
		"owner_id", "account_generation", "run_id", "execution_id", "plan_id", "plan_revision", "plan_digest", "task_id", "confirmation_id", "conversation_id", "turn_id",
		"status", "revision", "digest", "workspace_mode", "quote_digest", "execution_digest", "cancellation_requested", "cleanup", "artifact_ids", "failure_code", "failure_summary", "created_at", "updated_at",
	}
	artifactFields := []string{"owner_id", "account_generation", "artifact_id", "execution_id", "kind", "name", "media_type", "size_bytes", "sha256", "status", "created_at"}
	eventFields := []string{"event_id", "run_id", "owner_id", "account_generation", "revision", "sequence", "type", "at", "payload_digest", "status", "progress"}

	planGet, _ := ActionSpecFor(executionV2Name("plans.get"))
	assertCloudConditionalProperties(t, "plans.get.plan", planGet.Schema.Response["plan"].Properties, planFields)
	limits := planGet.Schema.Response["plan"].Properties["limits"].Properties
	if legacy := limits["max_tokens"]; legacy.Required || legacy.Presence == nil ||
		legacy.Presence.Omitted != "current_plan_has_no_cumulative_model_token_budget" ||
		legacy.Presence.Present != "positive_integer_for_legacy_plan_only" {
		t.Fatalf("plans.get limits.max_tokens = %#v", legacy)
	}
	if got := planGet.Schema.Response["plan"].Properties["proposal_reason"].Presence.Present; got != "required_when_record_kind=cloud_worker;one_of:explicit_user_cloud|central_delegation|local_budget_exceeded" {
		t.Fatalf("plans.get proposal_reason presence=%q", got)
	}
	planList, _ := ActionSpecFor(executionV2Name("plans.list"))
	assertCloudConditionalProperties(t, "plans.list.plans[]", planList.Schema.Response["plans"].Items.Properties, planFields)

	for _, action := range []string{"runs.get", "runs.list", "runs.cancel"} {
		spec, _ := ActionSpecFor(executionV2Name(action))
		var properties map[string]ActionFieldSchema
		if action == "runs.list" {
			properties = spec.Schema.Response["runs"].Items.Properties
		} else {
			properties = spec.Schema.Response["run"].Properties
		}
		assertCloudConditionalProperties(t, action+".run", properties, runFields)
	}
	artifactGet, _ := ActionSpecFor(executionV2Name("artifacts.get"))
	assertCloudConditionalProperties(t, "artifacts.get.artifact", artifactGet.Schema.Response["artifact"].Properties, artifactFields)
	runEvents, _ := ActionSpecFor(executionV2Name("runs.events"))
	assertCloudConditionalProperties(t, "runs.events.events[]", runEvents.Schema.Response["events"].Items.Properties, eventFields)
	if truncated := runEvents.Schema.Response["history_truncated"]; truncated.Type != "boolean" || !truncated.Required || truncated.Presence == nil || truncated.Presence.Present != "true_when_after_sequence_precedes_retained_history" {
		t.Fatalf("runs.events history_truncated schema=%#v", truncated)
	}
	progress := runEvents.Schema.Response["events"].Items.Properties["progress"]
	if progress.Type != "object" || progress.Presence == nil || progress.Presence.Present != "required_when_type_is_worker_progress;forbidden_otherwise;strict_secret_free_snapshot" {
		t.Fatalf("runs.events progress schema=%#v", progress)
	}
	progressFields := map[string]string{
		"phase":      "one_of:claimed|preparing_inputs|running_pi|uploading_result|completing",
		"elapsed_ms": "integer_0_to_86400000", "last_activity_at": "rfc3339_nano_not_after_event_at",
		"cpu_time_ms": "integer_0_to_604800000", "memory_high_water_bytes": "integer_0_to_68719476736",
		"invocation_count": "integer_0_to_1000000", "uploaded_bytes": "integer_0_to_9437184",
		"output_truncated": "authoritative_runtime_output_truncation_flag",
	}
	if len(progress.Properties) != len(progressFields) {
		t.Fatalf("runs.events progress fields=%#v", progress.Properties)
	}
	for field, rule := range progressFields {
		value, ok := progress.Properties[field]
		if !ok || !value.Required || value.Presence == nil || value.Presence.Present != rule {
			t.Errorf("runs.events progress.%s=%#v", field, value)
		}
	}
	for _, forbidden := range []string{"text", "raw", "model_text", "secret", "stderr", "env", "s3_url", "bucket", "key"} {
		if _, exposed := progress.Properties[forbidden]; exposed {
			t.Errorf("runs.events progress exposes private field %q", forbidden)
		}
	}
	sequenceRule := runEvents.Schema.Response["events"].Items.Properties["sequence"].Presence
	if sequenceRule == nil || sequenceRule.Present != "required_when_record_kind=cloud_worker;positive_contiguous_after_previous;first_equals_after_sequence_plus_one_unless_history_truncated" {
		t.Fatalf("runs.events sequence schema=%#v", sequenceRule)
	}

	for _, field := range []ActionFieldSchema{
		planGet.Schema.Response["plan"].Properties["account_generation"],
		planList.Schema.Response["plans"].Items.Properties["account_generation"],
		runEvents.Schema.Response["events"].Items.Properties["account_generation"],
	} {
		if field.Required || field.Presence == nil || field.Presence.Present == "" || field.Presence.Omitted == "" {
			t.Fatalf("conditional account_generation field = %#v", field)
		}
	}
	quote := planGet.Schema.Response["plan"].Properties["quote"]
	if status := planGet.Schema.Response["plan"].Properties["status"]; status.Presence == nil || status.Presence.Present != "required_when_record_kind=cloud_worker;exact:waiting_user" {
		t.Fatalf("Cloud Worker plan status schema=%#v", status)
	}
	for _, field := range []string{"amount_micros", "currency", "source_time", "expires_at", "maximum_authorized_cost_micros", "digest"} {
		if !quote.Properties[field].Required {
			t.Errorf("quote.%s is not strict when quote is present", field)
		}
	}
	if _, exposed := quote.Properties["basis_digest"]; exposed {
		t.Fatal("public quote must not expose the private authorization basis digest")
	}
	secretGrant := planGet.Schema.Response["plan"].Properties["secret_grants"].Items
	if secretGrant == nil || !secretGrant.Properties["purpose"].Required {
		t.Fatalf("secret grant purpose-only schema = %#v", secretGrant)
	}
	for _, forbidden := range []string{"configured", "count", "reference_id", "binding_digest", "secret_ref", "credential_id"} {
		if _, exposed := secretGrant.Properties[forbidden]; exposed {
			t.Errorf("secret grant exposes private locator %q", forbidden)
		}
	}
	for _, action := range []string{"runs.get", "runs.list", "runs.cancel"} {
		spec, _ := ActionSpecFor(executionV2Name(action))
		var properties map[string]ActionFieldSchema
		if action == "runs.list" {
			properties = spec.Schema.Response["runs"].Items.Properties
		} else {
			properties = spec.Schema.Response["run"].Properties
		}
		if field := properties["cancellation_requested"]; field.Type != "boolean" || field.Presence == nil {
			t.Errorf("%s cancellation_requested schema = %#v", action, field)
		}
	}
	artifact := artifactGet.Schema.Response["artifact"].Properties
	for _, field := range []string{"owner_id", "account_generation"} {
		if artifact[field].Presence == nil || artifact[field].Presence.Present == "" {
			t.Errorf("artifact %s authority schema = %#v", field, artifact[field])
		}
	}
}

func assertCloudConditionalProperties(t *testing.T, name string, properties map[string]ActionFieldSchema, fields []string) {
	t.Helper()
	if len(properties) != len(fields) {
		t.Fatalf("%s fields=%d want=%d: %#v", name, len(properties), len(fields), properties)
	}
	for _, field := range fields {
		value, ok := properties[field]
		if !ok {
			t.Errorf("%s is missing %s", name, field)
			continue
		}
		if field != "status" || name != "runs.events.events[]" {
			if value.Presence == nil {
				t.Errorf("%s.%s lacks conditional presence", name, field)
			}
		}
	}
}
