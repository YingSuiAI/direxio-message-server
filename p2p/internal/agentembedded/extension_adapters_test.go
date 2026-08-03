package agentembedded

import (
	"strings"
	"testing"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

func TestExtensionConfirmationBindingRoundTripsCanonicalDigests(t *testing.T) {
	digest := strings.Repeat("a", 64)
	schemaDigest := strings.Repeat("b", 64)
	binding := extensions.ConfirmationBinding{
		OwnerID: "owner", Operation: "execute", TargetID: "installation", VersionID: "00000000-0000-0000-0000-000000000002",
		TargetRevision: 4, SourceCommit: strings.Repeat("c", 40), ToolName: "echo", ToolSchemaDigest: schemaDigest,
		ParameterDigest: digest, ContentDigest: digest, ManifestDigest: digest, ExecutionDigest: digest,
		NetworkDigest: digest, SecretDigest: digest, NetworkGrants: []string{"https://mcp.example:443/mcp:" + digest},
		SecretGrants: []extensions.SecretGrant{{ReferenceID: "00000000-0000-0000-0000-000000000001", Purpose: "mcp_credential", BindingDigest: digest, Configured: true}},
	}
	core := toConfirmationBinding(binding)
	if core.OperationDomain != "extension.execute" || core.TargetKind != "mcp" || core.ExtensionVersionID != binding.VersionID || core.ParameterDigest != coreconfirmation.Digest(digest) || core.PermissionDigest != coreconfirmation.Digest(schemaDigest) {
		t.Fatalf("core binding = %#v", core)
	}
	roundTrip := fromConfirmation(coreconfirmation.Confirmation{ConfirmationID: "confirmation", OwnerID: "owner", Binding: core}).Binding
	if !roundTrip.Equal(binding) {
		t.Logf("round trip: %+v", roundTrip)
		t.Logf("original: %+v", binding)
		t.Fatalf("round-trip binding = %#v, want %#v", roundTrip, binding)
	}
	if got := toConfirmationBinding(roundTrip).OperationDomain; got != "extension.execute" {
		t.Fatalf("round-trip operation domain = %q", got)
	}
	wire := confirmationMap(coreconfirmation.Confirmation{Binding: core})["binding"].(map[string]any)
	if wire["extension_version_id"] != binding.VersionID {
		t.Fatalf("wire extension version = %#v", wire["extension_version_id"])
	}
}

func TestExtensionConfirmationBindingAvoidsDoubleOperationPrefix(t *testing.T) {
	digest := strings.Repeat("a", 64)
	core := coreconfirmation.Binding{
		OwnerID: "owner", OperationDomain: "extension.execute", TargetID: "installation", TargetRevision: 1, TargetKind: "mcp", ExtensionVersionID: "00000000-0000-0000-0000-000000000001",
		SourceCommit: strings.Repeat("c", 40), ContentDigest: coreconfirmation.Digest(digest), ParameterDigest: coreconfirmation.Digest(digest),
		NetworkDigest: coreconfirmation.Digest(digest), SecretGrantDigest: coreconfirmation.Digest(digest), ExecutionDigest: coreconfirmation.Digest(digest),
		ManifestDigest: coreconfirmation.Digest(digest), PermissionDigest: coreconfirmation.Digest(digest),
	}
	binding := fromConfirmation(coreconfirmation.Confirmation{Binding: core}).Binding
	if binding.Operation != "execute" {
		t.Fatalf("operation = %q, want execute", binding.Operation)
	}
	if got := toConfirmationBinding(binding).OperationDomain; got != "extension.execute" {
		t.Fatalf("operation domain = %q, want extension.execute", got)
	}
}

func TestExtensionConfirmationBindingCanonicalizesRepeatedOperationPrefix(t *testing.T) {
	binding := extensions.ConfirmationBinding{Operation: "extension.extension.execute"}
	if got := toConfirmationBinding(binding).OperationDomain; got != "extension.execute" {
		t.Fatalf("operation domain = %q, want extension.execute", got)
	}
}
