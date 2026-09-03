package agent

import (
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

func TestNormalizeConfigUsesExternalAIForMissingAndLegacyOnlineIdentity(t *testing.T) {
	tests := []struct {
		name string
		in   dirextalkdomain.AgentConfig
		want dirextalkdomain.AgentIdentityConfig
	}{
		{
			name: "missing identity",
			in:   dirextalkdomain.AgentConfig{Enabled: true},
			want: dirextalkdomain.AgentIdentityConfig{DisplayName: "External AI"},
		},
		{
			name: "legacy default identity",
			in: dirextalkdomain.AgentConfig{
				Enabled: true,
				OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{
					DisplayName: "Your Agent",
					AvatarURL:   "mxc://example.com/legacy-avatar",
				},
			},
			want: dirextalkdomain.AgentIdentityConfig{DisplayName: "External AI", AvatarURL: "mxc://example.com/legacy-avatar"},
		},
		{
			name: "custom identity",
			in: dirextalkdomain.AgentConfig{
				Enabled: true,
				OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{
					DisplayName: "Codex Online",
					AvatarURL:   "mxc://example.com/custom-avatar",
				},
			},
			want: dirextalkdomain.AgentIdentityConfig{DisplayName: "Codex Online", AvatarURL: "mxc://example.com/custom-avatar"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeConfig(tt.in).OnlineAgentIdentity
			if got != tt.want {
				t.Fatalf("online identity = %#v, want %#v", got, tt.want)
			}
		})
	}
}
