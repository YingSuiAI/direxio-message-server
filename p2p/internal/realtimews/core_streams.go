package realtimews

import (
	"net/http"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

// rejectAgentCoreStream keeps the retired wire frame parseable for older
// clients while making it impossible to reach an external Core transport.
// Native Agent streaming is exclusively client.native_agent_stream.
func (m *Module) rejectAgentCoreStream(client *connection, frame map[string]any) {
	id := actionbase.String(frame["turn_id"])
	if id == "" {
		id = actionbase.String(frame["id"])
	}
	client.send(map[string]any{
		"type":      "server.agent_core_stream.error",
		"turn_id":   id,
		"code":      "agent_core_retired",
		"retryable": false,
		"status":    http.StatusGone,
		"first_seq": int64(0),
		"last_seq":  int64(0),
	})
}
