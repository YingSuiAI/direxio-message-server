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
	{Name: "agent.session.create", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: agentSessionCreateSchema()},
	{Name: "agent.matrix_session.create", Auth: ActionAuthAgent, Transport: ActionTransportHTTPOnly},
	{Name: "agent.config.get", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: agentAccountConfigSchema(false)},
	{Name: "agent.config.update", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly, Schema: agentAccountConfigSchema(true)},

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
