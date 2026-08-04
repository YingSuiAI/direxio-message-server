package p2p

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExternalVoiceCallbackRelayForwardsBoundedAuthenticatedRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/agent/voice/volc/custom-llm" || r.URL.Query().Get("session_id") != "voice-1" {
			t.Errorf("callback target=%s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get(voiceCallbackRelayAuthHeader); got != "relay-secret" {
			t.Errorf("relay auth header=%q", got)
		}
		if got := r.Header.Get(voiceCallbackGenerationHeader); got != "7" {
			t.Errorf("generation header=%q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer provider-hmac" {
			t.Errorf("provider auth header=%q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"session_id":"voice-1","text":"hello"}` {
			t.Errorf("callback body=%q", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	service := NewService(Config{
		ServerName:                         "example.test",
		AccountGeneration:                  7,
		NativeAgentVoiceCallbackURL:        server.URL,
		NativeAgentVoiceCallbackAuthToken:  "relay-secret",
		NativeAgentVoiceCallbackHTTPClient: server.Client(),
	})
	request := httptest.NewRequest(http.MethodPost, "/_p2p/agent/voice/volc/custom-llm?session_id=voice-1", strings.NewReader(`{"session_id":"voice-1","text":"hello"}`))
	request.Header.Set("Authorization", "Bearer provider-hmac")
	response := httptest.NewRecorder()
	voiceCustomLLMHandler(service)(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "data: [DONE]\n\n" {
		t.Fatalf("relay response status=%d body=%q", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("downstream calls=%d", calls.Load())
	}
}

func TestExternalVoiceCallbackRelayRejectsOversizedAndMissingConfiguration(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	service := NewService(Config{
		ServerName:                         "example.test",
		AccountGeneration:                  7,
		NativeAgentVoiceCallbackURL:        server.URL,
		NativeAgentVoiceCallbackAuthToken:  "relay-secret",
		NativeAgentVoiceCallbackHTTPClient: server.Client(),
	})
	request := httptest.NewRequest(http.MethodPost, "/_p2p/agent/voice/webhook?session_id=voice-1", strings.NewReader(strings.Repeat("x", defaultVoiceCallbackMaxBodyBytes+1)))
	response := httptest.NewRecorder()
	voiceEventWebhookHandler(service)(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d", response.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("oversized request reached Agent: %d", calls.Load())
	}

	missing := NewService(Config{ServerName: "example.test", AccountGeneration: 7})
	response = httptest.NewRecorder()
	voiceEventWebhookHandler(missing)(response, httptest.NewRequest(http.MethodPost, "/_p2p/agent/voice/webhook?session_id=voice-1", strings.NewReader(`{}`)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing relay config status=%d", response.Code)
	}
}

func TestExternalVoiceCallbackRelayMapsForgedStaleAndDownstreamErrors(t *testing.T) {
	mode := "forged"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "forged":
			w.WriteHeader(http.StatusUnauthorized)
		case "stale":
			if r.Header.Get(voiceCallbackGenerationHeader) != "8" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	service := NewService(Config{
		ServerName:                         "example.test",
		AccountGeneration:                  7,
		NativeAgentVoiceCallbackURL:        server.URL,
		NativeAgentVoiceCallbackAuthToken:  "relay-secret",
		NativeAgentVoiceCallbackHTTPClient: server.Client(),
	})
	invoke := func() int {
		response := httptest.NewRecorder()
		voiceEventWebhookHandler(service)(response, httptest.NewRequest(http.MethodPost, "/_p2p/agent/voice/webhook?session_id=voice-1", strings.NewReader(`{"signature":"provider"}`)))
		return response.Code
	}
	if got := invoke(); got != http.StatusUnauthorized {
		t.Fatalf("forged callback status=%d", got)
	}
	mode = "stale"
	if got := invoke(); got != http.StatusUnauthorized {
		t.Fatalf("stale callback status=%d", got)
	}
	mode = "downstream"
	if got := invoke(); got != http.StatusBadGateway {
		t.Fatalf("downstream callback status=%d", got)
	}
}
