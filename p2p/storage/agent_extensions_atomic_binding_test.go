package storage

import (
	"strings"
	"testing"

	confirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	extensions "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

func TestToCoreBindingPinsExtensionToolSchemaDigest(t *testing.T) {
	binding := extensions.ConfirmationBinding{
		OwnerID:          "@extension-binding:example.test",
		Operation:        "execute",
		TargetID:         "target-1",
		TargetRevision:   3,
		VersionID:        "00000000-0000-4000-8000-000000000001",
		ToolName:         "search",
		ToolSchemaDigest: strings.Repeat("a", 64),
		SourceVersion:    "1.2.3",
		SourceCommit:     strings.Repeat("b", 40),
		ParameterDigest:  strings.Repeat("c", 64),
		ContentDigest:    strings.Repeat("d", 64),
		ManifestDigest:   strings.Repeat("e", 64),
		ExecutionDigest:  strings.Repeat("f", 64),
		NetworkDigest:    strings.Repeat("1", 64),
		SecretDigest:     strings.Repeat("2", 64),
	}

	got := toCoreBinding(binding)
	if got.PermissionDigest != confirmation.Digest(binding.ToolSchemaDigest) {
		t.Fatalf("permission digest = %q, want tool schema digest %q", got.PermissionDigest, binding.ToolSchemaDigest)
	}
	if got.ExtensionVersionID != binding.VersionID || got.TargetKind != "mcp" {
		t.Fatalf("extension pin mapping = version=%q kind=%q", got.ExtensionVersionID, got.TargetKind)
	}
	prefixed := binding
	prefixed.Operation = "extension.execute"
	if domain := toCoreBinding(prefixed).OperationDomain; domain != "extension.execute" {
		t.Fatalf("operation domain=%q, want extension.execute", domain)
	}
	if got.Digest == "" {
		t.Fatal("core binding digest is empty")
	}
	changed := binding
	changed.ToolSchemaDigest = strings.Repeat("3", 64)
	if toCoreBinding(changed).Digest == got.Digest {
		t.Fatal("changing the tool schema digest did not change the canonical binding digest")
	}
}

func TestToCoreBindingCanonicalizesExtensionGrantOrder(t *testing.T) {
	digest := strings.Repeat("a", 64)
	base := extensions.ConfirmationBinding{
		OwnerID:          "@extension-grants:example.test",
		Operation:        "execute",
		TargetID:         "target-1",
		TargetRevision:   1,
		VersionID:        "00000000-0000-4000-8000-000000000011",
		ToolSchemaDigest: digest,
		SourceVersion:    "1.0.0",
		ContentDigest:    digest,
		ManifestDigest:   digest,
		ExecutionDigest:  digest,
		ParameterDigest:  digest,
		NetworkDigest:    digest,
		SecretDigest:     digest,
		NetworkGrants:    []string{"https://z.example", "https://a.example"},
		SecretGrants: []extensions.SecretGrant{
			{ReferenceID: "00000000-0000-4000-8000-000000000013", Purpose: "mcp_credential", BindingDigest: digest},
			{ReferenceID: "00000000-0000-4000-8000-000000000012", Purpose: "mcp_credential", BindingDigest: digest},
		},
	}
	first := toCoreBinding(base)
	if len(first.NetworkGrants) != 2 || first.NetworkGrants[0] != "https://a.example" || first.NetworkGrants[1] != "https://z.example" {
		t.Fatalf("network grants were not canonicalized: %#v", first.NetworkGrants)
	}
	if len(first.SecretGrants) != 2 || first.SecretGrants[0].ReferenceID != "00000000-0000-4000-8000-000000000012" {
		t.Fatalf("secret grants were not canonicalized: %#v", first.SecretGrants)
	}
	base.NetworkGrants[0], base.NetworkGrants[1] = base.NetworkGrants[1], base.NetworkGrants[0]
	base.SecretGrants[0], base.SecretGrants[1] = base.SecretGrants[1], base.SecretGrants[0]
	if second := toCoreBinding(base); second.Digest != first.Digest {
		t.Fatalf("grant order changed canonical digest: first=%s second=%s", first.Digest, second.Digest)
	}
}
