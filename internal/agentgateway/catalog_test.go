package agentgateway

import (
	"bytes"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

func TestValidateCatalogRejectsMissingOperationAndBadDigest(t *testing.T) {
	descriptor := &capv1.CapabilityDescriptor{
		CapabilityId:    "agent.chat.v1",
		SemanticVersion: "1.0.0",
		ProtocolVersion: 1,
		Readiness:       true,
		Operations:      []*capv1.OperationDescriptor{{OperationId: "chat"}},
	}
	catalog := &capv1.DescribeCapabilitiesResponse{CatalogVersion: 1, Capabilities: []*capv1.CapabilityDescriptor{descriptor}}
	catalog.CatalogDigest = computeCatalogDigest(catalog.Capabilities)
	if err := ValidateCatalog(catalog, []CatalogRequirement{{Action: "agent.chat"}}); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	if err := ValidateCatalog(catalog, []CatalogRequirement{{Action: "agent.chat.conversations.get"}}); err == nil {
		t.Fatal("catalog missing operation was accepted")
	}
	catalog.CatalogDigest = bytes.Repeat([]byte{0x42}, 32)
	if err := ValidateCatalog(catalog, []CatalogRequirement{{Action: "agent.chat"}}); err == nil {
		t.Fatal("catalog with wrong digest was accepted")
	}
}

func TestValidateCatalogRejectsUnreadyOrWrongProtocolDescriptor(t *testing.T) {
	for name, descriptor := range map[string]*capv1.CapabilityDescriptor{
		"unready":        {CapabilityId: "agent.chat.v1", SemanticVersion: "1.0.0", ProtocolVersion: 1, Operations: []*capv1.OperationDescriptor{{OperationId: "chat"}}},
		"wrong protocol": {CapabilityId: "agent.chat.v1", SemanticVersion: "1.0.0", ProtocolVersion: 2, Readiness: true, Operations: []*capv1.OperationDescriptor{{OperationId: "chat"}}},
	} {
		t.Run(name, func(t *testing.T) {
			catalog := &capv1.DescribeCapabilitiesResponse{CatalogVersion: 1, Capabilities: []*capv1.CapabilityDescriptor{descriptor}}
			catalog.CatalogDigest = computeCatalogDigest(catalog.Capabilities)
			if err := ValidateCatalog(catalog, []CatalogRequirement{{Action: "agent.chat"}}); err == nil {
				t.Fatal("invalid descriptor was accepted")
			}
		})
	}
}
