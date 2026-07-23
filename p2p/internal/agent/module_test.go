package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
)

type recordingMCPInvoker struct {
	action string
}

type fakeVoiceTokenSigner struct {
	appID     string
	appKey    string
	roomID    string
	userID    string
	expiresAt time.Time
}

func (s *fakeVoiceTokenSigner) SignRTC(appID, appKey, roomID, userID string, expiresAt time.Time) (string, error) {
	s.appID = appID
	s.appKey = appKey
	s.roomID = roomID
	s.userID = userID
	s.expiresAt = expiresAt
	return fmt.Sprintf("rtc-token:%s:%s", roomID, userID), nil
}

type fakeVoiceChatClient struct {
	started []voiceSession
	stopped []voiceSession
}

func (c *fakeVoiceChatClient) StartVoiceChat(_ context.Context, session voiceSession) error {
	c.started = append(c.started, session)
	return nil
}

func (c *fakeVoiceChatClient) StopVoiceChat(_ context.Context, session voiceSession) error {
	c.stopped = append(c.stopped, session)
	return nil
}

func newTestVoiceCoordinator(cfg voiceConfig) (*voiceCoordinator, *fakeVoiceTokenSigner, *fakeVoiceChatClient) {
	if cfg.WebhookSecret == "" {
		cfg.WebhookSecret = "secret"
	}
	if cfg.WebhookURL == "" && cfg.CustomLLMURL == "" {
		cfg.WebhookURL = "https://example.com/_p2p/agent/voice/webhook"
	}
	voice := newVoiceCoordinator(cfg)
	signer := &fakeVoiceTokenSigner{}
	client := &fakeVoiceChatClient{}
	voice.signer = signer
	voice.client = client
	return voice, signer, client
}

func TestVoiceSessionCreateStreamInterruptAndEnd(t *testing.T) {
	voice, signer, client := newTestVoiceCoordinator(voiceConfig{
		AppID:          "rtc-app",
		AppKey:         "app-key",
		VoiceChatAppID: "ai-agent-app",
		AIUserID:       "ai-user",
	})
	value, actionErr := voice.create(context.Background(), map[string]any{"source": "native_agent"})
	if actionErr != nil {
		t.Fatalf("create voice session: %v", actionErr)
	}
	session := value.(map[string]any)
	sessionID := session["session_id"].(string)
	if session["app_id"] != "rtc-app" || session["ai_user_id"] != "ai-user" {
		t.Fatalf("unexpected voice session response: %#v", session)
	}
	if !volcRTCIDPattern.MatchString(session["room_id"].(string)) || !volcRTCIDPattern.MatchString(session["user_id"].(string)) {
		t.Fatalf("generated RTC IDs must match Volc constraints: %#v", session)
	}
	if signer.appID != "rtc-app" || signer.appKey != "app-key" || signer.roomID != session["room_id"] || signer.userID != session["user_id"] {
		t.Fatalf("token signer not called with session identity: signer=%#v session=%#v", signer, session)
	}
	if session["token"] != "rtc-token:"+session["room_id"].(string)+":"+session["user_id"].(string) {
		t.Fatalf("unexpected signed token: %#v", session)
	}
	if len(client.started) != 0 {
		t.Fatalf("StartVoiceChat called before explicit start: %#v", client.started)
	}
	if _, actionErr := voice.start(context.Background(), map[string]any{"session_id": sessionID}); actionErr != nil {
		t.Fatalf("start voice session: %v", actionErr)
	}
	if len(client.started) != 1 || client.started[0].TaskID != sessionID || client.started[0].AIUserID != "ai-user" || client.started[0].AppID != "rtc-app" || client.started[0].VoiceChatAppID != "ai-agent-app" {
		t.Fatalf("StartVoiceChat not called with dynamic session: %#v", client.started)
	}
	if _, actionErr := voice.start(context.Background(), map[string]any{"session_id": sessionID}); actionErr != nil {
		t.Fatalf("repeat start voice session: %v", actionErr)
	}
	if len(client.started) != 1 {
		t.Fatalf("repeat start should be idempotent: %#v", client.started)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan nativeagent.Event, 4)
	done := make(chan error, 1)
	go func() {
		done <- voice.stream(ctx, map[string]any{"session_id": sessionID}, func(event nativeagent.Event) error {
			events <- event
			return nil
		})
	}()
	first := nextVoiceEvent(t, events)
	if first.Event != "listening" {
		t.Fatalf("first event = %#v", first)
	}
	if _, actionErr := voice.interrupt(context.Background(), map[string]any{"session_id": sessionID}); actionErr != nil {
		t.Fatalf("interrupt voice session: %v", actionErr)
	}
	if event := nextVoiceEvent(t, events); event.Event != "listening" {
		t.Fatalf("interrupt event = %#v", event)
	}
	if _, actionErr := voice.end(context.Background(), map[string]any{"session_id": sessionID}); actionErr != nil {
		t.Fatalf("end voice session: %v", actionErr)
	}
	if len(client.stopped) != 1 || client.stopped[0].TaskID != sessionID {
		t.Fatalf("StopVoiceChat not called once for ended session: %#v", client.stopped)
	}
	if event := nextVoiceEvent(t, events); event.Event != "done" {
		t.Fatalf("end event = %#v", event)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("voice stream returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("voice stream did not finish after session end")
	}
}

type voiceRunnerStub struct {
	params      map[string]any
	count       int
	doneText    string
	deltaText   string
	seenPrompts []string
}

func (r *voiceRunnerStub) Apply(context.Context, string) error { return nil }

func (r *voiceRunnerStub) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (r *voiceRunnerStub) Stream(_ context.Context, _ string, params map[string]any, emit func(nativeagent.Event) error) error {
	r.count++
	r.seenPrompts = append(r.seenPrompts, actionbase.String(params["prompt"]))
	r.params = cloneMap(params)
	deltaText := r.deltaText
	if deltaText == "" {
		deltaText = "回答"
	}
	if err := emit(nativeagent.Event{Event: "delta", Data: map[string]any{"text": deltaText}}); err != nil {
		return err
	}
	doneText := r.doneText
	if doneText == "" {
		doneText = "回答"
	}
	return emit(nativeagent.Event{Event: "done", Data: map[string]any{
		"text": doneText,
		"references": []map[string]any{{
			"kind":      "room",
			"room_id":   "!team:example.com",
			"room_type": "group",
			"title":     "产品群",
		}},
	}})
}

type voiceRunnerDeltaOnlyDoneStub struct {
	params map[string]any
}

func (r *voiceRunnerDeltaOnlyDoneStub) Apply(context.Context, string) error { return nil }

func (r *voiceRunnerDeltaOnlyDoneStub) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func (r *voiceRunnerDeltaOnlyDoneStub) Stream(_ context.Context, _ string, params map[string]any, emit func(nativeagent.Event) error) error {
	r.params = cloneMap(params)
	if err := emit(nativeagent.Event{Event: "delta", Data: map[string]any{"text": "第一段"}}); err != nil {
		return err
	}
	if err := emit(nativeagent.Event{Event: "delta", Data: map[string]any{"text": "第二段"}}); err != nil {
		return err
	}
	return emit(nativeagent.Event{Event: "done", Data: map[string]any{
		"references": []map[string]any{{
			"kind":      "room",
			"room_id":   "!team:example.com",
			"room_type": "group",
			"title":     "产品群",
		}},
	}})
}

func TestVoiceSessionRequiresVolcConfiguration(t *testing.T) {
	voice := newVoiceCoordinator(voiceConfig{})
	_, actionErr := voice.create(context.Background(), map[string]any{"source": "native_agent"})
	if actionErr == nil || actionErr.Status != 503 || actionErr.Code != "volc_rtc_not_configured" {
		t.Fatalf("expected missing config error, got %#v", actionErr)
	}
	voice = newVoiceCoordinator(voiceConfig{AppID: "volc-app"})
	_, actionErr = voice.create(context.Background(), map[string]any{"source": "native_agent"})
	if actionErr == nil || actionErr.Status != 503 || actionErr.Code != "volc_rtc_app_key_not_configured" {
		t.Fatalf("expected missing app key error, got %#v", actionErr)
	}
	voice = newVoiceCoordinator(voiceConfig{AppID: "volc-app", AppKey: "app-key"})
	_, actionErr = voice.create(context.Background(), map[string]any{"source": "native_agent"})
	if actionErr == nil || actionErr.Status != 503 || actionErr.Code != "volc_openapi_not_configured" {
		t.Fatalf("expected missing OpenAPI config error, got %#v", actionErr)
	}
}

func TestVoiceSessionAllowsSeparateRTCAndVoiceChatAppIDs(t *testing.T) {
	voice, signer, client := newTestVoiceCoordinator(voiceConfig{
		AppID:          "rtc-app",
		AppKey:         "app-key",
		VoiceChatAppID: "voice-chat-app",
	})
	value, actionErr := voice.create(context.Background(), map[string]any{"source": "native_agent"})
	if actionErr != nil {
		t.Fatalf("create voice session with separate app ids: %v", actionErr)
	}
	sessionID := value.(map[string]any)["session_id"].(string)
	if _, actionErr := voice.start(context.Background(), map[string]any{"session_id": sessionID}); actionErr != nil {
		t.Fatalf("start voice session with separate app ids: %v", actionErr)
	}
	if signer.appID != "rtc-app" {
		t.Fatalf("RTC token should use rtc app id, got %#v", signer)
	}
	if len(client.started) != 1 || client.started[0].AppID != "rtc-app" || client.started[0].VoiceChatAppID != "voice-chat-app" {
		t.Fatalf("VoiceChat should use separate ai agent app id, got %#v", client.started)
	}
}

func TestVoiceWebhookRunsNativeAgentAndPublishesReferences(t *testing.T) {
	runner := &voiceRunnerStub{}
	module := New(Config{Runner: runner})
	voice, _, _ := newTestVoiceCoordinator(voiceConfig{
		AppID:         "volc-app",
		AppKey:        "app-key",
		WebhookSecret: "secret",
	})
	module.voice = voice
	value, actionErr := module.createVoiceSession(context.Background(), map[string]any{
		"source":          "native_agent",
		"conversation_id": "voice-conversation",
		"room_id":         "!product:example.com",
		"room_type":       "group",
		"model_profile":   map[string]any{"provider": "openai_compatible", "model": "mock"},
		"conversation_context": map[string]any{
			"summary": "用户上一轮确认了产品群。",
			"messages": []any{
				map[string]any{"role": "user", "text": "我昨天建的群是什么"},
				map[string]any{"role": "assistant", "text": "产品群"},
			},
		},
		"api_key": "request-scoped-key",
	})
	if actionErr != nil {
		t.Fatalf("create voice session: %v", actionErr)
	}
	sessionID := value.(map[string]any)["session_id"].(string)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan nativeagent.Event, 8)
	done := make(chan error, 1)
	go func() {
		done <- module.Stream(ctx, "agent.voice.session.stream", map[string]any{"session_id": sessionID}, func(event nativeagent.Event) error {
			events <- event
			return nil
		})
	}()
	if event := nextVoiceEvent(t, events); event.Event != "listening" {
		t.Fatalf("first voice stream event = %#v", event)
	}
	response, actionErr := module.HandleVoiceWebhook(context.Background(), "secret", map[string]any{
		"session_id":       sessionID,
		"status":           "transcribing",
		"transcript_final": "总结产品群",
	})
	if actionErr != nil || response["ok"] != true {
		t.Fatalf("voice webhook = %#v, %v", response, actionErr)
	}
	if event := nextVoiceEvent(t, events); event.Event != "transcribing" {
		t.Fatalf("webhook transcript event = %#v", event)
	}
	if event := nextVoiceEvent(t, events); event.Event != "thinking" {
		t.Fatalf("thinking event = %#v", event)
	}
	if event := nextVoiceEvent(t, events); event.Event != "answer" || event.Data["answer_delta"] != "回答" {
		t.Fatalf("answer event = %#v", event)
	}
	doneEvent := nextVoiceEvent(t, events)
	if doneEvent.Event != "done" || doneEvent.Data["references"] == nil {
		t.Fatalf("done event = %#v", doneEvent)
	}
	if runner.params["prompt"] != "总结产品群" ||
		runner.params["api_key"] != "request-scoped-key" ||
		runner.params["conversation_id"] != "voice-conversation" ||
		runner.params["room_id"] != "!product:example.com" ||
		runner.params["room_type"] != "group" {
		t.Fatalf("native agent params not preserved: %#v", runner.params)
	}
	if contextParams, ok := runner.params["conversation_context"].(map[string]any); !ok || contextParams["summary"] != "用户上一轮确认了产品群。" {
		t.Fatalf("conversation context not forwarded: %#v", runner.params["conversation_context"])
	}
	assertVoiceStreamOpenAfterTurn(t, done)
	endVoiceStream(t, module, sessionID, done)
}

func TestVoiceCustomLLMRunsNativeAgentAndStreamsTTSChunks(t *testing.T) {
	runner := &voiceRunnerStub{}
	module := New(Config{Runner: runner})
	voice, _, _ := newTestVoiceCoordinator(voiceConfig{
		AppID:         "volc-app",
		AppKey:        "app-key",
		WebhookSecret: "secret",
		CustomLLMURL:  "https://example.com/_p2p/agent/voice/volc/custom-llm",
	})
	module.voice = voice
	value, actionErr := module.createVoiceSession(context.Background(), map[string]any{
		"source":          "native_agent",
		"conversation_id": "voice-conversation",
		"room_id":         "!product:example.com",
		"room_type":       "group",
		"api_key":         "request-scoped-key",
	})
	if actionErr != nil {
		t.Fatalf("create voice session: %v", actionErr)
	}
	sessionID := value.(map[string]any)["session_id"].(string)
	if value.(map[string]any)["client_transcript_submit_enabled"] != false {
		t.Fatalf("CustomLLM sessions should disable client transcript submit: %#v", value)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan nativeagent.Event, 8)
	done := make(chan error, 1)
	go func() {
		done <- module.Stream(ctx, "agent.voice.session.stream", map[string]any{"session_id": sessionID}, func(event nativeagent.Event) error {
			events <- event
			return nil
		})
	}()
	if event := nextVoiceEvent(t, events); event.Event != "listening" {
		t.Fatalf("first voice stream event = %#v", event)
	}
	var ttsChunks []string
	response, actionErr := module.HandleVoiceCustomLLM(context.Background(), "secret", sessionID, map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "ignored"},
			map[string]any{"role": "user", "content": "总结产品群"},
		},
	}, func(text string) error {
		ttsChunks = append(ttsChunks, text)
		return nil
	})
	if actionErr != nil || response["ok"] != true || response["text"] != "回答" {
		t.Fatalf("custom llm response = %#v, %v", response, actionErr)
	}
	if len(ttsChunks) != 1 || ttsChunks[0] != "回答" {
		t.Fatalf("tts chunks = %#v", ttsChunks)
	}
	if event := nextVoiceEvent(t, events); event.Event != "transcribing" || event.Data["transcript_final"] != "总结产品群" {
		t.Fatalf("transcribing event = %#v", event)
	}
	if event := nextVoiceEvent(t, events); event.Event != "thinking" {
		t.Fatalf("thinking event = %#v", event)
	}
	if event := nextVoiceEvent(t, events); event.Event != "answer" || event.Data["answer_delta"] != "回答" {
		t.Fatalf("answer event = %#v", event)
	}
	doneEvent := nextVoiceEvent(t, events)
	if doneEvent.Event != "done" || doneEvent.Data["references"] == nil {
		t.Fatalf("done event = %#v", doneEvent)
	}
	if runner.params["prompt"] != "总结产品群" ||
		runner.params["api_key"] != "request-scoped-key" ||
		runner.params["conversation_id"] != "voice-conversation" {
		t.Fatalf("native agent params not preserved: %#v", runner.params)
	}
	assertVoiceStreamOpenAfterTurn(t, done)
	endVoiceStream(t, module, sessionID, done)
}

func TestVoiceCustomLLMUsesAccumulatedDeltaAsSummaryWhenDoneTextMissing(t *testing.T) {
	runner := &voiceRunnerDeltaOnlyDoneStub{}
	module := New(Config{Runner: runner})
	voice, _, _ := newTestVoiceCoordinator(voiceConfig{
		AppID:         "volc-app",
		AppKey:        "app-key",
		WebhookSecret: "secret",
		CustomLLMURL:  "https://example.com/_p2p/agent/voice/volc/custom-llm",
	})
	module.voice = voice
	value, actionErr := module.createVoiceSession(context.Background(), map[string]any{
		"source":  "native_agent",
		"api_key": "request-scoped-key",
	})
	if actionErr != nil {
		t.Fatalf("create voice session: %v", actionErr)
	}
	sessionID := value.(map[string]any)["session_id"].(string)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan nativeagent.Event, 8)
	done := make(chan error, 1)
	go func() {
		done <- module.Stream(ctx, "agent.voice.session.stream", map[string]any{"session_id": sessionID}, func(event nativeagent.Event) error {
			events <- event
			return nil
		})
	}()
	if event := nextVoiceEvent(t, events); event.Event != "listening" {
		t.Fatalf("first voice stream event = %#v", event)
	}
	var ttsChunks []string
	response, actionErr := module.HandleVoiceCustomLLM(context.Background(), "secret", sessionID, map[string]any{
		"transcript": "总结产品群",
	}, func(text string) error {
		ttsChunks = append(ttsChunks, text)
		return nil
	})
	if actionErr != nil || response["text"] != "第一段第二段" {
		t.Fatalf("custom llm response = %#v, %v", response, actionErr)
	}
	if len(ttsChunks) != 2 || ttsChunks[0] != "第一段" || ttsChunks[1] != "第二段" {
		t.Fatalf("tts chunks = %#v", ttsChunks)
	}
	for _, want := range []string{"transcribing", "thinking", "answer", "answer"} {
		if event := nextVoiceEvent(t, events); event.Event != want {
			t.Fatalf("event = %#v, want %s", event, want)
		}
	}
	doneEvent := nextVoiceEvent(t, events)
	if doneEvent.Event != "done" || doneEvent.Data["summary"] != "第一段第二段" {
		t.Fatalf("done event should include accumulated summary: %#v", doneEvent)
	}
	assertVoiceStreamOpenAfterTurn(t, done)
	endVoiceStream(t, module, sessionID, done)
}

func TestVoiceCustomLLMRejectsBadSecret(t *testing.T) {
	module := New(Config{Runner: &voiceRunnerStub{}})
	voice, _, _ := newTestVoiceCoordinator(voiceConfig{
		AppID:         "volc-app",
		AppKey:        "app-key",
		WebhookSecret: "secret",
		CustomLLMURL:  "https://example.com/_p2p/agent/voice/volc/custom-llm",
	})
	module.voice = voice
	_, actionErr := module.HandleVoiceCustomLLM(context.Background(), "wrong", "voice_1", map[string]any{
		"transcript": "总结产品群",
	}, nil)
	if actionErr == nil || actionErr.Status != 401 {
		t.Fatalf("expected unauthorized custom llm callback, got %#v", actionErr)
	}
}

func TestVoiceTranscriptActionRunsNativeAgent(t *testing.T) {
	runner := &voiceRunnerStub{}
	module := New(Config{Runner: runner})
	voice, _, _ := newTestVoiceCoordinator(voiceConfig{
		AppID:  "volc-app",
		AppKey: "app-key",
	})
	module.voice = voice
	value, actionErr := module.createVoiceSession(context.Background(), map[string]any{
		"source":          "native_agent",
		"conversation_id": "voice-conversation",
		"room_id":         "!channel:example.com",
		"room_type":       "channel",
		"api_key":         "request-scoped-key",
	})
	if actionErr != nil {
		t.Fatalf("create voice session: %v", actionErr)
	}
	sessionID := value.(map[string]any)["session_id"].(string)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan nativeagent.Event, 8)
	done := make(chan error, 1)
	go func() {
		done <- module.Stream(ctx, "agent.voice.session.stream", map[string]any{"session_id": sessionID}, func(event nativeagent.Event) error {
			events <- event
			return nil
		})
	}()
	if event := nextVoiceEvent(t, events); event.Event != "listening" {
		t.Fatalf("first voice stream event = %#v", event)
	}
	response, actionErr := module.submitVoiceTranscript(context.Background(), map[string]any{
		"session_id":       sessionID,
		"transcript_final": "帮我查频道帖子",
	})
	if actionErr != nil || response.(map[string]any)["ok"] != true {
		t.Fatalf("voice transcript action = %#v, %v", response, actionErr)
	}
	if event := nextVoiceEvent(t, events); event.Event != "transcribing" || event.Data["transcript_final"] != "帮我查频道帖子" {
		t.Fatalf("transcript event = %#v", event)
	}
	if event := nextVoiceEvent(t, events); event.Event != "thinking" {
		t.Fatalf("thinking event = %#v", event)
	}
	if event := nextVoiceEvent(t, events); event.Event != "answer" || event.Data["answer_delta"] != "回答" {
		t.Fatalf("answer event = %#v", event)
	}
	doneEvent := nextVoiceEvent(t, events)
	if doneEvent.Event != "done" || doneEvent.Data["references"] == nil {
		t.Fatalf("done event = %#v", doneEvent)
	}
	if runner.params["prompt"] != "帮我查频道帖子" ||
		runner.params["api_key"] != "request-scoped-key" ||
		runner.params["conversation_id"] != "voice-conversation" ||
		runner.params["room_id"] != "!channel:example.com" ||
		runner.params["room_type"] != "channel" {
		t.Fatalf("native agent params not preserved: %#v", runner.params)
	}
	assertVoiceStreamOpenAfterTurn(t, done)
	endVoiceStream(t, module, sessionID, done)
}

func TestVoiceSessionDeduplicatesFinalTranscript(t *testing.T) {
	voice, _, _ := newTestVoiceCoordinator(voiceConfig{
		AppID:  "volc-app",
		AppKey: "app-key",
	})
	value, actionErr := voice.create(context.Background(), map[string]any{"source": "native_agent"})
	if actionErr != nil {
		t.Fatalf("create voice session: %v", actionErr)
	}
	sessionID := value.(map[string]any)["session_id"].(string)
	if _, accepted, _ := voice.acceptTranscript(sessionID, "重复问题"); !accepted {
		t.Fatal("first transcript should be accepted")
	}
	if _, accepted, reason := voice.acceptTranscript(sessionID, "重复问题"); accepted || reason != "duplicate" {
		t.Fatal("duplicate transcript should be ignored")
	}
	if _, accepted, _ := voice.acceptTranscript(sessionID, "新的问题"); !accepted {
		t.Fatal("new transcript should be accepted")
	}
}

func nextVoiceEvent(t *testing.T, events <-chan nativeagent.Event) nativeagent.Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for voice event")
		return nativeagent.Event{}
	}
}

func assertVoiceStreamOpenAfterTurn(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("voice stream returned before session end: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
}

func endVoiceStream(t *testing.T, module *Module, sessionID string, done <-chan error) {
	t.Helper()
	if _, actionErr := module.endVoiceSession(context.Background(), map[string]any{"session_id": sessionID}); actionErr != nil {
		t.Fatalf("end voice session: %v", actionErr)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("voice stream returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("voice stream did not finish after session end")
	}
}

func (i *recordingMCPInvoker) InvokeCapability(_ context.Context, action string, _ map[string]any) (any, *dirextalkmcp.Error) {
	i.action = action
	return map[string]any{"action": action}, nil
}

func TestRuntimeActionsUseConfiguredMCPService(t *testing.T) {
	invoker := &recordingMCPInvoker{}
	module := New(Config{MCP: dirextalkmcp.NewService(invoker)})
	handlers := module.Handlers()

	for _, test := range []struct {
		action string
		want   string
	}{
		{"agent.contacts.list", dirextalkmcp.ActionContactsList},
		{"agent.contacts.search", dirextalkmcp.ActionContactsSearch},
		{"agent.rooms.search", dirextalkmcp.ActionRoomsSearch},
		{"agent.messages.list", dirextalkmcp.ActionMessagesList},
		{"agent.messages.send", dirextalkmcp.ActionMessagesSend},
		{"agent.room_members.list", dirextalkmcp.ActionRoomMembersList},
		{"agent.channel_posts.list", dirextalkmcp.ActionChannelPostsList},
		{"agent.channel_comments.list", dirextalkmcp.ActionChannelCommentsList},
		{"agent.channel_comments.create", dirextalkmcp.ActionChannelCommentsCreate},
	} {
		t.Run(test.action, func(t *testing.T) {
			invoker.action = ""
			value, actionErr := handlers[test.action](context.Background(), map[string]any{})
			if actionErr != nil {
				t.Fatalf("invoke %s: %v", test.action, actionErr)
			}
			result := value.(map[string]any)
			if invoker.action != test.want || result["action"] != test.want {
				t.Fatalf("mapped to %q with result %#v, want %q", invoker.action, result, test.want)
			}
		})
	}
}

type recordingAccountPort struct {
	password      string
	session       MatrixSession
	sessionParams map[string]any
	config        dirextalkdomain.AgentConfig
	published     bool
}

func (p *recordingAccountPort) Password() string { return p.password }

func (p *recordingAccountPort) CreateMatrixSession(_ context.Context, params map[string]any) (MatrixSession, *actionbase.Error) {
	p.sessionParams = cloneMap(params)
	return p.session, nil
}

func (p *recordingAccountPort) Config() dirextalkdomain.AgentConfig { return p.config }

func (p *recordingAccountPort) UpdateConfig(_ context.Context, mutate func(dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig) (dirextalkdomain.AgentConfig, *actionbase.Error) {
	p.config = mutate(p.config)
	return p.config, nil
}

func (p *recordingAccountPort) PublishOffline(context.Context) *actionbase.Error {
	p.published = true
	return nil
}

func TestAccountHandlersPreserveSessionAndConfigContracts(t *testing.T) {
	accessToken := "agent-access-token"
	account := &recordingAccountPort{
		password: "portal-password",
		session: MatrixSession{
			AccessToken: &accessToken,
			DeviceID:    "AGENT_DEVICE",
			UserID:      "@agent:example.com",
			Homeserver:  "https://example.com",
		},
		config: dirextalkdomain.AgentConfig{
			DisplayName:   "Agent",
			ContextWindow: 30,
			Enabled:       true,
			Native:        map[string]any{"api_key": "must-not-return"},
		},
	}
	module := New(Config{Account: account})
	handlers := module.Handlers()

	password, actionErr := handlers["agent.password"](context.Background(), nil)
	if actionErr != nil || password.(map[string]any)["password"] != "portal-password" {
		t.Fatalf("agent.password = %#v, %v", password, actionErr)
	}

	session, actionErr := handlers["agent.matrix_session.create"](context.Background(), map[string]any{"device_id": "AGENT_DEVICE"})
	if actionErr != nil {
		t.Fatalf("agent.matrix_session.create: %v", actionErr)
	}
	if got := session.(map[string]any); got["access_token"] != "agent-access-token" || got["device_id"] != "AGENT_DEVICE" || got["user_id"] != "@agent:example.com" || got["homeserver"] != "https://example.com" {
		t.Fatalf("unexpected agent Matrix session: %#v", got)
	}
	if account.sessionParams["device_id"] != "AGENT_DEVICE" {
		t.Fatalf("expected full session params to reach account port, got %#v", account.sessionParams)
	}
	account.session.AccessToken = nil
	session, actionErr = handlers["agent.matrix_session.create"](context.Background(), nil)
	if actionErr != nil {
		t.Fatalf("agent.matrix_session.create without issuer: %v", actionErr)
	}
	if accessToken, exists := session.(map[string]any)["access_token"]; !exists || accessToken != nil {
		t.Fatalf("unconfigured issuer must preserve null access_token, got %#v", session)
	}

	updated, actionErr := handlers["agent.config.update"](context.Background(), map[string]any{
		"display_name":         " Ops Agent ",
		"avatar_url":           "",
		"context_window":       float64(64),
		"enabled":              false,
		"model":                " local-model ",
		"system_prompt":        " concise ",
		"mcp_blocked_room_ids": []any{"!secret:example.com", " !secret:example.com ", ""},
	})
	if actionErr != nil {
		t.Fatalf("agent.config.update: %v", actionErr)
	}
	config := updated.(map[string]any)
	if config["display_name"] != "Ops Agent" || config["enabled"] != false || config["model"] != "local-model" || config["system_prompt"] != "concise" {
		t.Fatalf("unexpected public config: %#v", config)
	}
	if _, found := config["api_key"]; found || !account.published {
		t.Fatalf("config must stay sanitized and disabling must publish offline: %#v published=%v", config, account.published)
	}
	blocked := config["mcp_blocked_room_ids"].([]string)
	if len(blocked) != 1 || blocked[0] != "!secret:example.com" {
		t.Fatalf("blocked rooms = %#v", blocked)
	}
}
