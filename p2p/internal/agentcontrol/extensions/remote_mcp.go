package extensions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	MCPProtocolVersion = "2025-06-18"
	MCPMaxBodyBytes    = 1 << 20
	MCPMaxSchemaBytes  = 64 << 10
	MCPMaxTools        = 256
)

var ErrMCPProtocol = errors.New("extension: mcp protocol")
var ErrMCPUnavailable = errors.New("extension: mcp unavailable")

type MCPClient struct {
	OwnerID, InstallationID, VersionID, BindingDigest string
	Endpoint                                          Endpoint
	Secret                                            SecretResolver
	HTTP                                              *http.Client
	Timeout                                           time.Duration
	id                                                atomic.Uint64
}
type mcpRPC struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func (c *MCPClient) validate() (*url.URL, error) {
	if c == nil || c.Secret == nil || strings.TrimSpace(c.OwnerID) == "" || !validUUID(c.InstallationID) || !validUUID(c.VersionID) || !validDigest(c.BindingDigest) {
		return nil, ErrInvalid
	}
	u, e := endpoint(c.Endpoint.URL)
	if e != nil {
		return nil, ErrInvalid
	}
	if !validUUID(c.Endpoint.CredentialReferenceID) {
		return nil, ErrInvalid
	}
	return u, nil
}
func (c *MCPClient) client() *http.Client {
	var out http.Client
	if c.HTTP != nil {
		out = *c.HTTP // Never mutate an injected client shared by another caller.
	} else {
		out.Timeout = c.timeout()
	}
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := out.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if base, ok := transport.(*http.Transport); ok {
		clone := base.Clone()
		// A proxy can resolve/connect to an otherwise forbidden endpoint after we
		// have validated it, so remote MCP never uses ambient proxy settings.
		clone.Proxy = nil
		dial := clone.DialContext
		if dial == nil {
			var d net.Dialer
			dial = d.DialContext
		}
		clone.DialContext = publicDialContext(dial)
		// DialTLSContext bypasses DialContext.  Clear it so every TLS connection
		// is resolved and pinned to a public address immediately before dialing.
		clone.DialTLSContext = nil
		out.Transport = clone
	} else {
		// Test transports may be deliberately in-memory.  They still get the
		// endpoint validation below, while production transports must expose a
		// DialContext that can be guarded against DNS rebinding.
		out.Transport = publicRoundTripper{next: transport}
	}
	return &out
}
func (c *MCPClient) timeout() time.Duration {
	if c.Timeout > 0 && c.Timeout <= time.Minute {
		return c.Timeout
	}
	return 15 * time.Second
}
func (c *MCPClient) request(ctx context.Context, u *url.URL, session, version string, method string, params any, notify bool) (json.RawMessage, string, error) {
	if err := validatePublicHost(ctx, u.Hostname()); err != nil {
		return nil, "", ErrMCPUnavailable
	}
	id := c.id.Add(1)
	body, e := json.Marshal(mcpRPC{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if e != nil {
		return nil, "", ErrMCPProtocol
	}
	if notify {
		body = bytes.Replace(body, []byte(fmt.Sprintf(",\"id\":%d", id)), nil, 1)
	}
	secret, e := c.Secret.Resolve(ctx, c.OwnerID, c.VersionID, c.Endpoint.CredentialReferenceID, "mcp_credential", c.BindingDigest)
	if e != nil {
		return nil, "", ErrMCPUnavailable
	}
	defer clear(secret)
	reqctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, e := http.NewRequestWithContext(reqctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if e != nil {
		return nil, "", ErrMCPUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if len(secret) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(secret))
	}
	defer req.Header.Del("Authorization")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, e := c.client().Do(req)
	if e != nil {
		return nil, "", ErrMCPUnavailable
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, MCPMaxBodyBytes+1))
	if e != nil || len(raw) > MCPMaxBodyBytes {
		return nil, "", ErrMCPProtocol
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", ErrMCPUnavailable
	}
	responseSessionID := resp.Header.Get("Mcp-Session-Id")
	if responseSessionID != "" && !validMCPSessionID(responseSessionID) {
		return nil, "", ErrMCPProtocol
	}
	if notify {
		return nil, responseSessionID, nil
	}
	contentType := resp.Header.Get("Content-Type")
	mediaType, _, parseErr := mime.ParseMediaType(contentType)
	if parseErr != nil {
		return nil, "", ErrMCPProtocol
	}
	result, parseErr := parseMCPRPCResponse(raw, strings.ToLower(mediaType), id)
	if parseErr != nil {
		return nil, "", ErrMCPProtocol
	}
	return result, responseSessionID, nil
}
func (c *MCPClient) Initialize(ctx context.Context) (string, error) {
	sid, _, err := c.initialize(ctx)
	return sid, err
}

func (c *MCPClient) initialize(ctx context.Context) (string, string, error) {
	u, e := c.validate()
	if e != nil {
		return "", "", e
	}
	result, sid, e := c.request(ctx, u, "", "", "initialize", map[string]any{"protocolVersion": MCPProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "dirextalk-message-server", "version": "1"}}, false)
	if e != nil {
		return "", "", e
	}
	var init struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
	}
	if json.Unmarshal(result, &init) != nil || !supportedMCPProtocolVersion(init.ProtocolVersion) || init.Capabilities["tools"] == nil {
		return "", "", ErrMCPProtocol
	}
	if _, _, e = c.request(ctx, u, sid, init.ProtocolVersion, "notifications/initialized", map[string]any{}, true); e != nil {
		return "", "", e
	}
	return sid, init.ProtocolVersion, nil
}
func (c *MCPClient) ListTools(ctx context.Context) ([]Tool, error) {
	sid, version, e := c.initialize(ctx)
	if e != nil {
		return nil, e
	}
	u, _ := c.validate()
	result, e := c.list(ctx, u, sid, version, "")
	return result, e
}
func (c *MCPClient) list(ctx context.Context, u *url.URL, sid, version, cursor string) ([]Tool, error) {
	if version == "" {
		version = MCPProtocolVersion
	}
	out := []Tool{}
	seen := map[string]bool{}
	seenCursors := map[string]bool{}
	for page := 0; page < 32; page++ {
		p := map[string]any{}
		if cursor != "" {
			p["cursor"] = cursor
		}
		raw, _, e := c.request(ctx, u, sid, version, "tools/list", p, false)
		if e != nil {
			return nil, e
		}
		var v struct {
			Tools []struct {
				Name, Description string
				InputSchema       json.RawMessage `json:"inputSchema"`
			}
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Tools == nil {
			return nil, ErrMCPProtocol
		}
		for _, x := range v.Tools {
			schema, e := canonicalSchema(x.InputSchema)
			if e != nil || x.Name == "" || len(x.Name) > 48 || strings.ContainsAny(x.Name, " \t\r\n") || len(schema) > MCPMaxSchemaBytes || seen[x.Name] {
				return nil, ErrMCPProtocol
			}
			seen[x.Name] = true
			out = append(out, Tool{Name: x.Name, Description: strings.TrimSpace(x.Description), InputSchema: schema, InputSchemaDigest: DigestBytes(schema)})
		}
		if len(out) > MCPMaxTools {
			return nil, ErrMCPProtocol
		}
		cursor = strings.TrimSpace(v.NextCursor)
		if cursor == "" {
			return out, nil
		}
		if len(cursor) > 512 || seenCursors[cursor] {
			return nil, ErrMCPProtocol
		}
		seenCursors[cursor] = true
	}
	return nil, ErrMCPProtocol
}

func supportedMCPProtocolVersion(version string) bool {
	switch strings.TrimSpace(version) {
	case "2025-03-26", MCPProtocolVersion:
		return true
	default:
		return false
	}
}

func validMCPSessionID(sessionID string) bool {
	return sessionID == strings.TrimSpace(sessionID) &&
		len(sessionID) <= 512 &&
		!strings.ContainsAny(sessionID, "\r\n\x00")
}

func parseMCPRPCResponse(body []byte, mediaType string, requestID uint64) (json.RawMessage, error) {
	var (
		messages [][]byte
		err      error
	)
	switch mediaType {
	case "application/json":
		messages = [][]byte{body}
	case "text/event-stream":
		messages, err = parseMCPSSEMessages(body)
		if err != nil {
			return nil, err
		}
	default:
		return nil, ErrMCPProtocol
	}
	expectedID := strconv.FormatUint(requestID, 10)
	for _, message := range messages {
		var response mcpResponse
		if json.Unmarshal(message, &response) != nil || response.JSONRPC != "2.0" {
			continue
		}
		if string(bytes.TrimSpace(response.ID)) != expectedID {
			continue
		}
		if len(response.Error) > 0 && string(bytes.TrimSpace(response.Error)) != "null" {
			return nil, ErrMCPUnavailable
		}
		if len(response.Result) == 0 {
			return nil, ErrMCPProtocol
		}
		return append(json.RawMessage(nil), response.Result...), nil
	}
	return nil, ErrMCPProtocol
}

func parseMCPSSEMessages(body []byte) ([][]byte, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), MCPMaxBodyBytes)
	var (
		messages  [][]byte
		dataLines []string
	)
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		messages = append(messages, []byte(strings.Join(dataLines, "\n")))
		dataLines = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if scanner.Err() != nil {
		return nil, ErrMCPProtocol
	}
	flush()
	if len(messages) == 0 {
		return nil, ErrMCPProtocol
	}
	return messages, nil
}

// publicRoundTripper protects injected in-memory test transports. Production
// networking uses publicDialContext below, which resolves again per connection.
type publicRoundTripper struct{ next http.RoundTripper }

func (r publicRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || validatePublicHost(req.Context(), req.URL.Hostname()) != nil {
		return nil, ErrMCPUnavailable
	}
	return r.next.RoundTrip(req)
}

func publicDialContext(next func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrMCPUnavailable
		}
		addresses, err := publicAddresses(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, ErrMCPUnavailable
		}
		// Dial an already-validated address, rather than the hostname, so the
		// resolver cannot return a different (private) answer between check/dial.
		return next(ctx, network, net.JoinHostPort(addresses[0].String(), port))
	}
}

func validatePublicHost(ctx context.Context, host string) error {
	_, err := publicAddresses(ctx, host)
	return err
}

func publicAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, ErrMCPUnavailable
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddress(ip) {
			return nil, ErrMCPUnavailable
		}
		return []netip.Addr{ip}, nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, ErrMCPUnavailable
	}
	for _, ip := range addresses {
		if !isPublicAddress(ip) {
			return nil, ErrMCPUnavailable
		}
	}
	return addresses, nil
}

func isPublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, blocked := range nonPublicPrefixes {
		if blocked.Contains(ip) {
			return false
		}
	}
	return true
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2001:10::/28"),
}

func canonicalSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > MCPMaxSchemaBytes {
		return nil, ErrMCPProtocol
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, ErrMCPProtocol
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, ErrMCPProtocol
	}
	canonical, err := json.Marshal(value)
	if err != nil || len(canonical) > MCPMaxSchemaBytes {
		return nil, ErrMCPProtocol
	}
	return canonical, nil
}
func (c *MCPClient) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if strings.TrimSpace(name) == "" || len(args) > MCPMaxSchemaBytes {
		return nil, ErrInvalid
	}
	sid, version, e := c.initialize(ctx)
	if e != nil {
		return nil, e
	}
	u, _ := c.validate()
	raw, _, e := c.request(ctx, u, sid, version, "tools/call", map[string]any{"name": name, "arguments": json.RawMessage(args)}, false)
	if e != nil {
		return nil, e
	}
	return raw, nil
}
