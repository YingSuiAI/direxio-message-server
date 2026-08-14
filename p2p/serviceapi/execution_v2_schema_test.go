package serviceapi

import "testing"

func TestExecutionV2ActionsAreOwnerHTTPAndWSWithStrictMutations(t *testing.T) {
	want := []string{"projects.analyze", "analyses.get", "targets.list", "targets.get", "targets.import", "targets.reserve", "targets.observe", "plans.create", "plans.revise", "plans.get", "plans.list", "deployments.list", "deployments.get", "deployments.events", "runs.create", "runs.get", "runs.list", "runs.cancel", "runs.retry", "runs.events", "artifacts.get", "artifacts.download", "service_bindings.list", "service_bindings.get", "service_bindings.invoke", "secrets.create", "secrets.get", "secrets.list", "secrets.revoke"}
	for _, bare := range want {
		name := executionV2Name(bare)
		s, ok := ActionSpecFor(name)
		if !ok || s.Auth != ActionAuthOwner || s.Transport != ActionTransportHTTPOnly || s.Schema == nil {
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
		"owner_id", "account_generation", "plan_id", "revision", "status", "execution_id", "task_id", "confirmation_id", "conversation_id", "turn_id",
		"objective_summary", "proposal_reason", "persistent_worker_reuse", "workspace_mode", "aws", "compute", "limits", "quote", "created_at", "updated_at",
	}
	runFields := []string{
		"owner_id", "account_generation", "run_id", "execution_id", "plan_id", "plan_revision", "task_id", "confirmation_id", "conversation_id", "turn_id",
		"status", "revision", "worker_id", "persistent_worker", "artifact_ids", "failure_code", "failure_summary", "created_at", "updated_at",
	}
	artifactFields := []string{"owner_id", "account_generation", "artifact_id", "execution_id", "kind", "name", "media_type", "size_bytes", "sha256", "status", "created_at"}
	eventFields := []string{"event_id", "run_id", "owner_id", "account_generation", "revision", "sequence", "type", "at", "payload_digest", "status"}

	planGet, _ := ActionSpecFor(executionV2Name("plans.get"))
	assertCloudConditionalProperties(t, "plans.get.plan", planGet.Schema.Response["plan"].Properties, planFields)
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
	compute := planGet.Schema.Response["plan"].Properties["compute"]
	if status := planGet.Schema.Response["plan"].Properties["status"]; status.Presence == nil || status.Presence.Present != "required_when_record_kind=cloud_worker;exact:waiting_user" {
		t.Fatalf("Cloud Worker plan status schema=%#v", status)
	}
	for _, field := range []string{"instance_type", "vcpu", "memory_gib", "volume_gib", "volume_type", "volume_iops", "volume_throughput_mib"} {
		if !compute.Properties[field].Required {
			t.Errorf("compute.%s is not strict when compute is present", field)
		}
	}
	for _, field := range []string{"amount_micros", "compute_micros_per_hour", "currency", "source_time", "expires_at", "maximum_authorized_cost_micros"} {
		if !quote.Properties[field].Required {
			t.Errorf("quote.%s is not strict when quote is present", field)
		}
	}
	for _, retired := range []string{"digest", "basis_digest"} {
		if _, exposed := quote.Properties[retired]; exposed {
			t.Fatalf("public quote exposes retired field %q", retired)
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
