// Package release owns the ProductCore release status, apply, and client-build
// reporting workflows.
package release

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/releasecontrol"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/google/uuid"
)

var releaseJobIDPattern = regexp.MustCompile(`^job_[A-Za-z0-9_-]{1,124}$`)

const (
	ClientSessionStaleCode = "client_session_stale"
	UpdaterUnavailableCode = "updater_unavailable"

	actionClientVersionReport = "client.version.report"
	actionReleaseStatus       = "release.v2.status"
	actionReleaseApply        = "release.v2.apply"
	v2StatusInvalidParamsCode = "release_v2_status_invalid_params"
	v2ApplyInvalidParamsCode  = "release_v2_apply_invalid_params"
	clientVersionIncompatible = "client_version_incompatible"
	serverVersionIncompatible = "server_version_incompatible"
	agentVersionUnavailable   = "agent_current_version_unavailable"
	releaseTargetMismatch     = "release_target_mismatch"
	releaseTargetNotNewer     = "release_target_not_newer"
)

type Session struct {
	DeviceID   string
	Generation uint64
}

type Snapshot struct {
	DeviceID   string
	Generation uint64
	Client     dirextalkdomain.ClientBuild
	Controller releasecontrol.Controller
}

// StatePort keeps the module on the Service's single portal-state instance and
// preserves the existing device-scoped durable CAS.
type StatePort interface {
	Session(context.Context) (Session, bool)
	Snapshot() Snapshot
	SaveClientBuild(context.Context, string, dirextalkdomain.ClientBuild) (bool, error)
	CommitClientBuild(dirextalkdomain.ClientBuild) string
}

type Config struct {
	SessionLocker             sync.Locker
	Now                       func() time.Time
	CentralVersionSource      releasecontrol.CentralVersionSource
	CentralAgentVersionSource releasecontrol.CentralAgentVersionSource
	AgentVersionSource        releasecontrol.AgentVersionSource
}

type Module struct {
	state              StatePort
	cfg                Config
	centralSource      releasecontrol.CentralVersionSource
	centralAgentSource releasecontrol.CentralAgentVersionSource
	agentVersionSource releasecontrol.AgentVersionSource
}

func New(state StatePort, cfg Config) *Module {
	centralSource := cfg.CentralVersionSource
	if centralSource == nil {
		centralSource = releasecontrol.NewCentralVersionSource(releasecontrol.CentralVersionSourceConfig{})
	}
	centralAgentSource := cfg.CentralAgentVersionSource
	if centralAgentSource == nil {
		centralAgentSource = releasecontrol.NewCentralAgentVersionSource(releasecontrol.CentralVersionSourceConfig{})
	}
	return &Module{state: state, cfg: cfg, centralSource: centralSource, centralAgentSource: centralAgentSource, agentVersionSource: cfg.AgentVersionSource}
}

func (m *Module) Handlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{
		actionClientVersionReport: m.reportClientVersion,
		actionReleaseStatus:       m.status,
		actionReleaseApply:        m.apply,
	}
}

func (m *Module) reportClientVersion(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m.cfg.SessionLocker != nil {
		m.cfg.SessionLocker.Lock()
		defer m.cfg.SessionLocker.Unlock()
	}
	identity, ok := m.state.Session(ctx)
	if !ok {
		return nil, staleSessionError()
	}
	snapshot := m.state.Snapshot()
	if identity.DeviceID != snapshot.DeviceID || identity.Generation != snapshot.Generation {
		return nil, staleSessionError()
	}
	values := actionbase.Params(params)
	version, err := releasecontrol.NormalizeClientVersion(values.String("client_version"))
	if err != nil {
		return nil, actionbase.CodedError(http.StatusBadRequest, "client_version_invalid", "client_version must be a stable semantic version")
	}
	buildNumber, ok := optionalText(values.Raw("build_number"), 64)
	if !ok {
		return nil, actionbase.CodedError(http.StatusBadRequest, "client_build_invalid", "build_number is invalid")
	}
	platform, ok := optionalText(values.Raw("platform"), 64)
	if !ok {
		return nil, actionbase.CodedError(http.StatusBadRequest, "client_platform_invalid", "platform is invalid")
	}
	reportedAt := m.now().Format(time.RFC3339Nano)
	build := dirextalkdomain.ClientBuild{Version: version, BuildNumber: buildNumber, Platform: platform, ReportedAt: reportedAt}
	updated, err := m.state.SaveClientBuild(ctx, identity.DeviceID, build)
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	if !updated {
		return nil, staleSessionError()
	}
	deviceID := m.state.CommitClientBuild(build)
	return map[string]any{
		"client_version": version,
		"build_number":   buildNumber,
		"platform":       platform,
		"device_id":      deviceID,
		"reported_at":    reportedAt,
	}, nil
}

func (m *Module) status(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if len(params) != 0 {
		return nil, actionbase.CodedError(http.StatusBadRequest, v2StatusInvalidParamsCode, "release v2 status does not accept parameters")
	}
	buildInfo := internal.CurrentBuildInfo()
	snapshot := m.state.Snapshot()
	type updaterResult struct {
		status releasecontrol.Status
		err    error
	}
	type centralResult struct {
		version releasecontrol.CentralAgentVersion
		err     error
	}
	type agentResult struct {
		version string
		err     error
	}
	updaterCh := make(chan updaterResult, 1)
	centralCh := make(chan centralResult, 1)
	agentCh := make(chan agentResult, 1)
	go func() {
		if snapshot.Controller == nil {
			updaterCh <- updaterResult{err: context.Canceled}
			return
		}
		status, err := snapshot.Controller.Status(ctx)
		updaterCh <- updaterResult{status: status, err: err}
	}()
	go func() {
		version, err := m.centralAgentSource.CurrentAgentVersion(ctx)
		centralCh <- centralResult{version: version, err: err}
	}()
	go func() {
		if m.agentVersionSource == nil {
			agentCh <- agentResult{err: &releasecontrol.AgentVersionError{Code: releasecontrol.AgentVersionUnavailableCode}}
			return
		}
		version, err := m.agentVersionSource.CurrentAgentVersion(ctx)
		agentCh <- agentResult{version: version, err: err}
	}()
	updater := <-updaterCh
	central := <-centralCh
	agent := <-agentCh
	centralReason := ""
	if central.err != nil || validateCentralAgentVersion(central.version) != nil {
		centralReason = centralVersionReason(central.err)
	}
	return releaseStatusMap(updater.status, updater.err, agent.version, agent.err, central.version, centralReason, buildInfo.Version, snapshot.Client.Version), nil
}

func (m *Module) apply(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	request, apiErr := validateV2ApplyRequest(params)
	if apiErr != nil {
		return nil, apiErr
	}
	snapshot := m.state.Snapshot()
	controller := snapshot.Controller
	if controller == nil {
		return nil, unavailableError()
	}
	buildInfo := internal.CurrentBuildInfo()
	updaterStatus, err := controller.Status(ctx)
	if err != nil || !validUpdaterStatus(updaterStatus) || !updaterStatus.Available || !updaterStatus.UpdaterReady {
		return nil, unavailableError()
	}
	switch request.Component {
	case releasecontrol.ReleaseComponentServer:
		if apiErr := m.gateServerUpdate(ctx, snapshot, buildInfo.Version, &request); apiErr != nil {
			return nil, apiErr
		}
	case releasecontrol.ReleaseComponentAgent:
		if apiErr := m.gateAgentUpdate(ctx, buildInfo.Version, &request); apiErr != nil {
			return nil, apiErr
		}
	default:
		return nil, v2InvalidParamsError()
	}
	ticket, err := controller.Apply(ctx, request)
	if err != nil {
		return nil, controllerError(err)
	}
	return map[string]any{
		"job_id": ticket.JobID, "job_token": ticket.JobToken,
		"status_url": ticket.StatusURL, "status": ticket.Status,
	}, nil
}

func (m *Module) gateServerUpdate(ctx context.Context, snapshot Snapshot, runningVersion string, request *releasecontrol.ApplyRequest) *actionbase.Error {
	central, err := m.centralSource.CurrentServerVersion(ctx)
	if err != nil || validateCentralServerVersion(central) != nil {
		return centralVersionError(err)
	}
	if request.TargetVersion != central.Version {
		return actionbase.CodedError(http.StatusConflict, releaseTargetMismatch, "target_version no longer matches the central server version")
	}
	clientVersion, err := releasecontrol.CanonicalStableVersion("client_version", snapshot.Client.Version)
	if err != nil {
		return actionbase.CodedError(http.StatusConflict, clientVersionIncompatible, "current client version is not compatible with the server update")
	}
	comparison, err := releasecontrol.CompareCanonicalStableVersions(clientVersion, central.PreVersion)
	if err != nil || comparison < 0 {
		return actionbase.CodedError(http.StatusConflict, clientVersionIncompatible, "current client version is not compatible with the server update")
	}
	comparison, err = releasecontrol.CompareCanonicalServerVersions(request.TargetVersion, runningVersion)
	if err != nil || comparison <= 0 {
		return actionbase.CodedError(http.StatusConflict, releaseTargetNotNewer, "target_version must be newer than the running server")
	}
	request.MinimumServerVersion = ""
	return nil
}

func (m *Module) gateAgentUpdate(ctx context.Context, runningVersion string, request *releasecontrol.ApplyRequest) *actionbase.Error {
	central, err := m.centralAgentSource.CurrentAgentVersion(ctx)
	if err != nil || validateCentralAgentVersion(central) != nil {
		return centralVersionError(err)
	}
	if request.TargetVersion != central.Version {
		return actionbase.CodedError(http.StatusConflict, releaseTargetMismatch, "target_version no longer matches the central Agent version")
	}
	serverComparison, err := releasecontrol.CompareCanonicalStableVersions(runningVersion, central.PreVersion)
	if err != nil || serverComparison < 0 {
		return actionbase.CodedError(http.StatusConflict, serverVersionIncompatible, "running server version is not compatible with the Agent update")
	}
	if m.agentVersionSource == nil {
		return actionbase.CodedError(http.StatusConflict, agentVersionUnavailable, "current Agent version is unavailable")
	}
	observed, err := m.agentVersionSource.CurrentAgentVersion(ctx)
	if err != nil {
		return actionbase.CodedError(http.StatusConflict, agentVersionUnavailable, "current Agent version is unavailable")
	}
	current, err := releasecontrol.CanonicalStableVersion("agent.current_version", observed)
	if err != nil {
		return actionbase.CodedError(http.StatusConflict, agentVersionUnavailable, "current Agent version is unavailable")
	}
	comparison, err := releasecontrol.CompareCanonicalStableVersions(request.TargetVersion, current)
	if err != nil || comparison <= 0 {
		return actionbase.CodedError(http.StatusConflict, releaseTargetNotNewer, "target_version must be newer than the running Agent")
	}
	request.MinimumServerVersion = central.PreVersion
	return nil
}

func (m *Module) SetDesiredState(ctx context.Context, state releasecontrol.DesiredState) *actionbase.Error {
	controller := m.state.Snapshot().Controller
	if controller == nil {
		return unavailableError()
	}
	if err := controller.SetDesiredState(ctx, state); err != nil {
		return controllerError(err)
	}
	return nil
}

func (m *Module) now() time.Time {
	if m.cfg.Now == nil {
		return time.Now().UTC()
	}
	return m.cfg.Now().UTC()
}

func staleSessionError() *actionbase.Error {
	return actionbase.CodedError(http.StatusUnauthorized, ClientSessionStaleCode, "client session is stale")
}

func unavailableError() *actionbase.Error {
	return actionbase.CodedError(http.StatusServiceUnavailable, UpdaterUnavailableCode, "updater is unavailable")
}

func validateCentralAgentVersion(version releasecontrol.CentralAgentVersion) error {
	if version.AppID != "1" || version.ChannelID != "agents" {
		return &releasecontrol.CentralVersionError{Code: releasecontrol.CentralVersionInvalidCode, Message: "central version response is invalid"}
	}
	if _, err := releasecontrol.CanonicalStableVersion("agent_version", version.Version); err != nil {
		return err
	}
	if _, err := releasecontrol.CanonicalStableVersion("agent_minimum_server_version", version.PreVersion); err != nil {
		return err
	}
	return nil
}

func validateV2ApplyRequest(params map[string]any) (releasecontrol.ApplyRequest, *actionbase.Error) {
	allowed := map[string]struct{}{"component": {}, "target_version": {}, "idempotency_key": {}, "confirm": {}}
	for key := range params {
		if _, ok := allowed[key]; !ok {
			return releasecontrol.ApplyRequest{}, v2InvalidParamsError()
		}
	}
	componentText, ok := exactString(params["component"])
	if !ok {
		return releasecontrol.ApplyRequest{}, v2InvalidParamsError()
	}
	component := releasecontrol.ReleaseComponent(componentText)
	if component != releasecontrol.ReleaseComponentServer && component != releasecontrol.ReleaseComponentAgent {
		return releasecontrol.ApplyRequest{}, v2InvalidParamsError()
	}
	targetVersion, ok := exactString(params["target_version"])
	if !ok {
		return releasecontrol.ApplyRequest{}, v2InvalidParamsError()
	}
	var err error
	if component == releasecontrol.ReleaseComponentAgent {
		targetVersion, err = releasecontrol.CanonicalStableVersion("target_version", targetVersion)
	} else {
		targetVersion, err = releasecontrol.CanonicalServerVersion("target_version", targetVersion)
	}
	if err != nil {
		return releasecontrol.ApplyRequest{}, v2InvalidParamsError()
	}
	idempotencyKey, ok := exactString(params["idempotency_key"])
	if !ok {
		return releasecontrol.ApplyRequest{}, v2InvalidParamsError()
	}
	parsedUUID, err := uuid.Parse(idempotencyKey)
	if err != nil || parsedUUID.String() != idempotencyKey {
		return releasecontrol.ApplyRequest{}, v2InvalidParamsError()
	}
	confirm, ok := exactString(params["confirm"])
	if !ok || confirm != releasecontrol.ApplyConfirmation {
		return releasecontrol.ApplyRequest{}, v2InvalidParamsError()
	}
	return releasecontrol.ApplyRequest{
		Component: component, TargetVersion: targetVersion, IdempotencyKey: idempotencyKey, Confirm: confirm,
	}, nil
}

func exactString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || text == "" || text != strings.TrimSpace(text) {
		return "", false
	}
	return text, true
}

func v2InvalidParamsError() *actionbase.Error {
	return actionbase.CodedError(http.StatusBadRequest, v2ApplyInvalidParamsCode, "release v2 apply accepts only component, target_version, idempotency_key, and confirm")
}

func validateCentralServerVersion(version releasecontrol.CentralServerVersion) error {
	if version.AppID != "1" || version.ChannelID != "server" {
		return &releasecontrol.CentralVersionError{Code: releasecontrol.CentralVersionInvalidCode, Message: "central version response is invalid"}
	}
	if _, err := releasecontrol.CanonicalServerVersion("version", version.Version); err != nil {
		return &releasecontrol.CentralVersionError{Code: releasecontrol.CentralVersionInvalidCode, Message: "central version response is invalid"}
	}
	if _, err := releasecontrol.CanonicalStableVersion("pre_version", version.PreVersion); err != nil {
		return &releasecontrol.CentralVersionError{Code: releasecontrol.CentralVersionInvalidCode, Message: "central version response is invalid"}
	}
	return nil
}

func centralVersionError(err error) *actionbase.Error {
	if centralErr, ok := releasecontrol.AsCentralVersionError(err); ok {
		switch centralErr.Code {
		case releasecontrol.CentralVersionInvalidCode:
			return actionbase.CodedError(http.StatusBadGateway, centralErr.Code, "central version response is invalid")
		case releasecontrol.CentralVersionUnavailableCode:
			return actionbase.CodedError(http.StatusServiceUnavailable, centralErr.Code, "central version service is unavailable")
		}
	}
	return actionbase.CodedError(http.StatusBadGateway, releasecontrol.CentralVersionInvalidCode, "central version response is invalid")
}

func centralVersionReason(err error) string {
	if centralErr, ok := releasecontrol.AsCentralVersionError(err); ok {
		return centralErr.Code
	}
	return releasecontrol.CentralVersionInvalidCode
}

func releaseStatusMap(status releasecontrol.Status, statusErr error, agentVersion string, agentErr error, central releasecontrol.CentralAgentVersion, centralReason, currentVersion, clientVersion string) map[string]any {
	valid := statusErr == nil && validUpdaterStatus(status)
	updaterAvailable := valid && status.Available
	updaterReady := updaterAvailable && status.UpdaterReady
	desiredState := "unknown"
	var activeJob any
	watchdog := watchdogMap(releasecontrol.WatchdogStatus{})
	if valid {
		desiredState = normalizedDesiredState(status.DesiredState)
		activeJob = activeJobMap(status.ActiveJob)
		watchdog = watchdogMap(status.Watchdog)
	}
	return map[string]any{
		"available":         true,
		"current_version":   currentVersion,
		"client_version":    clientVersion,
		"updater_available": updaterAvailable,
		"updater_ready":     updaterReady,
		"desired_state":     desiredState,
		"active_job":        activeJob,
		"watchdog":          watchdog,
		"agent":             agentStatusMap(agentVersion, agentErr, central, centralReason, currentVersion),
	}
}

func validUpdaterStatus(status releasecontrol.Status) bool {
	if status.UpdaterReady && !status.Available {
		return false
	}
	desiredState := normalizedDesiredState(status.DesiredState)
	jobValid := validActiveJob(status.ActiveJob)
	switch desiredState {
	case "running":
		return status.ActiveJob == nil
	case "upgrading":
		return status.ActiveJob != nil && jobValid && !status.UpdaterReady
	case "maintenance", "deprovisioned":
		return status.ActiveJob == nil && !status.UpdaterReady
	default:
		return false
	}
}

func normalizedDesiredState(value string) string {
	switch value {
	case "running", "upgrading", "maintenance", "deprovisioned":
		return value
	default:
		return "unknown"
	}
}

func activeJobMap(job *releasecontrol.ActiveJob) any {
	if !validActiveJob(job) {
		return nil
	}
	component := releasecontrol.ReleaseComponent(job.Component)
	result := map[string]any{
		"job_id":            job.JobID,
		"component":         string(component),
		"status":            job.Status,
		"service_available": job.ServiceAvailable,
	}
	result["current_version"] = job.CurrentVersion
	result["target_version"] = job.TargetVersion
	return result
}

func validActiveJob(job *releasecontrol.ActiveJob) bool {
	if job == nil {
		return false
	}
	if !releaseJobIDPattern.MatchString(job.JobID) || (job.Status != "queued" && job.Status != "pulling") {
		return false
	}
	switch releasecontrol.ReleaseComponent(job.Component) {
	case releasecontrol.ReleaseComponentServer:
		comparison, err := releasecontrol.CompareCanonicalServerVersions(job.TargetVersion, job.CurrentVersion)
		return err == nil && comparison > 0
	case releasecontrol.ReleaseComponentAgent:
		comparison, err := releasecontrol.CompareCanonicalStableVersions(job.TargetVersion, job.CurrentVersion)
		return err == nil && comparison > 0
	default:
		return false
	}
}

func agentStatusMap(observedVersion string, observationErr error, central releasecontrol.CentralAgentVersion, centralReason, currentServerVersion string) map[string]any {
	reasons := []string{}
	result := map[string]any{
		"available": false, "current_version": "", "latest_version": "",
		"minimum_server_version": "", "update_available": false,
		"compatibility": "unknown", "reasons": reasons,
	}
	if centralReason == "" {
		result["latest_version"] = central.Version
		result["minimum_server_version"] = central.PreVersion
	}
	if observationErr != nil {
		result["reasons"] = []string{releasecontrol.AgentVersionReason(observationErr)}
		return result
	}
	current, err := releasecontrol.CanonicalStableVersion("agent.current_version", observedVersion)
	if err != nil {
		result["reasons"] = []string{releasecontrol.AgentVersionInvalidCode}
		return result
	}
	result["available"] = true
	result["current_version"] = current
	if centralReason != "" {
		result["reasons"] = appendReason(reasons, centralReason)
		return result
	}
	latest := central.Version
	minimumServer := central.PreVersion
	serverComparison, err := releasecontrol.CompareCanonicalStableVersions(currentServerVersion, minimumServer)
	if err != nil {
		serverComparison = -1
	}
	agentComparison, err := releasecontrol.CompareCanonicalStableVersions(latest, current)
	if err != nil {
		return result
	}
	compatibility := "compatible"
	if agentComparison <= 0 {
		reasons = appendReason(reasons, "agent_up_to_date")
	} else if serverComparison < 0 {
		compatibility = "incompatible"
		reasons = appendReason(reasons, "agent_requires_newer_server")
	} else {
		reasons = appendReason(reasons, "agent_update_available")
	}
	return map[string]any{
		"available": true, "current_version": current, "latest_version": latest,
		"minimum_server_version": minimumServer,
		"update_available":       agentComparison > 0 && serverComparison >= 0,
		"compatibility":          compatibility, "reasons": reasons,
	}
}

func appendReason(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func watchdogMap(status releasecontrol.WatchdogStatus) map[string]any {
	watchdogStatus := status.Status
	switch watchdogStatus {
	case "healthy", "observing", "repairing", "degraded", "suppressed":
	default:
		watchdogStatus = "unknown"
	}
	errorCode := status.ErrorCode
	switch errorCode {
	case "", "observation_failed", "repair_failed":
	default:
		errorCode = ""
	}
	return map[string]any{
		"status": watchdogStatus, "degraded": watchdogStatus == "degraded",
		"cooldown_until":   normalizedTime(status.CooldownUntil),
		"last_observed_at": normalizedTime(status.LastObservedAt), "error_code": errorCode,
	}
}

func normalizedTime(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339)
}

func optionalText(value any, limit int) (string, bool) {
	text := actionbase.String(value)
	if text == "" {
		return "", true
	}
	if len(text) > limit || strings.ContainsAny(text, "\r\n\t") {
		return "", false
	}
	return text, true
}

func controllerError(err error) *actionbase.Error {
	if controllerErr, ok := releasecontrol.AsControllerError(err); ok {
		status := controllerErr.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		code := controllerErr.Code
		if code == "" {
			code = "updater_rejected"
		}
		message := "updater rejected the request"
		if code == UpdaterUnavailableCode {
			message = "updater is unavailable"
		}
		return actionbase.CodedError(status, code, message)
	}
	return unavailableError()
}
