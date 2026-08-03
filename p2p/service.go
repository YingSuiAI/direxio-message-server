package p2p

import (
	"context"
	"errors"
	"fmt"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
	"github.com/YingSuiAI/dirextalk-message-server/internal/pushrules"
	"github.com/YingSuiAI/dirextalk-message-server/internal/realtime"
	"github.com/YingSuiAI/dirextalk-message-server/internal/releasecontrol"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	schedulesmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent/schedules"
	executionplanning "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionplanning"
	agentruntime "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/runtime"
	agentembeddedmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	blocksmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/blocks"
	callsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/calls"
	channelsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/channels"
	contactsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/contacts"
	conversationmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/conversation"
	eventsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/events"
	groupsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/groups"
	mcpmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/mcp"
	membersmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/members"
	operationsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/operations"
	pluginsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/plugins"
	portalmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/portal"
	profilemodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/profile"
	projectormodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/projector"
	realtimewsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/realtimews"
	releasemodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/release"
	reportsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/reports"
	socialmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/social"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type Config struct {
	ServerName                      string
	Homeserver                      string
	RemoteNodeInsecureSkipTLSVerify bool
	RemoteNodeAllowPrivateBaseURLs  bool
	P2PEventRetentionMaxRows        int64
	P2PEventRetentionPruneOnWrite   bool
	PushRules                       PushRuleManager
	RealtimeSessions                *realtime.SessionStore
	PluginRunner                    PluginRunner
	NativeAgentRunner               NativeAgentRunner
	NativeAgentDataDir              string
	ModelProfileKeyFile             string
	AgentSecretKeyringFile          string
	AgentArtifactDir                string
	ReleaseController               releasecontrol.Controller
	CentralVersionSource            releasecontrol.CentralVersionSource
	// ExecutionV2 is an explicit, dedicated action-port seam. It is never
	// routed through the generic embedded task worker.
	ExecutionV2               agentembeddedmodule.ExecutionV2Config
	ExecutionPlanningSources  executionplanning.SourceResolver
	ExecutionPlanningTargets  executionplanning.TargetResolver
	ExecutionPlanningBindings executionplanning.StepBindingResolver
}

const (
	ownerLocalpart            = "owner"
	agentLocalpart            = "agent"
	agentRoomName             = "Agents"
	defaultNativeAgentDataDir = "/var/dirextalk-message-server/agent"
)

func nativeAgentDataDir(configured string) string {
	if dataDir := strings.TrimSpace(configured); dataDir != "" {
		return dataDir
	}
	if dataDir := strings.TrimSpace(os.Getenv("P2P_NATIVE_AGENT_DATA_DIR")); dataDir != "" {
		return dataDir
	}
	return defaultNativeAgentDataDir
}

func agentArtifactDir(configured, dataDir string) string {
	if artifactDir := strings.TrimSpace(configured); artifactDir != "" {
		return artifactDir
	}
	if artifactDir := strings.TrimSpace(os.Getenv("P2P_AGENT_ARTIFACT_DIR")); artifactDir != "" {
		return artifactDir
	}
	return filepath.Join(dataDir, "artifacts")
}

func transportWriteError(err error) *apiError {
	if err == nil {
		return nil
	}
	var policyErr *productpolicy.PolicyError
	if errors.As(err, &policyErr) {
		status := policyErr.Code
		if status <= 0 {
			status = http.StatusForbidden
		}
		return statusError(status, policyErr.Message)
	}
	return internalError(err)
}

type Service struct {
	mu                 sync.Mutex
	matrixSessionMu    sync.Mutex
	accountOperationMu sync.RWMutex
	memberWritesMu     sync.Mutex
	systemRoomMu       sync.Mutex
	memberWrites       map[string]*memberWriteEntry

	serverName                string
	homeserver                string
	store                     Store
	transport                 Transport
	pushRules                 PushRuleManager
	sessions                  MatrixSessionIssuer
	matrixMessages            matrixMessageReader
	matrixProfiles            matrixProfileResolver
	readMarkerPositions       readMarkerPositionResolver
	remoteHTTPClient          *http.Client
	remoteAllowPrivate        bool
	accountDeactivator        AccountDeactivator
	accountDeprovisioner      AccountDeprovisioner
	accountDeletionInProgress bool
	accountDeprovisioned      bool
	storeMode                 string
	projectorStarted          bool
	agentModule               *agentmodule.Module
	scheduleModule            *schedulesmodule.Module
	scheduleRunning           bool
	agentRuntimeStarted       bool
	agentEmbedded             *agentembeddedmodule.Module
	agentTaskExecutor         *embeddedTaskExecutor
	agentTaskRuntime          *agentruntime.Worker
	agentScheduleLoop         *agentruntime.ScheduleLoop
	agentRuntimeInitErr       error
	executionV2Ready          func() bool
	executionV2PlanReady      func() bool
	executionV2ObserveReady   func() bool
	executionV2RunReady       func() bool
	executionV2BindingsReady  func() bool
	executionV2InvokeReady    func() bool
	executionV2TransportReady func() bool
	executionV2ProvisionReady func() bool
	executionV2SecretsReady   func() bool
	executionV2Runtime        *ExecutionV2Runtime
	executionV2RuntimeInitErr error

	agentConfirmationSweep         func(context.Context, string, time.Time) error
	agentConfirmationSweepInterval time.Duration
	agentSecretGuard               *p2pstorage.AgentSecretRuntimeGuard
	agentSecretGuardCloseOnce      sync.Once
	agentSecretEnveloper           *p2pstorage.AgentSecretEnveloper
	agentSecretKeyringFile         string
	agentSecretReady               bool
	modelProfiles                  p2pstorage.ModelProfileStore
	modelProfileInitErr            error
	mcpModule                      *mcpmodule.Module
	mcpCapabilities                *dirextalkmcp.Service
	releaseController              releasecontrol.Controller

	servicePortalState
	actions              map[string]actionHandler
	blocksModule         *blocksmodule.Module
	callsModule          *callsmodule.Module
	channelsModule       *channelsmodule.Module
	channelContentModule *channelsmodule.ContentModule
	contactsModule       *contactsmodule.Module
	conversationModule   *conversationmodule.Module
	eventsModule         *eventsmodule.Module
	groupsModule         *groupsmodule.Module
	membersModule        *membersmodule.Module
	pluginsModule        *pluginsmodule.Module
	portalModule         *portalmodule.Module
	profileModule        *profilemodule.Module
	projectorModule      *projectormodule.Module
	realtimeModule       *realtimewsmodule.Module
	releaseModule        *releasemodule.Module
	reportsModule        *reportsmodule.Module
	socialModule         *socialmodule.Module

	serviceOperationState
}

func executionV2BindingReadsReady(cfg agentembeddedmodule.ExecutionV2Config) bool {
	return cfg.Store != nil && cfg.BindingsReady != nil && cfg.BindingsReady()
}

func executionV2HTTPAPIInvokeReady(cfg agentembeddedmodule.ExecutionV2Config) bool {
	return cfg.Invoke != nil && cfg.InvokeReady != nil && cfg.InvokeReady()
}

type PushRuleManager interface {
	QueryPushRules(ctx context.Context, userID string) (*pushrules.AccountRuleSets, error)
	PerformPushRulesPut(ctx context.Context, userID string, ruleSets *pushrules.AccountRuleSets) error
}

type AccountDeactivator interface {
	DeactivateAccount(ctx context.Context, localpart string) error
}

type AccountDeprovisioner interface {
	DeprovisionAccount(ctx context.Context) error
}

type Store interface {
	agentturns.Store
	operationsmodule.Store
	portalStore
	readMarkerStore
	channelStore
	channelsmodule.ContentStore
	contactStore
	channelInviteGrantStore
	blockStore
	groupStore
	callStore
	socialStore
	memberStore
	conversationStore
	eventStore
	pluginsmodule.Store
	reportsmodule.Store
}

type socialStore = socialmodule.Store
type callStore = callsmodule.Store
type blockStore = blocksmodule.Store
type channelStore = channelsmodule.Store
type contactStore = contactsmodule.Store
type eventStore = eventsmodule.Store
type groupStore = groupsmodule.Store

type portalState = dirextalkdomain.PortalState
type ownerProfile = dirextalkdomain.OwnerProfile
type agentConfig = dirextalkdomain.AgentConfig

const matrixPortalDeviceID = "P2P_PORTAL"

type readMarker = dirextalkdomain.ReadMarker
type channel = dirextalkdomain.Channel
type channelInviteGrant = dirextalkdomain.ChannelInviteGrant
type channelPostRecord = channelsmodule.Post
type channelCommentRecord = channelsmodule.Comment
type contactRecord = contactsmodule.View
type blockRecord = dirextalkdomain.BlockRecord
type groupRecord = groupsmodule.View
type callRecord = dirextalkdomain.CallRecord
type favoriteRecord = dirextalkdomain.FavoriteRecord
type followRecord = dirextalkdomain.FollowRecord
type reactionRecord = dirextalkdomain.ReactionRecord
type memberRecord = dirextalkdomain.MemberRecord
type clientBuild = dirextalkdomain.ClientBuild

func NewService(cfg Config) *Service {
	return newService(cfg, p2pstorage.NewMemoryStore(), nil, portalState{}, false)
}

func NewServiceWithTransport(cfg Config, transport Transport) *Service {
	return newService(cfg, p2pstorage.NewMemoryStore(), transport, portalState{}, false)
}

func NewServiceWithStore(ctx context.Context, cfg Config, store Store) (*Service, error) {
	return NewServiceWithStoreAndTransport(ctx, cfg, store, nil)
}

func NewServiceWithStoreAndTransport(ctx context.Context, cfg Config, store Store, transport Transport) (*Service, error) {
	portalStore := portalStoreFrom(store)
	state, ok, err := portalStore.LoadPortal(ctx)
	if err != nil {
		return nil, err
	}
	migratedAgentConfig, err := migrateLegacyAgentPluginConfig(ctx, store, &state)
	if err != nil {
		return nil, err
	}
	shouldPersist := !ok || !state.Initialized || strings.TrimSpace(state.Password) == "" || migratedAgentConfig
	service := newService(cfg, store, transport, state, ok)
	if err := service.agentModule.ReadyError(); err != nil {
		return nil, err
	}
	if err := service.pluginsModule.CheckStore(ctx); err != nil {
		return nil, err
	}
	agentRoomChanged, err := service.ensureAgentRoom(ctx)
	if err != nil {
		return nil, err
	}
	systemRoomChanged, err := service.ensureSystemRoom(ctx)
	if err != nil {
		return nil, err
	}
	if shouldPersist || agentRoomChanged || systemRoomChanged {
		service.mu.Lock()
		state = service.portalStateLocked()
		service.mu.Unlock()
		if err := portalStore.SavePortal(ctx, state); err != nil {
			return nil, err
		}
	}
	if err := service.portalModule.WriteCurrentCredentials(); err != nil {
		return nil, err
	}
	if err := service.repairLocalChannelOwnerRoles(ctx); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *Service) ensureAgentRoom(ctx context.Context) (bool, error) {
	s.mu.Lock()
	currentRoomID := strings.TrimSpace(s.agentRoomID)
	ownerMXID := s.ownerMXID
	ownerDisplayName := s.profile.DisplayName
	ownerAvatarURL := s.profile.AvatarURL
	agentMXID := s.agentMXIDLocked()
	agentDisplayName := s.agentDisplayNameLocked()
	s.mu.Unlock()
	if s.transport == nil {
		return false, nil
	}
	if !needsAgentRoomCreate(currentRoomID, s.serverName) {
		if currentRoomID != "" {
			if err := s.ensureAgentRoomAgentMember(ctx, currentRoomID, ownerMXID, agentMXID, agentDisplayName); err != nil {
				return false, err
			}
			if err := s.ensureAgentRoomOwnerMember(ctx, currentRoomID, ownerMXID, ownerDisplayName, agentMXID); err != nil {
				return false, err
			}
			if err := s.ensureAgentRoomPowerLevels(ctx, currentRoomID, ownerMXID, agentMXID); err != nil {
				return false, err
			}
			if err := s.publishAgentStatusState(ctx, currentRoomID, agentMXID, agentMXID, false); err != nil {
				return false, err
			}
			if err := s.ensureAgentRoomPushRule(ctx, currentRoomID, ownerMXID); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	res, err := s.transport.CreateRoom(ctx, CreateRoomRequest{
		CreatorMXID:        ownerMXID,
		CreatorDisplayName: ownerDisplayName,
		CreatorAvatarURL:   ownerAvatarURL,
		Name:               agentRoomName,
		Topic:              "Dirextalk agents room",
		Visibility:         "private",
		InviteMXIDs:        []string{agentMXID},
		InitialState:       []RoomStateEvent{agentRoomPowerLevelsStateEvent(ownerMXID, agentMXID)},
	})
	if err != nil {
		return false, err
	}
	roomID := strings.TrimSpace(res.RoomID)
	if roomID == "" {
		return false, errors.New("agent room creation returned empty room_id")
	}
	s.mu.Lock()
	s.agentRoomID = roomID
	s.mu.Unlock()
	if err := s.ensureAgentRoomAgentMember(ctx, roomID, ownerMXID, agentMXID, agentDisplayName); err != nil {
		return false, err
	}
	if err := s.publishAgentStatusState(ctx, roomID, agentMXID, agentMXID, false); err != nil {
		return false, err
	}
	if err := s.ensureAgentRoomPushRule(ctx, roomID, ownerMXID); err != nil {
		return false, err
	}
	return roomID != currentRoomID, nil
}

func (s *Service) ensureAgentRoomPushRule(ctx context.Context, roomID, ownerMXID string) error {
	roomID = strings.TrimSpace(roomID)
	ownerMXID = strings.TrimSpace(ownerMXID)
	if roomID == "" || ownerMXID == "" {
		return nil
	}
	s.mu.Lock()
	pushRulesAPI := s.pushRules
	serverName := s.serverName
	s.mu.Unlock()
	if pushRulesAPI == nil {
		return nil
	}
	ruleSets, err := pushRulesAPI.QueryPushRules(ctx, ownerMXID)
	if err != nil {
		return err
	}
	if ruleSets == nil {
		ruleSets = pushrules.DefaultAccountRuleSets(ownerLocalpart, spec.ServerName(serverName))
	}
	for _, rule := range ruleSets.Global.Room {
		if rule != nil && rule.RuleID == roomID {
			return nil
		}
	}
	ruleSets.Global.Room = append([]*pushrules.Rule{{
		RuleID:  roomID,
		Default: false,
		Enabled: true,
		Actions: []*pushrules.Action{},
	}}, ruleSets.Global.Room...)
	return pushRulesAPI.PerformPushRulesPut(ctx, ownerMXID, ruleSets)
}

func (s *Service) ensureAgentRoomPowerLevels(ctx context.Context, roomID, ownerMXID, agentMXID string) error {
	if s.transport == nil || strings.TrimSpace(roomID) == "" || strings.TrimSpace(ownerMXID) == "" {
		return nil
	}
	return s.transport.SendStateEvent(ctx, SendStateEventRequest{
		RoomID:     strings.TrimSpace(roomID),
		SenderMXID: strings.TrimSpace(ownerMXID),
		Event:      agentRoomPowerLevelsStateEvent(ownerMXID, agentMXID),
	})
}

func (s *Service) ensureAgentRoomAgentMember(ctx context.Context, roomID, ownerMXID, agentMXID, agentDisplayName string) error {
	if strings.TrimSpace(roomID) == "" || strings.TrimSpace(agentMXID) == "" {
		return nil
	}
	if _, err := s.transport.JoinRoom(ctx, JoinRoomRequest{
		RoomIDOrAlias: roomID,
		UserMXID:      agentMXID,
		DisplayName:   agentDisplayName,
	}); err == nil {
		return nil
	}
	if strings.TrimSpace(ownerMXID) != "" {
		if err := s.transport.InviteUser(ctx, InviteUserRequest{
			RoomID:      roomID,
			InviterMXID: ownerMXID,
			InviteeMXID: agentMXID,
			Reason:      "Dirextalk agents gateway",
		}); err != nil {
			return err
		}
	}
	_, err := s.transport.JoinRoom(ctx, JoinRoomRequest{
		RoomIDOrAlias: roomID,
		UserMXID:      agentMXID,
		DisplayName:   agentDisplayName,
	})
	return err
}

func (s *Service) ensureAgentRoomOwnerMember(ctx context.Context, roomID, ownerMXID, ownerDisplayName, agentMXID string) error {
	if strings.TrimSpace(roomID) == "" || strings.TrimSpace(ownerMXID) == "" {
		return nil
	}
	if _, err := s.transport.JoinRoom(ctx, JoinRoomRequest{
		RoomIDOrAlias: roomID,
		UserMXID:      ownerMXID,
		DisplayName:   ownerDisplayName,
	}); err == nil {
		return nil
	}
	if strings.TrimSpace(agentMXID) != "" {
		if err := s.transport.InviteUser(ctx, InviteUserRequest{
			RoomID:      roomID,
			InviterMXID: agentMXID,
			InviteeMXID: ownerMXID,
			Reason:      "Dirextalk agents owner",
		}); err != nil {
			return err
		}
	}
	_, err := s.transport.JoinRoom(ctx, JoinRoomRequest{
		RoomIDOrAlias: roomID,
		UserMXID:      ownerMXID,
		DisplayName:   ownerDisplayName,
	})
	return err
}

func (s *Service) agentMXIDLocked() string {
	return "@" + agentLocalpart + ":" + strings.TrimSpace(s.serverName)
}

func (s *Service) agentDisplayNameLocked() string {
	return fallbackString(strings.TrimSpace(s.agentConfig.DisplayName), "Agent")
}

func needsAgentRoomCreate(roomID, serverName string) bool {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return true
	}
	if strings.EqualFold(roomID, "!agent:"+strings.TrimSpace(serverName)) {
		return true
	}
	return strings.HasPrefix(roomID, "!agent:")
}

func storeMode(store Store) string {
	if store == nil {
		return "memory"
	}
	if _, ok := store.(*p2pstorage.MemoryStore); ok {
		return "memory"
	}
	return "database"
}

func ownerMXIDForServer(serverName string) string {
	return "@" + ownerLocalpart + ":" + serverName
}

func (s *Service) SetMatrixSessionIssuer(issuer MatrixSessionIssuer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = issuer
}

func (s *Service) SetPushRuleManager(manager PushRuleManager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushRules = manager
}

func (s *Service) SetMatrixMessageReader(reader matrixMessageReader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matrixMessages = reader
}

func (s *Service) SetReadMarkerPositionResolver(resolver readMarkerPositionResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readMarkerPositions = resolver
}

func (s *Service) SetMatrixProfileResolver(resolver matrixProfileResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matrixProfiles = resolver
}

func (s *Service) SetAccountDeactivator(deactivator AccountDeactivator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountDeactivator = deactivator
}

func (s *Service) SetAccountDeprovisioner(deprovisioner AccountDeprovisioner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountDeprovisioner = deprovisioner
}

func (s *Service) SetProjectorStarted(started bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectorStarted = started
}

func newService(cfg Config, store Store, transport Transport, state portalState, hasPortal bool) *Service {
	serverName := strings.TrimSpace(cfg.ServerName)
	if serverName == "" {
		serverName = "localhost"
	}
	homeserver := strings.TrimSpace(cfg.Homeserver)
	if homeserver == "" {
		homeserver = "https://" + serverName
	}
	if !hasPortal {
		ownerMXID := ownerMXIDForServer(serverName)
		agentConfig := state.AgentConfig
		state = portalState{
			Initialized:    false,
			Password:       defaultPortalPassword(),
			AccessToken:    randomToken("p2p_access"),
			MatrixDeviceID: matrixPortalDeviceID,
			AgentToken:     randomToken("p2p_agent"),
			OwnerMXID:      ownerMXID,
			Profile: ownerProfile{
				UserID: ownerMXID,
				Domain: serverName,
			},
			AgentConfig: agentConfig,
		}
	}
	if strings.TrimSpace(state.Password) == "" {
		state.Password = defaultPortalPassword()
	}
	if strings.TrimSpace(state.AccessToken) == "" {
		state.AccessToken = randomToken("p2p_access")
	}
	if state.MatrixDeviceID == "" {
		state.MatrixDeviceID = matrixPortalDeviceID
	}
	if state.AgentToken == "" {
		state.AgentToken = randomToken("p2p_agent")
	}
	if state.OwnerMXID == "" {
		state.OwnerMXID = ownerMXIDForServer(serverName)
	}
	if state.Profile.UserID == "" {
		state.Profile.UserID = state.OwnerMXID
	}
	if state.Profile.Domain == "" {
		state.Profile.Domain = serverName
	}
	state.AgentConfig = normalizeAgentConfig(state.AgentConfig)
	realtimeSessions := cfg.RealtimeSessions
	if realtimeSessions == nil {
		realtimeSessions = realtime.DefaultSessionStore
	}
	basePluginRunner := cfg.PluginRunner
	if basePluginRunner == nil {
		basePluginRunner = pluginsmodule.NewEnvironmentRunner()
	}
	service := &Service{
		serverName:         serverName,
		homeserver:         homeserver,
		store:              store,
		transport:          transport,
		pushRules:          cfg.PushRules,
		remoteHTTPClient:   newRemotePublicHTTPClient(cfg.RemoteNodeInsecureSkipTLSVerify),
		remoteAllowPrivate: cfg.RemoteNodeAllowPrivateBaseURLs,
		storeMode:          storeMode(store),
		releaseController:  cfg.ReleaseController,
		servicePortalState: servicePortalState{
			initialized:             state.Initialized,
			password:                state.Password,
			accessToken:             state.AccessToken,
			matrixDeviceID:          state.MatrixDeviceID,
			agentToken:              state.AgentToken,
			ownerMXID:               state.OwnerMXID,
			agentRoomID:             state.AgentRoomID,
			systemRoomID:            state.SystemRoomID,
			profile:                 state.Profile,
			agentConfig:             state.AgentConfig,
			clientBuild:             state.ClientBuild,
			portalSessionGeneration: 1,
		},
	}
	service.executionV2Ready = cfg.ExecutionV2.Ready
	service.executionV2PlanReady = cfg.ExecutionV2.PlanReady
	service.executionV2ObserveReady = cfg.ExecutionV2.ObserveReady
	service.executionV2RunReady = cfg.ExecutionV2.RunReady
	service.executionV2BindingsReady = cfg.ExecutionV2.BindingsReady
	service.executionV2InvokeReady = cfg.ExecutionV2.InvokeReady
	service.executionV2TransportReady = cfg.ExecutionV2.TransportAWSReady
	service.executionV2ProvisionReady = cfg.ExecutionV2.TargetReserveReady
	service.executionV2SecretsReady = cfg.ExecutionV2.SecretsReady
	service.eventsModule = eventsmodule.New(service.store, eventsmodule.Config{
		RetentionMaxRows:      cfg.P2PEventRetentionMaxRows,
		RetentionPruneOnWrite: cfg.P2PEventRetentionPruneOnWrite,
		Now:                   time.Now,
	})
	service.conversationModule = conversationmodule.New(service.store, serviceConversationHydrator{service: service})
	service.channelsModule = channelsmodule.New(service.store, service.conversationModule, service.store, channelsmodule.Config{
		NewChannelID: func() string { return "ch_" + randomToken("channel") },
		CreateRoom:   service.createChannelRoom,
		SaveOwnerMember: func(ctx context.Context, roomID, channelID string) error {
			return service.saveOwnerMember(ctx, roomID, channelID)
		},
		PublishState:       service.publishChannelState,
		PublishHistory:     service.publishChannelHistoryVisibilityState,
		SetMemberMute:      service.setChannelMemberMute,
		RequireOwner:       service.requireOwnerMember,
		OwnerMXID:          service.memberOwnerMXID,
		RemotePublicGet:    service.remotePublicChannelGet,
		FetchRoomChannel:   service.fetchRoomChannel,
		RemoteUserChannels: service.remoteUserPublicChannels,
		IsMatrixRoomID:     matrixRoomIDQuery,
	})
	service.channelContentModule = channelsmodule.NewContent(
		service.store,
		service.channelsModule,
		nil,
		service.conversationModule,
		channelsmodule.ContentConfig{
			Owner: func() channelsmodule.ContentOwner {
				service.mu.Lock()
				defer service.mu.Unlock()
				return channelsmodule.ContentOwner{MXID: service.ownerMXID, DisplayName: service.profile.DisplayName}
			},
			Matrix:   func() channelsmodule.MatrixContentPort { return service.transport },
			Now:      time.Now,
			NewToken: randomToken,
			NewEventID: func(contentID string) string {
				return "$" + contentID + ":" + service.serverName
			},
			RequireJoined:     service.requireJoinedChannelContent,
			AuthorizeRecall:   service.authorizeChannelContentRecall,
			MapTransportError: transportWriteError,
		},
	)
	service.groupsModule = groupsmodule.New(service.store, service.conversationModule, groupsmodule.Config{
		CreateRoom: service.createGroupRoom,
		SaveOwnerMember: func(ctx context.Context, roomID string) error {
			return service.saveOwnerMember(ctx, roomID, "")
		},
		PublishState:  service.publishGroupState,
		SetMemberMute: service.setGroupMemberMute,
		RequireOwner:  service.requireOwnerMember,
		OwnerMXID:     service.memberOwnerMXID,
	})
	service.reportsModule = reportsmodule.New(
		service.store,
		serviceReportTargetPort{service: service},
		serviceReportSystemRoomPort{service: service},
		serviceReportMatrixPort{service: service},
		service.conversationModule,
		reportsmodule.Config{
			NewReportID:       func() string { return "report_" + randomToken("report") },
			Now:               time.Now,
			MapTransportError: transportWriteError,
		},
	)
	service.pluginsModule = pluginsmodule.New(service.store, basePluginRunner, pluginsmodule.Config{
		Homeserver: service.homeserver,
		Now:        time.Now,
		NewJobID:   func() string { return randomToken("plugin_job") },
	})
	service.portalModule = portalmodule.New(
		servicePortalModulePort{service: service},
		servicePortalMatrixPort{service: service},
		&service.matrixSessionMu,
		servicePortalCredentialsPort{service: service},
		portalmodule.Config{
			NewAccessToken:    func() string { return randomToken("p2p_access") },
			RequestedDeviceID: requestedMatrixDeviceID,
		},
	)
	service.releaseModule = releasemodule.New(serviceReleasePort{service: service}, releasemodule.Config{
		SessionLocker:        &service.matrixSessionMu,
		Now:                  time.Now,
		CentralVersionSource: cfg.CentralVersionSource,
	})
	service.profileModule = profilemodule.New(serviceProfilePort{service: service})
	var joinDirectRoom contactsmodule.DirectRoomJoiner
	if service.transport != nil {
		joinDirectRoom = service.joinContactDirectRoomTransport
	}
	service.contactsModule = contactsmodule.New(service.store, service.conversationModule, contactsmodule.Config{
		ServerName:         service.serverName,
		AcceptDirectRoom:   service.acceptDirectContactRoom,
		VerifyAcceptedRoom: service.transport != nil,
		CreateDirectRoom:   service.createContactDirectRoom,
		InviteDirectRoom:   service.inviteContactDirectRoom,
		JoinDirectRoom:     joinDirectRoom,
		NewDirectRoomID: func() string {
			return "!dm-" + randomToken("room") + ":" + service.serverName
		},
		LocalProfile:         service.localContactProfileSnapshot,
		ReactivatePeer:       service.reactivatePeerContact,
		ReactivateDirectRoom: service.reactivateRetainedDirectRoom,
		MatrixJoined:         service.matrixMemberJoined,
		CheckPeerBlocked: func(ctx context.Context, peerMXID string) (bool, error) {
			if service.blocksModule == nil {
				return false, errors.New("blocks module is not configured")
			}
			return service.blocksModule.Exists(ctx, "contact", peerMXID)
		},
		DeleteGroup: func(ctx context.Context, roomID string) error {
			return service.store.DeleteGroup(ctx, roomID)
		},
		LeaveRoom: func(ctx context.Context, roomID string) *apiError {
			if service.transport == nil {
				return nil
			}
			service.mu.Lock()
			ownerMXID := service.ownerMXID
			service.mu.Unlock()
			if err := service.transport.LeaveRoom(ctx, LeaveRoomRequest{RoomID: roomID, UserMXID: ownerMXID}); err != nil && !isAlreadyLeftRoomError(err) {
				return transportWriteError(err)
			}
			return nil
		},
	})
	service.blocksModule = blocksmodule.New(service.store, blocksmodule.Config{
		LookupContact: func(ctx context.Context, peerMXID string) (dirextalkdomain.ContactRecord, bool, error) {
			contact, ok, err := service.lookupContactByPeer(ctx, peerMXID)
			return contactStorageRecordFromContact(contact), ok, err
		},
	})
	service.membersModule = membersmodule.New(service.store, membersmodule.Config{
		ResolveTarget:            service.memberTarget,
		NewMember:                service.memberRecordFor,
		LookupMember:             service.lookupMember,
		SaveMember:               service.saveMember,
		SaveMemberGeneration:     service.saveMemberIfState,
		PublishPolicy:            service.publishMemberPolicyState,
		Conversation:             service.conversationModule,
		ResolveRoomOwner:         service.resolveRoomOwner,
		OwnerMXID:                service.memberOwnerMXID,
		KickMember:               service.kickMember,
		LeaveMember:              service.leaveMember,
		PublishJoinRequest:       service.publishJoinRequestState,
		CompleteJoinRequest:      service.completeChannelJoinRequest,
		LookupChannel:            service.channelByIDOrRoom,
		RequireOwner:             service.requireOwnerMember,
		RejectBlocked:            service.rejectIfBlocked,
		PrepareInvite:            service.prepareMemberInvite,
		ShareRoomMembers:         service.shareRoomMembersForInviteGrant,
		ChannelSnapshot:          service.channelSnapshot,
		ApplyLocalProfile:        service.applyLocalOwnerMemberProfile,
		MatrixJoined:             service.matrixMemberJoined,
		JoinRetained:             service.joinAndProjectRetainedRoom,
		SaveRetainedMetadata:     service.saveRetainedRoomInviteMetadata,
		ForwardPublicJoinRequest: service.remoteChannelJoinRequest,
		EmitJoinRequestChanged: func(ctx context.Context, member memberRecord, status string) {
			_ = service.appendP2PEvent(ctx, p2pEvent{
				Type:    "channel.join_request.changed",
				RoomID:  member.RoomID,
				Payload: map[string]any{"user_id": member.UserID, "status": status, "channel_id": member.ChannelID},
			})
		},
		NewGrantID:   func() string { return "grant_" + randomToken("channel_invite") },
		NewRequestID: func() string { return "request_" + randomToken("channel_join") },
		Now:          time.Now,
	})
	service.projectorModule = projectormodule.New(projectormodule.Dependencies{
		Events:         service.eventsModule,
		Conversations:  service.conversationModule,
		Channels:       serviceProjectorChannelPort{service: service},
		ChannelContent: service.channelContentModule,
		Groups:         serviceProjectorGroupPort{service: service},
		Contacts:       serviceProjectorContactPort{service: service},
		Members:        serviceProjectorMemberPort{service: service},
		Blocks:         service.blocksModule,
		DirectRooms:    serviceProjectorDirectRoomPort{service: service},
		Identity: func() projectormodule.IdentitySnapshot {
			service.mu.Lock()
			defer service.mu.Unlock()
			return projectormodule.IdentitySnapshot{
				OwnerMXID:        service.ownerMXID,
				OwnerDisplayName: service.profile.DisplayName,
				OwnerAvatarURL:   service.profile.AvatarURL,
				AgentRoomID:      service.agentRoomID,
			}
		},
	}, projectormodule.Config{Now: time.Now})
	service.callsModule = callsmodule.New(service.store, callsmodule.Config{
		ServerName:   service.serverName,
		OwnerMXID:    service.ownerMXID,
		NewCallID:    func() string { return "call_" + randomToken("p2p") },
		PublishEvent: service.appendP2PEvent,
	})
	service.socialModule = socialmodule.New(service.store, socialmodule.Config{})
	service.mcpModule = mcpmodule.New(mcpmodule.Dependencies{
		Conversations:  service.conversationModule,
		Contacts:       service.contactsModule,
		Channels:       service.channelsModule,
		ChannelContent: service.channelContentModule,
		Groups:         service.groupsModule,
		Members:        service.store,
		Social:         service.socialModule,
		Matrix:         service.transport,
	}, mcpmodule.Config{
		Identity: func() mcpmodule.Identity {
			service.mu.Lock()
			defer service.mu.Unlock()
			return mcpmodule.Identity{
				OwnerMXID:        service.ownerMXID,
				OwnerProfile:     service.profile,
				AgentMXID:        service.agentMXIDLocked(),
				AgentDisplayName: service.agentDisplayNameLocked(),
				AgentRoomID:      service.agentRoomID,
				BlockedRoomIDs:   append([]string(nil), service.agentConfig.MCPBlockedRoomIDs...),
			}
		},
		MessageReader: func() dirextalkmcp.MessageReader {
			service.mu.Lock()
			defer service.mu.Unlock()
			return service.matrixMessages
		},
		ProfileResolver: func() mcpmodule.ProfileResolver {
			service.mu.Lock()
			defer service.mu.Unlock()
			return service.matrixProfiles
		},
		ResolveRoomOwner:      service.resolveRoomOwner,
		BeginAccountOperation: service.beginAccountOperation,
		AccountDeprovisioned:  service.accountIsDeprovisioned,
		AgentRoomName:         agentRoomName,
		Now:                   time.Now,
	})
	service.mcpCapabilities = service.mcpModule.Service()
	agentDataDir := nativeAgentDataDir(cfg.NativeAgentDataDir)
	if profileStore, ok := store.(p2pstorage.ModelProfileStore); ok {
		service.modelProfiles = profileStore
	} else if dbStore, ok := store.(*p2pstorage.DatabaseStore); ok {
		keyringFile := strings.TrimSpace(cfg.AgentSecretKeyringFile)
		if keyringFile == "" {
			keyringFile = filepath.Join(agentDataDir, "secret-keyring.json")
		}
		legacyKeyFile := strings.TrimSpace(cfg.ModelProfileKeyFile)
		if legacyKeyFile == "" {
			legacyKeyFile = filepath.Join(agentDataDir, "model-profile.master.key")
		}
		service.agentSecretKeyringFile = keyringFile
		// Initialize an absent keyring only after the database migrations have
		// completed and the exclusive maintenance guard has proved that no
		// keyring-bound ciphertext exists. Existing or corrupt keyrings are
		// still loaded fail-closed; legacy model-profile rows remain a separate
		// explicit upgrade concern.
		keyring, guard, secretErr := p2pstorage.BootstrapAgentSecretRuntime(context.Background(), dbStore.DB(), keyringFile)
		if secretErr == nil {
			service.agentSecretGuard = guard
		}
		if secretErr == nil {
			service.agentSecretEnveloper, secretErr = p2pstorage.NewAgentSecretEnveloper(keyring)
		}
		if secretErr == nil {
			secretErr = p2pstorage.VerifyAgentSecretDatabase(context.Background(), dbStore.DB(), p2pstorage.AgentSecretRotationOptions{
				KeyringFile:               keyringFile,
				LegacyModelProfileKeyFile: legacyKeyFile,
			})
		}
		if secretErr == nil {
			service.modelProfiles, secretErr = p2pstorage.NewDatabaseModelProfileStoreWithKeyring(context.Background(), dbStore, keyringFile, legacyKeyFile)
		}
		if secretErr != nil {
			if service.agentSecretGuard != nil {
				_ = service.agentSecretGuard.Close()
				service.agentSecretGuard = nil
			}
			service.agentSecretEnveloper = nil
			service.modelProfileInitErr = secretErr
		} else {
			service.agentSecretReady = true
		}
	}
	var scheduleStore p2pstorage.ScheduleStore
	if candidate, ok := store.(p2pstorage.ScheduleStore); ok {
		scheduleStore = candidate
	}
	var scheduleMaterializer schedulesmodule.OccurrenceMaterializer
	if dbStore, ok := store.(*p2pstorage.DatabaseStore); ok {
		scheduleMaterializer = embeddedScheduleMaterializer{store: dbStore}
	}
	service.scheduleModule = schedulesmodule.New(schedulesmodule.Config{Store: scheduleStore, Profiles: service.modelProfiles, Materializer: scheduleMaterializer, OwnerID: service.OwnerMXID, SchedulerReady: func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return service.scheduleRunning
	}})
	agentModelProfiles := service.modelProfiles
	if agentModelProfiles != nil && !agentModelProfiles.ModelProfileStoreReady() {
		agentModelProfiles = nil
	}
	service.agentModule = agentmodule.New(agentmodule.Config{
		Runner: cfg.NativeAgentRunner, DataDir: cfg.NativeAgentDataDir,
		Store: nativeAgentConfigStore{service: service}, MCP: service.mcpCapabilities,
		Control: agentmodule.ControlInvokerAdapter{
			Ready: func(action string) bool {
				return service.nativeAgentControlActionReady(action)
			},
			Call: func(ctx context.Context, action string, params map[string]any) (any, error) {
				// The embedded module and its late-bound ports are assembled below.
				// Resolving the handler at invocation time keeps capability readiness
				// dynamic and avoids capturing a partially initialized service.
				module := service.agentEmbedded
				if module == nil {
					return nil, agentembeddedmodule.ErrUnavailable
				}
				handler := module.Handlers()[strings.TrimSpace(action)]
				if handler == nil {
					return nil, fmt.Errorf("native agent control action %q is unavailable", action)
				}
				value, actionErr := handler(ctx, params)
				if actionErr != nil {
					return nil, fmt.Errorf("%s", actionErr.Error)
				}
				return value, nil
			},
		},
		ScheduleTools: service.scheduleModule.Tools(), Account: serviceAgentAccountPort{service: service}, Turns: service.store,
		OwnerID: service.OwnerMXID, ModelProfiles: agentModelProfiles,
		VoiceEnabled: true,
		VoiceActive: func(owner string) bool {
			return strings.TrimSpace(owner) == strings.TrimSpace(service.OwnerMXID()) && !service.accountIsDeprovisioned()
		},
		VoiceGeneration: func() uint64 { service.mu.Lock(); defer service.mu.Unlock(); return service.portalSessionGeneration },
		Memory: func() nativeagent.ConversationMemoryStore {
			if candidate, ok := store.(nativeagent.ConversationMemoryStore); ok {
				return candidate
			}
			return nil
		}(),
		PersistentMemoryReady: func() bool { _, ok := store.(*p2pstorage.DatabaseStore); return ok }(),
		ModelProfileResolver: func() nativeagent.ModelProfileResolver {
			if service.modelProfiles == nil {
				return nil
			}
			return nativeModelProfileResolver{store: service.modelProfiles, owner: service.OwnerMXID}
		}(),
	})
	embeddedConfig := agentembeddedmodule.Config{
		OwnerID:         service.OwnerMXID,
		ModelProfiles:   service.modelProfiles,
		Schedules:       scheduleStore,
		CapabilityReady: service.embeddedAgentCapabilityReady,
	}
	// Execution.v2 has an independent port and readiness hook. The generic
	// embedded task worker is intentionally not used for this surface.
	executionV2Config := cfg.ExecutionV2
	if dbStore, ok := store.(*p2pstorage.DatabaseStore); ok {
		if executionV2Config.Store == nil {
			executionV2Config.Store = p2pstorage.NewDatabaseExecutionStore(dbStore.DB(), time.Now)
		}
		if executionV2Config.Secrets == nil && service.agentSecretEnveloper != nil {
			secretStore := p2pstorage.NewDatabaseExecutionSecretStore(dbStore.DB(), service.agentSecretEnveloper, time.Now)
			executionV2Config.Secrets = secretStore
			executionV2Config.SecretsReady = secretStore.Ready
		}
		artifactDir := agentArtifactDir(cfg.AgentArtifactDir, agentDataDir)
		if service.agentSecretEnveloper != nil {
			service.executionV2Runtime, service.executionV2RuntimeInitErr = NewExecutionV2Runtime(ExecutionV2RuntimeConfig{
				Store:           dbStore,
				OwnerID:         strings.TrimSpace(service.OwnerMXID()),
				ArtifactDir:     artifactDir,
				SecretEnveloper: service.agentSecretEnveloper,
				WorkerID:        "execution-v2",
				Clock:           time.Now,
			})
			if service.executionV2RuntimeInitErr == nil {
				// The runtime store/coordinator share one authoritative executor
				// catalog. Publishing a separately constructed coordinator would
				// bypass admission checks for unavailable executors.
				if executionV2Config.Coordinator == nil {
					executionV2Config.Coordinator = service.executionV2Runtime.coord
				}
				if executionV2Config.Ready == nil {
					executionV2Config.Ready = service.executionV2Runtime.Ready
				}
				if executionV2Config.Observe == nil {
					executionV2Config.Observe = service.executionV2Runtime
				}
				if executionV2Config.TargetImport == nil {
					executionV2Config.TargetImport = service.executionV2Runtime
				}
				if executionV2Config.ObserveReady == nil {
					executionV2Config.ObserveReady = service.executionV2Runtime.ObserveReady
				}
				if executionV2Config.TargetImportReady == nil {
					executionV2Config.TargetImportReady = service.executionV2Runtime.TargetImportReady
				}
				if executionV2Config.TargetReserve == nil {
					executionV2Config.TargetReserve = service.executionV2Runtime
				}
				if executionV2Config.TargetReserveReady == nil {
					executionV2Config.TargetReserveReady = service.executionV2Runtime.ProvisionReady
				}
				if executionV2Config.RunReady == nil {
					executionV2Config.RunReady = service.executionV2Runtime.Ready
				}
				if executionV2Config.Reconcile == nil {
					executionV2Config.Reconcile = service.executionV2Runtime
				}
				if executionV2Config.ReconcileReady == nil {
					executionV2Config.ReconcileReady = service.executionV2Runtime.ReconcileReady
				}
				if executionV2Config.BindingsReady == nil {
					executionV2Config.BindingsReady = service.executionV2Runtime.BindingsReady
				}
				if executionV2Config.TransportAWSReady == nil {
					executionV2Config.TransportAWSReady = service.executionV2Runtime.Ready
				}
			}
		}
		if executionV2Config.Ready == nil {
			executionV2Config.Ready = func() bool { return false }
		}
		planningSources := cfg.ExecutionPlanningSources
		planningTargets := cfg.ExecutionPlanningTargets
		planningBindings := cfg.ExecutionPlanningBindings
		if executionStore, storeOK := executionV2Config.Store.(*p2pstorage.DatabaseExecutionStore); storeOK {
			if planningTargets == nil {
				planningTargets = executionplanning.NewDatabaseTargetResolver(executionStore)
			}
			if planningSources == nil && service.executionV2Runtime != nil {
				planningSources = executionplanning.NewProductionSourceResolver(executionStore, service.executionV2Runtime.artifacts)
			}
			if planningBindings == nil {
				planningBindings = executionplanning.NewProductionBindingResolver(executionStore, time.Now)
			}
		}
		if executionV2Config.Analyze == nil || executionV2Config.PlanCompiler == nil {
			if recipes, recipeErr := agentrecipes.Builtin(); recipeErr == nil {
				var revisionWriter executionplanning.PlanRevisionWriter
				if rw, ok := executionV2Config.Store.(executionplanning.PlanRevisionWriter); ok {
					revisionWriter = rw
				}
				var executorSealer executionplanning.ExecutorSealer
				if service.executionV2Runtime != nil {
					executorSealer = executionplanning.NewArtifactExecutorSealer(service.executionV2Runtime.artifacts)
				}
				var credentials executionplanning.CredentialResolver
				if resolver, ok := executionV2Config.Secrets.(executionplanning.CredentialResolver); ok {
					credentials = resolver
				}
				planner := executionplanning.New(executionplanning.Config{AnalysisStore: executionV2Config.Store, PlanStore: executionV2Config.Store, RevisionWriter: revisionWriter, Sources: planningSources, Targets: planningTargets, ExecutionSecrets: credentials, Bindings: planningBindings, Executors: executorSealer, Recipes: recipes})
				if executionV2Config.Analyze == nil {
					executionV2Config.Analyze = planner
				}
				if executionV2Config.PlanCompiler == nil {
					executionV2Config.PlanCompiler = planner
				}
				if executionV2Config.PlanReady == nil {
					executionV2Config.PlanReady = planner.PlanReady
				}
				service.executionV2PlanReady = executionV2Config.PlanReady
			}
		}
	}
	service.executionV2Ready = executionV2Config.Ready
	service.executionV2ObserveReady = executionV2Config.ObserveReady
	service.executionV2RunReady = executionV2Config.RunReady
	service.executionV2BindingsReady = func() bool { return executionV2BindingReadsReady(executionV2Config) }
	service.executionV2InvokeReady = func() bool { return executionV2HTTPAPIInvokeReady(executionV2Config) }
	service.executionV2TransportReady = executionV2Config.TransportAWSReady
	service.executionV2ProvisionReady = executionV2Config.TargetReserveReady
	service.executionV2SecretsReady = executionV2Config.SecretsReady
	embeddedConfig.ExecutionV2 = agentembeddedmodule.NewExecutionV2ActionPort(executionV2Config)
	embeddedConfig.ExecutionV2PlanReady = executionV2Config.PlanReady != nil && executionV2Config.PlanReady()
	if service.executionV2PlanReady == nil {
		service.executionV2PlanReady = func() bool { return false }
	}
	if dbStore, ok := store.(*p2pstorage.DatabaseStore); ok {
		ownerID := strings.TrimSpace(service.OwnerMXID())
		taskStore := p2pstorage.NewDatabaseTaskStore(dbStore.DB())
		confirmationStore := p2pstorage.NewDatabaseConfirmationStore(dbStore.DB())
		service.agentConfirmationSweep = confirmationStore.ExpireOverdue
		controls := newEmbeddedControlRuntime(dbStore, taskStore, confirmationStore, ownerID, service.agentSecretEnveloper)
		service.agentTaskExecutor = &embeddedTaskExecutor{
			agent: service.agentModule,
			aws:   controls.aws,
			mcp:   controls.mcp,
		}
		service.agentTaskRuntime, service.agentRuntimeInitErr = agentruntime.New(agentruntime.Config{
			Store: taskStore, Executor: service.agentTaskExecutor, MaxConcurrent: 4,
			LeaseTTL: 30 * time.Second, Holder: "dirextalk-message-server",
		})
		if service.agentRuntimeInitErr == nil {
			service.agentScheduleLoop, service.agentRuntimeInitErr = agentruntime.NewScheduleLoop(dbStore, agentruntime.CronCalculator{}, time.Second)
		}
		embeddedConfig.Tasks = taskStore
		embeddedConfig.TaskRetry = embeddedTaskRetryAdapter{store: taskStore}
		embeddedConfig.Confirmations = confirmationStore
		embeddedConfig.MCP = controls.mcpPort
		embeddedConfig.AWS = controls.awsPort
		if trigger, ok := any(dbStore).(interface {
			TriggerSchedule(context.Context, string, string, string) (p2pstorage.Schedule, string, string, error)
		}); ok {
			embeddedConfig.ScheduleTrigger = trigger
		}
	}
	service.agentEmbedded = agentembeddedmodule.New(embeddedConfig)
	service.actions = service.actionHandlers()
	service.realtimeModule = realtimewsmodule.New(realtimewsmodule.Dependencies{
		Actions:      serviceRealtimeActionPort{service: service},
		Events:       service.eventsModule,
		Sessions:     realtimeSessions,
		Plugins:      service.pluginsModule,
		Agent:        service.agentModule,
		TicketActive: service.realtimeWSTicketActive,
	}, realtimewsmodule.Config{
		Now:               time.Now,
		NewToken:          randomToken,
		HeartbeatInterval: realtimewsmodule.DefaultHeartbeatInterval,
	})
	if memoryStore, ok := store.(*p2pstorage.MemoryStore); ok {
		service.mu.Lock()
		state := service.portalStateLocked()
		service.mu.Unlock()
		if err := memoryStore.SavePortal(context.Background(), state); err != nil {
			panic("seed P2P memory store portal: " + err.Error())
		}
	}
	return service
}

func (s *Service) resolveRoomOwner(ctx context.Context, roomID string) (string, error) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" || s.conversationModule == nil {
		return "", nil
	}
	record, ok, err := s.conversationModule.GetRecord(ctx, "", roomID)
	if err != nil || !ok {
		return "", err
	}
	if record.Kind != dirextalkdomain.ConversationKindGroup && record.Kind != dirextalkdomain.ConversationKindChannel {
		return "", nil
	}
	if s.transport == nil {
		return strings.TrimSpace(record.CreatedByMXID), nil
	}
	reader, ok := s.transport.(RoomCreatorReader)
	if !ok {
		if strings.TrimSpace(record.CreatedByMXID) != "" {
			if err := s.conversationModule.SetCreator(ctx, roomID, ""); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	creatorMXID, err := reader.ReadRoomCreator(ctx, roomID)
	if err != nil {
		return "", err
	}
	creatorMXID = strings.TrimSpace(creatorMXID)
	if strings.TrimSpace(record.CreatedByMXID) != creatorMXID {
		if err := s.conversationModule.SetCreator(ctx, roomID, creatorMXID); err != nil {
			return "", err
		}
	}
	return creatorMXID, nil
}

func normalizeAgentConfig(cfg agentConfig) agentConfig {
	return agentmodule.NormalizeConfig(cfg)
}

func (s *Service) AccessToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accessToken
}

func (s *Service) AgentToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentToken
}

func (s *Service) OwnerMXID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownerMXID
}

func (s *Service) Handle(ctx context.Context, action string, params map[string]any) (any, *apiError) {
	action = strings.TrimSpace(action)
	handler, ok := s.actions[action]
	if !ok {
		return nil, badRequest("unknown action")
	}
	if action == "portal.account.delete" {
		return handler(ctx, params)
	}
	ctx, finishOperation := s.beginAccountOperation(ctx)
	defer finishOperation()
	if s.accountIsDeprovisioned() {
		return nil, statusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	}
	canonicalParams, canonicalErr := s.canonicalRecoverableParams(ctx, action, params)
	if canonicalErr != nil {
		return nil, canonicalErr
	}
	params = canonicalParams
	ctx, canonicalErr = s.preflightRecoverablePublicAction(ctx, action, params)
	if canonicalErr != nil {
		return nil, canonicalErr
	}
	if rebuildErr := validateExplicitRetainedRoomRebuild(action, params); rebuildErr != nil {
		return nil, rebuildErr
	}
	releaseMemberWorkflow, workflowErr := s.lockMemberWorkflowForAction(ctx, action, params)
	if workflowErr != nil {
		return nil, workflowErr
	}
	defer releaseMemberWorkflow()
	if s.store != nil && (recoverableProductAction(action) || explicitRetainedRoomRebuildAction(action, params)) {
		return s.handleRecoverableOperation(ctx, action, params, handler)
	}
	return handler(ctx, params)
}

func (s *Service) Authenticate(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return token != "" && token == s.accessToken
}

func (s *Service) Authorize(token, action string) bool {
	_, authorized := s.authorizeProductAction(token, action)
	return authorized
}

func (s *Service) authorizeProductAction(token, action string) (portalActionSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "" {
		return portalActionSession{}, false
	}
	if _, ok := serviceapi.ActionSpecFor(action); !ok {
		return portalActionSession{}, false
	}
	if token == s.accessToken {
		return portalActionSession{DeviceID: cleanMatrixDeviceID(s.matrixDeviceID), Generation: s.portalSessionGeneration}, true
	}
	return portalActionSession{}, token == s.agentToken && serviceapi.AgentAction(action)
}

func (s *Service) publishCurrentAgentStatusState(ctx context.Context) error {
	s.mu.Lock()
	roomID := s.agentRoomID
	agentMXID := s.agentMXIDLocked()
	s.mu.Unlock()
	return s.publishAgentStatusState(ctx, roomID, agentMXID, agentMXID, false)
}

func (s *Service) publishAgentStatusState(ctx context.Context, roomID, senderMXID, agentMXID string, online bool) error {
	if s.transport == nil || strings.TrimSpace(roomID) == "" || strings.TrimSpace(senderMXID) == "" {
		return nil
	}
	return s.transport.SendStateEvent(ctx, SendStateEventRequest{
		RoomID:     strings.TrimSpace(roomID),
		SenderMXID: strings.TrimSpace(senderMXID),
		Event:      agentStatusStateEvent(agentMXID, online),
	})
}
