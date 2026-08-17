package p2p

import (
	"context"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	portalmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/portal"
)

type servicePortalState struct {
	initialized             bool
	password                string
	accessToken             string
	matrixDeviceID          string
	agentToken              string
	ownerMXID               string
	agentRoomID             string
	systemRoomID            string
	profile                 ownerProfile
	agentConfig             agentConfig
	clientBuild             clientBuild
	portalSessionGeneration uint64
}

type portalStore interface {
	LoadPortal(context.Context) (portalState, bool, error)
	SavePortal(context.Context, portalState) error
	SaveClientBuild(context.Context, string, clientBuild) (bool, error)
}

func portalStoreFrom(store Store) portalStore {
	return store
}

func (s *Service) portalStore() portalStore {
	if s.store == nil {
		return nil
	}
	return s.store
}

type servicePortalModulePort struct{ service *Service }

func (p servicePortalModulePort) Authenticate(password string) (portalmodule.Snapshot, bool) {
	p.service.mu.Lock()
	defer p.service.mu.Unlock()
	if password != p.service.password {
		return portalmodule.Snapshot{}, false
	}
	return p.snapshotLocked(), true
}

func (p servicePortalModulePort) RotatePassword(oldPassword, newPassword string, newAccessToken func() string) (portalmodule.Snapshot, bool) {
	p.service.mu.Lock()
	defer p.service.mu.Unlock()
	if oldPassword == "" || oldPassword != p.service.password {
		return portalmodule.Snapshot{}, false
	}
	p.service.password = newPassword
	p.service.accessToken = newAccessToken()
	p.service.portalSessionGeneration++
	p.service.initialized = true
	return p.snapshotLocked(), true
}

func (p servicePortalModulePort) CommitMatrixSession(accessToken, deviceID string) portalmodule.Snapshot {
	p.service.mu.Lock()
	defer p.service.mu.Unlock()
	p.service.accessToken = accessToken
	if p.service.matrixDeviceID != deviceID {
		p.service.clientBuild = clientBuild{}
	}
	p.service.matrixDeviceID = deviceID
	p.service.portalSessionGeneration++
	return p.snapshotLocked()
}

func (p servicePortalModulePort) RuntimeStatus() portalmodule.RuntimeStatus {
	p.service.mu.Lock()
	defer p.service.mu.Unlock()
	return portalmodule.RuntimeStatus{
		Initialized: p.service.initialized, UserID: p.service.ownerMXID,
		Homeserver: p.service.homeserver, StoreMode: p.service.storeMode,
		ProjectorStarted: p.service.projectorStarted,
		MatrixPolicy:     p.service.transport != nil,
	}
}

func (p servicePortalModulePort) Save(ctx context.Context, state dirextalkdomain.PortalState) error {
	store := p.service.portalStore()
	if store == nil {
		return nil
	}
	return store.SavePortal(ctx, state)
}

func (p servicePortalModulePort) snapshotLocked() portalmodule.Snapshot {
	return portalmodule.Snapshot{
		State: p.service.portalStateLocked(),
		Session: portalmodule.Session{
			AccessToken: p.service.accessToken, DeviceID: cleanMatrixDeviceID(p.service.matrixDeviceID),
			UserID:     p.service.ownerMXID,
			Homeserver: p.service.homeserver, AgentRoomID: p.service.agentRoomID,
			SystemRoomID: p.service.systemRoomID, Password: p.service.password,
			Initialized: p.service.initialized,
		},
	}
}

type servicePortalMatrixPort struct{ service *Service }

func (p servicePortalMatrixPort) EnsureOwnerSession(ctx context.Context, deviceID string, revokeExistingDevices bool) (string, bool, error) {
	p.service.mu.Lock()
	issuer := p.service.sessions
	userID := p.service.profile.UserID
	displayName := p.service.profile.DisplayName
	avatarURL := p.service.profile.AvatarURL
	p.service.mu.Unlock()
	if issuer == nil {
		return "", false, nil
	}
	token, err := issuer.EnsureMatrixSession(ctx, userID, displayName, avatarURL, deviceID, revokeExistingDevices)
	return token, true, err
}

type servicePortalCredentialsPort struct{ service *Service }

func (p servicePortalCredentialsPort) WriteCurrent() error {
	p.service.mu.Lock()
	credentials := portalmodule.Credentials{
		Version: 1, GeneratedAt: time.Now().UTC(),
		OwnerUserID: p.service.ownerMXID, UserID: p.service.ownerMXID,
		Homeserver: p.service.homeserver, AccessToken: p.service.accessToken,
		DeviceID: matrixPortalDeviceID, AgentToken: p.service.agentToken,
		Password: p.service.password, AgentRoomID: p.service.agentRoomID,
		SystemRoomID: p.service.systemRoomID,
	}
	p.service.mu.Unlock()
	return portalmodule.WriteCurrent(portalmodule.CredentialsFilePath(), credentials)
}

func (p servicePortalCredentialsPort) WriteDeleted() error {
	p.service.mu.Lock()
	credentials := portalmodule.DeletedCredentials{
		Version: 1, Deprovisioned: true, DeletedAt: time.Now().UTC(),
		OwnerUserID: p.service.ownerMXID, UserID: p.service.ownerMXID,
		Homeserver: p.service.homeserver,
	}
	p.service.mu.Unlock()
	return portalmodule.WriteDeleted(portalmodule.CredentialsFilePath(), credentials)
}
