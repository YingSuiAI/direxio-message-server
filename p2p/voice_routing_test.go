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

func TestVoiceCallbackRequestIDDoesNotUseTaskID(t *testing.T) {
	withoutMessages := map[string]any{"task_id": "task-only", "session_id": "voice-1"}
	if got := voiceCallbackRequestID(withoutMessages); got == "" {
		t.Fatal("history fallback must remain retry-stable")
	}
	if got := voiceCallbackRequestID(map[string]any{"request_id": "req-1", "task_id": "task"}); got != "req-1" {
		t.Fatalf("request id=%q", got)
	}
	first := voiceCallbackRequestID(map[string]any{"messages": []any{map[string]any{"content": "hello"}}, "session_id": "voice-1", "task_id": "task-a"})
	retry := voiceCallbackRequestID(map[string]any{"messages": []any{map[string]any{"content": "hello"}}, "session_id": "voice-1", "task_id": "task-b"})
	if first == "" || first != retry {
		t.Fatalf("history fallback changed with task id: %q != %q", first, retry)
	}
	if changed := voiceCallbackRequestID(map[string]any{"messages": []any{map[string]any{"content": "hello"}}, "session_id": "voice-2", "task_id": "task-a"}); changed == first {
		t.Fatal("session marker must participate in history digest")
	}
}
