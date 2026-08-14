package serviceapi

import "testing"

func TestExecutionV2PublishesOnlyRetainedActions(t *testing.T) {
	for _, bare := range []string{"plans.get", "plans.list", "runs.get", "runs.list", "runs.cancel", "runs.events", "artifacts.get", "artifacts.download", "artifacts.delete"} {
		name := executionV2Name(bare)
		s, ok := ActionSpecFor(name)
		if !ok || s.Auth != ActionAuthOwner || s.Transport != ActionTransportHTTPOnly || s.Schema == nil {
			t.Fatalf("%s spec = %#v", name, s)
		}
		recordKind := s.Schema.Request["record_kind"]
		wantRecordKind := "exact:cloud_worker"
		if bare == "artifacts.get" || bare == "artifacts.download" || bare == "artifacts.delete" {
			wantRecordKind = "one_of:cloud_worker|local_sandbox"
		}
		if !recordKind.Required || recordKind.Type != "string" || recordKind.Presence == nil || recordKind.Presence.Present != wantRecordKind {
			t.Fatalf("%s record_kind = %#v", name, recordKind)
		}
	}
	for _, bare := range []string{
		"projects.analyze", "analyses.get",
		"targets.list", "targets.get", "targets.import", "targets.reserve", "targets.observe",
		"plans.create", "plans.revise",
		"deployments.list", "deployments.get", "deployments.events",
		"runs.create", "runs.retry",
		"service_bindings.list", "service_bindings.get", "service_bindings.invoke",
		"secrets.create", "secrets.get", "secrets.list", "secrets.revoke",
	} {
		if _, ok := ActionSpecFor(executionV2Name(bare)); ok {
			t.Errorf("retired public action %s remains registered", bare)
		}
	}
	if _, ok := ActionSpecFor("plans.get"); ok {
		t.Fatal("bare execution action alias registered")
	}
	cancel, _ := ActionSpecFor(executionV2Name("runs.cancel"))
	if !cancel.Schema.Request["idempotency_key"].Required || !cancel.Schema.Request["expected_revision"].Required {
		t.Fatal("runs.cancel must require idempotency and expected_revision")
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
	if rule := download.Schema.Request["record_kind"].Presence; rule == nil || rule.Present != "one_of:cloud_worker|local_sandbox" {
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
	deleteSpec, _ := ActionSpecFor(executionV2Name("artifacts.delete"))
	if len(deleteSpec.Schema.Request) != 3 || !deleteSpec.Schema.Request["idempotency_key"].Required ||
		len(deleteSpec.Schema.Response) != 2 || !deleteSpec.Schema.Response["artifact"].Required || !deleteSpec.Schema.Response["deleted"].Required {
		t.Fatalf("artifact delete schema is not closed: %#v", deleteSpec.Schema)
	}
	if rule := deleteSpec.Schema.Response["deleted"].Presence; rule == nil || rule.Present != "exact:true" {
		t.Fatalf("artifact delete result = %#v", deleteSpec.Schema.Response["deleted"])
	}
}

func TestExecutionV2CloudWorkerResponseSchemasPinStrictPublicProjection(t *testing.T) {
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
	artifactDelete, _ := ActionSpecFor(executionV2Name("artifacts.delete"))
	assertCloudConditionalProperties(t, "artifacts.delete.artifact", artifactDelete.Schema.Response["artifact"].Properties, artifactFields)
	runEvents, _ := ActionSpecFor(executionV2Name("runs.events"))
	assertCloudConditionalProperties(t, "runs.events.events[]", runEvents.Schema.Response["events"].Items.Properties, eventFields)
	if truncated := runEvents.Schema.Response["history_truncated"]; truncated.Type != "boolean" || !truncated.Required || truncated.Presence == nil || truncated.Presence.Present != "true_when_after_sequence_precedes_retained_history" {
		t.Fatalf("runs.events history_truncated schema=%#v", truncated)
	}
	sequenceRule := runEvents.Schema.Response["events"].Items.Properties["sequence"].Presence
	if sequenceRule == nil || sequenceRule.Present != "positive_contiguous_after_previous;first_equals_after_sequence_plus_one_unless_history_truncated" {
		t.Fatalf("runs.events sequence schema=%#v", sequenceRule)
	}

	for _, field := range []ActionFieldSchema{
		planGet.Schema.Response["plan"].Properties["account_generation"],
		planList.Schema.Response["plans"].Items.Properties["account_generation"],
		runEvents.Schema.Response["events"].Items.Properties["account_generation"],
	} {
		if field.Required || field.Presence == nil || field.Presence.Present == "" {
			t.Fatalf("account_generation field = %#v", field)
		}
	}
	quote := planGet.Schema.Response["plan"].Properties["quote"]
	compute := planGet.Schema.Response["plan"].Properties["compute"]
	if status := planGet.Schema.Response["plan"].Properties["status"]; status.Presence == nil || status.Presence.Present != "exact:waiting_user" {
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
