package serviceapi

import "testing"

func TestCoreConfirmationSchemasPinCloudWorkerIdentityRevisionAndQuote(t *testing.T) {
	for _, action := range []string{
		"agent.core.confirmations.get",
		"agent.core.confirmations.list",
		"agent.core.confirmations.confirm",
		"agent.core.confirmations.reject",
	} {
		t.Run(action, func(t *testing.T) {
			spec, ok := ActionSpecFor(action)
			if !ok || spec.Schema == nil {
				t.Fatalf("missing action schema for %s", action)
			}
			var confirmation map[string]ActionFieldSchema
			if action == "agent.core.confirmations.list" {
				items := spec.Schema.Response["confirmations"].Items
				if items == nil {
					t.Fatal("confirmation list item schema is missing")
				}
				confirmation = items.Properties
			} else {
				confirmation = spec.Schema.Response["confirmation"].Properties
			}
			for _, field := range []string{"confirmation_id", "owner_id", "binding", "task_id", "state", "revision", "created_at", "updated_at", "expires_at"} {
				if !confirmation[field].Required {
					t.Errorf("confirmation.%s must be required", field)
				}
			}
			if rule := confirmation["owner_id"].Presence; rule == nil || rule.Present != "authenticated_owner_id" {
				t.Fatalf("confirmation owner authority=%#v", confirmation["owner_id"])
			}
			binding := confirmation["binding"].Properties
			if !binding["owner_id"].Required || binding["owner_id"].Presence == nil {
				t.Fatalf("binding owner authority=%#v", binding["owner_id"])
			}
			accountGeneration := binding["account_generation"]
			if accountGeneration.Required || accountGeneration.Presence == nil ||
				accountGeneration.Presence.Omitted != "confirmation_domain_without_owner_generation_fence" ||
				accountGeneration.Presence.Present != "positive_integer_required_for_cloud_worker.execute|execution_v2.run|extension.execute" {
				t.Errorf("conditional authority binding.account_generation=%#v", accountGeneration)
			}
			for _, field := range []string{
				"execution_id", "plan_id", "plan_revision", "quote",
			} {
				value := binding[field]
				if value.Required || value.Presence == nil || value.Presence.Omitted != "non_cloud_worker_confirmation" || value.Presence.Present == "" {
					t.Errorf("conditional Cloud Worker binding.%s=%#v", field, value)
				}
			}
			quote := binding["quote"]
			if quote.Type != "object" || quote.Presence == nil || len(quote.Properties) != 6 {
				t.Fatalf("conditional Cloud Worker quote=%#v", quote)
			}
			for _, field := range []string{"amount_micros", "compute_micros_per_hour", "currency", "source_time", "expires_at", "maximum_authorized_cost_micros"} {
				if !quote.Properties[field].Required {
					t.Errorf("Cloud Worker quote.%s must be required", field)
				}
			}
			grant := binding["secret_grants"].Items
			if grant == nil || !grant.Properties["purpose"].Required {
				t.Fatalf("secret grant purpose schema=%#v", grant)
			}
			if _, present := grant.Properties["secret_revision"]; present {
				t.Fatal("public confirmation schema exposes unsupported secret_revision")
			}
			for _, field := range []string{"reference_id", "binding_digest"} {
				value := grant.Properties[field]
				if value.Required || value.Presence == nil || value.Presence.Omitted != "cloud_worker.execute exposes purpose only" || value.Presence.Present != "required_for_non_cloud_worker_confirmation" {
					t.Errorf("secret grant conditional %s=%#v", field, value)
				}
			}
		})
	}
}
