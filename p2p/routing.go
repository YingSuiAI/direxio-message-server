package p2p

import (
	"context"
	"net/http"

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
	router.HandleFunc("/agent/chat/conversations/{conversation_id}/turns", agentChatTurnCreateHandler(service)).Methods(http.MethodPost, http.MethodOptions)
	router.HandleFunc("/agent/chat/conversations/{conversation_id}/turns/{turn_id}", agentChatTurnGetHandler(service)).Methods(http.MethodGet, http.MethodOptions)
	router.HandleFunc("/agent/chat/conversations/{conversation_id}/turns/{turn_id}/events", agentChatTurnEventsHandler(service)).Methods(http.MethodGet, http.MethodOptions)
	router.HandleFunc("/ws", realtimeWSHandler(service)).Methods(http.MethodGet, http.MethodOptions)
	router.HandleFunc("/agent/voice/volc/custom-llm", voiceCustomLLMHandler(service)).Methods(http.MethodPost, http.MethodOptions)
	router.HandleFunc("/agent/voice/webhook", voiceEventWebhookHandler(service)).Methods(http.MethodPost, http.MethodOptions)
	// The process health endpoint gates deployment and component updates. Agent
	// capability readiness remains fail-closed on Agent actions, but must not
	// make an otherwise healthy Message Server impossible to upgrade before the
	// independently versioned Agent image.
	router.HandleFunc("/health", httpapi.HealthHandler(nil)).Methods(http.MethodGet, http.MethodOptions)
}

func voiceEventWebhookHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if service != nil && service.externalNativeAgent {
			if service.voiceCallbackRelay == nil || service.voiceCallbackRelayInitErr != nil {
				http.Error(w, "voice unavailable", http.StatusServiceUnavailable)
				return
			}
			service.voiceCallbackRelay.handle(w, r, "/agent/voice/webhook")
			return
		}
		http.Error(w, "voice unavailable", http.StatusServiceUnavailable)
	}
}

func voiceCustomLLMHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if service != nil && service.externalNativeAgent {
			if service.voiceCallbackRelay == nil || service.voiceCallbackRelayInitErr != nil {
				http.Error(w, "voice unavailable", http.StatusServiceUnavailable)
				return
			}
			service.voiceCallbackRelay.handle(w, r, "/agent/voice/volc/custom-llm")
			return
		}
		http.Error(w, "voice unavailable", http.StatusServiceUnavailable)
	}
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
