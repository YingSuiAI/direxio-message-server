package storage

import "testing"

func TestWorkloadDeploymentEventTypeMapsInternalKinds(t *testing.T) {
	tests := map[string]string{
		"requested":          "queued",
		"consumed":           "dispatch",
		"provider_error":     "error",
		"uncertain":          "error",
		"readback":           "progress",
		"recovered_readback": "progress",
		"completed":          "complete",
		"succeeded":          "succeeded",
		"destroyed":          "destroyed",
	}
	for kind, want := range tests {
		if got := workloadDeploymentEventType(kind, "running"); got != want {
			t.Fatalf("kind %q mapped to %q, want %q", kind, got, want)
		}
	}
	if got := workloadDeploymentEventType("legacy_unknown", "succeeded"); got != "succeeded" {
		t.Fatalf("status fallback = %q, want succeeded", got)
	}
	if got := workloadDeploymentEventType("credential=secret", "running"); got != "unknown" {
		t.Fatalf("unsafe kind = %q, want unknown", got)
	}
}

func TestWorkloadDeploymentEventStatusIsPublicProjection(t *testing.T) {
	for raw, want := range map[string]string{
		"waiting_user": "pending",
		"queued":       "pending",
		"running":      "running",
		"uncertain":    "uncertain",
		"completed":    "succeeded",
		"canceled":     "failed",
	} {
		got := normalizeDeploymentStatusForStorage(raw)
		if raw == "uncertain" {
			// Uncertain is retained by the workload domain until reconciliation;
			// it is not silently presented as success.
			if got != want {
				t.Fatalf("status %q normalized to %q, want %q", raw, got, want)
			}
			continue
		}
		if got != want {
			t.Fatalf("status %q normalized to %q, want %q", raw, got, want)
		}
	}
}
