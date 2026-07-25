package agentcore

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type conversationFake struct {
	agentv1.UnimplementedConversationServiceServer
	mu                    sync.Mutex
	starts, gets, cancels int
	authValues            []string
	malformedWatch        int
}

func (f *conversationFake) recordAuth(ctx context.Context) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		f.mu.Lock()
		f.authValues = append(f.authValues, md.Get("authorization")...)
		f.mu.Unlock()
	}
}
func (f *conversationFake) StartTurn(ctx context.Context, r *agentv1.ConversationServiceStartTurnRequest) (*agentv1.ConversationServiceStartTurnResponse, error) {
	f.recordAuth(ctx)
	f.mu.Lock()
	f.starts++
	f.mu.Unlock()
	return &agentv1.ConversationServiceStartTurnResponse{Turn: &agentv1.CoreConversationTurn{TurnId: "core-" + r.GetIdempotencyKey(), ConversationId: r.GetConversationId(), State: "accepted", LastSequence: 1}}, nil
}
func (f *conversationFake) GetTurn(ctx context.Context, r *agentv1.ConversationServiceGetTurnRequest) (*agentv1.ConversationServiceGetTurnResponse, error) {
	f.recordAuth(ctx)
	f.mu.Lock()
	f.gets++
	f.mu.Unlock()
	return &agentv1.ConversationServiceGetTurnResponse{Turn: &agentv1.CoreConversationTurn{TurnId: r.GetTurnId(), ConversationId: "conv", State: "done", Revision: 1, LastSequence: 3, UpdatedAt: timestamppb.Now()}}, nil
}
func (f *conversationFake) CancelTurn(ctx context.Context, r *agentv1.ConversationServiceCancelTurnRequest) (*agentv1.ConversationServiceCancelTurnResponse, error) {
	f.recordAuth(ctx)
	f.mu.Lock()
	f.cancels++
	f.mu.Unlock()
	return &agentv1.ConversationServiceCancelTurnResponse{Turn: &agentv1.CoreConversationTurn{TurnId: r.GetTurnId(), ConversationId: "conv", State: "canceled", LastSequence: 2}}, nil
}

func (f *conversationFake) StreamEvents(ctx context.Context, r *agentv1.ConversationServiceWatchTurnEventsRequest, send func(*agentv1.ConversationServiceWatchTurnEventsResponse) error) error {
	f.recordAuth(ctx)
	f.mu.Lock()
	malformed := f.malformedWatch
	f.mu.Unlock()
	if malformed > 0 {
		event := &agentv1.CoreConversationTurnEvent{TurnId: r.GetTurnId(), Sequence: 2, Kind: "delta"}
		if malformed == 1 {
			event.TurnId = "another-turn"
		} else if malformed == 2 {
			event.Sequence = r.GetAfterSequence()
		} else {
			event.Kind = "unknown"
		}
		return send(&agentv1.ConversationServiceWatchTurnEventsResponse{Event: event})
	}
	if r.GetAfterSequence() == 999 {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.GetAfterSequence() == 998 {
		for i := int64(1); i <= 16; i++ {
			if err := send(&agentv1.ConversationServiceWatchTurnEventsResponse{Event: &agentv1.CoreConversationTurnEvent{TurnId: r.GetTurnId(), Sequence: 998 + i, Kind: "delta"}}); err != nil {
				return err
			}
		}
		return io.EOF
	}
	for _, n := range []int64{2, 3} {
		if n <= r.GetAfterSequence() {
			continue
		}
		if err := send(&agentv1.ConversationServiceWatchTurnEventsResponse{Event: &agentv1.CoreConversationTurnEvent{TurnId: r.GetTurnId(), Sequence: n, Kind: map[int64]string{2: "delta", 3: "done"}[n], Text: map[int64]string{2: "hello", 3: ""}[n]}}); err != nil {
			return err
		}
	}
	return nil
}

// grpc's generated server interface is generic; adapt the fake with a small
// forwarding implementation so the stream contract remains wire-tested.
type conversationServer struct{ *conversationFake }

func (s conversationServer) WatchTurnEvents(r *agentv1.ConversationServiceWatchTurnEventsRequest, stream agentv1.ConversationService_WatchTurnEventsServer) error {
	return s.StreamEvents(stream.Context(), r, stream.Send)
}

func TestConversationTLSRPCsUseExactAuthAndOrderedResume(t *testing.T) {
	ca, certPEM, keyPEM := testCertificate(t, "core.example")
	dir := t.TempDir()
	caPath, tokenPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	fake := &conversationFake{}
	gs := grpc.NewServer()
	agentv1.RegisterConversationServiceServer(gs, conversationServer{fake})
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	go gs.Serve(tlsLn)
	defer gs.Stop()
	cfg := completeConfig()
	cfg.Address = ln.Addr().String()
	cfg.CAFile = caPath
	cfg.TokenFile = tokenPath
	cfg.StreamIdleTimeout = 20 * time.Millisecond
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := client.StartTurn(context.Background(), TurnStart{ClientTurnID: "client-1", ConversationID: "conv", Message: "prompt", ModelProfileID: "profile"})
	if err != nil || turn.CoreTurnID != "core-client-1" {
		t.Fatalf("start=%#v err=%v", turn, err)
	}
	events, err := client.WatchTurnEvents(context.Background(), turn.CoreTurnID, 1)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for ev := range events {
		got = append(got, ev.Sequence)
	}
	if len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 0 {
		t.Fatalf("events=%v", got)
	}
	if _, err := client.CancelTurn(context.Background(), turn.CoreTurnID, "cancel-1", 0); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	authValues := append([]string(nil), fake.authValues...)
	starts, cancels := fake.starts, fake.cancels
	fake.mu.Unlock()
	if len(authValues) == 0 || authValues[0] != "DTX-Agent-Token "+token {
		t.Fatalf("auth=%v", authValues)
	}
	if starts != 1 || cancels != 1 {
		t.Fatalf("calls starts=%d cancels=%d", starts, cancels)
	}
	idle, err := client.WatchTurnEvents(context.Background(), "core-client-1", 999)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev, ok := <-idle:
		if !ok || ev.ErrorCode != "agent_core_stream_idle" {
			t.Fatalf("idle stream result=%#v open=%v", ev, ok)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle stream did not converge")
	}
	saturated, err := client.WatchTurnEvents(context.Background(), "core-client-1", 998)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		if ev := <-saturated; ev.Sequence != int64(999+i) {
			t.Fatalf("buffer event=%#v", ev)
		}
	}
	if ev := <-saturated; (ev.ErrorCode != "agent_core_stream_ended" && ev.ErrorCode != "agent_core_stream_failed") || ev.Err == nil {
		t.Fatalf("buffer terminal=%#v", ev)
	}
	for malformed := 1; malformed <= 3; malformed++ {
		fake.mu.Lock()
		fake.malformedWatch = malformed
		fake.mu.Unlock()
		bad, err := client.WatchTurnEvents(context.Background(), "core-client-1", 1)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case ev := <-bad:
			if ev.ErrorCode != "agent_core_stream_failed" {
				t.Fatalf("malformed watch %d result=%#v", malformed, ev)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("malformed watch %d did not converge", malformed)
		}
	}
}
