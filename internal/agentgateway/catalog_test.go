package agentgateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

func TestKnowledgeCatalogPinsCurrentAgentSchemaResults(t *testing.T) {
	want := map[string]string{
		"agent.knowledge.config.get":    "87c332e9185a0436d6488041bbfc11cd66c9f40e345af02d9f97a76676cd65ae",
		"agent.knowledge.config.update": "87c332e9185a0436d6488041bbfc11cd66c9f40e345af02d9f97a76676cd65ae",
		"agent.knowledge.search":        "3bff0a96cb6f09421ee1a5ea243b8801a0b61fa4b1f8e01cdf98653acfd99761",
		"agent.knowledge.memory.search": "3bff0a96cb6f09421ee1a5ea243b8801a0b61fa4b1f8e01cdf98653acfd99761",
		"agent.knowledge.status":        "01c2cda93a058ad6e133692089afb4b81b9e5800d18003f6cd2f3e62f8efa4a3",
	}
	for action, expected := range want {
		requirement := NewCatalogRequirement(action)
		if got := hex.EncodeToString(requirement.ResultSchemaDigest); got != expected {
			t.Errorf("%s result schema digest = %s, want %s", action, got, expected)
		}
	}
}

func TestValidateCatalogPinsKnowledgeStatusQuotaSchema(t *testing.T) {
	const input = `{"type":"object","additionalProperties":true}`
	const result = `{"additionalProperties":false,"properties":{"checked_at":{"format":"date-time","type":"string"},"cleanup_pending_count":{"minimum":0,"type":"integer"},"count":{"minimum":0,"type":"integer"},"embedding_indexed":{"minimum":0,"type":"integer"},"embedding_model":{"type":"string"},"embedding_profile_id":{"format":"uuid","type":"string"},"embedding_profile_revision":{"minimum":1,"type":"integer"},"embedding_stale":{"minimum":0,"type":"integer"},"failed_count":{"minimum":0,"type":"integer"},"indexing_count":{"minimum":0,"type":"integer"},"max_source_bytes":{"const":16777216,"type":"integer"},"quota_limit_bytes":{"const":67108864,"type":"integer"},"quota_remaining_bytes":{"minimum":0,"type":"integer"},"quota_used_bytes":{"minimum":0,"type":"integer"},"ready_count":{"minimum":0,"type":"integer"},"supported":{"type":"boolean"},"uploading_count":{"minimum":0,"type":"integer"}},"required":["supported","count","embedding_indexed","embedding_stale","ready_count","uploading_count","indexing_count","failed_count","cleanup_pending_count","checked_at","quota_used_bytes","quota_limit_bytes","quota_remaining_bytes","max_source_bytes"],"type":"object"}`
	descriptor := catalogTestDescriptor("agent.knowledge.v1", "status", input, result)
	catalog := catalogTestWithDigest(t, descriptor)
	requirement := NewCatalogRequirement("agent.knowledge.status")
	if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err != nil {
		t.Fatalf("current Agent knowledge status schema rejected: %v", err)
	}
	descriptor.Operations[0].ResultSchemaJson = `{"type":"object"}`
	drifted := sha256.Sum256([]byte(descriptor.Operations[0].ResultSchemaJson))
	descriptor.Operations[0].ResultSchemaDigest = drifted[:]
	catalog.CatalogDigest = computeCatalogDigest(catalog.Capabilities)
	if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err == nil {
		t.Fatal("catalog accepted drifted knowledge status quota schema")
	}
}

func TestValidateCatalogAcceptsCurrentAgentInfoSchemas(t *testing.T) {
	const input = `{"additionalProperties":false,"properties":{},"type":"object"}`
	const backend = `{"additionalProperties":false,"properties":{"api_version":{"type":"string"},"available":{"type":"boolean"},"capabilities":{"items":{"type":"string"},"type":"array"},"configured":{"type":"boolean"},"instance_id":{"type":"string"},"release_version":{"type":"string"},"status":{"type":"string"},"supported_model_providers":{"items":{"type":"string"},"type":"array"}},"required":["available","configured","status","capabilities","supported_model_providers"],"type":"object"}`
	const backends = `{"additionalProperties":false,"properties":{"core":{"$ref":"#/$defs/backend"},"embedded":{"$ref":"#/$defs/backend"}},"required":["core","embedded"],"$defs":{"backend":` + backend + `},"type":"object"}`

	descriptor := &capv1.CapabilityDescriptor{
		CapabilityId:    "agent.info.v1",
		SemanticVersion: "1.0.0",
		ProtocolVersion: 1,
		Readiness:       true,
		Operations: []*capv1.OperationDescriptor{
			catalogTestDescriptor("agent.info.v1", "get_backends", input, backends).Operations[0],
			catalogTestDescriptor("agent.info.v1", "get_status", input, backend).Operations[0],
		},
	}
	catalog := catalogTestWithDigest(t, descriptor)
	wantDigest := map[string]string{
		"agent.backends.get":    "4a0f95cd99ddf917e51efbf74e83f2dd78775f7602437f9afe31df0a25e82d19",
		"agent.core.status.get": "677f2cc7224b592e8bebc7e63eeecef4a63bda3b74fbe673999fc5bf559b675e",
	}
	for _, action := range []string{"agent.backends.get", "agent.core.status.get"} {
		t.Run(action, func(t *testing.T) {
			requirement := NewCatalogRequirement(action)
			if got := hex.EncodeToString(requirement.ResultSchemaDigest); got != wantDigest[action] {
				t.Fatalf("result schema digest = %s, want %s", got, wantDigest[action])
			}
			if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err != nil {
				t.Fatalf("current Agent info schema rejected: %v", err)
			}
		})
	}
}

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
		`{"additionalProperties":false,"properties":{"models":{"items":{"additionalProperties":false,"properties":{"context_length":{"type":"integer"},"context_window":{"type":"integer"},"created":{"type":"number"},"created_at":{"type":"string"},"id":{"type":"string"},"input_modalities":{"items":{"type":"string"},"type":"array"},"input_token_limit":{"type":"integer"},"max_input_tokens":{"type":"integer"},"max_output_tokens":{"type":"integer"},"max_tokens":{"type":"integer"},"name":{"type":"string"},"object":{"type":"string"},"output_modalities":{"items":{"enum":["audio","embedding","image","text","video"],"type":"string"},"type":"array"},"output_token_limit":{"type":"integer"},"owned_by":{"type":"string"},"provider":{"type":"string"},"type":{"type":"string"}},"required":["id","provider"],"type":"object"},"type":"array"},"providers":{"items":{"additionalProperties":false,"properties":{"default_base_url":{"type":"string"},"dynamic_models":{"type":"boolean"},"provider":{"type":"string"},"requires_api_key":{"type":"boolean"}},"required":["provider","requires_api_key","dynamic_models"],"type":"object"},"type":"array"}},"required":["models","providers"],"type":"object"}`,
	)
	catalog := catalogTestWithDigest(t, descriptor)
	const wantResultDigest = "52078e2bf86ed500efb85a81acfcfebe601b16d4ad169760c693320cc6fe7fca"
	if got := hex.EncodeToString(requirement.ResultSchemaDigest); got != wantResultDigest {
		t.Fatalf("agent.models.list result schema digest = %s, want %s", got, wantResultDigest)
	}
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

func TestValidateCatalogAcceptsCurrentMutationSchemas(t *testing.T) {
	const objectResult = `{"type":"object"}`
	const knowledgeConfigResult = `{"type":"object","properties":{"embedding_profile_id":{"type":"string"},"embedding_profile_revision":{"type":"integer"},"embedding_model":{"type":"string"},"dimension":{"type":"integer"},"collection":{"type":"string"},"collection_config_digest":{"type":"string"},"revision":{"type":"integer"},"updated_at":{"type":"string"}},"required":["embedding_profile_id","embedding_profile_revision","embedding_model","collection_config_digest","revision"]}`
	tests := []struct {
		action, capability, operation, input, result string
	}{
		{"agent.chat.conversations.create", "agent.chat.v1", "create_conversation", `{"type":"object","properties":{"title":{"type":"string"},"conversation_id":{"type":"string","format":"uuid"},"idempotency_key":{"type":"string","format":"uuid"}},"required":["conversation_id","idempotency_key"]}`, objectResult},
		{"agent.knowledge.config.update", "agent.knowledge.v1", "update_config", `{"type":"object","properties":{"idempotency_key":{"format":"uuid","type":"string"},"expected_revision":{"type":"integer"},"embedding_profile_id":{"type":"string"},"profile_id":{"type":"string"},"dimension":{"type":"integer"},"collection":{"type":"string"},"collection_config_digest":{"type":"string"}},"required":["idempotency_key","expected_revision"]}`, knowledgeConfigResult},
		{"agent.knowledge.sources.delete", "agent.knowledge.v1", "delete_source", `{"type":"object","properties":{"source_id":{"type":"string"},"expected_revision":{"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["source_id","expected_revision","idempotency_key"]}`, objectResult},
		{"agent.knowledge.upload.start", "agent.knowledge.v1", "start_upload", `{"type":"object","properties":{"upload_id":{"type":"string"},"source_id":{"type":"string"},"title":{"type":"string"},"relative_path":{"type":"string"},"media_type":{"type":"string"},"declared_size":{"type":"integer"},"content_sha256":{"type":"string"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["declared_size","content_sha256","idempotency_key"]}`, objectResult},
		{"agent.knowledge.upload.chunk", "agent.knowledge.v1", "append_upload_chunk", `{"type":"object","properties":{"upload_id":{"type":"string"},"ordinal":{"type":"integer"},"offset_bytes":{"type":"integer"},"data":{"type":"string"},"chunk_sha256":{"type":"string"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["upload_id","data","chunk_sha256","idempotency_key"]}`, objectResult},
		{"agent.knowledge.upload.finish", "agent.knowledge.v1", "commit_upload", `{"type":"object","properties":{"upload_id":{"type":"string"},"expected_revision":{"type":"integer"},"content_sha256":{"type":"string"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["upload_id","content_sha256","idempotency_key"]}`, objectResult},
		{"agent.knowledge.memory.create", "agent.knowledge.v1", "create_memory", `{"type":"object","properties":{"source_id":{"type":"string"},"title":{"type":"string"},"content":{"type":"string"},"content_sha256":{"type":"string"},"media_type":{"type":"string"},"tags":{"type":"array"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["content","idempotency_key"]}`, objectResult},
		{"agent.knowledge.memories.update", "agent.knowledge.v1", "update_memory", `{"type":"object","properties":{"memory_id":{"type":"string"},"source_id":{"type":"string"},"expected_revision":{"type":"integer"},"title":{"type":"string"},"content":{"type":"string"},"content_sha256":{"type":"string"},"media_type":{"type":"string"},"tags":{"type":"array"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["memory_id","expected_revision","content","idempotency_key"]}`, objectResult},
		{"agent.knowledge.memories.delete", "agent.knowledge.v1", "delete_memory", `{"type":"object","properties":{"memory_id":{"type":"string"},"expected_revision":{"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["memory_id","expected_revision","idempotency_key"]}`, objectResult},
		{"agent.knowledge.index", "agent.knowledge.v1", "index_sources", `{"type":"object","properties":{"source_ids":{"items":{"format":"uuid","type":"string"},"type":"array"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["source_ids","idempotency_key"]}`, objectResult},
		{"agent.core.aws.credentials.test", "agent.aws.v1", "test_credential", `{"type":"object","additionalProperties":false,"properties":{"credential_id":{"format":"uuid","type":"string"},"expected_revision":{"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["credential_id","expected_revision","idempotency_key"]}`, `{"additionalProperties":false,"properties":{"account_id":{"type":"string"},"credential_id":{"format":"uuid","type":"string"},"credential_revision":{"minimum":1,"type":"integer"},"principal_id":{"type":"string"},"tested_at":{"format":"date-time","type":"string"},"user_arn":{"type":"string"}},"required":["credential_id","credential_revision","account_id","user_arn","principal_id","tested_at"],"type":"object"}`},
	}

	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			descriptor := catalogTestDescriptor(test.capability, test.operation, test.input, test.result)
			catalog := catalogTestWithDigest(t, descriptor)
			if err := ValidateCatalog(catalog, []CatalogRequirement{NewCatalogRequirement(test.action)}); err != nil {
				t.Fatalf("current Agent schema rejected: %v", err)
			}
		})
	}
}
