// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package setup

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	appserviceAPI "github.com/YingSuiAI/dirextalk-message-server/appservice/api"
	"github.com/YingSuiAI/dirextalk-message-server/clientapi"
	"github.com/YingSuiAI/dirextalk-message-server/clientapi/api"
	"github.com/YingSuiAI/dirextalk-message-server/federationapi"
	federationAPI "github.com/YingSuiAI/dirextalk-message-server/federationapi/api"
	"github.com/YingSuiAI/dirextalk-message-server/internal/caching"
	"github.com/YingSuiAI/dirextalk-message-server/internal/httputil"
	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
	"github.com/YingSuiAI/dirextalk-message-server/internal/releasecontrol"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/internal/transactions"
	"github.com/YingSuiAI/dirextalk-message-server/mediaapi"
	"github.com/YingSuiAI/dirextalk-message-server/p2p"
	"github.com/YingSuiAI/dirextalk-message-server/relayapi"
	relayAPI "github.com/YingSuiAI/dirextalk-message-server/relayapi/api"
	roomserverAPI "github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/setup/jetstream"
	"github.com/YingSuiAI/dirextalk-message-server/setup/process"
	"github.com/YingSuiAI/dirextalk-message-server/syncapi"
	"github.com/YingSuiAI/dirextalk-message-server/syncapi/agenthistory"
	syncstorage "github.com/YingSuiAI/dirextalk-message-server/syncapi/storage"
	userapi "github.com/YingSuiAI/dirextalk-message-server/userapi/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
)

// Monolith represents an instantiation of all dependencies required to build
// all components of Dendrite, for use in monolith mode.
type Monolith struct {
	Config    *config.Dendrite
	KeyRing   *gomatrixserverlib.KeyRing
	Client    *fclient.Client
	FedClient fclient.FederationClient

	AppserviceAPI appserviceAPI.AppServiceInternalAPI
	FederationAPI federationAPI.FederationInternalAPI
	RoomserverAPI roomserverAPI.RoomserverInternalAPI
	UserAPI       userapi.UserInternalAPI
	RelayAPI      relayAPI.RelayInternalAPI

	// Optional
	ExtPublicRoomsProvider   api.ExtraPublicRoomsProvider
	ExtUserDirectoryProvider userapi.QuerySearchProfilesAPI
}

// AddAllPublicRoutes attaches all public paths to the given router
func (m *Monolith) AddAllPublicRoutes(
	processCtx *process.ProcessContext,
	cfg *config.Dendrite,
	routers httputil.Routers,
	cm *sqlutil.Connections,
	natsInstance *jetstream.NATSInstance,
	caches *caching.Caches,
	enableMetrics bool,
) {
	userDirectoryProvider := m.ExtUserDirectoryProvider
	if userDirectoryProvider == nil {
		userDirectoryProvider = m.UserAPI
	}
	clientapi.AddPublicRoutes(
		processCtx, routers, cfg, natsInstance, m.FedClient, m.RoomserverAPI, m.AppserviceAPI, transactions.New(),
		m.FederationAPI, m.UserAPI, userDirectoryProvider,
		m.ExtPublicRoomsProvider, enableMetrics,
	)
	federationapi.AddPublicRoutes(
		processCtx, routers, cfg, natsInstance, m.UserAPI, m.FedClient, m.KeyRing, m.RoomserverAPI, m.FederationAPI, enableMetrics,
	)
	mediaapi.AddPublicRoutes(routers, cm, cfg, m.UserAPI, m.Client, m.FedClient, m.KeyRing)
	syncapi.AddPublicRoutes(processCtx, routers, cfg, cm, natsInstance, m.UserAPI, m.RoomserverAPI, caches, enableMetrics)
	remoteNodeInsecureSkipTLSVerify := p2pRemoteNodeInsecureSkipTLSVerifyFromEnv()
	p2pConfig := p2p.Config{
		ServerName:                      string(cfg.Global.ServerName),
		Homeserver:                      cfg.Global.WellKnownClientName,
		RemoteNodeInsecureSkipTLSVerify: remoteNodeInsecureSkipTLSVerify,
		RemoteNodeAllowPrivateBaseURLs:  remoteNodeInsecureSkipTLSVerify,
		P2PEventRetentionMaxRows:        p2pEventRetentionMaxRowsFromEnv(),
		P2PEventRetentionPruneOnWrite:   p2pEventRetentionPruneOnWriteFromEnv(),
		PushRules:                       m.UserAPI,
		ReleaseController:               releasecontrol.NewUnixController(releasecontrol.UnixControllerConfig{}),
	}
	if agentCoreConfig, err := p2p.AgentCoreConfigFromEnv(); err != nil {
		logrus.WithError(err).Fatal("invalid P2P Agent Core configuration")
	} else {
		p2pConfig.AgentCore = agentCoreConfig
	}
	matrixHistoryBaseURL := matrixHistoryReaderBaseURL(p2pConfig.Homeserver)
	matrixProfileResolver := p2p.NewHTTPMatrixProfileResolver(matrixHistoryBaseURL, nil)
	p2pTransport := p2p.NewDendriteTransport(cfg.Global.ServerName, cfg.Global.KeyID, cfg.Global.PrivateKey, m.RoomserverAPI)
	accountDeprovisioner := newAccountDeprovisioner(processCtx, cfg, cm)
	p2pService, err := newPersistentP2PService(processCtx.Context(), p2pConfig, cm, p2pDatabaseOptions(cfg), p2pTransport)
	if err != nil {
		logrus.WithError(err).Fatal("P2P integrated AS persistent state is required")
	}
	p2pTransport.SetBlockedDirectMessageChecker(p2pService.BlockedDirectMessage)
	cfg.ClientAPI.DirextalkBlockChecker = p2pService.BlockedDirectMessage
	cfg.FederationAPI.DirextalkBlockChecker = func(ctx context.Context, roomID string, senderID spec.SenderID) (bool, error) {
		isDirect, err := productpolicy.IsDirextalkDirectRoom(ctx, m.RoomserverAPI, roomID)
		if err != nil || !isDirect {
			return false, err
		}
		validRoomID, err := spec.NewRoomID(roomID)
		if err != nil {
			return false, err
		}
		senderMXID, err := m.RoomserverAPI.QueryUserIDForSender(ctx, *validRoomID, senderID)
		if err != nil || senderMXID == nil {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("sender identity unavailable")
		}
		return p2pService.BlockedDirectMessage(ctx, roomID, senderMXID.String())
	}
	p2pService.SetMatrixSessionIssuer(p2p.NewDendriteMatrixSessionIssuer(m.UserAPI, cfg.Global.ServerName))
	p2pService.SetAccountDeactivator(p2p.NewDendriteAccountDeactivator(m.UserAPI, cfg.Global.ServerName))
	p2pService.SetAccountDeprovisioner(accountDeprovisioner)
	matrixHistoryReader := p2p.NewHTTPMatrixHistoryReader(matrixHistoryBaseURL, p2pService.MatrixHistoryAccessToken, nil)
	p2pService.SetMatrixMessageReader(matrixHistoryReader)
	p2pService.SetMatrixProfileResolver(matrixProfileResolver)
	if syncDB, err := syncstorage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database); err != nil {
		logrus.WithError(err).Warn("P2P native Agent sync DB reader unavailable; using Matrix HTTP history reader")
	} else {
		p2pService.SetReadMarkerPositionResolver(agenthistory.NewReadMarkerPositionResolver(
			syncDB, m.RoomserverAPI, p2pService.OwnerMXID(),
		))
		p2pService.SetMatrixMessageReader(p2p.NewCompositeMatrixHistoryReader(
			agenthistory.NewReader(syncDB, m.RoomserverAPI, p2pService.OwnerMXID()),
			matrixHistoryReader,
		))
	}
	if natsInstance != nil {
		js, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		if err := p2p.NewOutputRoomEventConsumer(processCtx, &cfg.Global.JetStream, js, p2pService).Start(); err != nil {
			logrus.WithError(err).Warn("P2P integrated AS projector unavailable")
		} else {
			p2pService.SetProjectorStarted(true)
		}
	}
	p2p.Register(routers.P2P, p2pService)
	p2p.RegisterMCP(routers.MCP, p2pService)
	p2p.RegisterWellKnown(routers.PortalWellKnown, p2pService)

	if m.RelayAPI != nil {
		relayapi.AddPublicRoutes(routers, cfg, m.KeyRing, m.RelayAPI)
	}
}

func p2pDatabaseOptions(cfg *config.Dendrite) *config.DatabaseOptions {
	if cfg.Global.DatabaseOptions.ConnectionString != "" {
		return &cfg.Global.DatabaseOptions
	}
	return &cfg.RoomServer.Database
}

func newPersistentP2PService(ctx context.Context, p2pConfig p2p.Config, cm *sqlutil.Connections, dbOptions *config.DatabaseOptions, transport p2p.Transport) (*p2p.Service, error) {
	store, err := p2p.NewDatabaseStore(ctx, cm, dbOptions)
	if err != nil {
		return nil, fmt.Errorf("P2P integrated AS store unavailable: %w", err)
	}
	service, err := p2p.NewServiceWithStoreAndTransport(ctx, p2pConfig, store, transport)
	if err != nil {
		return nil, fmt.Errorf("P2P integrated AS state load failed: %w", err)
	}
	return service, nil
}

func matrixHistoryReaderBaseURL(configured string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" ||
		strings.EqualFold(configured, "auto") ||
		strings.EqualFold(configured, "http://auto") ||
		strings.EqualFold(configured, "https://auto") {
		return "http://127.0.0.1:8008"
	}
	return configured
}

func p2pRemoteNodeInsecureSkipTLSVerifyFromEnv() bool {
	value := strings.TrimSpace(os.Getenv("P2P_REMOTE_NODE_INSECURE_SKIP_TLS_VERIFY"))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		logrus.WithField("value", value).Warn("Ignoring invalid P2P_REMOTE_NODE_INSECURE_SKIP_TLS_VERIFY value")
		return false
	}
	return parsed
}

func p2pEventRetentionMaxRowsFromEnv() int64 {
	value := strings.TrimSpace(os.Getenv("P2P_EVENT_RETENTION_MAX_ROWS"))
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		logrus.WithField("value", value).Warn("Ignoring invalid P2P_EVENT_RETENTION_MAX_ROWS value")
		return 0
	}
	return parsed
}

func p2pEventRetentionPruneOnWriteFromEnv() bool {
	value := strings.TrimSpace(os.Getenv("P2P_EVENT_RETENTION_PRUNE_ON_WRITE"))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		logrus.WithField("value", value).Warn("Ignoring invalid P2P_EVENT_RETENTION_PRUNE_ON_WRITE value")
		return false
	}
	return parsed
}
