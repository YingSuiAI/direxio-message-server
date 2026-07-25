package p2p

import (
	"context"
	"fmt"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
)

type apiError = actionbase.Error
type actionHandler = actionbase.Handler

type actionHandlerModule struct {
	name     string
	handlers map[string]actionHandler
}

func (s *Service) actionHandlers() map[string]actionHandler {
	modules := []actionHandlerModule{
		{name: "portal", handlers: s.portalModule.Handlers()},
		s.collectActionHandlerModule("portal-cross-domain", s.registerPortalActions),
		{name: "release", handlers: s.releaseModule.Handlers()},
		{name: "profile", handlers: s.profileModule.Handlers()},
		s.collectActionHandlerModule("sync", s.registerSyncActions),
		{name: "conversations", handlers: s.conversationModule.Handlers()},
		{name: "agent", handlers: s.agentModule.Handlers()},
		{name: "agent-core", handlers: s.agentCore.Handlers()},
		{name: "plugins", handlers: s.pluginsModule.Handlers()},
		{name: "contacts", handlers: s.contactsModule.Handlers()},
		{name: "members", handlers: s.membersModule.Handlers()},
		{name: "blocks", handlers: s.blocksModule.Handlers()},
		{name: "social", handlers: s.socialModule.Handlers()},
		{name: "calls", handlers: s.callsModule.Handlers()},
		{name: "groups", handlers: s.groupsModule.Handlers()},
		{name: "channels", handlers: s.channelsModule.Handlers()},
		{name: "channel-content", handlers: s.channelContentModule.Handlers()},
		s.collectActionHandlerModule("channel-adapters", s.registerChannelActions),
		{name: "reports", handlers: s.reportsModule.Handlers()},
	}
	return mustBuildActionHandlers(
		serviceapi.ActionSpecs(),
		[]string{serviceapi.RealtimeWSTicketAction},
		modules,
	)
}

func (s *Service) collectActionHandlerModule(name string, register func(map[string]actionHandler)) actionHandlerModule {
	handlers := make(map[string]actionHandler)
	register(handlers)
	return actionHandlerModule{name: name, handlers: handlers}
}

func mustBuildActionHandlers(specs []serviceapi.ActionSpec, routeSpecial []string, modules []actionHandlerModule) map[string]actionHandler {
	registry, err := actionbase.NewRegistry(specs, routeSpecial...)
	if err != nil {
		panic(fmt.Sprintf("build ProductCore action registry: %v", err))
	}
	for _, module := range modules {
		if err := registry.Merge(module.name, module.handlers); err != nil {
			panic(fmt.Sprintf("build ProductCore action registry: %v", err))
		}
	}
	if err := registry.Validate(); err != nil {
		panic(fmt.Sprintf("build ProductCore action registry: %v", err))
	}
	return registry.Handlers()
}

func (s *Service) syncBootstrapAction(ctx context.Context, _ map[string]any) (any, *apiError) {
	return s.syncBootstrap(ctx)
}

func (s *Service) registerPortalActions(actions map[string]actionHandler) {
	actions["portal.account.delete"] = s.deleteAccount
}

func (s *Service) registerSyncActions(actions map[string]actionHandler) {
	actions["sync.bootstrap"] = s.syncBootstrapAction
	actions["sync.read_marker"] = s.updateReadMarker
}

func (s *Service) registerChannelActions(actions map[string]actionHandler) {
	actions["channels.read_marker"] = s.updateReadMarker
}
