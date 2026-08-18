// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package setup

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	appserviceAPI "github.com/YingSuiAI/dirextalk-message-server/appservice/api"
	"github.com/YingSuiAI/dirextalk-message-server/clientapi"
	"github.com/YingSuiAI/dirextalk-message-server/clientapi/api"
	"github.com/YingSuiAI/dirextalk-message-server/federationapi"
	federationAPI "github.com/YingSuiAI/dirextalk-message-server/federationapi/api"
	"github.com/YingSuiAI/dirextalk-message-server/internal/caching"
	"github.com/YingSuiAI/dirextalk-message-server/internal/httputil"
	productcapability "github.com/YingSuiAI/dirextalk-message-server/internal/productcapability"
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
	accountGeneration, generationErr := accountGenerationFromEnv()
	if generationErr != nil {
		logrus.WithError(generationErr).Fatal("invalid account generation")
	}
	centralVersionSource, centralAgentVersionSource, releaseCatalogErr := releaseCatalogSourcesFromEnv()
	if releaseCatalogErr != nil {
		logrus.WithError(releaseCatalogErr).Fatal("invalid release catalog configuration")
	}
	p2pConfig := p2p.Config{
		ServerName:                             string(cfg.Global.ServerName),
		Homeserver:                             cfg.Global.WellKnownClientName,
		AccountGeneration:                      accountGeneration,
		RemoteNodeInsecureSkipTLSVerify:        remoteNodeInsecureSkipTLSVerify,
		RemoteNodeAllowPrivateBaseURLs:         remoteNodeInsecureSkipTLSVerify,
		P2PEventRetentionMaxRows:               p2pEventRetentionMaxRowsFromEnv(),
		P2PEventRetentionPruneOnWrite:          p2pEventRetentionPruneOnWriteFromEnv(),
		PushRules:                              m.UserAPI,
		ReleaseController:                      releasecontrol.NewUnixController(releasecontrol.UnixControllerConfig{}),
		CentralVersionSource:                   centralVersionSource,
		CentralAgentVersionSource:              centralAgentVersionSource,
		AllowAccountDeleteWithoutUpdater:       boolEnv("P2P_ALLOW_ACCOUNT_DELETE_WITHOUT_UPDATER"),
		NativeAgentVoiceCallbackURL:            firstNonEmptyEnv("P2P_AGENT_VOICE_CALLBACK_URL", "DIREXTALK_AGENT_VOICE_CALLBACK_URL"),
		NativeAgentVoiceCallbackAuthToken:      readOptionalSecretEnv("P2P_AGENT_VOICE_CALLBACK_AUTH_TOKEN_FILE", "DIREXTALK_AGENT_VOICE_CALLBACK_AUTH_TOKEN_FILE"),
		NativeAgentVoiceCallbackCAFile:         firstNonEmptyEnv("P2P_AGENT_VOICE_CALLBACK_CA_FILE", "DIREXTALK_CAPABILITY_CA_FILE"),
		NativeAgentVoiceCallbackClientCertFile: firstNonEmptyEnv("P2P_AGENT_VOICE_CALLBACK_CLIENT_CERT_FILE", "DIREXTALK_MS_CLIENT_CERT_FILE"),
		NativeAgentVoiceCallbackClientKeyFile:  firstNonEmptyEnv("P2P_AGENT_VOICE_CALLBACK_CLIENT_KEY_FILE", "DIREXTALK_MS_CLIENT_KEY_FILE"),
		NativeAgentVoiceCallbackServerName:     firstNonEmptyEnv("P2P_AGENT_VOICE_CALLBACK_SERVER_NAME", "DIREXTALK_AGENT_TLS_SERVER_NAME"),
	}
	grantPrivateKey, grantPrivateKeyErr := capabilityGrantPrivateKeyFromEnv()
	if grantPrivateKeyErr != nil {
		logrus.WithError(grantPrivateKeyErr).Fatal("invalid capability grant signing key")
	}
	p2pConfig.NativeAgentGrantPrivateKey = grantPrivateKey
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
	logrus.Debug("external Native Agent service owns scheduler and execution workers")
	p2p.Register(routers.P2P, p2pService)
	p2p.RegisterMCP(routers.MCP, p2pService)
	p2p.RegisterWellKnown(routers.PortalWellKnown, p2pService)
	if err := startProductCapabilityServer(processCtx, p2pService, grantPrivateKey); err != nil {
		logrus.WithError(err).Fatal("failed to start Product Capability service")
	}

	if m.RelayAPI != nil {
		relayapi.AddPublicRoutes(routers, cfg, m.KeyRing, m.RelayAPI)
	}
}

func releaseCatalogSourcesFromEnv() (releasecontrol.CentralVersionSource, releasecontrol.CentralAgentVersionSource, error) {
	origin, ok := os.LookupEnv("DIREXTALK_RELEASE_CATALOG_ORIGIN")
	if !ok || origin == "" {
		return nil, nil, errors.New("DIREXTALK_RELEASE_CATALOG_ORIGIN is required")
	}
	normalized, err := releasecontrol.NormalizeReleaseCatalogOrigin(origin)
	if err != nil {
		return nil, nil, fmt.Errorf("DIREXTALK_RELEASE_CATALOG_ORIGIN: %w", err)
	}
	config := releasecontrol.CentralVersionSourceConfig{Origin: normalized}
	return releasecontrol.NewCentralVersionSource(config), releasecontrol.NewCentralAgentVersionSource(config), nil
}

func startProductCapabilityServer(processCtx *process.ProcessContext, service *p2p.Service, grantPrivateKey []byte) error {
	listenAddr := strings.TrimSpace(firstNonEmptyEnv("P2P_PRODUCT_CAPABILITY_LISTEN_ADDR", "DIREXTALK_PRODUCT_CAPABILITY_LISTEN_ADDR"))
	if listenAddr == "" {
		if boolEnv("P2P_REQUIRE_PRODUCT_CAPABILITY") {
			return errors.New("product capability listen address is required")
		}
		return nil
	}
	grantPublicKey, keyErr := capabilityGrantPublicKeyFromEnv()
	if keyErr != nil {
		return keyErr
	}
	config, err := productCapabilityConfigFromEnv(listenAddr, grantPublicKey, grantPrivateKey, service.ProductCapabilityDatabase())
	if err != nil {
		return err
	}
	config.PreparedMatrixStore = service.PreparedMatrixMutationStore()
	config.ServiceOwnerID = service.OwnerMXID()
	config.RecordAgentExecutionCompletion = service.RecordAgentExecutionCompletion
	registry, registryErr := productcapability.NewRegistryWithInvokerAndOptionsChecked(service.InvokeProductCapability, productcapability.RegistryOptions{
		MatrixMutationReady: service.DurableMatrixMutationReady(),
	})
	if registryErr != nil {
		return fmt.Errorf("build Product Capability catalog: %w", registryErr)
	}
	server, err := productcapability.New(config, registry)
	if err != nil {
		return err
	}
	if err := server.Start(); err != nil {
		return err
	}
	processCtx.RegisterShutdownCallback(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Stop(ctx); err != nil {
			logrus.WithError(err).Warn("Product Capability service shutdown failed")
		}
	})
	logrus.WithField("listen_addr", listenAddr).Info("Product Capability service started")
	return nil
}

func productCapabilityConfigFromEnv(listenAddr string, grantPublicKey, grantPrivateKey []byte, db *sql.DB) (*productcapability.Config, error) {
	accountGeneration, generationErr := accountGenerationFromEnv()
	if generationErr != nil {
		return nil, generationErr
	}
	config := &productcapability.Config{
		ListenAddr:                listenAddr,
		CACertFile:                firstNonEmptyEnv("P2P_PRODUCT_CAPABILITY_CA_FILE", "DIREXTALK_CAPABILITY_CA_FILE"),
		ServerCertFile:            firstNonEmptyEnv("P2P_PRODUCT_CAPABILITY_SERVER_CERT_FILE", "DIREXTALK_MS_SERVER_CERT_FILE"),
		ServerKeyFile:             firstNonEmptyEnv("P2P_PRODUCT_CAPABILITY_SERVER_KEY_FILE", "DIREXTALK_MS_SERVER_KEY_FILE"),
		TokenFile:                 firstNonEmptyEnv("P2P_PRODUCT_CAPABILITY_TOKEN_FILE", "DIREXTALK_AGENT_TO_MS_TOKEN_FILE"),
		InstanceID:                firstNonEmptyEnv("P2P_MESSAGE_SERVER_INSTANCE_ID", "DIREXTALK_MESSAGE_SERVER_INSTANCE_ID"),
		PeerInstanceID:            firstNonEmptyEnv("P2P_AGENT_INSTANCE_ID", "DIREXTALK_AGENT_INSTANCE_ID"),
		PeerCommonName:            firstNonEmptyEnv("P2P_PRODUCT_CAPABILITY_PEER_COMMON_NAME", "DIREXTALK_AGENT_CAPABILITY_PEER_COMMON_NAME"),
		ExpectedAccountGeneration: accountGeneration,
		GrantPublicKey:            grantPublicKey,
		GrantPrivateKey:           grantPrivateKey,
		DB:                        db,
	}
	for name, value := range map[string]string{"CA": config.CACertFile, "server cert": config.ServerCertFile, "server key": config.ServerKeyFile, "token": config.TokenFile, "instance id": config.InstanceID, "peer instance id": config.PeerInstanceID} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("product capability %s is required", name)
		}
	}
	if len(config.GrantPrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("product capability grant private key is required")
	}
	return config, nil
}

// accountGenerationFromEnv is deliberately process-immutable: the same
// deployment value is used for the P2P metadata and Product peer fence. A
// fresh account stack may set a new positive generation; absent configuration
// keeps the clean-project default at one.
func accountGenerationFromEnv() (int64, error) {
	raw := strings.TrimSpace(firstNonEmptyEnv("P2P_ACCOUNT_GENERATION", "DIREXTALK_ACCOUNT_GENERATION"))
	if raw == "" {
		return 1, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("account generation must be a positive integer")
	}
	return value, nil
}

func capabilityGrantPrivateKeyFromEnv() ([]byte, error) {
	path := strings.TrimSpace(firstNonEmptyEnv("P2P_CAPABILITY_GRANT_PRIVATE_KEY_FILE", "DIREXTALK_CAPABILITY_GRANT_PRIVATE_KEY_FILE"))
	if path == "" {
		return nil, nil
	}
	secret, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read capability grant private key: %w", err)
	}
	if len(secret) != ed25519.PrivateKeySize {
		return nil, errors.New("capability grant private key must be exactly 64 bytes")
	}
	return secret, nil
}

func capabilityGrantPublicKeyFromEnv() ([]byte, error) {
	path := strings.TrimSpace(firstNonEmptyEnv("P2P_CAPABILITY_GRANT_PUBLIC_KEY_FILE", "DIREXTALK_CAPABILITY_GRANT_PUBLIC_KEY_FILE"))
	if path == "" {
		return nil, errors.New("capability grant public key file is required")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read capability grant public key: %w", err)
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, errors.New("capability grant public key must be exactly 32 bytes")
	}
	return key, nil
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func readOptionalSecretEnv(names ...string) string {
	path := firstNonEmptyEnv(names...)
	if path == "" {
		return ""
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(value))
}

func boolEnv(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
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
