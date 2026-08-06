package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/releasecontrol"
)

type recordingReleaseController struct {
	statusRequest      releasecontrol.StatusRequest
	applyRequest       releasecontrol.ApplyRequest
	directApplyRequest releasecontrol.DirectApplyRequest
	desiredStates      []releasecontrol.DesiredState
	desiredErrors      map[releasecontrol.DesiredState]error
	status             releasecontrol.UpdaterStatus
	directStatus       releasecontrol.DirectStatus
	ticket             releasecontrol.JobTicket
	directTicket       releasecontrol.JobTicket
	statusErr          error
	directStatusErr    error
	applyErr           error
	directApplyErr     error
	desiredErr         error
}

type recordingCentralVersionSource struct {
	version releasecontrol.CentralServerVersion
	err     error
	calls   int
}

type blockingClientBuildStore struct {
	Store
	mu             sync.Mutex
	state          portalState
	narrowEntered  chan struct{}
	releaseNarrow  chan struct{}
	fullReportSave chan struct{}
	portalSaved    chan portalState
}

func (s *blockingClientBuildStore) SavePortal(_ context.Context, state portalState) error {
	if state.ClientBuild.Version != "" {
		select {
		case s.fullReportSave <- struct{}{}:
		default:
		}
	}
	s.mu.Lock()
	if cleanMatrixDeviceID(s.state.MatrixDeviceID) == cleanMatrixDeviceID(state.MatrixDeviceID) {
		state.ClientBuild = s.state.ClientBuild
	}
	s.state = state
	s.mu.Unlock()
	if s.portalSaved != nil {
		s.portalSaved <- state
	}
	return nil
}

func (s *blockingClientBuildStore) SaveClientBuild(_ context.Context, expectedDeviceID string, build clientBuild) (bool, error) {
	close(s.narrowEntered)
	<-s.releaseNarrow
	s.mu.Lock()
	defer s.mu.Unlock()
	if cleanMatrixDeviceID(s.state.MatrixDeviceID) != cleanMatrixDeviceID(expectedDeviceID) {
		return false, nil
	}
	s.state.ClientBuild = build
	return true, nil
}

func (c *recordingReleaseController) Status(_ context.Context, request releasecontrol.StatusRequest) (releasecontrol.UpdaterStatus, error) {
	c.statusRequest = request
	return c.status, c.statusErr
}

func (c *recordingReleaseController) Apply(_ context.Context, request releasecontrol.ApplyRequest) (releasecontrol.JobTicket, error) {
	c.applyRequest = request
	return c.ticket, c.applyErr
}

func (c *recordingReleaseController) StatusDirect(_ context.Context) (releasecontrol.DirectStatus, error) {
	return c.directStatus, c.directStatusErr
}

func (c *recordingReleaseController) ApplyDirect(_ context.Context, request releasecontrol.DirectApplyRequest) (releasecontrol.JobTicket, error) {
	c.directApplyRequest = request
	return c.directTicket, c.directApplyErr
}

func (c *recordingReleaseController) SetDesiredState(_ context.Context, state releasecontrol.DesiredState) error {
	c.desiredStates = append(c.desiredStates, state)
	if c.desiredErrors != nil && c.desiredErrors[state] != nil {
		return c.desiredErrors[state]
	}
	return c.desiredErr
}

func (s *recordingCentralVersionSource) CurrentServerVersion(context.Context) (releasecontrol.CentralServerVersion, error) {
	s.calls++
	return s.version, s.err
}

func TestReleaseActionsOwnerAuthTransportAndPersistence(t *testing.T) {
	controller := &recordingReleaseController{
		status: releasecontrol.UpdaterStatus{
			Available:        true,
			ReleaseAvailable: true,
			UpdateAvailable:  true,
			DiscoveryStatus:  "fresh",
			CurrentVersion:   "v1.0.1",
			LatestVersion:    "v1.1.0",
			ClientVersion:    "v2.3.4",
			Compatibility:    "compatible",
			Operations:       []releasecontrol.Operation{{Kind: "upgrade", PlanToken: "opaque-plan", TargetVersion: "v1.1.0"}},
			Agent: releasecontrol.AgentReleaseStatus{
				Available: true, CurrentVersion: "v1.0.0", LatestVersion: "v1.1.0",
				MinimumServerVersion: "v1.1.0", UpdateAvailable: true,
				Reasons: []string{"agent_update_available"},
			},
			Watchdog: releasecontrol.WatchdogStatus{
				Status:         "degraded",
				Degraded:       true,
				CooldownUntil:  "2026-07-10T12:15:00Z",
				LastObservedAt: "2026-07-10T12:00:00Z",
				ErrorCode:      "repair_failed",
			},
		},
		ticket: releasecontrol.JobTicket{JobID: "job_test", JobToken: "job-secret", StatusURL: "/_dirextalk/updater/v1/jobs/job_test"},
	}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller})
	router := newP2PTestRouter(service)

	report := releaseRoute(t, router, service.AccessToken(), "client.version.report", map[string]any{
		"client_version": "2.3.4",
		"build_number":   "42",
		"platform":       "android",
	})
	if report["client_version"] != "v2.3.4" || report["device_id"] == "" || report["reported_at"] == "" {
		t.Fatalf("unexpected report response: %#v", report)
	}
	service.mu.Lock()
	persisted := service.portalStateLocked().ClientBuild
	service.mu.Unlock()
	if persisted.Version != "v2.3.4" || persisted.BuildNumber != "42" || persisted.Platform != "android" {
		t.Fatalf("client report was not persisted on portal state: %#v", persisted)
	}

	status := releaseRoute(t, router, service.AccessToken(), "release.v1.status", nil)
	if status["available"] != true || status["release_available"] != true || status["compatibility"] != "compatible" {
		t.Fatalf("unexpected release status: %#v", status)
	}
	agentStatus, ok := status["agent"].(map[string]any)
	if !ok || agentStatus["available"] != true || agentStatus["current_version"] != "v1.0.0" || agentStatus["latest_version"] != "v1.1.0" || agentStatus["minimum_server_version"] != "v1.1.0" || agentStatus["compatibility"] != "compatible" || agentStatus["update_available"] != true {
		t.Fatalf("unexpected Agent release status: %#v", status["agent"])
	}
	watchdog, ok := status["watchdog"].(map[string]any)
	if !ok || watchdog["status"] != "degraded" || watchdog["degraded"] != true || watchdog["error_code"] != "repair_failed" {
		t.Fatalf("unexpected public watchdog status: %#v", status["watchdog"])
	}
	if controller.statusRequest.CurrentVersion != "v1.1.1" || controller.statusRequest.CurrentSchemaVersion != 2 || controller.statusRequest.CurrentSchemaCompatVersion != 1 || controller.statusRequest.ClientVersion != "v2.3.4" {
		t.Fatalf("unexpected controller status request: %#v", controller.statusRequest)
	}

	apply := releaseRoute(t, router, service.AccessToken(), "release.v1.apply", map[string]any{
		"plan_token":      "opaque-plan",
		"idempotency_key": "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e",
		"confirm":         releasecontrol.ApplyConfirmation,
	})
	if apply["job_id"] != "job_test" || apply["job_token"] != "job-secret" {
		t.Fatalf("unexpected apply response: %#v", apply)
	}
	stateRaw, _ := json.Marshal(persisted)
	if strings.Contains(string(stateRaw), "job-secret") || strings.Contains(string(stateRaw), "opaque-plan") {
		t.Fatalf("job credentials entered durable portal state: %s", stateRaw)
	}

	for _, action := range []string{"client.version.report", "release.v1.status", "release.v1.apply"} {
		for _, token := range []string{"", service.AgentToken()} {
			response := releaseRouteRaw(t, router, token, action, map[string]any{})
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s token=%q expected 401, got %d body=%s", action, token, response.Code, response.Body.String())
			}
		}
	}

	wsIdentity, authorized := service.authorizeProductAction(service.AccessToken(), "client.version.report")
	if !authorized {
		t.Fatal("expected current owner session")
	}
	record := realtimeWSTicket{Role: "owner", UserID: service.OwnerMXID(), DeviceID: wsIdentity.DeviceID, Generation: wsIdentity.Generation}
	for _, action := range []string{"client.version.report", "release.v1.status"} {
		frame := service.handleRealtimeWSRequest(context.Background(), record, map[string]any{
			"id": "ws-" + action, "action": action, "params": map[string]any{"client_version": "v2.3.4"},
		})
		if frame["ok"] != true {
			t.Fatalf("%s must be available over owner WS: %#v", action, frame)
		}
	}
}

func TestReleaseStatusUnavailableIsParseable(t *testing.T) {
	controller := &recordingReleaseController{statusErr: errors.New("socket unavailable: secret-token")}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller})
	status := mustHandle[map[string]any](t, service, "release.v1.status", nil)
	if status["available"] != false || status["release_available"] != false || status["discovery_status"] != "unavailable" || status["compatibility"] != "unknown" {
		t.Fatalf("unexpected unavailable status: %#v", status)
	}
	agentStatus, ok := status["agent"].(map[string]any)
	if !ok || agentStatus["available"] != false || agentStatus["current_version"] != "" || agentStatus["latest_version"] != "" || agentStatus["compatibility"] != "unknown" {
		t.Fatalf("unavailable updater must produce honest Agent status: %#v", status["agent"])
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "secret-token") {
		t.Fatalf("controller error leaked into status: %s", raw)
	}
}

func TestReleaseStatusGatesAgentUpgradeAgainstMessageServerVersionOnly(t *testing.T) {
	controller := &recordingReleaseController{status: releasecontrol.UpdaterStatus{
		Available: true, DiscoveryStatus: "fresh", Compatibility: "compatible",
		Operations: []releasecontrol.Operation{
			{Kind: "agent_upgrade", PlanToken: "agent-plan", TargetVersion: "v1.1.0"},
		},
		Agent: releasecontrol.AgentReleaseStatus{
			Available: true, CurrentVersion: "v1.0.0", LatestVersion: "v1.1.0",
			MinimumServerVersion: "v1.1.2", UpdateAvailable: true,
		},
	}}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller})
	service.mu.Lock()
	service.clientBuild = clientBuild{Version: "v999.0.0"}
	service.mu.Unlock()

	status := mustHandle[map[string]any](t, service, "release.v1.status", nil)
	agentStatus := status["agent"].(map[string]any)
	if agentStatus["compatibility"] != "incompatible" || agentStatus["update_available"] != false {
		t.Fatalf("Agent gate did not use running message-server v1.1.1: %#v", agentStatus)
	}
	operations := status["operations"].([]releasecontrol.Operation)
	if len(operations) != 0 {
		t.Fatalf("incompatible Agent operation was exposed: %#v", operations)
	}
}

func TestReleaseStatusRejectsNoncanonicalAgentVersions(t *testing.T) {
	controller := &recordingReleaseController{status: releasecontrol.UpdaterStatus{
		Available: true, DiscoveryStatus: "fresh", Compatibility: "compatible",
		Agent: releasecontrol.AgentReleaseStatus{
			Available: true, CurrentVersion: "1.0.0", LatestVersion: "v1.1.0",
			MinimumServerVersion: "v1.1.1", UpdateAvailable: true,
		},
	}}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller})
	status := mustHandle[map[string]any](t, service, "release.v1.status", nil)
	agentStatus := status["agent"].(map[string]any)
	if agentStatus["available"] != false || agentStatus["current_version"] != "" || agentStatus["latest_version"] != "" {
		t.Fatalf("noncanonical Agent version was exposed: %#v", agentStatus)
	}
}

func TestReleaseStatusKeepsLocalCurrentAndClientVersionsAuthoritative(t *testing.T) {
	controller := &recordingReleaseController{status: releasecontrol.UpdaterStatus{
		Available:       true,
		DiscoveryStatus: "fresh",
		CurrentVersion:  "v999.0.0",
		ClientVersion:   "v888.0.0",
		Compatibility:   "compatible",
	}}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller})
	service.mu.Lock()
	service.clientBuild = clientBuild{Version: "v2.3.4"}
	service.mu.Unlock()

	status := mustHandle[map[string]any](t, service, "release.v1.status", nil)
	if status["current_version"] != "v1.1.1" || status["client_version"] != "v2.3.4" {
		t.Fatalf("updater echo replaced authoritative local versions: %#v", status)
	}
}

func TestReleaseV2StatusIsOwnerHTTPOnlyAndContainsNoDiscoveryPlan(t *testing.T) {
	controller := &recordingReleaseController{directStatus: releasecontrol.DirectStatus{
		Available:      true,
		UpdaterReady:   false,
		CurrentVersion: "v1.1.1",
		DesiredState:   "upgrading",
		ActiveJob: &releasecontrol.ActiveJob{
			JobID: "job_active", Status: "pulling", CurrentVersion: "v1.0.2", TargetVersion: "v1.0.3", ServiceAvailable: true,
		},
		Watchdog: releasecontrol.WatchdogStatus{Status: "suppressed"},
	}}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller})
	router := newP2PTestRouter(service)
	releaseRoute(t, router, service.AccessToken(), "client.version.report", map[string]any{"client_version": "v1.0.2"})

	status := releaseRoute(t, router, service.AccessToken(), "release.v2.status", nil)
	if status["current_version"] != "v1.1.1" || status["client_version"] != "v1.0.2" || status["available"] != false || status["updater_available"] != true || status["updater_ready"] != false || status["desired_state"] != "upgrading" {
		t.Fatalf("unexpected release v2 status: %#v", status)
	}
	if _, exists := status["release_available"]; exists {
		t.Fatalf("release v2 status must not expose discovery fields: %#v", status)
	}
	if _, exists := status["operations"]; exists {
		t.Fatalf("release v2 status must not expose executable plans: %#v", status)
	}
	job, ok := status["active_job"].(map[string]any)
	if !ok || job["job_id"] != "job_active" || job["status"] != "pulling" || job["target_version"] != "v1.0.3" {
		t.Fatalf("unexpected sanitized active job: %#v", status["active_job"])
	}
	invalidStatus := releaseRouteRaw(t, router, service.AccessToken(), "release.v2.status", map[string]any{"release": "github"})
	if invalidStatus.Code != http.StatusBadRequest || releaseResponseCode(t, invalidStatus) != "release_v2_status_invalid_params" {
		t.Fatalf("release.v2.status must reject parameters: %d %s", invalidStatus.Code, invalidStatus.Body.String())
	}

	for _, action := range []string{"release.v2.status", "release.v2.apply"} {
		for _, token := range []string{"", service.AgentToken()} {
			response := releaseRouteRaw(t, router, token, action, map[string]any{})
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s token=%q expected 401, got %d body=%s", action, token, response.Code, response.Body.String())
			}
		}
	}
	identity, authorized := service.authorizeProductAction(service.AccessToken(), "release.v2.status")
	if !authorized {
		t.Fatal("expected current owner session")
	}
	frame := service.handleRealtimeWSRequest(context.Background(), realtimeWSTicket{Role: "owner", UserID: service.OwnerMXID(), DeviceID: identity.DeviceID, Generation: identity.Generation}, map[string]any{
		"id": "release-v2-status", "action": "release.v2.status", "params": map[string]any{},
	})
	if frame["ok"] != false || frame["status"] != http.StatusBadRequest || frame["error"] != "action requires http" {
		t.Fatalf("release.v2.status must remain HTTP-only: %#v", frame)
	}
}

func TestReleaseV2ApplyRevalidatesCentralVersionAndCreatesDirectJob(t *testing.T) {
	controller := &recordingReleaseController{
		directStatus: releasecontrol.DirectStatus{
			Available: true, UpdaterReady: true, CurrentVersion: "v1.1.1", DesiredState: "running",
			Watchdog: releasecontrol.WatchdogStatus{Status: "healthy"},
		},
		directTicket: releasecontrol.JobTicket{JobID: "job_direct", JobToken: "job-secret", StatusURL: "/_dirextalk/updater/v1/jobs/job_direct", Status: "queued"},
	}
	central := &recordingCentralVersionSource{version: releasecontrol.CentralServerVersion{
		AppID: "1", ChannelID: "server", Version: "v1.1.2", PreVersion: "v1.0.2",
	}}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralVersionSource: central})
	router := newP2PTestRouter(service)
	releaseRoute(t, router, service.AccessToken(), "client.version.report", map[string]any{"client_version": "v1.0.2"})
	apply := releaseRoute(t, router, service.AccessToken(), "release.v2.apply", map[string]any{
		"target_version": "v1.1.2", "idempotency_key": "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", "confirm": releasecontrol.ApplyConfirmation,
	})
	if apply["job_id"] != "job_direct" || apply["job_token"] != "job-secret" || apply["status"] != "queued" {
		t.Fatalf("unexpected direct apply response: %#v", apply)
	}
	if central.calls != 1 {
		t.Fatalf("central version source calls = %d, want 1", central.calls)
	}
	if controller.directApplyRequest != (releasecontrol.DirectApplyRequest{
		TargetVersion: "v1.1.2", IdempotencyKey: "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", Confirm: releasecontrol.ApplyConfirmation,
	}) {
		t.Fatalf("unexpected direct updater request: %#v", controller.directApplyRequest)
	}
	if controller.applyRequest.PlanToken != "" {
		t.Fatalf("v2 apply must not use the legacy plan controller: %#v", controller.applyRequest)
	}
}

func TestReleaseV2ApplyRejectsUnsafeParamsAndCompatibilityFailures(t *testing.T) {
	validCentral := releasecontrol.CentralServerVersion{AppID: "1", ChannelID: "server", Version: "v1.1.2", PreVersion: "v1.0.2"}
	baseParams := map[string]any{
		"target_version": "v1.1.2", "idempotency_key": "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", "confirm": releasecontrol.ApplyConfirmation,
	}
	newServiceAndRouter := func(central *recordingCentralVersionSource) (*Service, http.Handler, *recordingReleaseController) {
		controller := &recordingReleaseController{directStatus: releasecontrol.DirectStatus{
			Available: true, UpdaterReady: true, CurrentVersion: "v1.1.1", DesiredState: "running",
		}}
		service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralVersionSource: central})
		mustReportClientVersion(t, service, map[string]any{"client_version": "v1.0.2"})
		return service, newP2PTestRouter(service), controller
	}

	for _, key := range []string{"image", "digest", "url", "plan_token", "version", "shell", "compose_path", "service", "unknown"} {
		t.Run("reject_"+key, func(t *testing.T) {
			central := &recordingCentralVersionSource{version: validCentral}
			service, router, controller := newServiceAndRouter(central)
			params := cloneAnyMap(baseParams)
			params[key] = "attacker-controlled"
			response := releaseRouteRaw(t, router, service.AccessToken(), "release.v2.apply", params)
			if response.Code != http.StatusBadRequest || releaseResponseCode(t, response) != "release_v2_apply_invalid_params" {
				t.Fatalf("%s expected v2 invalid params, got %d %s", key, response.Code, response.Body.String())
			}
			if central.calls != 0 || controller.directApplyRequest.TargetVersion != "" {
				t.Fatalf("invalid params must not reach dependencies: central=%d request=%#v", central.calls, controller.directApplyRequest)
			}
		})
	}
	for name, mutate := range map[string]func(map[string]any){
		"noncanonical_target": func(params map[string]any) { params["target_version"] = "1.0.4" },
		"uppercase_uuid":      func(params map[string]any) { params["idempotency_key"] = "31A20813-C5D9-4F6D-B4F0-CDF8CFC75C6E" },
		"wrong_confirm":       func(params map[string]any) { params["confirm"] = "confirm" },
		"nonstring_target":    func(params map[string]any) { params["target_version"] = 104 },
	} {
		t.Run(name, func(t *testing.T) {
			central := &recordingCentralVersionSource{version: validCentral}
			service, router, controller := newServiceAndRouter(central)
			params := cloneAnyMap(baseParams)
			mutate(params)
			response := releaseRouteRaw(t, router, service.AccessToken(), "release.v2.apply", params)
			if response.Code != http.StatusBadRequest || releaseResponseCode(t, response) != "release_v2_apply_invalid_params" {
				t.Fatalf("expected v2 invalid params, got %d %s", response.Code, response.Body.String())
			}
			if central.calls != 0 || controller.directApplyRequest.TargetVersion != "" {
				t.Fatalf("invalid params must not reach dependencies: central=%d request=%#v", central.calls, controller.directApplyRequest)
			}
		})
	}

	for name, testCase := range map[string]struct {
		central    releasecontrol.CentralServerVersion
		client     string
		target     string
		wantStatus int
		wantCode   string
	}{
		"central_target_changed": {central: validCentral, client: "v1.0.2", target: "v1.1.3", wantStatus: http.StatusConflict, wantCode: "release_target_mismatch"},
		"client_too_old":         {central: releasecontrol.CentralServerVersion{AppID: "1", ChannelID: "server", Version: "v1.1.2", PreVersion: "v1.0.3"}, client: "v1.0.2", target: "v1.1.2", wantStatus: http.StatusConflict, wantCode: "client_version_incompatible"},
		"central_invalid":        {central: releasecontrol.CentralServerVersion{AppID: "1", ChannelID: "google", Version: "v1.1.2", PreVersion: "v1.0.2"}, client: "v1.0.2", target: "v1.1.2", wantStatus: http.StatusBadGateway, wantCode: "central_version_invalid"},
		"target_not_newer":       {central: releasecontrol.CentralServerVersion{AppID: "1", ChannelID: "server", Version: "v1.1.1", PreVersion: "v1.0.2"}, client: "v1.0.2", target: "v1.1.1", wantStatus: http.StatusConflict, wantCode: "release_target_not_newer"},
	} {
		t.Run(name, func(t *testing.T) {
			central := &recordingCentralVersionSource{version: testCase.central}
			controller := &recordingReleaseController{directStatus: releasecontrol.DirectStatus{
				Available: true, UpdaterReady: true, CurrentVersion: "v1.1.1", DesiredState: "running",
			}}
			service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralVersionSource: central})
			mustReportClientVersion(t, service, map[string]any{"client_version": testCase.client})
			params := cloneAnyMap(baseParams)
			params["target_version"] = testCase.target
			response := releaseRouteRaw(t, newP2PTestRouter(service), service.AccessToken(), "release.v2.apply", params)
			if response.Code != testCase.wantStatus || releaseResponseCode(t, response) != testCase.wantCode {
				t.Fatalf("expected %d/%s, got %d %s", testCase.wantStatus, testCase.wantCode, response.Code, response.Body.String())
			}
			if controller.directApplyRequest.TargetVersion != "" {
				t.Fatalf("rejected update reached direct updater: %#v", controller.directApplyRequest)
			}
		})
	}
}

func TestReleaseV2ApplyFailsClosedWhenUpdaterIsNotReady(t *testing.T) {
	central := &recordingCentralVersionSource{version: releasecontrol.CentralServerVersion{
		AppID: "1", ChannelID: "server", Version: "v1.1.2", PreVersion: "v1.0.2",
	}}
	controller := &recordingReleaseController{directStatus: releasecontrol.DirectStatus{
		Available: true, UpdaterReady: false, CurrentVersion: "v1.1.1", DesiredState: "upgrading",
	}}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralVersionSource: central})
	mustReportClientVersion(t, service, map[string]any{"client_version": "v1.0.2"})
	response := releaseRouteRaw(t, newP2PTestRouter(service), service.AccessToken(), "release.v2.apply", map[string]any{
		"target_version": "v1.1.2", "idempotency_key": "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", "confirm": releasecontrol.ApplyConfirmation,
	})
	if response.Code != http.StatusServiceUnavailable || releaseResponseCode(t, response) != updaterUnavailableCode {
		t.Fatalf("expected updater unavailable, got %d %s", response.Code, response.Body.String())
	}
	if central.calls != 0 || controller.directApplyRequest.TargetVersion != "" {
		t.Fatalf("unready updater must block before central/updater apply: central=%d request=%#v", central.calls, controller.directApplyRequest)
	}
}

func releaseResponseCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	code, _ := body["code"].(string)
	return code
}

func TestClientVersionReportFollowsCurrentPortalDevice(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)

	mustReportClientVersion(t, service, map[string]any{"client_version": "v1.2.3"})
	mustHandle[map[string]any](t, service, "portal.auth", map[string]any{"password": service.password, "device_id": "NEW_DEVICE"})
	if service.clientBuild.Version != "" {
		t.Fatalf("new portal device must clear previous device report: %#v", service.clientBuild)
	}

	mustReportClientVersion(t, service, map[string]any{"client_version": "v1.2.4"})
	mustHandle[map[string]any](t, service, "portal.auth", map[string]any{"password": service.password, "device_id": "NEW_DEVICE"})
	if service.clientBuild.Version != "v1.2.4" {
		t.Fatalf("same portal device must retain its report: %#v", service.clientBuild)
	}
}

func TestClientVersionReportRejectsHTTPAuthorizationCapturedBeforeDeviceSwitch(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.SetMatrixSessionIssuer(&recordingMatrixSessionIssuer{})
	oldToken := service.AccessToken()
	identity, authorized := service.authorizeProductAction(oldToken, "client.version.report")
	if !authorized {
		t.Fatal("expected current owner token to authorize")
	}

	switched := make(chan *apiError, 1)
	go func() {
		_, apiErr := service.Handle(context.Background(), "portal.auth", map[string]any{
			"password": service.password, "device_id": "NEW_DEVICE",
		})
		switched <- apiErr
	}()
	if apiErr := <-switched; apiErr != nil {
		t.Fatalf("switch portal device: %#v", apiErr)
	}

	_, apiErr := service.Handle(withPortalActionSession(context.Background(), identity), "client.version.report", map[string]any{
		"client_version": "v9.9.9",
	})
	if apiErr == nil || apiErr.Status != http.StatusUnauthorized || apiErr.Code != "client_session_stale" {
		t.Fatalf("expected stale authorized HTTP request to be rejected, got %#v", apiErr)
	}
	if service.clientBuild.Version != "" {
		t.Fatalf("stale HTTP request wrote the new device build: %#v", service.clientBuild)
	}
}

func TestClientVersionReportRejectsConnectedOwnerWSAfterDeviceSwitch(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.SetMatrixSessionIssuer(&recordingMatrixSessionIssuer{})
	ticketResult, apiErr := service.createRealtimeWSTicketForToken(service.AccessToken())
	if apiErr != nil {
		t.Fatalf("create WS ticket: %#v", apiErr)
	}
	record, consumeErr := service.consumeRealtimeWSTicketRecord(trimString(ticketResult["ticket"]))
	if consumeErr != nil {
		t.Fatalf("consume WS ticket: %v", consumeErr)
	}
	if _, apiErr := service.Handle(context.Background(), "portal.auth", map[string]any{
		"password": service.password, "device_id": "NEW_DEVICE",
	}); apiErr != nil {
		t.Fatalf("switch portal device: %#v", apiErr)
	}

	frame := service.handleRealtimeWSRequest(context.Background(), record, map[string]any{
		"id": "stale-ws", "action": "client.version.report", "params": map[string]any{"client_version": "v9.9.9"},
	})
	if frame["ok"] != false || frame["status"] != http.StatusUnauthorized || frame["code"] != "client_session_stale" {
		t.Fatalf("expected stale connected WS to be rejected, got %#v", frame)
	}
	if service.clientBuild.Version != "" {
		t.Fatalf("stale WS wrote the new device build: %#v", service.clientBuild)
	}
}

func TestClientVersionReportUsesNarrowDeviceCASWithoutLosingConcurrentPortalFields(t *testing.T) {
	service := NewService(withTestExternalAgent(Config{ServerName: "example.com"}))
	service.mu.Lock()
	initial := service.portalStateLocked()
	service.mu.Unlock()
	store := &blockingClientBuildStore{
		Store:          service.store,
		state:          initial,
		narrowEntered:  make(chan struct{}),
		releaseNarrow:  make(chan struct{}),
		fullReportSave: make(chan struct{}, 1),
	}
	service.store = store
	identity, authorized := service.authorizeProductAction(service.AccessToken(), "client.version.report")
	if !authorized {
		t.Fatal("expected owner action authorization")
	}
	reportDone := make(chan *apiError, 1)
	go func() {
		_, apiErr := service.Handle(withPortalActionSession(context.Background(), identity), "client.version.report", map[string]any{
			"client_version": "v2.3.4", "build_number": "42", "platform": "android",
		})
		reportDone <- apiErr
	}()

	select {
	case <-store.fullReportSave:
		t.Fatal("client version report used stale full-row SavePortal")
	case <-store.narrowEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("client version report did not reach narrow persistence")
	}
	if _, apiErr := service.Handle(context.Background(), "profile.update", map[string]any{"display_name": "Concurrent Profile"}); apiErr != nil {
		t.Fatalf("concurrent profile update: %#v", apiErr)
	}
	if _, apiErr := service.Handle(context.Background(), "agent.config.update", map[string]any{"system_prompt": "Concurrent Agent Config"}); apiErr != nil {
		t.Fatalf("concurrent agent update: %#v", apiErr)
	}
	close(store.releaseNarrow)
	if apiErr := <-reportDone; apiErr != nil {
		t.Fatalf("report client version: %#v", apiErr)
	}

	store.mu.Lock()
	durable := store.state
	store.mu.Unlock()
	if durable.Profile.DisplayName != "Concurrent Profile" {
		t.Fatalf("narrow report lost concurrent portal fields: %#v", durable)
	}
	if durable.AgentConfig.SystemPrompt != "" {
		t.Fatalf("Native Agent config must remain external, got local projection: %#v", durable.AgentConfig)
	}
	if durable.ClientBuild.Version != "v2.3.4" || durable.ClientBuild.BuildNumber != "42" {
		t.Fatalf("narrow report did not persist client build: %#v", durable.ClientBuild)
	}
}

func TestClientVersionReportSerializesSameDevicePasswordRotation(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		t.Run(transport, func(t *testing.T) {
			service := NewService(Config{ServerName: "example.com"})
			service.mu.Lock()
			initial := service.portalStateLocked()
			service.mu.Unlock()
			store := &blockingClientBuildStore{
				Store:          service.store,
				state:          initial,
				narrowEntered:  make(chan struct{}),
				releaseNarrow:  make(chan struct{}),
				fullReportSave: make(chan struct{}, 1),
				portalSaved:    make(chan portalState, 1),
			}
			service.store = store
			identity, authorized := service.authorizeProductAction(service.AccessToken(), "client.version.report")
			if !authorized {
				t.Fatal("expected current owner session")
			}
			record := realtimeWSTicket{Role: "owner", UserID: service.OwnerMXID(), DeviceID: identity.DeviceID, Generation: identity.Generation}
			reportDone := make(chan *apiError, 1)
			go func() {
				if transport == "ws" {
					frame := service.handleRealtimeWSRequest(context.Background(), record, map[string]any{
						"id": "same-device-race", "action": "client.version.report", "params": map[string]any{"client_version": "v2.3.4"},
					})
					if frame["ok"] != true {
						status, _ := frame["status"].(int)
						reportDone <- codedError(status, trimString(frame["code"]), trimString(frame["error"]))
						return
					}
					reportDone <- nil
					return
				}
				_, apiErr := service.Handle(withPortalActionSession(context.Background(), identity), "client.version.report", map[string]any{"client_version": "v2.3.4"})
				reportDone <- apiErr
			}()
			<-store.narrowEntered

			passwordDone := make(chan *apiError, 1)
			go func() {
				_, apiErr := service.Handle(context.Background(), "portal.password", map[string]any{
					"old_password": service.password,
					"new_password": "rotated-password",
					"device_id":    identity.DeviceID,
				})
				passwordDone <- apiErr
			}()
			passwordPersistedBeforeReport := false
			select {
			case <-store.portalSaved:
				passwordPersistedBeforeReport = true
			case <-time.After(300 * time.Millisecond):
			}
			close(store.releaseNarrow)
			if apiErr := <-reportDone; apiErr != nil {
				t.Fatalf("report client version: %#v", apiErr)
			}
			if apiErr := <-passwordDone; apiErr != nil {
				t.Fatalf("rotate portal password: %#v", apiErr)
			}
			if passwordPersistedBeforeReport {
				t.Fatal("same-device password token/generation mutation overtook an already-validated client report")
			}

			if transport == "ws" {
				frame := service.handleRealtimeWSRequest(context.Background(), record, map[string]any{
					"id": "stale-after-password", "action": "client.version.report", "params": map[string]any{"client_version": "v9.9.9"},
				})
				if frame["code"] != clientSessionStaleCode {
					t.Fatalf("old WS remained valid after same-device password rotation: %#v", frame)
				}
				return
			}
			_, apiErr := service.Handle(withPortalActionSession(context.Background(), identity), "client.version.report", map[string]any{"client_version": "v9.9.9"})
			if apiErr == nil || apiErr.Code != clientSessionStaleCode {
				t.Fatalf("old HTTP session remained valid after same-device password rotation: %#v", apiErr)
			}
		})
	}
}

func TestReleaseApplyRejectsUnknownOrInfrastructureParamsWithStructuredCode(t *testing.T) {
	controller := &recordingReleaseController{}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller})
	router := newP2PTestRouter(service)
	base := map[string]any{
		"plan_token": "opaque-plan", "idempotency_key": "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", "confirm": releasecontrol.ApplyConfirmation,
	}
	for _, key := range []string{"image", "digest", "version", "shell", "compose_path", "service", "unknown"} {
		params := cloneAnyMap(base)
		params[key] = "attacker-controlled"
		response := releaseRouteRaw(t, router, service.AccessToken(), "release.v1.apply", params)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s expected 400, got %d body=%s", key, response.Code, response.Body.String())
		}
		var body map[string]any
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		if body["code"] != "release_apply_invalid_params" {
			t.Fatalf("%s missing structured error code: %#v", key, body)
		}
	}
	if controller.applyRequest.PlanToken != "" {
		t.Fatalf("invalid request reached controller: %#v", controller.applyRequest)
	}
}

func releaseRoute(t *testing.T, router http.Handler, token, action string, params map[string]any) map[string]any {
	t.Helper()
	response := releaseRouteRaw(t, router, token, action, params)
	if response.Code != http.StatusOK {
		t.Fatalf("%s expected 200, got %d body=%s", action, response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func releaseRouteRaw(t *testing.T, router http.Handler, token, action string, params map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := jsonRequest(t, "/_p2p/command", map[string]any{"action": action, "params": params})
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func mustReportClientVersion(t *testing.T, service *Service, params map[string]any) map[string]any {
	t.Helper()
	identity, authorized := service.authorizeProductAction(service.AccessToken(), "client.version.report")
	if !authorized {
		t.Fatal("expected current owner session")
	}
	result, apiErr := service.Handle(withPortalActionSession(context.Background(), identity), "client.version.report", params)
	if apiErr != nil {
		t.Fatalf("client.version.report failed: %#v", apiErr)
	}
	return result.(map[string]any)
}
