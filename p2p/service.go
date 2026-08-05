package p2p

import (
	"context"
	"errors"
	"net/http"
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
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcompletion"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	blocksmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/blocks"
	callsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/calls"
	channelsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/channels"
	contactsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/contacts"
	conversationmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/conversation"
	eventsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/events"
	groupsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/groups"
	legacygatewaymodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/legacygateway"
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
	// NativeAgentChatRunner delegates only Chat/StreamChat to the independent
	// Agent service. All other Agent actions continue to use NativeAgentRunner
	// or the existing local runtime.
	NativeAgentChatRunner NativeAgentRunner
	// AgentRuntimeProfileClient delegates only the owner-selected immutable
	// model profile to the independent Agent. It never exposes credential
	// references or bytes to ProductCore.
	AgentRuntimeProfileClient AgentRuntimeProfileClient
	// AgentSearchProfileClient delegates only a catalog-bound, de-secreted
	// search profile and bounded query limits to the independent Agent.
	AgentSearchProfileClient AgentSearchProfileClient
	// AgentCompletionSource streams durable, report-bound Team completion
	// events from the independent Agent. ProductCore stores its own cursor and
	// publishes the result through the existing realtime event stream.
	AgentCompletionSource agentcompletion.Source
	NativeAgentDataDir    string
	ReleaseController     releasecontrol.Controller
	CentralVersionSource  releasecontrol.CentralVersionSource
}

const (
	ownerLocalpart = "owner"
	agentLocalpart = "agent"
	agentRoomName  = "Agents"
)

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
	mcpModule                 *mcpmodule.Module
	mcpCapabilities           *dirextalkmcp.Service
	releaseController         releasecontrol.Controller
	legacyAgentGatewayModule  *legacygatewaymodule.Module

	servicePortalState
	actions              map[string]actionHandler
	blocksModule         *blocksmodule.Module
	callsModule          *callsmodule.Module
	channelsModule       *channelsmodule.Module
	channelContentModule *channelsmodule.ContentModule
	contactsModule       *contactsmodule.Module
	conversationModule   *conversationmodule.Module
	eventsModule         *eventsmodule.Module
	agentCompletionRelay *agentcompletion.Relay
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
	agentcompletion.CursorStore
	operationsmodule.Store
	legacygatewaymodule.Store
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
	service.eventsModule = eventsmodule.New(service.store, eventsmodule.Config{
		RetentionMaxRows:      cfg.P2PEventRetentionMaxRows,
		RetentionPruneOnWrite: cfg.P2PEventRetentionPruneOnWrite,
		Now:                   time.Now,
	})
	if cfg.AgentCompletionSource != nil {
		service.agentCompletionRelay = agentcompletion.New(
			cfg.AgentCompletionSource,
			service.store,
			service.eventsModule,
			agentcompletion.Config{Now: time.Now},
		)
	}
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
	service.agentModule = agentmodule.New(agentmodule.Config{
		Runner:          cfg.NativeAgentRunner,
		ChatRunner:      cfg.NativeAgentChatRunner,
		RuntimeProfiles: cfg.AgentRuntimeProfileClient,
		SearchProfiles:  cfg.AgentSearchProfileClient,
		DataDir:         cfg.NativeAgentDataDir,
		Store:           nativeAgentConfigStore{service: service},
		MCP:             service.mcpCapabilities,
		Account:         serviceAgentAccountPort{service: service},
		Turns:           service.store,
		OwnerID:         service.OwnerMXID,
	})
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
