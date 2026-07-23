package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/sirupsen/logrus"
)

const voiceSessionTTL = time.Hour

var volcRTCIDPattern = regexp.MustCompile(`^[A-Za-z0-9@._-]{1,128}$`)

type voiceConfig struct {
	AppID                  string
	AppKey                 string
	VoiceChatAppID         string
	AIUserID               string
	WebhookSecret          string
	WebhookURL             string
	CustomLLMURL           string
	OpenAPIAccessKeyID     string
	OpenAPISecretAccessKey string
	OpenAPIHost            string
	OpenAPIRegion          string
	OpenAPIVersion         string
	VoiceChatConfigJSON    string
	TTSSpeaker             string
	TTSResourceID          string
	TTSSpeechRate          string
	TTSLoudnessRate        string
	TTSPitch               string
}

func voiceConfigFromEnv() voiceConfig {
	return voiceConfig{
		AppID:                  strings.TrimSpace(os.Getenv("VOLC_RTC_APP_ID")),
		AppKey:                 strings.TrimSpace(os.Getenv("VOLC_RTC_APP_KEY")),
		VoiceChatAppID:         strings.TrimSpace(os.Getenv("VOLC_VOICE_CHAT_APP_ID")),
		AIUserID:               strings.TrimSpace(os.Getenv("VOLC_RTC_AI_APP_ID")),
		WebhookSecret:          strings.TrimSpace(os.Getenv("VOLC_VOICE_WEBHOOK_SECRET")),
		WebhookURL:             strings.TrimSpace(os.Getenv("VOLC_VOICE_WEBHOOK_URL")),
		CustomLLMURL:           strings.TrimSpace(os.Getenv("VOLC_VOICE_CUSTOM_LLM_URL")),
		OpenAPIAccessKeyID:     strings.TrimSpace(os.Getenv("VOLC_ACCESS_KEY_ID")),
		OpenAPISecretAccessKey: strings.TrimSpace(os.Getenv("VOLC_SECRET_ACCESS_KEY")),
		OpenAPIHost:            strings.TrimSpace(os.Getenv("VOLC_RTC_OPENAPI_HOST")),
		OpenAPIRegion:          strings.TrimSpace(os.Getenv("VOLC_RTC_OPENAPI_REGION")),
		OpenAPIVersion:         strings.TrimSpace(os.Getenv("VOLC_VOICE_CHAT_OPENAPI_VERSION")),
		VoiceChatConfigJSON:    strings.TrimSpace(os.Getenv("VOLC_VOICE_CHAT_CONFIG_JSON")),
		TTSSpeaker:             strings.TrimSpace(os.Getenv("VOLC_VOICE_TTS_SPEAKER")),
		TTSResourceID:          strings.TrimSpace(os.Getenv("VOLC_VOICE_TTS_RESOURCE_ID")),
		TTSSpeechRate:          strings.TrimSpace(os.Getenv("VOLC_VOICE_TTS_SPEECH_RATE")),
		TTSLoudnessRate:        strings.TrimSpace(os.Getenv("VOLC_VOICE_TTS_LOUDNESS_RATE")),
		TTSPitch:               strings.TrimSpace(os.Getenv("VOLC_VOICE_TTS_PITCH")),
	}
}

type voiceCoordinator struct {
	mu       sync.Mutex
	cfg      voiceConfig
	signer   voiceTokenSigner
	client   voiceChatClient
	sessions map[string]*voiceSession
	streams  map[string]map[chan nativeagent.Event]struct{}
}

type voiceSession struct {
	SessionID      string
	TaskID         string
	AppID          string
	VoiceChatAppID string
	RoomID         string
	UserID         string
	Token          string
	AIUserID       string
	ExpiresAt      time.Time
	Params         map[string]any
	CustomLLM      bool
	Started        bool
	Ended          bool
	LastTranscript string
}

func newVoiceCoordinator(cfg voiceConfig) *voiceCoordinator {
	coordinator := &voiceCoordinator{
		cfg:      cfg,
		signer:   volcRTCTokenSigner{},
		sessions: map[string]*voiceSession{},
		streams:  map[string]map[chan nativeagent.Event]struct{}{},
	}
	if client := newVolcVoiceChatOpenAPIClient(cfg); client != nil {
		coordinator.client = client
	}
	return coordinator
}

func (m *Module) createVoiceSession(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent voice service is not configured")
	}
	return m.voice.create(ctx, params)
}

func (m *Module) startVoiceSession(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent voice service is not configured")
	}
	return m.voice.start(ctx, params)
}

func (m *Module) submitVoiceTranscript(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent voice service is not configured")
	}
	return m.submitVoiceTranscriptForSession(ctx, params)
}

func (m *Module) interruptVoiceSession(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent voice service is not configured")
	}
	return m.voice.interrupt(ctx, params)
}

func (m *Module) endVoiceSession(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent voice service is not configured")
	}
	return m.voice.end(ctx, params)
}

func (v *voiceCoordinator) create(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	source := actionbase.String(params["source"])
	if source == "" {
		source = "native_agent"
	}
	if source != "native_agent" {
		return nil, actionbase.BadRequest("source must be native_agent")
	}
	if strings.TrimSpace(v.cfg.AppID) == "" {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "volc_rtc_not_configured", "VOLC_RTC_APP_ID is required")
	}
	if strings.TrimSpace(v.cfg.AppKey) == "" {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "volc_rtc_app_key_not_configured", "VOLC_RTC_APP_KEY is required")
	}
	voiceChatAppID := fallback(v.cfg.VoiceChatAppID, v.cfg.AppID)
	if v.signer == nil {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "volc_rtc_token_signer_not_configured", "Volc RTC token signer is not configured")
	}
	if v.client == nil && (strings.TrimSpace(v.cfg.OpenAPIAccessKeyID) == "" || strings.TrimSpace(v.cfg.OpenAPISecretAccessKey) == "") {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "volc_openapi_not_configured", "VOLC_ACCESS_KEY_ID and VOLC_SECRET_ACCESS_KEY are required")
	}
	customLLM := true
	if customLLM {
		if strings.TrimSpace(v.cfg.WebhookSecret) == "" {
			return nil, actionbase.CodedError(http.StatusServiceUnavailable, "volc_voice_custom_llm_secret_not_configured", "VOLC_VOICE_WEBHOOK_SECRET is required for Volc CustomLLM")
		}
		if strings.TrimSpace(resolveCustomLLMURL(v.cfg.CustomLLMURL, v.cfg.WebhookURL)) == "" {
			return nil, actionbase.CodedError(http.StatusServiceUnavailable, "volc_voice_custom_llm_url_not_configured", "VOLC_VOICE_CUSTOM_LLM_URL or VOLC_VOICE_WEBHOOK_URL is required for Volc CustomLLM")
		}
	}
	sessionID := "voice_" + randomHex(12)
	roomID := "dirextalk_voice_" + randomHex(12)
	userID := "owner_" + randomHex(12)
	aiUserID := fallback(v.cfg.AIUserID, "dirextalk_ai_"+randomHex(8))
	if !volcRTCIDPattern.MatchString(roomID) || !volcRTCIDPattern.MatchString(userID) || !volcRTCIDPattern.MatchString(aiUserID) {
		return nil, actionbase.StatusError(http.StatusInternalServerError, "generated RTC identity is invalid")
	}
	expiresAt := time.Now().UTC().Add(voiceSessionTTL)
	token, err := v.signer.SignRTC(v.cfg.AppID, v.cfg.AppKey, roomID, userID, expiresAt)
	if err != nil {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "volc_rtc_token_sign_failed", err.Error())
	}
	session := &voiceSession{
		SessionID:      sessionID,
		TaskID:         sessionID,
		AppID:          v.cfg.AppID,
		VoiceChatAppID: voiceChatAppID,
		RoomID:         roomID,
		UserID:         userID,
		Token:          token,
		AIUserID:       aiUserID,
		ExpiresAt:      expiresAt,
		Params:         cloneMap(params),
		CustomLLM:      customLLM,
	}
	v.mu.Lock()
	v.sessions[sessionID] = session
	v.mu.Unlock()
	v.emit(sessionID, nativeagent.Event{Event: "listening", Data: map[string]any{"status": "listening"}})
	return session.response(), nil
}

func (v *voiceCoordinator) start(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	sessionID := actionbase.String(params["session_id"])
	if sessionID == "" {
		return nil, actionbase.BadRequest("session_id is required")
	}
	v.mu.Lock()
	session, ok := v.sessions[sessionID]
	if !ok || session.Ended {
		v.mu.Unlock()
		return nil, actionbase.StatusError(http.StatusNotFound, "voice session not found")
	}
	if session.Started {
		v.mu.Unlock()
		return map[string]any{"ok": true, "session_id": sessionID, "started": true, "already_started": true}, nil
	}
	session.Started = true
	sessionCopy := *session
	sessionCopy.Params = cloneMap(session.Params)
	v.mu.Unlock()
	if v.client != nil {
		logrus.WithFields(logrus.Fields{
			"component":         "native_agent_voice",
			"action":            "StartVoiceChat",
			"session_id":        sessionCopy.SessionID,
			"task_id":           sessionCopy.TaskID,
			"rtc_app_id":        sessionCopy.AppID,
			"voice_chat_app_id": sessionCopy.VoiceChatAppID,
			"room_id":           sessionCopy.RoomID,
			"user_id":           sessionCopy.UserID,
			"ai_user_id":        sessionCopy.AIUserID,
		}).Info("starting Volc VoiceChat session")
		if err := v.client.StartVoiceChat(ctx, sessionCopy); err != nil {
			v.mu.Lock()
			if current := v.sessions[sessionID]; current != nil {
				current.Started = false
			}
			v.mu.Unlock()
			logrus.WithError(err).WithFields(logrus.Fields{
				"component":         "native_agent_voice",
				"action":            "StartVoiceChat",
				"session_id":        sessionCopy.SessionID,
				"task_id":           sessionCopy.TaskID,
				"rtc_app_id":        sessionCopy.AppID,
				"voice_chat_app_id": sessionCopy.VoiceChatAppID,
				"room_id":           sessionCopy.RoomID,
				"user_id":           sessionCopy.UserID,
				"ai_user_id":        sessionCopy.AIUserID,
			}).Warn("Volc VoiceChat session failed to start")
			return nil, actionbase.CodedError(http.StatusBadGateway, "volc_voice_chat_start_failed", err.Error())
		}
		logrus.WithFields(logrus.Fields{
			"component":         "native_agent_voice",
			"action":            "StartVoiceChat",
			"session_id":        sessionCopy.SessionID,
			"task_id":           sessionCopy.TaskID,
			"rtc_app_id":        sessionCopy.AppID,
			"voice_chat_app_id": sessionCopy.VoiceChatAppID,
			"room_id":           sessionCopy.RoomID,
		}).Info("Volc VoiceChat session started")
	}
	v.emit(sessionID, nativeagent.Event{Event: "listening", Data: map[string]any{"status": "listening"}})
	return map[string]any{"ok": true, "session_id": sessionID, "started": true}, nil
}

func (m *Module) HandleVoiceWebhook(ctx context.Context, token string, params map[string]any) (map[string]any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent voice service is not configured")
	}
	if err := m.AuthorizeVoiceWebhook(token); err != nil {
		return nil, err
	}
	sessionID := actionbase.String(params["session_id"])
	if sessionID == "" {
		return nil, actionbase.BadRequest("session_id is required")
	}
	_, ok := m.voice.session(sessionID)
	if !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "voice session not found")
	}
	status := actionbase.String(params["status"])
	if status == "" {
		status = actionbase.String(params["event"])
	}
	data := map[string]any{}
	for _, key := range []string{"status", "transcript_delta", "transcript_final", "answer_delta", "summary", "references", "volume", "error"} {
		if value, exists := params[key]; exists {
			data[key] = value
		}
	}
	if status != "" {
		data["status"] = status
	}
	if len(data) > 0 {
		m.voice.emit(sessionID, nativeagent.Event{Event: fallback(status, "message"), Data: data})
	}
	transcript := strings.TrimSpace(actionbase.String(params["transcript_final"]))
	if transcript != "" {
		logrus.WithFields(voiceTranscriptLogFields("webhook", sessionID, transcript, nil)).Info("native agent voice transcript received")
		if session, accepted, reason := m.voice.acceptTranscript(sessionID, transcript); accepted {
			logrus.WithFields(voiceTranscriptLogFields("webhook", sessionID, transcript, session.Params)).Info("native agent voice transcript accepted")
			go m.runVoiceAgent(context.Background(), session, transcript)
		} else {
			fields := voiceTranscriptLogFields("webhook", sessionID, transcript, nil)
			fields["reason"] = reason
			logrus.WithFields(fields).Info("native agent voice transcript ignored")
		}
	}
	return map[string]any{"ok": true, "session_id": sessionID}, nil
}

type VoiceCustomLLMChunkEmitter func(string) error

func (m *Module) HandleVoiceCustomLLM(ctx context.Context, token, sessionID string, params map[string]any, emit VoiceCustomLLMChunkEmitter) (map[string]any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent voice service is not configured")
	}
	if err := m.AuthorizeVoiceWebhook(token); err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(fallback(sessionID, actionbase.String(params["session_id"])))
	if sessionID == "" {
		sessionID = m.voice.findSessionID(params)
	}
	if sessionID == "" {
		return nil, actionbase.BadRequest("session_id is required")
	}
	transcript := extractVoiceCustomLLMTranscript(params)
	if transcript == "" {
		return nil, actionbase.BadRequest("CustomLLM request does not contain user transcript")
	}
	logrus.WithFields(voiceTranscriptLogFields("volc_custom_llm", sessionID, transcript, nil)).Info("volc_asr_received")
	m.voice.emit(sessionID, nativeagent.Event{Event: "transcribing", Data: map[string]any{"status": "transcribing", "transcript_final": transcript}})
	session, accepted, reason := m.voice.acceptTranscript(sessionID, transcript)
	if !accepted {
		fields := voiceTranscriptLogFields("volc_custom_llm", sessionID, transcript, nil)
		fields["reason"] = reason
		logrus.WithFields(fields).Info("native agent voice transcript ignored")
		return map[string]any{"ok": true, "session_id": sessionID, "accepted": false, "reason": reason, "text": ""}, nil
	}
	logrus.WithFields(voiceTranscriptLogFields("volc_custom_llm", sessionID, transcript, session.Params)).Info("native agent voice transcript accepted")
	result := m.runVoiceAgentForTTS(ctx, session, transcript, emit)
	if result.Err != nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, result.Err.Error())
	}
	return map[string]any{
		"ok":               true,
		"session_id":       sessionID,
		"accepted":         true,
		"text":             result.Text,
		"references_count": result.ReferencesCount,
		"tool_calls_count": result.ToolCallsCount,
	}, nil
}

func (m *Module) AuthorizeVoiceWebhook(token string) *actionbase.Error {
	if m == nil || m.voice == nil {
		return actionbase.StatusError(http.StatusBadGateway, "native agent voice service is not configured")
	}
	if strings.TrimSpace(m.voice.cfg.WebhookSecret) == "" {
		return actionbase.CodedError(http.StatusServiceUnavailable, "volc_voice_webhook_not_configured", "VOLC_VOICE_WEBHOOK_SECRET is required")
	}
	if token == "" || token != m.voice.cfg.WebhookSecret {
		return actionbase.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	}
	return nil
}

func (m *Module) submitVoiceTranscriptForSession(ctx context.Context, params map[string]any) (map[string]any, *actionbase.Error) {
	sessionID := actionbase.String(params["session_id"])
	if sessionID == "" {
		return nil, actionbase.BadRequest("session_id is required")
	}
	_, ok := m.voice.session(sessionID)
	if !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "voice session not found")
	}
	delta := strings.TrimSpace(actionbase.String(params["transcript_delta"]))
	final := strings.TrimSpace(actionbase.String(params["transcript_final"]))
	if delta == "" && final == "" {
		return nil, actionbase.BadRequest("transcript_delta or transcript_final is required")
	}
	data := map[string]any{"status": "transcribing"}
	if delta != "" {
		data["transcript_delta"] = delta
	}
	if final != "" {
		data["transcript_final"] = final
	}
	m.voice.emit(sessionID, nativeagent.Event{Event: "transcribing", Data: data})
	if final != "" {
		logrus.WithFields(voiceTranscriptLogFields("action", sessionID, final, nil)).Info("native agent voice transcript received")
		if session, accepted, reason := m.voice.acceptTranscript(sessionID, final); accepted {
			logrus.WithFields(voiceTranscriptLogFields("action", sessionID, final, session.Params)).Info("native agent voice transcript accepted")
			go m.runVoiceAgent(context.Background(), session, final)
		} else {
			fields := voiceTranscriptLogFields("action", sessionID, final, nil)
			fields["reason"] = reason
			logrus.WithFields(fields).Info("native agent voice transcript ignored")
		}
	}
	_ = ctx
	return map[string]any{"ok": true, "session_id": sessionID, "accepted": true}, nil
}

func (m *Module) runVoiceAgent(ctx context.Context, session voiceSession, transcript string) {
	result := m.runVoiceAgentForTTS(ctx, session, transcript, nil)
	if result.Err != nil {
		logrus.WithError(result.Err).WithFields(logrus.Fields{
			"component":  "native_agent_voice",
			"session_id": session.SessionID,
		}).Warn("native agent voice stream failed")
		m.voice.emit(session.SessionID, nativeagent.Event{Event: "error", Data: map[string]any{"status": "error", "error": result.Err.Error()}})
	}
}

type voiceAgentRunResult struct {
	Text            string
	ReferencesCount int
	ToolCallsCount  int
	Err             error
}

func (m *Module) runVoiceAgentForTTS(ctx context.Context, session voiceSession, transcript string, ttsEmit VoiceCustomLLMChunkEmitter) voiceAgentRunResult {
	result := voiceAgentRunResult{}
	var answerBuilder strings.Builder
	if m == nil || m.runner == nil || m.voice == nil {
		result.Err = fmt.Errorf("native agent runtime is not configured")
		return result
	}
	m.voice.emit(session.SessionID, nativeagent.Event{Event: "thinking", Data: map[string]any{"status": "thinking", "transcript_final": transcript}})
	params := cloneMap(session.Params)
	params["prompt"] = transcript
	delete(params, "source")
	logrus.WithFields(voiceTranscriptLogFields("native_agent_stream", session.SessionID, transcript, params)).Info("native_agent_started")
	err := m.runner.Stream(ctx, "agent.chat.stream", params, func(event nativeagent.Event) error {
		switch event.Event {
		case "delta":
			text := actionbase.String(event.Data["text"])
			if text == "" {
				return nil
			}
			answerBuilder.WriteString(text)
			if ttsEmit != nil {
				if err := ttsEmit(text); err != nil {
					return err
				}
			}
			logrus.WithFields(logrus.Fields{
				"component":     "native_agent_voice",
				"session_id":    session.SessionID,
				"event":         "delta",
				"answer_length": len([]rune(text)),
			}).Debug("native agent voice stream delta")
			m.voice.emit(session.SessionID, nativeagent.Event{Event: "answer", Data: map[string]any{"status": "speaking", "answer_delta": text}})
		case "done":
			data := map[string]any{"status": "done"}
			result.Text = strings.TrimSpace(actionbase.String(event.Data["text"]))
			if result.Text == "" {
				result.Text = strings.TrimSpace(answerBuilder.String())
			}
			if result.Text != "" {
				data["summary"] = result.Text
			}
			if refs, ok := event.Data["references"]; ok {
				data["references"] = refs
			}
			result.ReferencesCount = countAnyList(event.Data["references"])
			result.ToolCallsCount = countAnyList(event.Data["tool_calls"])
			logrus.WithFields(logrus.Fields{
				"component":        "native_agent_voice",
				"session_id":       session.SessionID,
				"event":            "done",
				"summary_length":   len([]rune(result.Text)),
				"references_count": result.ReferencesCount,
				"tool_calls_count": result.ToolCallsCount,
			}).Info("native_agent_done")
			m.voice.emit(session.SessionID, nativeagent.Event{Event: "done", Data: data})
		case "error":
			data := map[string]any{"status": "error", "error": actionbase.String(event.Data["error"])}
			logrus.WithFields(logrus.Fields{
				"component":  "native_agent_voice",
				"session_id": session.SessionID,
				"event":      "error",
				"error":      actionbase.String(event.Data["error"]),
			}).Warn("native agent voice stream error")
			m.voice.emit(session.SessionID, nativeagent.Event{Event: "error", Data: data})
		}
		return nil
	})
	if err != nil {
		result.Err = err
	}
	if ttsEmit != nil {
		logrus.WithFields(logrus.Fields{
			"component":        "native_agent_voice",
			"session_id":       session.SessionID,
			"summary_length":   len([]rune(result.Text)),
			"references_count": result.ReferencesCount,
			"tool_calls_count": result.ToolCallsCount,
		}).Info("volc_tts_returned")
	}
	return result
}

func (v *voiceCoordinator) interrupt(_ context.Context, params map[string]any) (any, *actionbase.Error) {
	sessionID := actionbase.String(params["session_id"])
	if sessionID == "" {
		return nil, actionbase.BadRequest("session_id is required")
	}
	if !v.exists(sessionID) {
		return nil, actionbase.StatusError(http.StatusNotFound, "voice session not found")
	}
	v.emit(sessionID, nativeagent.Event{Event: "listening", Data: map[string]any{"status": "listening"}})
	return map[string]any{"ok": true, "session_id": sessionID, "interrupted": true}, nil
}

func (v *voiceCoordinator) end(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	sessionID := actionbase.String(params["session_id"])
	if sessionID == "" {
		return nil, actionbase.BadRequest("session_id is required")
	}
	v.mu.Lock()
	session, ok := v.sessions[sessionID]
	if ok {
		session.Ended = true
		delete(v.sessions, sessionID)
	}
	streams := v.streams[sessionID]
	delete(v.streams, sessionID)
	v.mu.Unlock()
	for ch := range streams {
		ch <- nativeagent.Event{Event: "done", Data: map[string]any{"status": "done"}}
		close(ch)
	}
	if ok && v.client != nil {
		logrus.WithFields(logrus.Fields{
			"component":         "native_agent_voice",
			"action":            "StopVoiceChat",
			"session_id":        session.SessionID,
			"task_id":           session.TaskID,
			"rtc_app_id":        session.AppID,
			"voice_chat_app_id": session.VoiceChatAppID,
			"room_id":           session.RoomID,
		}).Info("stopping Volc VoiceChat session")
		if err := v.client.StopVoiceChat(ctx, *session); err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"component":         "native_agent_voice",
				"action":            "StopVoiceChat",
				"session_id":        session.SessionID,
				"task_id":           session.TaskID,
				"rtc_app_id":        session.AppID,
				"voice_chat_app_id": session.VoiceChatAppID,
				"room_id":           session.RoomID,
			}).Warn("Volc VoiceChat session failed to stop")
		}
	}
	return map[string]any{"ok": true, "session_id": sessionID, "ended": ok}, nil
}

func (v *voiceCoordinator) stream(ctx context.Context, params map[string]any, emit func(nativeagent.Event) error) error {
	sessionID := actionbase.String(params["session_id"])
	if sessionID == "" {
		return emit(nativeagent.Event{Event: "error", Data: map[string]any{"status": "error", "error": "session_id is required"}})
	}
	ch := make(chan nativeagent.Event, 16)
	v.mu.Lock()
	session, ok := v.sessions[sessionID]
	if ok && !session.Ended {
		if v.streams[sessionID] == nil {
			v.streams[sessionID] = map[chan nativeagent.Event]struct{}{}
		}
		v.streams[sessionID][ch] = struct{}{}
	}
	v.mu.Unlock()
	if !ok || session.Ended {
		return emit(nativeagent.Event{Event: "error", Data: map[string]any{"status": "error", "error": "voice session not found"}})
	}
	defer func() {
		v.mu.Lock()
		if streams := v.streams[sessionID]; streams != nil {
			delete(streams, ch)
			if len(streams) == 0 {
				delete(v.streams, sessionID)
			}
		}
		v.mu.Unlock()
	}()
	if err := emit(nativeagent.Event{Event: "listening", Data: map[string]any{"status": "listening"}}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-ch:
			if !ok {
				return nil
			}
			if err := emit(event); err != nil {
				return err
			}
			if event.Event == "error" {
				return nil
			}
		}
	}
}

func (v *voiceCoordinator) exists(sessionID string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	session, ok := v.sessions[sessionID]
	return ok && !session.Ended
}

func (v *voiceCoordinator) session(sessionID string) (voiceSession, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	session, ok := v.sessions[sessionID]
	if !ok || session.Ended {
		return voiceSession{}, false
	}
	copy := *session
	copy.Params = cloneMap(session.Params)
	return copy, true
}

func (v *voiceCoordinator) findSessionID(params map[string]any) string {
	taskID := strings.TrimSpace(fallback(actionbase.String(params["TaskId"]), actionbase.String(params["task_id"])))
	roomID := strings.TrimSpace(fallback(actionbase.String(params["RoomId"]), actionbase.String(params["room_id"])))
	userID := strings.TrimSpace(fallback(actionbase.String(params["UserId"]), actionbase.String(params["user_id"])))
	v.mu.Lock()
	defer v.mu.Unlock()
	for sessionID, session := range v.sessions {
		if session == nil || session.Ended {
			continue
		}
		if taskID != "" && session.TaskID == taskID {
			return sessionID
		}
		if roomID != "" && session.RoomID == roomID {
			if userID == "" || session.UserID == userID || session.AIUserID == userID {
				return sessionID
			}
		}
	}
	return ""
}

func (v *voiceCoordinator) acceptTranscript(sessionID, transcript string) (voiceSession, bool, string) {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return voiceSession{}, false, "empty"
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	session, ok := v.sessions[sessionID]
	if !ok || session.Ended {
		return voiceSession{}, false, "session_not_found"
	}
	if session.LastTranscript == transcript {
		return voiceSession{}, false, "duplicate"
	}
	session.LastTranscript = transcript
	copy := *session
	copy.Params = cloneMap(session.Params)
	return copy, true, "accepted"
}

func (v *voiceCoordinator) emit(sessionID string, event nativeagent.Event) {
	v.mu.Lock()
	streams := make([]chan nativeagent.Event, 0, len(v.streams[sessionID]))
	for ch := range v.streams[sessionID] {
		streams = append(streams, ch)
	}
	v.mu.Unlock()
	for _, ch := range streams {
		select {
		case ch <- event:
		default:
		}
	}
}

func voiceTranscriptLogFields(source, sessionID, transcript string, params map[string]any) logrus.Fields {
	fields := logrus.Fields{
		"component":          "native_agent_voice",
		"source":             source,
		"session_id":         sessionID,
		"transcript_length":  len([]rune(transcript)),
		"transcript_preview": previewRunes(transcript, 48),
	}
	if params != nil {
		fields["conversation_id"] = actionbase.String(params["conversation_id"])
		fields["room_id"] = actionbase.String(params["room_id"])
		fields["room_type"] = actionbase.String(params["room_type"])
		fields["has_model_profile"] = params["model_profile"] != nil || actionbase.String(params["model_profile_id"]) != ""
		fields["has_api_key"] = actionbase.String(params["api_key"]) != ""
	}
	return fields
}

func countAnyList(value any) int {
	switch typed := value.(type) {
	case []any:
		return len(typed)
	case []map[string]any:
		return len(typed)
	case []nativeagent.Event:
		return len(typed)
	default:
		return 0
	}
}

func extractVoiceCustomLLMTranscript(params map[string]any) string {
	for _, key := range []string{"transcript_final", "transcript", "prompt", "query", "input", "text"} {
		if text := strings.TrimSpace(actionbase.String(params[key])); text != "" {
			return text
		}
	}
	if messages, ok := params["messages"].([]any); ok {
		for index := len(messages) - 1; index >= 0; index-- {
			message, _ := messages[index].(map[string]any)
			if message == nil {
				continue
			}
			role := strings.TrimSpace(actionbase.String(message["role"]))
			if role != "" && role != "user" {
				continue
			}
			if text := extractVoiceCustomLLMContent(message["content"]); text != "" {
				return text
			}
		}
	}
	return ""
}

func extractVoiceCustomLLMContent(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			switch content := item.(type) {
			case string:
				if text := strings.TrimSpace(content); text != "" {
					parts = append(parts, text)
				}
			case map[string]any:
				for _, key := range []string{"text", "content"} {
					if text := strings.TrimSpace(actionbase.String(content[key])); text != "" {
						parts = append(parts, text)
						break
					}
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case map[string]any:
		for _, key := range []string{"text", "content"} {
			if text := strings.TrimSpace(actionbase.String(typed[key])); text != "" {
				return text
			}
		}
	}
	return ""
}

func previewRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func (s voiceSession) response() map[string]any {
	return map[string]any{
		"session_id":                       s.SessionID,
		"task_id":                          s.TaskID,
		"app_id":                           s.AppID,
		"room_id":                          s.RoomID,
		"user_id":                          s.UserID,
		"token":                            s.Token,
		"expires_at":                       s.ExpiresAt.Unix(),
		"ai_user_id":                       s.AIUserID,
		"client_transcript_submit_enabled": !s.CustomLLM,
	}
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	}
	return hex.EncodeToString(buf)
}

func fallback(value, defaultValue string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return defaultValue
}
