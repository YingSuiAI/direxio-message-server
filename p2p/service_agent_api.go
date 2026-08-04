package p2p

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
)

// NativeAgentRunner is the external Native Agent capability boundary.
type NativeAgentRunner interface {
	Apply(context.Context, string) error
	Invoke(context.Context, string, map[string]any) (map[string]any, error)
	Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error
}

// serviceAgentAccountPort retains Service-owned locking, Matrix sessions and
// durable portal writes while internal/agent owns the ProductCore workflow.
type serviceAgentAccountPort struct{ service *Service }

func (p serviceAgentAccountPort) Password() string {
	p.service.mu.Lock()
	defer p.service.mu.Unlock()
	return p.service.password
}

func (p serviceAgentAccountPort) CreateMatrixSession(ctx context.Context, params map[string]any) (agentmodule.MatrixSession, *apiError) {
	p.service.matrixSessionMu.Lock()
	defer p.service.matrixSessionMu.Unlock()

	requestedDeviceID := requestedMatrixDeviceID(params)
	p.service.mu.Lock()
	issuer := p.service.sessions
	userID := p.service.agentMXIDLocked()
	onlineIdentity := agentmodule.OnlineAgentIdentity(p.service.agentConfig)
	homeserver := p.service.homeserver
	p.service.mu.Unlock()
	displayName := onlineIdentity.DisplayName
	session := agentmodule.MatrixSession{
		DeviceID:   requestedDeviceID,
		UserID:     userID,
		Homeserver: homeserver,
	}
	if issuer == nil {
		return session, nil
	}
	token, err := issuer.EnsureMatrixSession(ctx, userID, displayName, onlineIdentity.AvatarURL, requestedDeviceID, false)
	if err != nil {
		return agentmodule.MatrixSession{}, internalError(err)
	}
	session.AccessToken = &token
	return session, nil
}

func (p serviceAgentAccountPort) Config() agentConfig {
	p.service.mu.Lock()
	defer p.service.mu.Unlock()
	config := p.service.agentConfig
	config.MCPBlockedRoomIDs = append([]string(nil), config.MCPBlockedRoomIDs...)
	return config
}

func (p serviceAgentAccountPort) UpdateConfig(ctx context.Context, mutate func(agentConfig) agentConfig) (agentConfig, *apiError) {
	p.service.mu.Lock()
	p.service.agentConfig = mutate(p.service.agentConfig)
	config := p.service.agentConfig
	state := p.service.portalStateLocked()
	p.service.mu.Unlock()
	if store := p.service.portalStore(); store != nil {
		if err := store.SavePortal(ctx, state); err != nil {
			return agentConfig{}, internalError(err)
		}
	}
	return config, nil
}

func (p serviceAgentAccountPort) SyncOnlineIdentity(ctx context.Context, identity dirextalkdomain.AgentIdentityConfig) *apiError {
	return p.service.syncOnlineAgentIdentity(ctx, identity)
}

func (s *Service) syncOnlineAgentIdentity(ctx context.Context, identity dirextalkdomain.AgentIdentityConfig) *apiError {
	identity = agentmodule.OnlineAgentIdentity(dirextalkdomain.AgentConfig{OnlineAgentIdentity: identity})
	s.mu.Lock()
	issuer := s.sessions
	agentMXID := s.agentMXIDLocked()
	s.mu.Unlock()
	if updater, ok := issuer.(MatrixProfileUpdater); ok && updater != nil {
		if err := updater.UpdateMatrixProfile(ctx, agentMXID, identity.DisplayName, identity.AvatarURL); err != nil {
			return agentIdentitySyncError(err)
		}
	}
	changed, err := s.ensureAgentRoom(ctx)
	if err != nil {
		return agentIdentitySyncError(err)
	}
	if changed {
		s.mu.Lock()
		state := s.portalStateLocked()
		s.mu.Unlock()
		if store := s.portalStore(); store != nil {
			if err := store.SavePortal(ctx, state); err != nil {
				return internalError(err)
			}
		}
	}
	s.mu.Lock()
	roomID := strings.TrimSpace(s.agentRoomID)
	transport := s.transport
	agentMXID = s.agentMXIDLocked()
	s.mu.Unlock()
	if transport == nil || roomID == "" {
		return nil
	}
	if err := transport.UpdateMemberProfile(ctx, UpdateMemberProfileRequest{RoomID: roomID, UserMXID: agentMXID, DisplayName: identity.DisplayName, AvatarURL: identity.AvatarURL, Timestamp: time.Now().UTC()}); err != nil {
		return agentIdentitySyncError(err)
	}
	return nil
}

func agentIdentitySyncError(err error) *apiError {
	return codedError(http.StatusBadGateway, "agent_identity_sync_failed", "agent identity was saved but Matrix sync failed")
}

func (p serviceAgentAccountPort) PublishOffline(ctx context.Context) *apiError {
	return transportWriteError(p.service.publishCurrentAgentStatusState(ctx))
}
