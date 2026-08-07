package release

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/releasecontrol"
)

type fixedAgentVersionSource struct {
	version releasecontrol.CentralAgentVersion
}

func (s fixedAgentVersionSource) CurrentAgentVersion(context.Context) (releasecontrol.CentralAgentVersion, error) {
	return s.version, nil
}

func TestUpdaterStatusAndActiveJobAcceptDevelopmentServerVersions(t *testing.T) {
	status := releasecontrol.Status{
		Available:      true,
		UpdaterReady:   true,
		CurrentVersion: "dev1.1.7",
		DesiredState:   "running",
	}
	if !validUpdaterStatus(status, "dev1.1.7") {
		t.Fatal("development updater status must match the running development server")
	}

	job := activeJobMap(&releasecontrol.ActiveJob{
		JobID:            "job_dev",
		Component:        "server",
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

func TestUpdaterStatusRequiresSchemaEightStateMachine(t *testing.T) {
	serverJob := &releasecontrol.ActiveJob{
		JobID: "job_server", Component: "server", Status: "queued",
		CurrentVersion: "v1.1.1", TargetVersion: "v1.1.2", ServiceAvailable: true,
	}
	valid := map[string]releasecontrol.Status{
		"running": {
			Available: true, UpdaterReady: true, CurrentVersion: "v1.1.1", DesiredState: "running",
		},
		"upgrading": {
			Available: true, CurrentVersion: "v1.1.1", DesiredState: "upgrading", ActiveJob: serverJob,
		},
		"maintenance": {
			Available: true, CurrentVersion: "v1.1.1", DesiredState: "maintenance",
		},
		"deprovisioned": {
			Available: true, CurrentVersion: "v1.1.1", DesiredState: "deprovisioned",
		},
	}
	for name, status := range valid {
		t.Run("valid_"+name, func(t *testing.T) {
			if !validUpdaterStatus(status, "v1.1.1") {
				t.Fatalf("valid status rejected: %#v", status)
			}
		})
	}
	invalid := map[string]releasecontrol.Status{
		"running_not_ready": {
			Available: true, CurrentVersion: "v1.1.1", DesiredState: "running",
		},
		"upgrading_without_job": {
			Available: true, CurrentVersion: "v1.1.1", DesiredState: "upgrading",
		},
		"terminal_active_job": {
			Available: true, CurrentVersion: "v1.1.1", DesiredState: "upgrading",
			ActiveJob: &releasecontrol.ActiveJob{
				JobID: "job_server", Component: "server", Status: "failed",
				CurrentVersion: "v1.1.1", TargetVersion: "v1.1.2", ServiceAvailable: true,
			},
		},
		"missing_active_version": {
			Available: true, CurrentVersion: "v1.1.1", DesiredState: "upgrading",
			ActiveJob: &releasecontrol.ActiveJob{
				JobID: "job_server", Component: "server", Status: "pulling", TargetVersion: "v1.1.2", ServiceAvailable: true,
			},
		},
		"cross_channel_server_job": {
			Available: true, CurrentVersion: "v1.1.1", DesiredState: "upgrading",
			ActiveJob: &releasecontrol.ActiveJob{
				JobID: "job_server", Component: "server", Status: "pulling",
				CurrentVersion: "v1.1.1", TargetVersion: "dev1.1.2", ServiceAvailable: true,
			},
		},
		"maintenance_with_job": {
			Available: true, CurrentVersion: "v1.1.1", DesiredState: "maintenance", ActiveJob: serverJob,
		},
	}
	for name, status := range invalid {
		t.Run("invalid_"+name, func(t *testing.T) {
			if validUpdaterStatus(status, "v1.1.1") {
				t.Fatalf("invalid status accepted: %#v", status)
			}
		})
	}
}

func TestAgentStatusRejectsDevelopmentServerForStableMinimum(t *testing.T) {
	status := agentStatusMap(
		releasecontrol.AgentStatus{Available: true, CurrentVersion: "v1.0.0"},
		releasecontrol.CentralAgentVersion{
			AppID: "1", ChannelID: "agents", Version: "v1.0.2", PreVersion: "v1.1.0",
		},
		"",
		"dev1.2.0",
		true,
	)
	if status["available"] != true || status["compatibility"] != "incompatible" || status["update_available"] != false {
		t.Fatalf("development server must not satisfy stable Agent minimum: %#v", status)
	}
	reasons, ok := status["reasons"].([]string)
	if !ok || len(reasons) != 1 || reasons[0] != "agent_requires_newer_server" {
		t.Fatalf("unexpected compatibility reasons: %#v", status["reasons"])
	}
}

func TestAgentApplyRejectsDevelopmentServerForStableMinimum(t *testing.T) {
	module := &Module{centralAgentSource: fixedAgentVersionSource{version: releasecontrol.CentralAgentVersion{
		AppID: "1", ChannelID: "agents", Version: "v1.0.2", PreVersion: "v1.1.0",
	}}}
	request := releasecontrol.ApplyRequest{TargetVersion: "v1.0.2"}
	apiErr := module.gateAgentUpdate(
		context.Background(),
		releasecontrol.Status{Agent: releasecontrol.AgentStatus{Available: true, CurrentVersion: "v1.0.0"}},
		"dev1.2.0",
		&request,
	)
	if apiErr == nil || apiErr.Code != serverVersionIncompatible || request.MinimumServerVersion != "" {
		t.Fatalf("development server Agent gate = %#v, request=%#v", apiErr, request)
	}
}
