package nativeagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestScheduledRunnerIsClosedAndUsesPinnedKeyOnlyDuringExecute(t *testing.T) {
	const key = "schedule-canary-key"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
	}))
	defer server.Close()
	runner, err := NewScheduledRunner([]Tool{
		{Name: "dirextalk_contacts_list", Handler: func(context.Context, map[string]any) (any, error) { return map[string]any{}, nil }},
		{Name: "dirextalk_messages_send", Write: true, Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }},
		{Name: "native_agent_skills_install", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatal("runner construction must not use a credential")
	}
	profile := storage.ModelProfile{Provider: "openai", Model: "test", BaseURL: server.URL, APIKey: key}
	if _, err := runner.ExecuteScheduled(context.Background(), "hello", profile, []string{"dirextalk_messages_send"}); err == nil {
		t.Fatal("message write tool was accepted")
	}
	if _, err := runner.ExecuteScheduled(context.Background(), "hello", profile, []string{"native_agent_skills_install"}); err == nil {
		t.Fatal("install tool was accepted")
	}
	if _, err := runner.ExecuteScheduled(context.Background(), "hello", profile, []string{"unknown"}); err == nil {
		t.Fatal("unknown tool was accepted")
	}
	if requests != 0 {
		t.Fatal("rejected executions must not reach the model")
	}
	result, err := runner.ExecuteScheduled(context.Background(), "hello", profile, nil)
	if err != nil || result != "done" {
		t.Fatalf("execute = %q, %v", result, err)
	}
	if requests != 1 {
		t.Fatalf("model requests = %d", requests)
	}
}

func TestSanitizeScheduledTextRedactsCanaryAndCredentialPatterns(t *testing.T) {
	secret := "canary-secret-value"
	got := SanitizeScheduledText(fmt.Sprintf("output %s bearer abcdefghijkl api_key=%s", secret, secret), secret)
	if strings.Contains(got, secret) || strings.Contains(strings.ToLower(got), "bearer abc") {
		t.Fatalf("secret leaked: %q", got)
	}
}

func TestScheduledRunnerRejectsMiswiredAllowedWriteTool(t *testing.T) {
	if _, err := NewScheduledRunner([]Tool{{Name: "dirextalk_contacts_list", Write: true, Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }}}); err == nil {
		t.Fatal("write-capable allowlisted tool made the runner ready")
	}
}
