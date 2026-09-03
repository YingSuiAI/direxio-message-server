package p2p

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestServiceStartupMigratesLegacyOnlineAgentNameAndRepairsMatrixIdentity(t *testing.T) {
	ctx := context.Background()
	store := p2pstorage.NewMemoryStore()
	legacy := portalState{
		Initialized:    true,
		Password:       "12345678",
		AccessToken:    "portal-token",
		MatrixDeviceID: matrixPortalDeviceID,
		AgentToken:     "agent-token",
		OwnerMXID:      "@owner:example.com",
		AgentRoomID:    "!agents-real:example.com",
		Profile: ownerProfile{
			UserID: "@owner:example.com",
			Domain: "example.com",
		},
		AgentConfig: agentConfig{
			Enabled: true,
			OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{
				DisplayName: "Your Agent",
				AvatarURL:   "mxc://example.com/legacy-avatar",
			},
		},
	}
	if err := store.SavePortal(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	transport := &recordingTransport{}
	service, err := NewServiceWithStoreAndTransport(ctx, Config{ServerName: "example.com"}, store, transport)
	if err != nil {
		t.Fatal(err)
	}
	if len(transport.profileRequests) != 1 || transport.profileRequests[0].DisplayName != "External AI" || transport.profileRequests[0].AvatarURL != "mxc://example.com/legacy-avatar" {
		t.Fatalf("agent room profile repair = %#v, want External AI with legacy avatar", transport.profileRequests)
	}

	persisted, ok, err := store.LoadPortal(ctx)
	if err != nil || !ok {
		t.Fatalf("LoadPortal() = (%#v, %v, %v), want persisted state", persisted, ok, err)
	}
	if persisted.AgentConfig.OnlineAgentIdentity.DisplayName != "External AI" {
		t.Fatalf("persisted online identity = %#v, want External AI", persisted.AgentConfig.OnlineAgentIdentity)
	}

	issuer := &recordingMatrixSessionIssuer{}
	service.SetMatrixSessionIssuer(issuer)
	mustHandle[map[string]any](t, service, "agent.matrix_session.create", map[string]any{"device_id": "DIREXTALK_CLI"})
	if issuer.sessionName != "External AI" || issuer.sessionURL != "mxc://example.com/legacy-avatar" {
		t.Fatalf("Matrix profile/session identity = (%q, %q), want External AI with legacy avatar", issuer.sessionName, issuer.sessionURL)
	}
}
