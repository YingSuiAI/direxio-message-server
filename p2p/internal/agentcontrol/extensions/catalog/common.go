// Package catalog contains the bounded, remote-only MCP source catalog.
//
// The package deliberately has no execution or installation dependency.  It
// only turns pinned source metadata into the extensions boundary types.
package catalog

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
	"github.com/google/uuid"
)

const (
	SourceOfficialRegistry    = "official_registry"
	SourceSmithery            = "smithery"
	SourceGlama               = "glama"
	SourceGitHub              = "github"
	OfficialRegistryAuthority = "https://registry.modelcontextprotocol.io"
	SmitheryAuthority         = "https://api.smithery.ai"
	GlamaAuthority            = "https://glama.ai"
	GitHubAuthority           = "https://api.github.com"
	DefaultTimeout            = 15 * time.Second
	DefaultMaxBodyBytes       = 8 << 20
	MaxPageSize               = 100
)

var (
	ErrMalformed    = errors.New("catalog: source response malformed")
	ErrOversize     = errors.New("catalog: source response exceeds limit")
	ErrUnauthorized = errors.New("catalog: source authorization failed")
	ErrRedirect     = errors.New("catalog: source redirect rejected")
	ErrUnsupported  = errors.New("catalog: source artifact unsupported")
)

// Config is intentionally small.  Production authorities are fixed by New;
// custom URLs, clients, resolvers and dialers are accepted only by NewForTest.
type Config struct {
	Authorities  map[string]string
	Client       *http.Client
	Timeout      time.Duration
	MaxBodyBytes int64
	BearerToken  string
	Resolver     Resolver
	Dialer       Dialer
	SigningKey   []byte
}

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}
type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type Catalog struct {
	providers map[string]*provider
	key       []byte
}
type provider struct {
	source string
	c      *client
}
type client struct {
	base     *url.URL
	http     *http.Client
	timeout  time.Duration
	max      int64
	token    string
	resolver Resolver
	testOnly bool
}

func New(cfg Config) (*Catalog, error) {
	if len(cfg.Authorities) != 0 || cfg.Client != nil || cfg.Resolver != nil || cfg.Dialer != nil || len(cfg.SigningKey) != 0 {
		return nil, errors.New("catalog: custom source configuration is test-only")
	}
	return newCatalog(cfg, false)
}

// NewForTest enables httptest servers and injectable network controls.  It is
// kept separate so production cannot accidentally replace source authorities.
func NewForTest(cfg Config) (*Catalog, error) { return newCatalog(cfg, true) }

func newCatalog(cfg Config, testOnly bool) (*Catalog, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	key := append([]byte(nil), cfg.SigningKey...)
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
	}
	if len(key) < 32 {
		return nil, errors.New("catalog: signing key too short")
	}
	authorities := map[string]string{
		"official_registry": OfficialRegistryAuthority,
		"smithery":          SmitheryAuthority,
		"glama":             GlamaAuthority,
		"github":            GitHubAuthority,
	}
	if testOnly {
		for source, authority := range cfg.Authorities {
			if _, ok := authorities[source]; !ok {
				return nil, fmt.Errorf("catalog: unknown source %q", source)
			}
			authorities[source] = authority
		}
	}
	cat := &Catalog{providers: make(map[string]*provider), key: key}
	for source, authority := range authorities {
		c, err := newClient(authority, cfg, testOnly)
		if err != nil {
			return nil, err
		}
		cat.providers[source] = &provider{source: source, c: c}
	}
	return cat, nil
}

func newClient(raw string, cfg Config, testOnly bool) (*client, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(testOnly && u.Scheme == "http")) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host != strings.ToLower(u.Host) {
		return nil, errors.New("catalog: invalid source authority")
	}
	hc := cfg.Client
	if hc != nil && !testOnly {
		return nil, errors.New("catalog: custom client requires test-only mode")
	}
	if hc == nil {
		tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
		if !testOnly {
			tr.DialContext = safeDialer(cfg.Resolver)
		}
		hc = &http.Client{Transport: tr}
	} else if cfg.Dialer != nil {
		if tr, ok := hc.Transport.(*http.Transport); ok {
			clone := tr.Clone()
			clone.DialContext = cfg.Dialer.DialContext
			hc = &http.Client{Transport: clone, Timeout: hc.Timeout, CheckRedirect: hc.CheckRedirect}
		}
	}
	copyClient := *hc
	baseHost := strings.ToLower(u.Host)
	baseScheme := u.Scheme
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || strings.ToLower(req.URL.Host) != baseHost || (baseScheme == "https" && req.URL.Scheme != "https") {
			return ErrRedirect
		}
		return nil
	}
	return &client{base: u, http: &copyClient, timeout: cfg.Timeout, max: cfg.MaxBodyBytes, token: cfg.BearerToken, resolver: cfg.Resolver, testOnly: testOnly}, nil
}

func (c *client) get(ctx context.Context, requestPath string, query url.Values) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	u := *c.base
	u.Path = path.Join(strings.TrimSuffix(c.base.Path, "/"), "/", strings.TrimPrefix(requestPath, "/"))
	u.RawQuery = query.Encode()
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrMalformed
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
		defer req.Header.Del("Authorization")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirect) {
			return nil, ErrRedirect
		}
		return nil, ErrMalformed
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("catalog: source status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.max+1))
	if err != nil {
		return nil, ErrMalformed
	}
	if int64(len(b)) > c.max {
		return nil, ErrOversize
	}
	return b, nil
}

type cursor struct {
	Source string            `json:"s"`
	Query  string            `json:"q"`
	Size   int               `json:"z"`
	Offset int               `json:"o"`
	Remote string            `json:"r,omitempty"`
	Tokens map[string]string `json:"t,omitempty"`
	Done   map[string]bool   `json:"d,omitempty"`
}
type cursorEnvelope struct {
	Payload string `json:"p"`
	MAC     string `json:"m"`
}

func (c *Catalog) encodeCursor(v cursor) string {
	raw, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write([]byte(payload))
	env, _ := json.Marshal(cursorEnvelope{Payload: payload, MAC: hex.EncodeToString(mac.Sum(nil))})
	return base64.RawURLEncoding.EncodeToString(env)
}
func (c *Catalog) decodeCursor(token, source, query string, size int) (cursor, error) {
	if token == "" {
		return cursor{Source: source, Query: query, Size: size}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor{}, extensions.ErrInvalid
	}
	var env cursorEnvelope
	if json.Unmarshal(b, &env) != nil || env.Payload == "" || len(env.MAC) != 64 {
		return cursor{}, extensions.ErrInvalid
	}
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write([]byte(env.Payload))
	expected, err := hex.DecodeString(env.MAC)
	if err != nil || !hmac.Equal(mac.Sum(nil), expected) {
		return cursor{}, extensions.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(env.Payload)
	if err != nil {
		return cursor{}, extensions.ErrInvalid
	}
	var out cursor
	if json.Unmarshal(raw, &out) != nil || out.Source != source || out.Query != query || out.Size != size || out.Offset < 0 || out.Offset > 1_000_000 || len(out.Remote) > 2048 || len(out.Tokens) > 4 {
		return cursor{}, extensions.ErrInvalid
	}
	for key, token := range out.Tokens {
		if key != SourceOfficialRegistry && key != SourceSmithery && key != SourceGlama && key != SourceGitHub || len(token) > 2048 {
			return cursor{}, extensions.ErrInvalid
		}
	}
	for key := range out.Done {
		if key != SourceOfficialRegistry && key != SourceSmithery && key != SourceGlama && key != SourceGitHub {
			return cursor{}, extensions.ErrInvalid
		}
	}
	return out, nil
}

func parseJSON(b []byte, out any) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return ErrMalformed
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return ErrMalformed
	}
	return nil
}
func rawMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func rawSlice(v any) []any        { a, _ := v.([]any); return a }
func rawString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func digestJSON(v any) string { b, _ := json.Marshal(v); return extensions.DigestBytes(b) }
func credentialRef(id string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("dirextalk:catalog:credential:"+id)).String()
}
func validCommit(v string) bool {
	return regexp.MustCompile(`^[0-9a-fA-F]{40}$`).MatchString(strings.TrimSpace(v))
}
func fullBlobSHA(data []byte) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func remoteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Host != strings.ToLower(u.Host) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(u.Path, "..") {
		return nil, ErrUnsupported
	}
	return u, nil
}
func (c *client) validateRemote(ctx context.Context, raw string) error {
	u, err := remoteURL(raw)
	if err != nil {
		return err
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return ErrUnsupported
		}
		return nil
	}
	if c.testOnly {
		return nil
	}
	r := c.resolver
	if r == nil {
		r = net.DefaultResolver
	}
	ips, err := r.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return ErrUnsupported
	}
	for _, ip := range ips {
		if !isPublicIP(ip.IP) {
			return ErrUnsupported
		}
	}
	return nil
}
func isPublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsPrivate()
}
func safeDialer(resolver Resolver) func(context.Context, string, string) (net.Conn, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, ErrUnsupported
		}
		for _, candidate := range ips {
			if !isPublicIP(candidate.IP) {
				continue
			}
			if conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port)); err == nil {
				return conn, nil
			}
		}
		return nil, ErrUnsupported
	}
}

func inspection(candidate extensions.Candidate, manifest []byte, remote string) (extensions.Inspection, error) {
	u, err := remoteURL(remote)
	if err != nil {
		return extensions.Inspection{}, err
	}
	ref := credentialRef(candidate.ID)
	pathValue := u.EscapedPath()
	if pathValue == "" {
		pathValue = "/"
	}
	port := uint32(443)
	if u.Port() != "" {
		var n uint64
		for _, r := range u.Port() {
			if r < '0' || r > '9' {
				return extensions.Inspection{}, ErrUnsupported
			}
			n = n*10 + uint64(r-'0')
		}
		if n == 0 || n > 65535 {
			return extensions.Inspection{}, ErrUnsupported
		}
		port = uint32(n)
	}
	network := extensions.NetworkGrant{Scheme: "https", Host: u.Hostname(), Port: port, PathPrefix: pathValue, Digest: extensions.DigestBytes([]byte("https://" + u.Hostname() + pathValue))}
	// Inspection only declares the immutable grant.  A write-only credential
	// input configures it later at the lifecycle boundary.
	secret := extensions.SecretGrant{ReferenceID: ref, Purpose: "mcp_credential", BindingDigest: extensions.DigestBytes([]byte("credential:" + ref)), Configured: false}
	execution := extensions.Execution{Remote: &extensions.Endpoint{URL: u.String(), CredentialReferenceID: ref}}
	i := extensions.Inspection{Candidate: candidate, ContentDigest: extensions.DigestBytes(manifest), ManifestDigest: extensions.DigestBytes(manifest), Execution: execution, NetworkGrants: []extensions.NetworkGrant{network}, SecretGrants: []extensions.SecretGrant{secret}}
	i.ExecutionDigest = digestJSON(execution)
	i.NetworkDigest = digestJSON(i.NetworkGrants)
	i.SecretDigest = digestJSON(i.SecretGrants)
	if err := i.Validate(); err != nil {
		return extensions.Inspection{}, err
	}
	return i, nil
}
