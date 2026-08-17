package p2p

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingMatrixSessionIssuer struct {
	deviceID              string
	revokeExistingDevices bool
	sessionUser           string
	sessionName           string
	sessionURL            string
	profileUser           string
	profileName           string
	profileURL            string
	calls                 int
}

func (i *recordingMatrixSessionIssuer) EnsureMatrixSession(ctx context.Context, userID, displayName, avatarURL, deviceID string, revokeExistingDevices bool) (string, error) {
	i.calls++
	i.deviceID = deviceID
	i.revokeExistingDevices = revokeExistingDevices
	i.sessionUser = userID
	i.sessionName = displayName
	i.sessionURL = avatarURL
	return "matrix-token-for-" + deviceID, nil
}

func (i *recordingMatrixSessionIssuer) UpdateMatrixProfile(ctx context.Context, userID, displayName, avatarURL string) error {
	i.profileUser = userID
	i.profileName = displayName
	i.profileURL = avatarURL
	return nil
}

type concurrentGuardMatrixSessionIssuer struct {
	active    int32
	maxActive int32
	calls     int32
}

func (i *concurrentGuardMatrixSessionIssuer) EnsureMatrixSession(ctx context.Context, userID, displayName, avatarURL, deviceID string, revokeExistingDevices bool) (string, error) {
	active := atomic.AddInt32(&i.active, 1)
	for {
		maxActive := atomic.LoadInt32(&i.maxActive)
		if active <= maxActive || atomic.CompareAndSwapInt32(&i.maxActive, maxActive, active) {
			break
		}
	}
	defer atomic.AddInt32(&i.active, -1)
	call := atomic.AddInt32(&i.calls, 1)
	time.Sleep(20 * time.Millisecond)
	return "matrix-token-" + deviceID + "-" + strconv.FormatInt(int64(call), 10), nil
}

func (i *concurrentGuardMatrixSessionIssuer) UpdateMatrixProfile(ctx context.Context, userID, displayName, avatarURL string) error {
	return nil
}

func requireEightDigitPassword(t *testing.T, password string) {
	t.Helper()
	if len(password) != 8 {
		t.Fatalf("expected 8 digit password, got %q", password)
	}
	for _, char := range password {
		if char < '0' || char > '9' {
			t.Fatalf("expected 8 digit password, got %q", password)
		}
	}
}

func TestNewServiceInitializesOwnerProfileWithFullMXID(t *testing.T) {
	service := NewService(Config{ServerName: " example.com "})

	if service.ownerMXID != "@owner:example.com" {
		t.Fatalf("expected owner MXID @owner:example.com, got %q", service.ownerMXID)
	}
	if service.profile.UserID != service.ownerMXID {
		t.Fatalf("expected profile user ID to match owner MXID, got %q", service.profile.UserID)
	}
	if service.profile.Domain != "example.com" {
		t.Fatalf("expected profile domain example.com, got %q", service.profile.Domain)
	}
}

func TestPortalAuthUsesRequestedMatrixDeviceID(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)

	session := mustHandle[map[string]any](t, service, "portal.auth", map[string]any{
		"password":  service.password,
		"device_id": "DEVICE_B",
	})

	if issuer.deviceID != "DEVICE_B" {
		t.Fatalf("expected Matrix issuer to receive requested device id, got %q", issuer.deviceID)
	}
	if !issuer.revokeExistingDevices {
		t.Fatalf("expected portal auth to revoke existing Matrix devices")
	}
	if session["device_id"] != "DEVICE_B" || session["access_token"] != "matrix-token-for-DEVICE_B" {
		t.Fatalf("expected session to use requested device id and token, got %#v", session)
	}
	if _, ok := session["agent_token"]; ok {
		t.Fatalf("portal auth must not expose the long-lived Agent token: %#v", session)
	}
}

func TestAgentMatrixSessionCreateUsesAgentIdentity(t *testing.T) {
	service := NewService(withTestExternalAgent(Config{ServerName: "example.com"}))
	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)
	mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"online_agent_identity": map[string]any{
			"display_name": "Dirextalk Agent",
			"avatar_url":   "mxc://online-agent",
		},
	})
	if issuer.profileUser != "@agent:example.com" || issuer.profileName != "Dirextalk Agent" || issuer.profileURL != "mxc://online-agent" {
		t.Fatalf("expected online identity update to sync Matrix profile, got user=%q name=%q avatar=%q", issuer.profileUser, issuer.profileName, issuer.profileURL)
	}

	session := mustHandle[map[string]any](t, service, "agent.matrix_session.create", map[string]any{
		"device_id": "DIREXTALK_CLI",
	})

	if issuer.deviceID != "DIREXTALK_CLI" {
		t.Fatalf("expected Matrix issuer to receive CLI device id, got %q", issuer.deviceID)
	}
	if issuer.revokeExistingDevices {
		t.Fatalf("agent Matrix sessions must not revoke the portal user's devices")
	}
	if issuer.sessionUser != "@agent:example.com" {
		t.Fatalf("expected Matrix issuer to create an agent session, got user %q", issuer.sessionUser)
	}
	if issuer.sessionName != "Dirextalk Agent" {
		t.Fatalf("expected Matrix issuer to use agent display name, got %q", issuer.sessionName)
	}
	if issuer.sessionURL != "mxc://online-agent" {
		t.Fatalf("expected Matrix issuer to use online agent avatar, got %q", issuer.sessionURL)
	}
	if session["device_id"] != "DIREXTALK_CLI" {
		t.Fatalf("expected session device id to be DIREXTALK_CLI, got %#v", session)
	}
	if session["access_token"] != "matrix-token-for-DIREXTALK_CLI" {
		t.Fatalf("expected Matrix access token in internal session response, got %#v", session)
	}
	if session["user_id"] != "@agent:example.com" {
		t.Fatalf("expected local agent user id, got %#v", session)
	}
	if _, ok := session["password"]; ok {
		t.Fatalf("agent Matrix session must not expose portal password: %#v", session)
	}
	if _, ok := session["agent_token"]; ok {
		t.Fatalf("agent Matrix session must not echo agent token: %#v", session)
	}
}

func TestAgentMatrixSessionDoesNotReplacePortalAccessToken(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)

	portalSession := mustHandle[map[string]any](t, service, "portal.auth", map[string]any{
		"password":  service.password,
		"device_id": "PORTAL_DEVICE",
	})
	portalToken := portalSession["access_token"].(string)
	if portalToken == "" {
		t.Fatalf("expected portal auth to return access token")
	}

	agentSession := mustHandle[map[string]any](t, service, "agent.matrix_session.create", map[string]any{
		"device_id": "DIREXTALK_AGENT_TEST",
	})
	if agentSession["access_token"] == portalToken {
		t.Fatalf("expected agent Matrix session to have its own Matrix token")
	}
	if service.AccessToken() != portalToken {
		t.Fatalf("agent Matrix session must not replace portal access token, got %q want %q", service.AccessToken(), portalToken)
	}
	if !service.Authorize(portalToken, "channels.list") {
		t.Fatalf("existing portal token must remain valid after agent Matrix session creation")
	}
}

func TestMatrixHistoryAccessTokenUsesCurrentOwnerTokenWithoutRefreshingSession(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)

	portalSession := mustHandle[map[string]any](t, service, "portal.auth", map[string]any{
		"password":  service.password,
		"device_id": "OWNER_PHONE",
	})
	portalToken := portalSession["access_token"].(string)
	callsAfterPortalAuth := issuer.calls

	token, err := service.MatrixHistoryAccessToken(context.Background())
	if err != nil {
		t.Fatalf("MatrixHistoryAccessToken failed: %v", err)
	}
	if token != portalToken {
		t.Fatalf("expected Matrix history to reuse current owner token, got %q want %q", token, portalToken)
	}
	if issuer.calls != callsAfterPortalAuth {
		t.Fatalf("Matrix history token lookup must not create or refresh Matrix sessions, calls=%d want %d", issuer.calls, callsAfterPortalAuth)
	}
	if issuer.deviceID != "OWNER_PHONE" {
		t.Fatalf("Matrix history token lookup must not request a new device, got %q", issuer.deviceID)
	}
}

func TestPortalAuthSerializesMatrixSessionRefresh(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	issuer := &concurrentGuardMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := service.Handle(context.Background(), "portal.auth", map[string]any{
				"password":  service.password,
				"device_id": "DEVICE_A",
			})
			if err != nil {
				errs <- err.Error
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("portal.auth failed under concurrent refresh: %s", err)
	}

	if got := atomic.LoadInt32(&issuer.maxActive); got > 1 {
		t.Fatalf("expected Matrix session refresh to be serialized, max concurrent calls=%d", got)
	}
	if got := atomic.LoadInt32(&issuer.calls); got != workers {
		t.Fatalf("expected %d session refresh calls, got %d", workers, got)
	}
}

func TestPortalAuthWithoutDeviceIDDoesNotReusePreviousMatrixDeviceID(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)

	first := mustHandle[map[string]any](t, service, "portal.auth", map[string]any{
		"password":  service.password,
		"device_id": "DEVICE_A",
	})
	second := mustHandle[map[string]any](t, service, "portal.auth", map[string]any{
		"password": service.password,
	})

	firstDevice := first["device_id"].(string)
	secondDevice := second["device_id"].(string)
	if firstDevice != "DEVICE_A" {
		t.Fatalf("expected first auth to use requested device id, got %#v", first)
	}
	if secondDevice == "" || secondDevice == firstDevice || secondDevice == matrixPortalDeviceID {
		t.Fatalf("expected missing device_id to mint a fresh Matrix device id, first=%q second=%q", firstDevice, secondDevice)
	}
	if issuer.deviceID != secondDevice {
		t.Fatalf("expected issuer to receive generated device id %q, got %q", secondDevice, issuer.deviceID)
	}
}

func TestProfileUpdateSyncsMatrixProfileForUserSearch(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)

	profile := mustHandle[ownerProfile](t, service, "profile.update", map[string]any{
		"display_name": "Custom Nick",
		"avatar_url":   "mxc://example.com/custom",
	})

	if profile.DisplayName != "Custom Nick" {
		t.Fatalf("expected product profile to update, got %#v", profile)
	}
	if issuer.profileUser != "@owner:example.com" || issuer.profileName != "Custom Nick" || issuer.profileURL != "mxc://example.com/custom" {
		t.Fatalf("expected Matrix profile sync for user search, got user=%q name=%q avatar=%q", issuer.profileUser, issuer.profileName, issuer.profileURL)
	}
}
