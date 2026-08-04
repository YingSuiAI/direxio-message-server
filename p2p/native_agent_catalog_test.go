package p2p

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
)

func TestNativeAgentCatalogReadinessRecoversAndExpires(t *testing.T) {
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
	readiness.probeNow(context.Background())
	if ready, _ := readiness.readyState(); ready {
		t.Fatal("down Agent was marked ready")
	}
	up = true
	readiness.probeNow(context.Background())
	if ready, err := readiness.readyState(); !ready || err != nil {
		t.Fatalf("recovered Agent remained unready: ready=%v err=%v", ready, err)
	}
	clock = clock.Add(10 * time.Second)
	if ready, _ := readiness.readyState(); ready {
		t.Fatal("expired catalog remained trusted")
	}
}

func TestNativeAgentCatalogReadinessGenerationFence(t *testing.T) {
	generation := int64(1)
	readiness := newNativeAgentCatalogReadiness(func(context.Context, []agentgateway.CatalogRequirement) error {
		generation = 2
		return nil
	}, nil, func() int64 { return generation })
	readiness.probeNow(context.Background())
	if ready, _ := readiness.readyState(); ready {
		t.Fatal("catalog probe crossing account generation was trusted")
	}
}

func TestNativeAgentCatalogRequirementsKeepOptionalCapabilitiesExplicit(t *testing.T) {
	requirements := nativeAgentCatalogRequirements(nil)
	for _, requirement := range requirements {
		if requirement.Action == "agent.voice.session.create" {
			t.Fatal("optional voice capability entered the base readiness catalog")
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
