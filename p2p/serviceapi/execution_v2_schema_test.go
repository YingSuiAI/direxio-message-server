package serviceapi

import "testing"

func TestExecutionV2ActionsAreOwnerHTTPAndWSWithStrictMutations(t *testing.T) {
	want := []string{"projects.analyze", "analyses.get", "targets.list", "targets.get", "targets.import", "targets.reserve", "targets.observe", "plans.create", "plans.revise", "plans.get", "plans.list", "deployments.list", "deployments.get", "deployments.events", "runs.create", "runs.get", "runs.list", "runs.cancel", "runs.retry", "runs.reconcile", "runs.events", "confirmations.get", "confirmations.list", "confirmations.confirm", "confirmations.reject", "artifacts.get", "service_bindings.list", "service_bindings.get", "service_bindings.invoke", "secrets.create", "secrets.get", "secrets.list", "secrets.revoke"}
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
	for _, bare := range []string{"plans.revise", "runs.cancel", "runs.retry", "runs.reconcile", "confirmations.confirm", "confirmations.reject", "service_bindings.invoke", "secrets.revoke"} {
		name := executionV2Name(bare)
		s, _ := ActionSpecFor(name)
		if !s.Schema.Request["idempotency_key"].Required || !s.Schema.Request["expected_revision"].Required {
			t.Fatalf("%s must require idempotency and expected_revision", name)
		}
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
