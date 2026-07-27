package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type voiceRoundTrip func(*http.Request) (*http.Response, error)

func (f voiceRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVolcVoicePayloadUsesSessionTokenAndTypedAudioConfig(t *testing.T) {
	var body map[string]any
	client := &volcVoiceChatClient{httpClient: &http.Client{Transport: voiceRoundTrip(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte(`{}`))), Header: make(http.Header)}, nil
	})}, host: "https://rtc.volcengineapi.com", accessKey: "ak", secret: "sk", webhookURL: "https://voice.example.test/_p2p/agent/voice/volc/custom-llm", webhookSecret: "master-secret"}
	s := voiceSession{SessionID: "voice_1", CallbackToken: "session-token", VoiceChatAppID: "app", RoomID: "room", UserID: "user", AIUserID: "ai", SpeechProviderConfig: map[string]any{"tts_speaker": "speaker", "tts_speech_rate": "12", "tts_loudness_rate": "3", "tts_pitch": "-1", "tts_resource_id": "res"}}
	if err := client.StartVoiceChat(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	text := string(raw)
	if strings.Contains(text, "master-secret") {
		t.Fatal("master callback secret leaked into provider payload")
	}
	config := body["Config"].(map[string]any)
	llm := config["LLMConfig"].(map[string]any)
	if llm["APIKey"] != "session-token" {
		t.Fatalf("api key=%v", llm["APIKey"])
	}
	asr := config["ASRConfig"].(map[string]any)
	if asr["VADConfig"].(map[string]any)["SilenceTime"] != float64(900) {
		t.Fatal("vad shape")
	}
	tts := config["TTSConfig"].(map[string]any)["ProviderParams"].(map[string]any)
	credential := tts["Credential"].(map[string]any)
	if credential["ResourceId"] != "res" {
		t.Fatalf("tts credential=%#v", credential)
	}
	volcanoTTS := tts["VolcanoTTSParameters"].(string)
	var ttsParams map[string]any
	if err := json.Unmarshal([]byte(volcanoTTS), &ttsParams); err != nil {
		t.Fatal(err)
	}
	reqParams := ttsParams["req_params"].(map[string]any)
	audioParams := reqParams["audio_params"].(map[string]any)
	postProcess := reqParams["additions"].(map[string]any)["post_process"].(map[string]any)
	if reqParams["speaker"] != "speaker" || audioParams["speech_rate"] != float64(12) || audioParams["loudness_rate"] != float64(3) || postProcess["pitch"] != float64(-1) {
		t.Fatalf("tts shape=%#v", ttsParams)
	}
}
