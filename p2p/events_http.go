package p2p

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/httpapi"
)

const (
	productEventBatchLimit = 100
	productSSEHeartbeat    = 15 * time.Second
)

func productEventsHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpapi.SetCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ctx, ok := authorizeOwnerHTTP(service, r)
		if !ok {
			httpapi.WriteError(w, statusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN"))
			return
		}
		if service.eventsModule == nil {
			httpapi.WriteError(w, statusError(http.StatusServiceUnavailable, "event stream is unavailable"))
			return
		}
		if accept := r.Header.Get("Accept"); accept != "" && !strings.Contains(strings.ToLower(accept), "text/event-stream") {
			httpapi.WriteError(w, statusError(http.StatusNotAcceptable, "Accept must include text/event-stream"))
			return
		}
		afterSeq, err := agentChatAfterSeq(r)
		if err != nil {
			httpapi.WriteError(w, badRequest(err.Error()))
			return
		}
		status, err := service.eventsModule.CursorStatus(ctx, afterSeq)
		if err != nil {
			httpapi.WriteError(w, internalError(err))
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
		if status.Expired {
			_ = writeProductSSE(w, "", "cursor_reset", map[string]any{
				"since": status.Since, "min_seq": status.Bounds.MinSeq,
				"max_seq": status.Bounds.MaxSeq, "count": status.Bounds.Count,
				"recovery": "bootstrap_required",
			})
			flusher.Flush()
			return
		}
		flusher.Flush()
		heartbeat := time.NewTicker(productSSEHeartbeat)
		defer heartbeat.Stop()
		for {
			events, err := service.eventsModule.List(ctx, afterSeq, productEventBatchLimit)
			if err != nil {
				return
			}
			if len(events) > 0 {
				for _, event := range events {
					if err := writeProductEventSSE(w, event); err != nil {
						return
					}
					afterSeq = event.Seq
				}
				flusher.Flush()
				continue
			}
			waiter := service.eventsModule.Waiter()
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-waiter:
			}
		}
	}
}

func authorizeOwnerHTTP(service *Service, r *http.Request) (context.Context, bool) {
	if service == nil {
		return r.Context(), false
	}
	identity, ok := service.authorizeProductAction(httpapi.BearerToken(r.Header.Get("Authorization")), "sync.bootstrap")
	if !ok || identity.DeviceID == "" || identity.Generation == 0 {
		return r.Context(), false
	}
	return withPortalActionSession(r.Context(), identity), true
}

func writeProductEventSSE(w http.ResponseWriter, event dirextalkdomain.Event) error {
	if event.Seq <= 0 || strings.TrimSpace(event.Type) == "" {
		return fmt.Errorf("product event is invalid")
	}
	return writeProductSSE(w, fmt.Sprintf("%d", event.Seq), event.Type, event)
}

func writeProductSSE(w http.ResponseWriter, id, event string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
	return err
}
