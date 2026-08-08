package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	realtimewsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/realtimews"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type recordingReadMarkerStore struct {
	Store
	mu       sync.Mutex
	attempts int
	saved    []readMarker
	err      error
}

func TestRealtimeWSErrorIncludesStructuredRecoveryFields(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.actions["test.recovery"] = func(context.Context, map[string]any) (any, *apiError) {
		err := codedError(http.StatusGone, "request_expired", "request expired")
		err.OperationID = "op_test"
		err.CurrentRoomID = "!current:example.com"
		return nil, err
	}
	frame := service.handleRealtimeWSRequest(context.Background(), realtimeWSTicket{Role: "owner"}, map[string]any{
		"id": "request-1", "action": "test.recovery", "params": map[string]any{},
	})
	if frame["ok"] != false || frame["code"] != "request_expired" || frame["error_code"] != "request_expired" ||
		frame["operation_id"] != "op_test" || frame["current_room_id"] != "!current:example.com" {
		t.Fatalf("WS recovery error envelope changed: %#v", frame)
	}
}

func (s *recordingReadMarkerStore) SaveReadMarker(ctx context.Context, marker readMarker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.err != nil {
		return s.err
	}
	if err := s.Store.SaveReadMarker(ctx, marker); err != nil {
		return err
	}
	s.saved = append(s.saved, marker)
	return nil
}

func (s *recordingReadMarkerStore) snapshot() (int, []readMarker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts, append([]readMarker(nil), s.saved...)
}

func TestRealtimeWSTicketCreateIssuesSingleUseTicketForOwner(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)

	ticketResponse := mustCreateRealtimeWSTicketResponse(t, router, service.AccessToken())
	if int64(ticketResponse["expires_in_ms"].(float64)) != int64(realtimewsmodule.TicketTTL/time.Millisecond) {
		t.Fatalf("expected ticket TTL to be exposed as %dms, got %#v", int64(realtimewsmodule.TicketTTL/time.Millisecond), ticketResponse)
	}
	if int64(ticketResponse["expires_in_ms"].(float64)) != 120000 {
		t.Fatalf("expected weak-network ticket TTL to be 120000ms, got %#v", ticketResponse)
	}
	ownerTicket, _ := ticketResponse["ticket"].(string)
	if ownerTicket == "" {
		t.Fatal("expected owner ticket")
	}

	if err := service.consumeRealtimeWSTicket(ownerTicket); err != nil {
		t.Fatalf("expected owner ticket to be valid once: %v", err)
	}
	if err := service.consumeRealtimeWSTicket(ownerTicket); err == nil {
		t.Fatal("expected owner ticket to be single-use")
	}
}

func TestRealtimeWSUpgradeFailureDoesNotConsumeTicket(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)
	ticket := mustCreateRealtimeWSTicket(t, router, service.AccessToken())

	req := httptest.NewRequest(http.MethodGet, "/_p2p/ws?ticket="+ticket, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if err := service.consumeRealtimeWSTicket(ticket); err != nil {
		t.Fatalf("expected failed websocket upgrade to leave ticket usable: %v", err)
	}
}

func TestAccountStateClearInvalidatesRealtimeTickets(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	ticket, apiErr := service.createRealtimeWSTicketForToken(service.AccessToken())
	if apiErr != nil {
		t.Fatalf("create WS ticket: %#v", apiErr)
	}
	service.clearAccountStateInMemory()
	if err := service.consumeRealtimeWSTicket(ticket["ticket"].(string)); err == nil {
		t.Fatal("account-state clear left a realtime ticket usable")
	}
}

func TestRealtimeWSTicketCreateRejectsMissingOrInvalidBearer(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)

	for name, token := range map[string]string{
		"missing": "",
		"invalid": "not-valid",
		"agent":   service.AgentToken(),
	} {
		t.Run(name, func(t *testing.T) {
			req := jsonRequest(t, "/_p2p/command", map[string]any{
				"action": serviceapi.RealtimeWSTicketAction,
				"params": map[string]any{},
			})
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRealtimeWSAcceptsTicketAndReplaysEvents(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)
	if err := service.appendP2PEvent(context.Background(), p2pEvent{
		Seq:     1,
		Type:    "old.event",
		RoomID:  "!room:example.com",
		EventID: "$old",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.appendP2PEvent(context.Background(), p2pEvent{
		Seq:     2,
		Type:    "fresh.event",
		RoomID:  "!room:example.com",
		EventID: "$fresh",
		Payload: map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{
		"type":  "client.hello",
		"since": 1,
	})
	ready := readRealtimeFrame(t, conn)
	if ready["type"] != "server.ready" {
		t.Fatalf("expected server.ready, got %#v", ready)
	}
	capabilities, ok := ready["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("expected server.ready capabilities object, got %#v", ready["capabilities"])
	}
	if _, ok := capabilities["native_agent_turns"]; ok {
		t.Fatalf("unconfigured Native Agent capability was advertised: %#v", capabilities)
	}
	if _, ok := ready["native_agent_turns"]; ok {
		t.Fatalf("server.ready must not expose top-level native_agent_turns: %#v", ready)
	}
	event := readRealtimeFrame(t, conn)
	if event["type"] != "server.event" {
		t.Fatalf("expected server.event, got %#v", event)
	}
	payload, ok := event["event"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested event payload, got %#v", event)
	}
	if payload["type"] != "fresh.event" || int64(payload["seq"].(float64)) != 2 {
		t.Fatalf("expected replay of seq 2 fresh event, got %#v", payload)
	}
}

func TestRealtimeWSStreamsLiveEventsAndTracksClientState(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type":       "client.lifecycle",
		"foreground": true,
		"state":      "resumed",
		"hidden":     false,
		"flags":      map[string]any{"resumed": true},
	})
	writeRealtimeFrame(t, conn, map[string]any{
		"type":    "client.focus",
		"room_id": "!live:example.com",
		"flags":   map[string]any{"chat_open": true},
	})
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.ack", "seq": 7})

	waitForRealtimePushSuppressed(t, service, "!live:example.com")
	if service.shouldSuppressPushForRoom("!other:example.com") {
		t.Fatal("expected different room to keep normal push behavior")
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type":       "client.lifecycle",
		"foreground": true,
		"state":      "hidden",
		"hidden":     true,
		"flags":      map[string]any{"hidden": true, "background": true},
	})
	waitForRealtimePushNotSuppressed(t, service, "!live:example.com")
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.lifecycle", "foreground": true, "state": "resumed"})
	waitForRealtimePushSuppressed(t, service, "!live:example.com")

	if err := service.appendP2PEvent(context.Background(), p2pEvent{
		Type:    "live.event",
		RoomID:  "!live:example.com",
		EventID: "$live",
	}); err != nil {
		t.Fatal(err)
	}
	frame := readRealtimeFrame(t, conn)
	if frame["type"] != "server.event" {
		t.Fatalf("expected live server.event, got %#v", frame)
	}
}

func TestRealtimeWSClientRequestCallsOwnerProductActions(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.SetReadMarkerPositionResolver(staticReadMarkerResolver{
		"$event": {
			roomID: "!room:example.com", topologicalPosition: 17, streamPosition: 23,
			originServerTS: 1710000000000,
		},
	})
	readMarkers := &recordingReadMarkerStore{Store: service.store}
	service.store = readMarkers
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.request",
		"id":     "req-contacts-1",
		"action": "contacts.list",
		"params": map[string]any{},
	})
	contacts := readRealtimeResponse(t, conn, "req-contacts-1")
	if contacts["type"] != "server.response" || contacts["ok"] != true || contacts["action"] != "contacts.list" {
		t.Fatalf("expected contacts.list response, got %#v", contacts)
	}

	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.request",
		"id":     "req-group-1",
		"action": "groups.create",
		"params": map[string]any{
			"name": "WS Group",
		},
	})
	groupResponse := readRealtimeResponse(t, conn, "req-group-1")
	if groupResponse["ok"] != true || groupResponse["action"] != "groups.create" {
		t.Fatalf("expected groups.create response, got %#v", groupResponse)
	}
	groupResult, ok := groupResponse["result"].(map[string]any)
	if !ok || groupResult["room_id"] == "" {
		t.Fatalf("expected created group result, got %#v", groupResponse)
	}

	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.request",
		"id":     "cmd-read-1",
		"action": "sync.read_marker",
		"params": map[string]any{
			"room_id":          "!room:example.com",
			"event_id":         "$event",
			"origin_server_ts": int64(1710000000000),
		},
	})
	frame := readRealtimeResponse(t, conn, "cmd-read-1")
	if frame["type"] != "server.response" || frame["id"] != "cmd-read-1" || frame["ok"] != true {
		t.Fatalf("expected response for read marker, got %#v", frame)
	}
	result, ok := frame["result"].(map[string]any)
	if !ok || result["status"] != "ok" {
		t.Fatalf("expected ok command result, got %#v", frame)
	}
	_, saved := readMarkers.snapshot()
	if len(saved) != 1 || saved[0] != (readMarker{
		RoomID: "!room:example.com", EventID: "$event", OriginServerTS: 1710000000000,
		TopologicalPosition: 17, StreamPosition: 23,
	}) {
		t.Fatalf("expected read marker to persist via WS command, got %#v", saved)
	}
}

func TestRealtimeWSClientRequestsDoNotHeadOfLineBlock(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	longEntered := make(chan struct{})
	releaseLong := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseLong) })
	service.actions["test.long"] = func(ctx context.Context, _ map[string]any) (any, *apiError) {
		close(longEntered)
		select {
		case <-releaseLong:
			return map[string]any{"status": "completed"}, nil
		case <-ctx.Done():
			return nil, internalError(ctx.Err())
		}
	}
	service.actions["test.fast"] = func(context.Context, map[string]any) (any, *apiError) {
		return map[string]any{"status": "completed"}, nil
	}

	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.request", "id": "req-long", "action": "test.long", "params": map[string]any{},
	})
	select {
	case <-longEntered:
	case <-time.After(time.Second):
		t.Fatal("long request did not reach the ProductCore handler")
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.request", "id": "req-long", "action": "test.long", "params": map[string]any{},
	})
	duplicate := readRealtimeFrame(t, conn)
	if duplicate["type"] != "server.error" || duplicate["code"] != "duplicate_request_id" {
		t.Fatalf("expected uncorrelated duplicate request protocol error, got %#v", duplicate)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.request", "id": "req-fast", "action": "test.fast", "params": map[string]any{},
	})
	if fast := readRealtimeResponse(t, conn, "req-fast"); fast["ok"] != true {
		t.Fatalf("fast request failed while long request was in flight: %#v", fast)
	}

	releaseOnce.Do(func() { close(releaseLong) })
	if long := readRealtimeResponse(t, conn, "req-long"); long["ok"] != true {
		t.Fatalf("long request failed after release: %#v", long)
	}
}

func TestRealtimeWSClientRequestBackpressureIsBounded(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	entered := make(chan struct{}, realtimewsmodule.MaxInFlightRequests)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	service.actions["test.block"] = func(ctx context.Context, _ map[string]any) (any, *apiError) {
		entered <- struct{}{}
		select {
		case <-release:
			return map[string]any{"status": "completed"}, nil
		case <-ctx.Done():
			return nil, internalError(ctx.Err())
		}
	}

	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	for i := 0; i < realtimewsmodule.MaxInFlightRequests; i++ {
		writeRealtimeFrame(t, conn, map[string]any{
			"type": "client.request", "id": fmt.Sprintf("req-block-%d", i), "action": "test.block", "params": map[string]any{},
		})
	}
	for i := 0; i < realtimewsmodule.MaxInFlightRequests; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatalf("blocking request %d did not start", i)
		}
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.request", "id": "req-over-limit", "action": "test.block", "params": map[string]any{},
	})
	overLimit := readRealtimeResponse(t, conn, "req-over-limit")
	if overLimit["ok"] != false || int(overLimit["status"].(float64)) != http.StatusTooManyRequests {
		t.Fatalf("expected bounded request backpressure, got %#v", overLimit)
	}
}

func TestRealtimeWSCloseCancelsInFlightClientRequest(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	service.actions["test.cancel"] = func(ctx context.Context, _ map[string]any) (any, *apiError) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return nil, internalError(ctx.Err())
	}

	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type": "client.request", "id": "req-cancel", "action": "test.cancel", "params": map[string]any{},
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach ProductCore handler")
	}
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close websocket: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("connection close did not cancel in-flight ProductCore request")
	}
}

func TestReadMarkerStoreFailureReturnsErrorWithoutCommit(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.SetReadMarkerPositionResolver(staticReadMarkerResolver{
		"$failed": {
			roomID: "!room:example.com", topologicalPosition: 1, streamPosition: 1,
		},
	})
	readMarkers := &recordingReadMarkerStore{Store: service.store, err: errors.New("save failed")}
	service.store = readMarkers

	result, apiErr := service.Handle(context.Background(), "sync.read_marker", map[string]any{
		"room_id": "!room:example.com", "event_id": "$failed",
	})
	if result != nil || apiErr == nil || apiErr.Status != http.StatusInternalServerError {
		t.Fatalf("expected failed Store write to return an error, result=%#v err=%#v", result, apiErr)
	}
	attempts, saved := readMarkers.snapshot()
	if attempts != 1 || len(saved) != 0 {
		t.Fatalf("failed Store write must not look committed, attempts=%d saved=%#v", attempts, saved)
	}
}

func TestRealtimeWSClientCommandIsRemoved(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.command",
		"id":     "cmd-read-alias",
		"action": "channels.read_marker",
		"params": map[string]any{
			"room_id":  "!room:example.com",
			"event_id": "$event",
		},
	})
	frame := readRealtimeResponse(t, conn, "cmd-read-alias")
	if frame["type"] != "server.response" ||
		frame["ok"] != false ||
		int(frame["status"].(float64)) != http.StatusBadRequest ||
		frame["error"] != "unsupported frame type" {
		t.Fatalf("expected client.command to be rejected, got %#v", frame)
	}
}

func TestRealtimeWSRequestCoverageMatchesActionRegistry(t *testing.T) {
	registered := NewService(Config{ServerName: "example.com"}).actionHandlers()
	service := &Service{
		actions:            map[string]actionHandler{},
		servicePortalState: servicePortalState{ownerMXID: "@owner:example.com"},
	}
	service.realtimeModule = realtimewsmodule.New(realtimewsmodule.Dependencies{
		Actions: serviceRealtimeActionPort{service: service},
	}, realtimewsmodule.Config{})
	for action := range registered {
		action := action
		service.actions[action] = func(context.Context, map[string]any) (any, *apiError) {
			return map[string]any{"action": action}, nil
		}
	}
	record := realtimeWSTicket{Role: "owner", UserID: "@owner:example.com"}
	for action := range registered {
		params := map[string]any{}
		switch action {
		case "channels.public.join_request":
			params = map[string]any{
				"room_id": "!channel:example.com",
				"user_id": record.UserID,
			}
		case "channels.public.join_result":
			params = map[string]any{
				"room_id": "!channel:example.com",
				"user_id": record.UserID,
				"status":  "rejected",
			}
		}
		frame := service.handleRealtimeWSRequest(context.Background(), record, map[string]any{
			"type":   "client.request",
			"id":     "req-" + strings.ReplaceAll(action, ".", "-"),
			"action": action,
			"params": params,
		})
		if message := realtimewsmodule.ClientRequestBlockedMessage(action); message != "" {
			if frame["ok"] != false ||
				int(frame["status"].(int)) != http.StatusBadRequest ||
				frame["error"] != message {
				t.Fatalf("expected %s to be blocked with %q, got %#v", action, message, frame)
			}
			continue
		}
		if frame["ok"] != true || frame["action"] != action {
			t.Fatalf("expected %s to be callable over WS, got %#v", action, frame)
		}
	}
}

func TestRealtimeWSClientRequestValidationErrors(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}

	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.request",
		"id":     "req-unknown",
		"action": "does.not.exist",
		"params": map[string]any{},
	})
	unknown := readRealtimeResponse(t, conn, "req-unknown")
	if unknown["ok"] != false || int(unknown["status"].(float64)) != http.StatusBadRequest || unknown["error"] != "unknown action" {
		t.Fatalf("expected unknown action response, got %#v", unknown)
	}

	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.request",
		"id":     "req-bad-params",
		"action": "contacts.list",
		"params": []any{},
	})
	badParams := readRealtimeResponse(t, conn, "req-bad-params")
	if badParams["ok"] != false || int(badParams["status"].(float64)) != http.StatusBadRequest {
		t.Fatalf("expected malformed params response, got %#v", badParams)
	}

	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.request",
		"action": "contacts.list",
		"params": map[string]any{},
	})
	missingID := readRealtimeFrame(t, conn)
	if missingID["type"] != "server.response" ||
		missingID["ok"] != false ||
		int(missingID["status"].(float64)) != http.StatusBadRequest ||
		missingID["error"] != "id is required" {
		t.Fatalf("expected missing id response, got %#v", missingID)
	}

	writeRealtimeFrame(t, conn, map[string]any{
		"type":   "client.request",
		"id":     "req-handler-error",
		"action": "sync.read_marker",
		"params": map[string]any{"room_id": "!room:example.com"},
	})
	handlerErr := readRealtimeResponse(t, conn, "req-handler-error")
	if handlerErr["ok"] != false || int(handlerErr["status"].(float64)) != http.StatusBadRequest {
		t.Fatalf("expected handler error response, got %#v", handlerErr)
	}
}

func TestRealtimeWSSendsCursorResetForExpiredSince(t *testing.T) {
	service := NewService(Config{
		ServerName:                    "example.com",
		P2PEventRetentionMaxRows:      2,
		P2PEventRetentionPruneOnWrite: true,
	})
	router := newP2PTestRouter(service)
	for seq := int64(1); seq <= 4; seq++ {
		if err := service.appendP2PEvent(context.Background(), p2pEvent{
			Seq:     seq,
			Type:    "test.event",
			RoomID:  "!room:example.com",
			EventID: "$event",
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")

	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello", "since": 1})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.ready" {
		t.Fatalf("expected ready, got %#v", got)
	}
	reset := readRealtimeFrame(t, conn)
	if reset["type"] != "server.cursor_reset" {
		t.Fatalf("expected cursor reset, got %#v", reset)
	}
	if reset["recovery"] != "bootstrap_required" || int64(reset["min_seq"].(float64)) != 3 {
		t.Fatalf("expected reset payload with retained bounds, got %#v", reset)
	}
}

func mustCreateRealtimeWSTicket(t *testing.T, router http.Handler, token string) string {
	t.Helper()
	got := mustCreateRealtimeWSTicketResponse(t, router, token)
	ticket, _ := got["ticket"].(string)
	return ticket
}

func mustCreateRealtimeWSTicketResponse(t *testing.T, router http.Handler, token string) map[string]any {
	t.Helper()
	req := jsonRequest(t, "/_p2p/command", map[string]any{
		"action": serviceapi.RealtimeWSTicketAction,
		"params": map[string]any{},
	})
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket create expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func dialRealtimeWS(t *testing.T, serverURL, ticket string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/_p2p/ws?ticket=" + ticket
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	return conn
}

func writeRealtimeFrame(t *testing.T, conn *websocket.Conn, frame map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func readRealtimeFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var frame map[string]any
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return frame
}

func readRealtimeResponse(t *testing.T, conn *websocket.Conn, id string) map[string]any {
	t.Helper()
	for {
		frame := readRealtimeFrame(t, conn)
		if frame["type"] == "server.response" && frame["id"] == id {
			return frame
		}
	}
}

func waitForRealtimePushSuppressed(t *testing.T, service *Service, roomID string) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if service.shouldSuppressPushForRoom(roomID) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("expected focused foreground WS session to suppress push for %s", roomID)
		case <-tick.C:
		}
	}
}

func waitForRealtimePushNotSuppressed(t *testing.T, service *Service, roomID string) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if !service.shouldSuppressPushForRoom(roomID) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("expected WS session state to keep normal push for %s", roomID)
		case <-tick.C:
		}
	}
}
