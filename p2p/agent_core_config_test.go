package p2p

import (
	"context"
	"testing"

	agentcore "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcore"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestPersistentServiceRejectsIncompleteEnabledAgentCoreConfig(t *testing.T) {
	_, err := NewServiceWithStore(context.Background(), Config{
		ServerName: "example.com",
		AgentCore:  agentcore.Config{Enabled: true},
	}, p2pstorage.NewMemoryStore())
	if err == nil {
		t.Fatal("enabled incomplete Agent Core config unexpectedly accepted")
	}
}
