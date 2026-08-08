package agentgateway

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type closingWatchClient struct {
	grpc.ClientStream
	closeCalls int
	closeErr   error
}

func (c *closingWatchClient) CloseSend() error {
	c.closeCalls++
	return c.closeErr
}

func (c *closingWatchClient) Recv() (*capv1.WatchOperationEvent, error) {
	return nil, errors.New("unused")
}

type terminalWatchServer struct {
	capv1.UnimplementedAgentCapabilityServiceServer
	cancelled chan struct{}
}

func (s *terminalWatchServer) WatchOperation(_ *capv1.WatchOperationRequest, stream capv1.AgentCapabilityService_WatchOperationServer) error {
	if err := stream.Send(&capv1.WatchOperationEvent{
		OperationId: "operation",
		Sequence:    1,
		Event: &capv1.WatchOperationEvent_Result{
			Result: &capv1.ResultEvent{ResultJson: []byte(`{"status":"done"}`)},
		},
	}); err != nil {
		return err
	}
	<-stream.Context().Done()
	close(s.cancelled)
	return stream.Context().Err()
}

func TestCreateCallContextUsesBoundedTwoMinuteBudget(t *testing.T) {
	fixedNow := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	rootOperationID := uuid.NewString()
	client := &Client{now: func() time.Time { return fixedNow }}

	callCtx := client.createCallContext(rootOperationID)

	if got, want := callCtx.GetDeadlineUnixMs(), fixedNow.Add(2*time.Minute).UnixMilli(); got != want {
		t.Fatalf("call deadline = %d, want %d", got, want)
	}
	if got := callCtx.GetRootOperationId(); got != rootOperationID {
		t.Fatalf("root operation id = %q, want %q", got, rootOperationID)
	}
	if got := callCtx.GetRoute(); got != capv1.NodeMessage {
		t.Fatalf("call route = %q, want %q", got, capv1.NodeMessage)
	}
	if got := callCtx.GetHop(); got != 1 {
		t.Fatalf("call hop = %d, want 1", got)
	}
	if err := capv1.ValidateStrictCallContext(callCtx); err != nil {
		t.Fatalf("created call context is invalid: %v", err)
	}
}

func TestWatchStreamCloseCancelsTransportAndReleasesAdmissionOnce(t *testing.T) {
	streamCtx, cancel := context.WithCancel(context.Background())
	transport := &closingWatchClient{closeErr: errors.New("closed")}
	releaseCalls := 0
	stream := &watchStream{
		AgentCapabilityService_WatchOperationClient: transport,
		cancel:  cancel,
		release: func() { releaseCalls++ },
	}

	if err := stream.CloseSend(); err == nil || err.Error() != "closed" {
		t.Fatalf("CloseSend error = %v, want closed", err)
	}
	stream.Close()

	if transport.closeCalls != 1 {
		t.Fatalf("transport CloseSend calls = %d, want 1", transport.closeCalls)
	}
	if releaseCalls != 1 {
		t.Fatalf("admission release calls = %d, want 1", releaseCalls)
	}
	if streamCtx.Err() != context.Canceled {
		t.Fatalf("stream context error = %v, want canceled", streamCtx.Err())
	}
}

func TestWatchStreamCloseCancelsLiveGRPCStreamAfterTerminalEvent(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	service := &terminalWatchServer{cancelled: make(chan struct{})}
	capv1.RegisterAgentCapabilityServiceServer(grpcServer, service)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient(
		"passthrough:///agent-watch-test",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("create gRPC client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	streamCtx, cancel := context.WithCancel(context.Background())
	transport, err := capv1.NewAgentCapabilityServiceClient(conn).WatchOperation(streamCtx, &capv1.WatchOperationRequest{OperationId: "operation"})
	if err != nil {
		t.Fatalf("start watch: %v", err)
	}
	if event, recvErr := transport.Recv(); recvErr != nil || event.GetResult() == nil {
		t.Fatalf("terminal event = %#v, error = %v", event, recvErr)
	}

	releaseCalls := 0
	stream := &watchStream{
		AgentCapabilityService_WatchOperationClient: transport,
		cancel:  cancel,
		release: func() { releaseCalls++ },
	}
	stream.Close()
	stream.Close()

	select {
	case <-service.cancelled:
	case <-time.After(time.Second):
		t.Fatal("server-side gRPC stream did not observe client cancellation")
	}
	if releaseCalls != 1 {
		t.Fatalf("admission release calls = %d, want 1", releaseCalls)
	}
}
