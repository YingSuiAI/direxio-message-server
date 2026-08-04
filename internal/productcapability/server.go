package productcapability

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"sync"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Config 是 ProductCapabilityService 的配置
type Config struct {
	// ListenAddr 是监听地址
	ListenAddr string

	// TLS 配置
	CACertFile     string
	ServerCertFile string
	ServerKeyFile  string

	// TokenFile 是 Agent→MS 方向的 token 文件路径
	TokenFile string

	// InstanceID 是 message-server 实例 ID
	InstanceID string

	// 连接池配置
	MaxConcurrentRead     int // 默认 64
	MaxConcurrentMutation int // 默认 16
}

// Server 实现 ProductCapabilityService
type Server struct {
	capv1.UnimplementedProductCapabilityServiceServer

	config     *Config
	grpcServer *grpc.Server
	listener   net.Listener
	token      []byte

	// 并发控制
	readSem     chan struct{}
	mutationSem chan struct{}

	mu       sync.RWMutex
	ready    bool
	registry *Registry
}

// New 创建新的 ProductCapabilityService 服务器
func New(config *Config, registry *Registry) (*Server, error) {
	if config.MaxConcurrentRead <= 0 {
		config.MaxConcurrentRead = 64
	}
	if config.MaxConcurrentMutation <= 0 {
		config.MaxConcurrentMutation = 16
	}

	// 读取方向 token
	token, err := os.ReadFile(config.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read token file: %w", err)
	}

	s := &Server{
		config:      config,
		token:       token,
		registry:    registry,
		readSem:     make(chan struct{}, config.MaxConcurrentRead),
		mutationSem: make(chan struct{}, config.MaxConcurrentMutation),
	}

	// 加载 TLS 配置
	tlsConfig, err := s.loadTLSConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS config: %w", err)
	}

	// 创建 gRPC 服务器
	creds := credentials.NewTLS(tlsConfig)
	s.grpcServer = grpc.NewServer(
		grpc.Creds(creds),
		grpc.UnaryInterceptor(s.unaryInterceptor),
		grpc.StreamInterceptor(s.streamInterceptor),
	)

	capv1.RegisterProductCapabilityServiceServer(s.grpcServer, s)

	return s, nil
}

// loadTLSConfig 加载 mTLS 配置
func (s *Server) loadTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(s.config.ServerCertFile, s.config.ServerKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load server cert/key: %w", err)
	}

	caCert, err := os.ReadFile(s.config.CACertFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA cert: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA cert")
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caCertPool,
		MinVersion:   tls.VersionTLS13,
	}

	return tlsConfig, nil
}

// Start 启动服务器
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.config.ListenAddr, err)
	}

	s.listener = listener

	go func() {
		if err := s.grpcServer.Serve(listener); err != nil {
			fmt.Printf("gRPC server error: %v\n", err)
		}
	}()

	return nil
}

// Stop 停止服务器
func (s *Server) Stop(ctx context.Context) error {
	if s.grpcServer != nil {
		stopped := make(chan struct{})
		go func() {
			s.grpcServer.GracefulStop()
			close(stopped)
		}()

		select {
		case <-stopped:
			return nil
		case <-ctx.Done():
			s.grpcServer.Stop()
			return ctx.Err()
		}
	}
	return nil
}

// SetReady 设置 readiness 状态
func (s *Server) SetReady(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = ready
}

// IsReady 返回 readiness 状态
func (s *Server) IsReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

// unaryInterceptor 实现统一的请求拦截
func (s *Server) unaryInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	// 验证认证
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}

	// 验证 CallContext
	if err := s.validateCallContext(req); err != nil {
		return nil, err
	}

	// 检查 readiness（Describe 除外）
	if info.FullMethod != "/dirextalk.capability.v1.ProductCapabilityService/DescribeCapabilities" {
		if !s.IsReady() {
			return nil, status.Error(codes.Unavailable, "service not ready")
		}
	}

	return handler(ctx, req)
}

// streamInterceptor 实现流式请求拦截
func (s *Server) streamInterceptor(
	srv interface{},
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	if err := s.authenticate(ss.Context()); err != nil {
		return err
	}

	if !s.IsReady() {
		return status.Error(codes.Unavailable, "service not ready")
	}

	return handler(srv, ss)
}

// authenticate 验证 mTLS 和方向 token
func (s *Server) authenticate(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "no peer info")
	}

	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return status.Error(codes.Unauthenticated, "no TLS info")
	}

	if len(tlsInfo.State.VerifiedChains) == 0 {
		return status.Error(codes.Unauthenticated, "no verified chains")
	}

	// 验证客户端证书 CN（应该是 "agent-client"）
	cert := tlsInfo.State.PeerCertificates[0]
	if cert.Subject.CommonName != "agent-client" {
		return status.Errorf(codes.Unauthenticated, "invalid client CN: %s", cert.Subject.CommonName)
	}

	// TODO: 验证方向 token 和 instance ID

	return nil
}

// validateCallContext 验证请求中的 CallContext
func (s *Server) validateCallContext(req interface{}) error {
	type hasCallContext interface {
		GetCallContext() *capv1.CallContext
	}

	if r, ok := req.(hasCallContext); ok {
		ctx := r.GetCallContext()
		if ctx != nil {
			if err := capv1.ValidateCallContext(ctx); err != nil {
				return status.Errorf(codes.InvalidArgument, "invalid call_context: %v", err)
			}

			// ProductCapability 是终端节点，不应继续转发
			// 验证这不是从另一个 Product handler 来的
			if err := capv1.ValidateCallPath(ctx, "product"); err != nil {
				if err.Error() == "cycle detected" {
					return status.Error(codes.FailedPrecondition, "CYCLE_DETECTED")
				}
				return status.Errorf(codes.InvalidArgument, "invalid call path: %v", err)
			}
		}
	}

	return nil
}
