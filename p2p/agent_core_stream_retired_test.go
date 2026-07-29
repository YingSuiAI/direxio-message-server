package p2p

import (
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"
)

func TestRetiredAgentCoreRealtimeFramesFailClosed(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()

	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("ready frame = %#v", got)
	}

	for _, frameType := range []string{"client.agent_core_stream", "client.agent_core_stream.cancel"} {
		writeRealtimeFrame(t, conn, map[string]any{"type": frameType, "turn_id": "retired-turn"})
		got := readRealtimeFrame(t, conn)
		if got["type"] != "server.agent_core_stream.error" ||
			got["turn_id"] != "retired-turn" ||
			got["code"] != "agent_core_retired" ||
			got["retryable"] != false ||
			got["status"] != float64(410) {
			t.Fatalf("%s response = %#v", frameType, got)
		}
	}
}
