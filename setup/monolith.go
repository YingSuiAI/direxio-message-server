// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
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

	// AgentGRPCRunnerFactory is an optional construction override. Production
	// uses p2p.NewAgentGRPCChatRunner when it is nil.
	AgentGRPCRunnerFactory AgentGRPCRunnerFactory

	// Optional
	ExtPublicRoomsProvider   api.ExtraPublicRoomsProvider
	ExtUserDirectoryProvider userapi.QuerySearchProfilesAPI
}

// AgentGRPCDialConfig contains no secret value. ServiceKeyFile is a protected
// mounted-file reference consumed by the Agent gRPC client constructor.
type AgentGRPCDialConfig = p2p.AgentGRPCConfig

type AgentGRPCRunner = p2p.ClosableNativeAgentRunner

type AgentGRPCRunnerFactory func(context.Context, AgentGRPCDialConfig) (AgentGRPCRunner, error)

type p2pAgentGRPCBackendConfig struct {
	Enabled        bool
	Target         string
	CAFile         string
	ServerName     string
	ServiceKeyFile string
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
	agentBackend, err := p2pAgentGRPCBackendConfigFromEnv()
	if err != nil {
		logrus.Fatal("P2P Agent gRPC configuration is invalid")
	}
	agentChatRunner, err := newP2PAgentChatRunner(
		processCtx.Context(), string(cfg.Global.ServerName), agentBackend, m.AgentGRPCRunnerFactory,
	)
	if err != nil {
		logrus.Fatal("P2P Agent gRPC backend is unavailable")
	}
	agentRuntimeProfileClient, err := p2pAgentRuntimeProfileClient(agentBackend, agentChatRunner)
	if err != nil {
		if agentChatRunner != nil {
			_ = agentChatRunner.Close()
		}
		logrus.Fatal("P2P Agent gRPC runtime profile backend is unavailable")
	}
	agentSearchProfileClient, err := p2pAgentSearchProfileClient(agentBackend, agentChatRunner)
	if err != nil {
		if agentChatRunner != nil {
			_ = agentChatRunner.Close()
		}
		logrus.Fatal("P2P Agent gRPC search profile backend is unavailable")
	}
	agentCompletionSource, err := p2pAgentCompletionSource(
		agentBackend,
		agentChatRunner,
	)
	if err != nil {
		if agentChatRunner != nil {
			_ = agentChatRunner.Close()
		}
		logrus.Fatal("P2P Agent gRPC completion backend is unavailable")
	}
	agentCompletionSynthesizer, err := p2pAgentCompletionSynthesizer(
		agentBackend,
		agentChatRunner,
	)
	if err != nil {
		if agentChatRunner != nil {
			_ = agentChatRunner.Close()
		}
		logrus.Fatal("P2P Agent gRPC completion synthesis backend is unavailable")
	}
	agentArtifactSource, err := p2pAgentArtifactSource(
		agentBackend,
		agentChatRunner,
	)
	if err != nil {
		if agentChatRunner != nil {
			_ = agentChatRunner.Close()
		}
		logrus.Fatal("P2P Agent gRPC artifact backend is unavailable")
	}
	agentConversationStateReader, err := p2pAgentConversationStateReader(
		agentBackend,
		agentChatRunner,
	)
	if err != nil {
		if agentChatRunner != nil {
			_ = agentChatRunner.Close()
		}
		logrus.Fatal("P2P Agent gRPC conversation state backend is unavailable")
	}
	if agentChatRunner != nil {
		startAgentGRPCRunnerLifecycle(processCtx, agentChatRunner)
	}
	p2pConfig := p2p.Config{
		ServerName:                      string(cfg.Global.ServerName),
		Homeserver:                      cfg.Global.WellKnownClientName,
		RemoteNodeInsecureSkipTLSVerify: remoteNodeInsecureSkipTLSVerify,
		RemoteNodeAllowPrivateBaseURLs:  remoteNodeInsecureSkipTLSVerify,
		P2PEventRetentionMaxRows:        p2pEventRetentionMaxRowsFromEnv(),
		P2PEventRetentionPruneOnWrite:   p2pEventRetentionPruneOnWriteFromEnv(),
		NativeAgentChatRunner:           agentChatRunner,
		AgentRuntimeProfileClient:       agentRuntimeProfileClient,
		AgentSearchProfileClient:        agentSearchProfileClient,
		AgentCompletionSource:           agentCompletionSource,
		AgentCompletionSynthesizer:      agentCompletionSynthesizer,
		AgentArtifactSource:             agentArtifactSource,
		AgentConversationStateReader:    agentConversationStateReader,
		PushRules:                       m.UserAPI,
		ReleaseController:               releasecontrol.NewUnixController(releasecontrol.UnixControllerConfig{}),
	}
	matrixHistoryBaseURL := matrixHistoryReaderBaseURL(p2pConfig.Homeserver)
	matrixProfileResolver := p2p.NewHTTPMatrixProfileResolver(matrixHistoryBaseURL, nil)
	p2pTransport := p2p.NewDendriteTransport(cfg.Global.ServerName, cfg.Global.KeyID, cfg.Global.PrivateKey, m.RoomserverAPI)
	accountDeprovisioner := newAccountDeprovisioner(processCtx, cfg, cm)
	p2pService, err := newPersistentP2PService(processCtx.Context(), p2pConfig, cm, p2pDatabaseOptions(cfg), p2pTransport)
	if err != nil {
		logrus.WithError(err).Fatal("P2P integrated AS persistent state is required")
	}
	if agentCompletionSource != nil {
		startAgentCompletionRelayLifecycle(processCtx, p2pService)
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

func p2pAgentGRPCBackendConfigFromEnv() (p2pAgentGRPCBackendConfig, error) {
	if _, present := os.LookupEnv("P2P_AGENT_GRPC_SERVICE_KEY"); present {
		return p2pAgentGRPCBackendConfig{}, errors.New("inline Agent service key configuration is forbidden")
	}
	enabled, err := strictOptionalBool("P2P_AGENT_GRPC_ENABLED")
	if err != nil {
		return p2pAgentGRPCBackendConfig{}, err
	}
	config := p2pAgentGRPCBackendConfig{
		Enabled:        enabled,
		Target:         strings.TrimSpace(os.Getenv("P2P_AGENT_GRPC_TARGET")),
		CAFile:         strings.TrimSpace(os.Getenv("P2P_AGENT_GRPC_CA_FILE")),
		ServerName:     strings.TrimSpace(os.Getenv("P2P_AGENT_GRPC_SERVER_NAME")),
		ServiceKeyFile: strings.TrimSpace(os.Getenv("P2P_AGENT_GRPC_SERVICE_KEY_FILE")),
	}
	if !config.Enabled {
		return config, nil
	}
	if validateAgentGRPCTarget(config.Target) != nil || validateAgentGRPCServerName(config.ServerName) != nil ||
		validateAgentMountedFile(config.CAFile, false) != nil || validateAgentMountedFile(config.ServiceKeyFile, true) != nil {
		return p2pAgentGRPCBackendConfig{}, errors.New("enabled Agent gRPC backend is incomplete or invalid")
	}
	return config, nil
}

func strictOptionalBool(name string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("invalid boolean Agent gRPC configuration")
	}
	return value, nil
}

func validateAgentGRPCTarget(value string) error {
	target := strings.TrimSpace(value)
	if strings.HasPrefix(target, "dns:///") {
		target = strings.TrimPrefix(target, "dns:///")
	}
	if target == "" || strings.ContainsAny(target, "/?#@") {
		return errors.New("invalid Agent gRPC target")
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || strings.TrimSpace(host) == "" {
		return errors.New("invalid Agent gRPC target")
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return errors.New("invalid Agent gRPC target")
	}
	return nil
}

func validateAgentGRPCServerName(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " /\\@?#:\x00\r\n\t") {
		return errors.New("invalid Agent TLS server name")
	}
	return nil
}

func validateAgentMountedFile(path string, secret bool) error {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return errors.New("Agent mounted-file path is invalid")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return errors.New("Agent mounted file is unavailable")
	}
	if secret && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return errors.New("Agent service-key file permissions are too broad")
	}
	return nil
}

func newP2PAgentChatRunner(ctx context.Context, serverName string, config p2pAgentGRPCBackendConfig, factory AgentGRPCRunnerFactory) (AgentGRPCRunner, error) {
	if !config.Enabled {
		return nil, nil
	}
	serverName = strings.TrimSpace(serverName)
	if validateAgentGRPCServerName(serverName) != nil {
		return nil, errors.New("Message Server owner identity is invalid")
	}
	// OwnerID is a stable project-scoped protocol identifier, not a Matrix
	// user ID. This keeps Agent resource ownership independent of Matrix.
	ownerID := "dirextalk-project:" + strings.ToLower(serverName)
	if factory == nil {
		factory = func(ctx context.Context, config AgentGRPCDialConfig) (AgentGRPCRunner, error) {
			return p2p.NewAgentGRPCChatRunner(ctx, config)
		}
	}
	runner, err := factory(ctx, AgentGRPCDialConfig{
		Target: config.Target, CAFile: config.CAFile, ServerName: config.ServerName,
		ServiceKeyFile: config.ServiceKeyFile, OwnerID: ownerID,
	})
	if err != nil {
		if runner != nil {
			_ = runner.Close()
		}
		return nil, errors.New("Agent gRPC client construction failed")
	}
	if runner == nil {
		return nil, errors.New("Agent gRPC client construction returned no runner")
	}
	return runner, nil
}

func p2pAgentRuntimeProfileClient(config p2pAgentGRPCBackendConfig, runner AgentGRPCRunner) (p2p.AgentRuntimeProfileClient, error) {
	if !config.Enabled {
		return nil, nil
	}
	client, ok := runner.(p2p.AgentRuntimeProfileClient)
	if !ok || client == nil {
		return nil, errors.New("enabled Agent gRPC backend does not support runtime profile configuration")
	}
	return client, nil
}

func p2pAgentSearchProfileClient(config p2pAgentGRPCBackendConfig, runner AgentGRPCRunner) (p2p.AgentSearchProfileClient, error) {
	if !config.Enabled {
		return nil, nil
	}
	client, ok := runner.(p2p.AgentSearchProfileClient)
	if !ok || client == nil {
		return nil, errors.New("enabled Agent gRPC backend does not support search profile configuration")
	}
	return client, nil
}

func p2pAgentCompletionSource(
	config p2pAgentGRPCBackendConfig,
	runner AgentGRPCRunner,
) (p2p.AgentCompletionSource, error) {
	if !config.Enabled {
		return nil, nil
	}
	source, ok := runner.(p2p.AgentCompletionSource)
	if !ok || source == nil {
		return nil, errors.New(
			"enabled Agent gRPC backend does not support completion events",
		)
	}
	return source, nil
}

func p2pAgentCompletionSynthesizer(
	config p2pAgentGRPCBackendConfig,
	runner AgentGRPCRunner,
) (p2p.AgentCompletionSynthesizer, error) {
	if !config.Enabled {
		return nil, nil
	}
	synthesizer, ok := runner.(p2p.AgentCompletionSynthesizer)
	if !ok || synthesizer == nil {
		return nil, errors.New(
			"enabled Agent gRPC backend does not support completion synthesis",
		)
	}
	return synthesizer, nil
}

func p2pAgentArtifactSource(
	config p2pAgentGRPCBackendConfig,
	runner AgentGRPCRunner,
) (p2p.AgentArtifactSource, error) {
	if !config.Enabled {
		return nil, nil
	}
	source, ok := runner.(p2p.AgentArtifactSource)
	if !ok || source == nil {
		return nil, errors.New(
			"enabled Agent gRPC backend does not support artifact downloads",
		)
	}
	return source, nil
}

func p2pAgentConversationStateReader(
	config p2pAgentGRPCBackendConfig,
	runner AgentGRPCRunner,
) (p2p.AgentConversationStateReader, error) {
	if !config.Enabled {
		return nil, nil
	}
	reader, ok := runner.(p2p.AgentConversationStateReader)
	if !ok || reader == nil {
		return nil, errors.New(
			"enabled Agent gRPC backend does not support conversation state",
		)
	}
	return reader, nil
}

func startAgentCompletionRelayLifecycle(
	processCtx *process.ProcessContext,
	service *p2p.Service,
) {
	processCtx.ComponentStarted()
	go func() {
		defer processCtx.ComponentFinished()
		if err := service.RunAgentCompletionRelay(processCtx.Context()); err != nil && processCtx.Context().Err() == nil {
			logrus.Warn("P2P Agent completion relay stopped")
		}
	}()
}

func startAgentGRPCRunnerLifecycle(processCtx *process.ProcessContext, runner AgentGRPCRunner) {
	processCtx.ComponentStarted()
	go func() {
		defer processCtx.ComponentFinished()
		<-processCtx.Context().Done()
		if err := runner.Close(); err != nil {
			// Close errors are intentionally not logged because transport errors
			// are outside Message Server's secret-redaction boundary.
			logrus.Warn("P2P Agent gRPC connection did not close cleanly")
		}
	}()
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
