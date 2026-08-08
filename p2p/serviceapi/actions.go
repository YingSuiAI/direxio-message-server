package serviceapi

import (
	"fmt"
	"strings"
)

const RealtimeWSTicketAction = "realtime.ws_ticket.create"

type ActionAuth string

const (
	ActionAuthPublic ActionAuth = "public"
	ActionAuthOwner  ActionAuth = "owner"
	ActionAuthAgent  ActionAuth = "agent"
)

type ActionTransport string

const (
	ActionTransportHTTPOnly     ActionTransport = "http_only"
	ActionTransportHTTPAndWS    ActionTransport = "http_and_ws_request"
	ActionTransportWSStreamOnly ActionTransport = "ws_stream_only"
	ActionTransportInternalOnly ActionTransport = "internal_only"
)

type ActionSpec struct {
	Name      string
	Auth      ActionAuth
	Transport ActionTransport
	Schema    *ActionSchema
}

var actionSpecs = []ActionSpec{
	{Name: "portal.bootstrap", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "portal.auth", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "portal.status", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "portal.password", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "portal.account.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: RealtimeWSTicketAction, Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "client.version.report", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "release.v2.status", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: releaseStatusSchema()},
	{Name: "release.v2.apply", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: releaseApplySchema()},

	{Name: "profile.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "profile.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "sync.bootstrap", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "sync.read_marker", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "conversations.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "conversations.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},

	{Name: "agent.password", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.account.deprovision", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.matrix_session.create", Auth: ActionAuthAgent, Transport: ActionTransportHTTPOnly},
	{Name: "agent.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.config.propose_patch", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.backends.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.core.status.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.core.model_profiles.sync", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelProfileSyncSchema()},
	{Name: "agent.core.model_profiles.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelProfileListSchema()},
	{Name: "agent.core.model_profiles.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelProfileGetSchema()},
	{Name: "agent.core.model_profiles.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelProfileDeleteSchema()},
	{Name: "agent.model_profiles.sync", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelProfileSyncSchema()},
	{Name: "agent.model_profiles.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelProfileListSchema()},
	{Name: "agent.model_profiles.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelProfileGetSchema()},
	{Name: "agent.model_profiles.test", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelProfileTestSchema()},
	{Name: "agent.model_profiles.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelProfileDeleteSchema()},
	{Name: "agent.core.tasks.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreTaskGetSchema()},
	{Name: "agent.core.tasks.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreTaskListSchema()},
	{Name: "agent.core.tasks.cancel", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreTaskMutationSchema()},
	{Name: "agent.core.tasks.retry", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreTaskMutationSchema()},
	{Name: "agent.core.tasks.events", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreTaskEventsSchema()},
	{Name: "agent.core.schedules.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreScheduleCreateSchema()},
	{Name: "agent.core.schedules.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreScheduleGetSchema()},
	{Name: "agent.core.schedules.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreScheduleListSchema()},
	{Name: "agent.core.schedules.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreScheduleUpdateSchema()},
	{Name: "agent.core.schedules.pause", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreScheduleMutationSchema()},
	{Name: "agent.core.schedules.resume", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreScheduleMutationSchema()},
	{Name: "agent.core.schedules.trigger", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreScheduleTriggerSchema()},
	{Name: "agent.core.schedules.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreScheduleDeleteSchema()},
	{Name: "agent.core.confirmations.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreConfirmationGetSchema()},
	{Name: "agent.core.confirmations.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreConfirmationListSchema()},
	{Name: "agent.core.confirmations.confirm", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreConfirmationMutationSchema()},
	{Name: "agent.core.confirmations.reject", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreConfirmationMutationSchema()},
	{Name: "agent.core.confirmations.acknowledge_extension_execution_uncertain", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreConfirmationExtensionUncertainAcknowledgeSchema()},
	{Name: "agent.core.mcp.discover", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionDiscoverSchema()},
	{Name: "agent.core.mcp.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionGetSchema()},
	{Name: "agent.core.mcp.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionListSchema()},
	{Name: "agent.core.mcp.inspect", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionInspectSchema()},
	{Name: "agent.core.mcp.install", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionInstallSchema()},
	{Name: "agent.core.mcp.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionUpdateSchema()},
	{Name: "agent.core.mcp.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionRemoveSchema()},
	{Name: "agent.core.mcp.list_tools", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreMCPToolsSchema()},
	{Name: "agent.core.mcp.execute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreMCPExecuteSchema()},
	{Name: "agent.core.skills.discover", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionDiscoverSchema()},
	{Name: "agent.core.skills.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionGetSchema()},
	{Name: "agent.core.skills.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionListSchema()},
	{Name: "agent.core.skills.inspect", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionInspectSchema()},
	{Name: "agent.core.skills.install", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionInstallSchema()},
	{Name: "agent.core.skills.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionUpdateSchema()},
	{Name: "agent.core.skills.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreExtensionRemoveSchema()},
	{Name: "agent.core.skills.execute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreSkillExecuteSchema()},
	{Name: "agent.core.aws.credentials.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreAWSCredentialCreateSchema()},
	{Name: "agent.core.aws.credentials.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreAWSCredentialUpdateSchema()},
	{Name: "agent.core.aws.credentials.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreAWSCredentialDeleteSchema()},
	{Name: "agent.core.aws.credentials.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreAWSCredentialListSchema()},
	{Name: "agent.core.aws.credentials.test", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: coreAWSCredentialTestSchema()},
	{Name: "agent.chat", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: nativeAgentChatSchema(false)},
	{Name: "agent.chat.stream", Auth: ActionAuthOwner, Transport: ActionTransportWSStreamOnly, Schema: nativeAgentChatSchema(true)},
	{Name: "agent.chat.attachment.begin", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: nativeAgentAttachmentBeginSchema()},
	{Name: "agent.chat.attachment.append", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: nativeAgentAttachmentAppendSchema()},
	{Name: "agent.chat.attachment.commit", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: nativeAgentAttachmentCommitSchema()},
	{Name: "agent.web_search.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: webSearchConfigGetSchema()},
	{Name: "agent.web_search.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: webSearchConfigUpdateSchema()},
	{Name: "agent.web_search.test", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: webSearchTestSchema()},
	{Name: "agent.text_tools.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: textToolsConfigSchema(false)},
	{Name: "agent.text_tools.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: textToolsConfigSchema(true)},
	{Name: "agent.text_tools.execute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: textToolsExecuteSchema()},
	{Name: "agent.image_tools.upload.begin", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: imageToolUploadBeginSchema()},
	{Name: "agent.image_tools.upload.append", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: imageToolUploadAppendSchema()},
	{Name: "agent.image_tools.upload.commit", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: imageToolUploadCommitSchema()},
	{Name: "agent.image_tools.extract_text", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: imageToolExecuteSchema(false)},
	{Name: "agent.image_tools.translate_text", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: imageToolExecuteSchema(true)},
	{Name: "agent.chat.turn.stop", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: nativeAgentTurnStopSchema()},
	{Name: "agent.chat.turns.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: nativeAgentTurnsListSchema()},
	{Name: "agent.voice.session.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: voiceSessionCreateSchema()},
	{Name: "agent.voice.session.start", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: voiceSessionMutationSchema()},
	{Name: "agent.voice.session.transcript", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: voiceSessionTranscriptSchema()},
	{Name: "agent.voice.session.interrupt", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: voiceSessionMutationSchema()},
	{Name: "agent.voice.session.end", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: voiceSessionMutationSchema()},
	{Name: "agent.voice.session.stream", Auth: ActionAuthOwner, Transport: ActionTransportWSStreamOnly, Schema: voiceSessionStreamSchema()},
	{Name: "agent.chat.conversations.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: conversationSchema("create")},
	{Name: "agent.chat.conversations.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: conversationSchema("list")},
	{Name: "agent.chat.conversations.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: conversationSchema("get")},
	{Name: "agent.chat.conversations.rename", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: conversationSchema("rename")},
	{Name: "agent.chat.conversations.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: conversationSchema("delete")},
	{Name: "agent.context.compress", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.models.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: modelCatalogSchema()},
	{Name: "agent.runtime.inspect", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.runtime.install", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.runtime.which", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.runtime.run", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.skills.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.skills.install", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.skills.enable", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.skills.disable", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.skills.uninstall", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.skills.registry.search", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.mcp.servers.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.mcp.servers.install", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.mcp.servers.enable", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.mcp.servers.disable", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.mcp.servers.uninstall", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.mcp.registry.search", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeConfigSchema("get")},
	{Name: "agent.knowledge.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeConfigSchema("update")},
	{Name: "agent.knowledge.sources.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSourceSchema("list")},
	{Name: "agent.knowledge.sources.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSourceSchema("delete")},
	{Name: "agent.knowledge.upload.start", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSourceSchema("upload_start")},
	{Name: "agent.knowledge.upload.chunk", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSourceSchema("upload_chunk")},
	{Name: "agent.knowledge.upload.finish", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSourceSchema("upload_finish")},
	{Name: "agent.knowledge.memory.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSchema("create")},
	{Name: "agent.knowledge.memories.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSchema("memories_list")},
	{Name: "agent.knowledge.memories.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSchema("memories_update")},
	{Name: "agent.knowledge.memories.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSchema("memories_delete")},
	{Name: "agent.knowledge.search", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSchema("search")},
	{Name: "agent.knowledge.status", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: knowledgeSchema("status")},
	{Name: "agent.contacts.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.contacts.search", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.rooms.search", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.messages.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.messages.send", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.room_members.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.channel_posts.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.channel_comments.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.channel_comments.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.summarize", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	// execution.v2 is the owner-scoped, WS-first execution surface.  Provider
	// mutations remain fail-closed until a typed port reports readiness.
	{Name: executionV2Name("projects.analyze"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("projects.analyze")},
	{Name: executionV2Name("analyses.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("analyses.get")},
	{Name: executionV2Name("targets.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("targets.list")},
	{Name: executionV2Name("targets.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("targets.get")},
	{Name: executionV2Name("targets.import"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("targets.import")},
	{Name: executionV2Name("targets.reserve"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("targets.reserve")},
	{Name: executionV2Name("targets.observe"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("targets.observe")},
	{Name: executionV2Name("plans.create"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("plans.create")},
	{Name: executionV2Name("plans.revise"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("plans.revise")},
	{Name: executionV2Name("plans.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("plans.get")},
	{Name: executionV2Name("plans.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("plans.list")},
	{Name: executionV2Name("deployments.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("deployments.list")},
	{Name: executionV2Name("deployments.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("deployments.get")},
	{Name: executionV2Name("deployments.events"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("deployments.events")},
	{Name: executionV2Name("runs.create"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("runs.create")},
	{Name: executionV2Name("runs.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("runs.get")},
	{Name: executionV2Name("runs.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("runs.list")},
	{Name: executionV2Name("runs.cancel"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("runs.cancel")},
	{Name: executionV2Name("runs.retry"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("runs.retry")},
	{Name: executionV2Name("runs.events"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("runs.events")},
	{Name: executionV2Name("artifacts.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("artifacts.get")},
	{Name: executionV2Name("artifacts.download"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("artifacts.download")},
	{Name: executionV2Name("service_bindings.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("service_bindings.list")},
	{Name: executionV2Name("service_bindings.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("service_bindings.get")},
	{Name: executionV2Name("service_bindings.invoke"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("service_bindings.invoke")},
	{Name: executionV2Name("secrets.create"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("secrets.create")},
	{Name: executionV2Name("secrets.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("secrets.get")},
	{Name: executionV2Name("secrets.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("secrets.list")},
	{Name: executionV2Name("secrets.revoke"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS, Schema: executionV2Schema("secrets.revoke")},

	{Name: "plugins.catalog.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.installed.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.install", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.enable", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.disable", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.uninstall", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.job.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.health", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.logs.tail", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.invoke", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "plugins.invoke.stream", Auth: ActionAuthOwner, Transport: ActionTransportWSStreamOnly},

	{Name: "contacts.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "contacts.request", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "contacts.reactivate", Auth: ActionAuthPublic, Transport: ActionTransportHTTPAndWS},
	{Name: "rooms.reactivate", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "contacts.requests.accept", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "contacts.requests.reject", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "contacts.requests.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "contacts.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "contacts.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "blocks.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "blocks.add", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "blocks.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},

	{Name: "follows.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "follows.add", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "follows.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "favorites.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "favorites.add", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "favorites.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "favorites.delete_batch", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "reports.submit", Auth: ActionAuthPublic, Transport: ActionTransportHTTPAndWS},
	{Name: "calls.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "calls.incoming", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "calls.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "calls.event", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "calls.active", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "calls.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},

	{Name: "groups.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.invite", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.join", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.members", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.dissolve", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.leave", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.invite.reject", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.member.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.member.mute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.member.unmute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.mute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.unmute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "groups.invite_policy.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},

	{Name: "channels.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.join", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.invite_grant.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.invite", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.dissolve", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.leave", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.member.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.member.mute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.member.unmute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.join_request.approve", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.join_request.reject", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.mute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.unmute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.read_marker", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.members", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.public.search", Auth: ActionAuthPublic, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.public.get", Auth: ActionAuthPublic, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.public.posts.list", Auth: ActionAuthPublic, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.public.join_request", Auth: ActionAuthPublic, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.public.join_result", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "users.public_channels", Auth: ActionAuthPublic, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.posts.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.posts.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.posts.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.posts.recall", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.comments.recall", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.comments.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.comments.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.post_reaction.toggle", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.comment_reaction.toggle", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.my_comments", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "channels.my_reactions", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
}

var actionSpecIndex = mustBuildActionSpecIndex(actionSpecs)

func ActionSpecs() []ActionSpec {
	specs := make([]ActionSpec, len(actionSpecs))
	for i, spec := range actionSpecs {
		specs[i] = cloneActionSpec(spec)
	}
	return specs
}

func ActionSpecFor(action string) (ActionSpec, bool) {
	action = strings.TrimSpace(action)
	spec, ok := actionSpecIndex[action]
	if !ok {
		return ActionSpec{}, false
	}
	return cloneActionSpec(spec), true
}

func buildActionSpecIndex(specs []ActionSpec) (map[string]ActionSpec, error) {
	index := make(map[string]ActionSpec, len(specs))
	for _, spec := range specs {
		if _, exists := index[spec.Name]; exists {
			return nil, fmt.Errorf("duplicate action spec name %q", spec.Name)
		}
		index[spec.Name] = spec
	}
	return index, nil
}

func mustBuildActionSpecIndex(specs []ActionSpec) map[string]ActionSpec {
	index, err := buildActionSpecIndex(specs)
	if err != nil {
		panic(err)
	}
	return index
}

func PublicActions() []string {
	return actionsWithAuth(ActionAuthPublic)
}

func AgentActions() []string {
	return actionsWithAuth(ActionAuthAgent)
}

func PublicAction(action string) bool {
	spec, ok := ActionSpecFor(action)
	return ok && spec.Auth == ActionAuthPublic
}

func AgentAction(action string) bool {
	spec, ok := ActionSpecFor(action)
	return ok && spec.Auth == ActionAuthAgent
}

func HTTPAction(action string) bool {
	spec, ok := ActionSpecFor(action)
	if !ok {
		return false
	}
	return spec.Transport == ActionTransportHTTPOnly || spec.Transport == ActionTransportHTTPAndWS
}

func RealtimeWSClientRequestAction(action string) bool {
	spec, ok := ActionSpecFor(action)
	return ok && spec.Transport == ActionTransportHTTPAndWS
}

func HTTPOnlyAction(action string) bool {
	spec, ok := ActionSpecFor(action)
	return ok && spec.Transport == ActionTransportHTTPOnly
}

func WSStreamOnlyAction(action string) bool {
	spec, ok := ActionSpecFor(action)
	return ok && spec.Transport == ActionTransportWSStreamOnly
}

func actionsWithAuth(auth ActionAuth) []string {
	actions := make([]string, 0)
	for _, spec := range actionSpecs {
		if spec.Auth == auth {
			actions = append(actions, spec.Name)
		}
	}
	return actions
}
