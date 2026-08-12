package serviceapi

import "testing"

func TestMemoryActionsPublishOwnerHTTPAndWSSchemas(t *testing.T) {
	for _, action := range []string{"agent.memory.config.get", "agent.memory.config.update", "agent.memory.status"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Auth != ActionAuthOwner || spec.Transport != ActionTransportHTTPAndWS || spec.Schema == nil {
			t.Fatalf("%s schema = %#v", action, spec)
		}
	}
	update, _ := ActionSpecFor("agent.memory.config.update")
	for _, field := range []string{"idempotency_key", "expected_revision", "enabled"} {
		if !update.Schema.Request[field].Required {
			t.Errorf("memory update request.%s must be required", field)
		}
	}
	status, _ := ActionSpecFor("agent.memory.status")
	for _, field := range []string{"enabled", "embedding_configured", "revision", "active_fact_count", "timeline_event_count", "pending_observation_count", "failed_observation_count", "facts", "timeline"} {
		if !status.Schema.Response[field].Required {
			t.Errorf("memory status response.%s must be required", field)
		}
	}
}
