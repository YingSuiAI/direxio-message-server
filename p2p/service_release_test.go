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
	applyRequest  releasecontrol.ApplyRequest
	desiredStates []releasecontrol.DesiredState
	desiredErrors map[releasecontrol.DesiredState]error
	status        releasecontrol.Status
	ticket        releasecontrol.JobTicket
	statusErr     error
	applyErr      error
	desiredErr    error
}

type recordingCentralVersionSource struct {
	version releasecontrol.CentralServerVersion
	err     error
	calls   int
}

type recordingCentralAgentVersionSource struct {
	version releasecontrol.CentralAgentVersion
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

func (c *recordingReleaseController) Status(context.Context) (releasecontrol.Status, error) {
	return c.status, c.statusErr
}

func (c *recordingReleaseController) Apply(_ context.Context, request releasecontrol.ApplyRequest) (releasecontrol.JobTicket, error) {
	c.applyRequest = request
	return c.ticket, c.applyErr
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

func (s *recordingCentralAgentVersionSource) CurrentAgentVersion(context.Context) (releasecontrol.CentralAgentVersion, error) {
	s.calls++
	return s.version, s.err
}

func validCentralAgentSource() *recordingCentralAgentVersionSource {
	return &recordingCentralAgentVersionSource{version: releasecontrol.CentralAgentVersion{
		AppID: "1", ChannelID: "agents", Version: "v1.1.2", PreVersion: "v1.1.0",
	}}
}

func readyReleaseController() *recordingReleaseController {
	return &recordingReleaseController{
		status: releasecontrol.Status{
			Available: true, UpdaterReady: true, CurrentVersion: "v1.1.4", DesiredState: "running",
			Agent:    releasecontrol.AgentStatus{Available: true, CurrentVersion: "v1.0.0"},
			Watchdog: releasecontrol.WatchdogStatus{Status: "healthy"},
		},
		ticket: releasecontrol.JobTicket{JobID: "job_direct", JobToken: "job-secret", StatusURL: "/_dirextalk/updater/v1/jobs/job_direct", Status: "queued"},
	}
}

func TestReleaseV2StatusIsOwnerHTTPOnlyAndCombinesReceiptWithCentralAgent(t *testing.T) {
	controller := readyReleaseController()
	controller.status.UpdaterReady = false
	controller.status.DesiredState = "upgrading"
	controller.status.ActiveJob = &releasecontrol.ActiveJob{
		JobID: "job_active", Component: "agent", Status: "pulling", CurrentVersion: "v1.0.0", TargetVersion: "v1.1.2", ServiceAvailable: true,
	}
	central := validCentralAgentSource()
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralAgentVersionSource: central})
	router := newP2PTestRouter(service)
	releaseRoute(t, router, service.AccessToken(), "client.version.report", map[string]any{"client_version": "v1.0.2"})

	status := releaseRoute(t, router, service.AccessToken(), "release.v2.status", nil)
	if status["current_version"] != "v1.1.4" || status["client_version"] != "v1.0.2" || status["available"] != false || status["updater_available"] != true || status["updater_ready"] != false || status["desired_state"] != "upgrading" {
		t.Fatalf("unexpected status: %#v", status)
	}
	agent := status["agent"].(map[string]any)
	if agent["available"] != true || agent["current_version"] != "v1.0.0" || agent["latest_version"] != "v1.1.2" || agent["minimum_server_version"] != "v1.1.0" || agent["compatibility"] != "compatible" || agent["update_available"] != true {
		t.Fatalf("unexpected Agent status: %#v", agent)
	}
	job := status["active_job"].(map[string]any)
	if job["component"] != "agent" || job["job_id"] != "job_active" || job["target_version"] != "v1.1.2" {
		t.Fatalf("unexpected active job: %#v", job)
	}
	if central.calls != 1 {
		t.Fatalf("central Agent calls = %d", central.calls)
	}
	invalid := releaseRouteRaw(t, router, service.AccessToken(), "release.v2.status", map[string]any{"component": "agent"})
	if invalid.Code != http.StatusBadRequest || releaseResponseCode(t, invalid) != "release_v2_status_invalid_params" {
		t.Fatalf("status parameters not rejected: %d %s", invalid.Code, invalid.Body.String())
	}
	for _, token := range []string{"", service.AgentToken()} {
		if response := releaseRouteRaw(t, router, token, "release.v2.status", map[string]any{}); response.Code != http.StatusUnauthorized {
			t.Fatalf("status token=%q got %d", token, response.Code)
		}
	}
	identity, authorized := service.authorizeProductAction(service.AccessToken(), "release.v2.status")
	if !authorized {
		t.Fatal("expected owner session")
	}
	frame := service.handleRealtimeWSRequest(context.Background(), realtimeWSTicket{Role: "owner", UserID: service.OwnerMXID(), DeviceID: identity.DeviceID, Generation: identity.Generation}, map[string]any{
		"id": "release-v2-status", "action": "release.v2.status", "params": map[string]any{},
	})
	if frame["ok"] != false || frame["error"] != "action requires http" {
		t.Fatalf("release.v2.status must remain HTTP-only: %#v", frame)
	}
}

func TestReleaseV2StatusKeepsUpdaterAndCentralFailuresIndependent(t *testing.T) {
	t.Run("central unavailable keeps receipt-bound current Agent", func(t *testing.T) {
		controller := readyReleaseController()
		central := &recordingCentralAgentVersionSource{err: &releasecontrol.CentralVersionError{Code: releasecontrol.CentralVersionUnavailableCode}}
		service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralAgentVersionSource: central})
		status := mustHandle[map[string]any](t, service, "release.v2.status", nil)
		agent := status["agent"].(map[string]any)
		if agent["available"] != true || agent["current_version"] != "v1.0.0" || agent["latest_version"] != "" || agent["update_available"] != false {
			t.Fatalf("central failure erased runtime fact: %#v", agent)
		}
		reasons := agent["reasons"].([]string)
		if len(reasons) != 1 || reasons[0] != releasecontrol.CentralVersionUnavailableCode {
			t.Fatalf("unexpected reasons: %#v", reasons)
		}
	})
	t.Run("updater unavailable keeps validated central target", func(t *testing.T) {
		controller := readyReleaseController()
		controller.statusErr = errors.New("socket unavailable: secret")
		service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralAgentVersionSource: validCentralAgentSource()})
		status := mustHandle[map[string]any](t, service, "release.v2.status", nil)
		agent := status["agent"].(map[string]any)
		if agent["available"] != false || agent["latest_version"] != "v1.1.2" || agent["minimum_server_version"] != "v1.1.0" {
			t.Fatalf("updater failure erased central fact: %#v", agent)
		}
	})
}

func TestReleaseV2StatusFailureIsParseableAndRedacted(t *testing.T) {
	controller := &recordingReleaseController{statusErr: errors.New("socket unavailable: secret-token")}
	service := NewService(Config{
		ServerName: "example.com", ReleaseController: controller,
		CentralAgentVersionSource: validCentralAgentSource(),
	})
	status := mustHandle[map[string]any](t, service, "release.v2.status", nil)
	if status["available"] != false || status["updater_available"] != false || status["updater_ready"] != false || status["desired_state"] != "unknown" || status["active_job"] != nil {
		t.Fatalf("unexpected unavailable status: %#v", status)
	}
	agent := status["agent"].(map[string]any)
	if agent["available"] != false || agent["current_version"] != "" || agent["latest_version"] != "v1.1.2" || agent["minimum_server_version"] != "v1.1.0" || agent["update_available"] != false {
		t.Fatalf("updater failure erased or invented Agent facts: %#v", agent)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-token") {
		t.Fatalf("updater error leaked into status: %s", raw)
	}
}

func TestReleaseV2StatusRejectsNoncanonicalAgentCurrentVersion(t *testing.T) {
	controller := readyReleaseController()
	controller.status.Agent.CurrentVersion = "1.0.0"
	service := NewService(Config{
		ServerName: "example.com", ReleaseController: controller,
		CentralAgentVersionSource: validCentralAgentSource(),
	})
	status := mustHandle[map[string]any](t, service, "release.v2.status", nil)
	agent := status["agent"].(map[string]any)
	if agent["available"] != false || agent["current_version"] != "" || agent["latest_version"] != "v1.1.2" || agent["minimum_server_version"] != "v1.1.0" || agent["update_available"] != false {
		t.Fatalf("noncanonical Agent version was exposed: %#v", agent)
	}
	reasons := agent["reasons"].([]string)
	if len(reasons) != 1 || reasons[0] != "agent_release_invalid" {
		t.Fatalf("unexpected Agent reasons: %#v", reasons)
	}
}

func TestReleaseV2StatusKeepsLocalVersionsAuthoritative(t *testing.T) {
	controller := readyReleaseController()
	controller.status.CurrentVersion = "v999.0.0"
	service := NewService(Config{
		ServerName: "example.com", ReleaseController: controller,
		CentralAgentVersionSource: validCentralAgentSource(),
	})
	mustReportClientVersion(t, service, map[string]any{"client_version": "v2.3.4"})
	status := mustHandle[map[string]any](t, service, "release.v2.status", nil)
	if status["current_version"] != "v1.1.4" || status["client_version"] != "v2.3.4" || status["available"] != false || status["updater_available"] != false || status["updater_ready"] != false {
		t.Fatalf("updater receipt replaced local versions or remained trusted: %#v", status)
	}
	agent := status["agent"].(map[string]any)
	if agent["available"] != false || agent["latest_version"] != "v1.1.2" || agent["minimum_server_version"] != "v1.1.0" {
		t.Fatalf("invalid updater receipt erased central Agent facts: %#v", agent)
	}
}

func TestReleaseV2ApplyServerRevalidatesClientAndSendsFiveFieldCommand(t *testing.T) {
	controller := readyReleaseController()
	central := &recordingCentralVersionSource{version: releasecontrol.CentralServerVersion{AppID: "1", ChannelID: "server", Version: "v1.1.5", PreVersion: "v1.0.2"}}
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralVersionSource: central})
	router := newP2PTestRouter(service)
	releaseRoute(t, router, service.AccessToken(), "client.version.report", map[string]any{"client_version": "v1.0.2"})
	response := releaseRoute(t, router, service.AccessToken(), "release.v2.apply", map[string]any{
		"component": "server", "target_version": "v1.1.5", "idempotency_key": "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", "confirm": releasecontrol.ApplyConfirmation,
	})
	if response["job_id"] != "job_direct" || response["job_token"] != "job-secret" || response["status"] != "queued" {
		t.Fatalf("unexpected apply response: %#v", response)
	}
	want := releasecontrol.ApplyRequest{Component: releasecontrol.ReleaseComponentServer, TargetVersion: "v1.1.5", MinimumServerVersion: "", IdempotencyKey: "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", Confirm: releasecontrol.ApplyConfirmation}
	if controller.applyRequest != want || central.calls != 1 {
		t.Fatalf("unexpected updater command=%#v central calls=%d", controller.applyRequest, central.calls)
	}
	service.mu.Lock()
	persisted := service.portalStateLocked()
	service.mu.Unlock()
	raw, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "job-secret") || strings.Contains(string(raw), want.IdempotencyKey) {
		t.Fatalf("release credentials entered durable portal state: %s", raw)
	}
}

func TestReleaseV2ApplyIsOwnerHTTPOnly(t *testing.T) {
	controller := readyReleaseController()
	service := NewService(Config{
		ServerName: "example.com", ReleaseController: controller,
		CentralAgentVersionSource: validCentralAgentSource(),
	})
	router := newP2PTestRouter(service)
	params := map[string]any{
		"component": "agent", "target_version": "v1.1.2",
		"idempotency_key": "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e",
		"confirm":         releasecontrol.ApplyConfirmation,
	}
	for _, token := range []string{"", service.AgentToken()} {
		response := releaseRouteRaw(t, router, token, "release.v2.apply", params)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("apply token=%q got %d body=%s", token, response.Code, response.Body.String())
		}
	}
	identity, authorized := service.authorizeProductAction(service.AccessToken(), "release.v2.apply")
	if !authorized {
		t.Fatal("expected owner session")
	}
	frame := service.handleRealtimeWSRequest(context.Background(), realtimeWSTicket{
		Role: "owner", UserID: service.OwnerMXID(), DeviceID: identity.DeviceID, Generation: identity.Generation,
	}, map[string]any{"id": "release-v2-apply", "action": "release.v2.apply", "params": params})
	if frame["ok"] != false || frame["error"] != "action requires http" || controller.applyRequest.Component != "" {
		t.Fatalf("release.v2.apply must remain owner HTTP-only: %#v", frame)
	}
}

func TestReleaseV2ApplyAgentUsesServerCompatibilityAndReceiptCurrent(t *testing.T) {
	controller := readyReleaseController()
	central := validCentralAgentSource()
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralAgentVersionSource: central})
	router := newP2PTestRouter(service)
	response := releaseRoute(t, router, service.AccessToken(), "release.v2.apply", map[string]any{
		"component": "agent", "target_version": "v1.1.2", "idempotency_key": "41a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", "confirm": releasecontrol.ApplyConfirmation,
	})
	if response["job_id"] != "job_direct" {
		t.Fatalf("unexpected apply response: %#v", response)
	}
	want := releasecontrol.ApplyRequest{Component: releasecontrol.ReleaseComponentAgent, TargetVersion: "v1.1.2", MinimumServerVersion: "v1.1.0", IdempotencyKey: "41a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", Confirm: releasecontrol.ApplyConfirmation}
	if controller.applyRequest != want || central.calls != 1 {
		t.Fatalf("unexpected updater command=%#v central calls=%d", controller.applyRequest, central.calls)
	}

	controller.applyRequest = releasecontrol.ApplyRequest{}
	controller.status.Agent.CurrentVersion = "v1.1.2"
	responseRaw := releaseRouteRaw(t, router, service.AccessToken(), "release.v2.apply", map[string]any{
		"component": "agent", "target_version": "v1.1.2", "idempotency_key": "51a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", "confirm": releasecontrol.ApplyConfirmation,
	})
	if responseRaw.Code != http.StatusConflict || releaseResponseCode(t, responseRaw) != "release_target_not_newer" || controller.applyRequest.Component != "" {
		t.Fatalf("non-new Agent target was not fenced: %d %s command=%#v", responseRaw.Code, responseRaw.Body.String(), controller.applyRequest)
	}
}

func TestReleaseV2ApplyRejectsUnsafeAndIncompatibleRequests(t *testing.T) {
	validID := "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e"
	for _, params := range []map[string]any{
		{"component": "other", "target_version": "v1.1.2", "idempotency_key": validID, "confirm": releasecontrol.ApplyConfirmation},
		{"component": "agent", "target_version": "dev1.1.2", "idempotency_key": validID, "confirm": releasecontrol.ApplyConfirmation},
		{"component": "server", "target_version": "v1.1.2", "idempotency_key": validID, "confirm": releasecontrol.ApplyConfirmation, "image": "unsafe"},
	} {
		controller := readyReleaseController()
		service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralVersionSource: &recordingCentralVersionSource{}, CentralAgentVersionSource: validCentralAgentSource()})
		response := releaseRouteRaw(t, newP2PTestRouter(service), service.AccessToken(), "release.v2.apply", params)
		if response.Code != http.StatusBadRequest || releaseResponseCode(t, response) != "release_v2_apply_invalid_params" || controller.applyRequest.Component != "" {
			t.Fatalf("unsafe params accepted: %d %s", response.Code, response.Body.String())
		}
	}
	for name, testCase := range map[string]struct {
		central releasecontrol.CentralServerVersion
		target  string
		code    string
	}{
		"central target changed": {
			central: releasecontrol.CentralServerVersion{AppID: "1", ChannelID: "server", Version: "v1.1.5", PreVersion: "v1.0.2"},
			target:  "v1.1.6", code: "release_target_mismatch",
		},
		"client too old": {
			central: releasecontrol.CentralServerVersion{AppID: "1", ChannelID: "server", Version: "v1.1.5", PreVersion: "v1.0.3"},
			target:  "v1.1.5", code: "client_version_incompatible",
		},
		"central invalid": {
			central: releasecontrol.CentralServerVersion{AppID: "1", ChannelID: "other", Version: "v1.1.5", PreVersion: "v1.0.2"},
			target:  "v1.1.5", code: releasecontrol.CentralVersionInvalidCode,
		},
		"target not newer": {
			central: releasecontrol.CentralServerVersion{AppID: "1", ChannelID: "server", Version: "v1.1.4", PreVersion: "v1.0.2"},
			target:  "v1.1.4", code: "release_target_not_newer",
		},
	} {
		t.Run(name, func(t *testing.T) {
			controller := readyReleaseController()
			central := &recordingCentralVersionSource{version: testCase.central}
			service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralVersionSource: central})
			mustReportClientVersion(t, service, map[string]any{"client_version": "v1.0.2"})
			response := releaseRouteRaw(t, newP2PTestRouter(service), service.AccessToken(), "release.v2.apply", map[string]any{
				"component": "server", "target_version": testCase.target, "idempotency_key": validID, "confirm": releasecontrol.ApplyConfirmation,
			})
			if response.Code < http.StatusBadRequest || releaseResponseCode(t, response) != testCase.code || controller.applyRequest.Component != "" {
				t.Fatalf("server Gate did not reject: %d %s command=%#v", response.Code, response.Body.String(), controller.applyRequest)
			}
		})
	}

	controller := readyReleaseController()
	central := validCentralAgentSource()
	central.version.PreVersion = "v1.1.5"
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralAgentVersionSource: central})
	response := releaseRouteRaw(t, newP2PTestRouter(service), service.AccessToken(), "release.v2.apply", map[string]any{
		"component": "agent", "target_version": "v1.1.2", "idempotency_key": validID, "confirm": releasecontrol.ApplyConfirmation,
	})
	if response.Code != http.StatusConflict || releaseResponseCode(t, response) != "server_version_incompatible" || controller.applyRequest.Component != "" {
		t.Fatalf("Agent server minimum was not fenced: %d %s", response.Code, response.Body.String())
	}
}

func TestReleaseV2ApplyFailsClosedWhenUpdaterIsNotReady(t *testing.T) {
	controller := readyReleaseController()
	controller.status.UpdaterReady = false
	central := validCentralAgentSource()
	service := NewService(Config{ServerName: "example.com", ReleaseController: controller, CentralAgentVersionSource: central})
	response := releaseRouteRaw(t, newP2PTestRouter(service), service.AccessToken(), "release.v2.apply", map[string]any{
		"component": "agent", "target_version": "v1.1.2", "idempotency_key": "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", "confirm": releasecontrol.ApplyConfirmation,
	})
	if response.Code != http.StatusServiceUnavailable || releaseResponseCode(t, response) != updaterUnavailableCode || central.calls != 0 || controller.applyRequest.Component != "" {
		t.Fatalf("unready updater did not fail before central/apply: %d %s", response.Code, response.Body.String())
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
