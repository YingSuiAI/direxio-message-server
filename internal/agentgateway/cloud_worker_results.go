package agentgateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

var cloudWorkerStates = map[string]bool{
	"waiting_user": true, "queued": true, "provisioning": true, "awaiting_worker": true,
	"running": true, "collecting": true, "validating": true, "cleaning": true,
	"succeeded": true, "failed": true, "canceled": true, "rejected": true, "expired": true,
}

const (
	maxCloudWorkerProgressElapsedMS       = int64((24 * time.Hour) / time.Millisecond)
	maxCloudWorkerProgressCPUTimeMS       = int64((7 * 24 * time.Hour) / time.Millisecond)
	maxCloudWorkerProgressMemoryBytes     = int64(64 << 30)
	maxCloudWorkerProgressInvocationCount = int64(1_000_000)
	maxCloudWorkerProgressUploadedBytes   = int64(9 << 20)
)

var cloudWorkerProgressPhases = map[string]bool{
	"claimed": true, "preparing_inputs": true, "running_pi": true,
	"uploading_result": true, "completing": true,
}

func validateCloudWorkerActionResult(action string, request, output map[string]any, authority actionResultAuthority) error {
	if output == nil {
		return cloudWorkerResultError("response is missing")
	}
	if !authority.valid() {
		return cloudWorkerResultError("prepared owner authority is missing")
	}
	switch action {
	case "agent.execution.v2.plans.get":
		value, err := cloudWorkerEnvelope(output, "plan")
		if err != nil {
			return err
		}
		if err := validateCloudWorkerPlan(value); err != nil {
			return err
		}
		if err := validateCloudWorkerAuthority(value, authority); err != nil {
			return err
		}
		return validateCloudWorkerRequestedIdentity(request, value, "plan_id", "revision")
	case "agent.execution.v2.plans.list":
		items, err := cloudWorkerPage(output, "plans")
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := validateCloudWorkerPlan(item); err != nil {
				return err
			}
			if err := validateCloudWorkerAuthority(item, authority); err != nil {
				return err
			}
		}
		return nil
	case "agent.execution.v2.runs.get", "agent.execution.v2.runs.cancel":
		value, err := cloudWorkerEnvelope(output, "run")
		if err != nil {
			return err
		}
		if err := validateCloudWorkerRun(value); err != nil {
			return err
		}
		if err := validateCloudWorkerAuthority(value, authority); err != nil {
			return err
		}
		return validateCloudWorkerRequestedIdentity(request, value, "run_id", "")
	case "agent.execution.v2.runs.list":
		items, err := cloudWorkerPage(output, "runs")
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := validateCloudWorkerRun(item); err != nil {
				return err
			}
			if err := validateCloudWorkerAuthority(item, authority); err != nil {
				return err
			}
		}
		return nil
	case "agent.execution.v2.runs.events":
		if err := cloudExact(output, []string{"events", "next_sequence", "history_truncated"}, nil, "events envelope"); err != nil {
			return err
		}
		historyTruncated, ok := output["history_truncated"].(bool)
		if !ok {
			return cloudWorkerResultError("history_truncated must be a boolean")
		}
		raw, ok := output["events"].([]any)
		if !ok {
			return cloudWorkerResultError("events must be an array")
		}
		after := int64(0)
		if value, present := request["after_sequence"]; present {
			var valid bool
			after, valid = cloudInteger(value)
			if !valid || after < 0 {
				return cloudWorkerResultError("prepared run event cursor is invalid")
			}
		}
		last := after
		expectedRunID, ok := request["run_id"].(string)
		if !ok || !cloudUUID(expectedRunID) {
			return cloudWorkerResultError("prepared run event identity is missing")
		}
		for index, value := range raw {
			event, ok := value.(map[string]any)
			if !ok {
				return cloudWorkerResultError("event must be an object")
			}
			sequence, err := validateCloudWorkerEvent(event)
			if err != nil || sequence <= last {
				return cloudWorkerResultError("event sequence is invalid")
			}
			if (index > 0 || !historyTruncated) && sequence != last+1 {
				return cloudWorkerResultError("event sequence is invalid")
			}
			if index == 0 && historyTruncated && sequence == last+1 {
				return cloudWorkerResultError("event sequence is invalid")
			}
			if err := validateCloudWorkerAuthority(event, authority); err != nil {
				return err
			}
			if event["run_id"] != expectedRunID {
				return cloudWorkerResultError("event run identity does not match the prepared request")
			}
			last = sequence
		}
		next, ok := cloudInteger(output["next_sequence"])
		if !ok || next < after || (len(raw) > 0 && next != last) || (len(raw) == 0 && !historyTruncated && next != after) ||
			(len(raw) == 0 && historyTruncated && next == after) {
			return cloudWorkerResultError("next_sequence is invalid")
		}
		return nil
	case "agent.execution.v2.artifacts.get":
		value, err := cloudWorkerEnvelope(output, "artifact")
		if err != nil {
			return err
		}
		if err := validateCloudWorkerArtifact(value); err != nil {
			return err
		}
		if err := validateCloudWorkerAuthority(value, authority); err != nil {
			return err
		}
		return validateCloudWorkerRequestedIdentity(request, value, "artifact_id", "")
	case "agent.execution.v2.artifacts.download":
		return validateCloudWorkerArtifactDownload(request, output, authority)
	default:
		return cloudWorkerResultError("record_kind=cloud_worker is unsupported for %s", action)
	}
}

func cloudWorkerEnvelope(output map[string]any, key string) (map[string]any, error) {
	if err := cloudExact(output, []string{key}, nil, key+" envelope"); err != nil {
		return nil, err
	}
	value, ok := output[key].(map[string]any)
	if !ok {
		return nil, cloudWorkerResultError("%s must be an object", key)
	}
	return value, nil
}

func cloudWorkerPage(output map[string]any, key string) ([]map[string]any, error) {
	if err := cloudExact(output, []string{key, "next_page_token"}, nil, key+" page"); err != nil {
		return nil, err
	}
	if _, ok := output["next_page_token"].(string); !ok {
		return nil, cloudWorkerResultError("next_page_token must be a string")
	}
	raw, ok := output[key].([]any)
	if !ok {
		return nil, cloudWorkerResultError("%s must be an array", key)
	}
	items := make([]map[string]any, 0, len(raw))
	for _, value := range raw {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, cloudWorkerResultError("%s item must be an object", key)
		}
		items = append(items, item)
	}
	return items, nil
}

func validateCloudWorkerPlan(plan map[string]any) error {
	required := []string{
		"owner_id", "account_generation", "plan_id", "revision", "status", "digest", "execution_id", "task_id", "confirmation_id", "conversation_id", "turn_id",
		"recipe_id", "adapter", "objective_summary", "proposal_reason", "input_manifest_digest", "input_manifest_item_count", "workspace_mode", "model_authorization", "aws", "compute", "limits",
		"network_grants", "secret_grants", "artifact_retention_seconds", "quote", "execution_digest", "created_at", "updated_at",
	}
	if err := cloudExact(plan, required, nil, "plan"); err != nil {
		return err
	}
	if err := cloudOwned(plan, "plan_id"); err != nil {
		return err
	}
	for _, key := range []string{"execution_id", "task_id", "confirmation_id", "conversation_id", "turn_id"} {
		if !cloudUUID(plan[key]) {
			return cloudWorkerResultError("plan %s is not a canonical UUID", key)
		}
	}
	for _, key := range []string{"digest", "input_manifest_digest", "execution_digest"} {
		if !cloudDigest(plan[key]) {
			return cloudWorkerResultError("plan %s is not a digest", key)
		}
	}
	if plan["recipe_id"] != "ephemeral-pi-task" || plan["adapter"] != "pi_json_task_v1" || plan["status"] != "waiting_user" || !cloudWorkspace(plan["workspace_mode"]) {
		return cloudWorkerResultError("plan recipe, adapter, status, or workspace mode is invalid")
	}
	if !cloudNonemptyString(plan["objective_summary"]) || !map[string]bool{"explicit_user_cloud": true, "central_delegation": true, "local_budget_exceeded": true}[cloudString(plan["proposal_reason"])] {
		return cloudWorkerResultError("plan proposal is invalid")
	}
	if count, ok := cloudInteger(plan["input_manifest_item_count"]); !ok || count < 0 {
		return cloudWorkerResultError("plan input manifest count is invalid")
	}
	if retention, ok := cloudInteger(plan["artifact_retention_seconds"]); !ok || retention <= 0 {
		return cloudWorkerResultError("plan artifact retention is invalid")
	}
	if err := cloudTimestampOrder(plan, "created_at", "updated_at"); err != nil {
		return err
	}
	if err := validateCloudModelAuthorization(plan["model_authorization"]); err != nil {
		return err
	}
	if err := validateCloudAWS(plan["aws"]); err != nil {
		return err
	}
	if err := validateCloudCompute(plan["compute"]); err != nil {
		return err
	}
	if err := validateCloudLimits(plan["limits"]); err != nil {
		return err
	}
	if err := cloudStringArray(plan["network_grants"], "network_grants"); err != nil {
		return err
	}
	if err := validateCloudSecretGrants(plan["secret_grants"]); err != nil {
		return err
	}
	return validateCloudQuote(plan["quote"])
}

func validateCloudWorkerRun(run map[string]any) error {
	required := []string{
		"owner_id", "account_generation", "run_id", "execution_id", "plan_id", "plan_revision", "plan_digest", "task_id", "confirmation_id", "conversation_id", "turn_id",
		"status", "revision", "digest", "workspace_mode", "quote_digest", "execution_digest", "cancellation_requested", "cleanup", "artifact_ids", "failure_code", "failure_summary", "created_at", "updated_at",
	}
	if err := cloudExact(run, required, nil, "run"); err != nil {
		return err
	}
	if err := cloudOwned(run, "run_id"); err != nil {
		return err
	}
	if run["run_id"] != run["execution_id"] {
		return cloudWorkerResultError("run_id and execution_id differ")
	}
	for _, key := range []string{"plan_id", "task_id", "confirmation_id", "conversation_id", "turn_id"} {
		if !cloudUUID(run[key]) {
			return cloudWorkerResultError("run %s is not a canonical UUID", key)
		}
	}
	for _, key := range []string{"plan_digest", "digest", "quote_digest", "execution_digest"} {
		if !cloudDigest(run[key]) {
			return cloudWorkerResultError("run %s is not a digest", key)
		}
	}
	state := cloudString(run["status"])
	if !cloudWorkerStates[state] || !cloudWorkspace(run["workspace_mode"]) {
		return cloudWorkerResultError("run status or workspace mode is invalid")
	}
	if revision, ok := cloudInteger(run["plan_revision"]); !ok || revision <= 0 {
		return cloudWorkerResultError("run plan revision is invalid")
	}
	if _, ok := run["failure_code"].(string); !ok {
		return cloudWorkerResultError("run failure_code must be a string")
	}
	if _, ok := run["failure_summary"].(string); !ok {
		return cloudWorkerResultError("run failure_summary must be a string")
	}
	if _, ok := run["cancellation_requested"].(bool); !ok {
		return cloudWorkerResultError("run cancellation_requested must be a boolean")
	}
	cleanup, err := validateCloudCleanup(run["cleanup"])
	if err != nil {
		return err
	}
	if state == "succeeded" && !cleanup.verified || (state == "failed" || state == "canceled") && cleanup.total > 0 && !cleanup.verified || (state == "rejected" || state == "expired") && cleanup.total != 0 {
		return cloudWorkerResultError("run terminal state precedes verified cleanup")
	}
	if err := cloudUUIDArray(run["artifact_ids"], "artifact_ids"); err != nil {
		return err
	}
	return cloudTimestampOrder(run, "created_at", "updated_at")
}

func validateCloudWorkerArtifact(artifact map[string]any) error {
	if err := cloudExact(artifact, []string{"owner_id", "account_generation", "artifact_id", "execution_id", "kind", "name", "media_type", "size_bytes", "sha256", "status", "created_at"}, nil, "artifact"); err != nil {
		return err
	}
	if !cloudNonemptyString(artifact["owner_id"]) || !cloudUUID(artifact["artifact_id"]) || !cloudUUID(artifact["execution_id"]) || !cloudNonemptyString(artifact["kind"]) || !cloudNonemptyString(artifact["name"]) || !cloudNonemptyString(artifact["media_type"]) || !cloudDigest(artifact["sha256"]) {
		return cloudWorkerResultError("artifact identity or metadata is invalid")
	}
	if generation, ok := cloudInteger(artifact["account_generation"]); !ok || generation <= 0 {
		return cloudWorkerResultError("artifact account generation is invalid")
	}
	if size, ok := cloudInteger(artifact["size_bytes"]); !ok || size < 0 {
		return cloudWorkerResultError("artifact size is invalid")
	}
	if !map[string]bool{"pending": true, "verified": true, "rejected": true}[cloudString(artifact["status"])] || !cloudTimestamp(artifact["created_at"]) {
		return cloudWorkerResultError("artifact status or timestamp is invalid")
	}
	return nil
}

func validateCloudWorkerArtifactDownload(request, result map[string]any, authority actionResultAuthority) error {
	required := []string{
		"owner_id", "account_generation", "artifact_id", "execution_id", "offset_bytes",
		"data_base64", "chunk_sha256", "artifact_sha256", "size_bytes", "next_offset_bytes", "eof",
	}
	if err := cloudExact(result, required, nil, "artifact download"); err != nil {
		return err
	}
	if err := validateCloudWorkerAuthority(result, authority); err != nil {
		return err
	}
	if err := validateCloudWorkerRequestedIdentity(request, result, "artifact_id", ""); err != nil {
		return err
	}
	if !canonicalActionUUID(result["execution_id"]) || !canonicalActionSHA256(result["chunk_sha256"]) || !canonicalActionSHA256(result["artifact_sha256"]) {
		return cloudWorkerResultError("artifact download identity or digest is invalid")
	}

	requestedOffset, requestedOffsetOK := cloudInteger(request["offset_bytes"])
	requestedMaximum, requestedMaximumOK := cloudInteger(request["max_chunk_bytes"])
	offset, offsetOK := cloudInteger(result["offset_bytes"])
	size, sizeOK := cloudInteger(result["size_bytes"])
	nextOffset, nextOffsetOK := cloudInteger(result["next_offset_bytes"])
	eof, eofOK := result["eof"].(bool)
	if !requestedOffsetOK || requestedOffset < 0 || requestedOffset >= maxCloudWorkerArtifactBytes || !requestedMaximumOK || requestedMaximum < 1 || requestedMaximum > maxCloudWorkerArtifactChunkBytes ||
		!offsetOK || offset != requestedOffset || !sizeOK || size < 1 || size > maxCloudWorkerArtifactBytes || offset >= size || !nextOffsetOK || nextOffset <= offset || nextOffset > size || !eofOK {
		return cloudWorkerResultError("artifact download range metadata is invalid")
	}

	encoded, ok := result["data_base64"].(string)
	if !ok || len(encoded) > base64.StdEncoding.EncodedLen(int(maxCloudWorkerArtifactChunkBytes)) {
		return cloudWorkerResultError("artifact download chunk encoding is invalid")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded || len(decoded) == 0 || int64(len(decoded)) > requestedMaximum || int64(len(decoded)) > maxCloudWorkerArtifactChunkBytes {
		clear(decoded)
		return cloudWorkerResultError("artifact download chunk encoding is invalid")
	}
	defer clear(decoded)
	if nextOffset != offset+int64(len(decoded)) || eof != (nextOffset == size) {
		return cloudWorkerResultError("artifact download chunk range is inconsistent")
	}
	chunkDigest := sha256.Sum256(decoded)
	if result["chunk_sha256"] != hex.EncodeToString(chunkDigest[:]) {
		return cloudWorkerResultError("artifact download chunk digest does not match its bytes")
	}
	return nil
}

func validateCloudWorkerEvent(event map[string]any) (int64, error) {
	if err := cloudExact(event, []string{"event_id", "run_id", "owner_id", "account_generation", "revision", "sequence", "type", "at", "payload_digest"}, []string{"status", "progress"}, "event"); err != nil {
		return 0, err
	}
	eventType, eventTypeOK := event["type"].(string)
	if !cloudUUID(event["event_id"]) || !cloudUUID(event["run_id"]) || !cloudNonemptyString(event["owner_id"]) ||
		!eventTypeOK || eventType == "" || eventType != strings.TrimSpace(eventType) || !cloudTimestamp(event["at"]) || !cloudDigest(event["payload_digest"]) {
		return 0, cloudWorkerResultError("event identity or metadata is invalid")
	}
	if status, present := event["status"]; present && !cloudWorkerStates[cloudString(status)] {
		return 0, cloudWorkerResultError("event status is invalid")
	}
	progress, hasProgress := event["progress"]
	if eventType == "worker_progress" {
		if !hasProgress {
			return 0, cloudWorkerResultError("worker_progress event is missing progress")
		}
		if err := validateCloudWorkerProgress(progress, event["at"]); err != nil {
			return 0, err
		}
	} else if hasProgress {
		return 0, cloudWorkerResultError("lifecycle event contains progress")
	}
	revision, ok := cloudInteger(event["revision"])
	if !ok || revision <= 0 {
		return 0, cloudWorkerResultError("event revision is invalid")
	}
	sequence, ok := cloudInteger(event["sequence"])
	if !ok || sequence <= 0 {
		return 0, cloudWorkerResultError("event sequence is invalid")
	}
	return sequence, nil
}

func validateCloudWorkerProgress(value, eventAtValue any) error {
	progress, ok := value.(map[string]any)
	if !ok || cloudExact(progress, []string{
		"phase", "elapsed_ms", "last_activity_at", "cpu_time_ms", "memory_high_water_bytes",
		"invocation_count", "uploaded_bytes", "output_truncated",
	}, nil, "progress") != nil {
		return cloudWorkerResultError("progress is invalid")
	}
	phase, ok := progress["phase"].(string)
	if !ok || phase != strings.TrimSpace(phase) || !cloudWorkerProgressPhases[phase] {
		return cloudWorkerResultError("progress phase is invalid")
	}
	for key, maximum := range map[string]int64{
		"elapsed_ms":              maxCloudWorkerProgressElapsedMS,
		"cpu_time_ms":             maxCloudWorkerProgressCPUTimeMS,
		"memory_high_water_bytes": maxCloudWorkerProgressMemoryBytes,
		"invocation_count":        maxCloudWorkerProgressInvocationCount,
		"uploaded_bytes":          maxCloudWorkerProgressUploadedBytes,
	} {
		number, valid := cloudInteger(progress[key])
		if !valid || number < 0 || number > maximum {
			return cloudWorkerResultError("progress %s is invalid", key)
		}
	}
	if _, ok := progress["output_truncated"].(bool); !ok {
		return cloudWorkerResultError("progress output_truncated is invalid")
	}
	lastActivityAt, lastActivityOK := cloudTime(progress["last_activity_at"])
	eventAt, eventAtOK := cloudTime(eventAtValue)
	if !lastActivityOK || !eventAtOK || lastActivityAt.After(eventAt) {
		return cloudWorkerResultError("progress last_activity_at is invalid")
	}
	return nil
}

func validateCloudWorkerAuthority(value map[string]any, authority actionResultAuthority) error {
	owner, ownerOK := value["owner_id"].(string)
	generation, generationOK := cloudInteger(value["account_generation"])
	if !authority.valid() || !ownerOK || owner != strings.TrimSpace(owner) || owner != authority.ownerID ||
		!generationOK || generation != authority.accountGeneration {
		return cloudWorkerResultError("result owner authority does not match the prepared request")
	}
	return nil
}

func validateCloudWorkerRequestedIdentity(request, value map[string]any, idField, revisionField string) error {
	expectedID, ok := request[idField].(string)
	if !ok || !cloudUUID(expectedID) || value[idField] != expectedID {
		return cloudWorkerResultError("result identity does not match the prepared request")
	}
	if revisionField == "" {
		return nil
	}
	expectedRevisionValue, present := request[revisionField]
	if !present {
		return nil
	}
	expectedRevision, expectedOK := cloudInteger(expectedRevisionValue)
	actualRevision, actualOK := cloudInteger(value[revisionField])
	if !expectedOK || expectedRevision <= 0 || !actualOK || actualRevision != expectedRevision {
		return cloudWorkerResultError("result revision does not match the prepared request")
	}
	return nil
}

func validateCloudModelAuthorization(value any) error {
	object, ok := value.(map[string]any)
	if !ok || cloudExact(object, []string{"model_profile_id", "model_profile_revision", "provider", "model", "interface", "credential_version"}, nil, "model_authorization") != nil || !cloudUUID(object["model_profile_id"]) || !cloudNonemptyString(object["provider"]) || !cloudNonemptyString(object["model"]) || !cloudNonemptyString(object["interface"]) {
		return cloudWorkerResultError("model authorization is invalid")
	}
	for _, key := range []string{"model_profile_revision", "credential_version"} {
		if number, ok := cloudInteger(object[key]); !ok || number <= 0 {
			return cloudWorkerResultError("model authorization %s is invalid", key)
		}
	}
	return nil
}

func validateCloudAWS(value any) error {
	object, ok := value.(map[string]any)
	if !ok || cloudExact(object, []string{"account_id", "region", "credential_revision"}, nil, "aws") != nil || len(cloudString(object["account_id"])) != 12 || !cloudNonemptyString(object["region"]) {
		return cloudWorkerResultError("AWS binding is invalid")
	}
	if revision, ok := cloudInteger(object["credential_revision"]); !ok || revision <= 0 {
		return cloudWorkerResultError("AWS credential revision is invalid")
	}
	return nil
}

func validateCloudCompute(value any) error {
	object, ok := value.(map[string]any)
	required := []string{"instance_type", "architecture", "root_device_name", "volume_gib", "volume_type", "volume_iops", "volume_throughput_mib", "ami_id", "ami_digest", "worker_release_digest", "pi_runtime_digest", "host_network_policy_sha256"}
	if !ok || cloudExact(object, required, nil, "compute") != nil {
		return cloudWorkerResultError("compute binding is invalid")
	}
	for _, key := range []string{"instance_type", "architecture", "root_device_name", "volume_type", "ami_id"} {
		if !cloudNonemptyString(object[key]) {
			return cloudWorkerResultError("compute %s is invalid", key)
		}
	}
	for _, key := range []string{"ami_digest", "worker_release_digest", "pi_runtime_digest", "host_network_policy_sha256"} {
		if !cloudDigest(object[key]) {
			return cloudWorkerResultError("compute %s is invalid", key)
		}
	}
	for _, key := range []string{"volume_gib", "volume_iops", "volume_throughput_mib"} {
		if number, ok := cloudInteger(object[key]); !ok || number <= 0 {
			return cloudWorkerResultError("compute %s is invalid", key)
		}
	}
	return nil
}

func validateCloudLimits(value any) error {
	object, ok := value.(map[string]any)
	if !ok || cloudExact(object, []string{"max_runtime_seconds", "max_output_bytes"}, []string{"max_tokens"}, "limits") != nil {
		return cloudWorkerResultError("limits are invalid")
	}
	for _, key := range []string{"max_runtime_seconds", "max_output_bytes"} {
		if number, ok := cloudInteger(object[key]); !ok || number <= 0 {
			return cloudWorkerResultError("limit %s is invalid", key)
		}
	}
	// New Plans have no cumulative model-token allowance. A positive value is
	// accepted only so signed historical Plans remain readable.
	if value, present := object["max_tokens"]; present {
		if number, ok := cloudInteger(value); !ok || number <= 0 {
			return cloudWorkerResultError("limit max_tokens is invalid")
		}
	}
	return nil
}

func validateCloudSecretGrants(value any) error {
	items, ok := value.([]any)
	if !ok {
		return cloudWorkerResultError("secret_grants must be an array")
	}
	for _, value := range items {
		grant, ok := value.(map[string]any)
		if !ok || cloudExact(grant, []string{"purpose"}, nil, "secret grant") != nil || !cloudNonemptyString(grant["purpose"]) || len(cloudString(grant["purpose"])) > 64 {
			return cloudWorkerResultError("secret grant is invalid")
		}
	}
	return nil
}

func validateCloudQuote(value any) error {
	quote, ok := value.(map[string]any)
	if !ok || cloudExact(quote, []string{"amount_micros", "currency", "source_time", "expires_at", "maximum_authorized_cost_micros", "digest"}, nil, "quote") != nil || quote["currency"] != "USD" || !cloudDigest(quote["digest"]) {
		return cloudWorkerResultError("quote is invalid")
	}
	amount, amountOK := cloudInteger(quote["amount_micros"])
	maximum, maximumOK := cloudInteger(quote["maximum_authorized_cost_micros"])
	source, sourceOK := cloudTime(quote["source_time"])
	expires, expiresOK := cloudTime(quote["expires_at"])
	if !amountOK || !maximumOK || amount < 0 || maximum < amount || !sourceOK || !expiresOK || !expires.After(source) {
		return cloudWorkerResultError("quote cost or validity window is invalid")
	}
	return nil
}

type cloudCleanup struct {
	verified bool
	total    int64
}

func validateCloudCleanup(value any) (cloudCleanup, error) {
	cleanup, ok := value.(map[string]any)
	if !ok || cloudExact(cleanup, []string{"verified_destroyed", "resources_total", "resources_verified_destroyed"}, []string{"verified_at"}, "cleanup") != nil {
		return cloudCleanup{}, cloudWorkerResultError("cleanup is invalid")
	}
	verified, ok := cleanup["verified_destroyed"].(bool)
	total, totalOK := cloudInteger(cleanup["resources_total"])
	destroyed, destroyedOK := cloudInteger(cleanup["resources_verified_destroyed"])
	_, hasVerifiedAt := cleanup["verified_at"]
	if !ok || !totalOK || !destroyedOK || total < 0 || destroyed < 0 || destroyed > total || verified != (total > 0 && destroyed == total) || verified != hasVerifiedAt || hasVerifiedAt && !cloudTimestamp(cleanup["verified_at"]) {
		return cloudCleanup{}, cloudWorkerResultError("cleanup evidence is inconsistent")
	}
	return cloudCleanup{verified: verified, total: total}, nil
}

func cloudOwned(value map[string]any, idField string) error {
	if !cloudNonemptyString(value["owner_id"]) || !cloudUUID(value[idField]) {
		return cloudWorkerResultError("owner or %s is invalid", idField)
	}
	generation, generationOK := cloudInteger(value["account_generation"])
	revision, revisionOK := cloudInteger(value["revision"])
	if !generationOK || generation <= 0 || !revisionOK || revision <= 0 {
		return cloudWorkerResultError("account generation or revision is invalid")
	}
	return nil
}

func cloudExact(value map[string]any, required, optional []string, name string) error {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = true
		if _, present := value[key]; !present {
			return cloudWorkerResultError("%s is missing %s", name, key)
		}
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range value {
		if !allowed[key] {
			return cloudWorkerResultError("%s contains unknown field %s", name, key)
		}
	}
	return nil
}

func cloudInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		if number >= math.MinInt64 && number <= math.MaxInt64 && math.Trunc(number) == number {
			return int64(number), true
		}
	case int:
		return int64(number), true
	case int64:
		return number, true
	case uint64:
		if number <= math.MaxInt64 {
			return int64(number), true
		}
	case json.Number:
		result, err := number.Int64()
		return result, err == nil
	}
	return 0, false
}

func cloudString(value any) string {
	result, _ := value.(string)
	return strings.TrimSpace(result)
}

func cloudNonemptyString(value any) bool { return cloudString(value) != "" }

func cloudUUID(value any) bool {
	raw := cloudString(value)
	parsed, err := uuid.Parse(raw)
	return err == nil && parsed.String() == raw
}

func cloudDigest(value any) bool {
	raw := cloudString(value)
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(raw) == 64 && len(decoded) == 32 && raw == strings.ToLower(raw)
}

func cloudWorkspace(value any) bool {
	return map[string]bool{"none": true, "read_only": true, "write": true}[cloudString(value)]
}

func cloudTime(value any) (time.Time, bool) {
	raw, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	return parsed, err == nil
}

func cloudTimestamp(value any) bool {
	_, ok := cloudTime(value)
	return ok
}

func cloudTimestampOrder(value map[string]any, first, second string) error {
	created, createdOK := cloudTime(value[first])
	updated, updatedOK := cloudTime(value[second])
	if !createdOK || !updatedOK || updated.Before(created) {
		return cloudWorkerResultError("timestamps are invalid")
	}
	return nil
}

func cloudStringArray(value any, name string) error {
	items, ok := value.([]any)
	if !ok {
		return cloudWorkerResultError("%s must be an array", name)
	}
	for _, item := range items {
		if !cloudNonemptyString(item) {
			return cloudWorkerResultError("%s contains an invalid string", name)
		}
	}
	return nil
}

func cloudUUIDArray(value any, name string) error {
	items, ok := value.([]any)
	if !ok {
		return cloudWorkerResultError("%s must be an array", name)
	}
	seen := map[string]bool{}
	for _, item := range items {
		id := cloudString(item)
		if !cloudUUID(item) || seen[id] {
			return cloudWorkerResultError("%s contains an invalid or duplicate UUID", name)
		}
		seen[id] = true
	}
	return nil
}

func cloudWorkerResultError(format string, args ...any) error {
	return fmt.Errorf("%w: Cloud Worker "+format, append([]any{ErrInvalidActionResult}, args...)...)
}
