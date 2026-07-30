package p2p

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkstate"
	channelsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/channels"
)

// authorizeChannelContentRecall preserves the transportless compatibility
// rule: the local author or a projected channel owner may recall content.
func (s *Service) authorizeChannelContentRecall(ctx context.Context, roomID, authorMXID string) *apiError {
	s.mu.Lock()
	ownerMXID := s.ownerMXID
	s.mu.Unlock()
	if ownerMXID != "" && ownerMXID == authorMXID {
		return nil
	}
	if apiErr := s.requireOwnerMember(ctx, roomID); apiErr != nil {
		if apiErr.Status != http.StatusForbidden {
			return apiErr
		}
		return statusError(http.StatusForbidden, "content author or channel owner role is required")
	}
	return nil
}

func (s *Service) requireJoinedChannelContent(ctx context.Context, roomID string) *apiError {
	s.mu.Lock()
	ownerMXID := s.ownerMXID
	s.mu.Unlock()
	member, ok, err := s.lookupMember(ctx, strings.TrimSpace(roomID), ownerMXID)
	if err != nil {
		return internalError(err)
	}
	if !ok || !strings.EqualFold(strings.TrimSpace(member.Membership), "join") {
		return statusError(http.StatusForbidden, "channel membership is required")
	}
	return nil
}

type publicChannelPostsPage struct {
	ChannelID  string                `json:"channel_id"`
	RoomID     string                `json:"room_id"`
	Posts      []channelsmodule.Post `json:"posts"`
	Visibility string                `json:"visibility"`
	Page       int64                 `json:"page"`
	PageSize   int64                 `json:"page_size"`
	HasMore    bool                  `json:"has_more"`
	NextPage   *int64                `json:"next_page,omitempty"`
}

func (s *Service) publicChannelPosts(ctx context.Context, params map[string]any) (any, *apiError) {
	channelID := trimString(params["channel_id"])
	roomID := trimString(params["room_id"])
	if channelID == "" && roomID == "" {
		return nil, badRequest("channel_id or room_id is required")
	}

	if roomID == "" {
		ch, found, err := s.channelByIDOrRoom(ctx, channelID, "")
		if err != nil {
			return nil, internalError(err)
		}
		if !found {
			return nil, statusError(http.StatusNotFound, "channel not found")
		}
		channelID, roomID = ch.ChannelID, ch.RoomID
	}
	if roomServer, ok := roomServerFromMatrixRoomID(roomID); ok && roomServer != s.serverName {
		return s.remotePublicChannelPosts(ctx, channelID, roomID, params)
	}

	ch, found, err := s.channelByIDOrRoom(ctx, channelID, roomID)
	if err != nil {
		return nil, internalError(err)
	}
	if !found {
		return nil, statusError(http.StatusNotFound, "channel not found")
	}
	result, actionErr := s.channelContentModule.PublicPosts(ctx, ch.ChannelID, params)
	if actionErr != nil {
		return nil, actionErr
	}
	result["channel_id"] = ch.ChannelID
	result["room_id"] = ch.RoomID
	return result, nil
}

func (s *Service) remotePublicChannelPosts(
	ctx context.Context,
	channelID, roomID string,
	params map[string]any,
) (any, *apiError) {
	roomServer, ok := roomServerFromMatrixRoomID(roomID)
	if !ok {
		return nil, badRequest("valid Matrix room_id is required")
	}
	forward := cloneParams(params)
	forward["room_id"] = roomID
	if channelID != "" {
		forward["channel_id"] = channelID
	}
	var page publicChannelPostsPage
	status, err := s.remotePublicAction(ctx, roomServer, "channels.public.posts.list", forward, &page)
	if err != nil {
		if status != 0 && status != http.StatusBadGateway {
			return nil, statusError(status, err.Error())
		}
		return nil, statusError(http.StatusBadGateway, err.Error())
	}
	if status != http.StatusOK {
		return nil, statusError(status, "target node public posts lookup failed")
	}
	if page.RoomID != "" && page.RoomID != roomID {
		return nil, statusError(http.StatusBadGateway, "target node returned posts for a different room")
	}
	if page.ChannelID != "" && channelID != "" && page.ChannelID != channelID {
		return nil, statusError(http.StatusBadGateway, "target node returned posts for a different channel")
	}
	if !strings.EqualFold(strings.TrimSpace(page.Visibility), dirextalkdomain.ChannelPostVisibilityPublic) {
		return nil, statusError(http.StatusBadGateway, "target node returned a non-public post page")
	}
	for _, post := range page.Posts {
		if !strings.EqualFold(strings.TrimSpace(post.Visibility), dirextalkdomain.ChannelPostVisibilityPublic) ||
			(post.RoomID != "" && post.RoomID != roomID) ||
			(page.ChannelID != "" && post.ChannelID != "" && post.ChannelID != page.ChannelID) {
			return nil, statusError(http.StatusBadGateway, "target node returned invalid public post data")
		}
	}
	page.RoomID = roomID
	if page.ChannelID == "" {
		page.ChannelID = channelID
	}
	return page, nil
}

func (s *Service) channelStore() channelStore {
	if s.store == nil {
		return nil
	}
	return s.store
}

func (s *Service) createChannelRoom(ctx context.Context, ch channel) (string, *apiError) {
	initialState := []RoomStateEvent{channelStateEvent(ch, false)}
	if historyVisibilityState, ok := channelHistoryVisibilityStateEvent(ch.ChannelType); ok {
		initialState = append([]RoomStateEvent{historyVisibilityState}, initialState...)
	}
	return s.ensureProductRoom(ctx, "channel", CreateRoomRequest{
		Name:         ch.Name,
		Topic:        ch.Description,
		Visibility:   ch.Visibility,
		RoomType:     DirextalkRoomTypeChannel,
		IsDirect:     false,
		InitialState: initialState,
	})
}

func (s *Service) fetchRoomChannel(ctx context.Context, roomID string) (channel, bool, *apiError) {
	s.mu.Lock()
	transport := s.transport
	s.mu.Unlock()
	if transport == nil {
		return channel{}, false, nil
	}
	ch, found, err := transport.GetRoomChannel(ctx, roomID)
	if err != nil {
		if roomServer := domainFromMatrixID(roomID, "!"); roomServer != "" && roomServer != s.serverName {
			return channel{}, false, statusError(404, "channel not found")
		}
		return channel{}, false, internalError(err)
	}
	return ch, found, nil
}

func (s *Service) saveChannel(ctx context.Context, ch channel) error {
	return s.channelsModule.Save(ctx, ch)
}

func (s *Service) setChannelMemberMute(ctx context.Context, roomID, channelID string, muted bool) *apiError {
	if err := s.setProductMemberMute(ctx, roomID, channelID, muted); err != nil {
		return internalError(err)
	}
	return nil
}

func channelStateEvent(ch channel, dissolved bool) RoomStateEvent {
	return roomProfileForChannel(ch, dissolved)
}

func (s *Service) publishChannelState(ctx context.Context, ch channel, dissolved bool) error {
	if s.transport == nil || strings.TrimSpace(ch.RoomID) == "" {
		return nil
	}
	s.mu.Lock()
	senderMXID := s.ownerMXID
	s.mu.Unlock()
	if err := s.transport.SendStateEvent(ctx, SendStateEventRequest{
		RoomID:     ch.RoomID,
		SenderMXID: senderMXID,
		Event:      channelStateEvent(ch, dissolved),
	}); err != nil {
		return err
	}
	return nil
}

func (s *Service) publishChannelHistoryVisibilityState(ctx context.Context, ch channel) error {
	if s.transport == nil || strings.TrimSpace(ch.RoomID) == "" {
		return nil
	}
	if historyVisibilityState, ok := channelHistoryVisibilityStateEvent(ch.ChannelType); ok {
		s.mu.Lock()
		senderMXID := s.ownerMXID
		s.mu.Unlock()
		return s.transport.SendStateEvent(ctx, SendStateEventRequest{
			RoomID:     ch.RoomID,
			SenderMXID: senderMXID,
			Event:      historyVisibilityState,
		})
	}
	return nil
}

func (s *Service) publishJoinRequestState(ctx context.Context, roomID, userID, status, reason, requestID string) *apiError {
	if s.transport == nil || strings.TrimSpace(roomID) == "" || strings.TrimSpace(userID) == "" {
		return nil
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "pending", "approved", "rejected":
	default:
		return badRequest("invalid join request status")
	}
	s.mu.Lock()
	senderMXID := s.ownerMXID
	s.mu.Unlock()
	if strings.TrimSpace(senderMXID) == "" {
		return nil
	}
	if err := s.transport.SendStateEvent(ctx, SendStateEventRequest{
		RoomID:     roomID,
		SenderMXID: senderMXID,
		Event:      roomStateEvent(dirextalkstate.JoinRequestState(roomID, userID, status, reason, requestID, time.Now().UTC())),
	}); err != nil {
		return internalError(err)
	}
	return nil
}

func (s *Service) publishMemberPolicyState(ctx context.Context, member memberRecord) *apiError {
	if s.transport == nil || strings.TrimSpace(member.RoomID) == "" || strings.TrimSpace(member.UserID) == "" {
		return nil
	}
	s.mu.Lock()
	senderMXID := s.ownerMXID
	s.mu.Unlock()
	if strings.TrimSpace(senderMXID) == "" {
		return nil
	}
	if err := s.transport.SendStateEvent(ctx, SendStateEventRequest{
		RoomID:     member.RoomID,
		SenderMXID: senderMXID,
		Event:      roomStateEvent(dirextalkstate.MemberPolicyState(member)),
	}); err != nil {
		return internalError(err)
	}
	return nil
}

func (s *Service) channelByIDOrRoom(ctx context.Context, channelID, roomID string) (channel, bool, error) {
	return s.channelsModule.ByIDOrRoom(ctx, channelID, roomID)
}

func (s *Service) channelSnapshot(ctx context.Context, channelID string) channel {
	return s.channelsModule.Snapshot(ctx, channelID)
}

func (s *Service) refreshStoredChannelCounts(ctx context.Context, channelID string) error {
	return s.channelsModule.RefreshCounts(ctx, channelID)
}
