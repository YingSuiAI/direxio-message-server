package p2p

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentruntime "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/runtime"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestAgentSecretFailureSuppressesSecretDependentCapabilities(t *testing.T) {
	service := &Service{
		scheduleRunning:   true,
		agentTaskRuntime:  &agentruntime.Worker{},
		agentScheduleLoop: &agentruntime.ScheduleLoop{},
		agentTaskExecutor: &embeddedTaskExecutor{},
	}
	for _, capability := range []string{
		"model_profiles.server",
		"task",
		"confirmation",
		"schedules.server",
		"mcp",
		"aws.control",
		"workload.aws_ssm",
		"workload.aws_ecs",
	} {
		if service.embeddedAgentCapabilityReady(capability) {
			t.Fatalf("capability %q published while Agent secrets are unavailable", capability)
		}
	}
}

func TestDatabaseServiceMissingAgentKeyringFailsClosedWithoutCreatingIt(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(closeDB)
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyring := filepath.Join(dir, "missing-keyring.json")
	service, err := NewServiceWithStore(ctx, Config{ServerName: "example.com", AgentSecretKeyringFile: keyring}, store)
	if err != nil {
		t.Fatalf("ordinary service startup must survive unavailable Agent secrets: %v", err)
	}
	if service.agentSecretReady || !errors.Is(service.modelProfileInitErr, p2pstorage.ErrAgentSecretKeyringUnavailable) {
		t.Fatalf("Agent secret readiness=%v error=%v", service.agentSecretReady, service.modelProfileInitErr)
	}
	if _, err := os.Stat(keyring); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("server startup created missing keyring: %v", err)
	}
	if service.embeddedAgentCapabilityReady("model_profiles.server") || service.embeddedAgentCapabilityReady("task") {
		t.Fatal("Agent capability published without a verified keyring")
	}
	if !service.embeddedAgentCapabilityReady("memory.server") {
		t.Fatal("in-process PostgreSQL knowledge and memory must remain ready without an Agent keyring")
	}
	backends, apiErr := service.Handle(ctx, "agent.backends.get", nil)
	if apiErr != nil {
		t.Fatalf("agent.backends.get: %v", apiErr)
	}
	embedded, ok := backends.(map[string]any)["embedded"].(map[string]any)
	if !ok {
		t.Fatalf("embedded backend missing from response: %#v", backends)
	}
	capabilities, ok := embedded["capabilities"].([]string)
	if !ok {
		t.Fatalf("embedded capabilities have unexpected type: %#v", embedded["capabilities"])
	}
	memoryReady := false
	for _, capability := range capabilities {
		if capability == "memory.server" {
			memoryReady = true
			break
		}
	}
	if !memoryReady {
		t.Fatalf("single-image PostgreSQL backend did not advertise memory.server: %#v", capabilities)
	}
	if _, apiErr := service.Handle(ctx, "profile.get", nil); apiErr != nil {
		t.Fatalf("ordinary ProductCore remains available: %v", apiErr)
	}
}
