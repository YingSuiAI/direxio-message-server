package agentgateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
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

type blockingWatchClient struct {
	grpc.ClientStream
	done      chan struct{}
	closeOnce sync.Once
}

type pacedWatchClient struct {
	grpc.ClientStream
	delays   []time.Duration
	sequence int64
}

func (c *pacedWatchClient) CloseSend() error { return nil }

func (c *pacedWatchClient) Recv() (*capv1.WatchOperationEvent, error) {
	if len(c.delays) == 0 {
		return nil, errors.New("no scripted event")
	}
	delay := c.delays[0]
	c.delays = c.delays[1:]
	time.Sleep(delay)
	c.sequence++
	return &capv1.WatchOperationEvent{
		OperationId: "operation", Sequence: c.sequence,
		Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: []byte(`{"kind":"working"}`)}},
	}, nil
}

func (c *blockingWatchClient) CloseSend() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

func (c *blockingWatchClient) Recv() (*capv1.WatchOperationEvent, error) {
	<-c.done
	return nil, context.Canceled
}

type delayedWatchServer struct {
	capv1.UnimplementedAgentCapabilityServiceServer
	delay   time.Duration
	request chan *capv1.WatchOperationRequest
}

func (s *delayedWatchServer) WatchOperation(req *capv1.WatchOperationRequest, stream capv1.AgentCapabilityService_WatchOperationServer) error {
	s.request <- req
	select {
	case <-time.After(s.delay):
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	return stream.Send(&capv1.WatchOperationEvent{
		OperationId: req.GetOperationId(), Sequence: req.GetAfterSequence() + 1,
		Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: []byte(`{"kind":"working"}`)}},
	})
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

func TestCreateCallContextUsesBoundedAdmissionBudget(t *testing.T) {
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

func TestRefreshCallContextRenewsAdmissionWithoutForkingOperationTrace(t *testing.T) {
	fixedNow := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	client := &Client{now: func() time.Time { return fixedNow }}
	parent := &capv1.CallContext{
		ChainId: uuid.NewString(), RootOperationId: uuid.NewString(), ParentCallId: uuid.NewString(),
		Hop: 1, Route: capv1.NodeMessage, DeadlineUnixMs: fixedNow.Add(-time.Second).UnixMilli(),
	}

	refreshed := client.refreshCallContext(parent)

	if refreshed.GetChainId() != parent.GetChainId() || refreshed.GetRootOperationId() != parent.GetRootOperationId() ||
		refreshed.GetParentCallId() != parent.GetParentCallId() || refreshed.GetHop() != parent.GetHop() || refreshed.GetRoute() != parent.GetRoute() {
		t.Fatalf("refreshed call context forked operation trace: parent=%#v refreshed=%#v", parent, refreshed)
	}
	if got, want := refreshed.GetDeadlineUnixMs(), fixedNow.Add(agentAdmissionCallBudget).UnixMilli(); got != want {
		t.Fatalf("refreshed admission deadline = %d, want %d", got, want)
	}
	if parent.GetDeadlineUnixMs() == refreshed.GetDeadlineUnixMs() {
		t.Fatal("refreshed call context reused expired admission deadline")
	}
	if err := capv1.ValidateStrictCallContext(refreshed); err != nil {
		t.Fatalf("refreshed call context is invalid: %v", err)
	}
}

func TestWatchOperationOutlivesAdmissionDeadline(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	service := &delayedWatchServer{delay: 220 * time.Millisecond, request: make(chan *capv1.WatchOperationRequest, 1)}
	capv1.RegisterAgentCapabilityServiceServer(grpcServer, service)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient(
		"passthrough:///agent-watch-admission-test",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("create gRPC client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	token, err := capv1.EncodeCapabilityToken(bytes.Repeat([]byte{0x42}, capv1.CapabilityTokenBytes))
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{
		config: &Config{InstanceID: uuid.NewString(), AccountGeneration: 1, WatchIdleTimeout: time.Second},
		token:  []byte(token), client: capv1.NewAgentCapabilityServiceClient(conn),
		watchSem: make(chan struct{}, 1), watchIdleTimeout: time.Second,
	}
	operationID := uuid.NewString()
	callCtx := client.createCallContext(operationID)
	callCtx.DeadlineUnixMs = time.Now().Add(100 * time.Millisecond).UnixMilli()

	stream, err := client.WatchOperation(context.Background(), operationID, 7, &capv1.PermissionContext{}, callCtx)
	if err != nil {
		t.Fatalf("start WatchOperation: %v", err)
	}
	defer stream.CloseSend()
	request := <-service.request
	admissionDeadline := time.UnixMilli(request.GetCallContext().GetDeadlineUnixMs())
	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive event after admission deadline: %v", err)
	}
	if !time.Now().After(admissionDeadline) {
		t.Fatalf("watch event arrived before admission deadline %s", admissionDeadline)
	}
	if event.GetSequence() != 8 || event.GetProgress() == nil {
		t.Fatalf("watch event = %#v, want progress sequence 8", event)
	}
}

func TestWatchStreamIdleTimeoutCancelsOnlyAttachmentAndReleasesAdmission(t *testing.T) {
	transport := &blockingWatchClient{done: make(chan struct{})}
	releaseCalls := 0
	stream := &watchStream{
		AgentCapabilityService_WatchOperationClient: transport,
		cancel: func() {}, release: func() { releaseCalls++ }, idleTimeout: 20 * time.Millisecond,
	}

	event, err := stream.Recv()
	if event != nil || !errors.Is(err, ErrWatchIdleTimeout) {
		t.Fatalf("idle watch result = %#v, %v", event, err)
	}
	if releaseCalls != 1 {
		t.Fatalf("admission release calls = %d, want 1", releaseCalls)
	}
	select {
	case <-transport.done:
	default:
		t.Fatal("idle watch did not cancel its transport attachment")
	}
}

func TestWatchStreamActivityResetsIdleTimeoutWithoutTotalDeadline(t *testing.T) {
	transport := &pacedWatchClient{delays: []time.Duration{160 * time.Millisecond, 160 * time.Millisecond}}
	stream := &watchStream{
		AgentCapabilityService_WatchOperationClient: transport,
		cancel: func() {}, release: func() {}, idleTimeout: 300 * time.Millisecond,
	}
	started := time.Now()

	first, err := stream.Recv()
	if err != nil || first.GetSequence() != 1 {
		t.Fatalf("first progress = %#v, %v", first, err)
	}
	second, err := stream.Recv()
	if err != nil || second.GetSequence() != 2 {
		t.Fatalf("second progress = %#v, %v", second, err)
	}
	if elapsed := time.Since(started); elapsed <= stream.idleTimeout {
		t.Fatalf("watch completed within one idle interval: %s", elapsed)
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
