package p2p

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	"github.com/sirupsen/logrus"
)

const (
	// Leave time for the initial gRPC connection window plus the authenticated
	// DescribeCapabilities RPC. A shorter probe deadline can fail before the
	// transport's minimum connection attempt has completed.
	nativeAgentCatalogProbeTimeout = agentgateway.AgentCapabilityMinConnectTimeout + 2*time.Second
	nativeAgentCatalogTTL          = 20 * time.Second
	nativeAgentCatalogProbeEvery   = 5 * time.Second
)

// nativeAgentCatalogReadiness is intentionally independent of the request
// path. Health/readiness only trusts a recent, generation-bound catalog probe;
// action requests still perform their own live DescribeCapabilities lookup.
type nativeAgentCatalogReadiness struct {
	probe       func(context.Context, []agentgateway.CatalogRequirement) error
	requirement []agentgateway.CatalogRequirement
	generation  func() int64
	publish     func(bool) error
	publishable func(bool) bool
	now         func() time.Time
	ttl         time.Duration
	interval    time.Duration
	probeTO     time.Duration

	mu         sync.RWMutex
	ready      bool
	probedGen  int64
	expiresAt  time.Time
	lastErr    error
	probing    bool
	published  bool
	hasPublish bool
	cancel     context.CancelFunc
	done       chan struct{}
}

func newNativeAgentCatalogReadiness(probe func(context.Context, []agentgateway.CatalogRequirement) error, requirements []agentgateway.CatalogRequirement, generation func() int64) *nativeAgentCatalogReadiness {
	if probe == nil {
		return nil
	}
	if generation == nil {
		generation = func() int64 { return 0 }
	}
	return &nativeAgentCatalogReadiness{
		probe:       probe,
		requirement: append([]agentgateway.CatalogRequirement(nil), requirements...),
		generation:  generation,
		now:         time.Now,
		ttl:         nativeAgentCatalogTTL,
		interval:    nativeAgentCatalogProbeEvery,
		probeTO:     nativeAgentCatalogProbeTimeout,
	}
}

func (r *nativeAgentCatalogReadiness) start() {
	if r == nil || r.probe == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancel = cancel
	r.done = make(chan struct{})
	r.mu.Unlock()

	// The first probe is bounded and synchronous so startup/health cannot report
	// ready before the peer has proven the catalog. A down peer does not prevent
	// the ProductCore process from serving unrelated product traffic.
	r.probeNow(ctx)
	go r.loop(ctx)
}

func (r *nativeAgentCatalogReadiness) loop(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	defer func() {
		r.mu.Lock()
		if r.done != nil {
			close(r.done)
			r.done = nil
		}
		r.mu.Unlock()
	}()
	for {
		select {
		case <-ticker.C:
			if !r.shouldProbe() {
				continue
			}
			r.probeNow(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (r *nativeAgentCatalogReadiness) shouldProbe() bool {
	if r == nil || r.probe == nil {
		return false
	}
	generation := r.generation()
	now := r.now()
	r.mu.RLock()
	ready := r.ready && r.probedGen == generation && !r.expiresAt.IsZero() && now.Before(r.expiresAt)
	expiresAt := r.expiresAt
	r.mu.RUnlock()
	if !ready {
		return true
	}
	return !now.Add(r.interval + r.probeTO).Before(expiresAt)
}

func (r *nativeAgentCatalogReadiness) probeNow(parent context.Context) {
	if r == nil || r.probe == nil {
		return
	}
	r.mu.Lock()
	if r.probing {
		r.mu.Unlock()
		return
	}
	r.probing = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.probing = false
		r.mu.Unlock()
	}()

	generation := r.generation()
	ctx, cancel := context.WithTimeout(parent, r.probeTO)
	err := r.probe(ctx, r.requirement)
	cancel()
	observedGeneration := r.generation()
	generationChanged := generation != observedGeneration
	if generationChanged {
		err = errors.New("account generation changed during native agent catalog probe")
	}
	r.mu.Lock()
	if err != nil {
		leaseValid := !generationChanged && r.ready && r.probedGen == generation && !r.expiresAt.IsZero() && r.now().Before(r.expiresAt)
		if !leaseValid {
			r.ready = false
			r.expiresAt = time.Time{}
		}
		r.probedGen = observedGeneration
		r.lastErr = err
		isReady := r.ready
		r.mu.Unlock()
		r.publishReadiness(isReady)
		logrus.WithError(err).WithField("account_generation", generation).Warn("Native Agent capability catalog probe failed")
		return
	}
	r.ready = true
	r.probedGen = generation
	r.expiresAt = r.now().Add(r.ttl)
	r.lastErr = nil
	isReady := r.ready
	r.mu.Unlock()
	r.publishReadiness(isReady)
}

func (r *nativeAgentCatalogReadiness) publishReadiness(ready bool) {
	if r == nil || r.publish == nil {
		return
	}
	desired := ready
	if r.publishable != nil {
		desired = r.publishable(ready)
	}
	r.mu.RLock()
	alreadyPublished := r.hasPublish && r.published == desired
	r.mu.RUnlock()
	if alreadyPublished {
		return
	}
	if err := r.publish(desired); err != nil {
		logrus.WithError(err).WithField("online", desired).Warn("Native Agent readiness state publish failed")
		return
	}
	r.mu.Lock()
	r.published = desired
	r.hasPublish = true
	r.mu.Unlock()
}

func (r *nativeAgentCatalogReadiness) recordPublished(online bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.published = online
	r.hasPublish = true
	r.mu.Unlock()
}

func (r *nativeAgentCatalogReadiness) readyState() (bool, error) {
	if r == nil {
		return true, nil
	}
	generation := r.generation()
	r.mu.RLock()
	ready := r.ready && r.probedGen == generation && !r.expiresAt.IsZero() && r.now().Before(r.expiresAt)
	err := r.lastErr
	r.mu.RUnlock()
	if ready {
		return true, nil
	}
	if err == nil {
		err = errors.New("native agent catalog has not passed a fresh probe")
	}
	return false, err
}

func (r *nativeAgentCatalogReadiness) stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	done := r.done
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) nativeAgentCatalogReadinessError() error {
	if s == nil || s.nativeAgentCatalog == nil {
		return fmt.Errorf("external native agent capability catalog is not configured")
	}
	if ready, _ := s.nativeAgentCatalog.readyState(); ready {
		return nil
	}
	return fmt.Errorf("external native agent capability catalog is not ready")
}

// StopNativeAgentCatalogProbe is registered with the process shutdown path in
// production and is harmless for tests or embedded mode.
func (s *Service) StopNativeAgentCatalogProbe() {
	if s != nil && s.nativeAgentCatalog != nil {
		s.nativeAgentCatalog.stop()
	}
}

func nativeAgentCatalogRequirements(extra []string) []agentgateway.CatalogRequirement {
	base := []string{
		"agent.account.deprovision",
		"agent.backends.get", "agent.models.list",
		"agent.config.get", "agent.config.update",
		"agent.chat", "agent.chat.stream",
		"agent.chat.attachment.begin", "agent.chat.attachment.append", "agent.chat.attachment.commit",
		"agent.web_search.config.get", "agent.web_search.config.update", "agent.web_search.test",
		"agent.memory.config.get", "agent.memory.config.update", "agent.memory.status", "agent.memory.facts.update", "agent.memory.facts.delete",
		"agent.static_sites.list", "agent.static_sites.delete",
		"agent.text_tools.config.get", "agent.text_tools.config.update", "agent.text_tools.execute",
		"agent.chat.conversations.create", "agent.chat.conversations.list", "agent.chat.conversations.get", "agent.chat.conversations.rename", "agent.chat.conversations.delete", "agent.chat.turn.stop", "agent.chat.turn.steer", "agent.chat.turns.list",
		"agent.context.compress", "agent.summarize",
		"agent.model_profiles.sync", "agent.model_profiles.list", "agent.model_profiles.get", "agent.model_profiles.test", "agent.model_profiles.delete",
		"agent.knowledge.config.get", "agent.knowledge.config.update", "agent.knowledge.sources.list", "agent.knowledge.sources.delete", "agent.knowledge.upload.start", "agent.knowledge.upload.chunk", "agent.knowledge.upload.finish", "agent.knowledge.search", "agent.knowledge.status",
		"agent.core.tasks.get", "agent.core.tasks.list", "agent.core.tasks.cancel", "agent.core.tasks.retry", "agent.core.tasks.events",
		"agent.core.schedules.create", "agent.core.schedules.get", "agent.core.schedules.list", "agent.core.schedules.update", "agent.core.schedules.pause", "agent.core.schedules.resume", "agent.core.schedules.trigger", "agent.core.schedules.delete",
		"agent.core.confirmations.get", "agent.core.confirmations.list", "agent.core.confirmations.confirm", "agent.core.confirmations.reject", "agent.core.confirmations.acknowledge_extension_execution_uncertain",
	}
	seen := make(map[string]struct{}, len(base)+len(extra))
	requirements := make([]agentgateway.CatalogRequirement, 0, len(base)+len(extra))
	for index, action := range append(base, extra...) {
		action = strings.TrimSpace(action)
		if action == "" {
			continue
		}
		if _, exists := seen[action]; exists {
			continue
		}
		seen[action] = struct{}{}
		requirement := agentgateway.NewCatalogRequirement(action)
		// Every baseline binding is a release gate. If the generated digest table
		// is missing an action (for example while Agent is adding a new schema),
		// readiness must fail closed rather than accepting a self-consistent but
		// incompatible descriptor. Explicit extras remain optional and use the
		// normal self-consistency proof unless they are pinned in the table.
		if index < len(base) {
			requirement.RequireSchemaPin = true
		}
		requirements = append(requirements, requirement)
	}
	return requirements
}

type nativeAgentCatalogProbe interface {
	ProbeCatalog(context.Context, []agentgateway.CatalogRequirement) error
}

func (s *Service) configureNativeAgentCatalogReadiness(cfg Config) {
	if s == nil {
		return
	}
	var probe func(context.Context, []agentgateway.CatalogRequirement) error
	currentGeneration := func() int64 {
		return s.accountGeneration
	}
	if cfg.NativeAgentCatalogProbe != nil {
		probe = cfg.NativeAgentCatalogProbe
	} else if candidate, ok := cfg.NativeAgentRunner.(nativeAgentCatalogProbe); ok {
		probe = func(ctx context.Context, requirements []agentgateway.CatalogRequirement) error {
			// Account deletion/recreation advances the Service generation. Keep the
			// gateway metadata in lockstep before a recovery probe; otherwise a
			// healthy replacement Agent would be rejected with the old fence.
			if cfg.NativeAgentGateway != nil {
				cfg.NativeAgentGateway.SetAccountGeneration(currentGeneration())
			}
			return candidate.ProbeCatalog(ctx, requirements)
		}
	}
	// Tests may inject a runner without a live gateway. Production's setup
	// rejects a missing gateway before Service construction, so lack of a probe
	// here means this instance has no catalog authority and must not be marked
	// ready.
	if probe == nil {
		return
	}
	requirements := nativeAgentCatalogRequirements(cfg.NativeAgentRequiredActions)
	s.nativeAgentCatalog = newNativeAgentCatalogReadiness(probe, requirements, currentGeneration)
	s.nativeAgentCatalog.publishable = func(ready bool) bool {
		s.mu.Lock()
		enabled := s.agentConfig.Enabled
		s.mu.Unlock()
		return ready && enabled
	}
	s.nativeAgentCatalog.publish = func(online bool) error {
		ctx, cancel := context.WithTimeout(context.Background(), nativeAgentCatalogProbeTimeout)
		defer cancel()
		return s.publishNativeAgentReadinessState(ctx, online)
	}
	s.nativeAgentCatalog.start()
}
