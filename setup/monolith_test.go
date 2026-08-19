package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/p2p"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
)

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

func TestReleaseCatalogSourcesFromEnvRequireValidHTTPSOrigin(t *testing.T) {
	for _, origin := range []string{"", "http://imadmin.dirextalk.ai", "https://imadmin.dirextalk.ai/path", "https://user@imadmin.dirextalk.ai"} {
		t.Run(strings.ReplaceAll(origin, "/", "_"), func(t *testing.T) {
			t.Setenv("DIREXTALK_RELEASE_CATALOG_ORIGIN", origin)
			server, agent, err := releaseCatalogSourcesFromEnv()
			if err == nil || server != nil || agent != nil {
				t.Fatalf("invalid origin did not fail closed: server=%T agent=%T err=%v", server, agent, err)
			}
		})
	}
}

func TestReleaseCatalogSourcesFromEnvBuildBothChannelsFromOneOrigin(t *testing.T) {
	t.Setenv("DIREXTALK_RELEASE_CATALOG_ORIGIN", "https://imadmin.dirextalk.ai/")
	server, agent, err := releaseCatalogSourcesFromEnv()
	if err != nil || server == nil || agent == nil {
		t.Fatalf("valid release catalog origin rejected: server=%T agent=%T err=%v", server, agent, err)
	}
}

func TestAgentVersionSourceFromEnvIsExplicitAndValidated(t *testing.T) {
	t.Setenv("P2P_AGENT_VERSION_URL", "")
	source, err := agentVersionSourceFromEnv()
	if err != nil || source != nil {
		t.Fatalf("empty Agent version URL must stay disabled: source=%T err=%v", source, err)
	}

	t.Setenv("P2P_AGENT_VERSION_URL", "http://agent:8082/agent/v1/health")
	source, err = agentVersionSourceFromEnv()
	if err != nil || source == nil {
		t.Fatalf("valid Agent version URL rejected: source=%T err=%v", source, err)
	}

	t.Setenv("P2P_AGENT_VERSION_URL", "http://agent:8082/wrong")
	source, err = agentVersionSourceFromEnv()
	if err == nil || source != nil {
		t.Fatalf("invalid Agent version URL did not fail closed: source=%T err=%v", source, err)
	}
}
