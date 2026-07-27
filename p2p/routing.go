package p2p

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	httpapi "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/httpapi"
	"github.com/gorilla/mux"
)

const PathPrefix = "/_p2p/"

// envelope remains the shared product HTTP request shape used by outbound
// inter-node adapters and package integration tests.
type envelope struct {
	Action string         `json:"action"`
	Params map[string]any `json:"params"`
}

func Register(router *mux.Router, service *Service) {
	product := httpapi.ProductHandler(serviceHTTPProductPort{service: service})
	router.HandleFunc("/query", product).Methods(http.MethodPost, http.MethodOptions)
	router.HandleFunc("/command", product).Methods(http.MethodPost, http.MethodOptions)
	router.HandleFunc("/ws", realtimeWSHandler(service)).Methods(http.MethodGet, http.MethodOptions)
	router.HandleFunc("/agent/voice/volc/custom-llm", voiceCustomLLMHandler(service)).Methods(http.MethodPost, http.MethodOptions)
	router.HandleFunc("/agent/voice/webhook", voiceCustomLLMHandler(service)).Methods(http.MethodPost, http.MethodOptions)
	router.HandleFunc("/health", httpapi.HealthHandler(nil)).Methods(http.MethodGet, http.MethodOptions)
}

func voiceCustomLLMHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if service == nil || service.agentModule == nil {
			http.Error(w, "voice unavailable", http.StatusServiceUnavailable)
			return
		}
		sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
		token := strings.TrimSpace(r.Header.Get("Authorization"))
		token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
		if token == "" {
			token = strings.TrimSpace(r.Header.Get("X-Voice-Callback-Token"))
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "invalid callback", http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			http.Error(w, "invalid callback", http.StatusBadRequest)
			return
		}
		transcript := voiceCallbackTranscript(payload)
		if transcript == "" {
			http.Error(w, "transcript required", http.StatusBadRequest)
			return
		}
		if err := service.agentModule.AuthorizeVoiceCallback(sessionID, token); err != nil {
			http.Error(w, "voice callback rejected", http.StatusUnauthorized)
			return
		}
		if err := service.agentModule.ValidateVoiceProviderPayload(sessionID, payload); err != nil {
			http.Error(w, "voice callback rejected", http.StatusUnauthorized)
			return
		}
		requestID := voiceCallbackRequestID(payload)
		if requestID == "" {
			http.Error(w, "provider request id required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		writeChunk := func(text string) error {
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}})
			_, err := w.Write([]byte("data: " + string(chunk) + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return err
		}
		runErr := service.agentModule.RunVoiceCustomLLMStream(r.Context(), sessionID, token, transcript, writeChunk, requestID)
		if runErr != nil {
			_, _ = w.Write([]byte("data: {\"error\":\"voice callback rejected\"}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func voiceCallbackRequestID(payload map[string]any) string {
	for _, key := range []string{"request_id", "event_id", "RequestId", "EventId", "task_id"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func voiceCallbackTranscript(payload map[string]any) string {
	for _, key := range []string{"transcript_final", "text", "input", "prompt"} {
		if text, ok := payload[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	if messages, ok := payload["messages"].([]any); ok && len(messages) > 0 {
		if item, ok := messages[len(messages)-1].(map[string]any); ok {
			if text, ok := item["content"].(string); ok {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func RegisterMCP(router *mux.Router, service *Service) {
	handler := httpapi.MCPHandler(httpapi.MCPConfig{Port: serviceHTTPMCPPort{service: service}})
	router.HandleFunc("/mcp", handler).Methods(http.MethodPost, http.MethodGet, http.MethodOptions)
}

func RegisterWellKnown(router *mux.Router, service *Service) {
	handler := httpapi.WellKnownHandler(func() any { return service.profileModule.WellKnown() })
	router.HandleFunc("/owner.json", handler).Methods(http.MethodGet, http.MethodOptions)
}

type serviceHTTPProductPort struct{ service *Service }

func (p serviceHTTPProductPort) HasAction(action string) bool {
	if p.service == nil {
		return false
	}
	_, ok := p.service.actions[action]
	return ok
}

func (p serviceHTTPProductPort) Authorize(ctx context.Context, token, action string) (context.Context, bool) {
	if p.service == nil {
		return ctx, false
	}
	identity, authorized := p.service.authorizeProductAction(token, action)
	if !authorized {
		return ctx, false
	}
	if identity.Generation != 0 {
		ctx = withPortalActionSession(ctx, identity)
	}
	return ctx, true
}

func (p serviceHTTPProductPort) Handle(ctx context.Context, action string, params map[string]any) (any, *actionbase.Error) {
	return p.service.Handle(ctx, action, params)
}

func (p serviceHTTPProductPort) CreateWSTicket(token string) (any, *actionbase.Error) {
	return p.service.createRealtimeWSTicketForToken(token)
}

type serviceHTTPMCPPort struct{ service *Service }

func (p serviceHTTPMCPPort) TokenAuthorized(token string) bool {
	return p.service != nil && token != "" && token == p.service.AgentToken()
}

func (p serviceHTTPMCPPort) Tools() []dirextalkmcp.Tool {
	if p.service == nil {
		return nil
	}
	return p.service.dirextalkMCPService().Tools()
}

func (p serviceHTTPMCPPort) Invoke(ctx context.Context, action string, params map[string]any) (any, *dirextalkmcp.Error) {
	return p.service.dirextalkMCPService().Invoke(ctx, action, params)
}

// These compatibility helpers remain for root adapters and package tests.
func badRequest(message string) *apiError {
	return actionbase.BadRequest(message)
}

func internalError(err error) *apiError {
	return actionbase.InternalError(err)
}

func statusError(status int, message string) *apiError {
	return actionbase.StatusError(status, message)
}

func codedError(status int, code, message string) *apiError {
	return actionbase.CodedError(status, code, message)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	httpapi.WriteJSON(w, status, value)
}
