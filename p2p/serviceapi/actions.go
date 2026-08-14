package serviceapi

import (
	"fmt"
	"strings"
)

type ActionAuth string

const (
	ActionAuthPublic ActionAuth = "public"
	ActionAuthOwner  ActionAuth = "owner"
	ActionAuthAgent  ActionAuth = "agent"
)

type ActionTransport string

const (
	ActionTransportHTTPOnly     ActionTransport = "http_only"
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
	{Name: "client.version.report", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "release.v2.status", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: releaseStatusSchema()},
	{Name: "release.v2.apply", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: releaseApplySchema()},

	{Name: "profile.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "profile.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "sync.bootstrap", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "sync.read_marker", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "conversations.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "conversations.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},

	{Name: "agent.password", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.account.deprovision", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.matrix_session.create", Auth: ActionAuthAgent, Transport: ActionTransportHTTPOnly},
	{Name: "agent.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: agentConfigSchema(false)},
	{Name: "agent.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: agentConfigSchema(true)},
	{Name: "agent.backends.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.model_profiles.sync", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: modelProfileSyncSchema()},
	{Name: "agent.model_profiles.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: modelProfileListSchema()},
	{Name: "agent.model_profiles.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: modelProfileGetSchema()},
	{Name: "agent.model_profiles.test", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: modelProfileTestSchema()},
	{Name: "agent.model_profiles.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: modelProfileDeleteSchema()},
	{Name: "agent.core.tasks.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreTaskGetSchema()},
	{Name: "agent.core.tasks.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreTaskListSchema()},
	{Name: "agent.core.tasks.cancel", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreTaskMutationSchema()},
	{Name: "agent.core.tasks.retry", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreTaskMutationSchema()},
	{Name: "agent.core.tasks.events", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreTaskEventsSchema()},
	{Name: "agent.core.schedules.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreScheduleCreateSchema()},
	{Name: "agent.core.schedules.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreScheduleGetSchema()},
	{Name: "agent.core.schedules.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreScheduleListSchema()},
	{Name: "agent.core.schedules.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreScheduleUpdateSchema()},
	{Name: "agent.core.schedules.pause", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreScheduleMutationSchema()},
	{Name: "agent.core.schedules.resume", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreScheduleMutationSchema()},
	{Name: "agent.core.schedules.trigger", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreScheduleTriggerSchema()},
	{Name: "agent.core.schedules.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreScheduleDeleteSchema()},
	{Name: "agent.core.confirmations.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreConfirmationGetSchema()},
	{Name: "agent.core.confirmations.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreConfirmationListSchema()},
	{Name: "agent.core.confirmations.confirm", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreConfirmationMutationSchema()},
	{Name: "agent.core.confirmations.reject", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreConfirmationMutationSchema()},
	{Name: "agent.core.confirmations.acknowledge_extension_execution_uncertain", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreConfirmationExtensionUncertainAcknowledgeSchema()},
	{Name: "agent.core.mcp.discover", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreMCPDiscoverSchema()},
	{Name: "agent.core.mcp.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreMCPGetSchema()},
	{Name: "agent.core.mcp.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreMCPListSchema()},
	{Name: "agent.core.mcp.inspect", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreMCPInspectSchema()},
	{Name: "agent.core.mcp.install", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreMCPInstallSchema()},
	{Name: "agent.core.mcp.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreMCPUpdateSchema()},
	{Name: "agent.core.mcp.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreMCPRemoveSchema()},
	{Name: "agent.core.mcp.list_tools", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreMCPToolsSchema()},
	{Name: "agent.core.mcp.execute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreMCPExecuteSchema()},
	{Name: "agent.core.skills.discover", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreSkillDiscoverSchema()},
	{Name: "agent.core.skills.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreSkillGetSchema()},
	{Name: "agent.core.skills.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreSkillListSchema()},
	{Name: "agent.core.skills.inspect", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreSkillInspectSchema()},
	{Name: "agent.core.skills.install", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreSkillInstallSchema()},
	{Name: "agent.core.skills.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreSkillUpdateSchema()},
	{Name: "agent.core.skills.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreSkillRemoveSchema()},
	{Name: "agent.core.skills.execute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreSkillExecuteSchema()},
	{Name: "agent.core.aws.credentials.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreAWSCredentialCreateSchema()},
	{Name: "agent.core.aws.credentials.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreAWSCredentialUpdateSchema()},
	{Name: "agent.core.aws.credentials.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreAWSCredentialDeleteSchema()},
	{Name: "agent.core.aws.credentials.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreAWSCredentialListSchema()},
	{Name: "agent.core.aws.credentials.test", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: coreAWSCredentialTestSchema()},
	{Name: "agent.chat", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: nativeAgentChatSchema(false)},
	{Name: "agent.chat.attachment.begin", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: nativeAgentAttachmentBeginSchema()},
	{Name: "agent.chat.attachment.append", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: nativeAgentAttachmentAppendSchema()},
	{Name: "agent.chat.attachment.commit", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: nativeAgentAttachmentCommitSchema()},
	{Name: "agent.web_search.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: webSearchConfigGetSchema()},
	{Name: "agent.web_search.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: webSearchConfigUpdateSchema()},
	{Name: "agent.web_search.test", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: webSearchTestSchema()},
	{Name: "agent.memory.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: memoryConfigSchema(false)},
	{Name: "agent.memory.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: memoryConfigSchema(true)},
	{Name: "agent.memory.status", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: memoryStatusSchema()},
	{Name: "agent.memory.facts.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: memoryFactUpdateSchema()},
	{Name: "agent.memory.facts.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: memoryFactDeleteSchema()},
	{Name: "agent.static_sites.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: staticSiteListSchema()},
	{Name: "agent.static_sites.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: staticSiteDeleteSchema()},
	{Name: "agent.workers.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: workerListSchema()},
	{Name: "agent.workers.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: workerGetSchema()},
	{Name: "agent.workers.destroy", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: workerDestroySchema()},
	{Name: "agent.workers.bind_domain", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: workerBindDomainSchema(false)},
	{Name: "agent.workers.unbind_domain", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: workerBindDomainSchema(true)},
	{Name: "agent.text_tools.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: textToolsConfigSchema(false)},
	{Name: "agent.text_tools.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: textToolsConfigSchema(true)},
	{Name: "agent.text_tools.execute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: textToolsExecuteSchema()},
	{Name: "agent.image_tools.upload.begin", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: imageToolUploadBeginSchema()},
	{Name: "agent.image_tools.upload.append", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: imageToolUploadAppendSchema()},
	{Name: "agent.image_tools.upload.commit", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: imageToolUploadCommitSchema()},
	{Name: "agent.image_tools.extract_text", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: imageToolExecuteSchema(false)},
	{Name: "agent.image_tools.translate_text", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: imageToolExecuteSchema(true)},
	{Name: "agent.chat.turn.stop", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: nativeAgentTurnStopSchema()},
	{Name: "agent.chat.turn.steer", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: nativeAgentTurnSteerSchema()},
	{Name: "agent.chat.turns.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: nativeAgentTurnsListSchema()},
	{Name: "agent.voice.session.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: voiceSessionCreateSchema()},
	{Name: "agent.voice.session.start", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: voiceSessionMutationSchema()},
	{Name: "agent.voice.session.transcript", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: voiceSessionTranscriptSchema()},
	{Name: "agent.voice.session.interrupt", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: voiceSessionMutationSchema()},
	{Name: "agent.voice.session.end", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: voiceSessionMutationSchema()},
	{Name: "agent.chat.conversations.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: conversationSchema("create")},
	{Name: "agent.chat.conversations.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: conversationSchema("list")},
	{Name: "agent.chat.conversations.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: conversationSchema("get")},
	{Name: "agent.chat.conversations.rename", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: conversationSchema("rename")},
	{Name: "agent.chat.conversations.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: conversationSchema("delete")},
	{Name: "agent.context.compress", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.models.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: modelCatalogSchema()},
	{Name: "agent.runtime.inspect", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.runtime.install", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.runtime.which", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.runtime.run", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.knowledge.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: knowledgeConfigSchema("get")},
	{Name: "agent.knowledge.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: knowledgeConfigSchema("update")},
	{Name: "agent.knowledge.sources.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: knowledgeSourceSchema("list")},
	{Name: "agent.knowledge.sources.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: knowledgeSourceSchema("delete")},
	{Name: "agent.knowledge.upload.start", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: knowledgeSourceSchema("upload_start")},
	{Name: "agent.knowledge.upload.chunk", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: knowledgeSourceSchema("upload_chunk")},
	{Name: "agent.knowledge.upload.finish", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: knowledgeSourceSchema("upload_finish")},
	{Name: "agent.knowledge.search", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: knowledgeSchema("search")},
	{Name: "agent.knowledge.status", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: knowledgeSchema("status")},
	{Name: "agent.contacts.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.contacts.search", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.rooms.search", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.messages.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.messages.send", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.room_members.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.channel_posts.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.channel_comments.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.channel_comments.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "agent.summarize", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	// execution.v2 is the owner-scoped, WS-first execution surface.  Provider
	// mutations remain fail-closed until a typed port reports readiness.
	{Name: executionV2Name("projects.analyze"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("projects.analyze")},
	{Name: executionV2Name("analyses.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("analyses.get")},
	{Name: executionV2Name("targets.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("targets.list")},
	{Name: executionV2Name("targets.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("targets.get")},
	{Name: executionV2Name("targets.import"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("targets.import")},
	{Name: executionV2Name("targets.reserve"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("targets.reserve")},
	{Name: executionV2Name("targets.observe"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("targets.observe")},
	{Name: executionV2Name("plans.create"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("plans.create")},
	{Name: executionV2Name("plans.revise"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("plans.revise")},
	{Name: executionV2Name("plans.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("plans.get")},
	{Name: executionV2Name("plans.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("plans.list")},
	{Name: executionV2Name("deployments.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("deployments.list")},
	{Name: executionV2Name("deployments.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("deployments.get")},
	{Name: executionV2Name("deployments.events"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("deployments.events")},
	{Name: executionV2Name("runs.create"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("runs.create")},
	{Name: executionV2Name("runs.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("runs.get")},
	{Name: executionV2Name("runs.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("runs.list")},
	{Name: executionV2Name("runs.cancel"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("runs.cancel")},
	{Name: executionV2Name("runs.retry"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("runs.retry")},
	{Name: executionV2Name("runs.events"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("runs.events")},
	{Name: executionV2Name("artifacts.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("artifacts.get")},
	{Name: executionV2Name("artifacts.download"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("artifacts.download")},
	{Name: executionV2Name("service_bindings.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("service_bindings.list")},
	{Name: executionV2Name("service_bindings.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("service_bindings.get")},
	{Name: executionV2Name("service_bindings.invoke"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("service_bindings.invoke")},
	{Name: executionV2Name("secrets.create"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("secrets.create")},
	{Name: executionV2Name("secrets.get"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("secrets.get")},
	{Name: executionV2Name("secrets.list"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("secrets.list")},
	{Name: executionV2Name("secrets.revoke"), Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: executionV2Schema("secrets.revoke")},

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

	{Name: "contacts.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "contacts.request", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "contacts.reactivate", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "rooms.reactivate", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "contacts.requests.accept", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "contacts.requests.reject", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "contacts.requests.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "contacts.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "contacts.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "blocks.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "blocks.add", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "blocks.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},

	{Name: "follows.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "follows.add", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "follows.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "favorites.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "favorites.add", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "favorites.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "favorites.delete_batch", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "reports.submit", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "calls.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "calls.incoming", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "calls.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "calls.event", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "calls.active", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "calls.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},

	{Name: "groups.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.invite", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.join", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.members", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.dissolve", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.leave", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.invite.reject", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.member.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.member.mute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.member.unmute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.mute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.unmute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "groups.invite_policy.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},

	{Name: "channels.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.join", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.invite_grant.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.invite", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.dissolve", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.leave", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.member.remove", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.member.mute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.member.unmute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.join_request.approve", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.join_request.reject", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.mute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.unmute", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.read_marker", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.members", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.public.search", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "channels.public.get", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "channels.public.posts.list", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "channels.public.join_request", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "channels.public.join_result", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "users.public_channels", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "channels.posts.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.posts.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.posts.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.posts.recall", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.comments.recall", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.comments.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.comments.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.post_reaction.toggle", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.comment_reaction.toggle", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.my_comments", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "channels.my_reactions", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
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
	return spec.Transport == ActionTransportHTTPOnly
}

func HTTPOnlyAction(action string) bool {
	spec, ok := ActionSpecFor(action)
	return ok && spec.Transport == ActionTransportHTTPOnly
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
