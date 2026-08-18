package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rootinternal "github.com/YingSuiAI/dirextalk-message-server/internal"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
)

type mcpPortStub struct {
	allowedToken string
	tools        []dirextalkmcp.Tool
	result       any
	invokeErr    *dirextalkmcp.Error
	action       string
	params       map[string]any
}

func (p *mcpPortStub) TokenAuthorized(token string) bool {
	return token != "" && token == p.allowedToken
}
func (p *mcpPortStub) Tools() []dirextalkmcp.Tool { return p.tools }
func (p *mcpPortStub) Invoke(_ context.Context, action string, params map[string]any) (any, *dirextalkmcp.Error) {
	p.action = action
	p.params = params
	return p.result, p.invokeErr
}

func TestMCPHandlerInitializeAndToolsList(t *testing.T) {
	port := &mcpPortStub{
		allowedToken: "agent-token",
		tools: []dirextalkmcp.Tool{
			{
				Action:      dirextalkmcp.ActionMessagesList,
				Name:        "dirextalk_messages_list",
				Description: "List messages.",
				InputSchema: map[string]any{"type": "object"},
				Effect:      dirextalkmcp.ToolEffectRead,
			},
			{
				Action:      dirextalkmcp.ActionMessagesSend,
				Name:        "dirextalk_messages_send",
				Description: "Send a message.",
				InputSchema: map[string]any{"type": "object"},
				Effect:      dirextalkmcp.ToolEffectNonIdempotentWrite,
			},
		},
	}
	handler := MCPHandler(MCPConfig{
		Port: port,
		BuildInfo: func() rootinternal.BuildInfo {
			return rootinternal.BuildInfo{Version: "v-test"}
		},
	})

	initialize := serveMCP(t, handler, http.MethodPost, "/mcp", "http://example.com", "agent-token", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	result := requireJSONRPCResult(t, initialize)
	if initialize.Code != http.StatusOK || result["protocolVersion"] != MCPProtocolVersion {
		t.Fatalf("initialize changed: status=%d result=%#v", initialize.Code, result)
	}
	if sessionID := initialize.Header().Get("Mcp-Session-Id"); sessionID != "" {
		t.Fatalf("stateless MCP endpoint issued session ID %q", sessionID)
	}
	serverInfo := result["serverInfo"].(map[string]any)
	if serverInfo["name"] != "dirextalk-message-server" || serverInfo["version"] != "v-test" {
		t.Fatalf("serverInfo = %#v", serverInfo)
	}

	list := serveMCP(t, handler, http.MethodPost, "/mcp", "", "agent-token", `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`)
	tools := requireJSONRPCResult(t, list)["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "dirextalk_messages_list" || tool["description"] != "List messages." || tool["inputSchema"] == nil {
		t.Fatalf("tools/list = %#v", tools)
	}
	if _, leaked := tool["action"]; leaked {
		t.Fatalf("internal tool action leaked: %#v", tool)
	}
	annotations := tool["annotations"].(map[string]any)
	for key, want := range map[string]bool{
		"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": true,
	} {
		if annotations[key] != want {
			t.Fatalf("annotation %s = %#v, want %t; tool=%#v", key, annotations[key], want, tool)
		}
	}
	writeTool := tools[1].(map[string]any)
	writeAnnotations := writeTool["annotations"].(map[string]any)
	for key, want := range map[string]bool{
		"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": true,
	} {
		if writeAnnotations[key] != want {
			t.Fatalf("write annotation %s = %#v, want %t; tool=%#v", key, writeAnnotations[key], want, writeTool)
		}
	}
}

func TestMCPHandlerToolCallPreservesUseNumberAndResultShapes(t *testing.T) {
	port := &mcpPortStub{
		allowedToken: "agent-token",
		result:       map[string]any{"messages": []any{}, "cursor": "next"},
	}
	handler := MCPHandler(MCPConfig{Port: port})
	rec := serveMCP(t, handler, http.MethodPost, "/mcp", "", "agent-token", `{
		"jsonrpc":"2.0","id":"call","method":"tools/call",
		"params":{"name":"dirextalk_messages_list","arguments":{"room_id":"!room:example.com","limit":9007199254740993}}
	}`)
	result := requireJSONRPCResult(t, rec)
	if port.action != dirextalkmcp.ActionMessagesList || port.params["room_id"] != "!room:example.com" {
		t.Fatalf("invoke = action %q params %#v", port.action, port.params)
	}
	if _, ok := port.params["limit"].(json.Number); !ok {
		t.Fatalf("limit type = %T, want json.Number", port.params["limit"])
	}
	if result["isError"] != false || result["structuredContent"] == nil {
		t.Fatalf("successful call result = %#v", result)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || !strings.Contains(content["text"].(string), `"cursor":"next"`) {
		t.Fatalf("text content = %#v", content)
	}

	port.invokeErr = dirextalkmcp.StatusError(http.StatusForbidden, "room is blocked")
	errorRec := serveMCP(t, handler, http.MethodPost, "/mcp", "", "agent-token", `{
		"jsonrpc":"2.0","id":2,"method":"tools/call",
		"params":{"name":"dirextalk_messages_list","arguments":{}}
	}`)
	errorResult := requireJSONRPCResult(t, errorRec)
	if errorResult["isError"] != true || errorResult["structuredContent"] != nil {
		t.Fatalf("tool error result = %#v", errorResult)
	}
	errorContent := errorResult["content"].([]any)[0].(map[string]any)
	if errorContent["text"] != "room is blocked" {
		t.Fatalf("tool error content = %#v", errorContent)
	}
}

func TestMCPHandlerValidationAndErrorShapes(t *testing.T) {
	tooLarge := `{"jsonrpc":"2.0","id":1,"method":"initialize","padding":"` + strings.Repeat("x", 1024*1024) + `"}`
	tests := []struct {
		name       string
		method     string
		target     string
		origin     string
		token      string
		body       string
		wantStatus int
		wantError  string
		wantRPC    int
	}{
		{"query token", http.MethodPost, "/mcp?Access_Token=secret", "", "agent-token", mcpInitializeBody(), http.StatusBadRequest, "access tokens are not accepted in query string", 0},
		{"query token precedes OPTIONS", http.MethodOptions, "/mcp?token=secret", "", "", "", http.StatusBadRequest, "access tokens are not accepted in query string", 0},
		{"bad origin", http.MethodPost, "/mcp", "https://evil.example", "agent-token", mcpInitializeBody(), http.StatusForbidden, "origin is not allowed", 0},
		{"GET disabled", http.MethodGet, "/mcp", "", "", "", http.StatusMethodNotAllowed, "MCP server-to-client streaming is not enabled", 0},
		{"unauthorized", http.MethodPost, "/mcp", "", "owner-token", mcpInitializeBody(), http.StatusUnauthorized, "M_UNKNOWN_TOKEN", 0},
		{"parse error", http.MethodPost, "/mcp", "", "agent-token", `{`, http.StatusBadRequest, "", JSONRPCParseError},
		{"body too large", http.MethodPost, "/mcp", "", "agent-token", tooLarge, http.StatusBadRequest, "", JSONRPCParseError},
		{"invalid request", http.MethodPost, "/mcp", "", "agent-token", `{"jsonrpc":"1.0","id":3,"method":"initialize"}`, http.StatusOK, "", JSONRPCInvalidRequest},
		{"unknown method", http.MethodPost, "/mcp", "", "agent-token", `{"jsonrpc":"2.0","id":4,"method":"unknown"}`, http.StatusOK, "", JSONRPCMethodNotFound},
		{"missing tool name", http.MethodPost, "/mcp", "", "agent-token", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`, http.StatusOK, "", JSONRPCInvalidParams},
		{"non-object arguments", http.MethodPost, "/mcp", "", "agent-token", `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"dirextalk_messages_list","arguments":"bad"}}`, http.StatusOK, "", JSONRPCInvalidParams},
		{"unknown tool", http.MethodPost, "/mcp", "", "agent-token", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"unknown"}}`, http.StatusOK, "", JSONRPCInvalidParams},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := &mcpPortStub{allowedToken: "agent-token"}
			rec := serveMCP(t, MCPHandler(MCPConfig{Port: port}), tt.method, tt.target, tt.origin, tt.token, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			body := decodeObject(t, rec)
			if tt.wantRPC != 0 {
				rpcError := body["error"].(map[string]any)
				if rpcError["code"] != float64(tt.wantRPC) {
					t.Fatalf("JSON-RPC error = %#v, want code %d", rpcError, tt.wantRPC)
				}
				return
			}
			if body["error"] != tt.wantError {
				t.Fatalf("error = %#v, want %q", body["error"], tt.wantError)
			}
		})
	}
}

func TestMCPHandlerOptionsAndForwardedSameOrigin(t *testing.T) {
	handler := MCPHandler(MCPConfig{Port: &mcpPortStub{allowedToken: "agent-token"}})
	options := serveMCP(t, handler, http.MethodOptions, "/mcp", "http://example.com", "", "")
	if options.Code != http.StatusNoContent || options.Header().Get("Access-Control-Allow-Origin") != "http://example.com" {
		t.Fatalf("OPTIONS changed: status=%d headers=%#v", options.Code, options.Header())
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(mcpInitializeBody()))
	req.Host = "127.0.0.1:18008"
	req.Header.Set("Authorization", "Bearer agent-token")
	req.Header.Set("Origin", "https://portal.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "portal.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("forwarded same-origin status = %d; body=%s", rec.Code, rec.Body.String())
	}
}

func serveMCP(t *testing.T, handler http.Handler, method, target, origin, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "example.com"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func requireJSONRPCResult(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	response := decodeObject(t, rec)
	if response["jsonrpc"] != "2.0" || response["error"] != nil {
		t.Fatalf("unexpected JSON-RPC response: %#v", response)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("JSON-RPC result = %#v", response["result"])
	}
	return result
}

func mcpInitializeBody() string {
	return `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
}
