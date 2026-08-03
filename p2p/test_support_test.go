package p2p

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkplugin"
	channelsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/channels"
	portalmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/portal"
	"github.com/gorilla/mux"
)

type channelReactionHistory = channelsmodule.ReactionHistory
type pluginCatalogEntry = dirextalkplugin.CatalogEntry
type pluginInstance = dirextalkplugin.Instance
type portalCredentialsFile = portalmodule.Credentials
type reportRecord = dirextalkdomain.ReportRecord

func mustHandle[T any](t *testing.T, service *Service, action string, params map[string]any) T {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	result, apiErr := service.Handle(context.Background(), action, params)
	if apiErr != nil {
		t.Fatalf("%s failed: %#v", action, apiErr)
	}
	typed, ok := result.(T)
	if !ok {
		t.Fatalf("%s returned %T: %#v", action, result, result)
	}
	return typed
}

func bootstrapService(t *testing.T, service *Service) map[string]any {
	t.Helper()
	return mustHandle[map[string]any](t, service, "portal.bootstrap", map[string]any{"password": service.password})
}

func mustListP2PEvents(t *testing.T, service *Service) []p2pEvent {
	t.Helper()
	events, err := service.listP2PEvents(context.Background(), 0, 500)
	if err != nil {
		t.Fatalf("list P2P events: %v", err)
	}
	return events
}

func mustInsertChannelPost(t *testing.T, service *Service, post channelPostRecord) {
	t.Helper()
	record := dirextalkdomain.ChannelPostRecord{
		PostID: post.PostID, ChannelID: post.ChannelID, RoomID: post.RoomID,
		EventID: post.EventID, AuthorMXID: post.AuthorMXID, AuthorName: post.AuthorName,
		Body: post.Body, MessageType: post.MessageType, MediaJSON: post.MediaJSON,
		Visibility: post.Visibility, OriginServerTS: post.OriginServerTS, CommentCount: post.CommentCount,
	}
	if err := service.store.InsertChannelPost(context.Background(), record); err != nil {
		t.Fatalf("insert channel post %q: %v", post.PostID, err)
	}
}

func mustInsertChannelComment(t *testing.T, service *Service, comment channelCommentRecord) {
	t.Helper()
	record := dirextalkdomain.ChannelCommentRecord{
		CommentID: comment.CommentID, PostID: comment.PostID, ChannelID: comment.ChannelID,
		EventID: comment.EventID, AuthorMXID: comment.AuthorMXID, AuthorName: comment.AuthorName,
		Body: comment.Body, MessageType: comment.MessageType, MediaJSON: comment.MediaJSON,
		ReplyToCommentID: comment.ReplyToCommentID, ReplyToAuthorMXID: comment.ReplyToAuthorMXID,
		MentionsJSON: comment.MentionsJSON, OriginServerTS: comment.OriginServerTS,
	}
	if err := service.store.InsertChannelComment(context.Background(), record); err != nil {
		t.Fatalf("insert channel comment %q: %v", comment.CommentID, err)
	}
}

func mustUpsertReaction(t *testing.T, service *Service, reaction reactionRecord) {
	t.Helper()
	if err := service.store.UpsertReaction(context.Background(), reaction); err != nil {
		t.Fatalf("upsert reaction for %q: %v", reaction.TargetID, err)
	}
}

func mustUpsertFavorite(t *testing.T, service *Service, favorite favoriteRecord) {
	t.Helper()
	if err := service.store.UpsertFavorite(context.Background(), favorite); err != nil {
		t.Fatalf("upsert favorite %d: %v", favorite.ID, err)
	}
}

func jsonRequest(t *testing.T, path string, body map[string]any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func newP2PTestRouter(service *Service) *mux.Router {
	router := mux.NewRouter()
	Register(router.PathPrefix(PathPrefix).Subrouter(), service)
	RegisterMCP(router, service)
	return router
}

type recordingTransport struct {
	roomID           string
	eventID          string
	ts               int64
	createRooms      []CreateRoomRequest
	messages         []SendMessageRequest
	invites          []string
	joins            []string
	leaves           []string
	kicks            []string
	roomChannel      channel
	roomChannelError error
	roomMembers      []memberRecord
	profiles         []string
	profileErrors    map[string]error
	redactions       []string
	inviteRequests   []InviteUserRequest
	joinRequests     []JoinRoomRequest
	stateEvents      []SendStateEventRequest
}

func (t *recordingTransport) CreateRoom(ctx context.Context, req CreateRoomRequest) (CreateRoomResult, error) {
	t.createRooms = append(t.createRooms, req)
	if t.roomID == "" {
		t.roomID = "!recorded:example.com"
	}
	return CreateRoomResult{RoomID: t.roomID}, nil
}

func (t *recordingTransport) SendMessage(ctx context.Context, req SendMessageRequest) (SendMessageResult, error) {
	t.messages = append(t.messages, req)
	if t.eventID == "" {
		t.eventID = "$recorded:example.com"
	}
	if t.ts == 0 {
		t.ts = 1770000000000
	}
	return SendMessageResult{EventID: t.eventID, OriginServerTS: t.ts}, nil
}

func (t *recordingTransport) SendStateEvent(ctx context.Context, req SendStateEventRequest) error {
	t.stateEvents = append(t.stateEvents, req)
	return nil
}

func (t *recordingTransport) InviteUser(ctx context.Context, req InviteUserRequest) error {
	t.invites = append(t.invites, req.InviterMXID+" -> "+req.InviteeMXID+" in "+req.RoomID)
	t.inviteRequests = append(t.inviteRequests, req)
	return nil
}

func (t *recordingTransport) JoinRoom(ctx context.Context, req JoinRoomRequest) (JoinRoomResult, error) {
	t.joins = append(t.joins, req.UserMXID+" in "+req.RoomIDOrAlias)
	t.joinRequests = append(t.joinRequests, req)
	return JoinRoomResult{RoomID: req.RoomIDOrAlias}, nil
}

func (t *recordingTransport) LeaveRoom(ctx context.Context, req LeaveRoomRequest) error {
	t.leaves = append(t.leaves, req.UserMXID+" from "+req.RoomID)
	return nil
}

func (t *recordingTransport) KickUser(ctx context.Context, req KickUserRequest) error {
	t.kicks = append(t.kicks, req.SenderMXID+" kicks "+req.TargetMXID+" from "+req.RoomID)
	return nil
}

func (t *recordingTransport) GetRoomChannel(ctx context.Context, roomID string) (channel, bool, error) {
	if t.roomChannelError != nil {
		return channel{}, false, t.roomChannelError
	}
	if t.roomChannel.ChannelID == "" {
		return channel{}, false, nil
	}
	ch := t.roomChannel
	if ch.RoomID == "" {
		ch.RoomID = roomID
	}
	if ch.RoomID != roomID {
		return channel{}, false, nil
	}
	return ch, true, nil
}

func (t *recordingTransport) ListRoomMembers(ctx context.Context, roomID string) ([]memberRecord, error) {
	members := make([]memberRecord, 0, len(t.roomMembers))
	for _, member := range t.roomMembers {
		if member.RoomID == "" {
			member.RoomID = roomID
		}
		if member.RoomID == roomID {
			members = append(members, member)
		}
	}
	return members, nil
}

func (t *recordingTransport) UpdateMemberProfile(ctx context.Context, req UpdateMemberProfileRequest) error {
	t.profiles = append(t.profiles, req.UserMXID+" in "+req.RoomID+" as "+req.DisplayName+" "+req.AvatarURL)
	if t.profileErrors != nil {
		return t.profileErrors[req.RoomID]
	}
	return nil
}

func (t *recordingTransport) RedactEvent(ctx context.Context, req RedactEventRequest) (RedactEventResult, error) {
	t.redactions = append(t.redactions, req.SenderMXID+" redacts "+req.EventID+" in "+req.RoomID)
	return RedactEventResult{EventID: "$redaction:example.com"}, nil
}
