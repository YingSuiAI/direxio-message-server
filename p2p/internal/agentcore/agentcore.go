// Package agentcore owns the deployment-bound Agent Core discovery adapter.
// It deliberately exposes only the instance-level discovery surface; Core
// endpoints and credentials never cross this package's ProductCore boundary.
package agentcore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	DefaultConnectTimeout    = 3 * time.Second
	DefaultUnaryTimeout      = 5 * time.Second
	DefaultStreamIdleTimeout = 30 * time.Second
	DefaultProbeTimeout      = 5 * time.Second
	MaxConnectTimeout        = 30 * time.Second
	MaxUnaryTimeout          = 60 * time.Second
	MaxStreamIdleTimeout     = 10 * time.Minute
	MaxProbeTimeout          = 60 * time.Second
	ProbeFreshnessTTL        = 15 * time.Second
)

var requiredCapabilities = []string{"agent.info", "model.profile", "conversation"}

// Keep this allowlist tied to actual server-side adapters.
var implementedCapabilities = map[string]bool{"agent.info": true, "model.profile": true, "conversation": true, "task": true, "schedule": true, "confirmation": true, "mcp": true, "skill": true, "aws.control": true, "workload.core_runner": true, "workload.aws_ssm": true, "workload.aws_ecs": true}

// SupportedCapabilities is the complete capability vocabulary understood by
// this Message Server build, in stable response order.
var SupportedCapabilities = []string{
	"agent.info", "model.profile", "conversation", "conversation.extensions",
	"task", "schedule", "confirmation", "mcp", "skill", "knowledge",
	"aws.control", "workload.core_runner", "workload.aws_ssm", "workload.aws_ecs",
}

var SupportedModelProviders = []string{"openai_compatible", "anthropic", "gemini"}

// Config is deployment configuration. Protected values are read from files
// and retained only by the adapter; they are not serializable ProductCore
// state.
type Config struct {
	Enabled            bool
	Address            string
	ServerName         string
	CAFile             string
	TokenFile          string
	ExpectedInstanceID string
	ConnectTimeout     time.Duration
	UnaryTimeout       time.Duration
	StreamIdleTimeout  time.Duration
	ProbeTimeout       time.Duration
}

func (c Config) withDefaults() Config {
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	if c.UnaryTimeout == 0 {
		c.UnaryTimeout = DefaultUnaryTimeout
	}
	if c.StreamIdleTimeout == 0 {
		c.StreamIdleTimeout = DefaultStreamIdleTimeout
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = DefaultProbeTimeout
	}
	return c
}

// Validate returns an error only for enabled, incomplete or unbounded config.
func (c Config) Validate() error {
	c = c.withDefaults()
	if !c.Enabled {
		return nil
	}
	missing := make([]string, 0, 5)
	for _, item := range []struct{ name, value string }{
		{"P2P_AGENT_CORE_ADDRESS", c.Address}, {"P2P_AGENT_CORE_SERVER_NAME", c.ServerName},
		{"P2P_AGENT_CORE_CA_FILE", c.CAFile}, {"P2P_AGENT_CORE_TOKEN_FILE", c.TokenFile},
		{"P2P_AGENT_CORE_EXPECTED_INSTANCE_ID", c.ExpectedInstanceID},
	} {
		if strings.TrimSpace(item.value) == "" {
			missing = append(missing, item.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("agent core enabled but required configuration is incomplete: %s", strings.Join(missing, ", "))
	}
	if host, port, err := net.SplitHostPort(c.Address); err != nil || strings.TrimSpace(host) == "" || !validPort(port) {
		return fmt.Errorf("agent core address must be a host:port")
	}
	if strings.ContainsAny(c.ServerName, " \t\r\n/") || (net.ParseIP(c.ServerName) == nil && strings.Contains(c.ServerName, ":")) {
		return fmt.Errorf("agent core server name is invalid")
	}
	if _, err := canonicalInstanceUUID(c.ExpectedInstanceID); err != nil {
		return fmt.Errorf("agent core expected instance id is invalid")
	}
	if err := validateCAFile(c.CAFile); err != nil {
		return fmt.Errorf("agent core CA file is invalid: %w", err)
	}
	if _, err := readCanonicalToken(c.TokenFile); err != nil {
		return fmt.Errorf("agent core token file is invalid: %w", err)
	}
	for _, item := range []struct {
		name       string
		value, max time.Duration
	}{
		{"connect_timeout", c.ConnectTimeout, MaxConnectTimeout}, {"unary_timeout", c.UnaryTimeout, MaxUnaryTimeout},
		{"stream_idle_timeout", c.StreamIdleTimeout, MaxStreamIdleTimeout}, {"probe_timeout", c.ProbeTimeout, MaxProbeTimeout},
	} {
		if item.value <= 0 || item.value > item.max {
			return fmt.Errorf("agent core %s must be between 1ns and %s", item.name, item.max)
		}
	}
	return nil
}

func validPort(raw string) bool {
	port, err := strconv.Atoi(raw)
	return err == nil && port > 0 && port <= 65535
}

func canonicalInstanceUUID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw {
		return "", errors.New("expected canonical UUID")
	}
	return raw, nil
}

func validateCAFile(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("CA file is not a readable regular file")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return errors.New("CA file is not readable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return errors.New("CA file does not contain a valid PEM certificate")
	}
	return nil
}

func readCanonicalToken(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("token file is not readable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("token file is not readable")
	}
	if err := validateTokenFileInfo(info); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil || len(raw) > 128 {
		return nil, errors.New("token file is not readable")
	}
	encoded := string(raw)
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, errors.New("token must be canonical unpadded base64url for exactly 32 bytes")
	}
	return decoded, nil
}

// ConfigFromEnv reads protected deployment inputs. Invalid values are returned
// to startup; an enabled incomplete config must fail closed.
func ConfigFromEnv() (Config, error) {
	c := Config{Enabled: false}
	if raw := strings.TrimSpace(os.Getenv("P2P_AGENT_CORE_ENABLED")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return c, fmt.Errorf("P2P_AGENT_CORE_ENABLED: %w", err)
		}
		c.Enabled = value
	}
	c.Address, c.ServerName = strings.TrimSpace(os.Getenv("P2P_AGENT_CORE_ADDRESS")), strings.TrimSpace(os.Getenv("P2P_AGENT_CORE_SERVER_NAME"))
	c.CAFile, c.TokenFile, c.ExpectedInstanceID = strings.TrimSpace(os.Getenv("P2P_AGENT_CORE_CA_FILE")), strings.TrimSpace(os.Getenv("P2P_AGENT_CORE_TOKEN_FILE")), strings.TrimSpace(os.Getenv("P2P_AGENT_CORE_EXPECTED_INSTANCE_ID"))
	var err error
	for env, dst := range map[string]*time.Duration{
		"P2P_AGENT_CORE_CONNECT_TIMEOUT": &c.ConnectTimeout, "P2P_AGENT_CORE_UNARY_TIMEOUT": &c.UnaryTimeout,
		"P2P_AGENT_CORE_STREAM_IDLE_TIMEOUT": &c.StreamIdleTimeout, "P2P_AGENT_CORE_PROBE_TIMEOUT": &c.ProbeTimeout,
	} {
		if raw := strings.TrimSpace(os.Getenv(env)); raw != "" {
			*dst, err = time.ParseDuration(raw)
			if err != nil {
				return c, fmt.Errorf("%s: %w", env, err)
			}
		}
	}
	return c, c.Validate()
}

type Status string

const (
	StatusNotConfigured Status = "not_configured"
	StatusReady         Status = "ready"
	StatusUnavailable   Status = "unavailable"
	StatusIncompatible  Status = "incompatible"
)

type Snapshot struct {
	Configured              bool     `json:"configured"`
	Status                  Status   `json:"status"`
	InstanceID              string   `json:"instance_id,omitempty"`
	APIVersion              string   `json:"api_version,omitempty"`
	Capabilities            []string `json:"capabilities"`
	SupportedModelProviders []string `json:"supported_model_providers"`
}

type Client struct {
	cfg             Config
	mu              sync.RWMutex
	snapshot        Snapshot
	probeMu         sync.Mutex
	probeDone       chan struct{}
	probeGeneration uint64
	lastProbe       time.Time
	probeOverride   func(context.Context) (Snapshot, error)
	connMu          sync.Mutex
	conn            *grpc.ClientConn
}

func New(c Config) (*Client, error) {
	c = c.withDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	s := Snapshot{Configured: c.Enabled, Status: StatusNotConfigured, Capabilities: []string{}, SupportedModelProviders: []string{}}
	if c.Enabled {
		s.Status = StatusUnavailable
	}
	return &Client{cfg: c, snapshot: s}, nil
}

func (c *Client) Configured() bool { return c != nil && c.cfg.Enabled }
func (c *Client) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneSnapshot(c.snapshot)
}
func cloneSnapshot(s Snapshot) Snapshot {
	s.Capabilities = append([]string(nil), s.Capabilities...)
	s.SupportedModelProviders = append([]string(nil), s.SupportedModelProviders...)
	return s
}

func (c *Client) ReadyError() error {
	if c == nil {
		return errors.New("agent core client is unavailable")
	}
	return c.cfg.Validate()
}

// Probe performs an authenticated TLS 1.3 discovery probe. Peer failures are
// recorded as unavailable and returned as nil so startup remains nonfatal.
func (c *Client) Probe(ctx context.Context) error {
	if c == nil || !c.cfg.Enabled {
		return nil
	}
	now := time.Now()
	c.probeMu.Lock()
	if !c.lastProbe.IsZero() && now.Sub(c.lastProbe) < ProbeFreshnessTTL {
		c.probeMu.Unlock()
		return nil
	}
	if c.probeDone != nil {
		done := c.probeDone
		c.probeMu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.probeGeneration++
	generation := c.probeGeneration
	done := make(chan struct{})
	c.probeDone = done
	c.probeMu.Unlock()

	var candidate Snapshot
	var err error
	if c.probeOverride != nil {
		candidate, err = c.probeOverride(ctx)
	} else {
		candidate, err = c.probe(ctx)
	}
	c.probeMu.Lock()
	if generation == c.probeGeneration {
		c.lastProbe = time.Now()
		if err != nil {
			candidate = Snapshot{Configured: true, Status: classifyStatus(err), Capabilities: []string{}, SupportedModelProviders: []string{}}
		}
		c.mu.Lock()
		c.snapshot = cloneSnapshot(candidate)
		c.mu.Unlock()
	}
	c.probeDone = nil
	close(done)
	c.probeMu.Unlock()
	return nil
}

func classifyStatus(err error) Status {
	if errors.Is(err, errIncompatible) || status.Code(err) == codes.Unauthenticated || status.Code(err) == codes.PermissionDenied {
		return StatusIncompatible
	}
	return StatusUnavailable
}

var errIncompatible = errors.New("agent core protocol incompatible")

func (c *Client) probe(parent context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(parent, c.cfg.ProbeTimeout)
	defer cancel()
	caPEM, err := os.ReadFile(c.cfg.CAFile)
	if err != nil {
		return Snapshot{}, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return Snapshot{}, fmt.Errorf("invalid agent core CA")
	}
	token, err := readCanonicalToken(c.cfg.TokenFile)
	if err != nil {
		return Snapshot{}, err
	}
	conn, err := c.connection(ctx, pool)
	if err != nil {
		return Snapshot{}, err
	}
	client := agentv1.NewAgentServiceClient(conn)
	call := func(call func(context.Context) error) error {
		callCtx, cancel := context.WithTimeout(ctx, c.cfg.UnaryTimeout)
		defer cancel()
		return call(metadata.AppendToOutgoingContext(callCtx, "authorization", "DTX-Agent-Token "+base64.RawURLEncoding.EncodeToString(token)))
	}
	var info *agentv1.GetInstanceInfoResponse
	if err := call(func(callCtx context.Context) error {
		var e error
		info, e = client.GetInstanceInfo(callCtx, &agentv1.GetInstanceInfoRequest{})
		return e
	}); err != nil {
		return Snapshot{}, err
	}
	var caps *agentv1.GetCapabilitiesResponse
	if err := call(func(callCtx context.Context) error {
		var e error
		caps, e = client.GetCapabilities(callCtx, &agentv1.GetCapabilitiesRequest{})
		return e
	}); err != nil {
		return Snapshot{}, err
	}
	if info == nil || caps == nil || strings.TrimSpace(info.GetInstanceId()) != c.cfg.ExpectedInstanceID || strings.TrimSpace(info.GetApiVersion()) != "v1" || strings.TrimSpace(caps.GetApiVersion()) != "v1" {
		return Snapshot{}, errIncompatible
	}
	enabled := make(map[string]bool, len(caps.GetCapabilities()))
	for _, cap := range caps.GetCapabilities() {
		if cap != nil && cap.GetEnabled() {
			enabled[cap.GetName()] = true
		}
	}
	for _, required := range requiredCapabilities {
		if !enabled[required] || !implementedCapabilities[required] {
			return Snapshot{}, errIncompatible
		}
	}
	intersection := make([]string, 0, len(SupportedCapabilities))
	for _, name := range SupportedCapabilities {
		if enabled[name] && implementedCapabilities[name] {
			intersection = append(intersection, name)
		}
	}
	return Snapshot{Configured: true, Status: StatusReady, InstanceID: safeInstanceID(info.GetInstanceId()), APIVersion: "v1", Capabilities: intersection, SupportedModelProviders: append([]string(nil), SupportedModelProviders...)}, nil
}

// connection returns the deployment-bound TLS connection, reusing it across
// bounded unary calls. The connection contains no bearer credentials; those
// are read from the protected token file for each RPC.
func (c *Client) connection(parent context.Context, pool *x509.CertPool) (*grpc.ClientConn, error) {
	c.connMu.Lock()
	if c.conn != nil {
		conn := c.conn
		c.connMu.Unlock()
		return conn, nil
	}
	c.connMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, c.cfg.ConnectTimeout)
	defer cancel()
	conn, err := grpc.DialContext(ctx, c.cfg.Address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		ServerName: c.cfg.ServerName, RootCAs: pool,
	})), grpc.WithBlock())
	if err != nil {
		return nil, err
	}
	c.connMu.Lock()
	if c.conn == nil {
		c.conn = conn
		c.connMu.Unlock()
		return conn, nil
	}
	existing := c.conn
	c.connMu.Unlock()
	_ = conn.Close()
	return existing, nil
}

func (c *Client) unary(parent context.Context, call func(context.Context, agentv1.ModelProfileServiceClient) error) error {
	if c == nil || !c.cfg.Enabled {
		return status.Error(codes.Unavailable, "agent core is not configured")
	}
	ctx, cancel := context.WithTimeout(parent, c.cfg.UnaryTimeout)
	defer cancel()
	caPEM, err := os.ReadFile(c.cfg.CAFile)
	if err != nil {
		return status.Error(codes.Unavailable, "agent core trust is unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return status.Error(codes.Unavailable, "agent core trust is unavailable")
	}
	token, err := readCanonicalToken(c.cfg.TokenFile)
	if err != nil {
		return status.Error(codes.Unauthenticated, "agent core authentication failed")
	}
	conn, err := c.connection(ctx, pool)
	if err != nil {
		return err
	}
	callCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "DTX-Agent-Token "+base64.RawURLEncoding.EncodeToString(token))
	return call(callCtx, agentv1.NewModelProfileServiceClient(conn))
}

func modelActionError(err error) *actionbase.Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errIncompatible) {
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_incompatible", "agent core protocol is incompatible")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return actionbase.CodedError(http.StatusServiceUnavailable, "agent_core_unavailable", "agent core is unavailable")
	}
	code := status.Code(err)
	switch code {
	case codes.InvalidArgument:
		return actionbase.CodedError(http.StatusBadRequest, "agent_core_invalid_argument", "agent core rejected the model profile request")
	case codes.Unauthenticated, codes.PermissionDenied:
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_trust_failed", "agent core authentication failed")
	case codes.NotFound:
		return actionbase.CodedError(http.StatusNotFound, "agent_core_not_found", "agent core model profile was not found")
	case codes.Aborted:
		return actionbase.CodedError(http.StatusConflict, "agent_core_conflict", "agent core model profile revision conflict")
	case codes.FailedPrecondition:
		return actionbase.CodedError(http.StatusConflict, "agent_core_precondition_failed", "agent core model profile precondition failed")
	case codes.DeadlineExceeded, codes.Unavailable:
		return actionbase.CodedError(http.StatusServiceUnavailable, "agent_core_unavailable", "agent core is unavailable")
	case codes.Unimplemented:
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_incompatible", "agent core protocol is incompatible")
	default:
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_upstream_failed", "agent core model profile request failed")
	}
}

func (c *Client) modelProfileSync(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	idempotency, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	def, _, e := optionalProfileRef(params, "default_client_profile_id")
	if e != nil {
		return nil, e
	}
	raw, ok := params["entries"]
	if !ok {
		return nil, actionbase.BadRequest("entries is required")
	}
	entriesRaw, ok := raw.([]any)
	if !ok {
		if typed, typedOK := raw.([]map[string]any); typedOK {
			entriesRaw = make([]any, len(typed))
			for i := range typed {
				entriesRaw[i] = typed[i]
			}
			ok = true
		}
	}
	if !ok {
		return nil, actionbase.BadRequest("entries must be an array")
	}
	entries := make([]*agentv1.CoreModelProfileSyncEntry, 0, len(entriesRaw))
	for _, item := range entriesRaw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, actionbase.BadRequest("entries must contain objects")
		}
		entry, err := syncEntry(m)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	var response *agentv1.ModelProfileServiceSyncResponse
	err := c.unary(ctx, func(callCtx context.Context, client agentv1.ModelProfileServiceClient) error {
		var err error
		response, err = client.Sync(callCtx, &agentv1.ModelProfileServiceSyncRequest{IdempotencyKey: idempotency, DefaultClientProfileId: def, Entries: entries})
		return err
	})
	if err != nil {
		return nil, modelActionError(err)
	}
	if response == nil {
		return nil, modelActionError(errors.New("agent core returned an empty model profile sync response"))
	}
	return syncResponseMap(response), nil
}

func (c *Client) modelProfileList(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	pageSize, e := optionalInt32(params, "page_size")
	if e != nil {
		return nil, e
	}
	pageToken, e := optionalString(params, "page_token")
	if e != nil {
		return nil, e
	}
	var response *agentv1.ModelProfileServiceListResponse
	err := c.unary(ctx, func(callCtx context.Context, client agentv1.ModelProfileServiceClient) error {
		var err error
		response, err = client.List(callCtx, &agentv1.ModelProfileServiceListRequest{PageSize: pageSize, PageToken: pageToken})
		return err
	})
	if err != nil {
		return nil, modelActionError(err)
	}
	if response == nil {
		return nil, modelActionError(errors.New("agent core returned an empty model profile list response"))
	}
	profiles := make([]any, 0, len(response.GetProfiles()))
	for _, profile := range response.GetProfiles() {
		profiles = append(profiles, profileMap(profile))
	}
	return map[string]any{"profiles": profiles, "next_page_token": response.GetNextPageToken()}, nil
}

func (c *Client) modelProfileGet(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	id, e := requiredString(params, "profile_id")
	if e != nil {
		return nil, e
	}
	var response *agentv1.ModelProfileServiceGetResponse
	err := c.unary(ctx, func(callCtx context.Context, client agentv1.ModelProfileServiceClient) error {
		var err error
		response, err = client.Get(callCtx, &agentv1.ModelProfileServiceGetRequest{ProfileId: id})
		return err
	})
	if err != nil {
		return nil, modelActionError(err)
	}
	if response == nil {
		return nil, modelActionError(errors.New("agent core returned an empty model profile get response"))
	}
	return map[string]any{"profile": profileMap(response.GetProfile())}, nil
}

func (c *Client) modelProfileDelete(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	idempotency, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredString(params, "profile_id")
	if e != nil {
		return nil, e
	}
	revision, e := optionalInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	var err error
	err = c.unary(ctx, func(callCtx context.Context, client agentv1.ModelProfileServiceClient) error {
		_, err := client.Delete(callCtx, &agentv1.ModelProfileServiceDeleteRequest{IdempotencyKey: idempotency, ProfileId: id, ExpectedRevision: revision})
		return err
	})
	if err != nil {
		return nil, modelActionError(err)
	}
	return map[string]any{"deleted": true, "profile_id": id}, nil
}

func (c *Client) modelHandlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{
		"agent.core.model_profiles.sync":   c.modelProfileSync,
		"agent.core.model_profiles.list":   c.modelProfileList,
		"agent.core.model_profiles.get":    c.modelProfileGet,
		"agent.core.model_profiles.delete": c.modelProfileDelete,
	}
}

func requiredString(params map[string]any, key string) (string, *actionbase.Error) {
	value, err := optionalString(params, key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", actionbase.BadRequest(key + " is required")
	}
	return strings.TrimSpace(value), nil
}

// optionalProfileRef preserves the exact client-owned reference bytes. These
// IDs are stable cross-process keys; silently trimming them would change the
// idempotency/reference contract. Omission is distinct from a present empty
// value so callers cannot accidentally select an empty default.
func optionalProfileRef(params map[string]any, key string) (string, bool, *actionbase.Error) {
	value, present := params[key]
	if !present {
		return "", false, nil
	}
	s, ok := value.(string)
	if !ok || value == nil {
		return "", true, actionbase.BadRequest(key + " must be a string")
	}
	if strings.TrimSpace(s) == "" {
		return "", true, actionbase.BadRequest(key + " is required when present")
	}
	if strings.TrimSpace(s) != s {
		return "", true, actionbase.BadRequest(key + " must not have surrounding whitespace")
	}
	return s, true, nil
}

func optionalString(params map[string]any, key string) (string, *actionbase.Error) {
	value, ok := params[key]
	if !ok || value == nil {
		return "", nil
	}
	s, ok := value.(string)
	if !ok {
		return "", actionbase.BadRequest(key + " must be a string")
	}
	return s, nil
}

func optionalInt64(params map[string]any, key string) (int64, *actionbase.Error) {
	value, ok := params[key]
	if !ok || value == nil {
		return 0, nil
	}
	var result int64
	switch n := value.(type) {
	case int:
		result = int64(n)
	case int32:
		result = int64(n)
	case int64:
		result = n
	case uint:
		if uint64(n) > uint64(^uint64(0)>>1) {
			return 0, actionbase.BadRequest(key + " is out of range")
		}
		result = int64(n)
	case uint32:
		result = int64(n)
	case uint64:
		if n > uint64(^uint64(0)>>1) {
			return 0, actionbase.BadRequest(key + " is out of range")
		}
		result = int64(n)
	case float64:
		if n != float64(int64(n)) {
			return 0, actionbase.BadRequest(key + " must be an integer")
		}
		result = int64(n)
	case json.Number:
		parsed, err := n.Int64()
		if err != nil {
			return 0, actionbase.BadRequest(key + " must be an integer")
		}
		result = parsed
	default:
		return 0, actionbase.BadRequest(key + " must be an integer")
	}
	return result, nil
}

func optionalInt32(params map[string]any, key string) (int32, *actionbase.Error) {
	v, err := optionalInt64(params, key)
	if err != nil {
		return 0, err
	}
	if v < -2147483648 || v > 2147483647 {
		return 0, actionbase.BadRequest(key + " is out of range")
	}
	return int32(v), nil
}

func optionalFloat64(m map[string]any, key string) (*float64, *actionbase.Error) {
	value, ok := m[key]
	if !ok || value == nil {
		return nil, nil
	}
	var result float64
	switch n := value.(type) {
	case float64:
		result = n
	case float32:
		result = float64(n)
	case int:
		result = float64(n)
	case int64:
		result = float64(n)
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return nil, actionbase.BadRequest(key + " must be a number")
		}
		result = parsed
	default:
		return nil, actionbase.BadRequest(key + " must be a number")
	}
	return &result, nil
}

func syncEntry(m map[string]any) (*agentv1.CoreModelProfileSyncEntry, *actionbase.Error) {
	clientID, present, e := optionalProfileRef(m, "client_profile_id")
	if e != nil {
		return nil, e
	}
	if !present {
		return nil, actionbase.BadRequest("client_profile_id is required")
	}
	displayName, e := optionalString(m, "display_name")
	if e != nil {
		return nil, e
	}
	providerName, e := requiredString(m, "provider")
	if e != nil {
		return nil, e
	}
	provider, ok := parseProvider(providerName)
	if !ok {
		return nil, actionbase.BadRequest("provider is unsupported")
	}
	baseURL, e := optionalString(m, "base_url")
	if e != nil {
		return nil, e
	}
	model, e := optionalString(m, "model")
	if e != nil {
		return nil, e
	}
	systemPrompt, e := optionalString(m, "system_prompt")
	if e != nil {
		return nil, e
	}
	reasoning, e := optionalString(m, "reasoning_effort")
	if e != nil {
		return nil, e
	}
	revision, e := optionalInt64(m, "expected_revision")
	if e != nil {
		return nil, e
	}
	temperature, e := optionalFloat64(m, "temperature")
	if e != nil {
		return nil, e
	}
	topP, e := optionalFloat64(m, "top_p")
	if e != nil {
		return nil, e
	}
	maxTokens, e := optionalInt32(m, "max_output_tokens")
	if e != nil {
		return nil, e
	}
	contextWindow, e := optionalInt32(m, "context_window")
	if e != nil {
		return nil, e
	}
	entry := &agentv1.CoreModelProfileSyncEntry{ClientProfileId: clientID, DisplayName: displayName, Provider: provider, BaseUrl: baseURL, Model: model, SystemPrompt: systemPrompt, ReasoningEffort: reasoning, ExpectedRevision: nil, Temperature: temperature, TopP: topP, MaxOutputTokens: maxTokens, ContextWindow: contextWindow}
	if _, present := m["expected_revision"]; present {
		entry.ExpectedRevision = &revision
	}
	if value, present := m["api_key"]; present {
		key, ok := value.(string)
		if !ok {
			return nil, actionbase.BadRequest("api_key must be a string")
		}
		if key == "" {
			return nil, actionbase.BadRequest("api_key must be non-empty when present")
		}
		entry.ApiKey = &key
	}
	return entry, nil
}

func parseProvider(value string) (agentv1.CoreModelProvider, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "openai", "openai_compatible":
		return agentv1.CoreModelProvider_CORE_MODEL_PROVIDER_OPENAI_COMPATIBLE, true
	case "anthropic":
		return agentv1.CoreModelProvider_CORE_MODEL_PROVIDER_ANTHROPIC, true
	case "gemini":
		return agentv1.CoreModelProvider_CORE_MODEL_PROVIDER_GEMINI, true
	default:
		return agentv1.CoreModelProvider_CORE_MODEL_PROVIDER_UNSPECIFIED, false
	}
}

func providerName(value agentv1.CoreModelProvider) string {
	switch value {
	case agentv1.CoreModelProvider_CORE_MODEL_PROVIDER_OPENAI_COMPATIBLE:
		return "openai_compatible"
	case agentv1.CoreModelProvider_CORE_MODEL_PROVIDER_ANTHROPIC:
		return "anthropic"
	case agentv1.CoreModelProvider_CORE_MODEL_PROVIDER_GEMINI:
		return "gemini"
	default:
		return ""
	}
}

func profileMap(profile *agentv1.CoreModelProfile) map[string]any {
	if profile == nil {
		return nil
	}
	out := map[string]any{"profile_id": profile.GetProfileId(), "client_profile_id": profile.GetClientProfileId(), "display_name": profile.GetDisplayName(), "provider": providerName(profile.GetProvider()), "base_url": profile.GetBaseUrl(), "model": profile.GetModel(), "system_prompt": profile.GetSystemPrompt(), "api_key_configured": profile.GetApiKeyConfigured(), "max_output_tokens": profile.GetMaxOutputTokens(), "context_window": profile.GetContextWindow(), "reasoning_effort": profile.GetReasoningEffort(), "revision": profile.GetRevision()}
	if profile.Temperature != nil {
		out["temperature"] = profile.GetTemperature()
	}
	if profile.TopP != nil {
		out["top_p"] = profile.GetTopP()
	}
	if timestamp := profile.GetCreatedAt(); timestamp != nil && timestamp.IsValid() {
		out["created_at"] = timestamp.AsTime().UTC().Format(time.RFC3339Nano)
	}
	if timestamp := profile.GetUpdatedAt(); timestamp != nil && timestamp.IsValid() {
		out["updated_at"] = timestamp.AsTime().UTC().Format(time.RFC3339Nano)
	}
	return out
}

func syncResponseMap(response *agentv1.ModelProfileServiceSyncResponse) map[string]any {
	profiles := make([]any, 0, len(response.GetProfiles()))
	for _, profile := range response.GetProfiles() {
		profiles = append(profiles, profileMap(profile))
	}
	return map[string]any{"profiles": profiles, "default_client_profile_id": response.GetDefaultClientProfileId()}
}

func safeInstanceID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return "redacted-safe-id"
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:-", r) {
			continue
		}
		return "redacted-safe-id"
	}
	return value
}

// Handlers returns owner-facing sanitized discovery actions.
func (c *Client) Handlers() map[string]actionbase.Handler {
	handlers := map[string]actionbase.Handler{
		"agent.backends.get":    c.backendsGet,
		"agent.core.status.get": c.statusGet,
	}
	for name, handler := range c.modelHandlers() {
		handlers[name] = handler
	}
	for name, handler := range c.taskHandlers() {
		handlers[name] = handler
	}
	for name, handler := range c.scheduleHandlers() {
		handlers[name] = handler
	}
	for name, handler := range c.confirmationHandlers() {
		handlers[name] = handler
	}
	for name, handler := range c.workloadHandlers() {
		handlers[name] = handler
	}
	for name, handler := range c.awsHandlers() {
		handlers[name] = handler
	}
	for name, handler := range c.extensionHandlers(agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP, "mcp") {
		handlers[name] = handler
	}
	handlers["agent.core.mcp.list_tools"] = c.extensionListTools
	for name, handler := range c.extensionHandlers(agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL, "skills") {
		handlers[name] = handler
	}
	return handlers
}

func (c *Client) backendsGet(ctx context.Context, _ map[string]any) (any, *actionbase.Error) {
	_ = c.Probe(ctx)
	core := c.Snapshot()
	return map[string]any{
		"embedded": map[string]any{"available": true, "capabilities": []string{"chat", "models.query", "bundled_tools"}},
		"core":     snapshotMap(core),
	}, nil
}

func (c *Client) statusGet(ctx context.Context, _ map[string]any) (any, *actionbase.Error) {
	_ = c.Probe(ctx)
	return snapshotMap(c.Snapshot()), nil
}

func snapshotMap(s Snapshot) map[string]any {
	return map[string]any{"configured": s.Configured, "status": string(s.Status), "instance_id": s.InstanceID, "api_version": s.APIVersion, "capabilities": append([]string(nil), s.Capabilities...), "supported_model_providers": append([]string(nil), s.SupportedModelProviders...)}
}
