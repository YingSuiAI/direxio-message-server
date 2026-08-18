package p2p

import (
	"context"
	"errors"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	reportsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/reports"
)

const systemRoomName = reportsmodule.SystemRoomName

// ensureSystemRoom is the root Matrix-room adapter retained because room
// creation uses the shared Service identity and transport wiring.
func (s *Service) ensureSystemRoom(ctx context.Context) (bool, error) {
	s.systemRoomMu.Lock()
	defer s.systemRoomMu.Unlock()

	s.mu.Lock()
	currentRoomID := strings.TrimSpace(s.systemRoomID)
	ownerMXID := s.ownerMXID
	ownerDisplayName := s.profile.DisplayName
	ownerAvatarURL := s.profile.AvatarURL
	s.mu.Unlock()
	if currentRoomID != "" || s.transport == nil {
		return false, nil
	}
	res, err := s.transport.CreateRoom(ctx, CreateRoomRequest{
		CreatorMXID:        ownerMXID,
		CreatorDisplayName: ownerDisplayName,
		CreatorAvatarURL:   ownerAvatarURL,
		Name:               systemRoomName,
		Topic:              "Dirextalk system notifications",
		Visibility:         "private",
		RoomType:           DirextalkRoomTypeSystem,
		IsDirect:           false,
	})
	if err != nil {
		return false, err
	}
	roomID := strings.TrimSpace(res.RoomID)
	if roomID == "" {
		return false, nil
	}
	s.mu.Lock()
	state := s.portalStateLocked()
	state.SystemRoomID = roomID
	s.mu.Unlock()
	if store := s.portalStore(); store != nil {
		if err := store.SavePortal(ctx, state); err != nil {
			return false, err
		}
	}
	s.mu.Lock()
	s.systemRoomID = roomID
	s.mu.Unlock()
	return roomID != currentRoomID, nil
}

type serviceReportTargetPort struct{ service *Service }

func (p serviceReportTargetPort) Group(ctx context.Context, roomID string) (reportsmodule.Target, bool, error) {
	group, ok, err := p.service.groupsModule.ByRoom(ctx, roomID)
	if err != nil || !ok {
		return reportsmodule.Target{}, ok, err
	}
	return reportsmodule.Target{RoomID: group.RoomID, Name: group.Name}, true, nil
}

func (p serviceReportTargetPort) Channel(ctx context.Context, channelID, roomID string) (reportsmodule.Target, bool, error) {
	channel, ok, err := p.service.channelsModule.ByIDOrRoom(ctx, channelID, roomID)
	if err != nil || !ok {
		return reportsmodule.Target{}, ok, err
	}
	return reportsmodule.Target{RoomID: channel.RoomID, ChannelID: channel.ChannelID, Name: channel.Name}, true, nil
}

type serviceReportSystemRoomPort struct{ service *Service }

func (p serviceReportSystemRoomPort) Ensure(ctx context.Context) (reportsmodule.SystemRoom, error) {
	if _, err := p.service.ensureSystemRoom(ctx); err != nil {
		return reportsmodule.SystemRoom{}, err
	}
	p.service.mu.Lock()
	systemRoom := reportsmodule.SystemRoom{RoomID: p.service.systemRoomID, SenderMXID: p.service.ownerMXID}
	p.service.mu.Unlock()
	return systemRoom, nil
}

type serviceReportMatrixPort struct{ service *Service }

func (p serviceReportMatrixPort) SendMessage(ctx context.Context, req dirextalktransport.SendMessageRequest) (dirextalktransport.SendMessageResult, error) {
	if p.service == nil || p.service.transport == nil {
		return dirextalktransport.SendMessageResult{}, errors.New("matrix transport is not configured")
	}
	return p.service.transport.SendMessage(ctx, req)
}
