package p2p

import (
	"context"
	"errors"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

func TestAgentConfigNativeIdentityUpdateDoesNotSyncMatrixIdentity(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)
	service.agentRoomID = "!agents-real:example.com"
	service.agentConfig = normalizeAgentConfig(dirextalkdomain.AgentConfig{
		NativeAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Ying", AvatarURL: "mxc://example.com/ying"},
		OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Your Agent", AvatarURL: "mxc://example.com/online"},
	})

	result := mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"native_agent_identity": map[string]any{
			"display_name": "Ying Updated",
			"avatar_url":   "mxc://example.com/ying-updated",
		},
	})

	nativeIdentity := result["native_agent_identity"].(map[string]any)
	onlineIdentity := result["online_agent_identity"].(map[string]any)
	if nativeIdentity["display_name"] != "Ying Updated" ||
		onlineIdentity["display_name"] != "Your Agent" {
		t.Fatalf("unexpected identity result: %#v", result)
	}
	if issuer.profileUser != "" || len(transport.joinRequests) != 0 || len(transport.profileRequests) != 0 {
		t.Fatalf("native identity update must not sync Matrix identity, issuer=%#v joins=%#v profiles=%#v", issuer, transport.joinRequests, transport.profileRequests)
	}
}

func TestAgentConfigOnlineIdentityUpdateSyncsMatrixProfileAndRoomMember(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)
	service.agentRoomID = "!agents-real:example.com"
	service.agentConfig = normalizeAgentConfig(dirextalkdomain.AgentConfig{
		NativeAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Ying", AvatarURL: "mxc://example.com/ying"},
		OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Your Agent", AvatarURL: "mxc://example.com/old-online"},
	})

	result := mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"online_agent_identity": map[string]any{
			"display_name": "Your Agent Updated",
			"avatar_url":   "mxc://example.com/online-updated",
		},
	})

	nativeIdentity := result["native_agent_identity"].(map[string]any)
	onlineIdentity := result["online_agent_identity"].(map[string]any)
	if nativeIdentity["display_name"] != "Ying" ||
		onlineIdentity["display_name"] != "Your Agent Updated" ||
		onlineIdentity["avatar_url"] != "mxc://example.com/online-updated" {
		t.Fatalf("unexpected identity result: %#v", result)
	}
	if issuer.profileUser != "@agent:example.com" ||
		issuer.profileName != "Your Agent Updated" ||
		issuer.profileURL != "mxc://example.com/online-updated" {
		t.Fatalf("expected global Matrix profile sync, got %#v", issuer)
	}
	if len(transport.joinRequests) < 1 ||
		transport.joinRequests[0].UserMXID != "@agent:example.com" ||
		transport.joinRequests[0].DisplayName != "Your Agent Updated" ||
		transport.joinRequests[0].AvatarURL != "mxc://example.com/online-updated" {
		t.Fatalf("expected ensureAgentRoom to join with Online identity, got %#v", transport.joinRequests)
	}
	if len(transport.profileRequests) == 0 {
		t.Fatalf("expected room member profile sync")
	}
	lastProfile := transport.profileRequests[len(transport.profileRequests)-1]
	if lastProfile.RoomID != "!agents-real:example.com" ||
		lastProfile.UserMXID != "@agent:example.com" ||
		lastProfile.DisplayName != "Your Agent Updated" ||
		lastProfile.AvatarURL != "mxc://example.com/online-updated" {
		t.Fatalf("expected room member profile sync with Online identity, got %#v", transport.profileRequests)
	}
}

func TestAgentConfigOnlineIdentitySyncFailureKeepsDurableConfig(t *testing.T) {
	transport := &recordingTransport{profileErrors: map[string]error{
		"!agents-real:example.com": errors.New("member profile unavailable"),
	}}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.SetMatrixSessionIssuer(&recordingMatrixSessionIssuer{})
	service.agentRoomID = "!agents-real:example.com"
	service.agentConfig = normalizeAgentConfig(dirextalkdomain.AgentConfig{
		NativeAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Ying"},
		OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Your Agent"},
	})

	result, apiErr := service.Handle(context.Background(), "agent.config.update", map[string]any{
		"online_agent_identity": map[string]any{
			"display_name": "Saved Before Sync",
			"avatar_url":   "mxc://example.com/saved-before-sync",
		},
	})
	if apiErr == nil || apiErr.Code != "agent_identity_sync_failed" {
		t.Fatalf("expected stable Matrix sync error, result=%#v err=%#v", result, apiErr)
	}

	config := mustHandle[map[string]any](t, service, "agent.config.get", nil)
	onlineIdentity := config["online_agent_identity"].(map[string]any)
	if onlineIdentity["display_name"] != "Saved Before Sync" ||
		onlineIdentity["avatar_url"] != "mxc://example.com/saved-before-sync" {
		t.Fatalf("failed Matrix sync must keep persisted desired identity, got %#v", config)
	}
}
