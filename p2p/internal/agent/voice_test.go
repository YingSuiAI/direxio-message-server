package agent

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
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
	v := &voiceCoordinator{cfg: voiceRuntimeConfig{WebhookSecret: "callback-secret"}, sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}, active: func(string) bool { return true }}
	s := &voiceSession{SessionID: "voice-1", OwnerID: "owner", ExpiresAt: time.Now().Add(time.Minute), State: "started", ActiveTurnID: "turn-1", op: &sync.Mutex{}}
	v.sessions[s.SessionID] = s
	stream := make(chan nativeagent.Event, 1)
	v.streams[s.SessionID] = map[chan nativeagent.Event]struct{}{stream: {}}
	callbackToken := v.callbackToken(s.SessionID, s.ExpiresAt)
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
	if !s.Ended || s.PendingStopID != "turn-1" || s.State != "stopping" {
		t.Fatalf("ended=%v state=%s pending=%s", s.Ended, s.State, s.PendingStopID)
	}
	if _, ok := v.streams[s.SessionID]; ok {
		t.Fatal("stream remained attached while durable stop was pending")
	}
	m := &Module{voice: v}
	if err := m.AuthorizeVoiceCallback(s.SessionID, callbackToken); err == nil {
		t.Fatal("callback accepted while durable stop was pending")
	}
	if err := m.ValidateVoiceProviderPayload(s.SessionID, map[string]any{"session_id": s.SessionID}); err == nil {
		t.Fatal("provider payload accepted while durable stop was pending")
	}
	if _, err := m.RunVoiceCustomLLM(context.Background(), s.SessionID, callbackToken, "hello"); err == nil {
		t.Fatal("custom LLM accepted while durable stop was pending")
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

func TestVoiceEndCompactsTerminalSessionAndEvictsExpiredTombstone(t *testing.T) {
	client := &testVoiceClient{}
	v := &voiceCoordinator{cfg: voiceRuntimeConfig{WebhookSecret: "callback-secret"}, sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}, client: func(context.Context, string, *voiceSession) (voiceChatClient, *actionbase.Error) {
		return client, nil
	}}
	s := &voiceSession{SessionID: "voice-compact", OwnerID: "owner", ExpiresAt: time.Now().Add(time.Minute), Token: "rtc-token", CallbackToken: "callback-token", VoiceChatAppID: "app", RoomID: "room", UserID: "user", SpeechProfileID: "speech-profile", SpeechRevision: 3, SpeechCredential: 4, SpeechProviderConfig: map[string]any{"secret": "should-clear"}, op: &sync.Mutex{}}
	v.sessions[s.SessionID] = s
	callbackToken := v.callbackToken(s.SessionID, s.ExpiresAt)
	if _, err := v.end(context.Background(), "owner", map[string]any{"session_id": s.SessionID}); err != nil {
		t.Fatal(err)
	}
	if !s.Ended || s.Token != "" || s.CallbackToken != "" || s.SpeechProviderConfig != nil || s.VoiceChatAppID != "" || s.RoomID != "" || s.SpeechProfileID != "" {
		t.Fatalf("terminal session was not compacted: %#v", s)
	}
	if client.stops != 1 {
		t.Fatalf("provider stops=%d", client.stops)
	}
	second, err := v.end(context.Background(), "owner", map[string]any{"session_id": s.SessionID})
	if err != nil || !actionbase.Bool(second.(map[string]any)["already_ended"]) || client.stops != 1 {
		t.Fatalf("repeated end was not idempotent: response=%#v err=%v stops=%d", second, err, client.stops)
	}
	m := &Module{voice: v}
	if err := m.AuthorizeVoiceCallback(s.SessionID, callbackToken); err == nil {
		t.Fatal("terminal callback was accepted")
	}
	if err := m.ValidateVoiceProviderPayload(s.SessionID, map[string]any{"session_id": s.SessionID}); err == nil {
		t.Fatal("terminal provider payload was accepted")
	}
	v.mu.Lock()
	s.TombstoneExpiresAt = time.Now().Add(-time.Second)
	v.mu.Unlock()
	v.cleanupExpired(context.Background())
	if _, ok := v.sessions[s.SessionID]; ok {
		t.Fatal("expired tombstone was not evicted")
	}
}

func TestVoiceDurableRetryTombstoneRejectsTranscript(t *testing.T) {
	v := &voiceCoordinator{cfg: voiceRuntimeConfig{ClientTranscriptSubmit: true}, sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	s := &voiceSession{SessionID: "voice-transcript-retry", OwnerID: "owner", Ended: true, State: "stopping", PendingStopID: "turn-1", ProviderStopped: true, EndedAt: time.Now(), TombstoneExpiresAt: time.Now().Add(time.Hour)}
	v.sessions[s.SessionID] = s
	v.stop = func(context.Context, string, string) error { return fmt.Errorf("temporary") }
	if _, err := v.transcript(context.Background(), "owner", map[string]any{"session_id": s.SessionID, "transcript_final": "hello"}); err == nil || err.Status != http.StatusNotFound {
		t.Fatalf("transcript accepted during durable retry: %#v", err)
	}
}

func TestVoicePendingProviderStopRetainsRetryTombstone(t *testing.T) {
	attempts := 0
	client := &testVoiceClient{}
	v := &voiceCoordinator{sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}, client: func(context.Context, string, *voiceSession) (voiceChatClient, *actionbase.Error) {
		attempts++
		if attempts == 1 {
			return nil, actionbase.StatusError(http.StatusServiceUnavailable, "provider unavailable")
		}
		return client, nil
	}}
	s := &voiceSession{SessionID: "voice-provider-retry", OwnerID: "owner", ExpiresAt: time.Now().Add(-time.Minute), Token: "rtc-token", CallbackToken: "callback-token", VoiceChatAppID: "app", RoomID: "room", SpeechProfileID: "speech-profile", SpeechRevision: 1, SpeechCredential: 2, SpeechProviderConfig: map[string]any{"secret": "should-clear"}, op: &sync.Mutex{}}
	v.sessions[s.SessionID] = s
	v.cleanupExpired(context.Background())
	if !s.Ended || !s.ProviderStopPending || s.Token != "" || s.CallbackToken != "" || s.SpeechProviderConfig != nil || s.VoiceChatAppID != "app" || s.SpeechProfileID != "speech-profile" {
		t.Fatalf("pending retry state=%#v", s)
	}
	v.cleanupExpired(context.Background())
	if s.ProviderStopPending || client.stops != 1 || attempts != 2 || s.TombstoneExpiresAt.IsZero() {
		t.Fatalf("retry state pending=%v stops=%d attempts=%d", s.ProviderStopPending, client.stops, attempts)
	}
	if s.VoiceChatAppID != "" || s.RoomID != "" || s.SpeechProfileID != "" {
		t.Fatalf("provider retry material retained after success: %#v", s)
	}
}
func TestVoiceSessionTombstonesHaveHardCap(t *testing.T) {
	v := &voiceCoordinator{sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	now := time.Now()
	for i := 0; i < voiceSessionCapacity*2; i++ {
		id := fmt.Sprintf("voice-tombstone-%d", i)
		v.sessions[id] = &voiceSession{SessionID: id, OwnerID: "owner", Ended: true, EndedAt: now.Add(-time.Duration(i+1) * time.Second), TombstoneExpiresAt: now.Add(time.Hour), Token: "rtc-token", CallbackToken: "callback-token", SpeechProviderConfig: map[string]any{"profile": "config"}}
	}
	v.cleanupExpired(context.Background())
	if got := len(v.sessions); got > voiceSessionCapacity {
		t.Fatalf("tombstone cap exceeded: got %d cap %d", got, voiceSessionCapacity)
	}
	for id, s := range v.sessions {
		if s.Token != "" || s.CallbackToken != "" || s.SpeechProviderConfig != nil {
			t.Fatalf("tombstone %s retained terminal material: %#v", id, s)
		}
	}
}

func TestVoicePendingTombstonesSurviveCapacityPrune(t *testing.T) {
	v := &voiceCoordinator{sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	now := time.Now()
	pending := voiceSessionCapacity / 2
	for i := 0; i < pending; i++ {
		id := fmt.Sprintf("voice-pending-%d", i)
		v.sessions[id] = &voiceSession{SessionID: id, OwnerID: "owner", Ended: true, PendingStopID: "turn-" + id, ProviderStopPending: true, EndedAt: now.Add(-time.Minute), TombstoneExpiresAt: now.Add(time.Hour)}
	}
	for i := 0; i < voiceSessionCapacity-pending+16; i++ {
		id := fmt.Sprintf("voice-ordinary-%d", i)
		v.sessions[id] = &voiceSession{SessionID: id, OwnerID: "owner", Ended: true, EndedAt: now.Add(-time.Duration(i+1) * time.Second), TombstoneExpiresAt: now.Add(time.Hour)}
	}
	v.mu.Lock()
	v.pruneVoiceTombstonesLocked(now)
	v.mu.Unlock()
	if len(v.sessions) != voiceSessionCapacity {
		t.Fatalf("capacity=%d want %d", len(v.sessions), voiceSessionCapacity)
	}
	for i := 0; i < pending; i++ {
		if _, ok := v.sessions[fmt.Sprintf("voice-pending-%d", i)]; !ok {
			t.Fatalf("pending tombstone %d was evicted", i)
		}
	}
}

func TestVoiceCreateBackpressuresWhenAllSlotsPending(t *testing.T) {
	v := &voiceCoordinator{cfg: voiceRuntimeConfig{WebhookSecret: "secret", WebhookURL: "https://voice.example.test/events", CustomLLMURL: "https://voice.example.test/llm"}, sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	now := time.Now()
	for i := 0; i < voiceSessionCapacity; i++ {
		id := fmt.Sprintf("voice-pending-%d", i)
		v.sessions[id] = &voiceSession{SessionID: id, OwnerID: "owner", Ended: true, PendingStopID: "turn-" + id, ProviderStopPending: true, EndedAt: now, TombstoneExpiresAt: now.Add(time.Hour)}
	}
	providerCalls := 0
	v.client = func(context.Context, string, *voiceSession) (voiceChatClient, *actionbase.Error) {
		providerCalls++
		return &testVoiceClient{}, nil
	}
	_, err := v.create(context.Background(), "owner", map[string]any{"conversation_id": "conv"})
	if err == nil || err.Status != http.StatusServiceUnavailable || err.Code != "voice_capacity_exhausted" || providerCalls != 0 {
		t.Fatalf("capacity error=%#v providerCalls=%d", err, providerCalls)
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
