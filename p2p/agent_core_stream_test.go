package p2p

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentcoremodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcore"
	coreturns "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcoreturns"
	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	realtimewsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/realtimews"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type coreWireFake struct {
	agentv1.UnimplementedConversationServiceServer
	mu                    sync.Mutex
	starts, gets, cancels int
	block, gap            bool
	startProfileID        string
	failNextStart         bool
	invalidGet            bool
	completed             bool
}
type modelWireFake struct {
	agentv1.UnimplementedModelProfileServiceServer
}

const (
	testConversationID = "00000000-0000-4000-8000-000000000021"
	zeroTurnID         = "00000000-0000-4000-8000-000000000030"
	retryTurnID        = "00000000-0000-4000-8000-000000000031"
	mainTurnID         = "00000000-0000-4000-8000-000000000032"
	disconnectTurnID   = "00000000-0000-4000-8000-000000000033"
	gapTurnID          = "00000000-0000-4000-8000-000000000034"
)

func validTestUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func testCoreTurnID(clientTurnID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("agent-core-test/"+clientTurnID)).String()
}

func coreStreamFrame(turnID string, params map[string]any, after ...int64) map[string]any {
	return coreStreamFrameWithDigest(turnID, params, hex.EncodeToString(coreturns.DigestParams(params)), after...)
}

func coreStreamFrameWithDigest(turnID string, params map[string]any, digest string, after ...int64) map[string]any {
	frame := map[string]any{
		"type":           "client.agent_core_stream",
		"turn_id":        turnID,
		"request_digest": digest,
		"params":         params,
	}
	if len(after) > 0 {
		frame["after_seq"] = after[0]
	}
	return frame
}

func (modelWireFake) List(_ context.Context, r *agentv1.ModelProfileServiceListRequest) (*agentv1.ModelProfileServiceListResponse, error) {
	if r.GetPageToken() != "" {
		return &agentv1.ModelProfileServiceListResponse{}, nil
	}
	return &agentv1.ModelProfileServiceListResponse{Profiles: []*agentv1.CoreModelProfile{{ProfileId: "00000000-0000-4000-8000-000000000002", ClientProfileId: "00000000-0000-4000-8000-000000000011", Revision: 7}}}, nil
}

func (f *coreWireFake) StartTurn(_ context.Context, r *agentv1.ConversationServiceStartTurnRequest) (*agentv1.ConversationServiceStartTurnResponse, error) {
	if !validTestUUID(r.GetIdempotencyKey()) || !validTestUUID(r.GetConversationId()) || !validTestUUID(r.GetModelProfileId()) {
		return nil, status.Error(codes.InvalidArgument, "turn, conversation, and profile IDs must be canonical UUIDs")
	}
	if r.ExpectedRevision != nil && r.GetExpectedRevision() == 0 {
		return nil, status.Error(codes.InvalidArgument, "zero expected revision must be omitted")
	}
	f.mu.Lock()
	f.starts++
	f.startProfileID = r.GetModelProfileId()
	if f.failNextStart {
		f.failNextStart = false
		f.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "injected start failure")
	}
	f.mu.Unlock()
	return &agentv1.ConversationServiceStartTurnResponse{Turn: &agentv1.CoreConversationTurn{TurnId: testCoreTurnID(r.GetIdempotencyKey()), ConversationId: r.GetConversationId(), State: "accepted", Revision: 1, LastSequence: 1}}, nil
}

func TestAgentCoreReserveReplayRetainsResolvedProfileAfterStartFailure(t *testing.T) {
	fake, cfg := startCoreWireFake(t)
	fake.failNextStart = true
	service := NewService(Config{ServerName: "example.com", AgentCore: cfg})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, conn)
	params := map[string]any{"conversation_id": testConversationID, "message": "retry", "client_model_profile_id": "00000000-0000-4000-8000-000000000011"}
	writeRealtimeFrame(t, conn, coreStreamFrame(retryTurnID, params))
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_unavailable" {
		t.Fatalf("first start failure=%#v", got)
	}
	retryDigest := hex.EncodeToString(coreturns.DigestParams(params))
	writeRealtimeFrame(t, conn, coreStreamFrameWithDigest(retryTurnID, map[string]any{}, retryDigest))
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_reconciliation_required" {
		t.Fatalf("ambiguous digest-only replay=%#v", got)
	}
	writeRealtimeFrame(t, conn, coreStreamFrame(retryTurnID, params))
	if got := readRealtimeFrame(t, conn); got["type"] != "server.agent_core_stream.accepted" {
		t.Fatalf("retry accepted=%#v", got)
	}
	_ = readRealtimeFrame(t, conn)
	_ = readRealtimeFrame(t, conn)
	fake.mu.Lock()
	starts, profileID := fake.starts, fake.startProfileID
	fake.mu.Unlock()
	if starts != 2 || profileID != "00000000-0000-4000-8000-000000000002" {
		t.Fatalf("retry start profile=%q starts=%d", profileID, starts)
	}
	record, err := service.coreTurns.Get(context.Background(), service.OwnerMXID(), retryTurnID)
	if err != nil || record.CoreProfileID != "00000000-0000-4000-8000-000000000002" || record.ModelProfileRevision != 7 {
		t.Fatalf("persisted profile=%#v err=%v", record, err)
	}
}

func TestAgentCoreRequestDigestRequiredAndCanonical(t *testing.T) {
	_, cfg := startCoreWireFake(t)
	service := NewService(Config{ServerName: "example.com", AgentCore: cfg})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, conn)
	params := map[string]any{"conversation_id": testConversationID, "message": "digest", "client_model_profile_id": "00000000-0000-4000-8000-000000000011"}
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.agent_core_stream", "turn_id": zeroTurnID, "params": params})
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_digest_required" {
		t.Fatalf("missing digest=%#v", got)
	}
	writeRealtimeFrame(t, conn, coreStreamFrameWithDigest(zeroTurnID, params, strings.Repeat("A", 64)))
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_invalid_argument" {
		t.Fatalf("uppercase digest=%#v", got)
	}
	writeRealtimeFrame(t, conn, coreStreamFrameWithDigest(zeroTurnID, map[string]any{}, hex.EncodeToString(coreturns.DigestParams(params))))
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_digest_required" {
		t.Fatalf("empty initial request=%#v", got)
	}
	for _, invalidAfter := range []any{"2", 1.5} {
		frame := coreStreamFrame(zeroTurnID, params)
		frame["after_sequence"] = invalidAfter
		writeRealtimeFrame(t, conn, frame)
		if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_invalid_argument" {
			t.Fatalf("invalid after_sequence=%#v frame=%#v", invalidAfter, got)
		}
	}
	bothAliases := coreStreamFrame(zeroTurnID, params)
	bothAliases["after_seq"] = float64(0)
	bothAliases["after_sequence"] = float64(1)
	writeRealtimeFrame(t, conn, bothAliases)
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_invalid_argument" {
		t.Fatalf("conflicting replay aliases=%#v", got)
	}
	spacedDigest := coreStreamFrame(zeroTurnID, params)
	spacedDigest["request_digest"] = " " + spacedDigest["request_digest"].(string)
	writeRealtimeFrame(t, conn, spacedDigest)
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_invalid_argument" {
		t.Fatalf("trimmed request digest=%#v", got)
	}
	if _, err := service.coreTurns.Get(context.Background(), service.OwnerMXID(), zeroTurnID); !errors.Is(err, coreturns.ErrNotFound) {
		t.Fatalf("invalid requests persisted ledger row: %v", err)
	}
	if _, _, err := service.coreTurns.Reserve(context.Background(), coreturns.Record{OwnerID: service.OwnerMXID(), ClientTurnID: zeroTurnID, RequestDigest: coreturns.Digest("reserved")}); err != nil {
		t.Fatal(err)
	}
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.agent_core_stream.cancel", "turn_id": zeroTurnID})
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_reconciliation_required" {
		t.Fatalf("reserved cancel=%#v", got)
	}
}

func TestAgentCoreCancelReservedWithoutCoreTurnRequiresReconciliation(t *testing.T) {
	ledger := coreturns.New(nil)
	if _, _, err := ledger.Reserve(context.Background(), coreturns.Record{OwnerID: "owner", ClientTurnID: retryTurnID, RequestDigest: coreturns.Digest("request")}); err != nil {
		t.Fatal(err)
	}
	adapter := &agentCoreStreamAdapter{ledger: ledger}
	if err := adapter.CancelCoreStream(context.Background(), "owner", retryTurnID); !errors.Is(err, coreturns.ErrReconciliationRequired) {
		t.Fatalf("cancel reserved turn err=%v", err)
	}
}
func (f *coreWireFake) GetTurn(_ context.Context, r *agentv1.ConversationServiceGetTurnRequest) (*agentv1.ConversationServiceGetTurnResponse, error) {
	if !validTestUUID(r.GetTurnId()) {
		return nil, status.Error(codes.InvalidArgument, "turn ID must be a canonical UUID")
	}
	f.mu.Lock()
	f.gets++
	invalidGet := f.invalidGet
	f.mu.Unlock()
	turnID := r.GetTurnId()
	if invalidGet {
		turnID = testCoreTurnID("malicious")
	}
	return &agentv1.ConversationServiceGetTurnResponse{Turn: &agentv1.CoreConversationTurn{TurnId: turnID, ConversationId: testConversationID, State: "completed", Revision: 3, LastSequence: 3, UpdatedAt: timestamppb.Now()}}, nil
}
func (f *coreWireFake) CancelTurn(_ context.Context, r *agentv1.ConversationServiceCancelTurnRequest) (*agentv1.ConversationServiceCancelTurnResponse, error) {
	if !validTestUUID(r.GetIdempotencyKey()) || !validTestUUID(r.GetTurnId()) {
		return nil, status.Error(codes.InvalidArgument, "cancel IDs must be canonical UUIDs")
	}
	f.mu.Lock()
	f.cancels++
	completed := f.completed
	f.mu.Unlock()
	expectedRevision := r.GetExpectedRevision()
	if (!completed && expectedRevision != 1) || (completed && expectedRevision != 3) {
		return nil, status.Error(codes.Aborted, "stale cancel revision")
	}
	state := "canceled"
	if completed {
		state = "completed"
	}
	revision, sequence := int64(2), int64(2)
	if completed {
		revision, sequence = 3, 3
	}
	return &agentv1.ConversationServiceCancelTurnResponse{Turn: &agentv1.CoreConversationTurn{TurnId: r.GetTurnId(), ConversationId: testConversationID, State: state, Revision: revision, LastSequence: sequence}}, nil
}
func (f *coreWireFake) WatchTurnEvents(r *agentv1.ConversationServiceWatchTurnEventsRequest, stream agentv1.ConversationService_WatchTurnEventsServer) error {
	if !validTestUUID(r.GetTurnId()) {
		return status.Error(codes.InvalidArgument, "turn ID must be a canonical UUID")
	}
	ctx := stream.Context()
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.gap {
		return stream.Send(&agentv1.ConversationServiceWatchTurnEventsResponse{Event: &agentv1.CoreConversationTurnEvent{TurnId: r.GetTurnId(), ReplayGap: true, FirstSequence: 4, LastSequence: 8}})
	}
	for _, ev := range []struct {
		seq        int64
		kind, text string
	}{{2, "delta", "hello"}, {3, "completed", ""}} {
		if ev.seq <= r.GetAfterSequence() {
			continue
		}
		if err := stream.Send(&agentv1.ConversationServiceWatchTurnEventsResponse{Event: &agentv1.CoreConversationTurnEvent{TurnId: r.GetTurnId(), Sequence: ev.seq, Kind: ev.kind, Text: ev.text}}); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.completed = true
	f.mu.Unlock()
	return nil
}

func TestAgentCoreRealtimeLedgerReplayMismatchCancelAndMemberBoundary(t *testing.T) {
	fake, cfg := startCoreWireFake(t)
	service := NewService(Config{ServerName: "example.com", AgentCore: cfg})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, conn)
	params := map[string]any{"conversation_id": testConversationID, "message": "hello", "client_model_profile_id": "00000000-0000-4000-8000-000000000011"}
	zeroRevision := map[string]any{"conversation_id": testConversationID, "message": "zero", "client_model_profile_id": "00000000-0000-4000-8000-000000000011", "expected_revision": 0}
	writeRealtimeFrame(t, conn, coreStreamFrame(zeroTurnID, zeroRevision))
	if got := readRealtimeFrame(t, conn); got["type"] != "server.agent_core_stream.accepted" {
		t.Fatalf("zero expected revision should be omitted: %#v", got)
	}
	_ = readRealtimeFrame(t, conn)
	_ = readRealtimeFrame(t, conn)
	writeRealtimeFrame(t, conn, coreStreamFrame(mainTurnID, params))
	accepted := readRealtimeFrame(t, conn)
	if accepted["type"] != "server.agent_core_stream.accepted" {
		t.Fatalf("accepted=%#v", accepted)
	}
	if ev := readRealtimeFrame(t, conn); ev["seq"] != float64(2) {
		t.Fatalf("delta=%#v", ev)
	}
	if ev := readRealtimeFrame(t, conn); ev["seq"] != float64(3) {
		t.Fatalf("done=%#v", ev)
	}
	mainDigest := hex.EncodeToString(coreturns.DigestParams(params))
	partialReplay := coreStreamFrameWithDigest(mainTurnID, map[string]any{"conversation_id": testConversationID}, mainDigest, 2)
	writeRealtimeFrame(t, conn, partialReplay)
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_digest_mismatch" {
		t.Fatalf("partial semantic replay=%#v", got)
	}
	replayFrame := coreStreamFrameWithDigest(mainTurnID, map[string]any{}, mainDigest, 2)
	delete(replayFrame, "after_seq")
	replayFrame["after_sequence"] = 2
	writeRealtimeFrame(t, conn, replayFrame)
	if got := readRealtimeFrame(t, conn); got["type"] != "server.agent_core_stream.accepted" {
		t.Fatalf("replay accepted=%#v", got)
	}
	if got := readRealtimeFrame(t, conn); got["seq"] != float64(3) {
		t.Fatalf("replay done=%#v", got)
	}
	fake.mu.Lock()
	starts, gets := fake.starts, fake.gets
	startProfileID := fake.startProfileID
	fake.mu.Unlock()
	if starts != 2 || gets == 0 || startProfileID != "00000000-0000-4000-8000-000000000002" {
		t.Fatalf("replay calls starts=%d gets=%d profile=%q", starts, gets, startProfileID)
	}
	if record, err := service.coreTurns.Get(context.Background(), service.OwnerMXID(), mainTurnID); err != nil || record.ModelProfileRevision != 7 {
		t.Fatalf("ledger profile revision=%d err=%v", record.ModelProfileRevision, err)
	}
	mismatch := map[string]any{"conversation_id": testConversationID, "message": "changed", "client_model_profile_id": "00000000-0000-4000-8000-000000000011"}
	writeRealtimeFrame(t, conn, coreStreamFrame(mainTurnID, mismatch))
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_digest_mismatch" {
		t.Fatalf("mismatch=%#v", got)
	} else if raw, _ := json.Marshal(got); string(raw) == "" || strings.Contains(string(raw), "changed") || strings.Contains(string(raw), "hello") {
		t.Fatalf("error frame leaked request text: %s", raw)
	}
	writeRealtimeFrame(t, conn, coreStreamFrame("extension-turn", map[string]any{"conversation_id": testConversationID, "message": "hello", "client_model_profile_id": "00000000-0000-4000-8000-000000000011", "extensions": []any{map[string]any{"id": "third-party"}}}))
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_incompatible" {
		t.Fatalf("extensions=%#v", got)
	}
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.agent_core_stream.cancel", "turn_id": mainTurnID})
	if got := readRealtimeFrame(t, conn); got["type"] != "server.agent_core_stream.event" || got["event"] != "done" || got["seq"] != float64(3) {
		t.Fatalf("cancel=%#v", got)
	} else if data, ok := got["data"].(map[string]any); !ok || data["status"] != "completed" {
		t.Fatalf("cancel projection=%#v", got)
	}
	member := service.realtimeModule.IssueTicket(realtimewsmodule.Ticket{Role: "member", UserID: "@member:example.com"})
	memberConn := dialRealtimeWS(t, server.URL, member["ticket"].(string))
	defer memberConn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, memberConn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, memberConn)
	writeRealtimeFrame(t, memberConn, coreStreamFrame("member-turn", params))
	if got := readRealtimeFrame(t, memberConn); got["code"] != "M_FORBIDDEN" {
		t.Fatalf("member=%#v", got)
	}
	otherOwner := service.realtimeModule.IssueTicket(realtimewsmodule.Ticket{Role: "owner", UserID: "@other-owner:example.com"})
	otherConn := dialRealtimeWS(t, server.URL, otherOwner["ticket"].(string))
	defer otherConn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, otherConn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, otherConn)
	writeRealtimeFrame(t, otherConn, map[string]any{"type": "client.agent_core_stream.cancel", "turn_id": mainTurnID})
	if got := readRealtimeFrame(t, otherConn); got["code"] != "agent_core_not_found" {
		t.Fatalf("other-owner cancel=%#v", got)
	}
}

func TestAgentCoreRejectsInvalidGetTurnProjectionOnReattach(t *testing.T) {
	fake, cfg := startCoreWireFake(t)
	service := NewService(Config{ServerName: "example.com", AgentCore: cfg})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, conn)
	params := map[string]any{"conversation_id": testConversationID, "message": "projection", "client_model_profile_id": "00000000-0000-4000-8000-000000000011"}
	writeRealtimeFrame(t, conn, coreStreamFrame(mainTurnID, params))
	_ = readRealtimeFrame(t, conn)
	_ = readRealtimeFrame(t, conn)
	_ = readRealtimeFrame(t, conn)
	fake.mu.Lock()
	fake.invalidGet = true
	fake.mu.Unlock()
	digest := hex.EncodeToString(coreturns.DigestParams(params))
	writeRealtimeFrame(t, conn, coreStreamFrameWithDigest(mainTurnID, map[string]any{}, digest, 2))
	if got := readRealtimeFrame(t, conn); got["code"] != "agent_core_upstream_failed" {
		t.Fatalf("invalid GetTurn projection=%#v", got)
	}
}

func TestAgentCoreRealtimeDisconnectStopsWatcherWithoutCoreCancel(t *testing.T) {
	fake, cfg := startCoreWireFake(t)
	fake.block = true
	service := NewService(Config{ServerName: "example.com", AgentCore: cfg})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, conn)
	writeRealtimeFrame(t, conn, coreStreamFrame(disconnectTurnID, map[string]any{"conversation_id": testConversationID, "message": "body", "client_model_profile_id": "00000000-0000-4000-8000-000000000011"}))
	if got := readRealtimeFrame(t, conn); got["type"] != "server.agent_core_stream.accepted" {
		t.Fatalf("accepted=%#v", got)
	}
	if err := conn.Close(websocket.StatusNormalClosure, "page closed"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	fake.mu.Lock()
	cancels := fake.cancels
	fake.mu.Unlock()
	if cancels != 0 {
		t.Fatalf("disconnect invoked Core CancelTurn %d times", cancels)
	}
}

func TestAgentCoreRealtimeReplayGapReturnsBounds(t *testing.T) {
	fake, cfg := startCoreWireFake(t)
	fake.gap = true
	service := NewService(Config{ServerName: "example.com", AgentCore: cfg})
	router := newP2PTestRouter(service)
	server := httptest.NewServer(router)
	defer server.Close()
	conn := dialRealtimeWS(t, server.URL, mustCreateRealtimeWSTicket(t, router, service.AccessToken()))
	defer conn.Close(websocket.StatusNormalClosure, "")
	writeRealtimeFrame(t, conn, map[string]any{"type": "client.hello"})
	_ = readRealtimeFrame(t, conn)
	writeRealtimeFrame(t, conn, coreStreamFrame(gapTurnID, map[string]any{"conversation_id": testConversationID, "message": "body", "client_model_profile_id": "00000000-0000-4000-8000-000000000011"}, 2))
	_ = readRealtimeFrame(t, conn)
	errFrame := readRealtimeFrame(t, conn)
	if errFrame["code"] != "agent_core_replay_gap" || errFrame["first_seq"] != float64(4) || errFrame["last_seq"] != float64(8) {
		t.Fatalf("gap=%#v", errFrame)
	}
}

func startCoreWireFake(t *testing.T) (*coreWireFake, agentcoremodule.Config) {
	t.Helper()
	ca, certPEM, keyPEM := coreTestCert(t, "core.example")
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
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	fake := &coreWireFake{}
	gs := grpc.NewServer()
	agentv1.RegisterConversationServiceServer(gs, fake)
	agentv1.RegisterModelProfileServiceServer(gs, modelWireFake{})
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	go gs.Serve(tlsLn)
	t.Cleanup(func() { gs.Stop(); ln.Close() })
	return fake, agentcoremodule.Config{Enabled: true, Address: ln.Addr().String(), ServerName: "core.example", CAFile: caPath, TokenFile: tokenPath, ExpectedInstanceID: "00000000-0000-4000-8000-000000000001"}
}
func coreTestCert(t *testing.T, host string) ([]byte, []byte, []byte) {
	t.Helper()
	caKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	caT := &x509.Certificate{SerialNumber: new(big.Int).SetInt64(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	leafKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	leafT := &x509.Certificate{SerialNumber: new(big.Int).SetInt64(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafT, caT, &leafKey.PublicKey, caKey)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})
}
