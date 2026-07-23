package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestVolcVoiceChatClientReplacesDynamicSessionFields(t *testing.T) {
	var calls []map[string]any
	client := &volcVoiceChatOpenAPIClient{
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "" || req.Header.Get("X-Date") == "" || req.Header.Get("X-Content-Sha256") == "" {
				t.Fatalf("request was not signed: headers=%#v", req.Header)
			}
			if req.URL.Query().Get("Version") != "2024-12-01" {
				t.Fatalf("unexpected version query: %s", req.URL.RawQuery)
			}
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			body["_action"] = req.URL.Query().Get("Action")
			calls = append(calls, body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader([]byte(`{"ResponseMetadata":{"RequestId":"req"}}`))),
				Header:     make(http.Header),
			}, nil
		})},
		host:            "rtc.volcengineapi.com",
		region:          "cn-north-1",
		accessKeyID:     "ak",
		secretAccessKey: "sk",
		webhookURL:      "https://www.wenson.art/_p2p/agent/voice/webhook",
		customLLMURL:    "https://www.wenson.art/_p2p/agent/voice/volc/custom-llm",
		webhookSecret:   "secret",
		configTemplate: parseVoiceChatTemplate(`{
			"AppId":"template-app",
			"RoomId":"template-room",
			"TaskId":"template-task",
			"Config":{"CallbackUrl":"${VOLC_VOICE_WEBHOOK_URL}","ASRConfig":{"Provider":"volcano"},"LLMConfig":{"Mode":"ArkV3","ModelName":"doubao"}},
			"AgentConfig":{"TargetUserId":["template-user"],"UserId":"template-ai","WelcomeMessage":"hello"}
		}`),
	}
	session := voiceSession{
		SessionID:      "voice_1",
		TaskID:         "voice_1",
		AppID:          "123456781234567812345678",
		VoiceChatAppID: "123456781234567812345678",
		RoomID:         "dirextalk_voice_room",
		UserID:         "owner_user",
		AIUserID:       "dirextalk_ai_user",
	}
	if err := client.StartVoiceChat(context.Background(), session); err != nil {
		t.Fatalf("StartVoiceChat: %v", err)
	}
	if err := client.StopVoiceChat(context.Background(), session); err != nil {
		t.Fatalf("StopVoiceChat: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("unexpected call count: %#v", calls)
	}
	start := calls[0]
	if start["_action"] != "StartVoiceChat" || start["AppId"] != session.VoiceChatAppID || start["RoomId"] != session.RoomID || start["TaskId"] != session.TaskID {
		t.Fatalf("start payload did not use dynamic ids: %#v", start)
	}
	config := start["Config"].(map[string]any)
	if config["CallbackUrl"] != "https://www.wenson.art/_p2p/agent/voice/webhook" {
		t.Fatalf("webhook placeholder was not replaced: %#v", config)
	}
	llmConfig := config["LLMConfig"].(map[string]any)
	if llmConfig["Mode"] != "CustomLLM" || llmConfig["ModelName"] != nil || !strings.Contains(llmConfig["Url"].(string), "session_id=voice_1") {
		t.Fatalf("legacy LLM config should be overridden: %#v", llmConfig)
	}
	agentConfig := start["AgentConfig"].(map[string]any)
	targets := agentConfig["TargetUserId"].([]any)
	if len(targets) != 1 || targets[0] != session.UserID || agentConfig["UserId"] != session.AIUserID {
		t.Fatalf("agent config did not use dynamic users: %#v", agentConfig)
	}
	stop := calls[1]
	if stop["_action"] != "StopVoiceChat" || stop["AppId"] != session.VoiceChatAppID || stop["RoomId"] != session.RoomID || stop["TaskId"] != session.TaskID {
		t.Fatalf("stop payload did not use dynamic ids: %#v", stop)
	}
}

func TestVolcVoiceChatClientReportsNoPermissionWithHint(t *testing.T) {
	client := &volcVoiceChatOpenAPIClient{
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(`{
					"ResponseMetadata": {
						"RequestId": "req-no-permission",
						"Error": {
							"Code": "NoPermissionForApp",
							"Message": "no permission for app"
						}
					}
				}`)),
				Header: make(http.Header),
			}, nil
		})},
		host:            "rtc.volcengineapi.com",
		region:          "cn-north-1",
		accessKeyID:     "ak",
		secretAccessKey: "sk",
		configTemplate:  parseVoiceChatTemplate(`{"Config":{},"AgentConfig":{}}`),
	}
	err := client.StartVoiceChat(context.Background(), voiceSession{
		SessionID:      "voice_1",
		TaskID:         "voice_1",
		AppID:          "rtc-app",
		VoiceChatAppID: "rtc-app",
		RoomID:         "room",
		UserID:         "user",
		AIUserID:       "ai",
	})
	if err == nil {
		t.Fatal("expected NoPermissionForApp error")
	}
	text := err.Error()
	for _, want := range []string{"NoPermissionForApp", "req-no-permission", "VOLC_VOICE_CHAT_APP_ID"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q does not contain %q", text, want)
		}
	}
}

func TestDefaultVoiceChatTemplateUsesNoiseTolerantVAD(t *testing.T) {
	template := defaultVoiceChatTemplate()
	config := template["Config"].(map[string]any)
	asrConfig := config["ASRConfig"].(map[string]any)
	vadConfig := asrConfig["VADConfig"].(map[string]any)
	interruptConfig := asrConfig["InterruptConfig"].(map[string]any)
	llmConfig := config["LLMConfig"].(map[string]any)
	if vadConfig["SilenceTime"] != 900 {
		t.Fatalf("SilenceTime = %#v, want 900", vadConfig["SilenceTime"])
	}
	if interruptConfig["InterruptSpeechDuration"] != 700 {
		t.Fatalf("InterruptSpeechDuration = %#v, want 700", interruptConfig["InterruptSpeechDuration"])
	}
	if llmConfig["Mode"] != "CustomLLM" || llmConfig["ModelName"] != nil || llmConfig["SystemMessages"] != nil {
		t.Fatalf("LLMConfig must route to Dirextalk CustomLLM only: %#v", llmConfig)
	}
	fields := summarizeVoiceChatPayload(template)
	if fields["vad_silence_time"] != "900" ||
		fields["interrupt_speech_duration"] != "700" ||
		fields["tts_resource"] != defaultVolcVoiceTTSResourceID ||
		fields["tts_speaker"] != defaultVolcVoiceTTSSpeaker ||
		fields["tts_speech_rate"] != "18" ||
		fields["tts_loudness_rate"] != "2" ||
		fields["tts_pitch"] != "1" {
		t.Fatalf("summary should include VAD/interrupt values, got %#v", fields)
	}
	ttsConfig := config["TTSConfig"].(map[string]any)
	providerParams := ttsConfig["ProviderParams"].(map[string]any)
	credential := providerParams["Credential"].(map[string]any)
	if credential["ResourceId"] != defaultVolcVoiceTTSResourceID {
		t.Fatalf("unexpected tts resource: %#v", credential)
	}
	var ttsParams map[string]any
	if err := json.Unmarshal([]byte(providerParams["VolcanoTTSParameters"].(string)), &ttsParams); err != nil {
		t.Fatalf("decode VolcanoTTSParameters: %v", err)
	}
	reqParams := ttsParams["req_params"].(map[string]any)
	if reqParams["speaker"] != defaultVolcVoiceTTSSpeaker {
		t.Fatalf("unexpected default speaker: %#v", reqParams)
	}
	audioParams := reqParams["audio_params"].(map[string]any)
	if audioParams["speech_rate"] != float64(18) || audioParams["loudness_rate"] != float64(2) {
		t.Fatalf("unexpected tts audio params: %#v", audioParams)
	}
	postProcess := reqParams["additions"].(map[string]any)["post_process"].(map[string]any)
	if postProcess["pitch"] != float64(1) {
		t.Fatalf("unexpected tts post process: %#v", postProcess)
	}
}

func TestDefaultVoiceChatTemplateUsesTTSConfigOverrides(t *testing.T) {
	template := defaultVoiceChatTemplate(voiceConfig{
		TTSSpeaker:      "zh_female_vv_uranus_bigtts",
		TTSResourceID:   "custom-resource",
		TTSSpeechRate:   "12",
		TTSLoudnessRate: "3",
		TTSPitch:        "-1",
	})
	fields := summarizeVoiceChatPayload(template)
	if fields["tts_resource"] != "custom-resource" ||
		fields["tts_speaker"] != "zh_female_vv_uranus_bigtts" ||
		fields["tts_speech_rate"] != "12" ||
		fields["tts_loudness_rate"] != "3" ||
		fields["tts_pitch"] != "-1" {
		t.Fatalf("summary should include configured tts values, got %#v", fields)
	}
	config := template["Config"].(map[string]any)
	ttsConfig := config["TTSConfig"].(map[string]any)
	providerParams := ttsConfig["ProviderParams"].(map[string]any)
	if credential := providerParams["Credential"].(map[string]any); credential["ResourceId"] != "custom-resource" {
		t.Fatalf("unexpected credential: %#v", credential)
	}
	var ttsParams map[string]any
	if err := json.Unmarshal([]byte(providerParams["VolcanoTTSParameters"].(string)), &ttsParams); err != nil {
		t.Fatalf("decode VolcanoTTSParameters: %v", err)
	}
	reqParams := ttsParams["req_params"].(map[string]any)
	audioParams := reqParams["audio_params"].(map[string]any)
	postProcess := reqParams["additions"].(map[string]any)["post_process"].(map[string]any)
	if reqParams["speaker"] != "zh_female_vv_uranus_bigtts" ||
		audioParams["speech_rate"] != float64(12) ||
		audioParams["loudness_rate"] != float64(3) ||
		postProcess["pitch"] != float64(-1) {
		t.Fatalf("unexpected configured tts params: %#v", ttsParams)
	}
}

func TestVolcVoiceChatClientConfiguresCustomLLMCallback(t *testing.T) {
	var start map[string]any
	client := &volcVoiceChatOpenAPIClient{
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get("Action") == "StartVoiceChat" {
				if err := json.NewDecoder(req.Body).Decode(&start); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ResponseMetadata":{"RequestId":"req"}}`)),
				Header:     make(http.Header),
			}, nil
		})},
		host:            "rtc.volcengineapi.com",
		region:          "cn-north-1",
		accessKeyID:     "ak",
		secretAccessKey: "sk",
		webhookURL:      "https://www.wenson.art/_p2p/agent/voice/webhook",
		customLLMURL:    "https://www.wenson.art/_p2p/agent/voice/volc/custom-llm",
		webhookSecret:   "secret",
		configTemplate:  defaultVoiceChatTemplate(),
	}
	session := voiceSession{
		SessionID:      "voice_1",
		TaskID:         "voice_1",
		AppID:          "123456781234567812345678",
		VoiceChatAppID: "123456781234567812345678",
		RoomID:         "dirextalk_voice_room",
		UserID:         "owner_user",
		AIUserID:       "dirextalk_ai_user",
	}
	if err := client.StartVoiceChat(context.Background(), session); err != nil {
		t.Fatalf("StartVoiceChat: %v", err)
	}
	config := start["Config"].(map[string]any)
	llmConfig := config["LLMConfig"].(map[string]any)
	if llmConfig["Mode"] != "CustomLLM" || llmConfig["APIKey"] != "secret" {
		t.Fatalf("unexpected custom llm config: %#v", llmConfig)
	}
	if got := llmConfig["Url"].(string); !strings.Contains(got, "/_p2p/agent/voice/volc/custom-llm") || !strings.Contains(got, "session_id=voice_1") {
		t.Fatalf("custom llm url missing session id: %q", got)
	}
	if config["CallbackUrl"] != "https://www.wenson.art/_p2p/agent/voice/webhook" {
		t.Fatalf("webhook callback url not configured: %#v", config)
	}
	ttsConfig := config["TTSConfig"].(map[string]any)
	providerParams := ttsConfig["ProviderParams"].(map[string]any)
	var ttsParams map[string]any
	if err := json.Unmarshal([]byte(providerParams["VolcanoTTSParameters"].(string)), &ttsParams); err != nil {
		t.Fatalf("decode tts params: %v", err)
	}
	audioParams := ttsParams["req_params"].(map[string]any)["audio_params"].(map[string]any)
	if audioParams["speech_rate"] != float64(18) || audioParams["loudness_rate"] != float64(2) {
		t.Fatalf("custom llm payload should tune tts params: %#v", audioParams)
	}
}

func TestVolcVoiceChatClientTunesDemoTTSRatios(t *testing.T) {
	var start map[string]any
	client := &volcVoiceChatOpenAPIClient{
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get("Action") == "StartVoiceChat" {
				if err := json.NewDecoder(req.Body).Decode(&start); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ResponseMetadata":{"RequestId":"req"}}`)),
				Header:     make(http.Header),
			}, nil
		})},
		host:            "rtc.volcengineapi.com",
		region:          "cn-north-1",
		accessKeyID:     "ak",
		secretAccessKey: "sk",
		webhookURL:      "https://www.wenson.art/_p2p/agent/voice/webhook",
		customLLMURL:    "https://www.wenson.art/_p2p/agent/voice/volc/custom-llm",
		webhookSecret:   "secret",
		configTemplate: parseVoiceChatTemplate(`{
			"Config":{
				"ASRConfig":{"Provider":"volcano"},
				"TTSConfig":{
					"Provider":"volcano",
					"ProviderParams":{
						"audio":{"speed_ratio":1,"pitch_ratio":1,"volume_ratio":1}
					}
				}
			},
			"AgentConfig":{}
		}`),
	}
	if err := client.StartVoiceChat(context.Background(), voiceSession{
		SessionID:      "voice_1",
		TaskID:         "voice_1",
		AppID:          "123456781234567812345678",
		VoiceChatAppID: "123456781234567812345678",
		RoomID:         "dirextalk_voice_room",
		UserID:         "owner_user",
		AIUserID:       "dirextalk_ai_user",
	}); err != nil {
		t.Fatalf("StartVoiceChat: %v", err)
	}
	audio := start["Config"].(map[string]any)["TTSConfig"].(map[string]any)["ProviderParams"].(map[string]any)["audio"].(map[string]any)
	if audio["speed_ratio"] != 1.12 || audio["pitch_ratio"] != 1.02 || audio["volume_ratio"] != 1.05 {
		t.Fatalf("demo tts ratios not tuned: %#v", audio)
	}
}

func TestVolcRTCTokenSignerProducesScopedToken(t *testing.T) {
	signer := volcRTCTokenSigner{}
	expiresAt := time.Now().Add(time.Hour).UTC()
	token, err := signer.SignRTC(
		"123456781234567812345678",
		"app-key",
		"room",
		"user",
		expiresAt,
	)
	if err != nil {
		t.Fatalf("SignRTC: %v", err)
	}
	if !strings.HasPrefix(token, "001123456781234567812345678") {
		t.Fatalf("unexpected token prefix: %q", token)
	}
	parsed, err := parseVolcRTCTestToken(token)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if parsed.roomID != "room" || parsed.userID != "user" {
		t.Fatalf("unexpected token identity: room=%q user=%q", parsed.roomID, parsed.userID)
	}
	if parsed.expireAt != uint32(expiresAt.Unix()) {
		t.Fatalf("unexpected token expiry: got %d want %d", parsed.expireAt, expiresAt.Unix())
	}
	if len(parsed.privileges) != 6 {
		t.Fatalf("unexpected privileges: %#v", parsed.privileges)
	}
	for privilege := uint16(0); privilege <= 5; privilege++ {
		if parsed.privileges[privilege] != parsed.expireAt {
			t.Fatalf("privilege %d expiry = %d, want %d", privilege, parsed.privileges[privilege], parsed.expireAt)
		}
	}
}

type volcRTCTestToken struct {
	expireAt   uint32
	roomID     string
	userID     string
	privileges map[uint16]uint32
}

func parseVolcRTCTestToken(raw string) (volcRTCTestToken, error) {
	if !strings.HasPrefix(raw, "001") || len(raw) <= 27 {
		return volcRTCTestToken{}, fmt.Errorf("invalid token prefix")
	}
	content, err := base64.StdEncoding.DecodeString(raw[27:])
	if err != nil {
		return volcRTCTestToken{}, err
	}
	reader := bytes.NewReader(content)
	message, err := readVolcTestString(reader)
	if err != nil {
		return volcRTCTestToken{}, err
	}
	messageReader := bytes.NewReader(message)
	if _, err := readVolcTestUint32(messageReader); err != nil {
		return volcRTCTestToken{}, err
	}
	if _, err := readVolcTestUint32(messageReader); err != nil {
		return volcRTCTestToken{}, err
	}
	expireAt, err := readVolcTestUint32(messageReader)
	if err != nil {
		return volcRTCTestToken{}, err
	}
	roomID, err := readVolcTestString(messageReader)
	if err != nil {
		return volcRTCTestToken{}, err
	}
	userID, err := readVolcTestString(messageReader)
	if err != nil {
		return volcRTCTestToken{}, err
	}
	var count uint16
	if err := binary.Read(messageReader, binary.LittleEndian, &count); err != nil {
		return volcRTCTestToken{}, err
	}
	privileges := map[uint16]uint32{}
	for i := uint16(0); i < count; i++ {
		var privilege uint16
		var privilegeExpiresAt uint32
		if err := binary.Read(messageReader, binary.LittleEndian, &privilege); err != nil {
			return volcRTCTestToken{}, err
		}
		if err := binary.Read(messageReader, binary.LittleEndian, &privilegeExpiresAt); err != nil {
			return volcRTCTestToken{}, err
		}
		privileges[privilege] = privilegeExpiresAt
	}
	return volcRTCTestToken{
		expireAt:   expireAt,
		roomID:     string(roomID),
		userID:     string(userID),
		privileges: privileges,
	}, nil
}

func readVolcTestUint32(reader *bytes.Reader) (uint32, error) {
	var value uint32
	err := binary.Read(reader, binary.LittleEndian, &value)
	return value, err
}

func readVolcTestString(reader *bytes.Reader) ([]byte, error) {
	var length uint16
	if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
		return nil, err
	}
	value := make([]byte, length)
	_, err := reader.Read(value)
	return value, err
}
