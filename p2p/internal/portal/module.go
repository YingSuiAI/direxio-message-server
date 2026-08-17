// Package portal owns portal authentication, session rotation, password, and
// runtime-status ProductCore workflows.
package portal

import (
	"context"
	"net/http"
	"sync"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

const (
	actionBootstrap = "portal.bootstrap"
	actionAuth      = "portal.auth"
	actionStatus    = "portal.status"
	actionPassword  = "portal.password"
)

type Session struct {
	AccessToken  string
	DeviceID     string
	UserID       string
	Homeserver   string
	AgentRoomID  string
	SystemRoomID string
	Password     string
	Initialized  bool
}

func (s Session) Response() map[string]any {
	return map[string]any{
		"access_token": s.AccessToken, "device_id": s.DeviceID,
		"user_id":    s.UserID,
		"homeserver": s.Homeserver, "agent_room_id": s.AgentRoomID,
		"system_room_id": s.SystemRoomID, "password": s.Password,
		"initialized": s.Initialized,
	}
}

type Snapshot struct {
	State   dirextalkdomain.PortalState
	Session Session
}

type RuntimeStatus struct {
	Initialized      bool
	UserID           string
	Homeserver       string
	StoreMode        string
	ProjectorStarted bool
	MatrixPolicy     bool
}

type StatePort interface {
	Authenticate(string) (Snapshot, bool)
	RotatePassword(string, string, func() string) (Snapshot, bool)
	CommitMatrixSession(string, string) Snapshot
	RuntimeStatus() RuntimeStatus
	Save(context.Context, dirextalkdomain.PortalState) error
}

type MatrixPort interface {
	EnsureOwnerSession(context.Context, string, bool) (string, bool, error)
}

type CredentialsPort interface {
	WriteCurrent() error
	WriteDeleted() error
}

type Config struct {
	NewAccessToken    func() string
	RequestedDeviceID func(map[string]any) string
}

type Module struct {
	state       StatePort
	matrix      MatrixPort
	sessionGate sync.Locker
	credentials CredentialsPort
	cfg         Config
}

func New(state StatePort, matrix MatrixPort, sessionGate sync.Locker, credentials CredentialsPort, cfg Config) *Module {
	return &Module{state: state, matrix: matrix, sessionGate: sessionGate, credentials: credentials, cfg: cfg}
}

func (m *Module) Handlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{
		actionBootstrap: m.bootstrap,
		actionAuth:      m.auth,
		actionStatus:    m.status,
		actionPassword:  m.changePassword,
	}
}

func (m *Module) WriteCurrentCredentials() error {
	return m.credentials.WriteCurrent()
}

func (m *Module) WriteDeletedCredentials() error {
	return m.credentials.WriteDeleted()
}

func (m *Module) bootstrap(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	password := actionbase.Params(params).FirstString("password", "token")
	if password == "" {
		return nil, actionbase.BadRequest("password is required")
	}
	snapshot, ok := m.state.Authenticate(password)
	if !ok {
		return nil, actionbase.StatusError(http.StatusUnauthorized, "password invalid")
	}
	if err := m.state.Save(ctx, snapshot.State); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if err := m.credentials.WriteCurrent(); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return m.refreshMatrixSession(ctx, snapshot.Session, params)
}

func (m *Module) auth(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	password := actionbase.Params(params).String("password")
	snapshot, ok := m.state.Authenticate(password)
	if password == "" || !ok {
		return nil, actionbase.StatusError(http.StatusUnauthorized, "password invalid")
	}
	return m.refreshMatrixSession(ctx, snapshot.Session, params)
}

func (m *Module) changePassword(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	values := actionbase.Params(params)
	newPassword := values.String("new_password")
	if newPassword == "" {
		return nil, actionbase.BadRequest("new_password is required")
	}
	m.sessionGate.Lock()
	snapshot, ok := m.state.RotatePassword(values.String("old_password"), newPassword, m.cfg.NewAccessToken)
	if !ok {
		m.sessionGate.Unlock()
		return nil, actionbase.StatusError(http.StatusUnauthorized, "password invalid")
	}
	if err := m.state.Save(ctx, snapshot.State); err != nil {
		m.sessionGate.Unlock()
		return nil, actionbase.InternalError(err)
	}
	if err := m.credentials.WriteCurrent(); err != nil {
		m.sessionGate.Unlock()
		return nil, actionbase.InternalError(err)
	}
	m.sessionGate.Unlock()
	return m.refreshMatrixSession(ctx, snapshot.Session, params)
}

func (m *Module) status(context.Context, map[string]any) (any, *actionbase.Error) {
	status := m.state.RuntimeStatus()
	policyMode := "unavailable"
	if status.MatrixPolicy {
		policyMode = "matrix_state"
	}
	return map[string]any{
		"initialized": status.Initialized, "user_id": status.UserID,
		"homeserver": status.Homeserver, "store_mode": status.StoreMode,
		"projector_started": status.ProjectorStarted,
		"policy_index_mode": policyMode, "policy_index_ready": status.MatrixPolicy,
		"event_stream_ready": true,
	}, nil
}

func (m *Module) refreshMatrixSession(ctx context.Context, session Session, params map[string]any) (any, *actionbase.Error) {
	m.sessionGate.Lock()
	defer m.sessionGate.Unlock()

	requestedDeviceID := m.cfg.RequestedDeviceID(params)
	token, configured, err := m.matrix.EnsureOwnerSession(ctx, requestedDeviceID, true)
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	if !configured {
		session.DeviceID = requestedDeviceID
		return session.Response(), nil
	}
	snapshot := m.state.CommitMatrixSession(token, requestedDeviceID)
	if err := m.state.Save(ctx, snapshot.State); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if err := m.credentials.WriteCurrent(); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return snapshot.Session.Response(), nil
}
