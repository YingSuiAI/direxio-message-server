package p2p

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

type productSSEFrame struct {
	id    string
	event string
	data  []byte
}

func TestProductEventsSSEReplaysAndReconnectsWithoutDuplicates(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	appendProductEvent(t, service, dirextalkdomain.Event{Seq: 1, Type: "profile.updated", RoomID: "!room:example.com", EventID: "$one", Payload: map[string]any{"name": "Alice"}, CreatedAt: "2026-08-14T00:00:00Z"})
	appendProductEvent(t, service, dirextalkdomain.Event{Seq: 2, Type: "contacts.updated", Payload: map[string]any{"count": float64(2)}, CreatedAt: "2026-08-14T00:00:01Z"})

	server := httptest.NewServer(newP2PTestRouter(service))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	response := openProductEvents(t, ctx, server.URL, service.AccessToken(), "0", "")
	frame := readProductSSEFrame(t, response.Body)
	if frame.id != "1" || frame.event != "profile.updated" {
		t.Fatalf("first SSE frame = %#v", frame)
	}
	var event dirextalkdomain.Event
	if err := json.Unmarshal(frame.data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Seq != 1 || event.Type != "profile.updated" || event.RoomID != "!room:example.com" || event.EventID != "$one" || event.CreatedAt != "2026-08-14T00:00:00Z" || event.Payload["name"] != "Alice" {
		t.Fatalf("event data changed: %#v", event)
	}
	cancel()
	_ = response.Body.Close()

	reconnected := openProductEvents(t, context.Background(), server.URL, service.AccessToken(), "1", "1")
	defer reconnected.Body.Close()
	frame = readProductSSEFrame(t, reconnected.Body)
	if frame.id != "2" || frame.event != "contacts.updated" {
		t.Fatalf("reconnected SSE frame = %#v, want only seq 2", frame)
	}
}

func TestProductEventsSSEExpiredCursorResetsAndCloses(t *testing.T) {
	service := NewService(Config{ServerName: "example.com", P2PEventRetentionMaxRows: 2, P2PEventRetentionPruneOnWrite: true})
	for sequence := int64(1); sequence <= 4; sequence++ {
		appendProductEvent(t, service, dirextalkdomain.Event{Seq: sequence, Type: "projection.updated", CreatedAt: "2026-08-14T00:00:00Z"})
	}
	server := httptest.NewServer(newP2PTestRouter(service))
	defer server.Close()

	response := openProductEvents(t, context.Background(), server.URL, service.AccessToken(), "1", "")
	defer response.Body.Close()
	frame := readProductSSEFrame(t, response.Body)
	if frame.id != "" || frame.event != "cursor_reset" {
		t.Fatalf("cursor reset frame = %#v", frame)
	}
	var reset struct {
		Since    int64  `json:"since"`
		MinSeq   int64  `json:"min_seq"`
		MaxSeq   int64  `json:"max_seq"`
		Count    int64  `json:"count"`
		Recovery string `json:"recovery"`
	}
	if err := json.Unmarshal(frame.data, &reset); err != nil {
		t.Fatal(err)
	}
	if reset.Since != 1 || reset.MinSeq != 3 || reset.MaxSeq != 4 || reset.Count != 2 || reset.Recovery != "bootstrap_required" {
		t.Fatalf("cursor reset data = %#v", reset)
	}
	remaining, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expired cursor replayed retained events: %q", remaining)
	}
}

func TestProductEventsSSERejectsAgentAndConflictingCursors(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	server := httptest.NewServer(newP2PTestRouter(service))
	defer server.Close()

	for _, test := range []struct {
		name       string
		token      string
		query      string
		lastID     string
		wantStatus int
	}{
		{name: "Agent token", token: service.AgentToken(), query: "0", wantStatus: http.StatusUnauthorized},
		{name: "conflicting cursors", token: service.AccessToken(), query: "1", lastID: "2", wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, server.URL+PathPrefix+"events?after_seq="+test.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+test.token)
			req.Header.Set("Accept", "text/event-stream")
			if test.lastID != "" {
				req.Header.Set("Last-Event-ID", test.lastID)
			}
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
		})
	}
}

func appendProductEvent(t *testing.T, service *Service, event dirextalkdomain.Event) {
	t.Helper()
	if err := service.eventsModule.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

func openProductEvents(t *testing.T, ctx context.Context, baseURL, token, afterSeq, lastEventID string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+PathPrefix+"events?after_seq="+afterSeq, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("events status = %d body=%s", response.StatusCode, body)
	}
	return response
}

func readProductSSEFrame(t *testing.T, body io.Reader) productSSEFrame {
	t.Helper()
	frame := productSSEFrame{}
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			return frame
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "id":
			if _, err := strconv.ParseInt(value, 10, 64); err != nil {
				t.Fatalf("invalid event id %q", value)
			}
			frame.id = value
		case "event":
			frame.event = value
		case "data":
			frame.data = append(frame.data, value...)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("SSE stream closed before a complete frame")
	return productSSEFrame{}
}
