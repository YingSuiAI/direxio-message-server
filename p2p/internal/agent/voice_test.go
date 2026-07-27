package agent

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestVoiceSessionPinsRoleProfilesAndOwnerFence(t *testing.T) {
	store := storage.NewMemoryStore()
	result, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner-a", "voice-seed", storage.ModelProfileDefaults{ConversationClientProfileID: "chat", SpeechClientProfileID: "speech"}, []storage.ModelProfileSyncEntry{
		{ClientProfileID: "chat", Provider: "openai", Model: "gpt-4o", ModelKind: storage.ModelKindConversation, APIKey: stringPtr("chat-secret")},
		{ClientProfileID: "speech", Provider: "volc_voice", ModelKind: storage.ModelKindSpeech, ProviderConfig: map[string]any{"app_id": "123456781234567812345678"}, ProviderSecrets: map[string]string{"rtc_app_key": "rtc-secret", "access_key_id": "ak", "secret_access_key": "sk"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v := newVoiceCoordinator(store, func() string { return "owner-a" })
	generation := uint64(1)
	v.generation = func() uint64 { return generation }
	v.active = func(string) bool { return true }
	v.cfg.WebhookSecret, v.cfg.WebhookURL, v.cfg.CustomLLMURL = "callback-secret", "https://voice.example.test/events", "https://voice.example.test/custom"
	response, apiErr := v.create(context.Background(), "owner-a", map[string]any{"conversation_id": "conv-1"})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	projection := response.(map[string]any)
	if projection["conversation_profile_id"] != result.Profiles[0].ProfileID || projection["speech_profile_id"] == "" {
		t.Fatalf("unexpected profile pins: %#v", projection)
	}
	if _, ok := projection["api_key"]; ok {
		t.Fatal("voice response leaked api_key")
	}
	if _, apiErr = v.end(context.Background(), "owner-b", map[string]any{"session_id": projection["session_id"]}); apiErr == nil || apiErr.Status != 403 {
		t.Fatalf("owner fence = %#v", apiErr)
	}
	generation = 2
	if _, apiErr = v.end(context.Background(), "owner-a", map[string]any{"session_id": projection["session_id"]}); apiErr == nil || apiErr.Status != 401 {
		t.Fatalf("generation fence = %#v", apiErr)
	}
	var secrets map[string]string
	speech, _, _ := store.GetModelProfile(context.Background(), "owner-a", projection["speech_profile_id"].(string))
	if err := json.Unmarshal([]byte(speech.APIKey), &secrets); err != nil || secrets["rtc_app_key"] != "rtc-secret" {
		t.Fatal("speech secret bundle unavailable")
	}
}

func TestVoiceReadyRequiresHTTPSCallback(t *testing.T) {
	v := &voiceCoordinator{profiles: storage.NewMemoryStore(), cfg: voiceRuntimeConfig{WebhookSecret: "secret", WebhookURL: "https://voice.example.test/events", CustomLLMURL: "http://voice.example.test/custom"}}
	m := &Module{voice: v}
	if m.VoiceReady() {
		t.Fatal("insecure callback must not advertise voice readiness")
	}
	v.cfg.CustomLLMURL = "https://voice.example.test/custom"
	if !m.VoiceReady() {
		t.Fatal("both https callbacks should advertise voice readiness")
	}
	v.cfg.WebhookURL = ""
	if m.VoiceReady() {
		t.Fatal("event callback is mandatory")
	}
}

func TestVolcRTCTokenGrantsOnlyAudioPublishAndSubscribe(t *testing.T) {
	const appID = "123456781234567812345678"
	token, err := (volcRTCTokenSigner{}).SignRTC(appID, "rtc-secret", "room", "user", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(token[3+len(appID):])
	if err != nil || len(raw) < 2 {
		t.Fatalf("invalid token encoding: %v", err)
	}
	bodyLen := int(binary.LittleEndian.Uint16(raw[:2]))
	if len(raw) < 2+bodyLen {
		t.Fatal("invalid token body")
	}
	body := raw[2 : 2+bodyLen]
	// nonce, issue time, expiry, room, user, then privilege count.
	offset := 12
	for range 2 {
		if len(body) < offset+2 {
			t.Fatal("invalid rtc identity")
		}
		offset += 2 + int(binary.LittleEndian.Uint16(body[offset:]))
	}
	if len(body) < offset+2 {
		t.Fatal("missing privileges")
	}
	count := int(binary.LittleEndian.Uint16(body[offset:]))
	offset += 2
	if count != 2 || len(body) != offset+count*6 {
		t.Fatalf("privilege framing count=%d len=%d", count, len(body))
	}
	got := []uint16{binary.LittleEndian.Uint16(body[offset:]), binary.LittleEndian.Uint16(body[offset+6:])}
	if got[0] != 1 || got[1] != 4 {
		t.Fatalf("privileges=%v; want audio publish=1, subscribe=4", got)
	}
}

func TestVoiceProviderPayloadRequiresMatchingSessionMarker(t *testing.T) {
	m := &Module{voice: &voiceCoordinator{sessions: map[string]*voiceSession{"voice-1": {SessionID: "voice-1", RoomID: "room", UserID: "user"}}}}
	if err := m.ValidateVoiceProviderPayload("voice-1", map[string]any{}); err == nil {
		t.Fatal("missing marker accepted")
	}
	if err := m.ValidateVoiceProviderPayload("voice-1", map[string]any{"session_id": "other"}); err == nil {
		t.Fatal("mismatched marker accepted")
	}
	if err := m.ValidateVoiceProviderPayload("voice-1", map[string]any{"custom": `{"session_id":"voice-1"}`}); err != nil {
		t.Fatalf("valid marker rejected: %v", err)
	}
}

func TestVoiceCustomLLMDoesNotClaimAfterTerminalRevoke(t *testing.T) {
	called := 0
	v := &voiceCoordinator{cfg: voiceRuntimeConfig{WebhookSecret: "secret"}, sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	s := &voiceSession{SessionID: "voice-1", OwnerID: "owner", ExpiresAt: time.Now().Add(time.Minute), Ended: true, op: &sync.Mutex{}}
	v.sessions[s.SessionID] = s
	v.active = func(string) bool { return true }
	v.durable = func(context.Context, string, map[string]any, func(agentturns.StreamEvent) error) error {
		called++
		return nil
	}
	m := &Module{voice: v}
	_, err := m.RunVoiceCustomLLM(context.Background(), s.SessionID, v.callbackToken(s.SessionID, s.ExpiresAt), "hello", "request-1")
	if err == nil || called != 0 {
		t.Fatalf("terminal session launched durable=%d err=%v", called, err)
	}
}

func TestVoiceExpiryRetriesPendingDurableStop(t *testing.T) {
	attempts := 0
	v := &voiceCoordinator{profiles: storage.NewMemoryStore(), sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	s := &voiceSession{SessionID: "voice-1", OwnerID: "owner", ExpiresAt: time.Now().Add(-time.Minute), ActiveTurnID: "turn-1", op: &sync.Mutex{}}
	v.sessions[s.SessionID] = s
	v.stop = func(context.Context, string, string) error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("temporary")
		}
		return nil
	}
	v.cleanupExpired(context.Background())
	if got := v.sessions[s.SessionID].PendingStopID; got != "turn-1" {
		t.Fatalf("pending=%q", got)
	}
	v.cleanupExpired(context.Background())
	if got := v.sessions[s.SessionID].PendingStopID; got != "" || attempts != 2 {
		t.Fatalf("pending=%q attempts=%d", got, attempts)
	}
}

func TestVoiceExpiryRetriesProviderStopPending(t *testing.T) {
	calls := 0
	client := &testVoiceClient{}
	v := &voiceCoordinator{profiles: storage.NewMemoryStore(), sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	s := &voiceSession{SessionID: "voice-1", OwnerID: "owner", ExpiresAt: time.Now().Add(-time.Minute), op: &sync.Mutex{}}
	v.sessions[s.SessionID] = s
	v.client = func(context.Context, string, *voiceSession) (voiceChatClient, *actionbase.Error) {
		calls++
		if calls == 1 {
			return nil, actionbase.StatusError(503, "provider unavailable")
		}
		return client, nil
	}
	v.cleanupExpired(context.Background())
	if !v.sessions[s.SessionID].ProviderStopPending {
		t.Fatal("provider stop must stay pending after resolution failure")
	}
	v.cleanupExpired(context.Background())
	if v.sessions[s.SessionID].ProviderStopPending {
		t.Fatal("provider stop pending should clear after retry")
	}
	if calls != 2 || client.stops != 1 {
		t.Fatalf("calls=%d stops=%d", calls, client.stops)
	}
}

type testVoiceClient struct{ stops int }

func (c *testVoiceClient) StartVoiceChat(context.Context, voiceSession) error     { return nil }
func (c *testVoiceClient) StopVoiceChat(context.Context, voiceSession) error      { c.stops++; return nil }
func (c *testVoiceClient) InterruptVoiceChat(context.Context, voiceSession) error { return nil }

func TestVoiceAbortPropagatesProviderResolutionFailure(t *testing.T) {
	v := &voiceCoordinator{sessions: map[string]*voiceSession{"voice-1": {SessionID: "voice-1", OwnerID: "owner", op: &sync.Mutex{}}}, streams: map[string]map[chan nativeagent.Event]struct{}{}, client: func(context.Context, string, *voiceSession) (voiceChatClient, *actionbase.Error) {
		return nil, actionbase.StatusError(503, "provider unavailable")
	}}
	if err := (&Module{voice: v}).AbortVoiceSessions(context.Background()); err == nil {
		t.Fatal("abort swallowed provider resolution failure")
	}
}

func TestVoiceEndRetriesDurableStopWithoutSecondProviderStop(t *testing.T) {
	client, attempts := &testVoiceClient{}, 0
	v := &voiceCoordinator{sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}, active: func(string) bool { return true }}
	s := &voiceSession{SessionID: "voice-1", OwnerID: "owner", ExpiresAt: time.Now().Add(time.Minute), State: "started", ActiveTurnID: "turn-1", op: &sync.Mutex{}}
	v.sessions[s.SessionID] = s
	v.client = func(context.Context, string, *voiceSession) (voiceChatClient, *actionbase.Error) { return client, nil }
	v.stop = func(context.Context, string, string) error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("temporary")
		}
		return nil
	}
	if _, err := v.end(context.Background(), "owner", map[string]any{"session_id": s.SessionID}); err == nil {
		t.Fatal("first durable stop failure accepted")
	}
	if s.PendingStopID != "turn-1" || s.State != "stopping" {
		t.Fatalf("state=%s pending=%s", s.State, s.PendingStopID)
	}
	if _, err := v.end(context.Background(), "owner", map[string]any{"session_id": s.SessionID}); err != nil {
		t.Fatal(err)
	}
	if client.stops != 1 || attempts != 2 {
		t.Fatalf("provider stops=%d durable attempts=%d", client.stops, attempts)
	}
	if s.PendingStopID != "" {
		t.Fatalf("pending remained %q", s.PendingStopID)
	}
	v.cleanupExpired(context.Background())
	if client.stops != 1 || attempts != 2 {
		t.Fatal("terminal session repeated stop")
	}
}

func TestVoiceAbortStopsClaimedDurableTurnWithoutWaiting(t *testing.T) {
	claimed, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	v := &voiceCoordinator{cfg: voiceRuntimeConfig{WebhookSecret: "secret"}, sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}, active: func(string) bool { return true }}
	s := &voiceSession{SessionID: "voice-1", OwnerID: "owner", ExpiresAt: time.Now().Add(time.Minute), op: &sync.Mutex{}}
	v.sessions[s.SessionID] = s
	v.client = func(context.Context, string, *voiceSession) (voiceChatClient, *actionbase.Error) {
		return &testVoiceClient{}, nil
	}
	v.stop = func(context.Context, string, string) error { stopped <- struct{}{}; return nil }
	v.durable = func(context.Context, string, map[string]any, func(agentturns.StreamEvent) error) error {
		close(claimed)
		<-release
		return nil
	}
	m := &Module{voice: v}
	done := make(chan struct{})
	go func() {
		_, _ = m.RunVoiceCustomLLM(context.Background(), s.SessionID, v.callbackToken(s.SessionID, s.ExpiresAt), "hello", "request")
		close(done)
	}()
	<-claimed
	abortDone := make(chan error, 1)
	go func() { abortDone <- m.AbortVoiceSessions(context.Background()) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("abort waited for durable")
	}
	select {
	case err := <-abortDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort blocked")
	}
	close(release)
	<-done
}

func TestVoiceAbortRetriesProviderStopPending(t *testing.T) {
	client := &testVoiceClient{}
	v := &voiceCoordinator{sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	s := &voiceSession{SessionID: "voice-1", OwnerID: "owner", op: &sync.Mutex{}}
	v.sessions[s.SessionID] = s
	calls := 0
	v.client = func(context.Context, string, *voiceSession) (voiceChatClient, *actionbase.Error) {
		calls++
		if calls == 1 {
			return nil, actionbase.StatusError(503, "down")
		}
		return client, nil
	}
	m := &Module{voice: v}
	if err := m.AbortVoiceSessions(context.Background()); err == nil || !s.ProviderStopPending {
		t.Fatalf("first abort err=%v pending=%v", err, s.ProviderStopPending)
	}
	if err := m.AbortVoiceSessions(context.Background()); err != nil || s.ProviderStopPending {
		t.Fatalf("second abort err=%v pending=%v", err, s.ProviderStopPending)
	}
	if err := m.AbortVoiceSessions(context.Background()); err != nil || calls != 2 {
		t.Fatalf("third abort err=%v calls=%d", err, calls)
	}
}

func TestVoiceCustomLLMTerminalPathsDoNotDoubleUnlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		durable func(func(agentturns.StreamEvent) error) error
		wantErr bool
	}{{"done", func(emit func(agentturns.StreamEvent) error) error {
		return emit(agentturns.StreamEvent{Event: "done"})
	}, false}, {"error", func(func(agentturns.StreamEvent) error) error { return fmt.Errorf("boom") }, true}} {
		t.Run(tc.name, func(t *testing.T) {
			v := &voiceCoordinator{cfg: voiceRuntimeConfig{WebhookSecret: "secret"}, sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}, active: func(string) bool { return true }}
			s := &voiceSession{SessionID: "voice", OwnerID: "owner", ExpiresAt: time.Now().Add(time.Minute), op: &sync.Mutex{}}
			v.sessions[s.SessionID] = s
			v.durable = func(_ context.Context, _ string, _ map[string]any, emit func(agentturns.StreamEvent) error) error {
				return tc.durable(emit)
			}
			_, err := (&Module{voice: v}).RunVoiceCustomLLM(context.Background(), s.SessionID, v.callbackToken(s.SessionID, s.ExpiresAt), "hi", "req")
			if (err != nil) != tc.wantErr || s.Busy || s.ActiveTurnID != "" {
				t.Fatalf("err=%v busy=%v active=%q", err, s.Busy, s.ActiveTurnID)
			}
		})
	}
}

type failingStopClient struct{ fail *int }

func (c failingStopClient) StartVoiceChat(context.Context, voiceSession) error     { return nil }
func (c failingStopClient) InterruptVoiceChat(context.Context, voiceSession) error { return nil }
func (c failingStopClient) StopVoiceChat(context.Context, voiceSession) error {
	if *c.fail > 0 {
		*c.fail--
		return fmt.Errorf("down")
	}
	return nil
}
func TestVoiceAbortRetriesAllProviderStops(t *testing.T) {
	fails, calls := 2, 0
	v := &voiceCoordinator{sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	for _, id := range []string{"a", "b"} {
		v.sessions[id] = &voiceSession{SessionID: id, OwnerID: "o", op: &sync.Mutex{}}
	}
	v.client = func(context.Context, string, *voiceSession) (voiceChatClient, *actionbase.Error) {
		calls++
		return failingStopClient{&fails}, nil
	}
	m := &Module{voice: v}
	if m.AbortVoiceSessions(context.Background()) == nil || !v.sessions["a"].ProviderStopPending || !v.sessions["b"].ProviderStopPending {
		t.Fatal("both pending required")
	}
	if err := m.AbortVoiceSessions(context.Background()); err != nil || v.sessions["a"].ProviderStopPending || v.sessions["b"].ProviderStopPending {
		t.Fatal("retry failed")
	}
	if err := m.AbortVoiceSessions(context.Background()); err != nil || calls != 4 {
		t.Fatalf("third calls=%d", calls)
	}
}

func stringPtr(value string) *string { return &value }
