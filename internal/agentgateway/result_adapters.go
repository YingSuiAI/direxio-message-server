package agentgateway

// The Capability API intentionally exposes Agent Core DTOs, while the
// message-server ProductCore surface has a frozen response contract. Keep the
// translation here at the gateway boundary so clients never need to know Core
// field names, pagination cursors, or envelope changes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ErrInvalidActionResult marks a successful Agent operation whose payload
// violates the public ProductCore response contract. Callers must surface the
// failure instead of converting a malformed payload into a successful empty
// projection.
var ErrInvalidActionResult = errors.New("native agent action result is invalid")

const (
	knowledgeQuotaLimitBytes      int64 = 64 << 20
	knowledgeMaxSourceBytes       int64 = 16 << 20
	maxTextToolOutputBytes              = 64 << 10
	maxTextToolSources                  = 5
	maxTextToolSourceTitleBytes         = 512
	maxTextToolSourceURLBytes           = 8 << 10
	maxTextToolSourceSnippetBytes       = 4 << 10
	maxNodeArtifactBytes          int64 = 64 << 20
	maxNodeArtifactFiles          int64 = 8192
	managedNodeVersion                  = "v24.18.1"
	managedNPMVersion                   = "11.16.0"
)

// actionResultAuthority is captured once by Runner.prepare and travels with
// the capability response. Result validation must never call the mutable
// OwnerID/AccountGeneration getters after the request has been dispatched.
type actionResultAuthority struct {
	ownerID           string
	accountGeneration int64
}

func (authority actionResultAuthority) valid() bool {
	return strings.TrimSpace(authority.ownerID) != "" && authority.accountGeneration > 0
}

func adaptActionResult(action string, output map[string]any) (map[string]any, error) {
	return adaptActionResultForRequest(action, nil, output)
}

func adaptActionResultForRequest(action string, request, output map[string]any) (map[string]any, error) {
	return adaptActionResultForRequestWithAuthority(action, request, output, actionResultAuthority{})
}

func adaptActionResultForRequestWithAuthority(action string, request, output map[string]any, authority actionResultAuthority) (map[string]any, error) {
	recordKind, _ := request["record_kind"].(string)
	if strings.TrimSpace(recordKind) == "cloud_worker" {
		if err := validateCloudWorkerActionResult(strings.TrimSpace(action), request, output, authority); err != nil {
			return nil, err
		}
	}
	if err := validateActionResult(strings.TrimSpace(action), request, output, authority); err != nil {
		return nil, err
	}
	return projectActionResult(action, output), nil
}

func projectActionResult(action string, output map[string]any) map[string]any {
	if output == nil {
		return map[string]any{}
	}
	result := cloneParams(output)
	switch strings.TrimSpace(action) {
	case "agent.chat.conversations.create", "agent.chat.conversations.rename":
		return conversationMutationResult(result)
	case "agent.chat.conversations.delete":
		return conversationDeleteResult(result)
	case "agent.chat.conversations.get":
		return conversationGetResult(result)
	case "agent.chat.conversations.list":
		return conversationListResult(result)
	case "agent.chat.turns.list":
		return turnsListResult(result)
	case "agent.chat.attachment.commit":
		return map[string]any{"attachment": result}
	case "agent.core.tasks.get", "agent.core.tasks.cancel", "agent.core.tasks.retry":
		return taskResult(result)
	case "agent.core.tasks.list":
		return taskListResult(result)
	case "agent.core.tasks.events":
		return eventsResult(result)
	case "agent.core.schedules.create", "agent.core.schedules.get", "agent.core.schedules.update", "agent.core.schedules.pause", "agent.core.schedules.resume":
		return map[string]any{"schedule": result["schedule"]}
	case "agent.core.schedules.delete":
		return scheduleDeleteResult(result)
	case "agent.core.schedules.list":
		return scheduleListResult(result)
	case "agent.core.schedules.trigger":
		return scheduleTriggerResult(result)
	case "agent.core.mcp.list", "agent.core.skills.list":
		return installationListResult(result)
	case "agent.core.mcp.get", "agent.core.skills.get":
		return map[string]any{"installation": normalizeInstallation(result)}
	case "agent.core.mcp.install", "agent.core.mcp.update", "agent.core.mcp.remove", "agent.core.skills.install", "agent.core.skills.update", "agent.core.skills.remove":
		return installationMutationResult(result)
	case "agent.model_profiles.sync":
		return modelSyncResult(result)
	case "agent.models.list":
		return modelCatalogResult(result)
	case "agent.model_profiles.list":
		return modelListResult(result)
	case "agent.model_profiles.get":
		return modelGetResult(result)
	case "agent.model_profiles.delete":
		return modelDeleteResult(result)
	case "agent.knowledge.sources.list":
		return sourceListResult(result)
	case "agent.knowledge.config.get", "agent.knowledge.config.update":
		return embeddingConfigResult(result)
	case "agent.knowledge.sources.delete":
		return sourceDeleteResult(result)
	case "agent.knowledge.upload.start":
		return uploadResult(result, true)
	case "agent.knowledge.upload.chunk":
		return uploadResult(result, false)
	case "agent.knowledge.upload.finish":
		return uploadFinishResult(result)
	case "agent.knowledge.search":
		return knowledgeSearchResult(result)
	case "agent.knowledge.status":
		return knowledgeStatusResult(result)
	case "agent.chat":
		return chatResult(result)
	case "agent.web_search.config.get", "agent.web_search.config.update":
		return webSearchConfigResult(result)
	case "agent.memory.config.get", "agent.memory.config.update":
		return memoryConfigResult(result)
	case "agent.memory.status":
		return memoryStatusResult(result)
	case "agent.memory.facts.update":
		return memoryFactResult(result)
	case "agent.memory.facts.delete":
		return mapProjection(result, []string{"fact_id", "deleted"})
	case "agent.static_sites.list":
		return mapProjection(result, []string{"releases", "next_page_token"})
	case "agent.static_sites.delete":
		return mapProjection(result, []string{"release_id", "deleted", "replayed"})
	case "agent.web_search.test":
		return webSearchTestResult(result)
	case "agent.text_tools.config.get", "agent.text_tools.config.update":
		return textToolsConfigResult(result)
	case "agent.text_tools.execute":
		return textToolsExecutionResult(result)
	case "agent.image_tools.upload.begin", "agent.image_tools.upload.append", "agent.image_tools.upload.commit",
		"agent.image_tools.extract_text", "agent.image_tools.translate_text":
		return result
	case "agent.core.confirmations.get", "agent.core.confirmations.confirm", "agent.core.confirmations.reject":
		return confirmationResult(result)
	case "agent.core.confirmations.list":
		return confirmationListResult(result)
	case "agent.core.confirmations.acknowledge_extension_execution_uncertain":
		return confirmationAcknowledgeResult(result)
	case "agent.core.aws.credentials.create", "agent.core.aws.credentials.update":
		return map[string]any{"credential": result["credential"]}
	case "agent.core.aws.credentials.delete":
		return mapProjection(result, []string{"deleted", "credential_id"})
	case "agent.core.aws.credentials.list":
		return mapProjection(result, []string{"credentials", "next_page_token"})
	case "agent.core.aws.credentials.test":
		return mapProjection(result, []string{"credential_id", "account_id", "user_arn", "principal_id", "credential_revision", "tested_at"})
	case "agent.core.mcp.discover", "agent.core.skills.discover":
		return mapProjection(result, []string{"candidates", "next_page_token"})
	case "agent.core.mcp.inspect", "agent.core.skills.inspect":
		return map[string]any{"inspection": result["inspection"]}
	case "agent.core.mcp.list_tools":
		return mapProjection(result, []string{"tools"})
	case "agent.core.mcp.execute", "agent.core.skills.execute":
		return mapProjection(result, []string{"confirmation_id", "task_id"})
	default:
		return result
	}
}

func validateActionResult(action string, request, output map[string]any, authority actionResultAuthority) error {
	switch action {
	case "agent.core.confirmations.get", "agent.core.confirmations.list", "agent.core.confirmations.confirm", "agent.core.confirmations.reject":
		return validateCloudWorkerConfirmationActionResult(action, request, output, authority)
	case "agent.chat.attachment.begin", "agent.chat.attachment.append", "agent.chat.attachment.commit":
		return validateChatAttachmentActionResult(action, request, output, authority)
	case "agent.web_search.config.get", "agent.web_search.config.update":
		return validateWebSearchConfigResult(action, output)
	case "agent.memory.config.get", "agent.memory.config.update", "agent.memory.status", "agent.memory.facts.update", "agent.memory.facts.delete":
		return validateMemoryResult(action, output)
	case "agent.static_sites.list", "agent.static_sites.delete":
		return validateStaticSiteResult(action, request, output)
	case "agent.web_search.test":
		return validateWebSearchTestResult(action, output)
	case "agent.text_tools.config.get", "agent.text_tools.config.update":
		return validateTextToolsConfigResult(action, output)
	case "agent.text_tools.execute":
		return validateTextToolsExecutionResult(action, request, output)
	case "agent.image_tools.upload.begin", "agent.image_tools.upload.append", "agent.image_tools.upload.commit",
		"agent.image_tools.extract_text", "agent.image_tools.translate_text":
		return validateImageToolActionResult(action, request, output, authority)
	case "agent.models.list":
		return validateModelCatalogResult(output)
	case "agent.model_profiles.sync", "agent.model_profiles.list":
		return validateToolModelDefaultResult(output)
	case "agent.model_profiles.get", "agent.model_profiles.delete",
		"agent.knowledge.sources.delete", "agent.knowledge.upload.finish",
		"agent.core.tasks.get", "agent.core.tasks.cancel", "agent.core.tasks.retry",
		"agent.core.schedules.create", "agent.core.schedules.get", "agent.core.schedules.update", "agent.core.schedules.pause", "agent.core.schedules.resume", "agent.core.schedules.trigger",
		"agent.core.skills.get",
		"agent.core.skills.install", "agent.core.skills.update", "agent.core.skills.remove",
		"agent.core.aws.credentials.create", "agent.core.aws.credentials.update",
		"agent.core.mcp.inspect", "agent.core.skills.inspect":
		return validateCurrentAgentResultShape(action, output)
	case "agent.chat.turn.stop":
		return validateTurnStopResult(request, output)
	case "agent.chat.turn.steer":
		return validateTurnSteerResult(request, output)
	case "agent.chat.turns.list":
		return validateTurnsListResult(output)
	case "agent.core.mcp.get", "agent.core.mcp.list", "agent.core.mcp.install", "agent.core.mcp.update", "agent.core.mcp.remove":
		if err := validateCurrentAgentResultShape(action, output); err != nil {
			return err
		}
		return validateCoreMCPNodeReceipts(action, output)
	case "agent.knowledge.status":
		return validateKnowledgeStatusResult(output)
	case "agent.chat":
		return validateChatResult(output, authority)
	default:
		return nil
	}
}

func validateCoreMCPNodeReceipts(action string, output map[string]any) error {
	installations := make([]map[string]any, 0, 1)
	switch action {
	case "agent.core.mcp.list":
		for _, raw := range anySlice(output["installations"]) {
			installation, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: MCP installation list contains a non-object item", ErrInvalidActionResult)
			}
			installations = append(installations, installation)
		}
	case "agent.core.mcp.install", "agent.core.mcp.update", "agent.core.mcp.remove":
		installation, ok := output["installation"].(map[string]any)
		if !ok {
			return fmt.Errorf("%w: MCP mutation installation is missing", ErrInvalidActionResult)
		}
		installations = append(installations, installation)
	default:
		installations = append(installations, output)
	}
	for _, installation := range installations {
		if err := validateCoreMCPInstallationNodeReceipts(installation); err != nil {
			return err
		}
	}
	return nil
}

func validateCurrentAgentResultShape(action string, output map[string]any) error {
	requireObject := func(field string) error {
		if _, ok := output[field].(map[string]any); !ok {
			return fmt.Errorf("%w: %s result requires %s", ErrInvalidActionResult, action, field)
		}
		return nil
	}
	rejectObject := func(field, identity string) error {
		if _, wrapped := output[field]; wrapped {
			return fmt.Errorf("%w: %s result must be the unwrapped Agent %s", ErrInvalidActionResult, action, identity)
		}
		if value, ok := output[identity].(string); !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s result requires %s", ErrInvalidActionResult, action, identity)
		}
		return nil
	}
	switch action {
	case "agent.model_profiles.get", "agent.model_profiles.delete":
		return rejectObject("profile", "id")
	case "agent.knowledge.sources.delete":
		return requireObject("source")
	case "agent.knowledge.upload.finish":
		if err := requireObject("upload"); err != nil {
			return err
		}
		return requireObject("source")
	case "agent.core.tasks.get", "agent.core.tasks.cancel", "agent.core.tasks.retry":
		return rejectObject("task", "id")
	case "agent.core.schedules.create", "agent.core.schedules.get", "agent.core.schedules.update", "agent.core.schedules.pause", "agent.core.schedules.resume":
		return requireObject("schedule")
	case "agent.core.schedules.trigger":
		for _, field := range []string{"schedule", "occurrence", "task"} {
			if err := requireObject(field); err != nil {
				return err
			}
		}
		return nil
	case "agent.core.mcp.get", "agent.core.skills.get":
		return rejectObject("installation", "id")
	case "agent.core.mcp.install", "agent.core.mcp.update", "agent.core.mcp.remove",
		"agent.core.skills.install", "agent.core.skills.update", "agent.core.skills.remove":
		return requireObject("installation")
	case "agent.core.aws.credentials.create", "agent.core.aws.credentials.update":
		return requireObject("credential")
	case "agent.core.mcp.inspect", "agent.core.skills.inspect":
		return requireObject("inspection")
	default:
		return nil
	}
}

func validateCoreMCPInstallationNodeReceipts(installation map[string]any) error {
	transport, _ := installation["transport"].(string)
	source, _ := installation["source"].(string)
	candidateID, _ := installation["candidate_id"].(string)
	activeVersionID, _ := installation["active_version_id"].(string)
	activeReceipt := false
	for _, raw := range anySlice(installation["versions"]) {
		version, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: MCP installation version is invalid", ErrInvalidActionResult)
		}
		if !onlyResultFields(version, "version_id", "pin", "content_digest", "manifest_digest", "execution_digest", "network_schema_digest", "secret_schema_digest", "execution", "created_at", "network_grants", "secret_grants", "node_artifact") {
			return fmt.Errorf("%w: MCP installation version contains a non-public field", ErrInvalidActionResult)
		}
		versionID, _ := version["version_id"].(string)
		receiptRaw, present := version["node_artifact"]
		if !present || receiptRaw == nil {
			continue
		}
		if transport != "stdio_node" {
			return fmt.Errorf("%w: Node receipt is attached to a non-Node extension", ErrInvalidActionResult)
		}
		receipt, ok := receiptRaw.(map[string]any)
		if !ok || !exactResultFields(receipt, "package_name", "package_version", "artifact_bytes", "file_count", "node_version", "npm_version", "lifecycle_scripts_disabled", "native_addons_absent") {
			return fmt.Errorf("%w: Node receipt shape is invalid", ErrInvalidActionResult)
		}
		packageName, packageNameOK := receipt["package_name"].(string)
		packageVersion, packageVersionOK := receipt["package_version"].(string)
		if !packageNameOK || !validExtensionNPMPackageName(packageName) || !packageVersionOK || !validExtensionExactSemver(packageVersion) || !actionIntegerInRange(receipt["artifact_bytes"], 1, maxNodeArtifactBytes) || !actionIntegerInRange(receipt["file_count"], 1, maxNodeArtifactFiles) || receipt["node_version"] != managedNodeVersion || receipt["npm_version"] != managedNPMVersion || receipt["lifecycle_scripts_disabled"] != true || receipt["native_addons_absent"] != true {
			return fmt.Errorf("%w: Node receipt values are invalid", ErrInvalidActionResult)
		}
		if source == "npm" {
			pin, pinOK := version["pin"].(map[string]any)
			registryVersion, versionOK := pin["registry_version"].(string)
			if !pinOK || candidateID == "" || packageName != candidateID || !versionOK || packageVersion != registryVersion {
				return fmt.Errorf("%w: Node receipt does not match its immutable npm source", ErrInvalidActionResult)
			}
		}
		if versionID != "" && versionID == activeVersionID {
			activeReceipt = true
		}
	}
	if transport == "stdio_node" && activeVersionID != "" && !activeReceipt {
		return fmt.Errorf("%w: published Node active version receipt is missing", ErrInvalidActionResult)
	}
	return nil
}

func exactResultFields(value map[string]any, fields ...string) bool {
	if len(value) != len(fields) {
		return false
	}
	for _, field := range fields {
		if _, present := value[field]; !present {
			return false
		}
	}
	return true
}

func onlyResultFields(value map[string]any, fields ...string) bool {
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field] = struct{}{}
	}
	for field := range value {
		if _, ok := allowed[field]; !ok {
			return false
		}
	}
	return true
}

func validateToolModelDefaultResult(output map[string]any) error {
	for _, alias := range []string{"default_tool_profile_id", "DefaultToolProfileID", "DefaultToolClientProfileID"} {
		if _, present := output[alias]; present {
			return fmt.Errorf("%w: default tool client profile id must use the canonical field", ErrInvalidActionResult)
		}
	}
	value := output["default_tool_client_profile_id"]
	profileID, ok := value.(string)
	if !ok {
		return fmt.Errorf("%w: default tool client profile id must be a string", ErrInvalidActionResult)
	}
	if profileID == "" {
		return nil
	}
	profiles, _ := actionObjectSlice(output["profiles"])
	for _, profile := range profiles {
		if profile["client_profile_id"] != profileID {
			continue
		}
		kind, _ := profile["model_kind"].(string)
		if kind == "" || kind == "conversation" {
			return nil
		}
		return fmt.Errorf("%w: default tool client profile must reference a conversation profile", ErrInvalidActionResult)
	}
	// A paginated profile response need not contain the separately identified
	// default. Agent validates the authoritative role binding before returning.
	return nil
}

func validateTextToolsConfigResult(action string, output map[string]any) error {
	if output == nil {
		return fmt.Errorf("%w: text tools config response is missing", ErrInvalidActionResult)
	}
	if err := rejectUnexpectedTextToolsResultFields(output, "enabled", "revision", "tools", "updated_at"); err != nil {
		return err
	}
	if _, ok := output["enabled"].(bool); !ok {
		return fmt.Errorf("%w: text tools config enabled must be a boolean", ErrInvalidActionResult)
	}
	if revision, ok := turnInt64(output["revision"]); !ok || revision < 0 {
		return fmt.Errorf("%w: text tools config revision must be a non-negative integer", ErrInvalidActionResult)
	}
	if err := validateTextTools(action, output["tools"]); err != nil {
		return fmt.Errorf("%w: text tools config is invalid", ErrInvalidActionResult)
	}
	updatedAt, ok := output["updated_at"].(string)
	if !ok || len(updatedAt) == 0 || len(updatedAt) > 128 || !utf8.ValidString(updatedAt) {
		return fmt.Errorf("%w: text tools config updated_at is invalid", ErrInvalidActionResult)
	}
	if _, err := time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return fmt.Errorf("%w: text tools config updated_at is invalid", ErrInvalidActionResult)
	}
	return nil
}

func validateTextToolsExecutionResult(action string, request, output map[string]any) error {
	if output == nil {
		return fmt.Errorf("%w: text tools execution response is missing", ErrInvalidActionResult)
	}
	if err := rejectUnexpectedTextToolsResultFields(output, "tool_id", "output", "sources"); err != nil {
		return err
	}
	toolID := output["tool_id"]
	if !validTextToolID(toolID) || request == nil || toolID != request["tool_id"] {
		return fmt.Errorf("%w: text tools execution tool_id does not match the request", ErrInvalidActionResult)
	}
	if !validTextToolString(output["output"], 1, maxTextToolOutputBytes, false) {
		return fmt.Errorf("%w: text tools execution output must be UTF-8 of 1 to 65536 bytes", ErrInvalidActionResult)
	}
	sources, ok := actionObjectSlice(output["sources"])
	if !ok || len(sources) > maxTextToolSources {
		return fmt.Errorf("%w: text tools execution sources must contain at most 5 items", ErrInvalidActionResult)
	}
	for _, source := range sources {
		if err := rejectUnexpectedTextToolsResultFields(source, "title", "url", "snippet"); err != nil {
			return err
		}
		if !validTextToolString(source["title"], 1, maxTextToolSourceTitleBytes, false) ||
			!validTextToolString(source["url"], 1, maxTextToolSourceURLBytes, false) ||
			!validTextToolString(source["snippet"], 0, maxTextToolSourceSnippetBytes, false) {
			return fmt.Errorf("%w: text tools execution source is invalid", ErrInvalidActionResult)
		}
	}
	return nil
}

func rejectUnexpectedTextToolsResultFields(value map[string]any, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = struct{}{}
	}
	for field := range value {
		if _, ok := allowedSet[field]; !ok {
			return fmt.Errorf("%w: text tools response contains an unexpected field", ErrInvalidActionResult)
		}
	}
	return nil
}

func textToolsConfigResult(result map[string]any) map[string]any {
	projected := map[string]any{
		"enabled":    result["enabled"],
		"revision":   result["revision"],
		"updated_at": result["updated_at"],
	}
	tools, _ := actionObjectSlice(result["tools"])
	projectedTools := make([]any, 0, len(tools))
	for _, tool := range tools {
		projectedTools = append(projectedTools, map[string]any{
			"tool_id":       tool["tool_id"],
			"name":          tool["name"],
			"system_prompt": tool["system_prompt"],
			"order":         tool["order"],
			"enabled":       tool["enabled"],
		})
	}
	projected["tools"] = projectedTools
	return projected
}

func textToolsExecutionResult(result map[string]any) map[string]any {
	projected := map[string]any{
		"tool_id": result["tool_id"],
		"output":  result["output"],
	}
	sources, _ := actionObjectSlice(result["sources"])
	projectedSources := make([]any, 0, len(sources))
	for _, source := range sources {
		projectedSources = append(projectedSources, map[string]any{
			"title":   source["title"],
			"url":     source["url"],
			"snippet": source["snippet"],
		})
	}
	projected["sources"] = projectedSources
	return projected
}

func memoryConfigResult(value map[string]any) map[string]any {
	return mapProjection(value, []string{"enabled", "embedding_configured", "embedding_profile_id", "embedding_model", "revision", "updated_at"})
}

func memoryStatusResult(value map[string]any) map[string]any {
	result := memoryConfigResult(value)
	for _, field := range []string{"active_fact_count", "timeline_event_count", "pending_observation_count", "failed_observation_count"} {
		result[field] = value[field]
	}
	facts, _ := actionObjectSlice(value["facts"])
	projectedFacts := make([]any, 0, len(facts))
	for _, fact := range facts {
		projectedFacts = append(projectedFacts, mapProjection(fact, []string{"id", "subject", "predicate", "value", "kind", "confidence", "valid_from", "last_confirmed_at"}))
	}
	result["facts"] = projectedFacts
	events, _ := actionObjectSlice(value["timeline"])
	projectedEvents := make([]any, 0, len(events))
	for _, event := range events {
		projectedEvents = append(projectedEvents, mapProjection(event, []string{"kind", "summary", "effective_at", "observed_at"}))
	}
	result["timeline"] = projectedEvents
	return result
}

func memoryFactResult(value map[string]any) map[string]any {
	return mapProjection(value, []string{"id", "subject", "predicate", "value", "kind", "confidence", "valid_from", "last_confirmed_at"})
}

func validateMemoryResult(action string, output map[string]any) error {
	if output == nil {
		return fmt.Errorf("%w: %s response is missing", ErrInvalidActionResult, action)
	}
	if strings.HasSuffix(action, ".facts.update") {
		if !validMemoryFact(output) {
			return fmt.Errorf("%w: updated memory fact is invalid", ErrInvalidActionResult)
		}
		return nil
	}
	if strings.HasSuffix(action, ".facts.delete") {
		if !canonicalTurnUUID(output["fact_id"]) || output["deleted"] != true {
			return fmt.Errorf("%w: deleted memory fact result is invalid", ErrInvalidActionResult)
		}
		return nil
	}
	for _, field := range []string{"enabled", "embedding_configured"} {
		if _, ok := output[field].(bool); !ok {
			return fmt.Errorf("%w: memory %s must be a boolean", ErrInvalidActionResult, field)
		}
	}
	revision, ok := turnInt64(output["revision"])
	if !ok || revision < 0 {
		return fmt.Errorf("%w: memory revision is invalid", ErrInvalidActionResult)
	}
	if updatedAt, present := output["updated_at"]; present {
		if !validRFC3339String(updatedAt) {
			return fmt.Errorf("%w: memory updated_at is invalid", ErrInvalidActionResult)
		}
	}
	configured, _ := output["embedding_configured"].(bool)
	profileID, _ := output["embedding_profile_id"].(string)
	model, _ := output["embedding_model"].(string)
	if configured != (uuid.Validate(profileID) == nil && strings.TrimSpace(model) != "") {
		return fmt.Errorf("%w: memory embedding configuration is inconsistent", ErrInvalidActionResult)
	}
	if !configured && (profileID != "" || model != "") {
		return fmt.Errorf("%w: unconfigured memory response contains embedding identity", ErrInvalidActionResult)
	}
	if strings.HasSuffix(action, ".status") {
		for _, field := range []string{"active_fact_count", "timeline_event_count", "pending_observation_count", "failed_observation_count"} {
			value, valid := turnInt64(output[field])
			if !valid || value < 0 {
				return fmt.Errorf("%w: memory %s is invalid", ErrInvalidActionResult, field)
			}
		}
		facts, ok := actionObjectSlice(output["facts"])
		if !ok || len(facts) > 128 {
			return fmt.Errorf("%w: memory facts must be an array", ErrInvalidActionResult)
		}
		for _, fact := range facts {
			if !validMemoryFact(fact) {
				return fmt.Errorf("%w: memory fact is invalid", ErrInvalidActionResult)
			}
		}
		timeline, ok := actionObjectSlice(output["timeline"])
		if !ok || len(timeline) > 64 {
			return fmt.Errorf("%w: memory timeline must be an array", ErrInvalidActionResult)
		}
		for _, event := range timeline {
			if !validMemoryTimelineEvent(event) {
				return fmt.Errorf("%w: memory timeline event is invalid", ErrInvalidActionResult)
			}
		}
	}
	return nil
}

func validateStaticSiteResult(action string, request, output map[string]any) error {
	if output == nil {
		return fmt.Errorf("%w: %s response is missing", ErrInvalidActionResult, action)
	}
	if action == "agent.static_sites.delete" {
		if !exactResultFields(output, "release_id", "deleted", "replayed") ||
			!canonicalTurnUUID(output["release_id"]) || output["deleted"] != true {
			return fmt.Errorf("%w: static-site delete response is invalid", ErrInvalidActionResult)
		}
		if replayed, ok := output["replayed"].(bool); !ok || replayed && request == nil {
			return fmt.Errorf("%w: static-site delete replay state is invalid", ErrInvalidActionResult)
		}
		if request == nil || output["release_id"] != request["release_id"] {
			return fmt.Errorf("%w: static-site delete identity does not match the request", ErrInvalidActionResult)
		}
		return nil
	}
	if !exactResultFields(output, "releases", "next_page_token") {
		return fmt.Errorf("%w: static-site list response is invalid", ErrInvalidActionResult)
	}
	if next, ok := output["next_page_token"].(string); !ok || next != strings.TrimSpace(next) || len(next) > 4096 {
		return fmt.Errorf("%w: static-site list cursor is invalid", ErrInvalidActionResult)
	}
	releases, ok := actionObjectSlice(output["releases"])
	if !ok || len(releases) > 100 {
		return fmt.Errorf("%w: static-site release list is invalid", ErrInvalidActionResult)
	}
	seen := make(map[string]struct{}, len(releases))
	for _, release := range releases {
		if !exactResultFields(release, "site_id", "release_id", "conversation_id", "public_url", "public_path", "size_bytes", "created_at") ||
			!canonicalTurnUUID(release["site_id"]) || !canonicalTurnUUID(release["release_id"]) || !canonicalTurnUUID(release["conversation_id"]) ||
			!actionIntegerInRange(release["size_bytes"], 1, 196608) || !validStaticSitePublicLocation(release) {
			return fmt.Errorf("%w: static-site release is invalid", ErrInvalidActionResult)
		}
		createdAt, ok := release["created_at"].(string)
		if !ok || len(createdAt) > 128 {
			return fmt.Errorf("%w: static-site release timestamp is invalid", ErrInvalidActionResult)
		}
		if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return fmt.Errorf("%w: static-site release timestamp is invalid", ErrInvalidActionResult)
		}
		releaseID := release["release_id"].(string)
		if _, duplicate := seen[releaseID]; duplicate {
			return fmt.Errorf("%w: static-site release is duplicated", ErrInvalidActionResult)
		}
		seen[releaseID] = struct{}{}
	}
	return nil
}

func validStaticSitePublicLocation(release map[string]any) bool {
	publicPath, pathOK := release["public_path"].(string)
	publicURL, urlOK := release["public_url"].(string)
	siteID, siteOK := release["site_id"].(string)
	releaseID, releaseOK := release["release_id"].(string)
	wantPath := "/.sites/" + siteID + "/" + releaseID + "/"
	if !pathOK || !urlOK || publicPath != strings.TrimSpace(publicPath) || publicURL != strings.TrimSpace(publicURL) ||
		!siteOK || !releaseOK || publicPath != wantPath {
		return false
	}
	parsed, err := url.Parse(publicURL)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.EscapedPath() != publicPath {
		return false
	}
	return parsed.Scheme == "https" || parsed.Scheme == "http" && parsed.Hostname() == "localhost"
}

func validMemoryFact(fact map[string]any) bool {
	id, idOK := fact["id"].(string)
	predicate, predicateOK := fact["predicate"].(string)
	value, valueOK := fact["value"].(string)
	kind, kindOK := fact["kind"].(string)
	confidence, confidenceOK := finiteNumber(fact["confidence"])
	if !idOK || uuid.Validate(id) != nil || fact["subject"] != "user" || !predicateOK || strings.TrimSpace(predicate) == "" || len(predicate) > 128 || !valueOK || strings.TrimSpace(value) == "" || len(value) > 2048 || !kindOK || !stringIn(kind, "identity", "preference", "relationship", "goal", "constraint", "context", "fact") || !confidenceOK || confidence < 0 || confidence > 1 {
		return false
	}
	return validRFC3339String(fact["valid_from"]) && validRFC3339String(fact["last_confirmed_at"])
}

func finiteNumber(value any) (float64, bool) {
	var result float64
	switch typed := value.(type) {
	case float64:
		result = typed
	case float32:
		result = float64(typed)
	case int:
		result = float64(typed)
	case int64:
		result = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		result = parsed
	default:
		return 0, false
	}
	return result, !math.IsNaN(result) && !math.IsInf(result, 0)
}

func stringIn(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func validMemoryTimelineEvent(event map[string]any) bool {
	kind, kindOK := event["kind"].(string)
	summary, summaryOK := event["summary"].(string)
	return kindOK && stringIn(kind, "added", "confirmed", "replaced", "retracted") && summaryOK && strings.TrimSpace(summary) != "" && len(summary) <= 4096 && validRFC3339String(event["effective_at"]) && validRFC3339String(event["observed_at"])
}

func validRFC3339String(value any) bool {
	text, ok := value.(string)
	if !ok || text == "" || len(text) > 128 || !utf8.ValidString(text) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, text)
	return err == nil
}

func validateKnowledgeStatusResult(output map[string]any) error {
	if output == nil {
		return fmt.Errorf("%w: knowledge status response is missing", ErrInvalidActionResult)
	}
	values := make(map[string]int64, 4)
	for _, field := range []string{
		"quota_used_bytes", "quota_limit_bytes", "quota_remaining_bytes", "max_source_bytes",
	} {
		value := output[field]
		if value == nil {
			return fmt.Errorf("%w: knowledge status %s is required", ErrInvalidActionResult, field)
		}
		integer, ok := turnInt64(value)
		if !ok || integer < 0 {
			return fmt.Errorf("%w: knowledge status %s must be a non-negative integer", ErrInvalidActionResult, field)
		}
		values[field] = integer
	}
	used := values["quota_used_bytes"]
	limit := values["quota_limit_bytes"]
	remaining := values["quota_remaining_bytes"]
	maxSource := values["max_source_bytes"]
	if limit != knowledgeQuotaLimitBytes || maxSource != knowledgeMaxSourceBytes {
		return fmt.Errorf("%w: knowledge status quota limits violate the product contract", ErrInvalidActionResult)
	}
	if used > limit || remaining != limit-used {
		return fmt.Errorf("%w: knowledge status quota counters are inconsistent", ErrInvalidActionResult)
	}
	if maxSource > limit {
		return fmt.Errorf("%w: knowledge status max_source_bytes exceeds quota_limit_bytes", ErrInvalidActionResult)
	}
	return nil
}

func validateModelCatalogResult(output map[string]any) error {
	if output == nil {
		return fmt.Errorf("%w: model catalog response is missing", ErrInvalidActionResult)
	}
	modelsValue := output["models"]
	models, ok := modelsValue.([]any)
	if !ok {
		return fmt.Errorf("%w: model catalog models must be an array", ErrInvalidActionResult)
	}
	providersValue := output["providers"]
	providers, ok := providersValue.([]any)
	if !ok {
		return fmt.Errorf("%w: model catalog providers must be an array", ErrInvalidActionResult)
	}
	for index, item := range models {
		entry, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: model catalog model %d is malformed", ErrInvalidActionResult, index)
		}
		normalized := normalizeModelCatalogEntries([]any{entry}, map[string][]string{
			"id":                {"id"},
			"name":              {"name"},
			"provider":          {"provider"},
			"context_length":    {"context_length"},
			"context_window":    {"context_window"},
			"max_output_tokens": {"max_output_tokens"},
			"input_modalities":  {"input_modalities"},
			"output_modalities": {"output_modalities"},
		}, validModelCatalogModel)
		if len(normalized) != 1 {
			return fmt.Errorf("%w: model catalog model %d violates the schema", ErrInvalidActionResult, index)
		}
	}
	for index, item := range providers {
		entry, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: model catalog provider %d is malformed", ErrInvalidActionResult, index)
		}
		normalized := normalizeModelCatalogEntries([]any{entry}, map[string][]string{
			"provider":         {"provider"},
			"default_base_url": {"default_base_url"},
			"requires_api_key": {"requires_api_key"},
			"dynamic_models":   {"dynamic_models"},
		}, validModelCatalogProvider)
		if len(normalized) != 1 {
			return fmt.Errorf("%w: model catalog provider %d violates the schema", ErrInvalidActionResult, index)
		}
	}
	return nil
}

func validateWebSearchConfigResult(action string, output map[string]any) error {
	if output == nil {
		return fmt.Errorf("%w: %s response is missing", ErrInvalidActionResult, action)
	}
	if _, ok := output["config"]; ok {
		return fmt.Errorf("%w: %s response must be a config object", ErrInvalidActionResult, action)
	}
	if err := requireWebSearchBool(output, "enabled"); err != nil {
		return err
	}
	if err := requireWebSearchProvider(output); err != nil {
		return err
	}
	if err := requireWebSearchBool(output, "api_key_configured"); err != nil {
		return err
	}
	if err := requireWebSearchInteger(output, "revision", false); err != nil {
		return err
	}
	for _, field := range []string{"api_key_hint", "tested_at", "updated_at"} {
		value, present := webSearchValue(output, field)
		if present {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%w: web search %s must be a string", ErrInvalidActionResult, field)
			}
		}
	}
	return nil
}

func validateWebSearchTestResult(action string, output map[string]any) error {
	if output == nil {
		return fmt.Errorf("%w: %s response is missing", ErrInvalidActionResult, action)
	}
	for _, field := range []string{"ok", "enabled", "api_key_configured"} {
		if err := requireWebSearchBool(output, field); err != nil {
			return err
		}
	}
	if err := requireWebSearchProvider(output); err != nil {
		return err
	}
	if err := requireWebSearchInteger(output, "result_count", false); err != nil {
		return err
	}
	if err := requireWebSearchInteger(output, "revision", true); err != nil {
		return err
	}
	value, present := webSearchValue(output, "tested_at")
	if !present {
		return fmt.Errorf("%w: web search tested_at is required", ErrInvalidActionResult)
	}
	if _, ok := value.(string); !ok {
		return fmt.Errorf("%w: web search tested_at must be a string", ErrInvalidActionResult)
	}
	return nil
}

func webSearchValue(output map[string]any, field string) (any, bool) {
	value, ok := output[field]
	return value, ok
}

func requireWebSearchBool(output map[string]any, field string) error {
	value := output[field]
	if value == nil {
		return fmt.Errorf("%w: web search %s is required", ErrInvalidActionResult, field)
	}
	if _, ok := value.(bool); !ok {
		return fmt.Errorf("%w: web search %s must be a boolean", ErrInvalidActionResult, field)
	}
	return nil
}

func requireWebSearchProvider(output map[string]any) error {
	value := output["provider"]
	provider, ok := value.(string)
	if !ok || strings.TrimSpace(provider) == "" {
		return fmt.Errorf("%w: web search provider must be a string", ErrInvalidActionResult)
	}
	if strings.TrimSpace(provider) != "tavily" {
		return fmt.Errorf("%w: web search provider is unsupported", ErrInvalidActionResult)
	}
	return nil
}

func requireWebSearchInteger(output map[string]any, field string, positive bool) error {
	value := output[field]
	if value == nil {
		return fmt.Errorf("%w: web search %s is required", ErrInvalidActionResult, field)
	}
	if !isJSONInteger(value) {
		return fmt.Errorf("%w: web search %s must be an integer", ErrInvalidActionResult, field)
	}
	if positive && !positiveInteger(value) {
		return fmt.Errorf("%w: web search %s must be positive", ErrInvalidActionResult, field)
	}
	if !positive && isNegativeInteger(value) {
		return fmt.Errorf("%w: web search %s must be non-negative", ErrInvalidActionResult, field)
	}
	return nil
}

func isJSONInteger(value any) bool {
	switch typed := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		value := float64(typed)
		return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Trunc(value) == value
	case float64:
		return !math.IsNaN(typed) && !math.IsInf(typed, 0) && math.Trunc(typed) == typed
	default:
		return false
	}
}

func isNegativeInteger(value any) bool {
	switch typed := value.(type) {
	case int:
		return typed < 0
	case int8:
		return typed < 0
	case int16:
		return typed < 0
	case int32:
		return typed < 0
	case int64:
		return typed < 0
	case float32:
		return typed < 0
	case float64:
		return typed < 0
	default:
		return false
	}
}

func conversationMutationResult(value map[string]any) map[string]any {
	return map[string]any{"conversation": value["conversation"], "replayed": boolValue(value["replayed"])}
}

func conversationDeleteResult(value map[string]any) map[string]any {
	return map[string]any{"conversation": value["conversation"], "replayed": boolValue(value["replayed"])}
}

func conversationGetResult(value map[string]any) map[string]any {
	return map[string]any{
		"conversation": value["conversation"],
		"messages":     anySlice(value["messages"]),
		"next_cursor":  stringValue(value["next_page_token"]),
	}
}

func conversationListResult(value map[string]any) map[string]any {
	conversations := anySlice(value["conversations"])
	return map[string]any{"conversations": conversations, "next_cursor": stringValue(value["next_page_token"])}
}

func turnsListResult(value map[string]any) map[string]any {
	turns := anySlice(value["turns"])
	projected := make([]any, 0, len(turns))
	for _, item := range turns {
		turn := item.(map[string]any)
		projected = append(projected, map[string]any{
			"turn_id":          turn["turn_id"],
			"idempotency_key":  turn["idempotency_key"],
			"conversation_id":  turn["conversation_id"],
			"state":            turn["state"],
			"revision":         turn["revision"],
			"last_sequence":    turn["last_sequence"],
			"terminal_code":    turn["terminal_code"],
			"terminal_summary": turn["terminal_summary"],
			"created_at":       turn["created_at"],
			"updated_at":       turn["updated_at"],
		})
	}
	return map[string]any{
		"turns":       projected,
		"next_cursor": value["next_page_token"],
	}
}

func validateTurnStopResult(request, value map[string]any) error {
	if !validCanonicalTurn(value) {
		return fmt.Errorf("%w: stopped turn metadata is malformed", ErrInvalidActionResult)
	}
	if request == nil || value["turn_id"] != request["turn_id"] {
		return fmt.Errorf("%w: stopped turn identity does not match the request", ErrInvalidActionResult)
	}
	return nil
}

func validateTurnSteerResult(request, value map[string]any) error {
	if value == nil || len(value) != 11 {
		return fmt.Errorf("%w: steered turn metadata is malformed", ErrInvalidActionResult)
	}
	turn := cloneParams(value)
	delete(turn, "steer_idempotency_key")
	if !validCanonicalTurn(turn) {
		return fmt.Errorf("%w: steered turn metadata is malformed", ErrInvalidActionResult)
	}
	if request == nil || value["turn_id"] != request["turn_id"] || value["steer_idempotency_key"] != request["idempotency_key"] || !canonicalTurnUUID(value["steer_idempotency_key"]) {
		return fmt.Errorf("%w: steer receipt does not match the request", ErrInvalidActionResult)
	}
	state, _ := value["state"].(string)
	if state != "accepted" && state != "running" {
		return fmt.Errorf("%w: steered turn is not active", ErrInvalidActionResult)
	}
	return nil
}

func validateTurnsListResult(value map[string]any) error {
	if value == nil || len(value) != 2 {
		return fmt.Errorf("%w: turn list envelope is malformed", ErrInvalidActionResult)
	}
	turns, ok := value["turns"].([]any)
	if !ok {
		return fmt.Errorf("%w: turn list turns must be an array", ErrInvalidActionResult)
	}
	if _, ok := value["next_page_token"].(string); !ok {
		return fmt.Errorf("%w: turn list next_page_token must be a string", ErrInvalidActionResult)
	}
	for index, raw := range turns {
		turn, ok := raw.(map[string]any)
		if !ok || !validCanonicalTurn(turn) {
			return fmt.Errorf("%w: turn list item %d is malformed", ErrInvalidActionResult, index)
		}
	}
	return nil
}

func validCanonicalTurn(turn map[string]any) bool {
	if len(turn) != 10 {
		return false
	}
	for _, key := range []string{
		"turn_id", "idempotency_key", "conversation_id", "state", "revision", "last_sequence",
		"terminal_code", "terminal_summary", "created_at", "updated_at",
	} {
		if _, ok := turn[key]; !ok {
			return false
		}
	}
	if !canonicalTurnUUID(turn["turn_id"]) || !canonicalTurnUUID(turn["idempotency_key"]) || !canonicalTurnUUID(turn["conversation_id"]) {
		return false
	}
	state, ok := turn["state"].(string)
	if !ok {
		return false
	}
	switch state {
	case "accepted", "running", "waiting_confirmation", "completed", "canceled", "failed":
	default:
		return false
	}
	revision, ok := turnInt64(turn["revision"])
	if !ok || revision <= 0 {
		return false
	}
	lastSequence, ok := turnInt64(turn["last_sequence"])
	if !ok || lastSequence < 0 {
		return false
	}
	for _, key := range []string{"terminal_code", "terminal_summary"} {
		if _, ok := turn[key].(string); !ok {
			return false
		}
	}
	var stamps [2]time.Time
	for index, key := range []string{"created_at", "updated_at"} {
		stamp, ok := turn[key].(string)
		if !ok {
			return false
		}
		parsed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return false
		}
		stamps[index] = parsed
	}
	if stamps[1].Before(stamps[0]) {
		return false
	}
	return true
}

func canonicalTurnUUID(value any) bool {
	text, ok := value.(string)
	if !ok || text != strings.TrimSpace(text) {
		return false
	}
	parsed, err := uuid.Parse(text)
	return err == nil && parsed != uuid.Nil && parsed.String() == text
}

func turnInt64(value any) (int64, bool) {
	switch item := value.(type) {
	case int:
		return int64(item), true
	case int8:
		return int64(item), true
	case int16:
		return int64(item), true
	case int32:
		return int64(item), true
	case int64:
		return item, true
	case uint:
		if uint64(item) > math.MaxInt64 {
			return 0, false
		}
		return int64(item), true
	case uint8:
		return int64(item), true
	case uint16:
		return int64(item), true
	case uint32:
		return int64(item), true
	case uint64:
		if item > math.MaxInt64 {
			return 0, false
		}
		return int64(item), true
	case float32:
		value := float64(item)
		if math.IsNaN(value) || math.IsInf(value, 0) || value < -9223372036854775808.0 || value >= 9223372036854775808.0 || math.Trunc(value) != value {
			return 0, false
		}
		return int64(value), true
	case float64:
		if math.IsNaN(item) || math.IsInf(item, 0) || item < -9223372036854775808.0 || item >= 9223372036854775808.0 || math.Trunc(item) != item {
			return 0, false
		}
		return int64(item), true
	case json.Number:
		parsed, err := item.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func modelSyncResult(value map[string]any) map[string]any {
	result := map[string]any{"profiles": normalizeProfiles(value["profiles"])}
	copyModelDefaults(result, value)
	return result
}

func modelCatalogResult(value map[string]any) map[string]any {
	models := normalizeModelCatalogEntries(value["models"], map[string][]string{
		"id":                {"id"},
		"name":              {"name"},
		"provider":          {"provider"},
		"context_length":    {"context_length"},
		"context_window":    {"context_window"},
		"max_output_tokens": {"max_output_tokens"},
		"input_modalities":  {"input_modalities"},
		"output_modalities": {"output_modalities"},
	}, validModelCatalogModel)
	providers := normalizeModelCatalogEntries(value["providers"], map[string][]string{
		"provider":         {"provider"},
		"default_base_url": {"default_base_url"},
		"requires_api_key": {"requires_api_key"},
		"dynamic_models":   {"dynamic_models"},
	}, validModelCatalogProvider)
	return map[string]any{"models": models, "providers": providers}
}

func normalizeModelCatalogEntries(value any, fields map[string][]string, valid func(map[string]any) bool) []any {
	raw := anySlice(value)
	result := make([]any, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		normalized := make(map[string]any, len(fields))
		for canonical, names := range fields {
			if field := entry[names[0]]; field != nil {
				normalized[canonical] = field
			}
		}
		if valid(normalized) {
			result = append(result, normalized)
		}
	}
	return result
}

func validModelCatalogModel(value map[string]any) bool {
	if !nonEmptyCatalogString(value["id"]) || !nonEmptyCatalogString(value["provider"]) {
		return false
	}
	if item, ok := value["name"]; ok && !catalogString(item) {
		return false
	}
	for _, key := range []string{"context_length", "context_window", "max_output_tokens"} {
		if item, ok := value[key]; ok && !catalogInteger(item) {
			return false
		}
	}
	for _, key := range []string{"input_modalities", "output_modalities"} {
		if item, ok := value[key]; ok && !catalogStringList(item) {
			return false
		}
	}
	return true
}

func validModelCatalogProvider(value map[string]any) bool {
	if !nonEmptyCatalogString(value["provider"]) {
		return false
	}
	if item, ok := value["default_base_url"]; ok && !catalogString(item) {
		return false
	}
	for _, key := range []string{"requires_api_key", "dynamic_models"} {
		if _, ok := value[key].(bool); !ok {
			return false
		}
	}
	return true
}

func nonEmptyCatalogString(value any) bool {
	item, ok := value.(string)
	return ok && strings.TrimSpace(item) != ""
}

func catalogString(value any) bool {
	_, ok := value.(string)
	return ok
}

func catalogInteger(value any) bool {
	switch item := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return item == float32(int64(item))
	case float64:
		return item == float64(int64(item))
	default:
		return false
	}
}

func catalogStringList(value any) bool {
	switch items := value.(type) {
	case []string:
		return true
	case []any:
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func modelListResult(value map[string]any) map[string]any {
	result := map[string]any{
		"profiles":        normalizeProfiles(value["profiles"]),
		"next_page_token": stringValue(value["next_page_token"]),
	}
	copyModelDefaults(result, value)
	return result
}

func modelGetResult(value map[string]any) map[string]any {
	return map[string]any{"profile": normalizeProfile(value)}
}

func modelDeleteResult(value map[string]any) map[string]any {
	return map[string]any{"deleted": true, "profile_id": stringValue(value["id"])}
}

func copyModelDefaults(result, value map[string]any) {
	for _, key := range []string{
		"default_conversation_client_profile_id",
		"default_tool_client_profile_id", "default_embedding_client_profile_id",
		"default_speech_client_profile_id",
	} {
		if item := value[key]; item != nil {
			result[key] = item
		}
	}
}

func normalizeProfiles(value any) []any {
	raw := anySlice(value)
	result := make([]any, 0, len(raw))
	for _, item := range raw {
		if profile, ok := item.(map[string]any); ok {
			result = append(result, normalizeProfile(profile))
		}
	}
	return result
}

func normalizeProfile(value map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{"profile_id", "client_profile_id", "display_name", "provider", "base_url", "model", "system_prompt", "api_key_configured", "temperature", "top_p", "max_output_tokens", "context_window", "reasoning_effort", "revision", "credential_version", "model_kind", "input_modalities", "provider_config", "provider_secret_status", "created_at", "updated_at"} {
		if item := value[key]; item != nil {
			result[key] = item
		}
	}
	if profileID := stringValue(value["id"]); profileID != "" {
		result["profile_id"] = profileID
	}
	return result
}

func sourceListResult(value map[string]any) map[string]any {
	raw := anySlice(value["sources"])
	sources := make([]any, 0, len(raw))
	for _, item := range raw {
		if source, ok := item.(map[string]any); ok {
			sources = append(sources, normalizeSource(source))
		}
	}
	return map[string]any{"sources": sources, "next_page_token": stringValue(value["next_page_token"])}
}

func sourceDeleteResult(value map[string]any) map[string]any {
	return map[string]any{"source": normalizeSource(value["source"].(map[string]any)), "replayed": boolValue(value["replayed"])}
}

func uploadResult(value map[string]any, includeReplay bool) map[string]any {
	return normalizeUpload(value, includeReplay)
}

func uploadFinishResult(value map[string]any) map[string]any {
	return map[string]any{"source": normalizeSource(value["source"].(map[string]any))}
}

func knowledgeSearchResult(value map[string]any) map[string]any {
	raw := anySlice(value["items"])
	items := make([]any, 0, len(raw))
	for _, item := range raw {
		if match, ok := item.(map[string]any); ok {
			items = append(items, map[string]any{
				"source_id": stringValue(match["source_id"]),
				"chunk_ref": stringValue(match["chunk_ref"]),
				"snippet":   stringValue(match["snippet"]),
				"score":     numberValue(match["score"]),
			})
		}
	}
	result := map[string]any{"items": items, "next_cursor": stringValue(value["next_page_token"])}
	if mode := stringValue(value["search_mode"]); mode != "" {
		result["search_mode"] = mode
	}
	for _, key := range []string{
		"embedding_profile_id", "embedding_profile_revision", "embedding_model",
		"embedding_generation", "collection_config_digest",
	} {
		if item := value[key]; item != nil {
			result[key] = item
		}
	}
	return result
}

func knowledgeStatusResult(value map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{"supported", "count", "embedding_indexed", "embedding_stale", "ready_count", "uploading_count", "indexing_count", "failed_count", "cleanup_pending_count", "checked_at", "embedding_profile_id", "embedding_profile_revision", "embedding_model", "quota_used_bytes", "quota_limit_bytes", "quota_remaining_bytes", "max_source_bytes"} {
		if item := value[key]; item != nil {
			result[key] = item
		}
	}
	return result
}

func embeddingConfigResult(value map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{"embedding_profile_id", "embedding_profile_revision", "embedding_model", "dimension", "collection", "collection_config_digest", "revision", "updated_at"} {
		if item := value[key]; item != nil {
			result[key] = item
		}
	}
	return result
}

const (
	maxChatRelatedIDs    = 32
	maxChatReferences    = 32
	maxReferenceIdentity = 512
	maxReferenceRoomType = 128
	maxReferenceTitle    = 512
	maxReferencePreview  = 4096
)

var chatReferenceFields = map[string]struct{}{
	"kind": {}, "account_generation": {}, "task_id": {}, "plan_id": {},
	"plan_revision": {}, "plan_digest": {}, "run_id": {}, "run_revision": {},
	"run_digest": {}, "deployment_id": {}, "execution_id": {}, "confirmation_id": {},
	"confirmation_revision": {}, "stage_id": {}, "stage_revision": {},
	"stage_digest": {}, "target_id": {}, "target_revision": {}, "target_digest": {},
	"preview_digest": {}, "binding_digest": {}, "quote_digest": {},
	"execution_digest": {}, "risk_level": {}, "gate_type": {}, "binding_id": {},
	"binding_revision": {}, "project_id": {}, "status": {}, "state": {},
	"worker_id": {},
	"room_id":   {}, "room_type": {}, "channel_id": {}, "post_id": {}, "title": {},
	"preview": {},
}

var canonicalChatResponseFields = map[string]struct{}{
	"request_id": {}, "conversation_id": {}, "revision": {}, "message": {},
	"done": {}, "model_profile_id": {}, "related_task_ids": {},
	"related_plan_ids": {}, "references": {}, "tool_summaries": {}, "tool_results": {},
}

var canonicalDurableChatResponseFields = map[string]struct{}{
	"idempotency_key": {}, "conversation_id": {}, "revision": {}, "message": {},
	"done": {}, "model_profile_id": {}, "related_task_ids": {},
	"related_plan_ids": {}, "references": {}, "tool_summaries": {}, "tool_results": {},
}

var canonicalChatMessageFields = map[string]struct{}{
	"id": {}, "role": {}, "content": {}, "tool_calls": {}, "tool_results": {},
	"created_at": {}, "model_profile_id": {}, "related_task_ids": {},
	"related_plan_ids": {}, "references": {}, "tool_summaries": {},
}

var canonicalChatStreamEventFields = map[string]struct{}{
	"kind": {}, "idempotency_key": {}, "conversation_id": {}, "turn_id": {},
	"revision": {}, "text": {},
	"tool_call": {}, "tool_result": {}, "response": {},
	"error_code": {}, "error_summary": {}, "sequence": {},
	"confirmation_id": {}, "execution_id": {}, "status": {},
}

// chatResult projects only the canonical Agent ChatResponse shape. Assistant
// text and tool calls belong to message; authority-bearing linkage belongs to
// the response root. Alternate casing and wrapper locations are rejected by
// validateChatResult instead of being treated as compatibility aliases.
func chatResult(value map[string]any) map[string]any {
	result := map[string]any{}
	message, _ := value["message"].(map[string]any)
	if text, ok := message["content"].(string); ok && text != "" {
		result["text"] = text
	}
	if toolCalls, present := message["tool_calls"]; present {
		result["tool_calls"] = toolCalls
	}
	for _, field := range []string{"references", "related_task_ids", "related_plan_ids"} {
		if item, present := value[field]; present {
			result[field] = item
		}
	}
	return result
}

func validateChatResult(value map[string]any, authority actionResultAuthority) error {
	return validateCanonicalChatResult(value, authority, canonicalChatResponseFields, "")
}

func validateDurableChatResult(value map[string]any, authority actionResultAuthority) error {
	return validateCanonicalChatResult(value, authority, canonicalDurableChatResponseFields, "idempotency_key")
}

func validateCanonicalChatResult(value map[string]any, authority actionResultAuthority, allowed map[string]struct{}, requiredIdentity string) error {
	if value == nil {
		return fmt.Errorf("%w: canonical chat response is missing", ErrInvalidActionResult)
	}
	if field := firstUnknownChatResultField(value, allowed); field != "" {
		return fmt.Errorf("%w: chat response field %s is not canonical", ErrInvalidActionResult, field)
	}
	if requiredIdentity != "" {
		if !canonicalTurnUUID(value[requiredIdentity]) || !canonicalTurnUUID(value["conversation_id"]) {
			return fmt.Errorf("%w: durable chat response identity is invalid", ErrInvalidActionResult)
		}
		if revision, ok := turnInt64(value["revision"]); !ok || revision <= 0 {
			return fmt.Errorf("%w: durable chat response revision is invalid", ErrInvalidActionResult)
		}
		if done, ok := value["done"].(bool); !ok || !done {
			return fmt.Errorf("%w: durable chat response is not terminal", ErrInvalidActionResult)
		}
	}
	for _, field := range []string{
		"Message", "Response", "response", "text", "Text", "content", "Content",
		"tool_calls", "ToolCalls", "References", "RelatedTaskIDs", "RelatedPlanIDs",
	} {
		if _, present := value[field]; present {
			return fmt.Errorf("%w: chat response field %s is not canonical", ErrInvalidActionResult, field)
		}
	}
	message, ok := value["message"].(map[string]any)
	if !ok {
		return fmt.Errorf("%w: canonical chat response message is missing", ErrInvalidActionResult)
	}
	if field := firstUnknownChatResultField(message, canonicalChatMessageFields); field != "" {
		return fmt.Errorf("%w: chat message field %s is not canonical", ErrInvalidActionResult, field)
	}
	for _, field := range []string{"Content", "ToolCalls", "References", "RelatedTaskIDs", "RelatedPlanIDs", "Message", "Response", "response", "text", "Text"} {
		if _, present := message[field]; present {
			return fmt.Errorf("%w: chat message field %s is not canonical", ErrInvalidActionResult, field)
		}
	}
	if content, present := message["content"]; present {
		if _, ok := content.(string); !ok {
			return fmt.Errorf("%w: chat message content must be a string", ErrInvalidActionResult)
		}
	}
	if toolCalls, present := message["tool_calls"]; present {
		if _, ok := toolCalls.([]any); !ok {
			return fmt.Errorf("%w: chat message tool_calls must be an array", ErrInvalidActionResult)
		}
	}
	for _, field := range []string{"references", "related_task_ids", "related_plan_ids"} {
		rootValue, rootPresent := value[field]
		messageValue, messagePresent := message[field]
		if rootPresent != messagePresent || (rootPresent && !reflect.DeepEqual(rootValue, messageValue)) {
			return fmt.Errorf("%w: chat %s copies are missing or conflict", ErrInvalidActionResult, field)
		}
	}
	return validateCanonicalChatLinkage(value, authority)
}

func validateCanonicalChatLinkage(value map[string]any, authority actionResultAuthority) error {
	for _, field := range []struct {
		name  string
		limit int
	}{
		{name: "related_task_ids", limit: maxChatRelatedIDs},
		{name: "related_plan_ids", limit: maxChatRelatedIDs},
	} {
		item, present := value[field.name]
		if !present {
			continue
		}
		ids, ok := item.([]any)
		if !ok || len(ids) > field.limit {
			return fmt.Errorf("%w: chat %s must be a bounded UUID array", ErrInvalidActionResult, field.name)
		}
		for index, raw := range ids {
			if !canonicalTurnUUID(raw) {
				return fmt.Errorf("%w: chat %s item %d must be a canonical UUID", ErrInvalidActionResult, field.name, index)
			}
		}
	}

	rawReferences, present := value["references"]
	if !present {
		return nil
	}
	references, ok := rawReferences.([]any)
	if !ok || len(references) > maxChatReferences {
		return fmt.Errorf("%w: chat references must be a bounded array", ErrInvalidActionResult)
	}
	seen := make(map[string]struct{}, len(references))
	for index, raw := range references {
		reference, ok := raw.(map[string]any)
		if !ok || !validChatReference(reference, authority) {
			return fmt.Errorf("%w: chat reference %d violates the strict producer schema", ErrInvalidActionResult, index)
		}
		key := chatReferenceKey(reference)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: chat reference %d is duplicated", ErrInvalidActionResult, index)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func chatReferenceKey(reference map[string]any) string {
	normalized := map[string]any{"kind": reference["kind"]}
	for _, field := range []string{
		"task_id", "plan_id", "plan_digest", "run_id", "run_digest", "deployment_id",
		"execution_id", "confirmation_id", "stage_id", "stage_digest", "target_id",
		"target_digest", "preview_digest", "binding_digest", "quote_digest",
		"execution_digest", "risk_level", "gate_type", "binding_id", "project_id", "worker_id",
		"status", "state", "room_id", "room_type", "channel_id", "post_id", "title",
		"preview",
	} {
		if value, ok := reference[field].(string); ok && value != "" {
			normalized[field] = value
		}
	}
	for _, field := range []string{
		"account_generation", "plan_revision", "run_revision", "confirmation_revision",
		"stage_revision", "target_revision", "binding_revision",
	} {
		if value, ok := turnInt64(reference[field]); ok && value != 0 {
			normalized[field] = value
		}
	}
	encoded, _ := json.Marshal(normalized)
	return string(encoded)
}

func validChatReference(reference map[string]any, authority actionResultAuthority) bool {
	if len(reference) == 0 {
		return false
	}
	for field := range reference {
		if _, allowed := chatReferenceFields[field]; !allowed {
			return false
		}
	}
	kind, ok := reference["kind"].(string)
	if !ok || kind == "" || kind != strings.TrimSpace(kind) {
		return false
	}
	switch kind {
	case "room":
		return noExecutionReferenceFields(reference) &&
			validBoundedReferenceString(reference, "room_id", true, maxReferenceIdentity) &&
			validBoundedReferenceString(reference, "room_type", false, maxReferenceRoomType) &&
			zeroReferenceString(reference, "channel_id") && zeroReferenceString(reference, "post_id") &&
			validReferencePresentation(reference)
	case "channel_post":
		return noExecutionReferenceFields(reference) &&
			validBoundedReferenceString(reference, "room_id", true, maxReferenceIdentity) &&
			validBoundedReferenceString(reference, "channel_id", true, maxReferenceIdentity) &&
			validBoundedReferenceString(reference, "post_id", true, maxReferenceIdentity) &&
			zeroReferenceString(reference, "room_type") && validReferencePresentation(reference)
	case "execution_plan", "execution_run", "execution_confirmation":
		if _, cloud := reference["task_id"]; cloud {
			return validCloudWorkerChatReference(reference, kind, authority)
		}
		return validGenericExecutionChatReference(reference, kind)
	case "service_binding":
		return validServiceBindingChatReference(reference)
	default:
		return false
	}
}

func validCloudWorkerChatReference(reference map[string]any, kind string, authority actionResultAuthority) bool {
	generation, generationOK := turnInt64(reference["account_generation"])
	if !authority.valid() || !generationOK || generation != authority.accountGeneration ||
		!canonicalTurnUUID(reference["task_id"]) || !canonicalTurnUUID(reference["plan_id"]) ||
		!validPositiveReferenceInteger(reference["plan_revision"]) || !noConversationReferenceFields(reference) {
		return false
	}
	if !zeroReferenceFields(reference,
		"plan_digest", "run_revision", "run_digest", "deployment_id", "confirmation_revision",
		"stage_id", "stage_revision", "stage_digest", "target_id", "target_revision", "target_digest",
		"preview_digest", "binding_digest", "quote_digest", "execution_digest", "risk_level",
		"gate_type", "binding_id", "binding_revision", "project_id") {
		return false
	}
	switch kind {
	case "execution_plan":
		return canonicalTurnUUID(reference["confirmation_id"]) &&
			zeroReferenceFields(reference, "run_id", "execution_id", "worker_id", "state") &&
			validExecutionReferenceStatus(reference["status"])
	case "execution_run":
		workerValid := true
		if _, present := reference["worker_id"]; present {
			workerValid = validBoundedReferenceString(reference, "worker_id", true, maxReferenceIdentity)
		}
		return canonicalTurnUUID(reference["run_id"]) && canonicalTurnUUID(reference["execution_id"]) &&
			workerValid &&
			zeroReferenceFields(reference, "confirmation_id", "state") &&
			validExecutionReferenceStatus(reference["status"])
	case "execution_confirmation":
		return canonicalTurnUUID(reference["confirmation_id"]) &&
			zeroReferenceFields(reference, "run_id", "execution_id", "worker_id", "status") &&
			validConfirmationReferenceState(reference["state"])
	default:
		return false
	}
}

func validGenericExecutionChatReference(reference map[string]any, kind string) bool {
	if !noCloudReferenceFields(reference) || !noConversationReferenceFields(reference) {
		return false
	}
	switch kind {
	case "execution_plan":
		if !canonicalTurnUUID(reference["plan_id"]) || !validPositiveReferenceInteger(reference["plan_revision"]) ||
			!validReferenceDigest(reference["plan_digest"]) {
			return false
		}
		return zeroReferenceFields(reference,
			"run_id", "run_revision", "run_digest", "deployment_id", "confirmation_id",
			"confirmation_revision", "stage_id", "stage_revision", "stage_digest", "target_id",
			"target_revision", "target_digest", "preview_digest", "binding_digest", "risk_level",
			"gate_type", "binding_id", "binding_revision", "project_id", "status", "state")
	case "execution_run":
		if !canonicalTurnUUID(reference["run_id"]) || !validPositiveReferenceInteger(reference["run_revision"]) ||
			!validReferenceDigest(reference["run_digest"]) || !canonicalTurnUUID(reference["plan_id"]) ||
			!validPositiveReferenceInteger(reference["plan_revision"]) || !validReferenceDigest(reference["plan_digest"]) ||
			!validOptionalReferenceUUID(reference["deployment_id"]) || !validOptionalReferenceString(reference["status"], 64) {
			return false
		}
		return zeroReferenceFields(reference,
			"confirmation_id", "confirmation_revision", "stage_id", "stage_revision", "stage_digest",
			"target_id", "target_revision", "target_digest", "preview_digest", "binding_digest",
			"risk_level", "gate_type", "binding_id", "binding_revision", "project_id", "state")
	case "execution_confirmation":
		if !canonicalTurnUUID(reference["confirmation_id"]) || !canonicalTurnUUID(reference["plan_id"]) ||
			!validPositiveReferenceInteger(reference["plan_revision"]) || !validReferenceDigest(reference["plan_digest"]) ||
			!canonicalTurnUUID(reference["run_id"]) || !validPositiveReferenceInteger(reference["run_revision"]) ||
			!canonicalTurnUUID(reference["stage_id"]) || !validPositiveReferenceInteger(reference["stage_revision"]) ||
			!validReferenceDigest(reference["stage_digest"]) || !canonicalTurnUUID(reference["target_id"]) ||
			!validPositiveReferenceInteger(reference["target_revision"]) || !validReferenceDigest(reference["target_digest"]) ||
			!zeroReferenceFields(reference, "deployment_id", "binding_id", "binding_revision", "project_id", "status") {
			return false
		}
		full := !zeroReferenceValue(reference, "confirmation_revision") ||
			!zeroReferenceValue(reference, "binding_digest") || !zeroReferenceValue(reference, "preview_digest")
		if full {
			return validPositiveReferenceInteger(reference["confirmation_revision"]) &&
				validReferenceDigest(reference["binding_digest"]) && validReferenceDigest(reference["preview_digest"]) &&
				zeroReferenceString(reference, "run_digest") && validOptionalReferenceString(reference["state"], 64) &&
				validOptionalReferenceString(reference["risk_level"], 16) && validOptionalReferenceString(reference["gate_type"], 128)
		}
		return validReferenceDigest(reference["run_digest"]) &&
			zeroReferenceFields(reference, "state", "risk_level", "gate_type")
	default:
		return false
	}
}

func validServiceBindingChatReference(reference map[string]any) bool {
	if !noCloudReferenceFields(reference) || !noConversationReferenceFields(reference) ||
		!canonicalTurnUUID(reference["binding_id"]) || !validPositiveReferenceInteger(reference["binding_revision"]) ||
		!validReferenceDigest(reference["binding_digest"]) || !canonicalTurnUUID(reference["deployment_id"]) ||
		!canonicalTurnUUID(reference["project_id"]) || !canonicalTurnUUID(reference["run_id"]) ||
		!canonicalTurnUUID(reference["target_id"]) || !validPositiveReferenceInteger(reference["target_revision"]) ||
		!validReferenceDigest(reference["target_digest"]) {
		return false
	}
	return zeroReferenceFields(reference,
		"plan_id", "plan_revision", "plan_digest", "run_revision", "run_digest", "confirmation_id",
		"confirmation_revision", "stage_id", "stage_revision", "stage_digest", "preview_digest",
		"risk_level", "gate_type", "status", "state")
}

func zeroReferenceFields(reference map[string]any, fields ...string) bool {
	for _, field := range fields {
		if !zeroReferenceValue(reference, field) {
			return false
		}
	}
	return true
}

func noConversationReferenceFields(reference map[string]any) bool {
	return zeroReferenceFields(reference, "room_id", "room_type", "channel_id", "post_id", "title", "preview")
}

func validOptionalReferenceUUID(value any) bool {
	if value == nil || value == "" {
		return true
	}
	return canonicalTurnUUID(value)
}

func validOptionalReferenceString(value any, limit int) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && utf8.ValidString(text) && text == strings.TrimSpace(text) && len(text) <= limit
}

func noCloudReferenceFields(reference map[string]any) bool {
	for _, field := range []string{
		"account_generation", "task_id", "execution_id", "quote_digest", "execution_digest", "worker_id",
	} {
		if !zeroReferenceValue(reference, field) {
			return false
		}
	}
	return true
}

func noExecutionReferenceFields(reference map[string]any) bool {
	return zeroReferenceFields(reference,
		"account_generation", "task_id", "plan_id", "plan_revision", "plan_digest",
		"run_id", "run_revision", "run_digest", "deployment_id", "execution_id",
		"confirmation_id", "confirmation_revision", "stage_id", "stage_revision", "stage_digest",
		"target_id", "target_revision", "target_digest", "preview_digest", "binding_digest",
		"quote_digest", "execution_digest", "risk_level", "gate_type", "binding_id",
		"binding_revision", "project_id", "worker_id", "status", "state")
}

func zeroReferenceValue(reference map[string]any, field string) bool {
	value, present := reference[field]
	if !present || value == nil {
		return true
	}
	switch typed := value.(type) {
	case string:
		return typed == ""
	default:
		integer, ok := turnInt64(typed)
		return ok && integer == 0
	}
}

func zeroReferenceString(reference map[string]any, field string) bool {
	value, present := reference[field]
	if !present {
		return true
	}
	text, ok := value.(string)
	return ok && text == ""
}

func validBoundedReferenceString(reference map[string]any, field string, required bool, limit int) bool {
	value, present := reference[field]
	if !present {
		return !required
	}
	text, ok := value.(string)
	if !ok || !utf8.ValidString(text) || len(text) > limit {
		return false
	}
	if required {
		return text != "" && text == strings.TrimSpace(text)
	}
	return true
}

func validReferencePresentation(reference map[string]any) bool {
	return validBoundedReferenceString(reference, "title", false, maxReferenceTitle) &&
		validBoundedReferenceString(reference, "preview", false, maxReferencePreview)
}

func validPositiveReferenceInteger(value any) bool {
	integer, ok := turnInt64(value)
	return ok && integer > 0
}

func validReferenceDigest(value any) bool {
	text, ok := value.(string)
	if !ok || len(text) != sha256.Size*2 || text != strings.ToLower(text) {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil
}

func validExecutionReferenceStatus(value any) bool {
	state, ok := value.(string)
	if !ok {
		return false
	}
	switch state {
	case "waiting_user", "queued", "provisioning", "awaiting_worker", "running", "collecting", "validating", "cleaning", "succeeded", "failed", "canceled", "rejected", "expired":
		return true
	default:
		return false
	}
}

func validConfirmationReferenceState(value any) bool {
	state, ok := value.(string)
	if !ok {
		return false
	}
	switch state {
	case "pending", "confirmed", "consumed", "rejected", "expired":
		return true
	default:
		return false
	}
}

func validateChatStreamEvent(value map[string]any, authority actionResultAuthority) error {
	if value == nil {
		return fmt.Errorf("%w: chat stream event is missing", ErrInvalidActionResult)
	}
	if field := firstUnknownChatResultField(value, canonicalChatStreamEventFields); field != "" {
		return fmt.Errorf("%w: chat stream event field %s is not canonical", ErrInvalidActionResult, field)
	}
	kind, ok := value["kind"].(string)
	if !ok {
		return fmt.Errorf("%w: chat stream event kind is missing", ErrInvalidActionResult)
	}
	switch kind {
	case "accepted", "started", "delta", "tool_call", "tool_result", "waiting_confirmation", "done", "error":
	default:
		return fmt.Errorf("%w: chat stream event kind is invalid", ErrInvalidActionResult)
	}
	for _, field := range []string{"idempotency_key", "conversation_id", "turn_id"} {
		if !canonicalTurnUUID(value[field]) {
			return fmt.Errorf("%w: chat stream event %s must be a canonical UUID", ErrInvalidActionResult, field)
		}
	}
	if revision, ok := turnInt64(value["revision"]); !ok || revision <= 0 {
		return fmt.Errorf("%w: chat stream event revision must be positive", ErrInvalidActionResult)
	}
	if sequence, present := value["sequence"]; present {
		if parsed, ok := turnInt64(sequence); !ok || parsed < 0 {
			return fmt.Errorf("%w: chat stream event sequence must be non-negative", ErrInvalidActionResult)
		}
	}
	confirmationFields := []string{"confirmation_id", "execution_id", "status"}
	if kind == "waiting_confirmation" {
		for _, field := range []string{"text", "tool_call", "tool_result", "response", "error_code", "error_summary"} {
			if _, present := value[field]; present {
				return fmt.Errorf("%w: waiting confirmation event must not carry %s", ErrInvalidActionResult, field)
			}
		}
		for _, field := range confirmationFields[:2] {
			if !canonicalTurnUUID(value[field]) {
				return fmt.Errorf("%w: waiting confirmation event %s must be a canonical UUID", ErrInvalidActionResult, field)
			}
		}
		if value["status"] != "waiting_confirmation" {
			return fmt.Errorf("%w: waiting confirmation event status is invalid", ErrInvalidActionResult)
		}
	} else {
		for _, field := range confirmationFields {
			if _, present := value[field]; present {
				return fmt.Errorf("%w: only a waiting confirmation event may carry %s", ErrInvalidActionResult, field)
			}
		}
	}
	for _, field := range []string{
		"event", "Message", "Response", "References", "RelatedTaskIDs", "RelatedPlanIDs",
		"references", "related_task_ids", "related_plan_ids", "content", "Content", "ToolCalls",
	} {
		if _, present := value[field]; present {
			return fmt.Errorf("%w: chat stream event field %s is not canonical", ErrInvalidActionResult, field)
		}
	}
	rawResponse, present := value["response"]
	if !present {
		if kind == "done" {
			return fmt.Errorf("%w: done chat stream response is missing", ErrInvalidActionResult)
		}
		return nil
	}
	if kind != "done" {
		return fmt.Errorf("%w: only a done chat stream event may carry response", ErrInvalidActionResult)
	}
	for _, field := range []string{"text", "tool_calls", "references", "related_task_ids", "related_plan_ids", "message"} {
		if _, duplicate := value[field]; duplicate {
			return fmt.Errorf("%w: chat stream field %s duplicates the canonical response", ErrInvalidActionResult, field)
		}
	}
	response, ok := rawResponse.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: chat stream response must be an object", ErrInvalidActionResult)
	}
	if response["idempotency_key"] != value["idempotency_key"] || response["conversation_id"] != value["conversation_id"] {
		return fmt.Errorf("%w: chat stream response identity conflicts with its turn event", ErrInvalidActionResult)
	}
	return validateDurableChatResult(response, authority)
}

func firstUnknownChatResultField(value map[string]any, allowed map[string]struct{}) string {
	unknown := make([]string, 0)
	for field := range value {
		if _, ok := allowed[field]; !ok {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	sort.Strings(unknown)
	return unknown[0]
}

func promoteChatResultFields(target map[string]any, authority actionResultAuthority) error {
	if target == nil {
		return fmt.Errorf("%w: chat stream done payload is missing", ErrInvalidActionResult)
	}
	rawResponse, present := target["response"]
	if !present {
		return fmt.Errorf("%w: canonical chat stream response is missing", ErrInvalidActionResult)
	}
	response, ok := rawResponse.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: chat stream response must be an object", ErrInvalidActionResult)
	}
	if err := validateChatStreamEvent(target, authority); err != nil {
		return err
	}
	projected := chatResult(response)
	for field, item := range projected {
		if existing, present := target[field]; present && !reflect.DeepEqual(existing, item) {
			return fmt.Errorf("%w: chat stream field %s conflicts with canonical response", ErrInvalidActionResult, field)
		}
		target[field] = item
	}
	delete(target, "response")
	return nil
}

func webSearchConfigResult(value map[string]any) map[string]any {
	result := mapProjection(value,
		[]string{"enabled", "provider", "api_key_configured", "revision", "tested_at", "updated_at"},
	)
	if boolValue(value["api_key_configured"]) {
		result["api_key_hint"] = "configured"
	}
	return result
}

func webSearchTestResult(value map[string]any) map[string]any {
	return mapProjection(value,
		[]string{"ok", "provider", "result_count", "tested_at", "enabled", "api_key_configured", "revision"},
	)
}

func confirmationResult(value map[string]any) map[string]any {
	if confirmation, ok := value["confirmation"].(map[string]any); ok {
		return map[string]any{"confirmation": projectCloudWorkerConfirmation(confirmation)}
	}
	return map[string]any{"confirmation": projectCloudWorkerConfirmation(value)}
}

func confirmationListResult(value map[string]any) map[string]any {
	confirmations := anySlice(value["confirmations"])
	for index, item := range confirmations {
		if confirmation, ok := item.(map[string]any); ok {
			confirmations[index] = projectCloudWorkerConfirmation(confirmation)
		}
	}
	return map[string]any{
		"confirmations":   confirmations,
		"next_page_token": stringValue(value["next_page_token"]),
	}
}

func confirmationAcknowledgeResult(value map[string]any) map[string]any {
	return mapProjection(value, []string{"confirmation", "task", "resolution", "reservation_released"})
}

func mapProjection(value map[string]any, keys []string) map[string]any {
	result := make(map[string]any, len(keys))
	for _, key := range keys {
		if item := value[key]; item != nil {
			result[key] = item
		}
	}
	return result
}

func taskResult(value map[string]any) map[string]any {
	return map[string]any{"task": normalizeTask(value)}
}

func normalizeTask(value map[string]any) map[string]any {
	result := cloneParams(value)
	if id, ok := result["id"].(string); ok && strings.TrimSpace(id) != "" {
		result["task_id"] = id
		delete(result, "id")
	}
	return result
}

func eventsResult(value map[string]any) map[string]any {
	return map[string]any{"events": anySlice(value["events"])}
}

func taskListResult(value map[string]any) map[string]any {
	raw := anySlice(value["tasks"])
	items := make([]any, 0, len(raw))
	for _, item := range raw {
		if task, ok := item.(map[string]any); ok {
			items = append(items, normalizeTask(task))
		}
	}
	return map[string]any{"tasks": items, "next_page_token": stringValue(value["next_page_token"])}
}

func scheduleListResult(value map[string]any) map[string]any {
	result := map[string]any{"schedules": anySlice(value["schedules"])}
	cursor := stringValue(value["next_page_token"])
	result["next_page_token"] = cursor
	return result
}

func scheduleDeleteResult(value map[string]any) map[string]any {
	return map[string]any{"deleted": true, "schedule_id": stringValue(value["schedule_id"])}
}

func scheduleTriggerResult(value map[string]any) map[string]any {
	return map[string]any{
		"schedule":      value["schedule"],
		"occurrence_id": stringValue(value["occurrence"].(map[string]any)["occurrence_id"]),
		"task_id":       stringValue(value["task"].(map[string]any)["task_id"]),
	}
}

func installationListResult(value map[string]any) map[string]any {
	raw := anySlice(value["installations"])
	items := make([]any, 0, len(raw))
	for _, item := range raw {
		if installation, ok := item.(map[string]any); ok {
			items = append(items, normalizeInstallation(installation))
		}
	}
	return map[string]any{"installations": items, "next_page_token": stringValue(value["next_page_token"])}
}

func installationMutationResult(value map[string]any) map[string]any {
	result := map[string]any{"installation": normalizeInstallation(value["installation"].(map[string]any))}
	for _, key := range []string{"confirmation_id", "task_id"} {
		if item := value[key]; item != nil {
			result[key] = item
		}
	}
	return result
}

func normalizeInstallation(value map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{"id", "candidate", "kind", "source", "candidate_id", "name", "description", "transport", "revision", "state", "enabled", "active_version_id", "proposed_version_id", "versions", "network_grants", "secret_grants", "created_at", "updated_at"} {
		if value := value[key]; value != nil {
			if key == "versions" {
				result[key] = normalizeExtensionVersions(value)
			} else {
				result[key] = value
			}
		}
	}
	return result
}

func normalizeExtensionVersions(value any) []any {
	rawVersions := anySlice(value)
	versions := make([]any, 0, len(rawVersions))
	for _, raw := range rawVersions {
		version, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		normalized := mapProjection(version, []string{"version_id", "pin", "content_digest", "manifest_digest", "execution_digest", "network_schema_digest", "secret_schema_digest", "execution", "created_at", "network_grants", "secret_grants", "node_artifact"})
		if receipt, ok := normalized["node_artifact"].(map[string]any); ok {
			normalized["node_artifact"] = mapProjection(receipt, []string{"package_name", "package_version", "artifact_bytes", "file_count", "node_version", "npm_version", "lifecycle_scripts_disabled", "native_addons_absent"})
		}
		versions = append(versions, normalized)
	}
	return versions
}

func normalizeSource(value map[string]any) map[string]any {
	return map[string]any{
		"source_id":     stringValue(value["source_id"]),
		"kind":          stringValue(value["kind"]),
		"status":        stringValue(value["status"]),
		"title":         stringValue(value["title"]),
		"relative_path": stringValue(value["relative_path"]),
		"digest":        stringValue(value["digest"]),
		"size":          integerValue(value["size_bytes"]),
		"mime_type":     stringValue(value["media_type"]),
		"revision":      integerValue(value["revision"]),
		"created_at":    stringValue(value["created_at"]),
		"updated_at":    stringValue(value["updated_at"]),
		"error_code":    stringValue(value["error_code"]),
		"content_ref":   stringValue(value["content_ref"]),
		"tags":          anySlice(value["tags"]),
	}
}

func normalizeUpload(value map[string]any, includeReplay bool) map[string]any {
	metadata, _ := value["metadata"].(map[string]any)
	size := integerValue(value["declared_size"])
	if size == 0 {
		size = integerValue(metadata["declared_size"])
	}
	sourceID := stringValue(value["source_id"])
	if sourceID == "" {
		sourceID = stringValue(metadata["source_id"])
	}
	result := map[string]any{
		"upload_id":       stringValue(value["upload_id"]),
		"source_id":       sourceID,
		"status":          stringValue(value["status"]),
		"size":            size,
		"received_size":   integerValue(value["received_size"]),
		"max_chunk_bytes": integerValue(value["max_chunk_bytes"]),
		"progress":        numberValue(value["progress"]),
	}
	if includeReplay {
		if item := value["replayed"]; item != nil {
			result["replayed"] = item
		}
	}
	return result
}

func anySlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case nil:
		return []any{}
	default:
		return []any{}
	}
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
}

func boolValue(value any) bool {
	typed, _ := value.(bool)
	return typed
}

func integerValue(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func numberValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

// translateProductCoreInput converts the current public ProductCore field names
// to the current Agent capability field names before schema digesting.
func translateProductCoreInput(action string, input map[string]any) {
	alias := func(from, to string) {
		if _, exists := input[to]; !exists {
			if value, ok := input[from]; ok {
				input[to] = value
			}
		}
	}
	if action == "agent.knowledge.search" {
		alias("page_size", "limit")
		delete(input, "page_size")
	}
	switch action {
	case "agent.account.deprovision":
		// ProductCore keeps the established public `confirm` field while the
		// neutral Agent capability uses the explicit `confirmation` name.
		alias("confirm", "confirmation")
		delete(input, "confirm")
	case "agent.chat.conversations.get":
		alias("message_limit", "limit")
		alias("message_cursor", "page_token")
	case "agent.knowledge.upload.start":
		alias("filename", "title")
		alias("mime_type", "media_type")
		alias("size", "declared_size")
	case "agent.knowledge.upload.chunk":
		alias("offset", "offset_bytes")
	}
}
