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

func TestUnixControllerUsesUnifiedReleaseV2Contract(t *testing.T) {
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

	requests := make(chan struct {
		path string
		body map[string]any
	}, 4)
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
			_, _ = w.Write([]byte(`{"available":true,"updater_ready":true,"current_version":"v1.0.3","desired_state":"running","active_job":{"job_id":"job_active","component":"agent","status":"pulling","current_version":"v1.0.2","target_version":"v1.0.3","service_available":true,"secret":"discard"},"watchdog":{"status":"healthy","degraded":false},"agent":{"available":true,"current_version":"v1.0.2","reasons":[]},"image":"discard"}`))
		case ControlJobsPath:
			_, _ = w.Write([]byte(`{"job_id":"job_test","job_token":"job-secret","status_url":"/_dirextalk/updater/v1/jobs/job_test","status":"queued"}`))
		case ControlDesiredStatePath:
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	controller := NewUnixController(UnixControllerConfig{SocketPath: socketPath, ControlTokenPath: tokenPath})
	status, err := controller.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || !status.UpdaterReady || status.CurrentVersion != "v1.0.3" || status.Agent.CurrentVersion != "v1.0.2" || status.ActiveJob == nil || status.ActiveJob.Component != "agent" {
		t.Fatalf("unexpected status: %#v", status)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "discard") || strings.Contains(string(raw), `"image"`) || strings.Contains(string(raw), `"secret"`) {
		t.Fatalf("unsafe updater fields entered DTO: %s", raw)
	}

	serverRequest := ApplyRequest{
		Component: ReleaseComponentServer, TargetVersion: "dev1.0.4", MinimumServerVersion: "",
		IdempotencyKey: "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", Confirm: ApplyConfirmation,
	}
	if _, err := controller.Apply(context.Background(), serverRequest); err != nil {
		t.Fatalf("server apply: %v", err)
	}
	agentRequest := ApplyRequest{
		Component: ReleaseComponentAgent, TargetVersion: "v1.0.4", MinimumServerVersion: "v1.0.3",
		IdempotencyKey: "41a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e", Confirm: ApplyConfirmation,
	}
	if _, err := controller.Apply(context.Background(), agentRequest); err != nil {
		t.Fatalf("Agent apply: %v", err)
	}
	if err := controller.SetDesiredState(context.Background(), DesiredStateDeprovisioned); err != nil {
		t.Fatal(err)
	}

	statusCall := <-requests
	serverCall := <-requests
	agentCall := <-requests
	desiredCall := <-requests
	if statusCall.path != ControlStatusPath || len(statusCall.body) != 0 {
		t.Fatalf("status request must be empty: %#v", statusCall)
	}
	assertApplyBody(t, serverCall, "server", "dev1.0.4", "", serverRequest.IdempotencyKey)
	assertApplyBody(t, agentCall, "agent", "v1.0.4", "v1.0.3", agentRequest.IdempotencyKey)
	if desiredCall.path != ControlDesiredStatePath || desiredCall.body["desired_state"] != "deprovisioned" {
		t.Fatalf("unexpected desired-state request: %#v", desiredCall)
	}
}

func assertApplyBody(t *testing.T, call struct {
	path string
	body map[string]any
}, component, target, minimum, idempotencyKey string) {
	t.Helper()
	if call.path != ControlJobsPath || len(call.body) != 5 || call.body["component"] != component || call.body["target_version"] != target || call.body["minimum_server_version"] != minimum || call.body["idempotency_key"] != idempotencyKey || call.body["confirm"] != ApplyConfirmation {
		t.Fatalf("unexpected apply request: %#v", call)
	}
}

func TestUnixControllerRejectsInvalidUnifiedApplyRequests(t *testing.T) {
	controller := NewUnixController(UnixControllerConfig{})
	validID := "31a20813-c5d9-4f6d-b4f0-cdf8cfc75c6e"
	for name, request := range map[string]ApplyRequest{
		"component":      {Component: "other", TargetVersion: "v1.0.1", IdempotencyKey: validID, Confirm: ApplyConfirmation},
		"server_minimum": {Component: ReleaseComponentServer, TargetVersion: "v1.0.1", MinimumServerVersion: "v1.0.0", IdempotencyKey: validID, Confirm: ApplyConfirmation},
		"agent_minimum":  {Component: ReleaseComponentAgent, TargetVersion: "v1.0.1", IdempotencyKey: validID, Confirm: ApplyConfirmation},
		"agent_dev":      {Component: ReleaseComponentAgent, TargetVersion: "dev1.0.1", MinimumServerVersion: "v1.0.0", IdempotencyKey: validID, Confirm: ApplyConfirmation},
		"uuid":           {Component: ReleaseComponentServer, TargetVersion: "v1.0.1", IdempotencyKey: strings.ToUpper(validID), Confirm: ApplyConfirmation},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := controller.Apply(context.Background(), request)
			controllerErr, ok := AsControllerError(err)
			if !ok || controllerErr.Status != http.StatusBadRequest || controllerErr.Code != "updater_request_invalid" {
				t.Fatalf("unexpected error: %#v", err)
			}
		})
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
	_, err := controller.Status(context.Background())
	if err == nil || strings.Contains(err.Error(), "control-secret") {
		t.Fatalf("expected redacted unavailable error, got %v", err)
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
