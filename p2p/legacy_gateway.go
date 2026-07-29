package p2p

import (
	"context"

	legacygatewaymodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/legacygateway"
	roomserverAPI "github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/setup/process"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/nats-io/nats.go"
)

type LegacyAgentGatewayIngress = legacygatewaymodule.Ingress

type LegacyAgentGatewaySenderResolver interface {
	QueryUserIDForSender(context.Context, spec.RoomID, spec.SenderID) (*spec.UserID, error)
}

type LegacyAgentGatewayConfig struct {
	TenantID       string
	ConversationID string
	Ingress        LegacyAgentGatewayIngress
	SenderResolver LegacyAgentGatewaySenderResolver
}

// ConfigureLegacyAgentGateway prepares the compatibility module but does not
// start its room consumer. Production callers must first prove that the old
// Connect room consumer is stopped and fenced, so one input cannot execute in
// both paths.
func (s *Service) ConfigureLegacyAgentGateway(cfg LegacyAgentGatewayConfig) error {
	var resolveSender legacygatewaymodule.SenderResolver
	if cfg.SenderResolver != nil {
		resolveSender = cfg.SenderResolver.QueryUserIDForSender
	}
	module, err := legacygatewaymodule.New(s.store, cfg.Ingress, legacygatewaymodule.Config{
		TenantID:       cfg.TenantID,
		ConversationID: cfg.ConversationID,
		Identity: func() legacygatewaymodule.Identity {
			s.mu.Lock()
			defer s.mu.Unlock()
			return legacygatewaymodule.Identity{
				AgentRoomID: s.agentRoomID,
				OwnerMXID:   s.ownerMXID,
			}
		},
		ResolveSender: resolveSender,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.legacyAgentGatewayModule = module
	s.mu.Unlock()
	return nil
}

func (s *Service) LegacyAgentGatewayEnabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legacyAgentGatewayModule != nil
}

func (s *Service) ProcessLegacyAgentGatewayOutputEvent(ctx context.Context, output roomserverAPI.OutputEvent) error {
	ctx, finishOperation := s.beginAccountOperation(ctx)
	defer finishOperation()
	if s.accountIsDeprovisioned() {
		return nil
	}
	s.mu.Lock()
	module := s.legacyAgentGatewayModule
	s.mu.Unlock()
	if module == nil {
		return nil
	}
	return module.ProcessOutputEvent(ctx, output)
}

type LegacyAgentGatewayOutputRoomEventConsumer = legacygatewaymodule.OutputRoomEventConsumer

// NewLegacyAgentGatewayOutputRoomEventConsumer constructs the independent
// durable consumer. It is intentionally not wired into the production
// monolith until the exclusive-consumer cutover is implemented.
func NewLegacyAgentGatewayOutputRoomEventConsumer(
	processContext *process.ProcessContext,
	cfg *config.JetStream,
	js nats.JetStreamContext,
	service *Service,
) *LegacyAgentGatewayOutputRoomEventConsumer {
	var handler legacygatewaymodule.OutputRoomEventHandler
	if service != nil {
		handler = service.ProcessLegacyAgentGatewayOutputEvent
	}
	return legacygatewaymodule.NewOutputRoomEventConsumer(processContext, cfg, js, handler)
}
