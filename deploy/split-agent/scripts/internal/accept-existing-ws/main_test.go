package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStopDurableTurnUsesAcceptedIdentityAndRevision(t *testing.T) {
	const turnID = "11111111-1111-4111-8111-111111111111"
	const conversationID = "22222222-2222-4222-8222-222222222222"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		var body struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Action != "agent.chat.turn.stop" {
			t.Fatalf("action = %q", body.Action)
		}
		if body.Params["turn_id"] != turnID || body.Params["expected_revision"] != float64(4) {
			t.Fatalf("stop params = %#v", body.Params)
		}
		_, _ = writer.Write([]byte(`{"turn_id":"` + turnID + `","state":"canceled"}`))
	}))
	defer server.Close()

	err := stopDurableTurn(context.Background(), server.URL, "owner-token", map[string]any{
		"turn_id": turnID, "conversation_id": conversationID, "revision": float64(4),
	})
	if err != nil {
		t.Fatal(err)
	}
}
