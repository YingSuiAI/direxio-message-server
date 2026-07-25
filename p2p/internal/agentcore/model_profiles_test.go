package agentcore

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type modelProfileFake struct {
	agentv1.UnimplementedModelProfileServiceServer
	syncRequest *agentv1.ModelProfileServiceSyncRequest
	syncCalls   int
	syncError   error
	getError    error
}

func (f *modelProfileFake) Sync(_ context.Context, request *agentv1.ModelProfileServiceSyncRequest) (*agentv1.ModelProfileServiceSyncResponse, error) {
	f.syncCalls++
	f.syncRequest = request
	if f.syncError != nil {
		return nil, f.syncError
	}
	return &agentv1.ModelProfileServiceSyncResponse{
		DefaultClientProfileId: request.GetDefaultClientProfileId(),
		Profiles:               []*agentv1.CoreModelProfile{{ProfileId: "core-profile-1", ClientProfileId: request.GetEntries()[0].GetClientProfileId(), DisplayName: "safe", Provider: agentv1.CoreModelProvider_CORE_MODEL_PROVIDER_OPENAI_COMPATIBLE, ApiKeyConfigured: true, Revision: 4, CreatedAt: timestamppb.New(time.Unix(10, 0).UTC())}},
	}, nil
}

func (f *modelProfileFake) List(context.Context, *agentv1.ModelProfileServiceListRequest) (*agentv1.ModelProfileServiceListResponse, error) {
	return &agentv1.ModelProfileServiceListResponse{}, nil
}
func (f *modelProfileFake) Get(context.Context, *agentv1.ModelProfileServiceGetRequest) (*agentv1.ModelProfileServiceGetResponse, error) {
	if f.getError != nil {
		return nil, f.getError
	}
	return &agentv1.ModelProfileServiceGetResponse{Profile: &agentv1.CoreModelProfile{ProfileId: "profile-1"}}, nil
}
func (f *modelProfileFake) Delete(context.Context, *agentv1.ModelProfileServiceDeleteRequest) (*agentv1.ModelProfileServiceDeleteResponse, error) {
	return &agentv1.ModelProfileServiceDeleteResponse{}, nil
}

func TestModelProfileSyncMapsOptionalAPIKeyAndRedactsResponse(t *testing.T) {
	caPEM, certPEM, keyPEM := testCertificate(t, "core.example")
	dir := t.TempDir()
	caPath, tokenPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &modelProfileFake{}
	server := grpc.NewServer()
	agentv1.RegisterModelProfileServiceServer(server, fake)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
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
	handler := client.Handlers()["agent.core.model_profiles.sync"]
	result, actionErr := handler(context.Background(), map[string]any{
		"idempotency_key": "batch-1", "default_client_profile_id": "client-1",
		"entries": []any{map[string]any{"client_profile_id": "client-1", "provider": "openai", "model": "gpt", "api_key": "super-secret-key"}},
	})
	if actionErr != nil {
		t.Fatalf("sync error: %#v", actionErr)
	}
	if fake.syncRequest == nil || len(fake.syncRequest.GetEntries()) != 1 {
		t.Fatalf("sync request = %#v", fake.syncRequest)
	}
	if got := fake.syncRequest.GetEntries()[0].GetApiKey(); got != "super-secret-key" {
		t.Fatalf("wire api key = %q", got)
	}
	if got := fake.syncRequest.GetEntries()[0].GetClientProfileId(); got != "client-1" {
		t.Fatalf("wire client profile id = %q", got)
	}
	if got := fake.syncRequest.GetDefaultClientProfileId(); got != "client-1" {
		t.Fatalf("wire default client profile id = %q", got)
	}
	resultText := strings.TrimSpace(string(mustJSON(t, result)))
	if strings.Contains(resultText, "super-secret-key") {
		t.Fatalf("sync response leaked api key: %s", resultText)
	}
	if !strings.Contains(resultText, "api_key_configured") {
		t.Fatalf("sync response omitted configured marker: %s", resultText)
	}

	_, actionErr = handler(context.Background(), map[string]any{"idempotency_key": "batch-2", "entries": []any{map[string]any{"client_profile_id": "client-2", "provider": "anthropic"}}})
	if actionErr != nil {
		t.Fatalf("sync without key error: %#v", actionErr)
	}
	if fake.syncRequest.GetEntries()[0].ApiKey != nil {
		t.Fatal("omitted api_key must remain absent on the wire")
	}
	if fake.syncCalls != 2 {
		t.Fatalf("sync calls after omitted key = %d, want 2", fake.syncCalls)
	}
	_, actionErr = handler(context.Background(), map[string]any{"idempotency_key": "batch-3", "entries": []any{map[string]any{"client_profile_id": "client-3", "provider": "gemini", "api_key": ""}}})
	if actionErr == nil || actionErr.Status != 400 || !strings.Contains(actionErr.Error, "api_key must be non-empty") {
		t.Fatalf("present empty api_key error = %#v", actionErr)
	}
	if fake.syncCalls != 2 {
		t.Fatalf("present empty api_key reached Core: calls=%d", fake.syncCalls)
	}
	_, actionErr = handler(context.Background(), map[string]any{"idempotency_key": "batch-4", "default_client_profile_id": " client-4", "entries": []any{map[string]any{"client_profile_id": "client-4", "provider": "gemini"}}})
	if actionErr == nil || actionErr.Status != 400 || !strings.Contains(actionErr.Error, "surrounding whitespace") {
		t.Fatalf("non-canonical profile ref error = %#v", actionErr)
	}
	if fake.syncCalls != 2 {
		t.Fatalf("non-canonical profile ref reached Core: calls=%d", fake.syncCalls)
	}
}

func TestModelProfileErrorMappingIsStableAndSanitized(t *testing.T) {
	fake := &modelProfileFake{getError: status.Error(codes.Aborted, "upstream secret=do-not-forward")}
	client := newModelTestClient(t, fake)
	result, actionErr := client.Handlers()["agent.core.model_profiles.get"](context.Background(), map[string]any{"profile_id": "p"})
	if result != nil || actionErr == nil {
		t.Fatalf("get result=%#v err=%#v", result, actionErr)
	}
	if actionErr.Status != 409 || actionErr.Code != "agent_core_conflict" || actionErr.Error != "agent core model profile revision conflict" {
		t.Fatalf("unstable mapped error: %#v", actionErr)
	}
	if strings.Contains(actionErr.Error, "do-not-forward") {
		t.Fatal("upstream error text leaked")
	}
}

func newModelTestClient(t *testing.T, fake *modelProfileFake) *Client {
	t.Helper()
	caPEM, certPEM, keyPEM := testCertificate(t, "core.example")
	dir := t.TempDir()
	caPath, tokenPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0600); err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentv1.RegisterModelProfileServiceServer(server, fake)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	go server.Serve(tlsListener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	cfg := completeConfig()
	cfg.Address, cfg.CAFile, cfg.TokenFile = listener.Addr().String(), caPath, tokenPath
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
