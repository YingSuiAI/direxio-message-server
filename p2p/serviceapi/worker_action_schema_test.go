package serviceapi

import "testing"

func TestWorkerStatusPublishesAvailabilityWithoutChangingIdentity(t *testing.T) {
	for _, action := range []string{"agent.workers.list", "agent.workers.get"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Schema == nil {
			t.Fatalf("%s must publish an action schema", action)
		}
		status := spec.Schema.Response["worker"]
		if action == "agent.workers.list" {
			status = *spec.Schema.Response["workers"].Items
		}
		availability := status.Properties["availability"]
		if !availability.Required || availability.Presence == nil || availability.Presence.Present != "one_of:available|unavailable" {
			t.Fatalf("%s availability schema = %#v", action, availability)
		}
		if status.Properties["error"].Required || status.Properties["error"].Type != "string" {
			t.Fatalf("%s error schema = %#v", action, status.Properties["error"])
		}
		identity := status.Properties["identity"]
		if len(identity.Properties) != 8 {
			t.Fatalf("%s Worker identity fields = %d, want 8", action, len(identity.Properties))
		}
		for _, field := range []string{"worker_id", "instance_id", "key_pair_id", "security_group_id", "credential_id", "credential_revision", "account_id", "region"} {
			if !identity.Properties[field].Required {
				t.Errorf("%s Worker identity.%s must remain required", action, field)
			}
		}
	}
}
