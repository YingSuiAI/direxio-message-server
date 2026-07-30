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
}

var actionSpecs = []ActionSpec{
	{Name: "portal.bootstrap", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "portal.auth", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "portal.status", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
	{Name: "portal.password", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "portal.account.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: RealtimeWSTicketAction, Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "client.version.report", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "release.v1.status", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "release.v1.apply", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "release.v2.status", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	{Name: "release.v2.apply", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},

	{Name: "profile.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "profile.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "sync.bootstrap", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "sync.read_marker", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "conversations.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "conversations.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},

	{Name: "agent.password", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.matrix_session.create", Auth: ActionAuthAgent, Transport: ActionTransportHTTPOnly},
	{Name: "agent.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.config.propose_patch", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.chat", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.chat.stream", Auth: ActionAuthOwner, Transport: ActionTransportWSStreamOnly},
	{Name: "agent.chat.turn.stop", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.chat.turns.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.context.compress", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.models.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
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
	{Name: "agent.knowledge.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.sources.list", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.sources.delete", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.upload.start", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.upload.chunk", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.upload.finish", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.memory.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.search", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	{Name: "agent.knowledge.status", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
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
	copy(specs, actionSpecs)
	return specs
}

func ActionSpecFor(action string) (ActionSpec, bool) {
	action = strings.TrimSpace(action)
	spec, ok := actionSpecIndex[action]
	return spec, ok
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
