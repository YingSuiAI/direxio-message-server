package agentgateway

import (
	"bytes"
	"crypto/sha256"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

func TestValidateCatalogRejectsMissingOperationAndBadDigest(t *testing.T) {
	descriptor := &capv1.CapabilityDescriptor{
		CapabilityId:    "agent.chat.v1",
		SemanticVersion: "1.0.0",
		ProtocolVersion: 1,
		Readiness:       true,
		Operations:      []*capv1.OperationDescriptor{catalogTestOperation("chat")},
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
		"unready":        {CapabilityId: "agent.chat.v1", SemanticVersion: "1.0.0", ProtocolVersion: 1, Operations: []*capv1.OperationDescriptor{catalogTestOperation("chat")}},
		"wrong protocol": {CapabilityId: "agent.chat.v1", SemanticVersion: "1.0.0", ProtocolVersion: 2, Readiness: true, Operations: []*capv1.OperationDescriptor{catalogTestOperation("chat")}},
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

func catalogTestOperation(operationID string) *capv1.OperationDescriptor {
	input := `{"type":"object"}`
	result := `{"type":"object"}`
	inputDigest := sha256.Sum256([]byte(input))
	resultDigest := sha256.Sum256([]byte(result))
	return &capv1.OperationDescriptor{
		OperationId:        operationID,
		InputSchemaJson:    input,
		InputSchemaDigest:  inputDigest[:],
		ResultSchemaJson:   result,
		ResultSchemaDigest: resultDigest[:],
	}
}

func catalogTestDescriptor(capabilityID, operationID string, input, result string) *capv1.CapabilityDescriptor {
	inputDigest := sha256.Sum256([]byte(input))
	resultDigest := sha256.Sum256([]byte(result))
	return &capv1.CapabilityDescriptor{
		CapabilityId:    capabilityID,
		SemanticVersion: "1.0.0",
		ProtocolVersion: 1,
		Readiness:       true,
		Operations: []*capv1.OperationDescriptor{{
			OperationId:        operationID,
			InputSchemaJson:    input,
			InputSchemaDigest:  inputDigest[:],
			ResultSchemaJson:   result,
			ResultSchemaDigest: resultDigest[:],
		}},
	}
}

func catalogTestWithDigest(t *testing.T, descriptor *capv1.CapabilityDescriptor) *capv1.DescribeCapabilitiesResponse {
	t.Helper()
	catalog := &capv1.DescribeCapabilitiesResponse{CatalogVersion: 1, Capabilities: []*capv1.CapabilityDescriptor{descriptor}}
	catalog.CatalogDigest = computeCatalogDigest(catalog.Capabilities)
	return catalog
}

func TestValidateCatalogRequiresPinnedSchemaIdentity(t *testing.T) {
	requirement := NewCatalogRequirement("agent.models.list")
	descriptor := catalogTestDescriptor(
		"agent.info.v1",
		"list_models",
		`{"additionalProperties":false,"properties":{"api_key":{"type":"string","writeOnly":true},"base_url":{"type":"string"},"client_model_profile_id":{"type":"string"},"model_kind":{"default":"conversation","enum":["conversation","embedding","speech"],"type":"string"},"model_profile_id":{"type":"string"},"provider":{"type":"string"}},"type":"object"}`,
		`{"additionalProperties":false,"properties":{"models":{"items":{"additionalProperties":true,"type":"object"},"type":"array"},"providers":{"items":{"additionalProperties":false,"properties":{"default_base_url":{"type":"string"},"dynamic_models":{"type":"boolean"},"provider":{"type":"string"},"requires_api_key":{"type":"boolean"}},"required":["provider","requires_api_key","dynamic_models"],"type":"object"},"type":"array"}},"required":["models","providers"],"type":"object"}`,
	)
	catalog := catalogTestWithDigest(t, descriptor)
	if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err != nil {
		t.Fatalf("current Agent model catalog rejected: %v", err)
	}

	// Recomputing the catalog digest must not make a drifted schema acceptable.
	descriptor.Operations[0].InputSchemaJson = `{"type":"object"}`
	inputDigest := sha256.Sum256([]byte(descriptor.Operations[0].InputSchemaJson))
	descriptor.Operations[0].InputSchemaDigest = inputDigest[:]
	catalog.CatalogDigest = computeCatalogDigest(catalog.Capabilities)
	if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err == nil {
		t.Fatal("catalog with incompatible but self-consistent input schema was accepted")
	}

	descriptor.Operations[0].InputSchemaJson = ""
	descriptor.Operations[0].InputSchemaDigest = nil
	catalog.CatalogDigest = computeCatalogDigest(catalog.Capabilities)
	if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err == nil {
		t.Fatal("catalog with empty input schema was accepted")
	}
}

func TestValidateCatalogRejectsSchemaDigestDriftAfterCatalogRehash(t *testing.T) {
	descriptor := catalogTestDescriptor("agent.web_search.v1", "get_config", `{"additionalProperties":false,"properties":{},"type":"object"}`, `{"additionalProperties":false,"properties":{"api_key_configured":{"type":"boolean"},"api_key_hint":{"type":"string"},"enabled":{"type":"boolean"},"provider":{"enum":["tavily"],"type":"string"},"revision":{"minimum":0,"type":"integer"},"tested_at":{"format":"date-time","type":"string"},"updated_at":{"format":"date-time","type":"string"}},"required":["enabled","provider","api_key_configured","revision"],"type":"object"}`)
	requirement := NewCatalogRequirement("agent.web_search.config.get")
	catalog := catalogTestWithDigest(t, descriptor)
	if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err != nil {
		t.Fatalf("current Agent web-search catalog rejected: %v", err)
	}

	descriptor.Operations[0].ResultSchemaDigest = bytes.Repeat([]byte{0x01}, sha256.Size)
	catalog.CatalogDigest = computeCatalogDigest(catalog.Capabilities)
	if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err == nil {
		t.Fatal("catalog with mismatched advertised result schema digest was accepted")
	}
}

func TestValidateCatalogRequiresChatSchemaPins(t *testing.T) {
	requirements := []CatalogRequirement{NewCatalogRequirement("agent.chat"), NewCatalogRequirement("agent.chat.stream")}
	input := `{"type":"object","properties":{"idempotency_key":{"type":"string"},"conversation_id":{"type":"string"},"message":{"type":"string"},"model_profile_id":{"type":"string"},"model_profile_revision":{"type":"integer"},"credential_version":{"type":"integer"}},"required":["idempotency_key","message","model_profile_id","model_profile_revision","credential_version"]}`
	result := `{"type":"object"}`
	inputDigest := sha256.Sum256([]byte(input))
	resultDigest := sha256.Sum256([]byte(result))
	descriptor := &capv1.CapabilityDescriptor{
		CapabilityId:    "agent.chat.v1",
		SemanticVersion: "1.0.0",
		ProtocolVersion: 1,
		Readiness:       true,
		Operations: []*capv1.OperationDescriptor{
			{OperationId: "chat", InputSchemaJson: input, InputSchemaDigest: inputDigest[:], ResultSchemaJson: result, ResultSchemaDigest: resultDigest[:]},
			{OperationId: "stream_chat", InputSchemaJson: input, InputSchemaDigest: inputDigest[:], ResultSchemaJson: result, ResultSchemaDigest: resultDigest[:]},
		},
	}
	catalog := catalogTestWithDigest(t, descriptor)
	if err := ValidateCatalog(catalog, requirements); err != nil {
		t.Fatalf("pinned chat catalog rejected: %v", err)
	}
	descriptor.Operations[0].InputSchemaJson = `{"type":"object"}`
	drifted := sha256.Sum256([]byte(descriptor.Operations[0].InputSchemaJson))
	descriptor.Operations[0].InputSchemaDigest = drifted[:]
	catalog.CatalogDigest = computeCatalogDigest(catalog.Capabilities)
	if err := ValidateCatalog(catalog, requirements); err == nil {
		t.Fatal("chat catalog accepted drifted input schema")
	}
}
