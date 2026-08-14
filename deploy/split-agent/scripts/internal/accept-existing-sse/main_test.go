package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStartOnceAndResumeSSEWithoutDuplicate(t *testing.T) {
	const conversationID = "22222222-2222-4222-8222-222222222222"
	const turnID = "11111111-1111-4111-8111-111111111111"
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/turns"):
			posts.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"turn_id":"` + turnID + `","idempotency_key":"33333333-3333-4333-8333-333333333333","conversation_id":"` + conversationID + `","revision":1,"seq":1}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			after := r.URL.Query().Get("after_seq")
			if r.Header.Get("Last-Event-ID") != after {
				t.Fatalf("cursor mismatch: query=%q header=%q", after, r.Header.Get("Last-Event-ID"))
			}
			if after == "0" {
				_, _ = w.Write([]byte("id: 1\nevent: accepted\ndata: {\"event\":\"accepted\",\"seq\":1}\n\n"))
				return
			}
			_, _ = w.Write([]byte("id: 2\nevent: done\ndata: {\"event\":\"done\",\"seq\":2,\"data\":{\"text\":\"ok\"}}\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	receipt, err := startTurn(context.Background(), server.URL, "owner-token", conversationID, map[string]any{"conversation_id": conversationID})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	after, terminal, err := watchTurn(context.Background(), server.URL, "owner-token", receipt, 0, true, "done", &output)
	if err != nil || terminal || after != 1 {
		t.Fatalf("first watch = (%d,%v,%v)", after, terminal, err)
	}
	after, terminal, err = watchTurn(context.Background(), server.URL, "owner-token", receipt, after, false, "done", &output)
	if err != nil || !terminal || after != 2 || posts.Load() != 1 {
		t.Fatalf("resume = (%d,%v,%v) posts=%d", after, terminal, err, posts.Load())
	}
	if strings.Count(output.String(), `"seq":1`) != 1 || strings.Count(output.String(), `"seq":2`) != 1 {
		t.Fatalf("output duplicated or lost events: %s", output.String())
	}
}

func TestReadSSEFrameRejectsEnvelopeMismatch(t *testing.T) {
	_, _, err := readSSEFrame(bufio.NewScanner(strings.NewReader("id: 2\nevent: done\ndata: {\"event\":\"done\",\"seq\":1}\n\n")))
	if err == nil {
		t.Fatal("expected mismatched event metadata to fail")
	}
}

func TestStopDurableTurnUsesReceiptIdentityAndRevision(t *testing.T) {
	const turnID = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Action != "agent.chat.turn.stop" || body.Params["turn_id"] != turnID || body.Params["expected_revision"] != float64(4) {
			t.Fatalf("stop request = %#v", body)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	if err := stopDurableTurn(context.Background(), server.URL, "owner-token", map[string]any{"turn_id": turnID, "revision": float64(4)}); err != nil {
		t.Fatal(err)
	}
}
