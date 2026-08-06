// Package release owns the ProductCore release status, apply, and client-build
// reporting workflows.
package release

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/releasecontrol"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/google/uuid"
)

const (
	ClientSessionStaleCode = "client_session_stale"
	UpdaterUnavailableCode = "updater_unavailable"

	actionClientVersionReport = "client.version.report"
	actionReleaseStatus       = "release.v1.status"
	actionReleaseApply        = "release.v1.apply"
	actionReleaseV2Status     = "release.v2.status"
	actionReleaseV2Apply      = "release.v2.apply"
	applyInvalidParamsCode    = "release_apply_invalid_params"
	v2StatusInvalidParamsCode = "release_v2_status_invalid_params"
	v2ApplyInvalidParamsCode  = "release_v2_apply_invalid_params"
	clientVersionIncompatible = "client_version_incompatible"
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
	SessionLocker        sync.Locker
	Now                  func() time.Time
	CentralVersionSource releasecontrol.CentralVersionSource
}

type Module struct {
	state         StatePort
	cfg           Config
	centralSource releasecontrol.CentralVersionSource
}

func New(state StatePort, cfg Config) *Module {
	centralSource := cfg.CentralVersionSource
	if centralSource == nil {
		centralSource = releasecontrol.NewCentralVersionSource(releasecontrol.CentralVersionSourceConfig{})
	}
	return &Module{state: state, cfg: cfg, centralSource: centralSource}
}

func (m *Module) Handlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{
		actionClientVersionReport: m.reportClientVersion,
		actionReleaseStatus:       m.status,
		actionReleaseApply:        m.apply,
		actionReleaseV2Status:     m.statusV2,
		actionReleaseV2Apply:      m.applyV2,
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

func (m *Module) status(ctx context.Context, _ map[string]any) (any, *actionbase.Error) {
	buildInfo := internal.CurrentBuildInfo()
	snapshot := m.state.Snapshot()
	request := releasecontrol.StatusRequest{
		CurrentVersion:             buildInfo.Version,
		CurrentSchemaVersion:       buildInfo.SchemaVersion,
		CurrentSchemaCompatVersion: buildInfo.SchemaCompatVersion,
		ClientVersion:              snapshot.Client.Version,
	}
	status := unavailableStatus(request)
	if snapshot.Controller != nil {
		if current, err := snapshot.Controller.Status(ctx, request); err == nil {
			status = current
		}
	}
	status.CurrentVersion = request.CurrentVersion
	status.ClientVersion = request.ClientVersion
	return statusMap(status, buildInfo.SchemaVersion, buildInfo.SchemaCompatVersion, snapshot.Client, snapshot.DeviceID), nil
}

func (m *Module) apply(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	allowed := map[string]struct{}{"plan_token": {}, "idempotency_key": {}, "confirm": {}}
	for key := range params {
		if _, ok := allowed[key]; !ok {
			return nil, actionbase.CodedError(http.StatusBadRequest, applyInvalidParamsCode, "release apply accepts only plan_token, idempotency_key, and confirm")
		}
	}
	values := actionbase.Params(params)
	request := releasecontrol.ApplyRequest{
		PlanToken:      values.String("plan_token"),
		IdempotencyKey: values.String("idempotency_key"),
		Confirm:        values.String("confirm"),
	}
	if request.PlanToken == "" || len(request.PlanToken) > 4096 || strings.ContainsAny(request.PlanToken, "\r\n\t") || request.Confirm != releasecontrol.ApplyConfirmation {
		return nil, actionbase.CodedError(http.StatusBadRequest, applyInvalidParamsCode, "release apply parameters are invalid")
	}
	if _, err := uuid.Parse(request.IdempotencyKey); err != nil {
		return nil, actionbase.CodedError(http.StatusBadRequest, applyInvalidParamsCode, "idempotency_key must be a UUID")
	}
	controller := m.state.Snapshot().Controller
	if controller == nil {
		return nil, unavailableError()
	}
	ticket, err := controller.Apply(ctx, request)
	if err != nil {
		return nil, controllerError(err)
	}
	return map[string]any{"job_id": ticket.JobID, "job_token": ticket.JobToken, "status_url": ticket.StatusURL}, nil
}

// statusV2 deliberately asks only the direct updater control surface. It does
// not discover GitHub releases or generate an executable plan; the central
// record is consulted only when an owner requests an apply.
func (m *Module) statusV2(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if len(params) != 0 {
		return nil, actionbase.CodedError(http.StatusBadRequest, v2StatusInvalidParamsCode, "release v2 status does not accept parameters")
	}
	buildInfo := internal.CurrentBuildInfo()
	snapshot := m.state.Snapshot()
	status := releasecontrol.DirectStatus{}
	if controller, ok := snapshot.Controller.(releasecontrol.DirectController); ok {
		if current, err := controller.StatusDirect(ctx); err == nil {
			status = current
		}
	}
	return directStatusMap(status, buildInfo.Version, snapshot.Client.Version), nil
}

func (m *Module) applyV2(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	request, apiErr := validateV2ApplyRequest(params)
	if apiErr != nil {
		return nil, apiErr
	}
	snapshot := m.state.Snapshot()
	controller, ok := snapshot.Controller.(releasecontrol.DirectController)
	if !ok {
		return nil, unavailableError()
	}
	buildInfo := internal.CurrentBuildInfo()
	updaterStatus, err := controller.StatusDirect(ctx)
	if err != nil || !validDirectUpdaterStatus(updaterStatus, buildInfo.Version) || !updaterStatus.UpdaterReady {
		return nil, unavailableError()
	}
	central, err := m.centralSource.CurrentServerVersion(ctx)
	if err != nil {
		return nil, centralVersionError(err)
	}
	if err := validateCentralServerVersion(central); err != nil {
		return nil, centralVersionError(err)
	}
	if request.TargetVersion != central.Version {
		return nil, actionbase.CodedError(http.StatusConflict, releaseTargetMismatch, "target_version no longer matches the central server version")
	}
	clientVersion, err := releasecontrol.CanonicalStableVersion("client_version", snapshot.Client.Version)
	if err != nil {
		return nil, actionbase.CodedError(http.StatusConflict, clientVersionIncompatible, "current client version is not compatible with the server update")
	}
	comparison, err := releasecontrol.CompareCanonicalStableVersions(clientVersion, central.PreVersion)
	if err != nil || comparison < 0 {
		return nil, actionbase.CodedError(http.StatusConflict, clientVersionIncompatible, "current client version is not compatible with the server update")
	}
	comparison, err = releasecontrol.CompareCanonicalServerVersions(request.TargetVersion, buildInfo.Version)
	if err != nil || comparison <= 0 {
		return nil, actionbase.CodedError(http.StatusConflict, releaseTargetNotNewer, "target_version must be newer than the running server")
	}
	ticket, err := controller.ApplyDirect(ctx, request)
	if err != nil {
		return nil, controllerError(err)
	}
	return map[string]any{
		"job_id": ticket.JobID, "job_token": ticket.JobToken,
		"status_url": ticket.StatusURL, "status": ticket.Status,
	}, nil
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

func unavailableStatus(request releasecontrol.StatusRequest) releasecontrol.UpdaterStatus {
	return releasecontrol.UpdaterStatus{
		Available: false, ReleaseAvailable: false, UpdateAvailable: false,
		DiscoveryStatus: "unavailable", CurrentVersion: request.CurrentVersion,
		ClientVersion: request.ClientVersion, Compatibility: "unknown",
		Reasons: []string{UpdaterUnavailableCode}, Operations: []releasecontrol.Operation{},
	}
}

func validateV2ApplyRequest(params map[string]any) (releasecontrol.DirectApplyRequest, *actionbase.Error) {
	allowed := map[string]struct{}{"target_version": {}, "idempotency_key": {}, "confirm": {}}
	for key := range params {
		if _, ok := allowed[key]; !ok {
			return releasecontrol.DirectApplyRequest{}, v2InvalidParamsError()
		}
	}
	targetVersion, ok := exactString(params["target_version"])
	if !ok {
		return releasecontrol.DirectApplyRequest{}, v2InvalidParamsError()
	}
	targetVersion, err := releasecontrol.CanonicalServerVersion("target_version", targetVersion)
	if err != nil {
		return releasecontrol.DirectApplyRequest{}, v2InvalidParamsError()
	}
	idempotencyKey, ok := exactString(params["idempotency_key"])
	if !ok {
		return releasecontrol.DirectApplyRequest{}, v2InvalidParamsError()
	}
	parsedUUID, err := uuid.Parse(idempotencyKey)
	if err != nil || parsedUUID.String() != idempotencyKey {
		return releasecontrol.DirectApplyRequest{}, v2InvalidParamsError()
	}
	confirm, ok := exactString(params["confirm"])
	if !ok || confirm != releasecontrol.ApplyConfirmation {
		return releasecontrol.DirectApplyRequest{}, v2InvalidParamsError()
	}
	return releasecontrol.DirectApplyRequest{
		TargetVersion: targetVersion, IdempotencyKey: idempotencyKey, Confirm: confirm,
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
	return actionbase.CodedError(http.StatusBadRequest, v2ApplyInvalidParamsCode, "release v2 apply accepts only target_version, idempotency_key, and confirm")
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

func directStatusMap(status releasecontrol.DirectStatus, currentVersion, clientVersion string) map[string]any {
	valid := validDirectUpdaterStatus(status, currentVersion)
	updaterAvailable := valid && status.Available
	updaterReady := updaterAvailable && status.UpdaterReady
	return map[string]any{
		"available":         updaterReady,
		"current_version":   currentVersion,
		"client_version":    clientVersion,
		"updater_available": updaterAvailable,
		"updater_ready":     updaterReady,
		"desired_state":     normalizedDesiredState(status.DesiredState),
		"active_job":        directActiveJobMap(status.ActiveJob),
		"watchdog":          watchdogMap(status.Watchdog),
	}
}

func validDirectUpdaterStatus(status releasecontrol.DirectStatus, currentVersion string) bool {
	if !status.Available {
		return false
	}
	updaterVersion, err := releasecontrol.CanonicalServerVersion("current_version", status.CurrentVersion)
	if err != nil || updaterVersion != currentVersion {
		return false
	}
	return normalizedDesiredState(status.DesiredState) != "unknown"
}

func normalizedDesiredState(value string) string {
	switch value {
	case "running", "upgrading", "maintenance", "deprovisioned":
		return value
	default:
		return "unknown"
	}
}

func directActiveJobMap(job *releasecontrol.ActiveJob) any {
	if job == nil || !strings.HasPrefix(job.JobID, "job_") || len(job.JobID) > 128 || !knownJobStatus(job.Status) {
		return nil
	}
	result := map[string]any{
		"job_id":            job.JobID,
		"status":            job.Status,
		"service_available": job.ServiceAvailable,
	}
	if version, err := releasecontrol.CanonicalServerVersion("current_version", job.CurrentVersion); err == nil && version != "" {
		result["current_version"] = version
	}
	if version, err := releasecontrol.CanonicalServerVersion("target_version", job.TargetVersion); err == nil && version != "" {
		result["target_version"] = version
	}
	return result
}

func knownJobStatus(value string) bool {
	switch value {
	case "queued", "validating", "backing_up", "pulling", "stopping", "migrating", "starting", "health_check", "rolling_back", "restarting", "succeeded", "failed", "rolled_back":
		return true
	default:
		return false
	}
}

func statusMap(status releasecontrol.UpdaterStatus, schemaVersion, schemaCompatVersion int, client dirextalkdomain.ClientBuild, deviceID string) map[string]any {
	agentStatus := agentReleaseStatusMap(status.Agent, status.CurrentVersion)
	operations := releaseOperations(status.Operations, agentStatus)
	reasons := status.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	return map[string]any{
		"available": status.Available, "release_available": status.ReleaseAvailable,
		"update_available": status.UpdateAvailable, "discovery_status": status.DiscoveryStatus,
		"checked_at": status.CheckedAt, "current_version": status.CurrentVersion,
		"current_schema_version": schemaVersion, "current_schema_compat_version": schemaCompatVersion,
		"latest_version": status.LatestVersion, "client_version": status.ClientVersion,
		"client_build_number": client.BuildNumber, "client_platform": client.Platform,
		"client_device_id": deviceID, "client_reported_at": client.ReportedAt,
		"compatibility": status.Compatibility, "reasons": reasons,
		"release_notes_url": status.ReleaseNotesURL, "operations": operations,
		"watchdog": watchdogMap(status.Watchdog),
		"agent":    agentStatus,
	}
}

func releaseOperations(operations []releasecontrol.Operation, agentStatus map[string]any) []releasecontrol.Operation {
	result := make([]releasecontrol.Operation, 0, len(operations))
	agentUpdateAvailable, _ := agentStatus["update_available"].(bool)
	agentTarget, _ := agentStatus["latest_version"].(string)
	for _, operation := range operations {
		if operation.Kind == "agent_upgrade" && (!agentUpdateAvailable || operation.TargetVersion != agentTarget) {
			continue
		}
		result = append(result, operation)
	}
	return result
}

func agentReleaseStatusMap(status releasecontrol.AgentReleaseStatus, currentServerVersion string) map[string]any {
	reasons := []string{"agent_release_unavailable"}
	result := map[string]any{
		"available": false, "current_version": "", "latest_version": "",
		"minimum_server_version": "", "update_available": false,
		"compatibility": "unknown", "reasons": reasons,
	}
	if !status.Available {
		return result
	}
	current, err := releasecontrol.CanonicalStableVersion("agent.current_version", status.CurrentVersion)
	if err != nil {
		return result
	}
	latest, err := releasecontrol.CanonicalStableVersion("agent.latest_version", status.LatestVersion)
	if err != nil {
		return result
	}
	minimumServer, err := releasecontrol.CanonicalStableVersion("agent.minimum_server_version", status.MinimumServerVersion)
	if err != nil {
		return result
	}
	runningServer, err := releasecontrol.CanonicalStableVersion("current_version", currentServerVersion)
	if err != nil {
		return result
	}
	serverComparison, err := releasecontrol.CompareCanonicalStableVersions(runningServer, minimumServer)
	if err != nil {
		return result
	}
	agentComparison, err := releasecontrol.CompareCanonicalStableVersions(latest, current)
	if err != nil {
		return result
	}
	compatibility := "compatible"
	reasons = normalizedAgentReleaseReasons(status.Reasons)
	if serverComparison < 0 {
		compatibility = "incompatible"
		reasons = appendReason(reasons, "agent_requires_newer_server")
	}
	return map[string]any{
		"available": true, "current_version": current, "latest_version": latest,
		"minimum_server_version": minimumServer,
		"update_available":       status.UpdateAvailable && agentComparison > 0 && serverComparison >= 0,
		"compatibility":          compatibility, "reasons": reasons,
	}
}

func normalizedAgentReleaseReasons(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		switch value {
		case "agent_update_available", "agent_up_to_date", "agent_requires_newer_server":
			result = appendReason(result, value)
		}
	}
	return result
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
