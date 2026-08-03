package agent

import (
	"context"
	"net/http"
	"testing"
)

func TestEmbeddedExtensionCompatibilityActionsAreForbidden(t *testing.T) {
	module := New(Config{})
	handlers := module.Handlers()
	for _, action := range []string{"agent.runtime.run", "agent.skills.install", "agent.mcp.servers.install"} {
		t.Run(action, func(t *testing.T) {
			_, actionErr := handlers[action](context.Background(), map[string]any{})
			if actionErr == nil || actionErr.Status != http.StatusForbidden || actionErr.Error != "embedded native agent extensions are forbidden" {
				t.Fatalf("%s = %#v, want stable forbidden response", action, actionErr)
			}
		})
	}
}
