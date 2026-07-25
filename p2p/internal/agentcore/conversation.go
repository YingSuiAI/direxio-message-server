package agentcore

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type TurnStart struct {
	ClientTurnID, ConversationID, Message, ModelProfileID string
	ExpectedRevision                                      *int64
	ModelProfileRevision                                  int64
}
type Turn struct {
	CoreTurnID, ConversationID, Status, TerminalCode, TerminalSummary string
	LastSequence, Revision, ModelProfileRevision                      int64
}
type TurnEvent struct {
	Sequence                            int64
	Kind, Text, ErrorCode, ErrorSummary string
	FirstSequence, LastSequence         int64
	ReplayGap                           bool
	Err                                 error
}

// The short names mirror the Core RPC names for adapters and focused tests;
// the Core suffix variants below remain the explicit Message Server boundary.
func (c *Client) StartTurn(ctx context.Context, req TurnStart) (Turn, error) {
	return c.StartCoreTurn(ctx, req)
}
func (c *Client) GetTurn(ctx context.Context, id string) (Turn, error) {
	return c.GetCoreTurn(ctx, id)
}
func (c *Client) WatchTurnEvents(ctx context.Context, id string, after int64) (<-chan TurnEvent, error) {
	return c.WatchCoreTurnEvents(ctx, id, after)
}
func (c *Client) CancelTurn(ctx context.Context, id, requestID string, revision int64) (Turn, error) {
	return c.CancelCoreTurn(ctx, id, requestID, revision)
}

func (c *Client) conversationConn(ctx context.Context) (context.Context, agentv1.ConversationServiceClient, error) {
	if c == nil || !c.cfg.Enabled {
		return nil, nil, status.Error(codes.Unavailable, "agent core is not configured")
	}
	ca, err := os.ReadFile(c.cfg.CAFile)
	if err != nil {
		return nil, nil, status.Error(codes.Unavailable, "agent core trust is unavailable")
	}
	pool := newCertPool(ca)
	if pool == nil {
		return nil, nil, status.Error(codes.Unavailable, "agent core trust is unavailable")
	}
	token, err := readCanonicalToken(c.cfg.TokenFile)
	if err != nil {
		return nil, nil, status.Error(codes.Unauthenticated, "agent core authentication failed")
	}
	conn, err := c.connection(ctx, pool)
	if err != nil {
		return nil, nil, err
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "DTX-Agent-Token "+base64.RawURLEncoding.EncodeToString(token)), agentv1.NewConversationServiceClient(conn), nil
}

func newCertPool(pemBytes []byte) *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil
	}
	return pool
}

func (c *Client) StartCoreTurn(ctx context.Context, req TurnStart) (Turn, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.cfg.UnaryTimeout)
	defer cancel()
	callCtx, client, err := c.conversationConn(callCtx)
	if err != nil {
		return Turn{}, err
	}
	r := &agentv1.ConversationServiceStartTurnRequest{IdempotencyKey: req.ClientTurnID, ConversationId: req.ConversationID, Message: req.Message, ModelProfileId: req.ModelProfileID, ExpectedRevision: req.ExpectedRevision}
	res, err := client.StartTurn(callCtx, r)
	if err != nil {
		return Turn{}, err
	}
	if res == nil || res.GetTurn() == nil {
		return Turn{}, errors.New("agent core returned an empty turn response")
	}
	return turnFromProto(res.GetTurn()), nil
}

type ResolvedModelProfile struct {
	ClientProfileID, CoreProfileID string
	Revision                       int64
}

// ResolveModelProfile turns the client-owned stable UUID into the deployment's
// Core profile UUID and captures its current nonzero revision. The client ID
// is never sent as a Core profile ID.
func (c *Client) ResolveModelProfile(ctx context.Context, clientProfileID string) (ResolvedModelProfile, error) {
	clientProfileID = strings.TrimSpace(clientProfileID)
	if _, err := uuid.Parse(clientProfileID); err != nil || uuid.MustParse(clientProfileID).String() != clientProfileID {
		return ResolvedModelProfile{}, status.Error(codes.InvalidArgument, "client model profile id is invalid")
	}
	var profiles []*agentv1.CoreModelProfile
	err := c.unary(ctx, func(callCtx context.Context, client agentv1.ModelProfileServiceClient) error {
		page := ""
		for {
			resp, err := client.List(callCtx, &agentv1.ModelProfileServiceListRequest{PageSize: 100, PageToken: page})
			if err != nil {
				return err
			}
			if resp == nil {
				return errors.New("agent core returned an empty model profile list response")
			}
			profiles = append(profiles, resp.GetProfiles()...)
			page = resp.GetNextPageToken()
			if page == "" {
				break
			}
		}
		return nil
	})
	if err != nil {
		return ResolvedModelProfile{}, err
	}
	for _, profile := range profiles {
		if profile == nil || profile.GetClientProfileId() != clientProfileID {
			continue
		}
		coreID := strings.TrimSpace(profile.GetProfileId())
		parsed, parseErr := uuid.Parse(coreID)
		if parseErr != nil || parsed.String() != coreID || profile.GetRevision() <= 0 {
			return ResolvedModelProfile{}, status.Error(codes.FailedPrecondition, "agent core model profile is invalid")
		}
		return ResolvedModelProfile{ClientProfileID: clientProfileID, CoreProfileID: coreID, Revision: profile.GetRevision()}, nil
	}
	return ResolvedModelProfile{}, status.Error(codes.NotFound, "agent core model profile was not found")
}
func (c *Client) GetCoreTurn(ctx context.Context, id string) (Turn, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.cfg.UnaryTimeout)
	defer cancel()
	callCtx, client, err := c.conversationConn(callCtx)
	if err != nil {
		return Turn{}, err
	}
	res, err := client.GetTurn(callCtx, &agentv1.ConversationServiceGetTurnRequest{TurnId: id})
	if err != nil {
		return Turn{}, err
	}
	if res == nil || res.GetTurn() == nil {
		return Turn{}, errors.New("agent core returned an empty turn response")
	}
	turn := turnFromProto(res.GetTurn())
	if err := ValidateCoreTurn(turn, id, ""); err != nil {
		return Turn{}, err
	}
	return turn, nil
}

// ValidateCoreTurn is the single boundary check for all Core turn projections
// consumed by recovery, cancellation, and reattachment paths.
func ValidateCoreTurn(turn Turn, requestedID, expectedConversationID string) error {
	if strings.TrimSpace(requestedID) == "" || turn.CoreTurnID != requestedID || strings.TrimSpace(turn.ConversationID) == "" {
		return errors.New("agent core returned an invalid turn identity")
	}
	if expectedConversationID != "" && turn.ConversationID != expectedConversationID {
		return errors.New("agent core returned an unexpected conversation")
	}
	if turn.LastSequence < 0 || turn.Revision <= 0 || !validCoreTurnStatus(turn.Status) {
		return errors.New("agent core returned an invalid turn projection")
	}
	return nil
}

func validCoreTurnStatus(status string) bool {
	switch status {
	case "accepted", "running", "waiting_confirmation", "completed", "done", "succeeded", "failed", "canceled", "uncertain":
		return true
	default:
		return false
	}
}
func (c *Client) CancelCoreTurn(ctx context.Context, id, requestID string, revision int64) (Turn, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.cfg.UnaryTimeout)
	defer cancel()
	callCtx, client, err := c.conversationConn(callCtx)
	if err != nil {
		return Turn{}, err
	}
	res, err := client.CancelTurn(callCtx, &agentv1.ConversationServiceCancelTurnRequest{IdempotencyKey: requestID, TurnId: id, ExpectedRevision: revision})
	if err != nil {
		return Turn{}, err
	}
	if res == nil || res.GetTurn() == nil {
		return Turn{}, errors.New("agent core returned an empty turn response")
	}
	return turnFromProto(res.GetTurn()), nil
}

func (c *Client) WatchCoreTurnEvents(ctx context.Context, id string, after int64) (<-chan TurnEvent, error) {
	if after < 0 {
		return nil, status.Error(codes.InvalidArgument, "after_sequence must be non-negative")
	}
	streamCtx, streamCancel := context.WithCancel(ctx)
	callCtx, client, err := c.conversationConn(streamCtx)
	if err != nil {
		streamCancel()
		return nil, err
	}
	stream, err := client.WatchTurnEvents(callCtx, &agentv1.ConversationServiceWatchTurnEventsRequest{TurnId: id, AfterSequence: after, Limit: 100})
	if err != nil {
		streamCancel()
		return nil, err
	}
	out := make(chan TurnEvent, 16)
	go func() {
		defer close(out)
		defer streamCancel()
		timer := time.NewTimer(c.cfg.StreamIdleTimeout)
		defer timer.Stop()
		lastSequence := after
		for {
			recv := make(chan struct {
				ev  *agentv1.ConversationServiceWatchTurnEventsResponse
				err error
			}, 1)
			go func() {
				ev, e := stream.Recv()
				recv <- struct {
					ev  *agentv1.ConversationServiceWatchTurnEventsResponse
					err error
				}{ev, e}
			}()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				select {
				case out <- TurnEvent{Kind: "watch_error", ErrorCode: "agent_core_stream_idle", ErrorSummary: "agent core stream idle timeout", Err: context.DeadlineExceeded}:
				case <-ctx.Done():
				}
				return
			case r := <-recv:
				if r.err != nil {
					ev := TurnEvent{Kind: "watch_error", ErrorCode: "agent_core_stream_failed", ErrorSummary: "agent core stream failed", Err: r.err}
					if errors.Is(r.err, io.EOF) {
						ev.ErrorCode, ev.ErrorSummary = "agent_core_stream_ended", "agent core stream ended"
					}
					select {
					case out <- ev:
					case <-ctx.Done():
					}
					return
				}
				timer.Reset(c.cfg.StreamIdleTimeout)
				if r.ev == nil || r.ev.GetEvent() == nil {
					sendConversationStreamValidationError(ctx, out, "agent core returned an empty turn event")
					return
				}
				event := r.ev.GetEvent()
				if event.GetTurnId() != id {
					sendConversationStreamValidationError(ctx, out, "agent core returned an event for another turn")
					return
				}
				if event.GetReplayGap() {
					if event.GetFirstSequence() <= after || event.GetLastSequence() < event.GetFirstSequence() {
						sendConversationStreamValidationError(ctx, out, "agent core returned invalid replay bounds")
						return
					}
				} else {
					if event.GetSequence() <= lastSequence || !validTurnEventKind(event.GetKind()) || (event.GetKind() == "error" && strings.TrimSpace(event.GetErrorCode()) == "") {
						sendConversationStreamValidationError(ctx, out, "agent core returned an invalid or non-monotonic turn event")
						return
					}
					lastSequence = event.GetSequence()
				}
				select {
				case out <- eventFromProto(event):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func sendConversationStreamValidationError(ctx context.Context, out chan<- TurnEvent, summary string) {
	select {
	case out <- TurnEvent{Kind: "watch_error", ErrorCode: "agent_core_stream_failed", ErrorSummary: summary, Err: errors.New(summary)}:
	case <-ctx.Done():
	}
}

func validTurnEventKind(kind string) bool {
	switch kind {
	case "started", "delta", "tool_call", "tool_result", "done", "completed", "error", "canceled":
		return true
	default:
		return false
	}
}

func turnFromProto(t *agentv1.CoreConversationTurn) Turn {
	if t == nil {
		return Turn{}
	}
	return Turn{CoreTurnID: t.GetTurnId(), ConversationID: t.GetConversationId(), Status: t.GetState(), TerminalCode: t.GetTerminalCode(), TerminalSummary: t.GetTerminalSummary(), LastSequence: t.GetLastSequence(), Revision: t.GetRevision()}
}
func eventFromProto(e *agentv1.CoreConversationTurnEvent) TurnEvent {
	return TurnEvent{Sequence: e.GetSequence(), Kind: e.GetKind(), Text: e.GetText(), ErrorCode: e.GetErrorCode(), ErrorSummary: e.GetErrorSummary(), FirstSequence: e.GetFirstSequence(), LastSequence: e.GetLastSequence(), ReplayGap: e.GetReplayGap()}
}

func conversationActionError(err error) *actionbase.Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return actionbase.CodedError(http.StatusServiceUnavailable, "agent_core_unavailable", "agent core is unavailable")
	}
	switch status.Code(err) {
	case codes.InvalidArgument:
		return actionbase.CodedError(http.StatusBadRequest, "agent_core_invalid_argument", "agent core rejected the conversation request")
	case codes.Unauthenticated, codes.PermissionDenied:
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_trust_failed", "agent core authentication failed")
	case codes.NotFound:
		return actionbase.CodedError(http.StatusNotFound, "agent_core_not_found", "agent core conversation was not found")
	case codes.Aborted:
		return actionbase.CodedError(http.StatusConflict, "agent_core_conflict", "agent core conversation revision conflict")
	case codes.FailedPrecondition:
		return actionbase.CodedError(http.StatusConflict, "agent_core_precondition_failed", "agent core conversation precondition failed")
	case codes.DeadlineExceeded, codes.Unavailable:
		return actionbase.CodedError(http.StatusServiceUnavailable, "agent_core_unavailable", "agent core is unavailable")
	case codes.Unimplemented:
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_incompatible", "agent core protocol is incompatible")
	default:
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_upstream_failed", "agent core conversation request failed")
	}
}

func digestTurn(req TurnStart) []byte {
	h := sha256.New()
	h.Write([]byte(req.ClientTurnID))
	h.Write([]byte{0})
	h.Write([]byte(req.ConversationID))
	h.Write([]byte{0})
	h.Write([]byte(req.Message))
	h.Write([]byte{0})
	h.Write([]byte(req.ModelProfileID))
	return h.Sum(nil)
}
func trim(v string) string { return strings.TrimSpace(v) }
