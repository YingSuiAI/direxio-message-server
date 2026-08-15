package p2p

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
)

const (
	testHTTPConversationID = "018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c12"
	testHTTPOperationID    = "018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c13"
	testHTTPTurnID         = "018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c14"
)

type httpChatRunner struct {
	mu            sync.Mutex
	starts        int
	watches       int
	afterSeqs     []int64
	listPageToken []string
}

func (r *httpChatRunner) Apply(context.Context, string) error { return nil }
func (r *httpChatRunner) ProbeCatalog(context.Context, []agentgateway.CatalogRequirement) error {
	return nil
}
func (r *httpChatRunner) Invoke(_ context.Context, action string, params map[string]any) (map[string]any, error) {
	if action != "agent.chat.turns.list" {
		return map[string]any{}, nil
	}
	page, _ := params["page_token"].(string)
	r.mu.Lock()
	r.listPageToken = append(r.listPageToken, page)
	r.mu.Unlock()
	if page == "" {
		return map[string]any{"turns": []any{}, "next_cursor": "second-page"}, nil
	}
	return map[string]any{"turns": []any{map[string]any{
		"turn_id": testHTTPTurnID, "idempotency_key": testHTTPOperationID,
		"conversation_id": testHTTPConversationID, "state": "running",
		"revision": int64(2), "last_sequence": int64(2),
		"terminal_code": "", "terminal_summary": "",
		"created_at": "2026-08-14T00:00:00Z", "updated_at": "2026-08-14T00:00:01Z",
	}}, "next_cursor": ""}, nil
}
func (r *httpChatRunner) Stream(_ context.Context, action string, params map[string]any, emit func(agentstream.Event) error) error {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	return emit(httpChatEvent("accepted", 1, map[string]any{"kind": "accepted"}))
}
func (r *httpChatRunner) WatchDurableChat(_ context.Context, operationID, conversationID string, afterSeq int64, emit func(agentstream.Event) error) error {
	if operationID != testHTTPOperationID || conversationID != testHTTPConversationID {
		return context.Canceled
	}
	r.mu.Lock()
	r.watches++
	r.afterSeqs = append(r.afterSeqs, afterSeq)
	r.mu.Unlock()
	for _, event := range []agentstream.Event{
		httpChatEvent("accepted", 1, map[string]any{"kind": "accepted"}),
		httpChatEvent("waiting_confirmation", 2, map[string]any{"kind": "waiting_confirmation"}),
		httpChatEvent("done", 3, map[string]any{"kind": "done", "text": "hello"}),
	} {
		if event.Seq > afterSeq {
			if err := emit(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func httpChatEvent(name string, seq int64, extra map[string]any) agentstream.Event {
	data := map[string]any{
		"idempotency_key": testHTTPOperationID, "conversation_id": testHTTPConversationID,
		"turn_id": testHTTPTurnID, "revision": int64(2),
	}
	for key, value := range extra {
		data[key] = value
	}
	return agentstream.Event{Event: name, Seq: seq, Data: data}
}

func TestAgentChatHTTPPostAndSSEReconnectNeverRepeatMutation(t *testing.T) {
	runner := &httpChatRunner{}
	service := NewService(Config{ServerName: "example.test", AccountGeneration: 7, NativeAgentRunner: runner})
	router := newP2PTestRouter(service)
	path := "/_p2p/agent/chat/conversations/" + testHTTPConversationID + "/turns"
	request := jsonRequest(t, path, map[string]any{
		"idempotency_key": testHTTPOperationID, "message": "hello",
		"model_profile_id": "profile", "model_profile_revision": 1, "credential_version": 1,
	})
	request.Header.Set("Authorization", "Bearer "+service.AccessToken())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Location"); !strings.HasSuffix(got, "/turns/"+testHTTPTurnID) {
		t.Fatalf("Location=%q", got)
	}
	var receipt map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt["turn_id"] != testHTTPTurnID || receipt["seq"] != float64(1) {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}

	eventsPath := response.Header().Get("Location") + "/events"
	for index, cursor := range []int64{0, 2} {
		request := httptest.NewRequest(http.MethodGet, eventsPath+"?after_seq="+strconv.FormatInt(cursor, 10), nil)
		request.Header.Set("Authorization", "Bearer "+service.AccessToken())
		request.Header.Set("Accept", "text/event-stream")
		request.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("SSE[%d] status=%d headers=%v body=%s", index, response.Code, response.Header(), response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "event: done") || !strings.Contains(response.Body.String(), "id: 3") {
			t.Fatalf("SSE[%d] body=%q", index, response.Body.String())
		}
		if cursor == 0 && !strings.Contains(response.Body.String(), "event: waiting_confirmation") {
			t.Fatalf("SSE lost durable event name: %q", response.Body.String())
		}
		if cursor == 2 && (strings.Contains(response.Body.String(), "id: 1") || strings.Contains(response.Body.String(), "id: 2")) {
			t.Fatalf("resume replayed old events: %q", response.Body.String())
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.starts != 1 || runner.watches != 2 {
		t.Fatalf("starts=%d watches=%d", runner.starts, runner.watches)
	}
	if got := runner.afterSeqs; len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("after sequences=%v", got)
	}
	if len(runner.listPageToken) != 4 || runner.listPageToken[0] != "" || runner.listPageToken[1] != "second-page" {
		t.Fatalf("turn lookup pages=%v", runner.listPageToken)
	}
}

func TestAgentChatSSEPreservesCancelledTerminal(t *testing.T) {
	response := httptest.NewRecorder()
	event := agentstream.StreamEvent{
		TurnID: testHTTPTurnID, IdempotencyKey: testHTTPOperationID,
		ConversationID: testHTTPConversationID, Revision: 3, Seq: 9,
		Event: "cancelled", Data: map[string]any{"kind": "cancelled"},
	}
	terminal, err := writeAgentChatSSE(response, event)
	if err != nil || !terminal || !strings.Contains(response.Body.String(), "event: cancelled") || strings.Contains(response.Body.String(), "event: failed") {
		t.Fatalf("cancelled terminal=%v err=%v body=%q", terminal, err, response.Body.String())
	}
}

func TestAgentChatSSEPreservesWorkerPhase(t *testing.T) {
	response := httptest.NewRecorder()
	event := agentstream.StreamEvent{
		TurnID: testHTTPTurnID, IdempotencyKey: testHTTPOperationID,
		ConversationID: testHTTPConversationID, Revision: 3, Seq: 9,
		Event: "worker_status", Data: map[string]any{
			"kind": "worker_status", "status": "running", "phase": "executing_remote_task",
		},
	}
	terminal, err := writeAgentChatSSE(response, event)
	if err != nil || terminal || !strings.Contains(response.Body.String(), `"phase":"executing_remote_task"`) {
		t.Fatalf("worker phase terminal=%v err=%v body=%q", terminal, err, response.Body.String())
	}
}

func TestAgentChatSSERejectsConflictingResumeCursors(t *testing.T) {
	runner := &httpChatRunner{}
	service := NewService(Config{ServerName: "example.test", AccountGeneration: 7, NativeAgentRunner: runner})
	router := newP2PTestRouter(service)
	path := "/_p2p/agent/chat/conversations/" + testHTTPConversationID + "/turns/" + testHTTPTurnID + "/events?after_seq=1"
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+service.AccessToken())
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Last-Event-ID", "2")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.starts != 0 || runner.watches != 0 || len(runner.listPageToken) != 0 {
		t.Fatalf("invalid cursor reached Agent: %#v", runner)
	}
}

func TestAgentChatHTTPRejectsAgentTokenBeforeTurnLookup(t *testing.T) {
	runner := &httpChatRunner{}
	service := NewService(Config{ServerName: "example.test", AccountGeneration: 7, NativeAgentRunner: runner})
	router := newP2PTestRouter(service)
	paths := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/_p2p/agent/chat/conversations/" + testHTTPConversationID + "/turns", `{}`},
		{http.MethodGet, "/_p2p/agent/chat/conversations/" + testHTTPConversationID + "/turns/" + testHTTPTurnID, ""},
		{http.MethodGet, "/_p2p/agent/chat/conversations/" + testHTTPConversationID + "/turns/" + testHTTPTurnID + "/events?after_seq=invalid", ""},
	}
	for _, test := range paths {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer "+service.AgentToken())
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status=%d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.starts != 0 || runner.watches != 0 || len(runner.listPageToken) != 0 {
		t.Fatalf("agent token reached Agent: %#v", runner)
	}
}
