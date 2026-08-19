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

type fixedObservedAgentVersionSource string

func (s fixedObservedAgentVersionSource) CurrentAgentVersion(context.Context) (string, error) {
	return string(s), nil
}

func TestUpdaterStatusAndActiveJobAcceptDevelopmentServerVersions(t *testing.T) {
	status := releasecontrol.Status{
		Available: true, UpdaterReady: true, DesiredState: "running",
	}
	if !validUpdaterStatus(status) {
		t.Fatal("updater status must not depend on the running server version")
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
			Available: true, UpdaterReady: true, DesiredState: "running",
		},
		"running mutation unavailable": {
			DesiredState: "running",
		},
		"upgrading": {
			Available: true, DesiredState: "upgrading", ActiveJob: serverJob,
		},
		"maintenance": {
			Available: true, DesiredState: "maintenance",
		},
		"deprovisioned": {
			Available: true, DesiredState: "deprovisioned",
		},
	}
	for name, status := range valid {
		t.Run("valid_"+name, func(t *testing.T) {
			if !validUpdaterStatus(status) {
				t.Fatalf("valid status rejected: %#v", status)
			}
		})
	}
	invalid := map[string]releasecontrol.Status{
		"ready_but_unavailable": {
			UpdaterReady: true, DesiredState: "running",
		},
		"upgrading_without_job": {
			Available: true, DesiredState: "upgrading",
		},
		"terminal_active_job": {
			Available: true, DesiredState: "upgrading",
			ActiveJob: &releasecontrol.ActiveJob{
				JobID: "job_server", Component: "server", Status: "failed",
				CurrentVersion: "v1.1.1", TargetVersion: "v1.1.2", ServiceAvailable: true,
			},
		},
		"missing_active_version": {
			Available: true, DesiredState: "upgrading",
			ActiveJob: &releasecontrol.ActiveJob{
				JobID: "job_server", Component: "server", Status: "pulling", TargetVersion: "v1.1.2", ServiceAvailable: true,
			},
		},
		"cross_channel_server_job": {
			Available: true, DesiredState: "upgrading",
			ActiveJob: &releasecontrol.ActiveJob{
				JobID: "job_server", Component: "server", Status: "pulling",
				CurrentVersion: "v1.1.1", TargetVersion: "dev1.1.2", ServiceAvailable: true,
			},
		},
		"maintenance_with_job": {
			Available: true, DesiredState: "maintenance", ActiveJob: serverJob,
		},
	}
	for name, status := range invalid {
		t.Run("invalid_"+name, func(t *testing.T) {
			if validUpdaterStatus(status) {
				t.Fatalf("invalid status accepted: %#v", status)
			}
		})
	}
}

func TestAgentStatusRejectsDevelopmentServerForStableMinimum(t *testing.T) {
	status := agentStatusMap(
		"v1.0.0", nil,
		releasecontrol.CentralAgentVersion{
			AppID: "1", ChannelID: "agents", Version: "v1.0.2", PreVersion: "v1.1.0",
		},
		"",
		"dev1.2.0",
	)
	if status["available"] != true || status["compatibility"] != "incompatible" || status["update_available"] != false {
		t.Fatalf("development server must not satisfy stable Agent minimum: %#v", status)
	}
	reasons, ok := status["reasons"].([]string)
	if !ok || len(reasons) != 1 || reasons[0] != "agent_requires_newer_server" {
		t.Fatalf("unexpected compatibility reasons: %#v", status["reasons"])
	}
}

func TestAgentStatusKeepsCentralLatestSeparateFromObservedCurrent(t *testing.T) {
	for _, centralVersion := range []string{"v1.0.7", "v1.0.72"} {
		t.Run(centralVersion, func(t *testing.T) {
			status := agentStatusMap(
				"v1.0.72", nil,
				releasecontrol.CentralAgentVersion{
					AppID: "1", ChannelID: "agents", Version: centralVersion, PreVersion: "v9.0.0",
				},
				"",
				"v1.1.0",
			)
			if status["current_version"] != "v1.0.72" || status["latest_version"] != centralVersion || status["update_available"] != false || status["compatibility"] != "compatible" {
				t.Fatalf("central latest and observed current were not kept separate: %#v", status)
			}
			reasons, ok := status["reasons"].([]string)
			if !ok || len(reasons) != 1 || reasons[0] != "agent_up_to_date" {
				t.Fatalf("unexpected reasons: %#v", status["reasons"])
			}
		})
	}
}

func TestAgentApplyRejectsDevelopmentServerForStableMinimum(t *testing.T) {
	module := &Module{centralAgentSource: fixedAgentVersionSource{version: releasecontrol.CentralAgentVersion{
		AppID: "1", ChannelID: "agents", Version: "v1.0.2", PreVersion: "v1.1.0",
	}}, agentVersionSource: fixedObservedAgentVersionSource("v1.0.0")}
	request := releasecontrol.ApplyRequest{TargetVersion: "v1.0.2"}
	apiErr := module.gateAgentUpdate(
		context.Background(),
		"dev1.2.0",
		&request,
	)
	if apiErr == nil || apiErr.Code != serverVersionIncompatible || request.MinimumServerVersion != "" {
		t.Fatalf("development server Agent gate = %#v, request=%#v", apiErr, request)
	}
}
