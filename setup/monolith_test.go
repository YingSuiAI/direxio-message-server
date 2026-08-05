package setup

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/p2p"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
)

type testAgentGRPCRunner struct{}

func (*testAgentGRPCRunner) Apply(context.Context, string) error { return nil }
func (*testAgentGRPCRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}
func (*testAgentGRPCRunner) Stream(context.Context, string, map[string]any, func(nativeagent.Event) error) error {
	return nil
}
func (*testAgentGRPCRunner) Close() error { return nil }
func (*testAgentGRPCRunner) WatchTeamCompletionEvents(
	context.Context,
	int64,
	func(p2p.AgentCompletionSourceEvent) error,
) error {
	return nil
}
func (*testAgentGRPCRunner) GetRuntimeProfile(context.Context) (p2p.AgentRuntimeProfileState, error) {
	return p2p.AgentRuntimeProfileState{}, nil
}
func (*testAgentGRPCRunner) UpdateRuntimeProfile(context.Context, p2p.AgentRuntimeProfileUpdate) (p2p.AgentRuntimeProfileState, error) {
	return p2p.AgentRuntimeProfileState{}, nil
}
func (*testAgentGRPCRunner) GetSearchProfile(context.Context) (p2p.AgentSearchProfileState, error) {
	return p2p.AgentSearchProfileState{}, nil
}
func (*testAgentGRPCRunner) UpdateSearchProfile(context.Context, p2p.AgentSearchProfileUpdate) (p2p.AgentSearchProfileState, error) {
	return p2p.AgentSearchProfileState{}, nil
}

type chatOnlyAgentGRPCRunner struct{}

func (*chatOnlyAgentGRPCRunner) Apply(context.Context, string) error { return nil }
func (*chatOnlyAgentGRPCRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}
func (*chatOnlyAgentGRPCRunner) Stream(context.Context, string, map[string]any, func(nativeagent.Event) error) error {
	return nil
}
func (*chatOnlyAgentGRPCRunner) Close() error { return nil }

func TestP2PDatabaseOptionsUseGlobalDatabaseWhenConfigured(t *testing.T) {
	cfg := &config.Dendrite{}
	cfg.Global.DatabaseOptions.ConnectionString = "postgres://localhost/global?sslmode=disable"
	cfg.RoomServer.Database.ConnectionString = "postgres://localhost/roomserver?sslmode=disable"

	got := p2pDatabaseOptions(cfg)
	if got.ConnectionString != "postgres://localhost/global?sslmode=disable" {
		t.Fatalf("expected global database, got %q", got.ConnectionString)
	}
}

func TestP2PDatabaseOptionsFallbackToRoomserverDatabase(t *testing.T) {
	cfg := &config.Dendrite{}
	cfg.RoomServer.Database.ConnectionString = "postgres://localhost/roomserver?sslmode=disable"

	got := p2pDatabaseOptions(cfg)
	if got.ConnectionString != "postgres://localhost/roomserver?sslmode=disable" {
		t.Fatalf("expected roomserver database fallback, got %q", got.ConnectionString)
	}
}

func TestPersistentP2PServiceRejectsSQLiteInsteadOfFallingBackToMemory(t *testing.T) {
	dbOpts := config.DatabaseOptions{ConnectionString: "file:p2p.db"}

	service, err := newPersistentP2PService(
		context.Background(),
		p2p.Config{ServerName: "example.com"},
		sqlutil.NewConnectionManager(nil, dbOpts),
		&dbOpts,
		nil,
	)

	if err == nil || !strings.Contains(err.Error(), "SQLite") {
		t.Fatalf("expected SQLite-backed startup to fail explicitly, got service=%v err=%v", service, err)
	}
	if service != nil {
		t.Fatalf("expected no in-memory P2P service fallback")
	}
}

func TestP2PEventRetentionFromEnv(t *testing.T) {
	t.Setenv("P2P_EVENT_RETENTION_MAX_ROWS", "5000")
	t.Setenv("P2P_EVENT_RETENTION_PRUNE_ON_WRITE", "true")

	if got := p2pEventRetentionMaxRowsFromEnv(); got != 5000 {
		t.Fatalf("expected max rows 5000, got %d", got)
	}
	if !p2pEventRetentionPruneOnWriteFromEnv() {
		t.Fatalf("expected prune on write to be enabled")
	}
}

func TestP2PEventRetentionInvalidEnvDisablesPruning(t *testing.T) {
	t.Setenv("P2P_EVENT_RETENTION_MAX_ROWS", "-1")
	t.Setenv("P2P_EVENT_RETENTION_PRUNE_ON_WRITE", "not-bool")

	if got := p2pEventRetentionMaxRowsFromEnv(); got != 0 {
		t.Fatalf("expected invalid max rows to disable retention, got %d", got)
	}
	if p2pEventRetentionPruneOnWriteFromEnv() {
		t.Fatalf("expected invalid prune flag to disable pruning")
	}
}

func TestP2PAgentGRPCBackendDefaultsToLocal(t *testing.T) {
	unsetAgentGRPCEnvironment(t)
	config, err := p2pAgentGRPCBackendConfigFromEnv()
	if err != nil || config.Enabled {
		t.Fatalf("default Agent backend config=%#v err=%v", config, err)
	}
	runner, err := newP2PAgentChatRunner(context.Background(), "example.com", config, nil)
	if err != nil || runner != nil {
		t.Fatalf("default local Runner=%v err=%v", runner, err)
	}
}

func TestP2PAgentGRPCBackendFailsClosedForIncompleteOrInlineSecretConfiguration(t *testing.T) {
	for name, configure := range map[string]func(*testing.T){
		"incomplete enabled backend": func(t *testing.T) {
			t.Setenv("P2P_AGENT_GRPC_ENABLED", "true")
		},
		"invalid enabled flag": func(t *testing.T) {
			t.Setenv("P2P_AGENT_GRPC_ENABLED", "sometimes")
		},
		"inline secret": func(t *testing.T) {
			t.Setenv("P2P_AGENT_GRPC_SERVICE_KEY", "sk-"+strings.Repeat("q", 24))
		},
	} {
		t.Run(name, func(t *testing.T) {
			unsetAgentGRPCEnvironment(t)
			configure(t)
			_, err := p2pAgentGRPCBackendConfigFromEnv()
			if err == nil || strings.Contains(err.Error(), "sk-") || strings.Contains(err.Error(), strings.Repeat("q", 24)) {
				t.Fatalf("unsafe Agent backend configuration error=%v", err)
			}
		})
	}
}

func TestP2PAgentGRPCBackendBuildsChatOnlyRunnerWithTrustedOwner(t *testing.T) {
	unsetAgentGRPCEnvironment(t)
	caFile := writeAgentMountedFile(t, "agent-ca.pem", 0o644)
	serviceKeyFile := writeAgentMountedFile(t, "agent-service-key", 0o600)
	t.Setenv("P2P_AGENT_GRPC_ENABLED", "true")
	t.Setenv("P2P_AGENT_GRPC_TARGET", "dns:///agent.internal:7443")
	t.Setenv("P2P_AGENT_GRPC_CA_FILE", caFile)
	t.Setenv("P2P_AGENT_GRPC_SERVER_NAME", "agent.internal")
	t.Setenv("P2P_AGENT_GRPC_SERVICE_KEY_FILE", serviceKeyFile)

	config, err := p2pAgentGRPCBackendConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var received AgentGRPCDialConfig
	wantRunner := &testAgentGRPCRunner{}
	runner, err := newP2PAgentChatRunner(context.Background(), "example.com", config, func(_ context.Context, value AgentGRPCDialConfig) (AgentGRPCRunner, error) {
		received = value
		return wantRunner, nil
	})
	if err != nil || runner != wantRunner {
		t.Fatalf("remote Chat Runner=%v err=%v", runner, err)
	}
	runtimeProfileClient, err := p2pAgentRuntimeProfileClient(config, runner)
	if err != nil || runtimeProfileClient != wantRunner {
		t.Fatalf("remote runtime profile client=%v err=%v", runtimeProfileClient, err)
	}
	searchProfileClient, err := p2pAgentSearchProfileClient(config, runner)
	if err != nil || searchProfileClient != wantRunner {
		t.Fatalf("remote search profile client=%v err=%v", searchProfileClient, err)
	}
	completionSource, err := p2pAgentCompletionSource(config, runner)
	if err != nil || completionSource != wantRunner {
		t.Fatalf("remote completion source=%v err=%v", completionSource, err)
	}
	if received.Target != "dns:///agent.internal:7443" || received.CAFile != caFile || received.ServerName != "agent.internal" ||
		received.ServiceKeyFile != serviceKeyFile || received.OwnerID != "dirextalk-project:example.com" {
		t.Fatalf("Agent dial config=%#v", received)
	}

	factoryCanary := "sk-" + strings.Repeat("r", 24)
	_, err = newP2PAgentChatRunner(context.Background(), "example.com", config, func(context.Context, AgentGRPCDialConfig) (AgentGRPCRunner, error) {
		return nil, errors.New("dial failed: " + factoryCanary)
	})
	if err == nil || strings.Contains(err.Error(), factoryCanary) {
		t.Fatalf("factory failure was not fail-closed and redacted: %v", err)
	}
	if _, err = p2pAgentRuntimeProfileClient(config, &chatOnlyAgentGRPCRunner{}); err == nil {
		t.Fatal("enabled Agent backend accepted a Runner without runtime profile capability")
	}
	if _, err = p2pAgentSearchProfileClient(config, &chatOnlyAgentGRPCRunner{}); err == nil {
		t.Fatal("enabled Agent backend accepted a Runner without search profile capability")
	}
	if _, err = p2pAgentCompletionSource(config, &chatOnlyAgentGRPCRunner{}); err == nil {
		t.Fatal("enabled Agent backend accepted a Runner without completion events")
	}
}

func unsetAgentGRPCEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"P2P_AGENT_GRPC_ENABLED", "P2P_AGENT_GRPC_TARGET", "P2P_AGENT_GRPC_CA_FILE",
		"P2P_AGENT_GRPC_SERVER_NAME", "P2P_AGENT_GRPC_SERVICE_KEY_FILE", "P2P_AGENT_GRPC_SERVICE_KEY",
	} {
		unsetEnvironmentVariable(t, name)
	}
}

func writeAgentMountedFile(t *testing.T, name string, mode os.FileMode) string {
	t.Helper()
	path := t.TempDir() + string(os.PathSeparator) + name
	if err := os.WriteFile(path, []byte("synthetic mounted material\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func unsetEnvironmentVariable(t *testing.T, name string) {
	t.Helper()
	previous, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, previous)
			return
		}
		_ = os.Unsetenv(name)
	})
}
