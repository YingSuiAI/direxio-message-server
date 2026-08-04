package agentgateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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

	// 连接池配置
	MaxConcurrentQuery int // 默认 32
	MaxConcurrentWatch int // 默认 64
}

// Client 是 AgentCapabilityService 的客户端（message-server 端）
type Client struct {
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

	return tlsConfig, nil
}

// Close 关闭客户端
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// StartOperation 启动一个 Agent operation
func (c *Client) StartOperation(
	ctx context.Context,
	operationID, capabilityID, operation string,
	requestJSON []byte,
	requestDigest []byte,
	expectedRevision int64,
	permission *capv1.PermissionContext,
) (*capv1.StartOperationResponse, error) {
	// 获取 query semaphore（admission 阶段）
	if err := c.acquireQuerySem(ctx); err != nil {
		return nil, err
	}
	defer c.releaseQuerySem()

	// 创建 CallContext
	callCtx := c.createCallContext(operationID)

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

	return c.client.StartOperation(ctx, req)
}

// createCallContext 创建新的 CallContext
func (c *Client) createCallContext(rootOperationID string) *capv1.CallContext {
	chainID := uuid.New().String()
	deadline := time.Now().Add(30 * time.Second).UnixMilli()
	return capv1.NewCallContext(chainID, rootOperationID, deadline)
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
