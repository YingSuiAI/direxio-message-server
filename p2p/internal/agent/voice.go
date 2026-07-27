package agent

// Native Agent voice sessions are deliberately a thin server-owned boundary.
// Volc credentials are read from the encrypted speech profile only while a
// session is being created/started; they never enter a request/response or
// websocket event.
import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

const voiceSessionTTL = time.Hour

type voiceRuntimeConfig struct {
	WebhookURL, WebhookSecret, CustomLLMURL string
	ClientTranscriptSubmit                  bool
}

func voiceRuntimeConfigFromEnv() voiceRuntimeConfig {
	return voiceRuntimeConfig{
		WebhookURL:             strings.TrimSpace(os.Getenv("VOLC_VOICE_WEBHOOK_URL")),
		WebhookSecret:          strings.TrimSpace(os.Getenv("VOLC_VOICE_WEBHOOK_SECRET")),
		CustomLLMURL:           strings.TrimSpace(os.Getenv("VOLC_VOICE_CUSTOM_LLM_URL")),
		ClientTranscriptSubmit: strings.EqualFold(strings.TrimSpace(os.Getenv("VOLC_VOICE_CLIENT_TRANSCRIPT_SUBMIT_ENABLED")), "true"),
	}
}

type voiceSession struct {
	SessionID, OwnerID, ConversationID                     string
	ConversationProfileID, SpeechProfileID                 string
	ConversationRevision, ConversationCredential           int64
	SpeechRevision, SpeechCredential                       int64
	AppID, VoiceChatAppID, AIUserID, RoomID, UserID, Token string
	CallbackToken                                          string
	SpeechProviderConfig                                   map[string]any
	ExpiresAt                                              time.Time
	Started, Ended                                         bool
	State                                                  string
	ActiveTurnID                                           string
	TurnSequence                                           int64
	Generation                                             uint64
	Busy                                                   bool
	op                                                     *sync.Mutex
}

type voiceCoordinator struct {
	mu         sync.Mutex
	profiles   storage.ModelProfileStore
	owner      func() string
	cfg        voiceRuntimeConfig
	signer     voiceTokenSigner
	sessions   map[string]*voiceSession
	streams    map[string]map[chan nativeagent.Event]struct{}
	durable    func(context.Context, string, map[string]any, func(agentturns.StreamEvent) error) error
	stop       func(context.Context, string, string) error
	active     func(string) bool
	generation func() uint64
}

func (m *Module) VoiceReady() bool {
	if m == nil || m.voice == nil || m.voice.profiles == nil || m.voice.cfg.WebhookSecret == "" {
		return false
	}
	raw := m.voice.cfg.CustomLLMURL
	if raw == "" {
		raw = m.voice.cfg.WebhookURL
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

func (m *Module) AbortVoiceSessions(ctx context.Context) {
	if m == nil || m.voice == nil {
		return
	}
	m.voice.mu.Lock()
	sessions := make([]*voiceSession, 0)
	streams := make([]map[chan nativeagent.Event]struct{}, 0)
	for id, s := range m.voice.sessions {
		if s != nil && !s.Ended {
			cp := *s
			sessions = append(sessions, &cp)
			s.Ended = true
			s.State = "ended"
			streams = append(streams, m.voice.streams[id])
			delete(m.voice.streams, id)
		}
	}
	m.voice.mu.Unlock()
	for _, set := range streams {
		for ch := range set {
			select {
			case ch <- nativeagent.Event{Event: "session.done", Data: map[string]any{"status": "done", "session_ended": true}}:
			default:
			}
			close(ch)
		}
	}
	for _, s := range sessions {
		if client, err := m.voice.providerClient(ctx, s.OwnerID, s); err == nil && client != nil {
			_ = client.StopVoiceChat(ctx, *s)
		}
	}
}

func newVoiceCoordinator(profiles storage.ModelProfileStore, owner func() string) *voiceCoordinator {
	v := &voiceCoordinator{profiles: profiles, owner: owner, cfg: voiceRuntimeConfigFromEnv(), signer: volcRTCTokenSigner{}, sessions: map[string]*voiceSession{}, streams: map[string]map[chan nativeagent.Event]struct{}{}}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			v.cleanupExpired(context.Background())
		}
	}()
	return v
}

type voiceTokenSigner interface {
	SignRTC(string, string, string, string, time.Time) (string, error)
}

type volcRTCTokenSigner struct{}

func (volcRTCTokenSigner) SignRTC(appID, appKey, roomID, userID string, expiry time.Time) (string, error) {
	appID, appKey, roomID, userID = strings.TrimSpace(appID), strings.TrimSpace(appKey), strings.TrimSpace(roomID), strings.TrimSpace(userID)
	if appID == "" || appKey == "" || roomID == "" || userID == "" {
		return "", fmt.Errorf("Volc RTC credentials and identities are required")
	}
	if len(appID) != 24 {
		return "", fmt.Errorf("Volc RTC app id must be 24 characters")
	}
	if expiry.Before(time.Now().UTC()) || expiry.Unix() > int64(^uint32(0)) {
		return "", fmt.Errorf("Volc RTC expiry is invalid")
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	issued, expires := uint32(time.Now().Unix()), uint32(expiry.Unix())
	b := make([]byte, 0, 256)
	for _, n := range []uint32{binary.LittleEndian.Uint32(nonce[:]), issued, expires} {
		var x [4]byte
		binary.LittleEndian.PutUint32(x[:], n)
		b = append(b, x[:]...)
	}
	for _, s := range []string{roomID, userID} {
		if len(s) > 65535 {
			return "", fmt.Errorf("identity too long")
		}
		var x [2]byte
		binary.LittleEndian.PutUint16(x[:], uint16(len(s)))
		b = append(b, x[:]...)
		b = append(b, s...)
	}
	var count [2]byte
	binary.LittleEndian.PutUint16(count[:], 2)
	b = append(b, count[:]...)
	for _, privilege := range []uint16{1, 2} {
		var x [6]byte
		binary.LittleEndian.PutUint16(x[:2], privilege)
		binary.LittleEndian.PutUint32(x[2:], expires)
		b = append(b, x[:]...)
	}
	mac := hmac.New(sha256.New, []byte(appKey))
	_, _ = mac.Write(b)
	sig := mac.Sum(nil)
	wrap := func(s []byte) []byte {
		var x [2]byte
		binary.LittleEndian.PutUint16(x[:], uint16(len(s)))
		return append(x[:], s...)
	}
	content := append(wrap(b), wrap(sig)...)
	return "001" + appID + base64.StdEncoding.EncodeToString(content), nil
}

func (m *Module) createVoiceSession(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "native agent voice service is not configured")
	}
	return m.voice.create(ctx, m.currentOwnerID(), params)
}
func (m *Module) startVoiceSession(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "native agent voice service is not configured")
	}
	return m.voice.start(ctx, m.currentOwnerID(), params)
}
func (m *Module) submitVoiceTranscript(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "native agent voice service is not configured")
	}
	return m.voice.transcript(ctx, m.currentOwnerID(), params)
}
func (m *Module) interruptVoiceSession(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "native agent voice service is not configured")
	}
	return m.voice.interrupt(ctx, m.currentOwnerID(), params)
}
func (m *Module) endVoiceSession(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.voice == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "native agent voice service is not configured")
	}
	return m.voice.end(ctx, m.currentOwnerID(), params)
}

func (v *voiceCoordinator) resolve(ctx context.Context, owner, id, kind string) (storage.ModelProfile, error) {
	if v.profiles == nil {
		return storage.ModelProfile{}, fmt.Errorf("server model profiles are unavailable")
	}
	if strings.TrimSpace(id) == "" {
		list, err := v.profiles.ListModelProfiles(ctx, owner, 100, "")
		if err != nil {
			return storage.ModelProfile{}, err
		}
		if kind == storage.ModelKindSpeech {
			id = list.Defaults.SpeechClientProfileID
		} else {
			id = list.Defaults.ConversationClientProfileID
		}
		if id == "" {
			return storage.ModelProfile{}, fmt.Errorf("no default %s model profile", kind)
		}
		var found storage.ModelProfile
		for _, p := range list.Profiles {
			if p.ClientProfileID == id {
				found = p
				break
			}
		}
		if found.ProfileID == "" {
			return storage.ModelProfile{}, storage.ErrModelProfileNotFound
		}
		return v.profiles.ResolveModelProfilePin(ctx, owner, found.ProfileID)
	}
	p, ok, err := v.profiles.GetModelProfile(ctx, owner, id)
	if err != nil {
		return storage.ModelProfile{}, err
	}
	if !ok {
		return storage.ModelProfile{}, storage.ErrModelProfileNotFound
	}
	if !p.Deleted && p.ProfileID == "" {
		return storage.ModelProfile{}, storage.ErrModelProfileNotFound
	}
	if p.ModelKind == "" {
		p.ModelKind = storage.ModelKindConversation
	}
	if p.ModelKind != kind {
		return storage.ModelProfile{}, fmt.Errorf("model profile kind must be %s", kind)
	}
	return v.profiles.ResolveModelProfilePin(ctx, owner, p.ProfileID)
}

func (v *voiceCoordinator) providerClient(ctx context.Context, owner string, s *voiceSession) (voiceChatClient, *actionbase.Error) {
	profile, err := v.profiles.ResolveModelProfilePinned(ctx, owner, s.SpeechProfileID, s.SpeechRevision, s.SpeechCredential)
	if err != nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "speech profile credentials are unavailable")
	}
	secrets := map[string]string{}
	if json.Unmarshal([]byte(profile.APIKey), &secrets) != nil || secrets["access_key_id"] == "" || secrets["secret_access_key"] == "" {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "voice_openapi_not_configured", "speech profile OpenAPI credentials are required")
	}
	return newVolcVoiceChatClient(secrets["access_key_id"], secrets["secret_access_key"], v.cfg), nil
}

func profileID(params map[string]any, key string) string {
	return strings.TrimSpace(actionbase.String(params[key]))
}

func (v *voiceCoordinator) create(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	v.cleanupExpired(ctx)
	if v.active != nil && !v.active(owner) {
		return nil, actionbase.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	}
	conversationID := strings.TrimSpace(actionbase.String(params["conversation_id"]))
	if conversationID == "" {
		return nil, actionbase.BadRequest("conversation_id is required")
	}
	callbackURL := v.cfg.CustomLLMURL
	if callbackURL == "" {
		callbackURL = v.cfg.WebhookURL
	}
	parsedCallback, callbackErr := url.Parse(callbackURL)
	if v.cfg.WebhookSecret == "" || callbackErr != nil || parsedCallback.Scheme != "https" || parsedCallback.Host == "" {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "voice_callback_not_configured", "voice callback configuration is required")
	}
	conv, err := v.resolve(ctx, owner, profileID(params, "conversation_profile_id"), storage.ModelKindConversation)
	if err != nil {
		return nil, actionbase.StatusError(http.StatusBadRequest, err.Error())
	}
	speech, err := v.resolve(ctx, owner, profileID(params, "speech_profile_id"), storage.ModelKindSpeech)
	if err != nil {
		return nil, actionbase.StatusError(http.StatusBadRequest, err.Error())
	}
	if speech.Provider != "volc_voice" {
		return nil, actionbase.BadRequest("speech profile provider must be volc_voice")
	}
	// ResolveModelProfilePinned is the only call that decrypts the write-only
	// speech credential bundle, and the returned value is kept request-local.
	speech, err = v.profiles.ResolveModelProfilePinned(ctx, owner, speech.ProfileID, speech.Revision, speech.CredentialVersion)
	if err != nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "speech profile credentials are unavailable")
	}
	// The encrypted credential is deliberately read only to mint the short-lived RTC token.
	secrets := map[string]string{}
	if err := json.Unmarshal([]byte(speech.APIKey), &secrets); err != nil || secrets["rtc_app_key"] == "" {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "voice_rtc_not_configured", "speech profile rtc_app_key is required")
	}
	appID := stringConfig(speech.ProviderConfig, "app_id")
	if appID == "" {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "voice_rtc_not_configured", "speech profile app_id is required")
	}
	sid := "voice_" + randomVoiceHex(12)
	room := "dirextalk_voice_" + randomVoiceHex(12)
	uid := "owner_" + randomVoiceHex(12)
	if strings.HasSuffix(sid, "_") || strings.HasSuffix(room, "_") || strings.HasSuffix(uid, "_") {
		return nil, actionbase.StatusError(http.StatusInternalServerError, "voice identity generation failed")
	}
	ai := stringConfig(speech.ProviderConfig, "ai_user_id")
	if ai == "" {
		ai = "dirextalk_ai_" + randomVoiceHex(8)
	}
	if strings.HasSuffix(ai, "_") {
		return nil, actionbase.StatusError(http.StatusInternalServerError, "voice identity generation failed")
	}
	expiry := time.Now().UTC().Add(voiceSessionTTL)
	token, err := v.signer.SignRTC(appID, secrets["rtc_app_key"], room, uid, expiry)
	if err != nil {
		return nil, actionbase.CodedError(http.StatusServiceUnavailable, "voice_rtc_token_failed", err.Error())
	}
	generation := uint64(0)
	if v.generation != nil {
		generation = v.generation()
		if generation == 0 {
			return nil, actionbase.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
		}
	}
	s := &voiceSession{SessionID: sid, OwnerID: owner, Generation: generation, ConversationID: conversationID, ConversationProfileID: conv.ProfileID, SpeechProfileID: speech.ProfileID, ConversationRevision: conv.Revision, ConversationCredential: conv.CredentialVersion, SpeechRevision: speech.Revision, SpeechCredential: speech.CredentialVersion, AppID: appID, VoiceChatAppID: fallback(stringConfig(speech.ProviderConfig, "voice_chat_app_id"), appID), AIUserID: ai, RoomID: room, UserID: uid, Token: token, CallbackToken: v.callbackToken(sid, expiry), ExpiresAt: expiry, State: "created", SpeechProviderConfig: cloneMap(speech.ProviderConfig), op: &sync.Mutex{}}
	v.mu.Lock()
	v.sessions[sid] = s
	v.mu.Unlock()
	return map[string]any{"ok": true, "session_id": sid, "conversation_id": conversationID, "token": token, "app_id": appID, "voice_chat_app_id": s.VoiceChatAppID, "room_id": room, "user_id": uid, "ai_user_id": ai, "expires_at": expiry.Format(time.RFC3339), "conversation_profile_id": conv.ProfileID, "speech_profile_id": speech.ProfileID, "client_transcript_submit_enabled": v.cfg.ClientTranscriptSubmit}, nil
}

func (v *voiceCoordinator) callbackToken(sessionID string, expiry ...time.Time) string {
	mac := hmac.New(sha256.New, []byte(v.cfg.WebhookSecret))
	_, _ = mac.Write([]byte(sessionID))
	if len(expiry) > 0 {
		_, _ = mac.Write([]byte(fmt.Sprintf("|%d", expiry[0].Unix())))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// AuthorizeVoiceCallback authenticates a deployment callback and binds it to
// an unexpired session without exposing the session owner or credentials.
func (m *Module) AuthorizeVoiceCallback(sessionID, token string) error {
	if m == nil || m.voice == nil {
		return fmt.Errorf("voice service is not configured")
	}
	m.voice.mu.Lock()
	s := m.voice.sessions[strings.TrimSpace(sessionID)]
	m.voice.mu.Unlock()
	if s == nil || s.Ended || time.Now().After(s.ExpiresAt) {
		return fmt.Errorf("voice session not found")
	}
	if m.voice.active != nil && !m.voice.active(s.OwnerID) {
		return fmt.Errorf("voice session inactive")
	}
	if m.voice.generation != nil && s.Generation != m.voice.generation() {
		return fmt.Errorf("voice session generation expired")
	}
	want, got := []byte(m.voice.callbackToken(s.SessionID, s.ExpiresAt)), []byte(strings.TrimSpace(token))
	if len(want) != len(got) || subtle.ConstantTimeCompare(want, got) != 1 {
		return fmt.Errorf("invalid voice callback token")
	}
	return nil
}

func (m *Module) ValidateVoiceProviderPayload(sessionID string, payload map[string]any) error {
	if m == nil || m.voice == nil {
		return fmt.Errorf("voice service is not configured")
	}
	m.voice.mu.Lock()
	s := m.voice.sessions[strings.TrimSpace(sessionID)]
	m.voice.mu.Unlock()
	if s == nil {
		return fmt.Errorf("voice session not found")
	}
	for _, field := range []struct {
		keys []string
		want string
	}{{[]string{"TaskId", "task_id"}, s.SessionID}, {[]string{"RoomId", "room_id"}, s.RoomID}, {[]string{"UserId", "user_id"}, s.UserID}} {
		found := false
		for _, key := range field.keys {
			if got, ok := payload[key].(string); ok && strings.TrimSpace(got) != "" {
				found = true
				if strings.TrimSpace(got) != field.want {
					return fmt.Errorf("voice provider identity mismatch")
				}
			}
		}
		if !found {
			return fmt.Errorf("voice provider identity missing")
		}
	}
	return nil
}

// HandleVoiceCustomLLM is called by the deployment-owned Volc callback route.
// The route must pass the opaque session id and the callback HMAC; transcript
// text is never logged or reflected in an error.
func (m *Module) HandleVoiceCustomLLM(ctx context.Context, sessionID, token, transcript string) error {
	_, err := m.RunVoiceCustomLLM(ctx, sessionID, token, transcript)
	return err
}

func (m *Module) RunVoiceCustomLLM(ctx context.Context, sessionID, token, transcript string, requestID ...string) (string, error) {
	return m.runVoiceCustomLLM(ctx, sessionID, token, transcript, nil, requestID...)
}

func (m *Module) RunVoiceCustomLLMStream(ctx context.Context, sessionID, token, transcript string, emit func(string) error, requestID ...string) error {
	_, err := m.runVoiceCustomLLM(ctx, sessionID, token, transcript, emit, requestID...)
	return err
}

func (m *Module) runVoiceCustomLLM(ctx context.Context, sessionID, token, transcript string, emit func(string) error, requestID ...string) (string, error) {
	if err := m.AuthorizeVoiceCallback(sessionID, token); err != nil {
		return "", err
	}
	if m == nil || m.voice == nil || m.voice.durable == nil {
		return "", fmt.Errorf("native agent turn coordinator is not configured")
	}
	m.voice.mu.Lock()
	s := m.voice.sessions[sessionID]
	m.voice.mu.Unlock()
	if s == nil {
		return "", fmt.Errorf("voice session not found")
	}
	turnKey := m.voice.nextTurnKey(sessionID)
	if len(requestID) > 0 && strings.TrimSpace(requestID[0]) != "" {
		turnKey = transcriptDigest(strings.TrimSpace(requestID[0]))
	}
	turnID := sessionID + "_turn_" + turnKey
	m.voice.mu.Lock()
	if current := m.voice.sessions[sessionID]; current != nil {
		if current.Busy && current.ActiveTurnID != turnID {
			m.voice.mu.Unlock()
			return "", fmt.Errorf("voice session already has an active turn")
		}
		current.ActiveTurnID = turnID
		current.Busy = true
	}
	m.voice.mu.Unlock()
	if strings.TrimSpace(transcript) == "" {
		return "", fmt.Errorf("voice transcript is empty")
	}
	var answer strings.Builder
	err := m.voice.durable(ctx, s.OwnerID, map[string]any{"turn_id": turnID, "conversation_id": s.ConversationID, "model_profile_id": s.ConversationProfileID, "_voice_revision": s.ConversationRevision, "_voice_credential": s.ConversationCredential, "prompt": strings.TrimSpace(transcript)}, func(event agentturns.StreamEvent) error {
		if event.Event == "delta" {
			if text := actionbase.String(event.Data["text"]); text != "" {
				answer.WriteString(text)
				if emit != nil {
					if err := emit(text); err != nil {
						return err
					}
				}
				m.voice.emit(sessionID, nativeagent.Event{Event: "answer", Data: map[string]any{"status": "speaking", "answer_delta": text}})
			}
		}
		if event.Event == "done" {
			m.voice.mu.Lock()
			if current := m.voice.sessions[sessionID]; current != nil {
				current.ActiveTurnID = ""
				current.Busy = false
			}
			m.voice.mu.Unlock()
			m.voice.emit(sessionID, nativeagent.Event{Event: "turn.done", Data: map[string]any{"status": "done"}})
		}
		return nil
	})
	if err != nil {
		m.voice.mu.Lock()
		if current := m.voice.sessions[sessionID]; current != nil {
			current.ActiveTurnID = ""
			current.Busy = false
		}
		m.voice.mu.Unlock()
		m.voice.emit(sessionID, nativeagent.Event{Event: "error", Data: map[string]any{"status": "error", "error": "voice turn failed"}})
	}
	return answer.String(), err
}

func (v *voiceCoordinator) nextTurnKey(sessionID string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s := v.sessions[sessionID]; s != nil {
		s.TurnSequence++
		return fmt.Sprintf("seq-%d", s.TurnSequence)
	}
	return "seq-unknown"
}

func (v *voiceCoordinator) get(owner, id string) (*voiceSession, *actionbase.Error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.sessions[id]
	if s == nil || s.Ended {
		return nil, actionbase.StatusError(http.StatusNotFound, "voice session not found")
	}
	if s.OwnerID != owner {
		return nil, actionbase.StatusError(http.StatusForbidden, "M_FORBIDDEN")
	}
	if v.active != nil && !v.active(s.OwnerID) {
		return nil, actionbase.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	}
	if v.generation != nil && s.Generation != v.generation() {
		return nil, actionbase.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	}
	if time.Now().After(s.ExpiresAt) {
		return nil, actionbase.StatusError(http.StatusGone, "voice session expired")
	}
	cp := *s
	return &cp, nil
}

func (v *voiceCoordinator) cleanupExpired(ctx context.Context) {
	now := time.Now()
	var expired []*voiceSession
	var expiredStreams []map[chan nativeagent.Event]struct{}
	v.mu.Lock()
	for id, s := range v.sessions {
		if s != nil && !s.Ended && now.After(s.ExpiresAt) {
			cp := *s
			s.Ended = true
			s.State = "ended"
			expiredStreams = append(expiredStreams, v.streams[id])
			delete(v.streams, id)
			expired = append(expired, &cp)
		}
	}
	v.mu.Unlock()
	for _, s := range expired {
		if client, err := v.providerClient(ctx, s.OwnerID, s); err == nil && client != nil {
			_ = client.StopVoiceChat(ctx, *s)
		}
	}
	for _, streams := range expiredStreams {
		for ch := range streams {
			select {
			case ch <- nativeagent.Event{Event: "session.done", Data: map[string]any{"status": "done", "session_ended": true}}:
			default:
			}
			close(ch)
		}
	}
}
func (v *voiceCoordinator) start(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	v.cleanupExpired(ctx)
	id := profileID(params, "session_id")
	if id == "" {
		return nil, actionbase.BadRequest("session_id is required")
	}
	s, e := v.get(owner, id)
	if e != nil {
		return nil, e
	}
	if s.op != nil {
		s.op.Lock()
		defer s.op.Unlock()
	}
	v.mu.Lock()
	if current := v.sessions[id]; current != nil && current.Started {
		v.mu.Unlock()
		return map[string]any{"ok": true, "session_id": id, "started": true, "already_started": true}, nil
	}
	if cur := v.sessions[id]; cur != nil {
		cur.Started = true
		cur.State = "starting"
	}
	v.mu.Unlock()
	client, err := v.providerClient(ctx, owner, s)
	if err != nil {
		v.mu.Lock()
		if cur := v.sessions[id]; cur != nil {
			cur.Started = false
			cur.State = "created"
		}
		v.mu.Unlock()
		return nil, err
	}
	if client != nil {
		if err := client.StartVoiceChat(ctx, *s); err != nil {
			v.mu.Lock()
			if cur := v.sessions[id]; cur != nil {
				cur.Started = false
				cur.State = "created"
			}
			v.mu.Unlock()
			return nil, actionbase.StatusError(http.StatusBadGateway, "voice provider start failed")
		}
	}
	v.mu.Lock()
	if cur := v.sessions[id]; cur != nil {
		cur.State = "started"
	}
	v.mu.Unlock()
	return map[string]any{"ok": true, "session_id": s.SessionID, "started": true}, nil
}
func (v *voiceCoordinator) transcript(_ context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	v.cleanupExpired(context.Background())
	id := profileID(params, "session_id")
	if id == "" {
		return nil, actionbase.BadRequest("session_id is required")
	}
	_, e := v.get(owner, id)
	if e != nil {
		return nil, e
	}
	if !v.cfg.ClientTranscriptSubmit {
		return map[string]any{"ok": true, "session_id": id, "accepted": false, "reason": "client_transcript_submit_disabled"}, nil
	}
	text := strings.TrimSpace(actionbase.String(params["transcript_final"]))
	if text == "" {
		return nil, actionbase.BadRequest("transcript_final is required")
	}
	v.emit(id, nativeagent.Event{Event: "transcribing", Data: map[string]any{"status": "transcribing"}})
	if v.durable != nil {
		s, _ := v.get(owner, id)
		turnID := id + "_turn_" + v.nextTurnKey(id)
		v.mu.Lock()
		if current := v.sessions[id]; current != nil {
			if current.Busy {
				v.mu.Unlock()
				return nil, actionbase.StatusError(http.StatusConflict, "voice session already has an active turn")
			}
			current.ActiveTurnID = turnID
			current.Busy = true
		}
		v.mu.Unlock()
		go func() {
			err := v.durable(context.Background(), owner, map[string]any{"turn_id": turnID, "conversation_id": s.ConversationID, "model_profile_id": s.ConversationProfileID, "_voice_revision": s.ConversationRevision, "_voice_credential": s.ConversationCredential, "prompt": text}, func(event agentturns.StreamEvent) error {
				if event.Event == "delta" {
					if value := actionbase.String(event.Data["text"]); value != "" {
						v.emit(id, nativeagent.Event{Event: "answer", Data: map[string]any{"status": "speaking", "answer_delta": value}})
					}
				} else if event.Event == "done" {
					v.mu.Lock()
					if current := v.sessions[id]; current != nil {
						current.ActiveTurnID = ""
						current.Busy = false
					}
					v.mu.Unlock()
					v.emit(id, nativeagent.Event{Event: "turn.done", Data: map[string]any{"status": "done"}})
				}
				return nil
			})
			if err != nil {
				v.mu.Lock()
				if current := v.sessions[id]; current != nil {
					current.ActiveTurnID = ""
					current.Busy = false
				}
				v.mu.Unlock()
				v.emit(id, nativeagent.Event{Event: "error", Data: map[string]any{"status": "error", "error": "voice turn failed"}})
			}
		}()
	} else {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent turn coordinator is not configured")
	}
	return map[string]any{"ok": true, "session_id": id, "accepted": true}, nil
}
func (v *voiceCoordinator) interrupt(_ context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	v.cleanupExpired(context.Background())
	id := profileID(params, "session_id")
	s, e := v.get(owner, id)
	if e != nil {
		return nil, e
	}
	v.emit(id, nativeagent.Event{Event: "listening", Data: map[string]any{"status": "listening"}})
	if client, err := v.providerClient(context.Background(), owner, s); err == nil && client != nil {
		if err := client.InterruptVoiceChat(context.Background(), *s); err != nil {
			return nil, actionbase.StatusError(http.StatusBadGateway, "voice provider interrupt failed")
		}
	}
	if v.stop != nil && s.ActiveTurnID != "" {
		_ = v.stop(context.Background(), owner, s.ActiveTurnID)
	}
	return map[string]any{"ok": true, "session_id": id, "interrupted": true}, nil
}
func (v *voiceCoordinator) end(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	v.cleanupExpired(ctx)
	id := profileID(params, "session_id")
	if id == "" {
		return nil, actionbase.BadRequest("session_id is required")
	}
	v.mu.Lock()
	current := v.sessions[id]
	if current == nil {
		v.mu.Unlock()
		return nil, actionbase.StatusError(http.StatusNotFound, "voice session not found")
	}
	if current.OwnerID != owner {
		v.mu.Unlock()
		return nil, actionbase.StatusError(http.StatusForbidden, "M_FORBIDDEN")
	}
	if v.generation != nil && current.Generation != v.generation() {
		v.mu.Unlock()
		return nil, actionbase.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
	}
	if current.Ended || current.State == "ended" {
		v.mu.Unlock()
		return map[string]any{"ok": true, "session_id": id, "ended": true, "already_ended": true}, nil
	}
	s := *current
	v.mu.Unlock()
	if s.op != nil {
		s.op.Lock()
		defer s.op.Unlock()
	}
	if time.Now().After(s.ExpiresAt) {
		return nil, actionbase.StatusError(http.StatusGone, "voice session expired")
	}
	v.mu.Lock()
	if latest := v.sessions[id]; latest != nil {
		latest.State = "stopping"
	}
	v.mu.Unlock()
	if client, err := v.providerClient(ctx, owner, &s); err != nil {
		v.mu.Lock()
		if latest := v.sessions[id]; latest != nil {
			latest.State = "started"
		}
		v.mu.Unlock()
		return nil, err
	} else if client != nil {
		if err := client.StopVoiceChat(ctx, s); err != nil {
			v.mu.Lock()
			if latest := v.sessions[id]; latest != nil {
				latest.State = "started"
			}
			v.mu.Unlock()
			return nil, actionbase.StatusError(http.StatusBadGateway, "voice provider stop failed")
		}
	}
	if v.stop != nil && s.ActiveTurnID != "" {
		_ = v.stop(ctx, owner, s.ActiveTurnID)
	}
	v.mu.Lock()
	if latest := v.sessions[id]; latest != nil {
		latest.Ended = true
		latest.State = "ended"
		latest.ActiveTurnID = ""
	}
	streams := v.streams[id]
	delete(v.streams, id)
	v.mu.Unlock()
	for ch := range streams {
		select {
		case ch <- nativeagent.Event{Event: "session.done", Data: map[string]any{"status": "done", "session_ended": true}}:
		default:
		}
		close(ch)
	}
	return map[string]any{"ok": true, "session_id": id, "ended": true}, nil
}
func (v *voiceCoordinator) stream(ctx context.Context, owner string, params map[string]any, emit func(nativeagent.Event) error) error {
	v.cleanupExpired(ctx)
	id := profileID(params, "session_id")
	if id == "" {
		return fmt.Errorf("session_id is required")
	}
	ch := make(chan nativeagent.Event, 16)
	v.mu.Lock()
	s := v.sessions[id]
	if s == nil || s.Ended || time.Now().After(s.ExpiresAt) {
		v.mu.Unlock()
		return fmt.Errorf("voice session not found")
	}
	if s.OwnerID != owner {
		v.mu.Unlock()
		return fmt.Errorf("M_FORBIDDEN")
	}
	if v.streams[id] == nil {
		v.streams[id] = map[chan nativeagent.Event]struct{}{}
	}
	v.streams[id][ch] = struct{}{}
	v.mu.Unlock()
	defer func() { v.mu.Lock(); delete(v.streams[id], ch); v.mu.Unlock() }()
	if err := emit(nativeagent.Event{Event: "listening", Data: map[string]any{"status": "listening"}}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			if err := emit(e); err != nil {
				return err
			}
		}
	}
}
func (v *voiceCoordinator) emit(id string, event nativeagent.Event) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for ch := range v.streams[id] {
		select {
		case ch <- event:
		default:
		}
	}
}

func stringConfig(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	return strings.TrimSpace(actionbase.String(m[key]))
}
func randomVoiceHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", b)
}

func transcriptDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:8])
}
func errorsFromAction(e *actionbase.Error) error {
	if e == nil {
		return fmt.Errorf("voice session unavailable")
	}
	return fmt.Errorf("%s", e.Error)
}

func fallback(first, second string) string {
	if strings.TrimSpace(first) != "" {
		return first
	}
	return second
}
