package p2p

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/httpapi"
	"github.com/gorilla/mux"
)

const agentChatSSEHeartbeat = 15 * time.Second

func agentChatTurnCreateHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpapi.SetCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ctx, ok := authorizeAgentChatHTTP(service, r)
		if !ok {
			httpapi.WriteError(w, statusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN"))
			return
		}
		conversationID := strings.TrimSpace(mux.Vars(r)["conversation_id"])
		var params map[string]any
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
		decoder.UseNumber()
		if err := decoder.Decode(&params); err != nil || params == nil {
			httpapi.WriteError(w, badRequest("invalid json"))
			return
		}
		if bodyConversation, present := params["conversation_id"]; present && bodyConversation != conversationID {
			httpapi.WriteError(w, badRequest("conversation_id is path-bound"))
			return
		}
		params["conversation_id"] = conversationID
		if err := agentgateway.ValidateActionRequest("agent.chat.stream", params); err != nil {
			httpapi.WriteError(w, badRequest(err.Error()))
			return
		}
		receipt, apiErr := service.agentModule.StartTurn(ctx, service.OwnerMXID(), params)
		if apiErr != nil {
			httpapi.WriteError(w, apiErr)
			return
		}
		location := agentChatTurnLocation(conversationID, receipt.TurnID)
		w.Header().Set("Location", location)
		httpapi.WriteJSON(w, http.StatusAccepted, receipt)
	}
}

func agentChatTurnGetHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpapi.SetCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ctx, ok := authorizeAgentChatHTTP(service, r)
		if !ok {
			httpapi.WriteError(w, statusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN"))
			return
		}
		turn, apiErr := service.agentModule.GetTurn(ctx, strings.TrimSpace(mux.Vars(r)["conversation_id"]), strings.TrimSpace(mux.Vars(r)["turn_id"]))
		if apiErr != nil {
			httpapi.WriteError(w, apiErr)
			return
		}
		httpapi.WriteJSON(w, http.StatusOK, turn)
	}
}

func agentChatTurnEventsHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpapi.SetCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ctx, ok := authorizeAgentChatHTTP(service, r)
		if !ok {
			httpapi.WriteError(w, statusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN"))
			return
		}
		if accept := r.Header.Get("Accept"); accept != "" && !strings.Contains(strings.ToLower(accept), "text/event-stream") {
			httpapi.WriteError(w, statusError(http.StatusNotAcceptable, "Accept must include text/event-stream"))
			return
		}
		afterSeq, cursorErr := agentChatAfterSeq(r)
		if cursorErr != nil {
			httpapi.WriteError(w, badRequest(cursorErr.Error()))
			return
		}
		conversationID := strings.TrimSpace(mux.Vars(r)["conversation_id"])
		turnID := strings.TrimSpace(mux.Vars(r)["turn_id"])
		turn, apiErr := service.agentModule.GetTurn(ctx, conversationID, turnID)
		if apiErr != nil {
			httpapi.WriteError(w, apiErr)
			return
		}
		operationID := strings.TrimSpace(actionbase.String(turn["idempotency_key"]))
		if operationID == "" {
			httpapi.WriteError(w, statusError(http.StatusBadGateway, "native agent turn identity is invalid"))
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			httpapi.WriteError(w, statusError(http.StatusInternalServerError, "streaming is unavailable"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		events := make(chan agentstream.StreamEvent, 1)
		go func() {
			defer close(events)
			_ = service.agentModule.WatchTurn(ctx, service.OwnerMXID(), conversationID, turnID, operationID, afterSeq, func(event agentstream.StreamEvent) error {
				select {
				case events <- event:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		heartbeat := time.NewTicker(agentChatSSEHeartbeat)
		defer heartbeat.Stop()
		for {
			select {
			case event, open := <-events:
				if !open {
					return
				}
				terminal, err := writeAgentChatSSE(w, event)
				if err != nil {
					return
				}
				flusher.Flush()
				if terminal {
					return
				}
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-ctx.Done():
				return
			}
		}
	}
}

func authorizeAgentChatHTTP(service *Service, r *http.Request) (context.Context, bool) {
	if service == nil || service.agentModule == nil {
		return r.Context(), false
	}
	identity, ok := service.authorizeProductAction(httpapi.BearerToken(r.Header.Get("Authorization")), "agent.chat")
	if !ok || identity.DeviceID == "" || identity.Generation == 0 {
		return r.Context(), false
	}
	return withPortalActionSession(r.Context(), identity), true
}

func agentChatTurnLocation(conversationID, turnID string) string {
	return "/_p2p/agent/chat/conversations/" + url.PathEscape(conversationID) + "/turns/" + url.PathEscape(turnID)
}

func agentChatAfterSeq(r *http.Request) (int64, error) {
	queryValues, queryPresent := r.URL.Query()["after_seq"]
	query := ""
	if queryPresent {
		if len(queryValues) != 1 {
			return 0, fmt.Errorf("after_seq must appear once")
		}
		query = strings.TrimSpace(queryValues[0])
	}
	header, headerPresent := r.Header["Last-Event-Id"]
	headerValue := ""
	if headerPresent {
		if len(header) != 1 {
			return 0, fmt.Errorf("Last-Event-ID must appear once")
		}
		headerValue = strings.TrimSpace(header[0])
	}
	parse := func(value, name string) (int64, error) {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return 0, fmt.Errorf("%s must be a non-negative integer", name)
		}
		return parsed, nil
	}
	if !queryPresent && !headerPresent {
		return 0, nil
	}
	var querySeq, headerSeq int64
	var err error
	if queryPresent {
		if querySeq, err = parse(query, "after_seq"); err != nil {
			return 0, err
		}
	}
	if headerPresent {
		if headerSeq, err = parse(headerValue, "Last-Event-ID"); err != nil {
			return 0, err
		}
	}
	if queryPresent && headerPresent && querySeq != headerSeq {
		return 0, fmt.Errorf("after_seq and Last-Event-ID must agree")
	}
	if headerPresent {
		return headerSeq, nil
	}
	return querySeq, nil
}

func writeAgentChatSSE(w http.ResponseWriter, event agentstream.StreamEvent) (bool, error) {
	if event.Seq <= 0 {
		return false, fmt.Errorf("durable event sequence is invalid")
	}
	name, terminal := agentChatSSEEventName(event.Event)
	payload := map[string]any{
		"action": "agent.chat", "event": name, "data": event.Data,
		"turn_id": event.TurnID, "idempotency_key": event.IdempotencyKey,
		"revision": event.Revision, "conversation_id": event.ConversationID, "seq": event.Seq,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, name, encoded)
	return terminal, err
}

func agentChatSSEEventName(event string) (string, bool) {
	name := strings.TrimSpace(event)
	if name == "" {
		name = "delta"
	}
	return name, name == "done" || name == "error" || name == "cancelled"
}
