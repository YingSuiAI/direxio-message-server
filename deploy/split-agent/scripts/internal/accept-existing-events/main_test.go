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

func TestGlobalEventsDisconnectResumeAndCallLifecycle(t *testing.T) {
	const callID = "call_acceptance_test"
	var eventGets atomic.Int32
	var actionPosts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owner-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodGet && r.URL.Path == "/_p2p/events" {
			eventGets.Add(1)
			after := r.URL.Query().Get("after_seq")
			if r.Header.Get("Last-Event-ID") != after {
				t.Fatalf("cursors query=%q header=%q", after, r.Header.Get("Last-Event-ID"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if after == "0" {
				_, _ = w.Write([]byte("id: 10\nevent: call.changed\ndata: {\"seq\":10,\"type\":\"call.changed\",\"payload\":{\"call\":{\"call_id\":\"" + callID + "\",\"state\":\"ringing\"}}}\n\n"))
			} else {
				_, _ = w.Write([]byte("id: 11\nevent: call.changed\ndata: {\"seq\":11,\"type\":\"call.changed\",\"payload\":{\"call\":{\"call_id\":\"" + callID + "\",\"state\":\"ended\"}}}\n\n"))
			}
			return
		}
		actionPosts.Add(1)
		var envelope struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		state := "ringing"
		if envelope.Action == "calls.event" || envelope.Action == "calls.get" {
			state = "ended"
		}
		_, _ = w.Write([]byte(`{"call_id":"` + callID + `","state":"` + state + `"}`))
	}))
	defer server.Close()

	first, scanner, err := openEvents(context.Background(), server.URL, "owner-token", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := action(context.Background(), server.URL, "owner-token", "calls.create", map[string]any{"call_id": callID}); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	created, err := readMatchingCall(scanner, callID, "ringing", &output)
	first.Body.Close()
	if err != nil || created.Seq != 10 {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if _, err := action(context.Background(), server.URL, "owner-token", "calls.event", map[string]any{"call_id": callID, "event": "ended"}); err != nil {
		t.Fatal(err)
	}
	second, scanner, err := openEvents(context.Background(), server.URL, "owner-token", created.Seq)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := readMatchingCall(scanner, callID, "ended", &output)
	second.Body.Close()
	if err != nil || ended.Seq != 11 || eventGets.Load() != 2 || actionPosts.Load() != 2 {
		t.Fatalf("ended=%#v err=%v event_gets=%d posts=%d", ended, err, eventGets.Load(), actionPosts.Load())
	}
}

func TestReadFrameIgnoresHeartbeat(t *testing.T) {
	id, event, data, ok, err := readFrame(bufio.NewScanner(strings.NewReader(": heartbeat\n\nid: 4\nevent: call.changed\ndata: {\"seq\":4}\n\n")))
	if err != nil || !ok || id != 4 || event != "call.changed" || string(data) != `{"seq":4}` {
		t.Fatalf("frame=(%d,%q,%q,%v,%v)", id, event, data, ok, err)
	}
}
