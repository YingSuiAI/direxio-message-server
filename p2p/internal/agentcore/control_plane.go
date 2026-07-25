package agentcore

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// controlPlaneUnary authenticates each RPC with the deployment-bound token and
// keeps upstream error text out of the ProductCore boundary.
func (c *Client) controlPlaneUnary(parent context.Context, call func(context.Context, *grpc.ClientConn) error) error {
	if c == nil || !c.cfg.Enabled {
		return status.Error(codes.Unavailable, "agent core is not configured")
	}
	ctx, cancel := context.WithTimeout(parent, c.cfg.UnaryTimeout)
	defer cancel()
	pool, err := c.trustPool()
	if err != nil {
		return status.Error(codes.Unavailable, "agent core trust is unavailable")
	}
	if _, err := readCanonicalToken(c.cfg.TokenFile); err != nil {
		return status.Error(codes.Unauthenticated, "agent core authentication failed")
	}
	conn, err := c.connection(ctx, pool)
	if err != nil {
		return err
	}
	// The concrete generated clients attach the authorization metadata in this
	// package's existing model adapter. Reuse that helper so credentials never
	// appear in request DTOs or logs.
	return c.withAuthorization(ctx, call, conn)
}

func (c *Client) withAuthorization(ctx context.Context, call func(context.Context, *grpc.ClientConn) error, conn *grpc.ClientConn) error {
	// Keep the token read local to the call; rotation does not require a
	// process restart and the bytes never enter a ProductCore result.
	token, err := readCanonicalToken(c.cfg.TokenFile)
	if err != nil {
		return status.Error(codes.Unauthenticated, "agent core authentication failed")
	}
	return call(authenticatedContext(ctx, token), conn)
}

func authenticatedContext(ctx context.Context, token []byte) context.Context {
	return metadataAppend(ctx, token)
}

// Kept in a tiny helper to make it difficult for future adapters to forget the
// same metadata convention used by probe/model/conversation calls.
func metadataAppend(ctx context.Context, token []byte) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "DTX-Agent-Token "+base64.RawURLEncoding.EncodeToString(token))
}

func (c *Client) trustPool() (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(c.cfg.CAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid agent core CA")
	}
	return pool, nil
}

func (c *Client) capability(ctx context.Context, name string) *actionbase.Error {
	_ = c.Probe(ctx)
	s := c.Snapshot()
	if s.Status != StatusReady {
		return actionbase.CodedError(http.StatusPreconditionFailed, "agent_core_capability_unavailable", "agent core capability is unavailable")
	}
	for _, item := range s.Capabilities {
		if item == name {
			return nil
		}
	}
	return actionbase.CodedError(http.StatusPreconditionFailed, "agent_core_capability_unavailable", "agent core capability is unavailable")
}

func (c *Client) controlActionError(err error, family string) *actionbase.Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errIncompatible) {
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_incompatible", "agent core protocol is incompatible")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return actionbase.CodedError(http.StatusServiceUnavailable, "agent_core_unavailable", "agent core is unavailable")
	}
	code := status.Code(err)
	switch code {
	case codes.InvalidArgument:
		return actionbase.CodedError(http.StatusBadRequest, "agent_core_invalid_argument", "agent core rejected the "+family+" request")
	case codes.Unauthenticated, codes.PermissionDenied:
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_trust_failed", "agent core authentication failed")
	case codes.NotFound:
		return actionbase.CodedError(http.StatusNotFound, "agent_core_not_found", "agent core "+family+" was not found")
	case codes.Aborted:
		return actionbase.CodedError(http.StatusConflict, "agent_core_conflict", "agent core "+family+" revision conflict")
	case codes.FailedPrecondition:
		return actionbase.CodedError(http.StatusConflict, "agent_core_precondition_failed", "agent core "+family+" precondition failed")
	case codes.DeadlineExceeded, codes.Unavailable:
		return actionbase.CodedError(http.StatusServiceUnavailable, "agent_core_unavailable", "agent core is unavailable")
	case codes.Unimplemented:
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_incompatible", "agent core protocol is incompatible")
	default:
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_upstream_failed", "agent core "+family+" request failed")
	}
}

func safeText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		if r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, strings.TrimSpace(value))
	if len(value) > 512 {
		return value[:512]
	}
	return value
}

func timestampMap(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339Nano)
}

func enumName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"core_task_status_", "core_task_kind_", "core_schedule_state_", "core_confirmation_state_", "core_extension_kind_", "core_extension_source_", "core_extension_transport_", "core_extension_state_", "core_secret_purpose_", "core_secret_grant_purpose_"} {
		value = strings.TrimPrefix(value, prefix)
	}
	value = strings.TrimPrefix(value, "core_")
	value = strings.TrimPrefix(value, "unspecified_")
	value = strings.ReplaceAll(value, "_", "-")
	return value
}

func taskStatus(v agentv1.CoreTaskStatus) string                 { return enumName(v.String()) }
func taskKind(v agentv1.CoreTaskKind) string                     { return enumName(v.String()) }
func scheduleState(v agentv1.CoreScheduleState) string           { return enumName(v.String()) }
func confirmationState(v agentv1.CoreConfirmationState) string   { return enumName(v.String()) }
func extensionKind(v agentv1.CoreExtensionKind) string           { return enumName(v.String()) }
func extensionSource(v agentv1.CoreExtensionSource) string       { return enumName(v.String()) }
func extensionTransport(v agentv1.CoreExtensionTransport) string { return enumName(v.String()) }
func extensionState(v agentv1.CoreExtensionState) string         { return enumName(v.String()) }

func taskMap(task *agentv1.CoreTask) map[string]any {
	if task == nil {
		return nil
	}
	return map[string]any{
		"task_id": task.GetTaskId(), "goal": safeText(task.GetGoal()), "conversation_id": task.GetConversationId(),
		"model_profile_id": task.GetModelProfileId(), "attachment_refs": append([]string(nil), task.GetAttachmentRefs()...),
		"knowledge_refs": append([]string(nil), task.GetKnowledgeRefs()...), "timeout_seconds": task.GetTimeoutSeconds(),
		"status": taskStatus(task.GetStatus()), "attempt": task.GetAttempt(), "lease_epoch": task.GetLeaseEpoch(),
		"available_at": timestampMap(task.GetAvailableAt()), "retry_of_task_id": task.GetRetryOfTaskId(),
		"failure_code": safeText(task.GetFailureCode()), "failure_summary": safeText(task.GetFailureSummary()),
		"revision": task.GetRevision(), "kind": taskKind(task.GetKind()), "created_at": timestampMap(task.GetCreatedAt()), "updated_at": timestampMap(task.GetUpdatedAt()),
	}
}

func taskEventMap(event *agentv1.CoreTaskEvent) map[string]any {
	if event == nil {
		return nil
	}
	out := map[string]any{"task_id": event.GetTaskId(), "sequence": event.GetSequence(), "event_id": event.GetEventId(), "attempt": event.GetAttempt(), "status": taskStatus(event.GetStatus()), "phase": safeText(event.GetPhase()), "progress_message": safeText(event.GetProgressMessage()), "occurred_at": timestampMap(event.GetOccurredAt()), "error_code": safeText(event.GetErrorCode()), "error_summary": safeText(event.GetErrorSummary())}
	if event.Percent != nil {
		out["percent"] = event.GetPercent()
	}
	return out
}

func scheduleMap(schedule *agentv1.CoreSchedule) map[string]any {
	if schedule == nil {
		return nil
	}
	out := map[string]any{"schedule_id": schedule.GetScheduleId(), "name": safeText(schedule.GetName()), "state": scheduleState(schedule.GetState()), "next_run_at": timestampMap(schedule.GetNextRunAt()), "last_scheduled_for": timestampMap(schedule.GetLastScheduledFor()), "revision": schedule.GetRevision(), "created_at": timestampMap(schedule.GetCreatedAt()), "updated_at": timestampMap(schedule.GetUpdatedAt())}
	if trigger := schedule.GetTrigger(); trigger != nil {
		if runAt := trigger.GetRunAt(); runAt != nil {
			out["trigger"] = map[string]any{"kind": "run_at", "run_at": timestampMap(runAt)}
		} else if cron := trigger.GetCron(); cron != nil {
			out["trigger"] = map[string]any{"kind": "cron", "expression": safeText(cron.GetExpression()), "timezone": safeText(cron.GetTimezone())}
		}
	}
	if template := schedule.GetTaskTemplate(); template != nil {
		out["task_template"] = map[string]any{"goal": safeText(template.GetGoal()), "conversation_id": template.GetConversationId(), "model_profile_id": template.GetModelProfileId(), "attachment_refs": append([]string(nil), template.GetAttachmentRefs()...), "knowledge_refs": append([]string(nil), template.GetKnowledgeRefs()...), "timeout_seconds": template.GetTimeoutSeconds()}
	}
	return out
}

func confirmationMap(item *agentv1.CoreConfirmation) map[string]any {
	if item == nil {
		return nil
	}
	out := map[string]any{"confirmation_id": item.GetConfirmationId(), "task_id": item.GetTaskId(), "state": confirmationState(item.GetState()), "revision": item.GetRevision(), "created_at": timestampMap(item.GetCreatedAt()), "updated_at": timestampMap(item.GetUpdatedAt()), "expires_at": timestampMap(item.GetExpiresAt()), "terminal_reason": safeText(item.GetTerminalReason()), "terminal_code": safeText(item.GetTerminalCode()), "terminal_note": safeText(item.GetTerminalNote())}
	if binding := item.GetBinding(); binding != nil {
		grants := make([]any, 0, len(binding.GetSecretGrants()))
		for _, grant := range binding.GetSecretGrants() {
			if grant != nil {
				grants = append(grants, map[string]any{"reference_id": grant.GetReferenceId(), "purpose": enumName(grant.GetPurpose().String()), "binding_digest": grant.GetBindingDigest()})
			}
		}
		out["binding"] = map[string]any{"operation_domain": safeText(binding.GetOperationDomain()), "target_id": binding.GetTargetId(), "target_revision": binding.GetTargetRevision(), "source_version": safeText(binding.GetSourceVersion()), "source_commit": safeText(binding.GetSourceCommit()), "content_digest": binding.GetContentDigest(), "parameter_digest": binding.GetParameterDigest(), "network_digest": binding.GetNetworkDigest(), "secret_grant_digest": binding.GetSecretGrantDigest(), "network_grants": append([]string(nil), binding.GetNetworkGrants()...), "secret_grants": grants}
	}
	return out
}

func candidateMap(candidate *agentv1.CoreExtensionCandidate) map[string]any {
	if candidate == nil {
		return nil
	}
	out := map[string]any{"id": candidate.GetId(), "kind": extensionKind(candidate.GetKind()), "source": extensionSource(candidate.GetSource()), "name": safeText(candidate.GetName()), "description": safeText(candidate.GetDescription()), "transport": extensionTransport(candidate.GetTransport())}
	if pin := candidate.GetPin(); pin != nil {
		out["pin"] = map[string]any{"registry_version": safeText(pin.GetRegistryVersion()), "registry_sha256": pin.GetRegistrySha256(), "git_commit": pin.GetGitCommit(), "git_sha256": pin.GetGitSha256()}
	}
	return out
}

func installationMap(item *agentv1.CoreInstallation) map[string]any {
	if item == nil {
		return nil
	}
	out := map[string]any{"installation_id": item.GetInstallationId(), "kind": extensionKind(item.GetKind()), "source": extensionSource(item.GetSource()), "name": safeText(item.GetName()), "description": safeText(item.GetDescription()), "revision": item.GetRevision(), "state": extensionState(item.GetState()), "active_version_id": item.GetActiveVersionId(), "proposed_version_id": item.GetProposedVersionId(), "candidate_id": item.GetCandidateId(), "transport": extensionTransport(item.GetTransport()), "created_at": timestampMap(item.GetCreatedAt()), "updated_at": timestampMap(item.GetUpdatedAt())}
	versions := make([]any, 0, len(item.GetVersions()))
	for _, version := range item.GetVersions() {
		if version == nil {
			continue
		}
		versions = append(versions, map[string]any{"version_id": version.GetVersionId(), "content_digest": version.GetContentDigest(), "manifest_digest": version.GetManifestDigest(), "execution_digest": version.GetExecutionDigest(), "network_schema_digest": version.GetNetworkSchemaDigest(), "secret_schema_digest": version.GetSecretSchemaDigest(), "created_at": timestampMap(version.GetCreatedAt())})
	}
	out["versions"] = versions
	return out
}

func toolMap(tool *agentv1.CoreTool) map[string]any {
	if tool == nil {
		return nil
	}
	return map[string]any{"name": tool.GetName(), "description": safeText(tool.GetDescription()), "input_schema_digest": tool.GetInputSchemaDigest()}
}

func structInput(params map[string]any, key string) (*structpb.Struct, *actionbase.Error) {
	value, ok := params[key]
	if !ok || value == nil {
		return nil, nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, actionbase.BadRequest(key + " must be an object")
	}
	result, err := structpb.NewStruct(m)
	if err != nil {
		return nil, actionbase.BadRequest(key + " is invalid")
	}
	return result, nil
}

func parsePage(params map[string]any) (int32, string, *actionbase.Error) {
	size, err := optionalInt32(params, "page_size")
	if err != nil {
		return 0, "", err
	}
	token, err := optionalString(params, "page_token")
	if err != nil {
		return 0, "", err
	}
	return size, token, nil
}

func requiredUUID(params map[string]any, key string) (string, *actionbase.Error) {
	value, err := requiredString(params, key)
	if err != nil {
		return "", err
	}
	if _, parseErr := uuid.Parse(value); parseErr != nil {
		return "", actionbase.BadRequest(key + " must be a UUID")
	}
	return value, nil
}

func optionalEnum(params map[string]any, key string) (string, *actionbase.Error) {
	value, err := optionalString(params, key)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(value)), nil
}

func marshalSafe(value any) string {
	raw, _ := json.Marshal(value)
	return safeText(string(raw))
}

func ensureResponse[T any](response *T, family string) *actionbase.Error {
	if response == nil {
		return actionbase.CodedError(http.StatusBadGateway, "agent_core_upstream_failed", "agent core returned an empty "+family+" response")
	}
	return nil
}

func parseTaskStatus(value string) (agentv1.CoreTaskStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "queued":
		return agentv1.CoreTaskStatus_CORE_TASK_STATUS_QUEUED, true
	case "running":
		return agentv1.CoreTaskStatus_CORE_TASK_STATUS_RUNNING, true
	case "waiting-user", "waiting_user":
		return agentv1.CoreTaskStatus_CORE_TASK_STATUS_WAITING_USER, true
	case "succeeded":
		return agentv1.CoreTaskStatus_CORE_TASK_STATUS_SUCCEEDED, true
	case "failed":
		return agentv1.CoreTaskStatus_CORE_TASK_STATUS_FAILED, true
	case "canceled", "cancelled":
		return agentv1.CoreTaskStatus_CORE_TASK_STATUS_CANCELED, true
	default:
		return agentv1.CoreTaskStatus_CORE_TASK_STATUS_UNSPECIFIED, false
	}
}

func parseExtensionKind(value string, expected agentv1.CoreExtensionKind) (agentv1.CoreExtensionKind, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return expected, true
	}
	if value == "mcp" && expected == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP {
		return expected, true
	}
	if value == "skill" && expected == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL {
		return expected, true
	}
	return agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_UNSPECIFIED, false
}

func parseExtensionSource(value string) (agentv1.CoreExtensionSource, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "unspecified":
		return agentv1.CoreExtensionSource_CORE_EXTENSION_SOURCE_UNSPECIFIED, true
	case "official-registry", "official_registry":
		return agentv1.CoreExtensionSource_CORE_EXTENSION_SOURCE_OFFICIAL_REGISTRY, true
	case "smithery":
		return agentv1.CoreExtensionSource_CORE_EXTENSION_SOURCE_SMITHERY, true
	case "glama":
		return agentv1.CoreExtensionSource_CORE_EXTENSION_SOURCE_GLAMA, true
	case "github":
		return agentv1.CoreExtensionSource_CORE_EXTENSION_SOURCE_GITHUB, true
	case "skills-sh", "skills_sh":
		return agentv1.CoreExtensionSource_CORE_EXTENSION_SOURCE_SKILLS_SH, true
	default:
		return agentv1.CoreExtensionSource_CORE_EXTENSION_SOURCE_UNSPECIFIED, false
	}
}

func sourceAndKind(params map[string]any, expected agentv1.CoreExtensionKind) (agentv1.CoreExtensionKind, agentv1.CoreExtensionSource, *actionbase.Error) {
	kindName, err := optionalEnum(params, "kind")
	if err != nil {
		return 0, 0, err
	}
	kind, ok := parseExtensionKind(kindName, expected)
	if !ok {
		return 0, 0, actionbase.BadRequest("kind is invalid")
	}
	sourceName, err := optionalEnum(params, "source")
	if err != nil {
		return 0, 0, err
	}
	source, ok := parseExtensionSource(sourceName)
	if !ok {
		return 0, 0, actionbase.BadRequest("source is invalid")
	}
	return kind, source, nil
}

func (c *Client) taskHandlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{
		"agent.core.tasks.get": c.taskGet, "agent.core.tasks.list": c.taskList,
		"agent.core.tasks.cancel": c.taskCancel, "agent.core.tasks.retry": c.taskRetry,
		"agent.core.tasks.events": c.taskEvents,
	}
}

func (c *Client) taskGet(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "task"); e != nil {
		return nil, e
	}
	id, e := requiredString(params, "task_id")
	if e != nil {
		return nil, e
	}
	var response *agentv1.TaskServiceGetTaskResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewTaskServiceClient(conn).GetTask(callCtx, &agentv1.TaskServiceGetTaskRequest{TaskId: id})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "task")
	}
	if e := ensureResponse(response, "task"); e != nil {
		return nil, e
	}
	return map[string]any{"task": taskMap(response.GetTask())}, nil
}

func (c *Client) taskList(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "task"); e != nil {
		return nil, e
	}
	pageSize, pageToken, e := parsePage(params)
	if e != nil {
		return nil, e
	}
	statusName, e := optionalEnum(params, "status")
	if e != nil {
		return nil, e
	}
	statusValue := agentv1.CoreTaskStatus_CORE_TASK_STATUS_UNSPECIFIED
	if statusName != "" {
		var ok bool
		statusValue, ok = parseTaskStatus(statusName)
		if !ok {
			return nil, actionbase.BadRequest("status is invalid")
		}
	}
	var response *agentv1.TaskServiceListTasksResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewTaskServiceClient(conn).ListTasks(callCtx, &agentv1.TaskServiceListTasksRequest{PageSize: pageSize, PageToken: pageToken, Status: statusValue})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "task")
	}
	if e := ensureResponse(response, "task list"); e != nil {
		return nil, e
	}
	items := make([]any, 0, len(response.GetTasks()))
	for _, item := range response.GetTasks() {
		items = append(items, taskMap(item))
	}
	return map[string]any{"tasks": items, "next_page_token": response.GetNextPageToken()}, nil
}

func (c *Client) taskCancel(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "task"); e != nil {
		return nil, e
	}
	idempotency, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	taskID, e := requiredString(params, "task_id")
	if e != nil {
		return nil, e
	}
	revision, e := optionalInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	reason, e := optionalString(params, "reason")
	if e != nil {
		return nil, e
	}
	var response *agentv1.TaskServiceCancelTaskResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewTaskServiceClient(conn).CancelTask(callCtx, &agentv1.TaskServiceCancelTaskRequest{IdempotencyKey: idempotency, TaskId: taskID, ExpectedRevision: revision, Reason: safeText(reason)})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "task")
	}
	if e := ensureResponse(response, "task cancellation"); e != nil {
		return nil, e
	}
	return map[string]any{"task": taskMap(response.GetTask())}, nil
}

func (c *Client) taskRetry(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "task"); e != nil {
		return nil, e
	}
	idempotency, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	taskID, e := requiredString(params, "task_id")
	if e != nil {
		return nil, e
	}
	revision, e := optionalInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	var response *agentv1.TaskServiceRetryTaskResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewTaskServiceClient(conn).RetryTask(callCtx, &agentv1.TaskServiceRetryTaskRequest{IdempotencyKey: idempotency, TaskId: taskID, ExpectedRevision: revision})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "task")
	}
	if e := ensureResponse(response, "task retry"); e != nil {
		return nil, e
	}
	return map[string]any{"task": taskMap(response.GetTask())}, nil
}

func (c *Client) taskEvents(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "task"); e != nil {
		return nil, e
	}
	taskID, e := requiredString(params, "task_id")
	if e != nil {
		return nil, e
	}
	after, e := optionalInt64(params, "after_sequence")
	if e != nil {
		return nil, e
	}
	limit, e := optionalInt32(params, "limit")
	if e != nil {
		return nil, e
	}
	if limit <= 0 {
		limit = 128
	}
	if limit > 256 {
		return nil, actionbase.BadRequest("limit is too large")
	}
	var events []any
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		stream, err := agentv1.NewTaskServiceClient(conn).WatchTaskEvents(callCtx, &agentv1.TaskServiceWatchTaskEventsRequest{TaskId: taskID, AfterSequence: after})
		if err != nil {
			return err
		}
		for len(events) < int(limit) {
			item, recvErr := stream.Recv()
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if recvErr != nil {
				return recvErr
			}
			if item != nil && item.GetEvent() != nil {
				events = append(events, taskEventMap(item.GetEvent()))
			}
		}
		return nil
	})
	if err != nil {
		return nil, c.controlActionError(err, "task events")
	}
	return map[string]any{"events": events}, nil
}

func taskTemplateFromParams(params map[string]any, key string) (*agentv1.CoreTaskTemplate, *actionbase.Error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, actionbase.BadRequest(key + " must be an object")
	}
	goal, e := requiredString(m, "goal")
	if e != nil {
		return nil, e
	}
	conversationID, e := optionalString(m, "conversation_id")
	if e != nil {
		return nil, e
	}
	profileID, e := optionalString(m, "model_profile_id")
	if e != nil {
		return nil, e
	}
	timeout, e := optionalInt64(m, "timeout_seconds")
	if e != nil {
		return nil, e
	}
	return &agentv1.CoreTaskTemplate{Goal: safeText(goal), ConversationId: conversationID, ModelProfileId: profileID, TimeoutSeconds: timeout}, nil
}

func triggerFromParams(params map[string]any, key string) (*agentv1.CoreScheduleTrigger, *actionbase.Error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, actionbase.BadRequest(key + " must be an object")
	}
	kind, e := requiredString(m, "kind")
	if e != nil {
		return nil, e
	}
	switch strings.ToLower(kind) {
	case "run_at", "run-at":
		value, e := requiredString(m, "run_at")
		if e != nil {
			return nil, e
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, actionbase.BadRequest("run_at must be an RFC3339 timestamp")
		}
		return &agentv1.CoreScheduleTrigger{Trigger: &agentv1.CoreScheduleTrigger_RunAt{RunAt: timestamppb.New(parsed.UTC())}}, nil
	case "cron":
		expression, e := requiredString(m, "expression")
		if e != nil {
			return nil, e
		}
		timezone, e := requiredString(m, "timezone")
		if e != nil {
			return nil, e
		}
		return &agentv1.CoreScheduleTrigger{Trigger: &agentv1.CoreScheduleTrigger_Cron{Cron: &agentv1.CoreCronTrigger{Expression: safeText(expression), Timezone: safeText(timezone)}}}, nil
	default:
		return nil, actionbase.BadRequest("trigger kind is invalid")
	}
}

func (c *Client) scheduleHandlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{
		"agent.core.schedules.create": c.scheduleCreate, "agent.core.schedules.get": c.scheduleGet, "agent.core.schedules.list": c.scheduleList,
		"agent.core.schedules.update": c.scheduleUpdate, "agent.core.schedules.pause": c.schedulePause, "agent.core.schedules.resume": c.scheduleResume,
		"agent.core.schedules.trigger": c.scheduleTrigger, "agent.core.schedules.delete": c.scheduleDelete,
	}
}

func (c *Client) scheduleCreate(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "schedule"); e != nil {
		return nil, e
	}
	idempotency, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	name, e := requiredString(params, "name")
	if e != nil {
		return nil, e
	}
	template, e := taskTemplateFromParams(params, "task_template")
	if e != nil {
		return nil, e
	}
	trigger, e := triggerFromParams(params, "trigger")
	if e != nil {
		return nil, e
	}
	if template == nil || trigger == nil {
		return nil, actionbase.BadRequest("task_template and trigger are required")
	}
	var response *agentv1.ScheduleServiceCreateResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewScheduleServiceClient(conn).Create(callCtx, &agentv1.ScheduleServiceCreateRequest{IdempotencyKey: idempotency, Name: safeText(name), TaskTemplate: template, Trigger: trigger})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "schedule")
	}
	if e := ensureResponse(response, "schedule"); e != nil {
		return nil, e
	}
	return map[string]any{"schedule": scheduleMap(response.GetSchedule())}, nil
}

func (c *Client) scheduleGet(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "schedule"); e != nil {
		return nil, e
	}
	id, e := requiredString(params, "schedule_id")
	if e != nil {
		return nil, e
	}
	var response *agentv1.ScheduleServiceGetResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewScheduleServiceClient(conn).Get(callCtx, &agentv1.ScheduleServiceGetRequest{ScheduleId: id})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "schedule")
	}
	if e := ensureResponse(response, "schedule"); e != nil {
		return nil, e
	}
	return map[string]any{"schedule": scheduleMap(response.GetSchedule())}, nil
}
func (c *Client) scheduleList(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "schedule"); e != nil {
		return nil, e
	}
	size, token, e := parsePage(params)
	if e != nil {
		return nil, e
	}
	var response *agentv1.ScheduleServiceListResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewScheduleServiceClient(conn).List(callCtx, &agentv1.ScheduleServiceListRequest{PageSize: size, PageToken: token})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "schedule")
	}
	if e := ensureResponse(response, "schedule list"); e != nil {
		return nil, e
	}
	items := make([]any, 0, len(response.GetSchedules()))
	for _, item := range response.GetSchedules() {
		items = append(items, scheduleMap(item))
	}
	return map[string]any{"schedules": items, "next_page_token": response.GetNextPageToken()}, nil
}

func (c *Client) scheduleUpdate(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "schedule"); e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredString(params, "schedule_id")
	if e != nil {
		return nil, e
	}
	rev, e := optionalInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	name, e := optionalString(params, "name")
	if e != nil {
		return nil, e
	}
	template, e := taskTemplateFromParams(params, "task_template")
	if e != nil {
		return nil, e
	}
	trigger, e := triggerFromParams(params, "trigger")
	if e != nil {
		return nil, e
	}
	var namePtr *string
	if _, present := params["name"]; present {
		namePtr = &name
	}
	var response *agentv1.ScheduleServiceUpdateResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewScheduleServiceClient(conn).Update(callCtx, &agentv1.ScheduleServiceUpdateRequest{IdempotencyKey: idem, ScheduleId: id, ExpectedRevision: rev, Name: namePtr, TaskTemplate: template, Trigger: trigger})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "schedule")
	}
	if e := ensureResponse(response, "schedule"); e != nil {
		return nil, e
	}
	return map[string]any{"schedule": scheduleMap(response.GetSchedule())}, nil
}

func (c *Client) schedulePause(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	return c.scheduleStateMutation(ctx, params, true)
}
func (c *Client) scheduleResume(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	return c.scheduleStateMutation(ctx, params, false)
}
func (c *Client) scheduleStateMutation(ctx context.Context, params map[string]any, pause bool) (any, *actionbase.Error) {
	if e := c.capability(ctx, "schedule"); e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredString(params, "schedule_id")
	if e != nil {
		return nil, e
	}
	rev, e := optionalInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	var schedule *agentv1.CoreSchedule
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		if pause {
			response, err := agentv1.NewScheduleServiceClient(conn).Pause(callCtx, &agentv1.ScheduleServicePauseRequest{IdempotencyKey: idem, ScheduleId: id, ExpectedRevision: rev})
			if err == nil {
				schedule = response.GetSchedule()
			}
			return err
		}
		response, err := agentv1.NewScheduleServiceClient(conn).Resume(callCtx, &agentv1.ScheduleServiceResumeRequest{IdempotencyKey: idem, ScheduleId: id, ExpectedRevision: rev})
		if err == nil {
			schedule = response.GetSchedule()
		}
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "schedule")
	}
	if schedule == nil {
		return nil, ensureResponse((*agentv1.ScheduleServiceGetResponse)(nil), "schedule")
	}
	return map[string]any{"schedule": scheduleMap(schedule)}, nil
}
func (c *Client) scheduleTrigger(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "schedule"); e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredString(params, "schedule_id")
	if e != nil {
		return nil, e
	}
	var response *agentv1.ScheduleServiceTriggerNowResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewScheduleServiceClient(conn).TriggerNow(callCtx, &agentv1.ScheduleServiceTriggerNowRequest{IdempotencyKey: idem, ScheduleId: id})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "schedule")
	}
	if e := ensureResponse(response, "schedule trigger"); e != nil {
		return nil, e
	}
	return map[string]any{"schedule": scheduleMap(response.GetSchedule()), "occurrence_id": response.GetOccurrenceId(), "task_id": response.GetTaskId()}, nil
}
func (c *Client) scheduleDelete(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "schedule"); e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredString(params, "schedule_id")
	if e != nil {
		return nil, e
	}
	rev, e := optionalInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		_, err := agentv1.NewScheduleServiceClient(conn).Delete(callCtx, &agentv1.ScheduleServiceDeleteRequest{IdempotencyKey: idem, ScheduleId: id, ExpectedRevision: rev})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "schedule")
	}
	return map[string]any{"deleted": true, "schedule_id": id}, nil
}

func parseConfirmationState(value string) (agentv1.CoreConfirmationState, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "pending":
		return agentv1.CoreConfirmationState_CORE_CONFIRMATION_STATE_PENDING, true
	case "confirmed":
		return agentv1.CoreConfirmationState_CORE_CONFIRMATION_STATE_CONFIRMED, true
	case "consumed":
		return agentv1.CoreConfirmationState_CORE_CONFIRMATION_STATE_CONSUMED, true
	case "rejected":
		return agentv1.CoreConfirmationState_CORE_CONFIRMATION_STATE_REJECTED, true
	case "expired":
		return agentv1.CoreConfirmationState_CORE_CONFIRMATION_STATE_EXPIRED, true
	default:
		return agentv1.CoreConfirmationState_CORE_CONFIRMATION_STATE_UNSPECIFIED, false
	}
}

func (c *Client) confirmationHandlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{"agent.core.confirmations.get": c.confirmationGet, "agent.core.confirmations.list": c.confirmationList, "agent.core.confirmations.confirm": c.confirmationConfirm, "agent.core.confirmations.reject": c.confirmationReject}
}
func (c *Client) confirmationGet(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "confirmation"); e != nil {
		return nil, e
	}
	id, e := requiredString(params, "confirmation_id")
	if e != nil {
		return nil, e
	}
	var response *agentv1.ConfirmationServiceGetResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewConfirmationServiceClient(conn).Get(callCtx, &agentv1.ConfirmationServiceGetRequest{ConfirmationId: id})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "confirmation")
	}
	if e := ensureResponse(response, "confirmation"); e != nil {
		return nil, e
	}
	return map[string]any{"confirmation": confirmationMap(response.GetConfirmation())}, nil
}
func (c *Client) confirmationList(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "confirmation"); e != nil {
		return nil, e
	}
	size, token, e := parsePage(params)
	if e != nil {
		return nil, e
	}
	domain, e := optionalString(params, "operation_domain")
	if e != nil {
		return nil, e
	}
	target, e := optionalString(params, "target_id")
	if e != nil {
		return nil, e
	}
	rawStates, ok := params["states"]
	states := make([]agentv1.CoreConfirmationState, 0)
	if ok && rawStates != nil {
		values, ok := rawStates.([]any)
		if !ok {
			return nil, actionbase.BadRequest("states must be an array")
		}
		for _, raw := range values {
			text, ok := raw.(string)
			if !ok {
				return nil, actionbase.BadRequest("states must contain strings")
			}
			state, valid := parseConfirmationState(text)
			if !valid {
				return nil, actionbase.BadRequest("states contains an invalid value")
			}
			states = append(states, state)
		}
	}
	var response *agentv1.ConfirmationServiceListResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewConfirmationServiceClient(conn).List(callCtx, &agentv1.ConfirmationServiceListRequest{PageSize: size, PageToken: token, OperationDomain: domain, TargetId: target, States: states})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "confirmation")
	}
	if e := ensureResponse(response, "confirmation list"); e != nil {
		return nil, e
	}
	items := make([]any, 0, len(response.GetConfirmations()))
	for _, item := range response.GetConfirmations() {
		items = append(items, confirmationMap(item))
	}
	return map[string]any{"confirmations": items, "next_page_token": response.GetNextPageToken()}, nil
}
func (c *Client) confirmationConfirm(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "confirmation"); e != nil {
		return nil, e
	}
	id, e := requiredString(params, "confirmation_id")
	if e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	rev, e := optionalInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	var response *agentv1.ConfirmationServiceConfirmResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewConfirmationServiceClient(conn).Confirm(callCtx, &agentv1.ConfirmationServiceConfirmRequest{ConfirmationId: id, IdempotencyKey: idem, ExpectedRevision: rev})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "confirmation")
	}
	if e := ensureResponse(response, "confirmation"); e != nil {
		return nil, e
	}
	return map[string]any{"confirmation": confirmationMap(response.GetConfirmation())}, nil
}
func (c *Client) confirmationReject(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "confirmation"); e != nil {
		return nil, e
	}
	id, e := requiredString(params, "confirmation_id")
	if e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	rev, e := optionalInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	reason, e := optionalString(params, "reason")
	if e != nil {
		return nil, e
	}
	var response *agentv1.ConfirmationServiceRejectResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewConfirmationServiceClient(conn).Reject(callCtx, &agentv1.ConfirmationServiceRejectRequest{ConfirmationId: id, IdempotencyKey: idem, ExpectedRevision: rev, Reason: safeText(reason)})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "confirmation")
	}
	if e := ensureResponse(response, "confirmation"); e != nil {
		return nil, e
	}
	return map[string]any{"confirmation": confirmationMap(response.GetConfirmation())}, nil
}

func candidateFromParams(params map[string]any, key string, kind agentv1.CoreExtensionKind) (*agentv1.CoreExtensionCandidate, *actionbase.Error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, actionbase.BadRequest(key + " must be an object")
	}
	id, e := requiredString(m, "id")
	if e != nil {
		return nil, e
	}
	name, e := optionalString(m, "name")
	if e != nil {
		return nil, e
	}
	description, e := optionalString(m, "description")
	if e != nil {
		return nil, e
	}
	sourceName, e := optionalEnum(m, "source")
	if e != nil {
		return nil, e
	}
	source, valid := parseExtensionSource(sourceName)
	if !valid {
		return nil, actionbase.BadRequest("candidate.source is invalid")
	}
	return &agentv1.CoreExtensionCandidate{Id: id, Kind: kind, Source: source, Name: safeText(name), Description: safeText(description)}, nil
}

func secretInputsFromParams(params map[string]any, key string) ([]*agentv1.CoreExtensionSecretInput, *actionbase.Error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return nil, nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, actionbase.BadRequest(key + " must be an array")
	}
	out := make([]*agentv1.CoreExtensionSecretInput, 0, len(values))
	for _, item := range values {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, actionbase.BadRequest(key + " must contain objects")
		}
		ref, e := requiredString(m, "reference_id")
		if e != nil {
			return nil, e
		}
		purpose, e := optionalEnum(m, "purpose")
		if e != nil {
			return nil, e
		}
		value, e := requiredString(m, "secret_value")
		if e != nil {
			return nil, e
		}
		var p agentv1.CoreSecretPurpose
		switch purpose {
		case "mcp-credential", "mcp_credential":
			p = agentv1.CoreSecretPurpose_CORE_SECRET_PURPOSE_MCP_CREDENTIAL
		case "skill-secret", "skill_secret":
			p = agentv1.CoreSecretPurpose_CORE_SECRET_PURPOSE_SKILL_SECRET
		default:
			return nil, actionbase.BadRequest("secret purpose is invalid")
		}
		out = append(out, &agentv1.CoreExtensionSecretInput{ReferenceId: ref, Purpose: p, SecretValue: value})
	}
	return out, nil
}

func (c *Client) extensionHandlers(kind agentv1.CoreExtensionKind, prefix string) map[string]actionbase.Handler {
	return map[string]actionbase.Handler{"agent.core." + prefix + ".discover": c.extensionDiscover(kind), "agent.core." + prefix + ".get": c.extensionGet(kind), "agent.core." + prefix + ".list": c.extensionList(kind), "agent.core." + prefix + ".install": c.extensionInstall(kind), "agent.core." + prefix + ".update": c.extensionUpdate(kind), "agent.core." + prefix + ".remove": c.extensionRemove(kind), "agent.core." + prefix + ".execute": c.extensionExecute(kind)}
}

func (c *Client) extensionDiscover(kind agentv1.CoreExtensionKind) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		capability := "mcp"
		if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL {
			capability = "skill"
		}
		if e := c.capability(ctx, capability); e != nil {
			return nil, e
		}
		size, token, e := parsePage(params)
		if e != nil {
			return nil, e
		}
		sourceName, e := optionalEnum(params, "source")
		if e != nil {
			return nil, e
		}
		source, ok := parseExtensionSource(sourceName)
		if !ok {
			return nil, actionbase.BadRequest("source is invalid")
		}
		query, e := optionalString(params, "query")
		if e != nil {
			return nil, e
		}
		var candidates []*agentv1.CoreExtensionCandidate
		var next string
		err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
			if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP {
				response, err := agentv1.NewMCPServiceClient(conn).Search(callCtx, &agentv1.MCPServiceSearchRequest{Kind: kind, Source: source, Text: safeText(query), PageSize: size, PageToken: token})
				if err != nil {
					return err
				}
				candidates, next = response.GetCandidates(), response.GetNextPageToken()
				return nil
			}
			response, err := agentv1.NewSkillServiceClient(conn).Search(callCtx, &agentv1.SkillServiceSearchRequest{Kind: kind, Source: source, Text: safeText(query), PageSize: size, PageToken: token})
			if err != nil {
				return err
			}
			candidates, next = response.GetCandidates(), response.GetNextPageToken()
			return nil
		})
		if err != nil {
			return nil, c.controlActionError(err, "extension discovery")
		}
		items := make([]any, 0, len(candidates))
		for _, item := range candidates {
			items = append(items, candidateMap(item))
		}
		return map[string]any{"candidates": items, "next_page_token": next}, nil
	}
}

func (c *Client) extensionGet(kind agentv1.CoreExtensionKind) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		capability := "mcp"
		if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL {
			capability = "skill"
		}
		if e := c.capability(ctx, capability); e != nil {
			return nil, e
		}
		id, e := requiredString(params, "installation_id")
		if e != nil {
			return nil, e
		}
		var installation *agentv1.CoreInstallation
		err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
			if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP {
				response, err := agentv1.NewMCPServiceClient(conn).Get(callCtx, &agentv1.MCPServiceGetRequest{InstallationId: id})
				if err != nil {
					return err
				}
				installation = response.GetInstallation()
				return nil
			}
			response, err := agentv1.NewSkillServiceClient(conn).Get(callCtx, &agentv1.SkillServiceGetRequest{InstallationId: id})
			if err != nil {
				return err
			}
			installation = response.GetInstallation()
			return nil
		})
		if err != nil {
			return nil, c.controlActionError(err, "extension")
		}
		if installation == nil {
			return nil, actionbase.CodedError(http.StatusBadGateway, "agent_core_upstream_failed", "agent core returned an empty extension response")
		}
		return map[string]any{"installation": installationMap(installation)}, nil
	}
}

func (c *Client) extensionList(kind agentv1.CoreExtensionKind) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		capability := "mcp"
		if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL {
			capability = "skill"
		}
		if e := c.capability(ctx, capability); e != nil {
			return nil, e
		}
		size, token, e := parsePage(params)
		if e != nil {
			return nil, e
		}
		sourceName, e := optionalEnum(params, "source")
		if e != nil {
			return nil, e
		}
		source, ok := parseExtensionSource(sourceName)
		if !ok {
			return nil, actionbase.BadRequest("source is invalid")
		}
		var installations []*agentv1.CoreInstallation
		var next string
		err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
			if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP {
				response, err := agentv1.NewMCPServiceClient(conn).List(callCtx, &agentv1.MCPServiceListRequest{Kind: kind, Source: source, PageSize: size, PageToken: token})
				if err != nil {
					return err
				}
				installations, next = response.GetInstallations(), response.GetNextPageToken()
				return nil
			}
			response, err := agentv1.NewSkillServiceClient(conn).List(callCtx, &agentv1.SkillServiceListRequest{Kind: kind, Source: source, PageSize: size, PageToken: token})
			if err != nil {
				return err
			}
			installations, next = response.GetInstallations(), response.GetNextPageToken()
			return nil
		})
		if err != nil {
			return nil, c.controlActionError(err, "extension")
		}
		items := make([]any, 0, len(installations))
		for _, item := range installations {
			items = append(items, installationMap(item))
		}
		return map[string]any{"installations": items, "next_page_token": next}, nil
	}
}

func (c *Client) extensionInstall(kind agentv1.CoreExtensionKind) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		capability := "mcp"
		if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL {
			capability = "skill"
		}
		if e := c.capability(ctx, capability); e != nil {
			return nil, e
		}
		idem, e := requiredString(params, "idempotency_key")
		if e != nil {
			return nil, e
		}
		candidate, e := candidateFromParams(params, "candidate", kind)
		if e != nil {
			return nil, e
		}
		secrets, e := secretInputsFromParams(params, "secret_inputs")
		if e != nil {
			return nil, e
		}
		var installation *agentv1.CoreInstallation
		var confirmationID, taskID string
		err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
			if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP {
				response, err := agentv1.NewMCPServiceClient(conn).RequestInstall(callCtx, &agentv1.MCPServiceRequestInstallRequest{IdempotencyKey: idem, Candidate: candidate, SecretInputs: secrets})
				if err != nil {
					return err
				}
				installation, confirmationID, taskID = response.GetInstallation(), response.GetConfirmationId(), response.GetTaskId()
				return nil
			}
			response, err := agentv1.NewSkillServiceClient(conn).RequestInstall(callCtx, &agentv1.SkillServiceRequestInstallRequest{IdempotencyKey: idem, Candidate: candidate, SecretInputs: secrets})
			if err != nil {
				return err
			}
			installation, confirmationID, taskID = response.GetInstallation(), response.GetConfirmationId(), response.GetTaskId()
			return nil
		})
		if err != nil {
			return nil, c.controlActionError(err, "extension install")
		}
		return map[string]any{"installation": installationMap(installation), "confirmation_id": confirmationID, "task_id": taskID}, nil
	}
}

func (c *Client) extensionUpdate(kind agentv1.CoreExtensionKind) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		capability := "mcp"
		if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL {
			capability = "skill"
		}
		if e := c.capability(ctx, capability); e != nil {
			return nil, e
		}
		idem, e := requiredString(params, "idempotency_key")
		if e != nil {
			return nil, e
		}
		id, e := requiredString(params, "installation_id")
		if e != nil {
			return nil, e
		}
		revision, e := optionalInt64(params, "expected_revision")
		if e != nil {
			return nil, e
		}
		candidate, e := candidateFromParams(params, "candidate", kind)
		if e != nil {
			return nil, e
		}
		secrets, e := secretInputsFromParams(params, "secret_inputs")
		if e != nil {
			return nil, e
		}
		var installation *agentv1.CoreInstallation
		var confirmationID, taskID string
		err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
			if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP {
				mutation := &agentv1.MCPServiceRequestInstallRequest{IdempotencyKey: idem, InstallationId: id, ExpectedRevision: revision, Candidate: candidate, SecretInputs: secrets}
				response, err := agentv1.NewMCPServiceClient(conn).RequestUpdate(callCtx, &agentv1.MCPServiceRequestUpdateRequest{Mutation: mutation})
				if err != nil {
					return err
				}
				installation, confirmationID, taskID = response.GetInstallation(), response.GetConfirmationId(), response.GetTaskId()
				return nil
			}
			mutation := &agentv1.SkillServiceRequestInstallRequest{IdempotencyKey: idem, InstallationId: id, ExpectedRevision: revision, Candidate: candidate, SecretInputs: secrets}
			response, err := agentv1.NewSkillServiceClient(conn).RequestUpdate(callCtx, &agentv1.SkillServiceRequestUpdateRequest{Mutation: mutation})
			if err != nil {
				return err
			}
			installation, confirmationID, taskID = response.GetInstallation(), response.GetConfirmationId(), response.GetTaskId()
			return nil
		})
		if err != nil {
			return nil, c.controlActionError(err, "extension update")
		}
		return map[string]any{"installation": installationMap(installation), "confirmation_id": confirmationID, "task_id": taskID}, nil
	}
}

func (c *Client) extensionRemove(kind agentv1.CoreExtensionKind) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		capability := "mcp"
		if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL {
			capability = "skill"
		}
		if e := c.capability(ctx, capability); e != nil {
			return nil, e
		}
		idem, e := requiredString(params, "idempotency_key")
		if e != nil {
			return nil, e
		}
		id, e := requiredString(params, "installation_id")
		if e != nil {
			return nil, e
		}
		revision, e := optionalInt64(params, "expected_revision")
		if e != nil {
			return nil, e
		}
		var installation *agentv1.CoreInstallation
		var confirmationID, taskID string
		err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
			if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP {
				response, err := agentv1.NewMCPServiceClient(conn).RequestUninstall(callCtx, &agentv1.MCPServiceRequestUninstallRequest{IdempotencyKey: idem, InstallationId: id, ExpectedRevision: revision})
				if err != nil {
					return err
				}
				installation, confirmationID, taskID = response.GetInstallation(), response.GetConfirmationId(), response.GetTaskId()
				return nil
			}
			response, err := agentv1.NewSkillServiceClient(conn).RequestUninstall(callCtx, &agentv1.SkillServiceRequestUninstallRequest{IdempotencyKey: idem, InstallationId: id, ExpectedRevision: revision})
			if err != nil {
				return err
			}
			installation, confirmationID, taskID = response.GetInstallation(), response.GetConfirmationId(), response.GetTaskId()
			return nil
		})
		if err != nil {
			return nil, c.controlActionError(err, "extension removal")
		}
		return map[string]any{"installation": installationMap(installation), "confirmation_id": confirmationID, "task_id": taskID}, nil
	}
}

func (c *Client) extensionExecute(kind agentv1.CoreExtensionKind) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		capability := "mcp"
		if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL {
			capability = "skill"
		}
		if e := c.capability(ctx, capability); e != nil {
			return nil, e
		}
		idem, e := requiredString(params, "idempotency_key")
		if e != nil {
			return nil, e
		}
		id, e := requiredString(params, "installation_id")
		if e != nil {
			return nil, e
		}
		revision, e := optionalInt64(params, "expected_revision")
		if e != nil {
			return nil, e
		}
		input, e := structInput(params, "input")
		if e != nil {
			return nil, e
		}
		tool := ""
		if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP {
			tool, e = requiredString(params, "tool_name")
			if e != nil {
				return nil, e
			}
		}
		var taskID string
		err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
			if kind == agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP {
				response, err := agentv1.NewMCPServiceClient(conn).ExecuteTool(callCtx, &agentv1.MCPServiceExecuteToolRequest{IdempotencyKey: idem, InstallationId: id, ExpectedRevision: revision, ToolName: tool, Input: input})
				if err != nil {
					return err
				}
				taskID = response.GetTaskId()
				return nil
			}
			response, err := agentv1.NewSkillServiceClient(conn).Execute(callCtx, &agentv1.SkillServiceExecuteRequest{IdempotencyKey: idem, InstallationId: id, ExpectedRevision: revision, Input: input})
			if err != nil {
				return err
			}
			taskID = response.GetTaskId()
			return nil
		})
		if err != nil {
			return nil, c.controlActionError(err, "extension execution")
		}
		return map[string]any{"task_id": taskID}, nil
	}
}

func (c *Client) extensionListTools(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if e := c.capability(ctx, "mcp"); e != nil {
		return nil, e
	}
	id, e := requiredString(params, "installation_id")
	if e != nil {
		return nil, e
	}
	revision, e := optionalInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	var response *agentv1.MCPServiceListToolsResponse
	err := c.controlPlaneUnary(ctx, func(callCtx context.Context, conn *grpc.ClientConn) error {
		var err error
		response, err = agentv1.NewMCPServiceClient(conn).ListTools(callCtx, &agentv1.MCPServiceListToolsRequest{InstallationId: id, ExpectedRevision: revision})
		return err
	})
	if err != nil {
		return nil, c.controlActionError(err, "MCP tools")
	}
	if response == nil {
		return nil, actionbase.CodedError(http.StatusBadGateway, "agent_core_upstream_failed", "agent core returned an empty MCP tools response")
	}
	tools := make([]any, 0, len(response.GetTools()))
	for _, item := range response.GetTools() {
		tools = append(tools, toolMap(item))
	}
	return map[string]any{"tools": tools}, nil
}
