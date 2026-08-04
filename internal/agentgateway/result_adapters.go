package agentgateway

// The Capability API intentionally exposes Agent Core DTOs, while the
// message-server ProductCore surface has a frozen response contract. Keep the
// translation here at the gateway boundary so clients never need to know Core
// field names, pagination cursors, or envelope changes.

import (
	"strings"
)

func adaptActionResult(action string, output map[string]any) map[string]any {
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
	case "agent.core.tasks.get", "agent.core.tasks.cancel", "agent.core.tasks.retry":
		return taskResult(result)
	case "agent.core.tasks.list":
		return taskListResult(result)
	case "agent.core.tasks.events":
		return eventsResult(result)
	case "agent.core.schedules.create", "agent.core.schedules.get", "agent.core.schedules.update", "agent.core.schedules.pause", "agent.core.schedules.resume":
		return objectWrapper(result, "schedule")
	case "agent.core.schedules.delete":
		return scheduleDeleteResult(result, true)
	case "agent.core.schedules.list":
		return scheduleListResult(result, false)
	case "agent.core.schedules.trigger":
		return scheduleTriggerResult(result)
	case "agent.schedules.create", "agent.schedules.get", "agent.schedules.update", "agent.schedules.enable", "agent.schedules.disable":
		return objectWrapper(result, "schedule")
	case "agent.schedules.delete":
		return scheduleDeleteResult(result, false)
	case "agent.schedules.list":
		return scheduleListResult(result, true)
	case "agent.schedule_runs.list":
		return scheduleRunsListResult(result)
	case "agent.schedule_runs.get":
		return objectWrapper(result, "run")
	case "agent.schedules.run_now":
		return scheduleRunNowResult(result)
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
	case "agent.models.list", "agent.core.model_profiles.list", "agent.model_profiles.list":
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
	return map[string]any{
		"turns":       anySlice(valueByKey(value, "turns", "Turns")),
		"next_cursor": stringValue(valueByKey(value, "next_cursor", "next_page_token", "NextCursor")),
	}
}

func modelSyncResult(value map[string]any) map[string]any {
	result := map[string]any{"profiles": normalizeProfiles(valueByKey(value, "profiles", "Profiles"))}
	copyModelDefaults(result, value)
	return result
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
	for _, key := range []string{"embedding_profile_id", "embedding_profile_revision", "embedding_model"} {
		if item := valueByKey(value, key); item != nil {
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
		"embedding_profile_id":       {"embedding_profile_id", "EmbeddingProfileID"},
		"embedding_profile_revision": {"embedding_profile_revision", "EmbeddingProfileRevision"},
		"embedding_model":            {"embedding_model", "EmbeddingModel"},
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
	for _, pair := range [][2]string{{"embedding_profile_id", "EmbeddingProfileID"}, {"dimension", "Dimension"}, {"collection", "Collection"}, {"collection_config_digest", "CollectionConfigDigest"}, {"revision", "Revision"}, {"updated_at", "UpdatedAt"}} {
		if item := valueByKey(value, pair[0], pair[1]); item != nil {
			result[pair[0]] = item
		}
	}
	return result
}

func chatResult(value map[string]any) map[string]any {
	result := map[string]any{}
	if text := stringValue(valueByKey(value, "text", "Text")); text != "" {
		result["text"] = text
	}
	if calls := valueByKey(value, "tool_calls", "ToolCalls"); calls != nil {
		result["tool_calls"] = calls
	}
	if references := valueByKey(value, "references", "References"); references != nil {
		result["references"] = references
	}
	if message, ok := valueByKey(value, "message", "Message").(map[string]any); ok {
		if _, exists := result["text"]; !exists {
			if text := stringValue(valueByKey(message, "content", "Content")); text != "" {
				result["text"] = text
			}
		}
		if _, exists := result["tool_calls"]; !exists {
			if calls := valueByKey(message, "tool_calls", "ToolCalls"); calls != nil {
				result["tool_calls"] = calls
			}
		}
	}
	return result
}

func confirmationResult(value map[string]any) map[string]any {
	if confirmation, ok := valueByKey(value, "confirmation", "Confirmation").(map[string]any); ok {
		return map[string]any{"confirmation": confirmation}
	}
	return map[string]any{"confirmation": value}
}

func confirmationListResult(value map[string]any) map[string]any {
	return map[string]any{
		"confirmations":   anySlice(valueByKey(value, "confirmations", "Confirmations")),
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

func scheduleListResult(value map[string]any, legacy bool) map[string]any {
	result := map[string]any{"schedules": anySlice(valueByKey(value, "schedules", "Schedules"))}
	cursor := stringValue(valueByKey(value, "next_cursor", "next_page_token", "NextPageToken"))
	if legacy {
		result["next_cursor"] = cursor
	} else {
		result["next_page_token"] = cursor
	}
	return result
}

func scheduleDeleteResult(value map[string]any, core bool) map[string]any {
	if core {
		return map[string]any{"deleted": true, "schedule_id": stringValue(valueByKey(value, "schedule_id", "id", "ID"))}
	}
	result := map[string]any{"deleted": boolValue(valueByKey(value, "deleted", "Deleted"))}
	if schedule, ok := valueByKey(value, "schedule", "Schedule").(map[string]any); ok {
		result["schedule"] = schedule
	} else {
		result["schedule"] = value
	}
	return result
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

func scheduleRunNowResult(value map[string]any) map[string]any {
	if run, ok := valueByKey(value, "run", "occurrence", "Occurrence").(map[string]any); ok {
		return map[string]any{"run": run}
	}
	return map[string]any{"run": value}
}

func scheduleRunsListResult(value map[string]any) map[string]any {
	return map[string]any{"runs": anySlice(valueByKey(value, "runs", "Runs")), "next_cursor": stringValue(valueByKey(value, "next_cursor", "next_page_token", "NextPageToken"))}
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
			result[key] = value
		}
	}
	return result
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
		aliases := map[string][]string{
			"embedding_indexed":          {"embedding_indexed", "EmbeddingIndexed"},
			"embedding_profile_id":       {"embedding_profile_id", "EmbeddingProfileID"},
			"embedding_profile_revision": {"embedding_profile_revision", "EmbeddingProfileRevision"},
			"embedding_model":            {"embedding_model", "EmbeddingModel"},
		}
		for key, names := range aliases {
			if item := valueByKey(value, names...); item != nil {
				result[key] = item
			}
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
	for _, name := range []string{"agent.chat.conversations.list", "agent.core.model_profiles.list", "agent.model_profiles.list", "agent.knowledge.sources.list", "agent.knowledge.memories.list", "agent.knowledge.search", "agent.core.tasks.list", "agent.core.schedules.list", "agent.schedules.list", "agent.schedule_runs.list"} {
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
	case "agent.schedules.create", "agent.schedules.update":
		if _, ok := input["task_template"]; !ok {
			template := map[string]any{}
			if value, exists := input["prompt"]; exists {
				template["goal"] = value
			}
			if value, exists := input["model_profile_id"]; exists {
				template["model_profile_id"] = value
			}
			if len(template) > 0 {
				input["task_template"] = template
			}
		}
	}
	if action == "agent.schedules.create" || action == "agent.schedules.update" || action == "agent.core.schedules.create" || action == "agent.core.schedules.update" {
		if spec, ok := input["task_template"]; ok {
			input["spec"] = spec
			delete(input, "task_template")
		}
		if trigger, ok := input["trigger"].(map[string]any); ok {
			for _, key := range []string{"run_at", "cron", "timezone"} {
				if value, exists := trigger[key]; exists {
					input[key] = value
				}
			}
			delete(input, "trigger")
		}
	}
}
