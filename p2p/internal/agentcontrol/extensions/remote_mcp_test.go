package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type testSecretResolver struct{}

func (testSecretResolver) Resolve(context.Context, string, string, string, string, string) ([]byte, error) {
	return []byte("test-token"), nil
}

func testMCPClient(rt http.RoundTripper) *MCPClient {
	return &MCPClient{
		OwnerID: "owner", InstallationID: "00000000-0000-0000-0000-000000000001", VersionID: "00000000-0000-0000-0000-000000000002",
		BindingDigest: strings.Repeat("a", 64), Endpoint: Endpoint{URL: "https://8.8.8.8/mcp", CredentialReferenceID: "00000000-0000-0000-0000-000000000003"},
		Secret: testSecretResolver{}, HTTP: &http.Client{Transport: rt},
	}
}

func jsonResponse(status int, body string, contentType string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func requestID(t *testing.T, req *http.Request) (uint64, string) {
	t.Helper()
	var rpc mcpRPC
	if err := json.NewDecoder(req.Body).Decode(&rpc); err != nil {
		t.Fatal(err)
	}
	return rpc.ID, rpc.Method
}

func rpcResult(id uint64, result string) string {
	return `{"jsonrpc":"2.0","id":` + jsonNumber(id) + `,"result":` + result + `}`
}

func jsonNumber(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestMCPClientRejectsPrivateEndpointBeforeInjectedTransport(t *testing.T) {
	called := false
	c := testMCPClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not dial")
	}))
	c.Endpoint.URL = "https://127.0.0.1/mcp"
	if _, err := c.Initialize(context.Background()); !errors.Is(err, ErrMCPUnavailable) {
		t.Fatalf("Initialize error = %v, want unavailable", err)
	}
	if called {
		t.Fatal("private endpoint reached injected transport")
	}
}

func TestMCPClientRejectsRedirectAndAcceptsStreamableHTTPResponse(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		c := testMCPClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://8.8.4.4/mcp"}}, Body: io.NopCloser(strings.NewReader("redirect"))}, nil
		}))
		if _, err := c.Initialize(context.Background()); !errors.Is(err, ErrMCPUnavailable) {
			t.Fatalf("Initialize error = %v, want unavailable", err)
		}
	})
	t.Run("streamable http sse", func(t *testing.T) {
		c := testMCPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			id, method := requestID(t, req)
			if got := req.Header.Get("Accept"); got != "application/json, text/event-stream" {
				t.Fatalf("Accept = %q", got)
			}
			switch method {
			case "initialize":
				body := "event: message\ndata: " + rpcResult(id, `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}`) + "\n\n"
				return jsonResponse(http.StatusOK, body, "text/event-stream"), nil
			case "notifications/initialized":
				return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			default:
				t.Fatalf("unexpected method %q", method)
				return nil, nil
			}
		}))
		if _, err := c.Initialize(context.Background()); err != nil {
			t.Fatalf("Initialize error = %v", err)
		}
	})
	t.Run("malformed sse", func(t *testing.T) {
		c := testMCPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			_, _ = requestID(t, req)
			return jsonResponse(http.StatusOK, "event: message\nnot-data: {}\n\n", "text/event-stream"), nil
		}))
		if _, err := c.Initialize(context.Background()); !errors.Is(err, ErrMCPProtocol) {
			t.Fatalf("Initialize error = %v, want protocol", err)
		}
	})
}

func TestMCPClientCanonicalizesToolSchemaDigest(t *testing.T) {
	c := testMCPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		id, method := requestID(t, req)
		switch method {
		case "initialize":
			return jsonResponse(http.StatusOK, rpcResult(id, `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}`), "application/json; charset=utf-8"), nil
		case "notifications/initialized":
			return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		case "tools/list":
			return jsonResponse(http.StatusOK, rpcResult(id, `{"tools":[{"name":"echo","inputSchema":{"required":["x"],"type":"object","properties":{"x":{"type":"string"}}}}]}`), "application/json"), nil
		default:
			t.Fatalf("unexpected MCP method %q", method)
			return nil, nil
		}
	}))
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}
	want := `{"properties":{"x":{"type":"string"}},"required":["x"],"type":"object"}`
	if string(tools[0].InputSchema) != want || tools[0].InputSchemaDigest != DigestBytes([]byte(want)) {
		t.Fatalf("tool schema = %s digest = %s", tools[0].InputSchema, tools[0].InputSchemaDigest)
	}
}

func TestMCPClientUsesNegotiatedVersionAndRejectsInvalidSessionID(t *testing.T) {
	t.Run("negotiated version", func(t *testing.T) {
		const negotiated = "2025-03-26"
		c := testMCPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			id, method := requestID(t, req)
			switch method {
			case "initialize":
				if got := req.Header.Get("MCP-Protocol-Version"); got != "" {
					t.Fatalf("initialize protocol header = %q", got)
				}
				response := jsonResponse(http.StatusOK, rpcResult(id, `{"protocolVersion":"`+negotiated+`","capabilities":{"tools":{}}}`), "application/json")
				response.Header.Set("Mcp-Session-Id", "session-1")
				return response, nil
			case "notifications/initialized":
				if got := req.Header.Get("MCP-Protocol-Version"); got != negotiated {
					t.Fatalf("notification protocol header = %q", got)
				}
				return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			case "tools/list":
				if got := req.Header.Get("MCP-Protocol-Version"); got != negotiated {
					t.Fatalf("tools/list protocol header = %q", got)
				}
				return jsonResponse(http.StatusOK, rpcResult(id, `{"tools":[]}`), "application/json"), nil
			default:
				t.Fatalf("unexpected method %q", method)
				return nil, nil
			}
		}))
		if _, err := c.ListTools(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("invalid session", func(t *testing.T) {
		c := testMCPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			id, _ := requestID(t, req)
			response := jsonResponse(http.StatusOK, rpcResult(id, `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}`), "application/json")
			response.Header["Mcp-Session-Id"] = []string{"bad\x00session"}
			return response, nil
		}))
		if _, err := c.Initialize(context.Background()); !errors.Is(err, ErrMCPProtocol) {
			t.Fatalf("Initialize error = %v, want protocol", err)
		}
	})
}

func TestPublicDialContextRejectsPrivateAndPinsValidatedAddress(t *testing.T) {
	var got string
	dial := publicDialContext(func(_ context.Context, _ string, address string) (net.Conn, error) {
		got = address
		return nil, errors.New("stop after observing address")
	})
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:443"); !errors.Is(err, ErrMCPUnavailable) {
		t.Fatalf("private dial error = %v", err)
	}
	if got != "" {
		t.Fatalf("private address reached dialer: %q", got)
	}
	if _, err := dial(context.Background(), "tcp", "8.8.8.8:443"); err == nil || got != "8.8.8.8:443" {
		t.Fatalf("public dial got %q err %v", got, err)
	}
}
