package agentcore

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func completeConfig() Config {
	return Config{Enabled: true, Address: "127.0.0.1:9443", ServerName: "core.example", CAFile: "/tmp/ca", TokenFile: "/tmp/token", ExpectedInstanceID: "00000000-0000-4000-8000-000000000001", ConnectTimeout: time.Second, UnaryTimeout: time.Second, StreamIdleTimeout: time.Second, ProbeTimeout: time.Second}
}

func TestEnabledConfigRequiresProtectedInputs(t *testing.T) {
	c := completeConfig()
	c.TokenFile = ""
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "P2P_AGENT_CORE_TOKEN_FILE") {
		t.Fatalf("Validate() = %v, want missing token file startup error", err)
	}
}

func TestEnabledConfigValidatesAddressTrustFilesAndUUID(t *testing.T) {
	dir := t.TempDir()
	ca, _, _ := testCertificate(t, "core.example")
	caPath, tokenPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	validToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if err := os.WriteFile(tokenPath, []byte(validToken), 0600); err != nil {
		t.Fatal(err)
	}
	valid := completeConfig()
	valid.CAFile, valid.TokenFile = caPath, tokenPath
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"address": func(c *Config) { c.Address = "not-a-host-port" },
		"ca":      func(c *Config) { c.CAFile = filepath.Join(dir, "bad-ca") },
		"token": func(c *Config) {
			c.TokenFile = filepath.Join(dir, "bad-token")
			_ = os.WriteFile(c.TokenFile, []byte("not-base64"), 0600)
		},
		"instance": func(c *Config) { c.ExpectedInstanceID = "not-a-uuid" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid deployment config accepted")
			}
		})
	}
}

func TestTokenFileRequiresExactCanonicalBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	value := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, raw := range []string{value + "\n", " " + value, value + "extra"} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readCanonicalToken(path); err == nil {
			t.Fatalf("accepted non-exact token bytes %q", raw)
		}
	}
}

func TestProbeUsesAuthorizationTokenAndTLS13(t *testing.T) {
	caPEM, serverCert, serverKey := testCertificate(t, "core.example")
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 32)
	for i := range token {
		token[i] = byte(i)
	}
	encoded := base64.RawURLEncoding.EncodeToString(token)
	if err := os.WriteFile(tokenPath, []byte(encoded), 0600); err != nil {
		t.Fatal(err)
	}

	var seenMu sync.Mutex
	var seenAuthorization string
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, fakeAgentService{instanceID: "00000000-0000-4000-8000-000000000001", seen: func(md metadata.MD) { seenMu.Lock(); seenAuthorization = fmt.Sprint(md); seenMu.Unlock() }})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	go server.Serve(tlsListener)
	defer server.Stop()

	cfg := completeConfig()
	cfg.Address, cfg.CAFile, cfg.TokenFile = listener.Addr().String(), caPath, tokenPath
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	seenMu.Lock()
	got := seenAuthorization
	seenMu.Unlock()
	if want := "DTX-Agent-Token " + encoded; !strings.Contains(got, want) {
		t.Fatalf("authorization = %q, want %q", got, want)
	}
	if got := client.Snapshot().Status; got != StatusReady {
		t.Fatalf("status = %q, want ready with conversation adapter", got)
	}
}

func TestProbeSingleFlight(t *testing.T) {
	dir := t.TempDir()
	ca, cert, key := testCertificate(t, "core.example")
	caPath, tokenPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0600); err != nil {
		t.Fatal(err)
	}
	_ = cert
	_ = key
	cfg := completeConfig()
	cfg.CAFile, cfg.TokenFile = caPath, tokenPath
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	client.probeOverride = func(context.Context) (Snapshot, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return Snapshot{}, errIncompatible
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = client.Probe(context.Background()) }()
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("probe calls = %d, want single flight", got)
	}
	close(release)
	wg.Wait()
	if err := client.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("fresh probe calls = %d, want cached result", calls)
	}
}

func TestStaleProbeSuccessCannotPublish(t *testing.T) {
	dir := t.TempDir()
	ca, _, _ := testCertificate(t, "core.example")
	caPath, tokenPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := completeConfig()
	cfg.CAFile, cfg.TokenFile = caPath, tokenPath
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.probeOverride = func(context.Context) (Snapshot, error) {
		client.probeMu.Lock()
		client.probeGeneration++
		client.probeMu.Unlock()
		return Snapshot{Configured: true, Status: StatusReady, InstanceID: cfg.ExpectedInstanceID, APIVersion: "v1", Capabilities: []string{"agent.info"}}, nil
	}
	if err := client.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := client.Snapshot().Status; got != StatusUnavailable {
		t.Fatalf("stale probe published status %q", got)
	}
}

type fakeAgentService struct {
	agentv1.UnimplementedAgentServiceServer
	instanceID string
	seen       func(metadata.MD)
}

func (f fakeAgentService) GetInstanceInfo(ctx context.Context, _ *agentv1.GetInstanceInfoRequest) (*agentv1.GetInstanceInfoResponse, error) {
	if md, _ := metadata.FromIncomingContext(ctx); f.seen != nil {
		f.seen(md)
	}
	return &agentv1.GetInstanceInfoResponse{InstanceId: f.instanceID, ApiVersion: "v1"}, nil
}
func (f fakeAgentService) GetCapabilities(ctx context.Context, _ *agentv1.GetCapabilitiesRequest) (*agentv1.GetCapabilitiesResponse, error) {
	if md, _ := metadata.FromIncomingContext(ctx); f.seen != nil {
		f.seen(md)
	}
	return &agentv1.GetCapabilitiesResponse{ApiVersion: "v1", Capabilities: []*agentv1.AgentCapability{{Name: "agent.info", Enabled: true}, {Name: "model.profile", Enabled: true}, {Name: "conversation", Enabled: true}}}, nil
}

func testCertificate(t *testing.T, host string) ([]byte, []byte, []byte) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
}

func TestDisabledConfigIsNotConfigured(t *testing.T) {
	client, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Snapshot(); got.Status != StatusNotConfigured || got.Configured {
		t.Fatalf("snapshot = %#v, want not_configured disabled", got)
	}
	result, actionErr := client.Handlers()["agent.backends.get"](context.Background(), nil)
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	core := result.(map[string]any)["core"].(map[string]any)
	for _, forbidden := range []string{"address", "ca_file", "token_file", "error"} {
		if _, ok := core[forbidden]; ok {
			t.Fatalf("sanitized response contains %q: %#v", forbidden, core)
		}
	}
}

func TestSafeInstanceIDRedactsUnexpectedCharacters(t *testing.T) {
	if got := safeInstanceID("core/secret"); got != "redacted-safe-id" {
		t.Fatalf("safeInstanceID() = %q", got)
	}
	if got := safeInstanceID("core-1"); got != "core-1" {
		t.Fatalf("safeInstanceID() = %q, want validated id", got)
	}
}
