package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type voiceChatClient interface {
	StartVoiceChat(context.Context, voiceSession) error
	StopVoiceChat(context.Context, voiceSession) error
	InterruptVoiceChat(context.Context, voiceSession) error
}

type volcVoiceChatClient struct {
	httpClient                                                 *http.Client
	host, region, accessKey, secret, webhookURL, webhookSecret string
}

func newVolcVoiceChatClient(accessKey, secret string, cfg voiceRuntimeConfig) voiceChatClient {
	host := strings.TrimSpace(os.Getenv("VOLC_RTC_OPENAPI_HOST"))
	if host == "" {
		host = "rtc.volcengineapi.com"
	}
	if host != "rtc.volcengineapi.com" {
		host = "rtc.volcengineapi.com"
	}
	if !strings.HasPrefix(host, "https://") {
		host = "https://" + host
	}
	callback := cfg.CustomLLMURL
	if callback == "" {
		callback = cfg.WebhookURL
	}
	return &volcVoiceChatClient{httpClient: &http.Client{Timeout: 15 * time.Second}, host: host, region: "cn-north-1", accessKey: accessKey, secret: secret, webhookURL: callback, webhookSecret: cfg.WebhookSecret}
}

func (c *volcVoiceChatClient) StartVoiceChat(ctx context.Context, s voiceSession) error {
	callback := c.webhookURL
	if callback != "" {
		u, err := url.Parse(callback)
		if err != nil || u.Scheme != "https" {
			return fmt.Errorf("voice callback URL must use HTTPS")
		}
		q := u.Query()
		q.Set("session_id", s.SessionID)
		u.RawQuery = q.Encode()
		callback = u.String()
	}
	config := map[string]any{"CallbackUrl": callback, "ASRConfig": map[string]any{"Provider": "volcano", "ProviderParams": map[string]any{"Mode": "bigmodel", "ApiResourceId": "volc.seedasr.sauc.duration", "StreamMode": 2, "VolcanoASRParameters": "{\"request\":{\"enable_nonstream\":true}}"}, "VADConfig": map[string]any{"SilenceTime": 900}, "InterruptConfig": map[string]any{"InterruptSpeechDuration": 700}}, "LLMConfig": map[string]any{"Mode": "CustomLLM", "Url": callback, "APIKey": s.CallbackToken}, "TTSConfig": map[string]any{"Provider": "volcano_bidirection", "ProviderParams": map[string]any{}}}
	if s.SpeechProviderConfig != nil {
		params := config["TTSConfig"].(map[string]any)["ProviderParams"].(map[string]any)
		resource, speaker := logString(s.SpeechProviderConfig["tts_resource_id"]), logString(s.SpeechProviderConfig["tts_speaker"])
		if resource == "" {
			resource = "seed-tts-1.0"
		}
		if speaker == "" {
			speaker = "zh_female_qingxinnvsheng_mars_bigtts"
		}
		params["Credential"] = map[string]any{"ResourceId": resource}
		tts := map[string]any{"req_params": map[string]any{"speaker": speaker, "audio_params": map[string]any{"speech_rate": voiceConfigInt(logString(s.SpeechProviderConfig["tts_speech_rate"]), 18), "loudness_rate": voiceConfigInt(logString(s.SpeechProviderConfig["tts_loudness_rate"]), 2)}, "additions": map[string]any{"post_process": map[string]any{"pitch": voiceConfigInt(logString(s.SpeechProviderConfig["tts_pitch"]), 1)}}}}
		encoded, _ := json.Marshal(tts)
		params["VolcanoTTSParameters"] = string(encoded)
	}
	payload := map[string]any{"AppId": s.VoiceChatAppID, "RoomId": s.RoomID, "TaskId": s.SessionID, "Config": config, "AgentConfig": map[string]any{"TargetUserId": []string{s.UserID}, "UserId": s.AIUserID, "EnableConversationStateCallback": true}}
	return c.call(ctx, "StartVoiceChat", payload)
}
func (c *volcVoiceChatClient) StopVoiceChat(ctx context.Context, s voiceSession) error {
	return c.call(ctx, "StopVoiceChat", map[string]any{"AppId": s.VoiceChatAppID, "RoomId": s.RoomID, "TaskId": s.SessionID})
}
func (c *volcVoiceChatClient) InterruptVoiceChat(ctx context.Context, s voiceSession) error {
	return c.call(ctx, "UpdateVoiceChat", map[string]any{"AppId": s.VoiceChatAppID, "RoomId": s.RoomID, "TaskId": s.SessionID, "Command": "interrupt"})
}

func (c *volcVoiceChatClient) call(ctx context.Context, action string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(c.host, "/") + "/?Action=" + url.QueryEscape(action) + "&Version=2024-12-01"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	signVolcRequest(req, body, c.accessKey, c.secret, c.region, "rtc")
	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("voice provider returned status %d", resp.StatusCode)
	}
	var result struct {
		ResponseMetadata struct {
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
	}
	if json.Unmarshal(raw, &result) == nil && result.ResponseMetadata.Error != nil {
		return fmt.Errorf("voice provider request failed: %s", result.ResponseMetadata.Error.Code)
	}
	return nil
}

func signVolcRequest(req *http.Request, body []byte, accessKey, secret, region, service string) {
	date := time.Now().UTC().Format("20060102T150405Z")
	hash := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(hash[:])
	req.Header.Set("X-Date", date)
	req.Header.Set("X-Content-Sha256", bodyHash)
	req.Header.Set("Host", req.Host)
	canonical := req.Method + "\n/\n" + req.URL.RawQuery + "\ncontent-type:" + req.Header.Get("Content-Type") + "\nhost:" + req.Host + "\nx-content-sha256:" + bodyHash + "\nx-date:" + date + "\n\ncontent-type;host;x-content-sha256;x-date\n" + bodyHash
	sum := sha256.Sum256([]byte(canonical))
	scope := date[:8] + "/" + region + "/" + service + "/request"
	toSign := "HMAC-SHA256\n" + date + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte(secret), []byte(date[:8])), []byte(region)), []byte(service)), []byte("request"))
	sig := hex.EncodeToString(hmacSHA256(key, []byte(toSign)))
	req.Header.Set("Authorization", "HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders=content-type;host;x-content-sha256;x-date, Signature="+sig)
}

func voiceConfigInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}
func logString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}
