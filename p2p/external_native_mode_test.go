package p2p

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	"github.com/YingSuiAI/dirextalk-message-server/setup/process"
)

type externalDeprovisionRunner struct{ calls int }

func (r *externalDeprovisionRunner) Apply(context.Context, string) error { return nil }
func (r *externalDeprovisionRunner) ProbeCatalog(context.Context, []agentgateway.CatalogRequirement) error {
	return nil
}
func (r *externalDeprovisionRunner) Invoke(_ context.Context, action string, params map[string]any) (map[string]any, error) {
	if action != "agent.account.deprovision" {
		return map[string]any{"ok": true}, nil
	}
	r.calls++
	if params["confirm"] != "deprovision_account" || params["idempotency_key"] == "" {
		return nil, context.Canceled
	}
	return map[string]any{"status": "deprovisioned"}, nil
}
func (r *externalDeprovisionRunner) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	return nil
}

type externalNativeRunnerProbe struct {
	invoked     string
	invokeCalls int
	streamCalls int
}

func (p *externalNativeRunnerProbe) Apply(context.Context, string) error { return nil }

func (p *externalNativeRunnerProbe) ProbeCatalog(context.Context, []agentgateway.CatalogRequirement) error {
	return nil
}

func (p *externalNativeRunnerProbe) Invoke(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	p.invoked = action
	p.invokeCalls++
	return map[string]any{"ok": true}, nil
}

func (p *externalNativeRunnerProbe) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	p.streamCalls++
	return nil
}

func TestExternalNativeAgentModeDoesNotConstructEmbeddedRuntime(t *testing.T) {
	probe := &externalNativeRunnerProbe{}
	service := NewService(Config{
		ServerName:        "example.test",
		NativeAgentRunner: probe,
	})
	if service == nil || service.agentModule == nil {
		t.Fatal("external mode must retain the public facade module")
	}
	processCtx := process.NewProcessContext()
	processCtx.ShutdownDendrite()
	handler := service.actions["agent.chat"]
	if handler == nil {
		t.Fatal("external mode must preserve the agent.chat action surface")
	}
	if _, actionErr := handler(context.Background(), map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"message":                "hello",
		"model_profile_id":       "00000000-0000-4000-8000-000000000001",
		"model_profile_revision": int64(1),
		"credential_version":     int64(1),
	}); actionErr != nil {
		t.Fatalf("external agent action failed: %v", actionErr)
	}
	if probe.invoked != "agent.chat" {
		t.Fatalf("external action did not reach gateway runner: %q", probe.invoked)
	}
	coreHandler := service.actions["agent.core.tasks.list"]
	if coreHandler == nil {
		t.Fatal("external mode must preserve the core task action surface")
	}
	if _, actionErr := coreHandler(context.Background(), map[string]any{}); actionErr != nil {
		t.Fatalf("external core task action failed: %v", actionErr)
	}
	if probe.invoked != "agent.core.tasks.list" {
		t.Fatalf("external core task action did not reach gateway runner: %q", probe.invoked)
	}
	turnsHandler := service.actions["agent.chat.turns.list"]
	if turnsHandler == nil {
		t.Fatal("external mode must preserve the durable turn listing action")
	}
	if _, actionErr := turnsHandler(context.Background(), map[string]any{"conversation_id": "22222222-2222-4222-8222-222222222222"}); actionErr != nil {
		t.Fatalf("external turn listing failed: %v", actionErr)
	}
	if probe.invoked != "agent.chat.turns.list" {
		t.Fatalf("external turn listing did not reach Agent Core: %q", probe.invoked)
	}
}

func TestNativeAgentSensitiveChatKeyRejectedAtHTTPProductAction(t *testing.T) {
	const secret = "http-secret-canary"
	probe := &externalNativeRunnerProbe{}
	service := NewService(Config{ServerName: "example.test", NativeAgentRunner: probe})
	router := newP2PTestRouter(service)
	req := jsonRequest(t, PathPrefix+"query", map[string]any{
		"action": "agent.chat",
		"params": map[string]any{
			"idempotency_key":        "33333333-3333-4333-8333-333333333333",
			"message":                "hello",
			"model_profile_id":       "profile-id",
			"model_profile_revision": int64(2),
			"credential_version":     int64(3),
			"metadata":               []any{map[string]any{"dbPass": secret}},
		},
	})
	req.Header.Set("Authorization", "Bearer "+service.AccessToken())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("sensitive HTTP ProductAction status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if probe.invokeCalls != 0 {
		t.Fatalf("sensitive HTTP ProductAction reached Agent runner %d time(s)", probe.invokeCalls)
	}
	if strings.Contains(recorder.Body.String(), secret) {
		t.Fatalf("sensitive HTTP ProductAction response leaked value: %s", recorder.Body.String())
	}
}

func TestNativeAgentJSONProfilePinsAcceptedAtHTTPProductAction(t *testing.T) {
	probe := &externalNativeRunnerProbe{}
	service := NewService(Config{ServerName: "example.test", NativeAgentRunner: probe})
	req := jsonRequest(t, PathPrefix+"query", map[string]any{
		"action": "agent.chat",
		"params": map[string]any{
			"idempotency_key":        "44444444-4444-4444-8444-444444444444",
			"message":                "hello",
			"model_profile_id":       "profile-id",
			"model_profile_revision": int64(2),
			"credential_version":     int64(3),
		},
	})
	req.Header.Set("Authorization", "Bearer "+service.AccessToken())
	recorder := httptest.NewRecorder()
	newP2PTestRouter(service).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("valid HTTP ProductAction status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if probe.invokeCalls != 1 {
		t.Fatalf("valid HTTP ProductAction runner calls = %d, want 1", probe.invokeCalls)
	}
}

func TestExternalNativeAgentDeletionRequiresAgentDeprovision(t *testing.T) {
	runner := &externalDeprovisionRunner{}
	service := NewService(Config{ServerName: "example.test", AccountGeneration: 3, NativeAgentRunner: runner})
	if err := service.deprovisionExternalAgent(context.Background()); err != nil {
		t.Fatalf("Agent deprovision: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("deprovision calls=%d", runner.calls)
	}
}

func TestAccountDeletionFailsClosedWithoutExternalAgentDeprovision(t *testing.T) {
	service := NewService(Config{ServerName: "example.test", AccountGeneration: 3})
	if apiErr := service.deprovisionExternalAgent(context.Background()); apiErr == nil || apiErr.Status != http.StatusBadGateway {
		t.Fatalf("missing external Agent deprovision must fail closed, got %#v", apiErr)
	}
}

func TestExternalNativeAgentReadinessRequiresRunner(t *testing.T) {
	service := NewService(Config{ServerName: "example.test"})
	if err := service.agentModule.ReadyError(); err == nil {
		t.Fatal("missing external Agent runner must fail readiness")
	}
}

func TestNativeAgentActionReadinessFailsClosedInitiallyAndAfterLeaseExpiry(t *testing.T) {
	t.Run("initial probe failure", func(t *testing.T) {
		probe := &externalNativeRunnerProbe{}
		service := NewService(Config{
			ServerName: "example.test", NativeAgentRunner: probe,
			NativeAgentCatalogProbe: func(context.Context, []agentgateway.CatalogRequirement) error {
				return context.DeadlineExceeded
			},
		})
		defer service.StopNativeAgentCatalogProbe()
		if _, actionErr := service.actions["agent.core.tasks.list"](context.Background(), map[string]any{}); actionErr == nil || actionErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("initially unready action error = %#v, want HTTP 503", actionErr)
		}
		if probe.invokeCalls != 0 {
			t.Fatalf("initially unready action reached Agent %d time(s)", probe.invokeCalls)
		}
	})

	t.Run("expired lease", func(t *testing.T) {
		probe := &externalNativeRunnerProbe{}
		service := NewService(Config{ServerName: "example.test", NativeAgentRunner: probe})
		defer service.StopNativeAgentCatalogProbe()
		service.nativeAgentCatalog.mu.RLock()
		expiresAt := service.nativeAgentCatalog.expiresAt
		service.nativeAgentCatalog.mu.RUnlock()
		service.nativeAgentCatalog.now = func() time.Time { return expiresAt }
		if _, actionErr := service.actions["agent.core.tasks.list"](context.Background(), map[string]any{}); actionErr == nil || actionErr.Status != http.StatusServiceUnavailable {
			t.Fatalf("expired catalog action error = %#v, want HTTP 503", actionErr)
		}
		if probe.invokeCalls != 0 {
			t.Fatalf("expired catalog action reached Agent %d time(s)", probe.invokeCalls)
		}
	})
}
