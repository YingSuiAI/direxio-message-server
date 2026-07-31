package serviceapi

import "testing"

func TestAWSActionSurfaceRetainsCredentialsOnly(t *testing.T) {
	for _, action := range []string{
		"agent.core.aws.credentials.create",
		"agent.core.aws.credentials.update",
		"agent.core.aws.credentials.delete",
		"agent.core.aws.credentials.list",
		"agent.core.aws.credentials.test",
	} {
		if _, ok := ActionSpecFor(action); !ok {
			t.Fatalf("missing credential action %q", action)
		}
	}
	for _, action := range []string{
		"agent.core.aws.plans.get", "agent.core.aws.plans.list", "agent.core.aws.plans.quote",
		"agent.core.aws.changes.get", "agent.core.aws.changes.list", "agent.core.aws.changes.status",
	} {
		if _, ok := ActionSpecFor(action); ok {
			t.Fatalf("retired AWS action remains registered: %q", action)
		}
	}
	update, ok := ActionSpecFor("agent.core.aws.credentials.update")
	if !ok || update.Schema == nil || !update.Schema.Request["expected_revision"].Required {
		t.Fatal("credential update must require expected_revision")
	}
}
