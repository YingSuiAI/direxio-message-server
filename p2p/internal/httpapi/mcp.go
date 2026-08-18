package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

const (
	MCPProtocolVersion = "2025-06-18"

	JSONRPCParseError     = -32700
	JSONRPCInvalidRequest = -32600
	JSONRPCMethodNotFound = -32601
	JSONRPCInvalidParams  = -32602
)

// MCPPort is the narrow capability required by the Streamable HTTP adapter.
type MCPPort interface {
	TokenAuthorized(token string) bool
	Tools() []dirextalkmcp.Tool
	Invoke(ctx context.Context, action string, params map[string]any) (any, *dirextalkmcp.Error)
}

// MCPConfig configures the MCP Streamable HTTP adapter.
type MCPConfig struct {
	Port      MCPPort
	BuildInfo BuildInfoProvider
}

type mcpJSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params,omitempty"`
}

type mcpJSONRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *mcpJSONRPCError `json:"error,omitempty"`
}

type mcpJSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPHandler implements the existing MCP Streamable HTTP request/response
// subset. Server-to-client GET streaming remains intentionally disabled.
func MCPHandler(cfg MCPConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		SetCORSHeaders(w, r)
		if err := rejectMCPQueryTokens(r); err != nil {
			WriteError(w, err)
			return
		}
		if err := validateMCPOrigin(r); err != nil {
			WriteError(w, err)
			return
		}

		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodGet:
			WriteError(w, actionbase.StatusError(http.StatusMethodNotAllowed, "MCP server-to-client streaming is not enabled"))
			return
		case http.MethodPost:
		default:
			WriteError(w, actionbase.StatusError(http.StatusMethodNotAllowed, "method not allowed"))
			return
		}

		token := BearerToken(r.Header.Get("Authorization"))
		if cfg.Port == nil || !cfg.Port.TokenAuthorized(token) {
			WriteError(w, actionbase.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN"))
			return
		}

		var req mcpJSONRPCRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			writeMCPJSONRPCError(w, http.StatusBadRequest, nil, JSONRPCParseError, "parse error")
			return
		}
		if strings.TrimSpace(req.JSONRPC) != "2.0" || strings.TrimSpace(req.Method) == "" {
			writeMCPJSONRPCError(w, http.StatusOK, req.ID, JSONRPCInvalidRequest, "invalid request")
			return
		}
		if req.Params == nil {
			req.Params = map[string]any{}
		}

		switch strings.TrimSpace(req.Method) {
		case "initialize":
			writeMCPJSONRPCResult(w, req.ID, MCPInitializeResult(cfg.BuildInfo))
		case "tools/list":
			writeMCPJSONRPCResult(w, req.ID, mcpToolsListResult(cfg.Port))
		case "tools/call":
			result, rpcErr := mcpToolsCallResult(r.Context(), cfg.Port, req.Params)
			if rpcErr != nil {
				writeMCPJSONRPCError(w, http.StatusOK, req.ID, rpcErr.Code, rpcErr.Message)
				return
			}
			writeMCPJSONRPCResult(w, req.ID, result)
		default:
			writeMCPJSONRPCError(w, http.StatusOK, req.ID, JSONRPCMethodNotFound, "method not found")
		}
	}
}

func rejectMCPQueryTokens(r *http.Request) *actionbase.Error {
	for key := range r.URL.Query() {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "access_token", "agent_token", "token", "authorization", "auth_token":
			return actionbase.StatusError(http.StatusBadRequest, "access tokens are not accepted in query string")
		}
	}
	return nil
}

func validateMCPOrigin(r *http.Request) *actionbase.Error {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Scheme == "" || originURL.Host == "" {
		return actionbase.StatusError(http.StatusForbidden, "origin is not allowed")
	}
	requestURL, err := url.Parse(RequestBaseURL(r))
	if err != nil || requestURL.Scheme == "" || requestURL.Host == "" {
		return actionbase.StatusError(http.StatusForbidden, "origin is not allowed")
	}
	if strings.EqualFold(originURL.Scheme, requestURL.Scheme) && strings.EqualFold(originURL.Host, requestURL.Host) {
		return nil
	}
	return actionbase.StatusError(http.StatusForbidden, "origin is not allowed")
}

// MCPInitializeResult returns the canonical MCP server capabilities payload.
func MCPInitializeResult(buildInfo BuildInfoProvider) map[string]any {
	build := currentBuildInfo(buildInfo)
	return map[string]any{
		"protocolVersion": MCPProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "dirextalk-message-server",
			"version": build.Version,
		},
	}
}

func mcpToolsListResult(port MCPPort) map[string]any {
	available := port.Tools()
	tools := make([]map[string]any, 0, len(available))
	for _, tool := range available {
		tools = append(tools, map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": tool.InputSchema,
			"annotations": tool.Annotations(),
		})
	}
	return map[string]any{"tools": tools}
}

func mcpToolsCallResult(ctx context.Context, port MCPPort, params map[string]any) (map[string]any, *mcpJSONRPCError) {
	name, ok := params["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return nil, &mcpJSONRPCError{Code: JSONRPCInvalidParams, Message: "tool name is required"}
	}
	action, ok := dirextalkmcp.NativeToolAction(name)
	if !ok {
		return nil, &mcpJSONRPCError{Code: JSONRPCInvalidParams, Message: "unknown tool"}
	}
	arguments := map[string]any{}
	if rawArguments, exists := params["arguments"]; exists && rawArguments != nil {
		rawMap, ok := rawArguments.(map[string]any)
		if !ok {
			return nil, &mcpJSONRPCError{Code: JSONRPCInvalidParams, Message: "tool arguments must be an object"}
		}
		arguments = cloneAnyMap(rawMap)
	}
	value, mcpErr := port.Invoke(ctx, action, arguments)
	if mcpErr != nil {
		return map[string]any{
			"isError": true,
			"content": []map[string]string{
				{"type": "text", "text": mcpErr.Message},
			},
		}, nil
	}
	return map[string]any{
		"isError":           false,
		"structuredContent": value,
		"content": []map[string]string{
			{"type": "text", "text": mcpResultText(value)},
		},
	}, nil
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func mcpResultText(value any) string {
	data, err := json.Marshal(value)
	if err != nil || len(data) == 0 {
		return "null"
	}
	return string(data)
}

func writeMCPJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeMCPJSONRPCResponse(w, http.StatusOK, mcpJSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeMCPJSONRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	writeMCPJSONRPCResponse(w, status, mcpJSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpJSONRPCError{Code: code, Message: message},
	})
}

func writeMCPJSONRPCResponse(w http.ResponseWriter, status int, response mcpJSONRPCResponse) {
	WriteJSON(w, status, response)
}
