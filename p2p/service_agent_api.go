package p2p

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
)

// NativeAgentRunner remains the public runtime injection boundary while the
// default implementation and action ownership live in internal/agent.
type NativeAgentRunner interface {
	Apply(context.Context, string) error
	Invoke(context.Context, string, map[string]any) (map[string]any, error)
	Stream(context.Context, string, map[string]any, func(nativeagent.Event) error) error
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
	displayName := onlineIdentity.DisplayName
	avatarURL := onlineIdentity.AvatarURL
	homeserver := p.service.homeserver
	p.service.mu.Unlock()
	session := agentmodule.MatrixSession{
		DeviceID:   requestedDeviceID,
		UserID:     userID,
		Homeserver: homeserver,
	}
	if issuer == nil {
		return session, nil
	}
	token, err := issuer.EnsureMatrixSession(ctx, userID, displayName, avatarURL, requestedDeviceID, false)
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

func (p serviceAgentAccountPort) PublishOffline(ctx context.Context) *apiError {
	return transportWriteError(p.service.publishCurrentAgentStatusState(ctx))
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
	if err := transport.UpdateMemberProfile(ctx, UpdateMemberProfileRequest{
		RoomID: roomID, UserMXID: agentMXID,
		DisplayName: identity.DisplayName, AvatarURL: identity.AvatarURL,
		Timestamp: time.Now().UTC(),
	}); err != nil {
		return agentIdentitySyncError(err)
	}
	return nil
}

func agentIdentitySyncError(error) *apiError {
	return codedError(http.StatusBadGateway, "agent_identity_sync_failed", "agent identity was saved but Matrix sync failed")
}

// nativeAgentConfigStore adapts the account-scoped durable portal record to
// the runtime's narrow configuration store.
type nativeAgentConfigStore struct {
	service *Service
}

func (s nativeAgentConfigStore) Load(context.Context) (map[string]any, bool, error) {
	if s.service == nil {
		return map[string]any{}, false, nil
	}
	s.service.mu.Lock()
	defer s.service.mu.Unlock()
	return agentConfigToNativeMap(s.service.agentConfig), true, nil
}

func (s nativeAgentConfigStore) Save(ctx context.Context, config map[string]any) error {
	if s.service == nil {
		return fmt.Errorf("native agent config store is unavailable")
	}
	ctx, finishOperation := s.service.beginAccountOperation(ctx)
	defer finishOperation()
	if s.service.accountIsDeprovisioned() {
		return fmt.Errorf("account is deprovisioned")
	}
	s.service.mu.Lock()
	s.service.agentConfig = agentConfigFromNativeMap(s.service.agentConfig, config)
	state := s.service.portalStateLocked()
	s.service.mu.Unlock()
	if store := s.service.portalStore(); store != nil {
		return store.SavePortal(ctx, state)
	}
	return nil
}

// These wrappers keep the root Service construction and focused compatibility
// tests stable while Native Agent configuration ownership lives in
// internal/agent.
func agentConfigToNativeMap(cfg agentConfig) map[string]any {
	return agentmodule.ToNativeMap(cfg)
}

func agentConfigFromNativeMap(current agentConfig, config map[string]any) agentConfig {
	return agentmodule.FromNativeMap(current, config)
}

func migrateLegacyAgentPluginConfig(ctx context.Context, store Store, state *portalState) (bool, error) {
	return agentmodule.MigrateLegacyPluginConfig(ctx, store, state, agentmodule.LegacyPluginID)
}
