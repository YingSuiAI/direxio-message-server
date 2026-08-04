package productcapability

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
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

	// PeerInstanceID is the Agent instance permitted on this connection. Empty
	// means the peer must still provide an instance id, but no fixed id is
	// configured (useful for single-instance development).
	PeerInstanceID string
	// PeerCommonName binds the Agent mTLS client certificate to this service.
	// The local development certificate convention remains the default.
	PeerCommonName string

	// ExpectedAccountGeneration fences a deleted/recreated owner account. A
	// non-positive value disables the fixed generation check.
	ExpectedAccountGeneration int64

	// GrantPublicKey verifies the opaque Ed25519 grant-v1 supplied by Agent
	// calls. Product is the message-server-owned capability boundary, so it
	// also signs short-lived operation-control grants with GrantPrivateKey;
	// the Agent process is never given this private key.
	GrantPublicKey  []byte
	GrantPrivateKey []byte
	GrantCodec      capv1.GrantCodec

	// DB is the shared PostgreSQL handle used for durable Product operations.
	DB *sql.DB
	// PreparedMatrixStore stages exact signed Matrix PDUs before SendEvents.
	// Production wiring supplies the shared PostgreSQL implementation; a nil
	// value with a nil DB is retained for in-memory protocol tests only.
	PreparedMatrixStore dirextalktransport.PreparedMatrixMutationStore

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

	mu         sync.RWMutex
	ready      bool
	registry   *Registry
	operations *operationStore
}

// New 创建新的 ProductCapabilityService 服务器
func New(config *Config, registry *Registry) (*Server, error) {
	if config == nil {
		return nil, fmt.Errorf("product capability config is required")
	}
	if registry == nil {
		return nil, fmt.Errorf("product capability registry is required")
	}
	if len(config.GrantPublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("product capability grant public key is required")
	}
	if len(config.GrantPrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("product capability grant private key is required")
	}
	privateKey := ed25519.PrivateKey(config.GrantPrivateKey)
	if !ed25519.PublicKey(config.GrantPublicKey).Equal(privateKey.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("product capability grant key pair does not match")
	}
	if config.MaxConcurrentRead <= 0 {
		config.MaxConcurrentRead = 64
	}
	if config.MaxConcurrentMutation <= 0 {
		config.MaxConcurrentMutation = 16
	}
	if strings.TrimSpace(config.PeerCommonName) == "" {
		config.PeerCommonName = "agent-client"
	}

	// 读取方向 token
	token, err := os.ReadFile(config.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read token file: %w", err)
	}
	if len(token) == 0 || strings.TrimSpace(string(token)) != string(token) {
		return nil, fmt.Errorf("capability direction token is invalid")
	}
	if _, err := capv1.FormatCapabilityToken(string(token)); err != nil {
		return nil, fmt.Errorf("capability direction token is invalid: %w", err)
	}

	s := &Server{
		config:      config,
		token:       token,
		registry:    registry,
		operations:  newOperationStore(config.DB),
		readSem:     make(chan struct{}, config.MaxConcurrentRead),
		mutationSem: make(chan struct{}, config.MaxConcurrentMutation),
	}
	preparedStore := config.PreparedMatrixStore
	if preparedStore == nil && config.DB != nil {
		preparedStore = dirextalktransport.NewPostgresPreparedMatrixMutationStore(config.DB)
	}
	s.operations.setPreparedMatrixStore(preparedStore)
	if err := s.operations.ensureSchema(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to initialize product capability operation store: %w", err)
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
	s.SetReady(true)

	go func() {
		if err := s.grpcServer.Serve(listener); err != nil {
			fmt.Printf("gRPC server error: %v\n", err)
		}
	}()

	return nil
}

// Stop 停止服务器
func (s *Server) Stop(ctx context.Context) error {
	s.SetReady(false)
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

	// 验证客户端证书 CN
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return status.Error(codes.Unauthenticated, "client certificate unavailable")
	}
	cert := tlsInfo.State.PeerCertificates[0]
	if cert.Subject.CommonName != configPeerCommonName(s.config) {
		return status.Errorf(codes.Unauthenticated, "invalid client CN: %s", cert.Subject.CommonName)
	}
	if s.config.PeerInstanceID == "" && s.config.InstanceID == "" {
		return status.Error(codes.Unauthenticated, "peer instance id is not configured")
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "capability metadata is required")
	}
	metadataValues := map[string][]string{
		capv1.CapabilityAuthorizationMetadata: md.Get(capv1.CapabilityAuthorizationMetadata),
		capv1.CapabilityInstanceMetadata:      md.Get(capv1.CapabilityInstanceMetadata),
		capv1.CapabilityGenerationMetadata:    md.Get(capv1.CapabilityGenerationMetadata),
	}
	parsedMetadata, metadataErr := capv1.ParseCapabilityMetadata(metadataValues)
	if metadataErr != nil || !constantTimeEqual([]byte(parsedMetadata.Token), s.token) {
		return status.Error(codes.Unauthenticated, "invalid capability direction metadata")
	}
	peerID := strings.TrimSpace(parsedMetadata.InstanceID)
	expectedPeer := strings.TrimSpace(s.config.PeerInstanceID)
	if expectedPeer == "" {
		expectedPeer = strings.TrimSpace(s.config.InstanceID)
	}
	if peerID == "" || expectedPeer == "" || peerID != expectedPeer {
		return status.Error(codes.Unauthenticated, "invalid capability peer instance")
	}
	if expected := s.config.ExpectedAccountGeneration; expected > 0 && parsedMetadata.AccountGeneration != expected {
		return status.Error(codes.Unauthenticated, "stale account generation")
	}

	return nil
}

func configPeerCommonName(config *Config) string {
	if config == nil || strings.TrimSpace(config.PeerCommonName) == "" {
		return "agent-client"
	}
	return strings.TrimSpace(config.PeerCommonName)
}

func firstMetadata(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func constantTimeEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var value byte
	for i := range left {
		value |= left[i] ^ right[i]
	}
	return value == 0
}

// validateCallContext 验证请求中的 CallContext
func (s *Server) validateCallContext(req interface{}) error {
	type hasCallContext interface {
		GetCallContext() *capv1.CallContext
	}

	r, ok := req.(hasCallContext)
	if !ok || r.GetCallContext() == nil {
		return status.Error(codes.InvalidArgument, "call_context is required")
	}
	ctx := r.GetCallContext()
	if err := capv1.ValidateCallContext(ctx); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid call_context: %v", err)
	}
	// Product is the receiving terminal. Advance the peer-supplied ms→agent
	// (or agent) context before handlers bind grants; accepting the pre-advance
	// route in a handler would make the signed route ambiguous and would let a
	// caller reuse a grant at the wrong hop.
	advanced, err := capv1.ValidateAndAdvanceProductCallContext(ctx)
	if err != nil {
		if err.Error() == "cycle detected" {
			return status.Error(codes.FailedPrecondition, "CYCLE_DETECTED")
		}
		return status.Errorf(codes.InvalidArgument, "invalid call path: %v", err)
	}
	setRequestCallContext(req, advanced)
	return nil
}

func setRequestCallContext(req interface{}, callCtx *capv1.CallContext) {
	if callCtx == nil {
		return
	}
	switch value := req.(type) {
	case *capv1.ExchangeProductDelegationRequest:
		value.CallContext = callCtx
	case *capv1.QueryRequest:
		value.CallContext = callCtx
	case *capv1.StartOperationRequest:
		value.CallContext = callCtx
	case *capv1.GetOperationRequest:
		value.CallContext = callCtx
	case *capv1.WatchOperationRequest:
		value.CallContext = callCtx
	case *capv1.CancelOperationRequest:
		value.CallContext = callCtx
	case *capv1.ReconcileOperationRequest:
		value.CallContext = callCtx
	case *capv1.DescribeCapabilitiesRequest:
		value.CallContext = callCtx
	}
}
