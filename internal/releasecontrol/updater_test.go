package releasecontrol

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnixControllerStatusApplyAndDesiredState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	socketPath := shortUnixSocketPath(t)
	tokenPath := filepath.Join(dir, "control-token")
	if err := os.WriteFile(tokenPath, []byte("control-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	requests := make(chan map[string]any, 3)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ControlTokenHeader) != "control-secret" {
			http.Error(w, "missing control token", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		body["path"] = r.URL.Path
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case ControlStatusPath:
			_, _ = w.Write([]byte(`{"available":true,"release_available":true,"update_available":true,"discovery_status":"fresh","checked_at":"2026-07-10T12:00:00Z","current_version":"v1.0.0","latest_version":"v1.1.0","client_version":"v1.2.0","compatibility":"compatible","reasons":[],"release_notes_url":"https://github.com/YingSuiAI/dirextalk-message-server/releases/tag/v1.1.0","operations":[{"kind":"upgrade","plan_token":"opaque-plan","target_version":"v1.1.0","expires_at":"2026-07-10T12:15:00Z","confirm":"apply_release_change","future":"ignored"}],"watchdog":{"status":"degraded","degraded":true,"cooldown_until":"2026-07-10T12:15:00Z","last_observed_at":"2026-07-10T12:00:00Z","error_code":"repair_failed","attempts":["must-not-forward"]},"image":"must-not-forward","digest":"must-not-forward","future":"ignored"}`))
		case ControlJobsPath:
			_, _ = w.Write([]byte(`{"job_id":"job_test","job_token":"job-secret","status_url":"/_dirextalk/updater/v1/jobs/job_test","future":"ignored"}`))
		case ControlDesiredStatePath:
			_, _ = w.Write([]byte(`{"ok":true,"future":"ignored"}`))
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	controller := NewUnixController(UnixControllerConfig{SocketPath: socketPath, ControlTokenPath: tokenPath})
	status, err := controller.Status(context.Background(), StatusRequest{
		CurrentVersion:             "v1.0.0",
		CurrentSchemaVersion:       1,
		CurrentSchemaCompatVersion: 1,
		ClientVersion:              "v1.2.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || !status.ReleaseAvailable || status.Compatibility != "compatible" || len(status.Operations) != 1 || status.Operations[0].Kind != "upgrade" || status.Operations[0].PlanToken != "opaque-plan" || status.Operations[0].Confirm != ApplyConfirmation {
		t.Fatalf("unexpected status: %#v", status)
	}
	if status.Watchdog.Status != "degraded" || !status.Watchdog.Degraded || status.Watchdog.ErrorCode != "repair_failed" {
		t.Fatalf("unexpected watchdog status: %#v", status.Watchdog)
	}
	statusRaw, _ := json.Marshal(status)
	if strings.Contains(string(statusRaw), "must-not-forward") || strings.Contains(string(statusRaw), `"image"`) || strings.Contains(string(statusRaw), `"digest"`) || strings.Contains(string(statusRaw), `"attempts"`) {
		t.Fatalf("unsafe updater fields entered backend status: %s", statusRaw)
	}
	ticket, err := controller.Apply(context.Background(), ApplyRequest{PlanToken: "opaque-plan", IdempotencyKey: "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", Confirm: ApplyConfirmation})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.JobID != "job_test" || ticket.JobToken != "job-secret" || ticket.StatusURL != "/_dirextalk/updater/v1/jobs/job_test" {
		t.Fatalf("unexpected job ticket: %#v", ticket)
	}
	if err := controller.SetDesiredState(context.Background(), DesiredStateDeprovisioned); err != nil {
		t.Fatal(err)
	}

	statusRequest := <-requests
	applyRequest := <-requests
	desiredRequest := <-requests
	if statusRequest["path"] != ControlStatusPath || statusRequest["current_version"] != "v1.0.0" || statusRequest["client_version"] != "v1.2.0" {
		t.Fatalf("unexpected status request: %#v", statusRequest)
	}
	if applyRequest["path"] != ControlJobsPath || applyRequest["plan_token"] != "opaque-plan" || applyRequest["confirm"] != ApplyConfirmation {
		t.Fatalf("unexpected apply request: %#v", applyRequest)
	}
	if desiredRequest["path"] != ControlDesiredStatePath || desiredRequest["desired_state"] != string(DesiredStateDeprovisioned) {
		t.Fatalf("unexpected desired-state request: %#v", desiredRequest)
	}
	for _, request := range []map[string]any{statusRequest, applyRequest, desiredRequest} {
		raw, _ := json.Marshal(request)
		if strings.Contains(string(raw), "control-secret") || strings.Contains(string(raw), "job-secret") {
			t.Fatalf("secret leaked into request body: %s", raw)
		}
	}
}

func TestUnixControllerErrorsNeverContainControlToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "control-token")
	if err := os.WriteFile(tokenPath, []byte("control-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := NewUnixController(UnixControllerConfig{SocketPath: filepath.Join(dir, "missing.sock"), ControlTokenPath: tokenPath})
	_, err := controller.Status(context.Background(), StatusRequest{})
	if err == nil || strings.Contains(err.Error(), "control-secret") {
		t.Fatalf("expected redacted unavailable error, got %v", err)
	}
}

func TestStatusRequestAlwaysReportsClientVersionField(t *testing.T) {
	raw, err := json.Marshal(StatusRequest{CurrentVersion: "v1.0.0", CurrentSchemaVersion: 1, CurrentSchemaCompatVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"client_version":""`) {
		t.Fatalf("empty client version must still be sent to updater: %s", raw)
	}
}

func TestUnixControllerMapsStructuredErrorWithoutEchoingSecrets(t *testing.T) {
	dir := t.TempDir()
	socketPath := shortUnixSocketPath(t)
	tokenPath := filepath.Join(dir, "control-token")
	if err := os.WriteFile(tokenPath, []byte("control-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"plan_invalid_or_expired","message":"job-secret must never escape"}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	controller := NewUnixController(UnixControllerConfig{SocketPath: socketPath, ControlTokenPath: tokenPath})
	_, err = controller.Apply(context.Background(), ApplyRequest{PlanToken: "opaque", IdempotencyKey: "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", Confirm: ApplyConfirmation})
	controllerErr, ok := AsControllerError(err)
	if !ok || controllerErr.Status != http.StatusConflict || controllerErr.Code != "plan_invalid_or_expired" {
		t.Fatalf("unexpected structured error: %#v err=%v", controllerErr, err)
	}
	if strings.Contains(err.Error(), "job-secret") || strings.Contains(err.Error(), "control-secret") {
		t.Fatalf("secret leaked through updater error: %v", err)
	}
}

func TestUnixControllerDirectStatusAndApplyUseOnlyV2Fields(t *testing.T) {
	dir := t.TempDir()
	socketPath := shortUnixSocketPath(t)
	tokenPath := filepath.Join(dir, "control-token")
	if err := os.WriteFile(tokenPath, []byte("control-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	requests := make(chan struct {
		path string
		body map[string]any
	}, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ControlTokenHeader) != "control-secret" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		requests <- struct {
			path string
			body map[string]any
		}{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case ControlStatusPath:
			_, _ = w.Write([]byte(`{"available":true,"updater_ready":true,"current_version":"v1.0.3","desired_state":"running","active_job":{"job_id":"job_active","status":"pulling","current_version":"v1.0.2","target_version":"v1.0.3","service_available":true,"plan_token":"must-not-forward"},"watchdog":{"status":"healthy","degraded":false}}`))
		case ControlJobsPath:
			_, _ = w.Write([]byte(`{"job_id":"job_test","job_token":"job-secret","status_url":"/_dirextalk/updater/v1/jobs/job_test","status":"queued"}`))
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	controller := NewUnixController(UnixControllerConfig{SocketPath: socketPath, ControlTokenPath: tokenPath})
	direct, ok := controller.(DirectController)
	if !ok {
		t.Fatal("unix controller must implement DirectController")
	}
	status, err := direct.StatusDirect(context.Background())
	if err != nil {
		t.Fatalf("direct status: %v", err)
	}
	if !status.Available || !status.UpdaterReady || status.CurrentVersion != "v1.0.3" || status.ActiveJob == nil || status.ActiveJob.JobID != "job_active" {
		t.Fatalf("unexpected direct status: %#v", status)
	}
	statusRaw, _ := json.Marshal(status)
	if strings.Contains(string(statusRaw), "must-not-forward") || strings.Contains(string(statusRaw), "plan_token") {
		t.Fatalf("unsafe direct-status fields entered message-server DTO: %s", statusRaw)
	}
	ticket, err := direct.ApplyDirect(context.Background(), DirectApplyRequest{
		TargetVersion: "dev1.0.4", IdempotencyKey: "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", Confirm: ApplyConfirmation,
	})
	if err != nil {
		t.Fatalf("direct apply: %v", err)
	}
	if ticket.Status != "queued" || ticket.JobID != "job_test" {
		t.Fatalf("unexpected direct ticket: %#v", ticket)
	}
	if _, err := direct.ApplyDirect(context.Background(), DirectApplyRequest{
		TargetVersion: "v1.0.4", IdempotencyKey: "31A20813-C5D9-4F6D-B4F0-CDF8CFC75C6E", Confirm: ApplyConfirmation,
	}); err == nil {
		t.Fatal("direct apply accepted a noncanonical UUID")
	}
	if _, err := direct.ApplyDirect(context.Background(), DirectApplyRequest{
		TargetVersion: "dev01.0.4", IdempotencyKey: "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", Confirm: ApplyConfirmation,
	}); err == nil {
		t.Fatal("direct apply accepted a noncanonical development version")
	}

	statusRequest := <-requests
	applyRequest := <-requests
	if statusRequest.path != ControlStatusPath || len(statusRequest.body) != 0 {
		t.Fatalf("direct status must send exactly an empty object: %#v", statusRequest)
	}
	if applyRequest.path != ControlJobsPath || applyRequest.body["target_version"] != "dev1.0.4" || applyRequest.body["plan_token"] != nil || len(applyRequest.body) != 3 {
		t.Fatalf("unexpected direct apply request: %#v", applyRequest)
	}
	if applyRequest.body["idempotency_key"] != "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e" || applyRequest.body["confirm"] != ApplyConfirmation {
		t.Fatalf("unexpected direct apply request: %#v", applyRequest)
	}
}

func shortUnixSocketPath(t *testing.T) string {
	t.Helper()
	file, err := os.CreateTemp("", "dtx-updater-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}
