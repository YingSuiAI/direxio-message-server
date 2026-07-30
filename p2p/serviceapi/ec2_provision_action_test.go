package serviceapi

import "testing"

func TestEC2ProvisionActionsAreOwnerTypedAndHTTPWS(t *testing.T) {
	want := map[string][]string{
		"agent.core.aws.ec2_provisions.plan": {
			"credential_id", "expected_credential_revision", "region", "stack_name", "display_name", "instance_type", "volume_gib", "public_http", "acknowledge_public_exposure", "idempotency_key",
		},
		"agent.core.aws.ec2_provisions.get":             {"provision_id"},
		"agent.core.aws.ec2_provisions.list":            {"state", "page_size", "page_token"},
		"agent.core.aws.ec2_provisions.events":          {"provision_id", "after_sequence", "limit"},
		"agent.core.aws.ec2_provisions.create.request":  {"provision_id", "expected_revision", "idempotency_key"},
		"agent.core.aws.ec2_provisions.destroy.request": {"provision_id", "expected_revision", "idempotency_key"},
		"agent.core.aws.ec2_provisions.retry":           {"provision_id", "expected_revision", "idempotency_key"},
		"agent.core.aws.ec2_provisions.geolibre_install.plan": {
			"provision_id", "expected_revision", "expires_at", "idempotency_key",
		},
		"agent.core.aws.ec2_provisions.geolibre_install.request": {
			"provision_id", "expected_revision", "plan_id", "expires_at", "plan_revision", "plan_digest", "idempotency_key", "workload_id", "expected_workload_revision",
		},
	}
	for action, fields := range want {
		spec, ok := ActionSpecFor(action)
		if !ok {
			t.Fatalf("%s is not registered", action)
		}
		if spec.Auth != ActionAuthOwner || spec.Transport != ActionTransportHTTPAndWS {
			t.Fatalf("%s auth/transport = %q/%q", action, spec.Auth, spec.Transport)
		}
		if spec.Schema == nil {
			t.Fatalf("%s has no schema", action)
		}
		if len(spec.Schema.Request) != len(fields) {
			t.Fatalf("%s request fields = %v, want exactly %v", action, spec.Schema.Request, fields)
		}
		for _, field := range fields {
			if _, ok := spec.Schema.Request[field]; !ok {
				t.Errorf("%s missing request.%s", action, field)
			}
		}
		forbidden := []string{"owner_id", "template", "parameters", "command", "command_steps", "image", "image_uri", "image_digest", "provider", "provider_override", "access_key_id", "secret_access_key", "session_token"}
		for _, field := range forbidden {
			if _, ok := spec.Schema.Request[field]; ok {
				t.Errorf("%s exposes forbidden request.%s", action, field)
			}
		}
	}
}

func TestEC2ProvisionSchemasPinValidationAndTypedResponses(t *testing.T) {
	plan, _ := ActionSpecFor("agent.core.aws.ec2_provisions.plan")
	for _, field := range []string{"credential_id", "expected_credential_revision", "region", "stack_name", "display_name", "instance_type", "volume_gib", "public_http", "acknowledge_public_exposure", "idempotency_key"} {
		if !plan.Schema.Request[field].Required {
			t.Errorf("plan request.%s must be required", field)
		}
	}
	if got := plan.Schema.Request["credential_id"].Presence.Present; got != "canonical_uuid" {
		t.Fatalf("credential_id presence = %q", got)
	}
	if got := plan.Schema.Request["expected_credential_revision"].Presence.Present; got != "positive_integer" {
		t.Fatalf("expected_credential_revision presence = %q", got)
	}
	if got := plan.Schema.Request["acknowledge_public_exposure"].Presence.Present; got != "must_be_true" {
		t.Fatalf("public exposure acknowledgement presence = %q", got)
	}
	if _, ok := plan.Schema.Response["provision"].Properties["owner_digest"]; ok {
		t.Fatal("provision response unexpectedly exposes owner_digest")
	}
	if got := plan.Schema.Response["provision"].Properties["credential_binding_digest"]; got.Type != "string" || got.Required || got.Presence == nil || got.Presence.Present != "immutable_digest" {
		t.Fatalf("provision credential binding digest schema = %#v", got)
	}

	list, _ := ActionSpecFor("agent.core.aws.ec2_provisions.list")
	if got := list.Schema.Request["state"].Presence.Present; got != "one_of:planned|creating|active|destroying|destroyed|uncertain|failed" {
		t.Fatalf("state enum = %q", got)
	}
	for _, action := range []string{"agent.core.aws.ec2_provisions.create.request", "agent.core.aws.ec2_provisions.destroy.request"} {
		request, _ := ActionSpecFor(action)
		for _, response := range []string{"provision", "change", "task_id", "confirmation_id", "task", "confirmation"} {
			if !request.Schema.Response[response].Required {
				t.Errorf("%s response.%s must be required", action, response)
			}
		}
		if got := request.Schema.Response["change"].Properties["status"].Presence.Present; got != "one_of:waiting_user|running|succeeded|failed|canceled" {
			t.Fatalf("%s change status enum = %q", action, got)
		}
		if got := request.Schema.Response["provision"].Properties["state"].Presence.Present; got != "one_of:planned|creating|active|destroying|destroyed|uncertain|failed" {
			t.Fatalf("%s provision state enum = %q", action, got)
		}
		if _, ok := request.Schema.Response["owner_id"]; ok {
			t.Fatalf("%s response unexpectedly exposes owner_id", action)
		}
	}

	retry, _ := ActionSpecFor("agent.core.aws.ec2_provisions.retry")
	if len(retry.Schema.Response) != 1 || !retry.Schema.Response["provision"].Required {
		t.Fatalf("retry response must be provision-only: %#v", retry.Schema.Response)
	}

	events, _ := ActionSpecFor("agent.core.aws.ec2_provisions.events")
	event := events.Schema.Response["events"].Items.Properties
	for _, field := range []string{"event_id", "provision_id", "sequence", "kind", "revision", "at"} {
		if !event[field].Required {
			t.Errorf("event.%s must be required", field)
		}
	}
	if event["change_id"].Required || event["task_id"].Required {
		t.Fatal("event change_id/task_id must be optional projections")
	}
	if got := event["sequence"].Presence.Present; got != "monotonically_increasing_nonnegative_sequence" {
		t.Fatalf("event sequence presence = %q", got)
	}
	if got := events.Schema.Response["next_after_sequence"].Type; got != "integer" {
		t.Fatalf("next_after_sequence type = %q", got)
	}

	confirmation := eventsConfirmationSchema(t)
	for _, field := range []string{"confirmation_id", "task_id", "state", "revision", "created_at", "updated_at", "expires_at", "binding"} {
		if !confirmation[field].Required {
			t.Errorf("confirmation.%s must be required", field)
		}
	}
	binding := confirmation["binding"].Properties
	for _, field := range []string{"operation_domain", "target_id", "target_revision", "content_digest", "parameter_digest", "network_digest", "secret_grant_digest", "secret_grants"} {
		if !binding[field].Required {
			t.Errorf("confirmation.binding.%s must be required", field)
		}
	}
	for _, field := range []string{"terminal_reason", "terminal_code", "terminal_note"} {
		if _, ok := confirmation[field]; !ok {
			t.Errorf("confirmation.%s missing from handler projection schema", field)
		}
	}
	grant := binding["secret_grants"].Items.Properties
	for _, field := range []string{"reference_id", "purpose", "binding_digest"} {
		if !grant[field].Required {
			t.Errorf("confirmation.binding.secret_grants[].%s must be required", field)
		}
	}
	// This mirrors confirmationMap's handler-shaped payload. Every emitted
	// field must remain represented by the public confirmation schema.
	sample := map[string]any{
		"confirmation_id": "11111111-1111-4111-8111-111111111111", "task_id": "22222222-2222-4222-8222-222222222222", "state": "pending", "revision": int64(1),
		"created_at": "2026-07-30T00:00:00Z", "updated_at": "2026-07-30T00:00:00Z", "expires_at": "2026-07-31T00:00:00Z", "terminal_reason": "", "terminal_code": "", "terminal_note": "",
		"binding": map[string]any{"operation_domain": "aws", "target_id": "aws-target:sha256:example", "target_revision": int64(1), "content_digest": "a", "parameter_digest": "b", "network_digest": "c", "secret_grant_digest": "d", "network_grants": []any{}, "secret_grants": []any{}},
	}
	for field := range sample {
		if _, ok := confirmation[field]; !ok {
			t.Errorf("handler-shaped confirmation field %s missing from schema", field)
		}
	}

	for _, action := range []string{"agent.core.aws.ec2_provisions.create.request", "agent.core.aws.ec2_provisions.destroy.request", "agent.core.aws.ec2_provisions.retry"} {
		spec, _ := ActionSpecFor(action)
		if spec.Schema.Request["provision_id"].Presence.Present != "canonical_uuid" || spec.Schema.Request["expected_revision"].Presence.Present != "positive_integer" || spec.Schema.Request["idempotency_key"].Presence.Present != "canonical_uuid" {
			t.Fatalf("%s does not pin provision/revision/replay key", action)
		}
	}

	geoPlan, _ := ActionSpecFor("agent.core.aws.ec2_provisions.geolibre_install.plan")
	for _, field := range []string{"provision_id", "expected_revision", "expires_at", "idempotency_key"} {
		if !geoPlan.Schema.Request[field].Required {
			t.Fatalf("geolibre plan request.%s must be required", field)
		}
	}
	for _, field := range []string{"plan", "provision_id", "provision_revision", "expires_at"} {
		if !geoPlan.Schema.Response[field].Required {
			t.Fatalf("geolibre plan response.%s must be required", field)
		}
	}
	geoPlanFields := geoPlan.Schema.Response["plan"].Properties
	for _, forbidden := range []string{"command_steps", "image_uri", "owner_digest", "typed_secret_grants"} {
		if _, ok := geoPlanFields[forbidden]; ok {
			t.Fatalf("geolibre plan exposes forbidden field %s", forbidden)
		}
	}
	if !geoPlanFields["release"].Required || !geoPlanFields["typed_target"].Required || !geoPlanFields["typed_resource_limits"].Required {
		t.Fatal("geolibre plan is missing fixed release/provision-bound fields")
	}
	for _, field := range []string{"version", "commit", "image_digest", "manifest_digest", "command_digest"} {
		if geoPlanFields["release"].Properties[field].Presence == nil {
			t.Fatalf("geolibre release.%s is not marked fixed", field)
		}
	}
	geoRequest, _ := ActionSpecFor("agent.core.aws.ec2_provisions.geolibre_install.request")
	for _, field := range []string{"provision_id", "expected_revision", "plan_id", "expires_at", "idempotency_key"} {
		if !geoRequest.Schema.Request[field].Required {
			t.Fatalf("geolibre request request.%s must be required", field)
		}
	}
	for _, field := range []string{"plan_revision", "plan_digest"} {
		if !geoRequest.Schema.Request[field].Required {
			t.Fatalf("geolibre request request.%s must be required", field)
		}
	}
	for _, field := range []string{"workload_id", "expected_workload_revision"} {
		if geoRequest.Schema.Request[field].Required {
			t.Fatalf("geolibre request request.%s must remain optional", field)
		}
	}
	for _, field := range []string{"plan", "provision_id", "provision_revision", "expires_at", "workload_id", "operation", "task_id", "confirmation_id", "confirmation"} {
		if !geoRequest.Schema.Response[field].Required {
			t.Fatalf("geolibre request response.%s must be required", field)
		}
	}
	if !geoRequest.Schema.Response["task"].Required || !geoRequest.Schema.Response["operation"].Properties["summary"].Required {
		t.Fatal("geolibre request response must include full task and operation")
	}
	for _, field := range []string{"expected_workload_revision"} {
		if !geoRequest.Schema.Response[field].Required || !geoRequest.Schema.Response["operation"].Properties[field].Required {
			t.Fatalf("geolibre request response.%s must be required", field)
		}
	}
	if got := geoRequest.Schema.Response["task"].Properties["expected_workload_revision"]; got.Type != "integer" || got.Required || got.Presence == nil {
		t.Fatalf("geolibre task expected workload revision schema = %#v", got)
	}
	if got := geoRequest.Schema.Request["expected_workload_revision"].Presence.Present; got != "positive_integer_when_workload_id_present" {
		t.Fatalf("geolibre expected workload revision presence = %q", got)
	}
	for _, field := range []string{"provision_id", "provision_revision", "credential_id", "credential_revision", "account_id", "region", "instance_id", "public_endpoint", "service", "port", "exposure", "sidecar"} {
		if !geoRequest.Schema.Response["plan"].Properties["typed_target"].Properties[field].Required {
			t.Fatalf("geolibre typed_target.%s must be required", field)
		}
	}
	if got := len(geoRequest.Schema.Response["plan"].Properties["typed_target"].Properties); got != 12 {
		t.Fatalf("geolibre typed_target fields = %d, want exact flat projection", got)
	}

	for _, action := range []string{"agent.core.aws.credentials.create", "agent.core.aws.credentials.update"} {
		spec, _ := ActionSpecFor(action)
		credential := spec.Schema.Response["credential"]
		for _, field := range []string{"credential_id", "name", "region", "account_id", "user_arn", "access_key_configured", "secret_access_key_configured", "session_token_configured", "verified_revision", "revision", "created_at", "updated_at"} {
			if !credential.Properties[field].Required {
				t.Fatalf("%s credential.%s must be required", action, field)
			}
		}
		for _, forbidden := range []string{"has_access_key", "has_secret_key", "has_session_token", "access_key", "secret_access_key", "session_token"} {
			if _, ok := credential.Properties[forbidden]; ok {
				t.Fatalf("%s credential exposes forbidden field %s", action, forbidden)
			}
		}
		if credential.Properties["tested_at"].Presence == nil || credential.Properties["tested_at"].Presence.Omitted != "when_verified_revision_differs" {
			t.Fatalf("%s credential.tested_at must be conditional on verification", action)
		}
	}
	credentialList, _ := ActionSpecFor("agent.core.aws.credentials.list")
	if !credentialList.Schema.Response["credentials"].Required {
		t.Fatal("credential list response must be required")
	}
}

func eventsConfirmationSchema(t *testing.T) map[string]ActionFieldSchema {
	t.Helper()
	spec, ok := ActionSpecFor("agent.core.aws.ec2_provisions.create.request")
	if !ok || spec.Schema == nil {
		t.Fatal("create request confirmation schema missing")
	}
	return spec.Schema.Response["confirmation"].Properties
}
