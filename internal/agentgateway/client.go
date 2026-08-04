package agentgateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// Config 是 AgentCapability 客户端配置
type Config struct {
	// ServerAddr 是 Agent 的 AgentCapabilityService 地址
	ServerAddr string

	// TLS 配置
	CACertFile     string
	ClientCertFile string
	ClientKeyFile  string

	// TokenFile 是 MS→Agent 方向的 token 文件路径
	TokenFile string

	// InstanceID 是 message-server 实例 ID
	InstanceID string

	// ServerName is the TLS SNI/verification name of the Agent endpoint. It is
	// explicit so production deployments do not depend on a hard-coded
	// certificate name; the default is kept for local compatibility.
	ServerName string

	// AccountGeneration is the current owner account generation. It is sent
	// with every call so the peer can fence a deleted/recreated account.
	AccountGeneration int64

	// 连接池配置
	MaxConcurrentQuery int // 默认 32
	MaxConcurrentWatch int // 默认 64
}

// Client 是 AgentCapabilityService 的客户端（message-server 端）
type Client struct {
	mu     sync.RWMutex
	config *Config
	conn   *grpc.ClientConn
	client capv1.AgentCapabilityServiceClient
	token  []byte

	// 并发控制
	querySem chan struct{}
	watchSem chan struct{}
}

// New 创建新的 Agent gateway 客户端
func New(config *Config) (*Client, error) {
	if config == nil {
		return nil, fmt.Errorf("agent gateway config is required")
	}
	if strings.TrimSpace(config.ServerAddr) == "" {
		return nil, fmt.Errorf("agent gateway server address is required")
	}
	if strings.TrimSpace(config.InstanceID) == "" {
		return nil, fmt.Errorf("agent gateway instance id is required")
	}
	if config.AccountGeneration <= 0 {
		return nil, fmt.Errorf("agent gateway account generation must be positive")
	}
	if config.MaxConcurrentQuery <= 0 {
		config.MaxConcurrentQuery = 32
	}
	if config.MaxConcurrentWatch <= 0 {
		config.MaxConcurrentWatch = 64
	}

	// 读取方向 token
	token, err := os.ReadFile(config.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read token file: %w", err)
	}
	if len(token) == 0 || strings.TrimSpace(string(token)) != string(token) {
		return nil, fmt.Errorf("capability direction token is invalid")
	}
	if _, err := capv1.FormatCapabilityMetadata(string(token), config.InstanceID, config.AccountGeneration); err != nil {
		return nil, fmt.Errorf("capability direction metadata is invalid: %w", err)
	}

	c := &Client{
		config:   config,
		token:    token,
		querySem: make(chan struct{}, config.MaxConcurrentQuery),
		watchSem: make(chan struct{}, config.MaxConcurrentWatch),
	}

	// 加载 TLS 配置
	tlsConfig, err := c.loadTLSConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS config: %w", err)
	}

	// 创建 gRPC 连接
	creds := credentials.NewTLS(tlsConfig)
	conn, err := grpc.NewClient(
		config.ServerAddr,
		grpc.WithTransportCredentials(creds),
		grpc.WithConnectParams(grpc.ConnectParams{
			MinConnectTimeout: 3 * time.Second,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client: %w", err)
	}

	c.conn = conn
	c.client = capv1.NewAgentCapabilityServiceClient(conn)

	return c, nil
}

// SetAccountGeneration updates the outbound fence without rebuilding the
// gRPC connection. It is called by the Service when a portal generation
// changes after account deletion/recreation.
func (c *Client) SetAccountGeneration(generation int64) {
	if c == nil || generation <= 0 {
		return
	}
	c.mu.Lock()
	c.config.AccountGeneration = generation
	c.mu.Unlock()
}

// loadTLSConfig 加载 mTLS 配置
func (c *Client) loadTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.config.ClientCertFile, c.config.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load client cert/key: %w", err)
	}

	caCert, err := os.ReadFile(c.config.CACertFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA cert: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA cert")
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
		MinVersion:   tls.VersionTLS13,
		ServerName:   "dirextalk-agent", // SNI
	}
	if name := strings.TrimSpace(c.config.ServerName); name != "" {
		tlsConfig.ServerName = name
	}

	return tlsConfig, nil
}

// Close 关闭客户端
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// DescribeCapabilities fetches the Agent catalog over the authenticated
// connection. Callers use the returned operation descriptor to compute the
// canonical request digest; no action name is tunneled through the wire.
func (c *Client) DescribeCapabilities(ctx context.Context, callContexts ...*capv1.CallContext) (*capv1.DescribeCapabilitiesResponse, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("agent gateway is not configured")
	}
	if err := c.acquireQuerySem(ctx); err != nil {
		return nil, err
	}
	defer c.releaseQuerySem()
	rootOperationID := uuid.New().String()
	return c.client.DescribeCapabilities(c.authenticatedContext(ctx), &capv1.DescribeCapabilitiesRequest{
		CallContext: c.callContextFor(rootOperationID, callContexts...),
	})
}

// Query executes a read-only Agent capability. Mutations and durable streams
// must use StartOperation so their idempotency ledger is preserved.
func (c *Client) Query(ctx context.Context, operationID, capabilityID, operation string, requestJSON []byte, permission *capv1.PermissionContext, callContexts ...*capv1.CallContext) (*capv1.QueryResponse, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("agent gateway is not configured")
	}
	if err := c.acquireQuerySem(ctx); err != nil {
		return nil, err
	}
	defer c.releaseQuerySem()
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return nil, fmt.Errorf("operation id is required")
	}
	return c.client.Query(c.authenticatedContext(ctx), &capv1.QueryRequest{
		CallContext: c.callContextFor(operationID, callContexts...), CapabilityId: capabilityID,
		OperationId: operation, RequestJson: requestJSON, Permission: permission,
	})
}

// StartOperation 启动一个 Agent operation
func (c *Client) StartOperation(
	ctx context.Context,
	operationID, capabilityID, operation string,
	requestJSON []byte,
	requestDigest []byte,
	expectedRevision int64,
	permission *capv1.PermissionContext,
	callContexts ...*capv1.CallContext,
) (*capv1.StartOperationResponse, error) {
	// 获取 query semaphore（admission 阶段）
	if err := c.acquireQuerySem(ctx); err != nil {
		return nil, err
	}
	defer c.releaseQuerySem()

	callCtx := c.callContextFor(operationID, callContexts...)

	req := &capv1.StartOperationRequest{
		CallContext:      callCtx,
		Permission:       permission,
		OperationId:      operationID,
		CapabilityId:     capabilityID,
		Operation:        operation,
		RequestJson:      requestJSON,
		RequestDigest:    requestDigest,
		ExpectedRevision: expectedRevision,
	}

	return c.client.StartOperation(c.authenticatedContext(ctx), req)
}

// GetOperation returns a persisted operation status from the Agent.
func (c *Client) GetOperation(ctx context.Context, operationID string, permission *capv1.PermissionContext, callContexts ...*capv1.CallContext) (*capv1.GetOperationResponse, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("agent gateway is not configured")
	}
	callCtx := c.callContextFor(operationID, callContexts...)
	return c.client.GetOperation(c.authenticatedContext(ctx), &capv1.GetOperationRequest{
		CallContext: callCtx, Permission: permission, OperationId: operationID,
	})
}

// CancelOperation cancels a durable Agent operation using the same caller
// supplied call context as Start/Watch/Get. It is intentionally not derived
// from a fresh operation id so the Agent can validate one chain and grant
// binding across the complete lifecycle.
func (c *Client) CancelOperation(ctx context.Context, operationID string, permission *capv1.PermissionContext, callContexts ...*capv1.CallContext) (*capv1.CancelOperationResponse, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("agent gateway is not configured")
	}
	if err := c.acquireQuerySem(ctx); err != nil {
		return nil, err
	}
	defer c.releaseQuerySem()
	return c.client.CancelOperation(c.authenticatedContext(ctx), &capv1.CancelOperationRequest{
		CallContext: c.callContextFor(operationID, callContexts...), Permission: permission, OperationId: operationID,
	})
}

// WatchOperation watches a persisted Agent operation. The caller owns the
// returned stream and must drain or cancel it before releasing its request.
func (c *Client) WatchOperation(ctx context.Context, operationID string, afterSequence int64, permission *capv1.PermissionContext, callContexts ...*capv1.CallContext) (capv1.AgentCapabilityService_WatchOperationClient, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("agent gateway is not configured")
	}
	callCtx := c.callContextFor(operationID, callContexts...)
	if err := c.acquireWatchSem(ctx); err != nil {
		return nil, err
	}
	stream, err := c.client.WatchOperation(c.authenticatedContext(ctx), &capv1.WatchOperationRequest{
		CallContext: callCtx, Permission: permission, OperationId: operationID, AfterSequence: afterSequence,
	})
	if err != nil {
		c.releaseWatchSem()
		return nil, err
	}
	return &watchStream{AgentCapabilityService_WatchOperationClient: stream, release: c.releaseWatchSem}, nil
}

type watchStream struct {
	capv1.AgentCapabilityService_WatchOperationClient
	release     func()
	releaseOnce sync.Once
}

func (s *watchStream) Close() {
	if s == nil || s.release == nil {
		return
	}
	s.releaseOnce.Do(s.release)
}

// Recv releases the admission slot as soon as the peer closes the stream or
// the transport fails. This prevents a caller that forgets the optional Close
// helper from leaking a watch slot indefinitely.
func (s *watchStream) Recv() (*capv1.WatchOperationEvent, error) {
	event, err := s.AgentCapabilityService_WatchOperationClient.Recv()
	if err != nil {
		s.Close()
	}
	return event, err
}

func (s *watchStream) CloseSend() error {
	err := s.AgentCapabilityService_WatchOperationClient.CloseSend()
	if err != nil {
		s.Close()
	}
	return err
}

// authenticatedContext attaches the directional credentials to the gRPC
// metadata. The token is never placed in request JSON or logs.
func (c *Client) authenticatedContext(ctx context.Context) context.Context {
	if c == nil {
		return ctx
	}
	c.mu.RLock()
	instanceID := c.config.InstanceID
	generation := c.config.AccountGeneration
	token := append([]byte(nil), c.token...)
	c.mu.RUnlock()
	if generation <= 0 {
		// Construction and SetAccountGeneration both reject non-positive
		// generations. If a caller races an unsafe direct config mutation,
		// omit metadata so the peer fails closed instead of silently falling
		// back to generation one.
		return ctx
	}
	values, err := capv1.FormatCapabilityMetadata(string(token), instanceID, generation)
	if err != nil {
		// Config validation happens at construction and the generation is
		// synchronized from Service before any operation. Keep this helper
		// fail-closed if a caller mutates the config concurrently.
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx,
		capv1.CapabilityAuthorizationMetadata, values[capv1.CapabilityAuthorizationMetadata],
		capv1.CapabilityInstanceMetadata, values[capv1.CapabilityInstanceMetadata],
		capv1.CapabilityGenerationMetadata, values[capv1.CapabilityGenerationMetadata],
	)
}

// createCallContext 创建新的 CallContext
func (c *Client) createCallContext(rootOperationID string) *capv1.CallContext {
	chainID := uuid.New().String()
	deadline := time.Now().Add(30 * time.Second).UnixMilli()
	base := capv1.NewCallContext(chainID, rootOperationID, deadline)
	callContext, err := capv1.IncrementHop(base, "ms")
	if err != nil {
		return base
	}
	return callContext
}

// callContextFor preserves one caller-owned chain across every RPC in an
// operation lifecycle. The variadic form keeps older internal callers source
// compatible; Runner always supplies the context it created at the operation
// boundary, so Start/Watch/Get/Cancel cannot silently fork the audit route.
func (c *Client) callContextFor(rootOperationID string, callContexts ...*capv1.CallContext) *capv1.CallContext {
	if len(callContexts) > 0 && callContexts[0] != nil {
		return callContexts[0]
	}
	return c.createCallContext(rootOperationID)
}

// acquireQuerySem 获取 query semaphore
func (c *Client) acquireQuerySem(ctx context.Context) error {
	select {
	case c.querySem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(1 * time.Second):
		return fmt.Errorf("query semaphore exhausted")
	}
}

// releaseQuerySem 释放 query semaphore
func (c *Client) releaseQuerySem() {
	<-c.querySem
}

// acquireWatchSem 获取 watch semaphore
func (c *Client) acquireWatchSem(ctx context.Context) error {
	select {
	case c.watchSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(1 * time.Second):
		return fmt.Errorf("watch semaphore exhausted")
	}
}

// releaseWatchSem 释放 watch semaphore
func (c *Client) releaseWatchSem() {
	<-c.watchSem
}
