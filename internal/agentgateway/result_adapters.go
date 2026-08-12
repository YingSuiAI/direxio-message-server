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
		return objectWrapper(result, "attachment")
	case "agent.core.tasks.get", "agent.core.tasks.cancel", "agent.core.tasks.retry":
		return taskResult(result)
	case "agent.core.tasks.list":
		return taskListResult(result)
	case "agent.core.tasks.events":
		return eventsResult(result)
	case "agent.core.schedules.create", "agent.core.schedules.get", "agent.core.schedules.update", "agent.core.schedules.pause", "agent.core.schedules.resume":
		return objectWrapper(result, "schedule")
	case "agent.core.schedules.delete":
		return scheduleDeleteResult(result)
	case "agent.core.schedules.list":
		return scheduleListResult(result)
	case "agent.core.schedules.trigger":
		return scheduleTriggerResult(result)
	case "agent.core.mcp.list", "agent.core.skills.list", "agent.mcp.servers.list", "agent.skills.list":
		return installationListResult(result)
	case "agent.core.mcp.get", "agent.core.skills.get":
		return objectWrapper(result, "installation")
	case "agent.core.mcp.install", "agent.core.mcp.update", "agent.core.mcp.remove", "agent.core.skills.install", "agent.core.skills.update", "agent.core.skills.remove":
		return installationMutationResult(result)
	case "agent.core.mcp.enable", "agent.core.mcp.disable", "agent.core.skills.enable", "agent.core.skills.disable", "agent.mcp.servers.enable", "agent.mcp.servers.disable", "agent.skills.enable", "agent.skills.disable":
		return objectWrapper(result, "installation")
	case "agent.core.model_profiles.sync", "agent.model_profiles.sync":
		return modelSyncResult(result)
	case "agent.models.list":
		return modelCatalogResult(result)
	case "agent.core.model_profiles.list", "agent.model_profiles.list":
		return modelListResult(result)
	case "agent.core.model_profiles.get", "agent.model_profiles.get":
		return modelGetResult(result)
	case "agent.core.model_profiles.delete", "agent.model_profiles.delete":
		return modelDeleteResult(result)
	case "agent.knowledge.sources.list":
		return sourceListResult(result)
	case "agent.knowledge.sources.get":
		return sourceGetResult(result)
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
	case "agent.knowledge.memory.create":
		return memoryResult(result, "create")
	case "agent.knowledge.memories.list":
		return memoryListResult(result)
	case "agent.knowledge.memories.get":
		return memoryResult(result, "get")
	case "agent.knowledge.memories.update", "agent.knowledge.memories.delete":
		return memoryResult(result, "mutation")
	case "agent.knowledge.search":
		return knowledgeSearchResult(result)
	case "agent.knowledge.memory.search":
		return knowledgeSearchResult(result)
	case "agent.knowledge.status":
		return knowledgeStatusResult(result)
	case "agent.chat":
		return chatResult(result)
	case "agent.web_search.config.get", "agent.web_search.config.update":
		return webSearchConfigResult(result)
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
		return objectWrapper(result, "credential")
	case "agent.core.aws.credentials.delete":
		return mapProjection(result, []string{"deleted", "credential_id"}, map[string][]string{"credential_id": {"credential_id", "CredentialID"}})
	case "agent.core.aws.credentials.list":
		return mapProjection(result, []string{"credentials", "next_page_token"}, map[string][]string{"credentials": {"credentials", "Credentials"}, "next_page_token": {"next_page_token", "next_cursor", "NextPageToken"}})
	case "agent.core.aws.credentials.test":
		return mapProjection(result, []string{"credential_id", "account_id", "user_arn", "principal_id", "credential_revision", "tested_at"}, nil)
	case "agent.core.mcp.discover", "agent.core.skills.discover":
		return mapProjection(result, []string{"candidates", "next_page_token"}, nil)
	case "agent.core.mcp.inspect", "agent.core.skills.inspect":
		return objectWrapper(result, "inspection")
	case "agent.core.mcp.list_tools":
		return mapProjection(result, []string{"tools"}, nil)
	case "agent.core.mcp.execute", "agent.core.skills.execute":
		return mapProjection(result, []string{"confirmation_id", "task_id"}, nil)
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
	case "agent.core.model_profiles.sync", "agent.model_profiles.sync", "agent.core.model_profiles.list", "agent.model_profiles.list":
		return validateToolModelDefaultResult(output)
	case "agent.chat.turn.stop":
		return validateTurnStopResult(request, output)
	case "agent.chat.turn.steer":
		return validateTurnSteerResult(request, output)
	case "agent.chat.turns.list":
		return validateTurnsListResult(output)
	case "agent.core.mcp.get", "agent.core.mcp.list", "agent.core.mcp.install", "agent.core.mcp.update", "agent.core.mcp.remove":
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
		installation := output
		if wrapped, ok := output["installation"].(map[string]any); ok {
			installation = wrapped
		}
		installations = append(installations, installation)
	}
	for _, installation := range installations {
		if err := validateCoreMCPInstallationNodeReceipts(installation); err != nil {
			return err
		}
	}
	return nil
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

func validateKnowledgeStatusResult(output map[string]any) error {
	if output == nil {
		return fmt.Errorf("%w: knowledge status response is missing", ErrInvalidActionResult)
	}
	values := make(map[string]int64, 4)
	for _, field := range []struct {
		canonical string
		core      string
	}{
		{canonical: "quota_used_bytes", core: "QuotaUsedBytes"},
		{canonical: "quota_limit_bytes", core: "QuotaLimitBytes"},
		{canonical: "quota_remaining_bytes", core: "QuotaRemainingBytes"},
		{canonical: "max_source_bytes", core: "MaxSourceBytes"},
	} {
		value := valueByKey(output, field.canonical, field.core)
		if value == nil {
			return fmt.Errorf("%w: knowledge status %s is required", ErrInvalidActionResult, field.canonical)
		}
		integer, ok := turnInt64(value)
		if !ok || integer < 0 {
			return fmt.Errorf("%w: knowledge status %s must be a non-negative integer", ErrInvalidActionResult, field.canonical)
		}
		values[field.canonical] = integer
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
	modelsValue := valueByKey(output, "models", "Models")
	models, ok := modelsValue.([]any)
	if !ok {
		return fmt.Errorf("%w: model catalog models must be an array", ErrInvalidActionResult)
	}
	providersValue := valueByKey(output, "providers", "Providers")
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
			"id":                {"id", "ID", "model_id", "ModelID"},
			"name":              {"name", "Name"},
			"provider":          {"provider", "Provider"},
			"context_length":    {"context_length", "ContextLength"},
			"context_window":    {"context_window", "ContextWindow"},
			"max_output_tokens": {"max_output_tokens", "MaxOutputTokens"},
			"input_modalities":  {"input_modalities", "InputModalities"},
			"output_modalities": {"output_modalities", "OutputModalities"},
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
			"provider":         {"provider", "Provider"},
			"default_base_url": {"default_base_url", "DefaultBaseURL"},
			"requires_api_key": {"requires_api_key", "RequiresAPIKey"},
			"dynamic_models":   {"dynamic_models", "DynamicModels"},
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
	aliases := []string{field, strings.Title(strings.ReplaceAll(field, "_", ""))}
	for actual, value := range output {
		for _, alias := range aliases {
			if strings.EqualFold(actual, alias) {
				return value, true
			}
		}
	}
	return nil, false
}

func requireWebSearchBool(output map[string]any, field string) error {
	value := valueByKey(output, field, strings.Title(strings.ReplaceAll(field, "_", "")))
	if value == nil {
		return fmt.Errorf("%w: web search %s is required", ErrInvalidActionResult, field)
	}
	if _, ok := value.(bool); !ok {
		return fmt.Errorf("%w: web search %s must be a boolean", ErrInvalidActionResult, field)
	}
	return nil
}

func requireWebSearchProvider(output map[string]any) error {
	value := valueByKey(output, "provider", "Provider")
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
	value := valueByKey(output, field, strings.Title(strings.ReplaceAll(field, "_", "")))
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
	if conversation, wrapped := value["conversation"]; wrapped {
		return map[string]any{"conversation": conversation, "replayed": boolValue(value["replayed"])}
	}
	replayed := boolValue(value["replayed"])
	delete(value, "replayed")
	return map[string]any{"conversation": value, "replayed": replayed}
}

func conversationDeleteResult(value map[string]any) map[string]any {
	if conversation, wrapped := value["conversation"]; wrapped {
		// The frozen ProductCore response is intentionally narrower than the
		// Core mutation receipt. Do not leak Core-only deleted/receipt fields.
		return map[string]any{"conversation": conversation, "replayed": boolValue(value["replayed"])}
	}
	// Core v1 may return only {deleted:true} until its mutation receipt is
	// materialized. Keep the legacy envelope shape stable; an empty snapshot
	// is preferable to exposing a Core-only deleted field.
	conversation := map[string]any{}
	if raw, ok := valueByKey(value, "profile", "Profile").(map[string]any); ok {
		conversation = raw
	}
	return map[string]any{"conversation": conversation, "replayed": boolValue(value["replayed"])}
}

func conversationGetResult(value map[string]any) map[string]any {
	if conversation, wrapped := value["conversation"]; wrapped {
		return map[string]any{
			"conversation": conversation,
			"messages":     anySlice(valueByKey(value, "messages", "Messages")),
			"next_cursor":  stringValue(valueByKey(value, "next_cursor", "next_page_token", "NextCursor")),
		}
	}
	conversation := cloneParams(value)
	messages := anySlice(valueByKey(value, "messages", "Messages"))
	delete(conversation, "messages")
	delete(conversation, "Messages")
	return map[string]any{"conversation": conversation, "messages": messages, "next_cursor": stringValue(valueByKey(value, "next_cursor", "next_page_token", "NextCursor"))}
}

func conversationListResult(value map[string]any) map[string]any {
	conversations := anySlice(valueByKey(value, "conversations", "Conversations"))
	return map[string]any{"conversations": conversations, "next_cursor": stringValue(valueByKey(value, "next_cursor", "next_page_token", "NextCursor"))}
}

func renameNextCursor(value map[string]any) map[string]any {
	if _, ok := value["next_cursor"]; !ok {
		if cursor := valueByKey(value, "next_page_token", "NextCursor"); cursor != nil {
			value["next_cursor"] = cursor
		}
	}
	return value
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
	result := map[string]any{"profiles": normalizeProfiles(valueByKey(value, "profiles", "Profiles"))}
	copyModelDefaults(result, value)
	return result
}

func modelCatalogResult(value map[string]any) map[string]any {
	models := normalizeModelCatalogEntries(valueByKey(value, "models", "Models"), map[string][]string{
		"id":                {"id", "ID", "model_id", "ModelID"},
		"name":              {"name", "Name"},
		"provider":          {"provider", "Provider"},
		"context_length":    {"context_length", "ContextLength"},
		"context_window":    {"context_window", "ContextWindow"},
		"max_output_tokens": {"max_output_tokens", "MaxOutputTokens"},
		"input_modalities":  {"input_modalities", "InputModalities"},
		"output_modalities": {"output_modalities", "OutputModalities"},
	}, validModelCatalogModel)
	providers := normalizeModelCatalogEntries(valueByKey(value, "providers", "Providers"), map[string][]string{
		"provider":         {"provider", "Provider"},
		"default_base_url": {"default_base_url", "DefaultBaseURL"},
		"requires_api_key": {"requires_api_key", "RequiresAPIKey"},
		"dynamic_models":   {"dynamic_models", "DynamicModels"},
	}, validModelCatalogProvider)
	return map[string]any{"models": models, "providers": providers}
}

func normalizeModelCatalogEntries(value any, aliases map[string][]string, valid func(map[string]any) bool) []any {
	raw := anySlice(value)
	result := make([]any, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		normalized := make(map[string]any, len(aliases))
		for canonical, names := range aliases {
			if field := valueByKey(entry, names...); field != nil {
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
		"profiles":        normalizeProfiles(valueByKey(value, "profiles", "Profiles")),
		"next_page_token": stringValue(valueByKey(value, "next_page_token", "next_cursor", "NextCursor")),
	}
	copyModelDefaults(result, value)
	return result
}

func modelGetResult(value map[string]any) map[string]any {
	if profile, ok := valueByKey(value, "profile", "Profile").(map[string]any); ok {
		return map[string]any{"profile": normalizeProfile(profile)}
	}
	return map[string]any{"profile": normalizeProfile(value)}
}

func modelDeleteResult(value map[string]any) map[string]any {
	profileID := stringValue(valueByKey(value, "profile_id", "id", "ID"))
	if profile, ok := valueByKey(value, "profile", "Profile").(map[string]any); ok && profileID == "" {
		profileID = stringValue(valueByKey(profile, "profile_id", "id", "ID"))
	}
	return map[string]any{"deleted": boolValueOrDefault(valueByKey(value, "deleted", "Deleted"), true), "profile_id": profileID}
}

func copyModelDefaults(result, value map[string]any) {
	aliases := map[string][]string{
		"default_client_profile_id":              {"default_client_profile_id", "DefaultClientProfileID"},
		"default_conversation_client_profile_id": {"default_conversation_client_profile_id", "default_conversation_profile_id", "DefaultConversationProfileID"},
		"default_tool_client_profile_id":         {"default_tool_client_profile_id"},
		"default_embedding_client_profile_id":    {"default_embedding_client_profile_id", "default_embedding_profile_id", "DefaultEmbeddingProfileID"},
		"default_speech_client_profile_id":       {"default_speech_client_profile_id", "default_speech_profile_id", "DefaultSpeechProfileID"},
	}
	for key, names := range aliases {
		if item := valueByKey(value, names...); item != nil {
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
		aliases := []string{key}
		switch key {
		case "profile_id":
			aliases = append(aliases, "id", "ID")
		case "api_key_configured":
			aliases = append(aliases, "APIKeyConfigured")
		case "client_profile_id":
			aliases = append(aliases, "ClientProfileID")
		}
		if item := valueByKey(value, aliases...); item != nil {
			result[key] = item
		}
	}
	return result
}

func sourceListResult(value map[string]any) map[string]any {
	raw := anySlice(valueByKey(value, "sources", "Sources"))
	sources := make([]any, 0, len(raw))
	for _, item := range raw {
		if source, ok := item.(map[string]any); ok {
			sources = append(sources, normalizeSource(source))
		}
	}
	return map[string]any{"sources": sources, "next_page_token": stringValue(valueByKey(value, "next_page_token", "NextPageToken"))}
}

func sourceGetResult(value map[string]any) map[string]any {
	if source, ok := valueByKey(value, "source", "Source").(map[string]any); ok {
		return map[string]any{"source": normalizeSource(source)}
	}
	return map[string]any{"source": normalizeSource(value)}
}

func sourceDeleteResult(value map[string]any) map[string]any {
	replayed := boolValue(valueByKey(value, "replayed", "Replay"))
	if source, ok := valueByKey(value, "source", "Source").(map[string]any); ok {
		return map[string]any{"source": normalizeSource(source), "replayed": replayed}
	}
	return map[string]any{"source": normalizeSource(value), "replayed": replayed}
}

func uploadResult(value map[string]any, includeReplay bool) map[string]any {
	return normalizeUpload(value, includeReplay)
}

func uploadFinishResult(value map[string]any) map[string]any {
	if source, ok := valueByKey(value, "source", "Source").(map[string]any); ok {
		return map[string]any{"source": normalizeSource(source)}
	}
	return map[string]any{"source": normalizeSource(value)}
}

func memoryResult(value map[string]any, mode string) map[string]any {
	if source, ok := valueByKey(value, "source", "Source").(map[string]any); ok {
		return normalizeMemory(source, mode)
	}
	return normalizeMemory(value, mode)
}

func memoryListResult(value map[string]any) map[string]any {
	raw := anySlice(valueByKey(value, "items", "Items", "sources", "Sources"))
	items := make([]any, 0, len(raw))
	for _, item := range raw {
		if memory, ok := item.(map[string]any); ok {
			items = append(items, normalizeMemory(memory, "list"))
		}
	}
	return map[string]any{"items": items, "next_page_token": stringValue(valueByKey(value, "next_page_token", "NextPageToken"))}
}

func knowledgeSearchResult(value map[string]any) map[string]any {
	raw := anySlice(valueByKey(value, "items", "Items", "matches", "Matches"))
	items := make([]any, 0, len(raw))
	for _, item := range raw {
		if match, ok := item.(map[string]any); ok {
			items = append(items, map[string]any{
				"source_id": stringValue(valueByKey(match, "source_id", "SourceID")),
				"chunk_ref": stringValue(valueByKey(match, "chunk_ref", "ChunkRef")),
				"snippet":   stringValue(valueByKey(match, "snippet", "Snippet")),
				"score":     numberValue(valueByKey(match, "score", "Score")),
			})
		}
	}
	result := map[string]any{"items": items, "next_cursor": stringValue(valueByKey(value, "next_cursor", "next_page_token", "NextPageToken"))}
	if mode := stringValue(valueByKey(value, "search_mode", "SearchMode")); mode != "" {
		result["search_mode"] = mode
	}
	aliases := map[string][]string{
		"embedding_profile_id":       {"embedding_profile_id", "EmbeddingProfileID"},
		"embedding_profile_revision": {"embedding_profile_revision", "EmbeddingProfileRevision"},
		"embedding_model":            {"embedding_model", "EmbeddingModel"},
		"embedding_generation":       {"embedding_generation", "EmbeddingGeneration"},
		"collection_config_digest":   {"collection_config_digest", "CollectionConfigDigest"},
	}
	for key, names := range aliases {
		if item := valueByKey(value, names...); item != nil {
			result[key] = item
		}
	}
	return result
}

func knowledgeStatusResult(value map[string]any) map[string]any {
	result := map[string]any{}
	aliases := map[string][]string{
		"supported":                  {"supported", "Supported"},
		"count":                      {"count", "Count", "total", "Total"},
		"embedding_indexed":          {"embedding_indexed", "EmbeddingIndexed", "indexed", "Indexed"},
		"embedding_stale":            {"embedding_stale", "EmbeddingStale", "stale", "Stale"},
		"ready_count":                {"ready_count", "ReadyCount"},
		"uploading_count":            {"uploading_count", "UploadingCount"},
		"indexing_count":             {"indexing_count", "IndexingCount"},
		"failed_count":               {"failed_count", "FailedCount"},
		"cleanup_pending_count":      {"cleanup_pending_count", "CleanupPendingCount"},
		"checked_at":                 {"checked_at", "CheckedAt"},
		"embedding_profile_id":       {"embedding_profile_id", "EmbeddingProfileID"},
		"embedding_profile_revision": {"embedding_profile_revision", "EmbeddingProfileRevision"},
		"embedding_model":            {"embedding_model", "EmbeddingModel"},
		"quota_used_bytes":           {"quota_used_bytes", "QuotaUsedBytes"},
		"quota_limit_bytes":          {"quota_limit_bytes", "QuotaLimitBytes"},
		"quota_remaining_bytes":      {"quota_remaining_bytes", "QuotaRemainingBytes"},
		"max_source_bytes":           {"max_source_bytes", "MaxSourceBytes"},
	}
	for key, names := range aliases {
		if item := valueByKey(value, names...); item != nil {
			result[key] = item
		}
	}
	return result
}

func embeddingConfigResult(value map[string]any) map[string]any {
	result := map[string]any{}
	for _, pair := range [][2]string{{"embedding_profile_id", "EmbeddingProfileID"}, {"embedding_profile_revision", "EmbeddingProfileRevision"}, {"embedding_model", "EmbeddingModel"}, {"dimension", "Dimension"}, {"collection", "Collection"}, {"collection_config_digest", "CollectionConfigDigest"}, {"revision", "Revision"}, {"updated_at", "UpdatedAt"}} {
		if item := valueByKey(value, pair[0], pair[1]); item != nil {
			result[pair[0]] = item
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
	"room_id": {}, "room_type": {}, "channel_id": {}, "post_id": {}, "title": {},
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
		"execution_digest", "risk_level", "gate_type", "binding_id", "project_id",
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
		!validPositiveReferenceInteger(reference["plan_revision"]) || !validReferenceDigest(reference["plan_digest"]) ||
		!canonicalTurnUUID(reference["run_id"]) || !validPositiveReferenceInteger(reference["run_revision"]) ||
		!validReferenceDigest(reference["run_digest"]) || !canonicalTurnUUID(reference["execution_id"]) ||
		reference["run_id"] != reference["execution_id"] || !canonicalTurnUUID(reference["confirmation_id"]) ||
		!validPositiveReferenceInteger(reference["confirmation_revision"]) || !validReferenceDigest(reference["binding_digest"]) ||
		!validReferenceDigest(reference["quote_digest"]) || !validReferenceDigest(reference["execution_digest"]) ||
		!noConversationReferenceFields(reference) {
		return false
	}
	for _, field := range []string{
		"deployment_id", "stage_id", "stage_revision", "stage_digest", "target_id",
		"target_revision", "target_digest", "preview_digest", "risk_level", "gate_type",
		"binding_id", "binding_revision", "project_id",
	} {
		if !zeroReferenceValue(reference, field) {
			return false
		}
	}
	if kind == "execution_confirmation" {
		return zeroReferenceString(reference, "status") && validConfirmationReferenceState(reference["state"])
	}
	return zeroReferenceString(reference, "state") && validExecutionReferenceStatus(reference["status"])
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
		"account_generation", "task_id", "execution_id", "quote_digest", "execution_digest",
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
		"binding_revision", "project_id", "status", "state")
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
		map[string][]string{
			"enabled":            {"enabled", "Enabled"},
			"provider":           {"provider", "Provider"},
			"api_key_configured": {"api_key_configured", "APIKeyConfigured"},
			"revision":           {"revision", "Revision"},
			"tested_at":          {"tested_at", "TestedAt"},
			"updated_at":         {"updated_at", "UpdatedAt"},
		})
	if boolValue(valueByKey(value, "api_key_configured", "APIKeyConfigured")) {
		result["api_key_hint"] = "configured"
	}
	return result
}

func webSearchTestResult(value map[string]any) map[string]any {
	return mapProjection(value,
		[]string{"ok", "provider", "result_count", "tested_at", "enabled", "api_key_configured", "revision"},
		map[string][]string{
			"ok":                 {"ok", "OK"},
			"provider":           {"provider", "Provider"},
			"result_count":       {"result_count", "ResultCount"},
			"tested_at":          {"tested_at", "TestedAt"},
			"enabled":            {"enabled", "Enabled"},
			"api_key_configured": {"api_key_configured", "APIKeyConfigured"},
			"revision":           {"revision", "Revision"},
		})
}

func confirmationResult(value map[string]any) map[string]any {
	if confirmation, ok := valueByKey(value, "confirmation", "Confirmation").(map[string]any); ok {
		return map[string]any{"confirmation": projectCloudWorkerConfirmation(confirmation)}
	}
	return map[string]any{"confirmation": projectCloudWorkerConfirmation(value)}
}

func confirmationListResult(value map[string]any) map[string]any {
	confirmations := anySlice(valueByKey(value, "confirmations", "Confirmations"))
	for index, item := range confirmations {
		if confirmation, ok := item.(map[string]any); ok {
			confirmations[index] = projectCloudWorkerConfirmation(confirmation)
		}
	}
	return map[string]any{
		"confirmations":   confirmations,
		"next_page_token": stringValue(valueByKey(value, "next_page_token", "next_cursor", "NextPageToken")),
	}
}

func confirmationAcknowledgeResult(value map[string]any) map[string]any {
	return mapProjection(value, []string{"confirmation", "task", "resolution", "reservation_released"}, nil)
}

func mapProjection(value map[string]any, keys []string, aliases map[string][]string) map[string]any {
	result := make(map[string]any, len(keys))
	for _, key := range keys {
		names := []string{key}
		if aliases != nil {
			if configured, ok := aliases[key]; ok {
				names = configured
			}
		}
		if item := valueByKey(value, names...); item != nil {
			result[key] = item
		}
	}
	return result
}

func objectWrapper(value map[string]any, key string) map[string]any {
	if _, ok := value[key]; ok {
		return value
	}
	return map[string]any{key: value}
}

func taskResult(value map[string]any) map[string]any {
	if task, ok := valueByKey(value, "task", "Task").(map[string]any); ok {
		return map[string]any{"task": normalizeTask(task)}
	}
	return map[string]any{"task": normalizeTask(value)}
}

func normalizeTask(value map[string]any) map[string]any {
	result := cloneParams(value)
	for _, pair := range [][2]string{{"task_id", "ID"}, {"status", "State"}, {"revision", "Revision"}, {"attempt", "Attempt"}, {"lease_epoch", "LeaseEpoch"}, {"failure_code", "FailureCode"}} {
		if _, exists := result[pair[0]]; !exists {
			if item := valueByKey(value, pair[0], pair[1]); item != nil {
				result[pair[0]] = item
			}
		}
	}
	return result
}

func eventsResult(value map[string]any) map[string]any {
	return map[string]any{"events": anySlice(valueByKey(value, "events", "Events"))}
}

func taskListResult(value map[string]any) map[string]any {
	raw := anySlice(valueByKey(value, "tasks", "Tasks"))
	items := make([]any, 0, len(raw))
	for _, item := range raw {
		if task, ok := item.(map[string]any); ok {
			items = append(items, normalizeTask(task))
		}
	}
	return map[string]any{"tasks": items, "next_page_token": stringValue(valueByKey(value, "next_page_token", "NextPageToken"))}
}

func scheduleListResult(value map[string]any) map[string]any {
	result := map[string]any{"schedules": anySlice(valueByKey(value, "schedules", "Schedules"))}
	cursor := stringValue(valueByKey(value, "next_cursor", "next_page_token", "NextPageToken"))
	result["next_page_token"] = cursor
	return result
}

func scheduleDeleteResult(value map[string]any) map[string]any {
	return map[string]any{"deleted": true, "schedule_id": stringValue(valueByKey(value, "schedule_id", "id", "ID"))}
}

func scheduleTriggerResult(value map[string]any) map[string]any {
	result := map[string]any{}
	if schedule, ok := valueByKey(value, "schedule", "Schedule").(map[string]any); ok {
		result["schedule"] = schedule
	}
	if occurrence, ok := valueByKey(value, "occurrence", "Occurrence").(map[string]any); ok {
		result["occurrence_id"] = stringValue(valueByKey(occurrence, "id", "ID"))
		if result["occurrence_id"] == "" {
			result["occurrence_id"] = stringValue(valueByKey(occurrence, "occurrence_id", "OccurrenceID"))
		}
	}
	if task, ok := valueByKey(value, "task", "Task").(map[string]any); ok {
		result["task_id"] = stringValue(valueByKey(task, "id", "ID", "task_id", "TaskID"))
	}
	for _, key := range []string{"occurrence_id", "task_id"} {
		if value := stringValue(valueByKey(value, key)); value != "" {
			result[key] = value
		}
	}
	return result
}

func installationListResult(value map[string]any) map[string]any {
	raw := anySlice(valueByKey(value, "installations", "Installations"))
	items := make([]any, 0, len(raw))
	for _, item := range raw {
		if installation, ok := item.(map[string]any); ok {
			items = append(items, normalizeInstallation(installation))
		}
	}
	return map[string]any{"installations": items, "next_page_token": stringValue(valueByKey(value, "next_page_token", "NextPageToken"))}
}

func installationMutationResult(value map[string]any) map[string]any {
	result := map[string]any{}
	if installation, ok := valueByKey(value, "installation", "Installation").(map[string]any); ok {
		result["installation"] = normalizeInstallation(installation)
	} else {
		result["installation"] = normalizeInstallation(value)
	}
	for _, key := range []string{"confirmation_id", "task_id"} {
		if item := valueByKey(value, key, strings.ToUpper(key[:1])+key[1:]); item != nil {
			result[key] = item
		}
	}
	return result
}

func normalizeInstallation(value map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range []string{"id", "candidate", "kind", "source", "candidate_id", "name", "description", "transport", "revision", "state", "enabled", "active_version_id", "proposed_version_id", "versions", "network_grants", "secret_grants", "created_at", "updated_at"} {
		aliases := []string{key}
		if len(key) > 0 {
			aliases = append(aliases, strings.ToUpper(key[:1])+key[1:])
		}
		if value := valueByKey(value, aliases...); value != nil {
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
		normalized := mapProjection(version, []string{"version_id", "pin", "content_digest", "manifest_digest", "execution_digest", "network_schema_digest", "secret_schema_digest", "execution", "created_at", "network_grants", "secret_grants", "node_artifact"}, nil)
		if receipt, ok := normalized["node_artifact"].(map[string]any); ok {
			normalized["node_artifact"] = mapProjection(receipt, []string{"package_name", "package_version", "artifact_bytes", "file_count", "node_version", "npm_version", "lifecycle_scripts_disabled", "native_addons_absent"}, nil)
		}
		versions = append(versions, normalized)
	}
	return versions
}

func normalizeSource(value map[string]any) map[string]any {
	return map[string]any{
		"source_id":     stringValue(valueByKey(value, "source_id", "id", "ID")),
		"kind":          stringValue(valueByKey(value, "kind", "Kind")),
		"status":        stringValue(valueByKey(value, "status", "Status")),
		"title":         stringValue(valueByKey(value, "title", "Title")),
		"relative_path": stringValue(valueByKey(value, "relative_path", "RelativePath")),
		"digest":        stringValue(valueByKey(value, "digest", "Digest")),
		"size":          integerValue(valueByKey(value, "size", "size_bytes", "SizeBytes")),
		"mime_type":     stringValue(valueByKey(value, "mime_type", "media_type", "MediaType")),
		"revision":      integerValue(valueByKey(value, "revision", "Revision")),
		"created_at":    stringValue(valueByKey(value, "created_at", "CreatedAt")),
		"updated_at":    stringValue(valueByKey(value, "updated_at", "UpdatedAt")),
		"error_code":    stringValue(valueByKey(value, "error_code", "ErrorCode")),
		"content_ref":   stringValue(valueByKey(value, "content_ref", "ContentRef")),
		"tags":          anySlice(valueByKey(value, "tags", "Tags")),
	}
}

func normalizeMemory(value map[string]any, mode string) map[string]any {
	result := map[string]any{
		"memory_id": stringValue(valueByKey(value, "memory_id", "source_id", "id", "ID")),
		"title":     stringValue(valueByKey(value, "title", "Title")),
		"content":   stringValue(valueByKey(value, "content", "Content")),
		"tags":      anySlice(valueByKey(value, "tags", "Tags")),
	}
	if mode == "get" || mode == "mutation" || mode == "list" {
		for _, key := range []string{"revision", "created_at", "updated_at"} {
			if item := valueByKey(value, key, strings.ToUpper(key[:1])+key[1:]); item != nil {
				result[key] = item
			}
		}
		if mode == "mutation" {
			if item := valueByKey(value, "replayed", "Replay"); item != nil {
				result["replayed"] = item
			}
		}
	}
	if mode == "create" {
		if item := valueByKey(value, "created_at", "CreatedAt"); item != nil {
			result["created_at"] = item
		}
		if item := valueByKey(value, "replayed", "Replay"); item != nil {
			result["replayed"] = item
		}
	}
	aliases := map[string][]string{
		"embedding_indexed":          {"embedding_indexed", "EmbeddingIndexed"},
		"embedding_stale":            {"embedding_stale", "EmbeddingStale"},
		"embedding_status":           {"embedding_status", "EmbeddingStatus"},
		"embedding_profile_id":       {"embedding_profile_id", "EmbeddingProfileID"},
		"embedding_profile_revision": {"embedding_profile_revision", "EmbeddingProfileRevision"},
		"embedding_model":            {"embedding_model", "EmbeddingModel"},
		"error_code":                 {"error_code", "ErrorCode"},
	}
	for key, names := range aliases {
		if item := valueByKey(value, names...); item != nil {
			result[key] = item
		}
	}
	return result
}

func normalizeUpload(value map[string]any, includeReplay bool) map[string]any {
	metadata, _ := valueByKey(value, "metadata", "Metadata").(map[string]any)
	size := integerValue(valueByKey(value, "size", "declared_size", "DeclaredSize"))
	if size == 0 {
		size = integerValue(valueByKey(metadata, "size", "declared_size", "DeclaredSize"))
	}
	sourceID := stringValue(valueByKey(value, "source_id", "SourceID"))
	if sourceID == "" {
		sourceID = stringValue(valueByKey(metadata, "source_id", "SourceID"))
	}
	result := map[string]any{
		"upload_id":       stringValue(valueByKey(value, "upload_id", "ID", "UploadID")),
		"source_id":       sourceID,
		"status":          stringValue(valueByKey(value, "status", "Status")),
		"size":            size,
		"received_size":   integerValue(valueByKey(value, "received_size", "ReceivedSize")),
		"max_chunk_bytes": integerValue(valueByKey(value, "max_chunk_bytes", "MaxChunkBytes")),
		"progress":        numberValue(valueByKey(value, "progress", "Progress")),
	}
	if includeReplay {
		if item := valueByKey(value, "replayed", "Replay"); item != nil {
			result["replayed"] = item
		}
	}
	return result
}

func valueByKey(value map[string]any, keys ...string) any {
	for _, key := range keys {
		if item, ok := value[key]; ok {
			return item
		}
	}
	for actual, item := range value {
		for _, key := range keys {
			if strings.EqualFold(actual, key) {
				return item
			}
		}
	}
	return nil
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

func boolValueOrDefault(value any, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return boolValue(value)
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

// applyLegacyInputAliases converts the public ProductCore names to the Core
// capability request names before schema digesting. This is deliberately
// performed in the gateway, not in the client, so old Flutter builds and new
// clients share one canonical request digest.
func applyLegacyInputAliases(action string, input map[string]any) {
	alias := func(from, to string) {
		if _, exists := input[to]; !exists {
			if value, ok := input[from]; ok {
				input[to] = value
			}
		}
	}
	for _, name := range []string{"agent.chat.conversations.list", "agent.core.model_profiles.list", "agent.model_profiles.list", "agent.knowledge.sources.list", "agent.knowledge.memories.list", "agent.knowledge.search", "agent.core.tasks.list"} {
		if action == name {
			alias("page_size", "limit")
		}
	}
	switch action {
	case "agent.account.deprovision":
		// ProductCore keeps the established public `confirm` field while the
		// neutral Agent capability uses the explicit `confirmation` name.
		// Remove the legacy spelling because the Agent schema is closed.
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
