package releasecontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultUpdaterSocketPath       = "/run/dirextalk-updater/http.sock"
	DefaultUpdaterControlTokenPath = "/etc/dirextalk-updater/control-token"
	ControlTokenHeader             = "X-Dirextalk-Control-Token"
	ControlStatusPath              = "/_dirextalk/updater/v1/control/status"
	ControlJobsPath                = "/_dirextalk/updater/v1/control/jobs"
	ControlDesiredStatePath        = "/_dirextalk/updater/v1/control/desired-state"
	ApplyConfirmation              = "apply_release_change"
	maxUpdaterResponseBytes        = 256 * 1024
)

var controllerErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var updaterJobIDPattern = regexp.MustCompile(`^job_[A-Za-z0-9_-]+$`)

type DesiredState string

const (
	DesiredStateRunning       DesiredState = "running"
	DesiredStateUpgrading     DesiredState = "upgrading"
	DesiredStateMaintenance   DesiredState = "maintenance"
	DesiredStateDeprovisioned DesiredState = "deprovisioned"
)

type StatusRequest struct {
	CurrentVersion             string `json:"current_version"`
	CurrentSchemaVersion       int    `json:"current_schema_version"`
	CurrentSchemaCompatVersion int    `json:"current_schema_compat_version"`
	ClientVersion              string `json:"client_version"`
}

type Operation struct {
	Kind          string `json:"kind"`
	PlanToken     string `json:"plan_token"`
	TargetVersion string `json:"target_version,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	Confirm       string `json:"confirm,omitempty"`
}

type UpdaterStatus struct {
	Available        bool               `json:"available"`
	ReleaseAvailable bool               `json:"release_available"`
	UpdateAvailable  bool               `json:"update_available"`
	DiscoveryStatus  string             `json:"discovery_status"`
	CheckedAt        string             `json:"checked_at,omitempty"`
	CurrentVersion   string             `json:"current_version"`
	LatestVersion    string             `json:"latest_version,omitempty"`
	ClientVersion    string             `json:"client_version,omitempty"`
	Compatibility    string             `json:"compatibility"`
	Reasons          []string           `json:"reasons"`
	ReleaseNotesURL  string             `json:"release_notes_url,omitempty"`
	Operations       []Operation        `json:"operations"`
	Watchdog         WatchdogStatus     `json:"watchdog"`
	Agent            AgentReleaseStatus `json:"agent"`
}

// AgentReleaseStatus is the updater-owned, non-secret Agent release
// projection. The updater observes the receipt-bound runtime and central
// Agent channel; message-server validates these values again before exposing
// them to Flutter.
type AgentReleaseStatus struct {
	Available            bool     `json:"available"`
	CurrentVersion       string   `json:"current_version,omitempty"`
	LatestVersion        string   `json:"latest_version,omitempty"`
	MinimumServerVersion string   `json:"minimum_server_version,omitempty"`
	UpdateAvailable      bool     `json:"update_available"`
	Reasons              []string `json:"reasons"`
}

type WatchdogStatus struct {
	Status         string `json:"status"`
	Degraded       bool   `json:"degraded"`
	CooldownUntil  string `json:"cooldown_until,omitempty"`
	LastObservedAt string `json:"last_observed_at,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
}

// ActiveJob is the public, token-free progress subset the updater returns to
// message-server status callers. It deliberately has no plan, image, digest,
// command, path, recovery point, or job bearer fields.
type ActiveJob struct {
	JobID            string `json:"job_id"`
	Status           string `json:"status"`
	CurrentVersion   string `json:"current_version,omitempty"`
	TargetVersion    string `json:"target_version,omitempty"`
	ServiceAvailable bool   `json:"service_available"`
}

// DirectStatus is the small v2 updater control response. It intentionally
// excludes release discovery, plans, and operations; central version lookup is
// performed by the message server at apply time instead.
type DirectStatus struct {
	Available      bool           `json:"available"`
	UpdaterReady   bool           `json:"updater_ready"`
	CurrentVersion string         `json:"current_version"`
	DesiredState   string         `json:"desired_state"`
	ActiveJob      *ActiveJob     `json:"active_job,omitempty"`
	Watchdog       WatchdogStatus `json:"watchdog"`
}

type ApplyRequest struct {
	PlanToken      string `json:"plan_token"`
	IdempotencyKey string `json:"idempotency_key"`
	Confirm        string `json:"confirm"`
}

// DirectApplyRequest is the v2 central-version upgrade command. The target is
// a release identifier only; callers cannot select infrastructure details.
type DirectApplyRequest struct {
	TargetVersion  string `json:"target_version"`
	IdempotencyKey string `json:"idempotency_key"`
	Confirm        string `json:"confirm"`
}

type JobTicket struct {
	JobID     string `json:"job_id"`
	JobToken  string `json:"job_token"`
	StatusURL string `json:"status_url"`
	Status    string `json:"status,omitempty"`
}

type Controller interface {
	Status(context.Context, StatusRequest) (UpdaterStatus, error)
	Apply(context.Context, ApplyRequest) (JobTicket, error)
	SetDesiredState(context.Context, DesiredState) error
}

// DirectController is implemented by current updaters. It is separate from
// Controller so legacy v1 plan-token behavior remains source compatible while
// v2 can fail closed when installed updater binaries have not been migrated.
type DirectController interface {
	StatusDirect(context.Context) (DirectStatus, error)
	ApplyDirect(context.Context, DirectApplyRequest) (JobTicket, error)
}

type ControllerError struct {
	Status  int
	Code    string
	Message string
}

func (e *ControllerError) Error() string {
	if e == nil {
		return "updater request failed"
	}
	if e.Message != "" {
		return e.Message
	}
	return "updater request failed"
}

type UnixControllerConfig struct {
	SocketPath       string
	ControlTokenPath string
	Timeout          time.Duration
}

type unixController struct {
	controlTokenPath string
	client           *http.Client
}

func NewUnixController(config UnixControllerConfig) Controller {
	socketPath := strings.TrimSpace(config.SocketPath)
	if socketPath == "" {
		socketPath = DefaultUpdaterSocketPath
	}
	controlTokenPath := strings.TrimSpace(config.ControlTokenPath)
	if controlTokenPath == "" {
		controlTokenPath = DefaultUpdaterControlTokenPath
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &unixController{
		controlTokenPath: filepath.Clean(controlTokenPath),
		client:           &http.Client{Transport: transport, Timeout: timeout},
	}
}

func (c *unixController) Status(ctx context.Context, request StatusRequest) (UpdaterStatus, error) {
	var status UpdaterStatus
	if err := c.post(ctx, ControlStatusPath, request, &status); err != nil {
		return UpdaterStatus{}, err
	}
	return status, nil
}

func (c *unixController) Apply(ctx context.Context, request ApplyRequest) (JobTicket, error) {
	var ticket JobTicket
	if err := c.post(ctx, ControlJobsPath, request, &ticket); err != nil {
		return JobTicket{}, err
	}
	if err := validateJobTicket(ticket); err != nil {
		return JobTicket{}, err
	}
	return ticket, nil
}

func (c *unixController) StatusDirect(ctx context.Context) (DirectStatus, error) {
	var status DirectStatus
	if err := c.post(ctx, ControlStatusPath, struct{}{}, &status); err != nil {
		return DirectStatus{}, err
	}
	return status, nil
}

func (c *unixController) ApplyDirect(ctx context.Context, request DirectApplyRequest) (JobTicket, error) {
	if _, err := CanonicalStableVersion("target_version", request.TargetVersion); err != nil {
		return JobTicket{}, &ControllerError{Status: http.StatusBadRequest, Code: "updater_request_invalid", Message: "updater request is invalid"}
	}
	parsedID, err := uuid.Parse(request.IdempotencyKey)
	if err != nil || parsedID.String() != request.IdempotencyKey || request.Confirm != ApplyConfirmation {
		return JobTicket{}, &ControllerError{Status: http.StatusBadRequest, Code: "updater_request_invalid", Message: "updater request is invalid"}
	}
	var ticket JobTicket
	if err := c.post(ctx, ControlJobsPath, request, &ticket); err != nil {
		return JobTicket{}, err
	}
	if err := validateJobTicket(ticket); err != nil {
		return JobTicket{}, err
	}
	return ticket, nil
}

func (c *unixController) SetDesiredState(ctx context.Context, state DesiredState) error {
	if !validDesiredState(state) {
		return &ControllerError{Status: http.StatusBadRequest, Code: "desired_state_invalid", Message: "desired state is invalid"}
	}
	return c.post(ctx, ControlDesiredStatePath, map[string]DesiredState{"desired_state": state}, nil)
}

func (c *unixController) post(ctx context.Context, path string, input, output any) error {
	tokenData, err := os.ReadFile(c.controlTokenPath)
	if err != nil {
		return &ControllerError{Status: http.StatusServiceUnavailable, Code: "updater_unavailable", Message: "updater control token is unavailable"}
	}
	token := strings.TrimSpace(string(tokenData))
	if token == "" {
		return &ControllerError{Status: http.StatusServiceUnavailable, Code: "updater_unavailable", Message: "updater control token is unavailable"}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return &ControllerError{Status: http.StatusInternalServerError, Code: "updater_request_invalid", Message: "updater request could not be encoded"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix"+path, bytes.NewReader(body))
	if err != nil {
		return &ControllerError{Status: http.StatusInternalServerError, Code: "updater_request_invalid", Message: "updater request could not be created"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ControlTokenHeader, token)
	response, err := c.client.Do(req)
	if err != nil {
		return &ControllerError{Status: http.StatusServiceUnavailable, Code: "updater_unavailable", Message: "updater is unavailable"}
	}
	defer response.Body.Close()
	reader := io.LimitReader(response.Body, maxUpdaterResponseBytes+1)
	data, err := io.ReadAll(reader)
	if err != nil || len(data) > maxUpdaterResponseBytes {
		return &ControllerError{Status: http.StatusBadGateway, Code: "updater_response_invalid", Message: "updater returned an invalid response"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeControllerError(response.StatusCode, data)
	}
	if output == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return &ControllerError{Status: http.StatusBadGateway, Code: "updater_response_invalid", Message: "updater returned an invalid response"}
	}
	return nil
}

func decodeControllerError(status int, data []byte) error {
	var response struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &response)
	code := strings.TrimSpace(response.Code)
	if !controllerErrorCodePattern.MatchString(code) {
		code = ""
	}
	if code == "" && controllerErrorCodePattern.MatchString(response.Error) {
		code = response.Error
	}
	if code == "" {
		code = "updater_rejected"
	}
	return &ControllerError{Status: status, Code: code, Message: "updater rejected the request"}
}

func validDesiredState(state DesiredState) bool {
	switch state {
	case DesiredStateRunning, DesiredStateUpgrading, DesiredStateMaintenance, DesiredStateDeprovisioned:
		return true
	default:
		return false
	}
}

func validateJobTicket(ticket JobTicket) error {
	jobID := strings.TrimSpace(ticket.JobID)
	if !updaterJobIDPattern.MatchString(jobID) || strings.TrimSpace(ticket.JobToken) == "" {
		return &ControllerError{Status: http.StatusBadGateway, Code: "updater_response_invalid", Message: "updater returned an invalid job ticket"}
	}
	parsed, err := url.Parse(strings.TrimSpace(ticket.StatusURL))
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/_dirextalk/updater/v1/jobs/"+jobID {
		return &ControllerError{Status: http.StatusBadGateway, Code: "updater_response_invalid", Message: "updater returned an invalid job status URL"}
	}
	return nil
}

func AsControllerError(err error) (*ControllerError, bool) {
	var controllerErr *ControllerError
	if !errors.As(err, &controllerErr) {
		return nil, false
	}
	return controllerErr, true
}

func NormalizeClientVersion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "v") {
		value = "v" + value
	}
	if _, err := parseCanonicalVersion("client_version", value); err != nil {
		return "", fmt.Errorf("client_version is invalid: %w", err)
	}
	return value, nil
}
