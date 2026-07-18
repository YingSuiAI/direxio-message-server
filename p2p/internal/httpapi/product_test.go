package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rootinternal "github.com/YingSuiAI/dirextalk-message-server/internal"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
)

type productContextKey struct{}

func TestWriteErrorKeepsCodeAndAddsRecoveryAliases(t *testing.T) {
	recorder := httptest.NewRecorder()
	apiErr := actionbase.CodedError(http.StatusGone, actionbase.RequestExpiredCode, "request expired")
	apiErr.OperationID = "op_test"
	apiErr.CurrentRoomID = "!current:example.com"

	WriteError(recorder, apiErr)
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusGone || body["code"] != actionbase.RequestExpiredCode ||
		body["error_code"] != actionbase.RequestExpiredCode || body["operation_id"] != "op_test" ||
		body["current_room_id"] != "!current:example.com" {
		t.Fatalf("recovery error envelope changed: status=%d body=%#v", recorder.Code, body)
	}
}

type productPortStub struct {
	actions        map[string]bool
	authorized     bool
	authorizeCalls int
	authorizedWith string
	handledAction  string
	handledParams  map[string]any
	handleResult   any
	handleErr      *actionbase.Error
	ticketToken    string
	ticketResult   any
	ticketErr      *actionbase.Error
}

func (p *productPortStub) HasAction(action string) bool { return p.actions[action] }

func (p *productPortStub) Authorize(ctx context.Context, token, action string) (context.Context, bool) {
	p.authorizeCalls++
	p.authorizedWith = token + ":" + action
	if !p.authorized {
		return ctx, false
	}
	return context.WithValue(ctx, productContextKey{}, "authorized"), true
}

func (p *productPortStub) Handle(ctx context.Context, action string, params map[string]any) (any, *actionbase.Error) {
	if p.authorized && ctx.Value(productContextKey{}) != "authorized" {
		return nil, actionbase.StatusError(http.StatusInternalServerError, "authorized context was not forwarded")
	}
	p.handledAction = action
	p.handledParams = params
	return p.handleResult, p.handleErr
}

func (p *productPortStub) CreateWSTicket(token string) (any, *actionbase.Error) {
	p.ticketToken = token
	return p.ticketResult, p.ticketErr
}

func TestProductHandlerPreservesDecodeAuthAndResponseContract(t *testing.T) {
	response := map[string]any{"homeserver": "https://auto", "user_id": "@owner:example.com"}
	port := &productPortStub{
		actions:      map[string]bool{"profile.get": true},
		authorized:   true,
		handleResult: response,
	}
	req := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`{
		"action":" profile.get ",
		"params":{"sequence":9007199254740993}
	}`))
	req.Host = "127.0.0.1:18008"
	req.Header.Set("Authorization", "Bearer owner-token")
	req.Header.Set("Origin", "https://portal.example")
	req.Header.Set("X-Forwarded-Proto", "https, http")
	req.Header.Set("X-Forwarded-Host", "portal.example, internal.example")
	rec := httptest.NewRecorder()

	ProductHandler(port).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if port.authorizedWith != "owner-token:profile.get" || port.handledAction != "profile.get" {
		t.Fatalf("unexpected auth/dispatch: authorized=%q handled=%q", port.authorizedWith, port.handledAction)
	}
	if _, ok := port.handledParams["sequence"].(json.Number); !ok {
		t.Fatalf("sequence type = %T, want json.Number", port.handledParams["sequence"])
	}
	got := decodeObject(t, rec)
	if got["homeserver"] != "https://portal.example" {
		t.Fatalf("homeserver = %#v, want forwarded request base URL", got["homeserver"])
	}
	if response["homeserver"] != "https://auto" {
		t.Fatalf("handler response was mutated: %#v", response)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://portal.example" {
		t.Fatalf("CORS origin = %q", got)
	}
}

func TestProductHandlerPublicAndWSTicketDispatch(t *testing.T) {
	publicPort := &productPortStub{
		actions:      map[string]bool{"portal.status": true},
		handleResult: map[string]any{"initialized": false},
	}
	publicRec := serveProduct(t, publicPort, `{"action":"portal.status"}`, "")
	if publicRec.Code != http.StatusOK || publicPort.authorizeCalls != 0 || publicPort.handledParams == nil {
		t.Fatalf("public dispatch changed: status=%d auth_calls=%d params=%#v", publicRec.Code, publicPort.authorizeCalls, publicPort.handledParams)
	}

	ticketPort := &productPortStub{
		actions:      map[string]bool{},
		authorized:   true,
		ticketResult: map[string]any{"ticket": "one-use"},
	}
	ticketRec := serveProduct(t, ticketPort, `{"action":"`+serviceapi.RealtimeWSTicketAction+`"}`, "owner-token")
	if ticketRec.Code != http.StatusOK || ticketPort.ticketToken != "owner-token" || ticketPort.handledAction != "" {
		t.Fatalf("ticket dispatch changed: status=%d token=%q handled=%q", ticketRec.Code, ticketPort.ticketToken, ticketPort.handledAction)
	}
}

func TestAgentRuntimeProfileActionsRequireOwnerHTTPAndNoStore(t *testing.T) {
	for _, action := range []string{serviceapi.AgentRuntimeProfileGetAction, serviceapi.AgentRuntimeProfileUpdateAction} {
		t.Run(action, func(t *testing.T) {
			port := &productPortStub{actions: map[string]bool{action: true}, handleResult: map[string]any{"available": true}}
			unauthorized := serveProduct(t, port, `{"action":"`+action+`"}`, "")
			if unauthorized.Code != http.StatusUnauthorized || unauthorized.Header().Get("Cache-Control") != "no-store" || unauthorized.Header().Get("Pragma") != "no-cache" {
				t.Fatalf("unauthorized contract changed: status=%d headers=%v", unauthorized.Code, unauthorized.Header())
			}
			port.authorized = true
			succeeded := serveProduct(t, port, `{"action":"`+action+`"}`, "owner-token")
			if succeeded.Code != http.StatusOK || port.handledAction != action || succeeded.Header().Get("Cache-Control") != "no-store" || succeeded.Header().Get("Pragma") != "no-cache" {
				t.Fatalf("successful contract changed: status=%d handled=%q headers=%v", succeeded.Code, port.handledAction, succeeded.Header())
			}
		})
	}
}
func TestProductHandlerRejectsInvalidRequestsInContractOrder(t *testing.T) {
	tooLarge := `{"action":"profile.get","params":{"value":"` + strings.Repeat("x", 1024*1024) + `"}}`
	tests := []struct {
		name       string
		body       string
		port       *productPortStub
		wantStatus int
		wantError  string
		wantAuth   int
	}{
		{"invalid JSON", `{`, &productPortStub{}, http.StatusBadRequest, "invalid json", 0},
		{"body too large", tooLarge, &productPortStub{}, http.StatusBadRequest, "invalid json", 0},
		{"missing action", `{}`, &productPortStub{}, http.StatusBadRequest, "action is required", 0},
		{"unknown spec", `{"action":"retired.action"}`, &productPortStub{}, http.StatusBadRequest, "unknown action", 0},
		{"missing handler", `{"action":"profile.get"}`, &productPortStub{actions: map[string]bool{}}, http.StatusBadRequest, "unknown action", 0},
		{"websocket only", `{"action":"agent.chat.stream"}`, &productPortStub{actions: map[string]bool{"agent.chat.stream": true}}, http.StatusBadRequest, "action requires websocket", 0},
		{"unauthorized", `{"action":"profile.get"}`, &productPortStub{actions: map[string]bool{"profile.get": true}}, http.StatusUnauthorized, "M_UNKNOWN_TOKEN", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveProduct(t, tt.port, tt.body, "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := decodeObject(t, rec)["error"]; got != tt.wantError {
				t.Fatalf("error = %#v, want %q", got, tt.wantError)
			}
			if tt.port.authorizeCalls != tt.wantAuth {
				t.Fatalf("authorize calls = %d, want %d", tt.port.authorizeCalls, tt.wantAuth)
			}
		})
	}
}

func TestCommonHandlersPreserveCORSAndPublicPayloads(t *testing.T) {
	options := httptest.NewRequest(http.MethodOptions, "/query", nil)
	options.Header.Set("Origin", "https://portal.example")
	optionsRec := httptest.NewRecorder()
	ProductHandler(nil).ServeHTTP(optionsRec, options)
	if optionsRec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want 204", optionsRec.Code)
	}
	for key, want := range map[string]string{
		"Access-Control-Allow-Origin":          "https://portal.example",
		"Access-Control-Allow-Credentials":     "true",
		"Access-Control-Allow-Methods":         "GET, POST, PUT, DELETE, OPTIONS",
		"Access-Control-Allow-Headers":         "Origin, X-Requested-With, Content-Type, Accept, Authorization, Last-Event-ID",
		"Access-Control-Allow-Private-Network": "true",
		"Vary":                                 "Origin",
	} {
		if got := optionsRec.Header().Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}

	build := func() rootinternal.BuildInfo {
		return rootinternal.BuildInfo{Version: "v-test", Commit: "abc", BuildTime: "now", SchemaVersion: 7, SchemaCompatVersion: 6}
	}
	healthRec := httptest.NewRecorder()
	HealthHandler(build).ServeHTTP(healthRec, httptest.NewRequest(http.MethodGet, "/health", nil))
	health := decodeObject(t, healthRec)
	if healthRec.Code != http.StatusOK || health["status"] != "ok" || health["version"] != "v-test" || health["schema_version"] != float64(7) {
		t.Fatalf("health response changed: status=%d body=%#v", healthRec.Code, health)
	}

	wellKnownRec := httptest.NewRecorder()
	WellKnownHandler(func() any { return map[string]any{"matrix_user_id": "@owner:example.com"} }).ServeHTTP(
		wellKnownRec,
		httptest.NewRequest(http.MethodGet, "/owner.json", nil),
	)
	if got := decodeObject(t, wellKnownRec)["matrix_user_id"]; got != "@owner:example.com" {
		t.Fatalf("well-known matrix_user_id = %#v", got)
	}
}

func TestAutoHomeserverRecognition(t *testing.T) {
	for _, value := range []string{"auto", " AUTO ", "http://auto", "https://AUTO:8448/path"} {
		if !IsAutoHomeserver(value) {
			t.Fatalf("IsAutoHomeserver(%q) = false", value)
		}
	}
	for _, value := range []string{"", "automatic", "https://example.com"} {
		if IsAutoHomeserver(value) {
			t.Fatalf("IsAutoHomeserver(%q) = true", value)
		}
	}
}

func serveProduct(t *testing.T, port ProductPort, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	ProductHandler(port).ServeHTTP(rec, req)
	return rec
}

func decodeObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return value
}
