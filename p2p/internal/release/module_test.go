package release

import (
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/releasecontrol"
)

func TestDirectStatusAndActiveJobAcceptDevelopmentServerVersions(t *testing.T) {
	status := releasecontrol.DirectStatus{
		Available:      true,
		CurrentVersion: "dev1.1.7",
		DesiredState:   "running",
	}
	if !validDirectUpdaterStatus(status, "dev1.1.7") {
		t.Fatal("development updater status must match the running development server")
	}

	job := directActiveJobMap(&releasecontrol.ActiveJob{
		JobID:            "job_dev",
		Status:           "pulling",
		CurrentVersion:   "dev1.1.7",
		TargetVersion:    "dev1.1.8",
		ServiceAvailable: true,
	})
	result, ok := job.(map[string]any)
	if !ok || result["current_version"] != "dev1.1.7" || result["target_version"] != "dev1.1.8" {
		t.Fatalf("unexpected active job: %#v", job)
	}
}
