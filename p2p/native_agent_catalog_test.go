package p2p

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
)

func TestNativeAgentCatalogProbeBudgetCoversInitialConnection(t *testing.T) {
	if nativeAgentCatalogProbeTimeout <= agentgateway.AgentCapabilityMinConnectTimeout {
		t.Fatalf(
			"catalog probe timeout %s must exceed minimum gRPC connect timeout %s",
			nativeAgentCatalogProbeTimeout,
			agentgateway.AgentCapabilityMinConnectTimeout,
		)
	}
}

func TestNativeAgentCatalogReadinessInitialFailureFailsClosed(t *testing.T) {
	clock := time.Unix(100, 0)
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		return errors.New("agent down")
	}, nil, func() int64 { return 7 })
	readiness.now = func() time.Time { return clock }
	readiness.ttl = 10 * time.Second
	readiness.probeNow(context.Background())
	if ready, _ := readiness.readyState(); ready {
		t.Fatal("down Agent was marked ready")
	}
	if !readiness.expiresAt.IsZero() {
		t.Fatalf("initial probe failure retained an expiry: %v", readiness.expiresAt)
	}
}

func TestNativeAgentCatalogReadinessRenewsBeforeLeaseExpiry(t *testing.T) {
	clock := time.Unix(100, 0)
	probes := 0
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		probes++
		return nil
	}, nil, func() int64 { return 7 })
	readiness.now = func() time.Time { return clock }
	readiness.ttl = 20 * time.Second
	readiness.interval = 5 * time.Second
	readiness.probeTO = 2 * time.Second
	readiness.probeNow(context.Background())
	firstExpiry := readiness.expiresAt

	clock = clock.Add(readiness.ttl - readiness.interval - readiness.probeTO)
	if ready, _ := readiness.readyState(); !ready {
		t.Fatal("catalog lost readiness before its lease expired")
	}
	if !readiness.shouldProbe() {
		t.Fatal("catalog did not schedule a renewal within the probe safety window")
	}
	readiness.probeNow(context.Background())
	if probes != 2 {
		t.Fatalf("probe count = %d, want 2", probes)
	}
	if !readiness.expiresAt.After(firstExpiry) {
		t.Fatalf("renewal did not extend expiry: before=%v after=%v", firstExpiry, readiness.expiresAt)
	}
	if ready, err := readiness.readyState(); !ready || err != nil {
		t.Fatalf("renewed catalog was not ready: ready=%v err=%v", ready, err)
	}
	clock = readiness.expiresAt
	if ready, _ := readiness.readyState(); ready {
		t.Fatal("catalog remained ready after the renewed lease expired")
	}
}

func TestNativeAgentCatalogReadinessValidLeaseSurvivesProbeFailure(t *testing.T) {
	clock := time.Unix(100, 0)
	up := true
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		if !up {
			return errors.New("agent down")
		}
		return nil
	}, nil, func() int64 { return 7 })
	readiness.now = func() time.Time { return clock }
	readiness.ttl = 10 * time.Second
	readiness.probeNow(context.Background())
	firstExpiry := readiness.expiresAt

	clock = clock.Add(time.Second)
	up = false
	readiness.probeNow(context.Background())
	if ready, err := readiness.readyState(); !ready || err != nil {
		t.Fatalf("valid lease was revoked by a failed renewal: ready=%v err=%v", ready, err)
	}
	if readiness.expiresAt != firstExpiry {
		t.Fatalf("failed renewal changed the active lease: before=%v after=%v", firstExpiry, readiness.expiresAt)
	}
	if readiness.lastErr == nil {
		t.Fatal("failed renewal was not recorded")
	}
}

func TestNativeAgentCatalogReadinessExpiredLeaseFailureFailsClosed(t *testing.T) {
	clock := time.Unix(100, 0)
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		return nil
	}, nil, func() int64 { return 7 })
	readiness.now = func() time.Time { return clock }
	readiness.ttl = 10 * time.Second
	readiness.probeNow(context.Background())
	clock = readiness.expiresAt
	readiness.probe = func(context.Context, []agentgateway.CatalogRequirement) error {
		return errors.New("agent down")
	}
	readiness.probeNow(context.Background())
	if ready, _ := readiness.readyState(); ready {
		t.Fatal("expired catalog remained ready after a failed probe")
	}
	if !readiness.expiresAt.IsZero() {
		t.Fatalf("expired lease retained expiry: %v", readiness.expiresAt)
	}
}

func TestNativeAgentCatalogReadinessNotifiesOnlyStateTransitions(t *testing.T) {
	clock := time.Unix(100, 0)
	up := false
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		if !up {
			return errors.New("agent down")
		}
		return nil
	}, nil, func() int64 { return 7 })
	readiness.now = func() time.Time { return clock }
	readiness.ttl = 10 * time.Second
	var transitions []bool
	readiness.onReady = func(ready bool) {
		transitions = append(transitions, ready)
	}

	readiness.probeNow(context.Background())
	up = true
	readiness.probeNow(context.Background())
	readiness.probeNow(context.Background())
	if len(transitions) != 1 || !transitions[0] {
		t.Fatalf("healthy renewals transitions = %#v, want [true]", transitions)
	}

	up = false
	clock = readiness.expiresAt.Add(-time.Second)
	readiness.probeNow(context.Background())
	if len(transitions) != 1 {
		t.Fatalf("valid lease failure changed readiness: %#v", transitions)
	}
	clock = readiness.expiresAt
	readiness.probeNow(context.Background())
	if len(transitions) != 2 || transitions[1] {
		t.Fatalf("expired lease transitions = %#v, want [true false]", transitions)
	}
}

func TestNativeAgentCatalogReadinessRetriesFailedPublication(t *testing.T) {
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		return nil
	}, nil, func() int64 { return 7 })
	readiness.now = func() time.Time { return time.Unix(100, 0) }
	readiness.ttl = 10 * time.Second
	publicationAttempts := 0
	readiness.publish = func(online bool) error {
		publicationAttempts++
		if !online {
			t.Fatal("healthy catalog attempted to publish offline")
		}
		if publicationAttempts == 1 {
			return errors.New("temporary Matrix write failure")
		}
		return nil
	}

	readiness.probeNow(context.Background())
	readiness.probeNow(context.Background())
	readiness.probeNow(context.Background())

	if publicationAttempts != 2 {
		t.Fatalf("publication attempts = %d, want one retry and then suppression", publicationAttempts)
	}
}

func TestNativeAgentCatalogReadinessPublishesEffectiveDisabledState(t *testing.T) {
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		return nil
	}, nil, func() int64 { return 7 })
	readiness.now = func() time.Time { return time.Unix(100, 0) }
	readiness.ttl = 10 * time.Second
	enabled := false
	readiness.publishable = func(ready bool) bool { return ready && enabled }
	var publications []bool
	readiness.publish = func(online bool) error {
		publications = append(publications, online)
		return nil
	}

	readiness.probeNow(context.Background())
	enabled = true
	readiness.probeNow(context.Background())

	if len(publications) != 2 || publications[0] || !publications[1] {
		t.Fatalf("effective publications = %#v, want [false true]", publications)
	}
}

func TestNativeAgentCatalogReadinessRepublishesAfterExplicitOffline(t *testing.T) {
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		return nil
	}, nil, func() int64 { return 7 })
	readiness.now = func() time.Time { return time.Unix(100, 0) }
	readiness.ttl = 10 * time.Second
	var publications []bool
	readiness.publish = func(online bool) error {
		publications = append(publications, online)
		return nil
	}

	readiness.probeNow(context.Background())
	readiness.recordPublished(false)
	readiness.probeNow(context.Background())

	if len(publications) != 2 || !publications[0] || !publications[1] {
		t.Fatalf("publications after explicit offline = %#v, want [true true]", publications)
	}
}

func TestNativeAgentCatalogReadinessGenerationFence(t *testing.T) {
	generation := int64(1)
	up := false
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		if !up {
			return nil
		}
		generation = 2
		return errors.New("replacement Agent is not ready")
	}, nil, func() int64 { return generation })
	readiness.now = func() time.Time { return time.Unix(100, 0) }
	readiness.ttl = 10 * time.Second
	readiness.probeNow(context.Background())
	up = true
	readiness.probeNow(context.Background())
	if ready, _ := readiness.readyState(); ready {
		t.Fatal("catalog probe crossing account generation was trusted")
	}
	if !readiness.expiresAt.IsZero() {
		t.Fatalf("generation-fenced probe retained expiry: %v", readiness.expiresAt)
	}
}

func TestNativeAgentCatalogRequirementsKeepOptionalCapabilitiesExplicit(t *testing.T) {
	requirements := nativeAgentCatalogRequirements(nil)
	requiredActions := map[string]bool{
		"agent.chat.turn.stop":            false,
		"agent.chat.turn.steer":           false,
		"agent.chat.turns.list":           false,
		"agent.models.list":               false,
		"agent.web_search.config.get":     false,
		"agent.web_search.config.update":  false,
		"agent.web_search.test":           false,
		"agent.text_tools.config.get":     false,
		"agent.text_tools.config.update":  false,
		"agent.text_tools.execute":        false,
		"agent.knowledge.config.get":      false,
		"agent.knowledge.config.update":   false,
		"agent.knowledge.sources.list":    false,
		"agent.knowledge.sources.delete":  false,
		"agent.knowledge.upload.start":    false,
		"agent.knowledge.upload.chunk":    false,
		"agent.knowledge.upload.finish":   false,
		"agent.knowledge.memory.create":   false,
		"agent.knowledge.memories.list":   false,
		"agent.knowledge.memories.update": false,
		"agent.knowledge.memories.delete": false,
		"agent.knowledge.search":          false,
		"agent.knowledge.status":          false,
	}
	for _, requirement := range requirements {
		if !requirement.RequireSchemaPin {
			t.Errorf("baseline Native Agent action %q is not schema-pin gated", requirement.Action)
		}
		if len(requirement.InputSchemaDigest) != 32 || len(requirement.ResultSchemaDigest) != 32 {
			t.Errorf("baseline Native Agent action %q is missing pinned schema digests", requirement.Action)
		}
		if requirement.Action == "agent.chat.stream" && (!requirement.RequireEventSchemaPin || len(requirement.EventSchemaDigest) != 32) {
			t.Error("durable Native Agent chat stream is missing its event schema pin")
		}
		if requirement.Action == "agent.voice.session.create" {
			t.Fatal("optional voice capability entered the base readiness catalog")
		}
		if _, tracked := requiredActions[requirement.Action]; tracked {
			requiredActions[requirement.Action] = true
		}
	}
	for action, found := range requiredActions {
		if !found {
			t.Errorf("required Native Agent action %q is missing from the readiness catalog", action)
		}
	}

	requirements = nativeAgentCatalogRequirements([]string{"agent.voice.session.create"})
	found := false
	for _, requirement := range requirements {
		if requirement.Action == "agent.voice.session.create" {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit optional voice requirement was dropped")
	}
}

func TestImageToolsDoNotBlockNativeAgentBaselineReadiness(t *testing.T) {
	for _, requirement := range nativeAgentCatalogRequirements(nil) {
		if strings.HasPrefix(requirement.Action, "agent.image_tools.") {
			t.Fatalf("optional image action %s was added to the global Native Agent readiness baseline", requirement.Action)
		}
	}
}

func TestPortalSessionRotationDoesNotChangeExternalAccountGeneration(t *testing.T) {
	service := NewService(Config{AccountGeneration: 42, NativeAgentRunner: &externalNativeRunnerProbe{}})
	beforeSession := service.portalSessionGeneration
	servicePortalModulePort{service: service}.CommitMatrixSession("matrix-token", "device-2")
	if service.accountGeneration != 42 {
		t.Fatalf("external account generation changed with Matrix session: %d", service.accountGeneration)
	}
	if service.portalSessionGeneration == beforeSession {
		t.Fatal("Matrix portal session generation did not advance")
	}
}
