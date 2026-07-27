package p2p

import "testing"

func TestVoiceCallbackExtractsTranscriptAndProviderRequestID(t *testing.T) {
	payload := map[string]any{"event_id": "evt-1", "messages": []any{map[string]any{"content": "hello voice"}}}
	if got := voiceCallbackTranscript(payload); got != "hello voice" {
		t.Fatalf("transcript=%q", got)
	}
	if got := voiceCallbackRequestID(payload); got != "evt-1" {
		t.Fatalf("request id=%q", got)
	}
}
