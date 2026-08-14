package agentgateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

func TestExtensionMutationCatalogPinsCurrentStrictAgentSchemas(t *testing.T) {
	want := map[string]string{
		"agent.core.mcp.install":    "d298a2044537c6fe426468db657e114784169bc3404008073ac37e6b29a62d83",
		"agent.core.mcp.update":     "38e0d16c5ba8fb0ee20323ddb997840cdbaae6aa9b16dfe6e20f992ca2ee735c",
		"agent.core.skills.install": "9acc5baf60e119e1870f8521f9a5dc7686bd7e7cc3f86a88db232efc095b3296",
		"agent.core.skills.update":  "21a4aae561dcc8d6078c6f1637402e62711f7170b1da205e5ecc246b9ac8ae20",
	}
	for action, expected := range want {
		requirement := NewCatalogRequirement(action)
		if got := hex.EncodeToString(requirement.InputSchemaDigest); got != expected {
			t.Errorf("%s input schema digest = %s, want %s", action, got, expected)
		}
	}
}

func TestExtensionReadCatalogPinsCurrentClosedAgentSchemas(t *testing.T) {
	const (
		discover = `{"additionalProperties":false,"properties":{"page_size":{"maximum":100,"minimum":1,"type":"integer"},"page_token":{"maxLength":4096,"type":"string"},"query":{"type":"string"},"source":{"type":"string"}},"type":"object"}`
		list     = `{"additionalProperties":false,"properties":{"page_size":{"maximum":100,"minimum":1,"type":"integer"},"page_token":{"maxLength":4096,"type":"string"},"source":{"type":"string"},"state":{"type":"string"}},"type":"object"}`
		result   = `{"type":"object"}`
	)
	for _, test := range []struct{ action, operation, input string }{
		{"agent.core.skills.discover", "discover_skill", discover},
		{"agent.core.skills.list", "list_skills", list},
		{"agent.core.mcp.discover", "discover_mcp", discover},
		{"agent.core.mcp.list", "list_mcp", list},
	} {
		descriptor := catalogTestDescriptor("agent.skills.v1", test.operation, test.input, result)
		if err := ValidateCatalog(catalogTestWithDigest(t, descriptor), []CatalogRequirement{NewCatalogRequirement(test.action)}); err != nil {
			t.Fatalf("%s current Agent schema rejected: %v", test.action, err)
		}
	}
}

func TestKnowledgeCatalogPinsCurrentAgentSchemaResults(t *testing.T) {
	want := map[string]string{
		"agent.knowledge.config.get":    "87c332e9185a0436d6488041bbfc11cd66c9f40e345af02d9f97a76676cd65ae",
		"agent.knowledge.config.update": "87c332e9185a0436d6488041bbfc11cd66c9f40e345af02d9f97a76676cd65ae",
		"agent.knowledge.search":        "3bff0a96cb6f09421ee1a5ea243b8801a0b61fa4b1f8e01cdf98653acfd99761",
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

func TestArtifactDownloadCatalogPinsExactAgentSchemas(t *testing.T) {
	requirement := NewCatalogRequirement("agent.execution.v2.artifacts.download")
	if got, want := hex.EncodeToString(requirement.InputSchemaDigest), "1f89699ab07b14d135619ee5f6b2ffd0d8d0821fb8f1ba236662814c0586706c"; got != want {
		t.Fatalf("artifact download input schema digest = %s, want %s", got, want)
	}
	if got, want := hex.EncodeToString(requirement.ResultSchemaDigest), "6ea5feead715aa50feeff464e6da618564f9b6e422025c94743faf173478689d"; got != want {
		t.Fatalf("artifact download result schema digest = %s, want %s", got, want)
	}
	if !requirement.RequireSchemaPin {
		t.Fatal("artifact download catalog requirement is not fail-closed")
	}
}

func TestCloudWorkerExecutionCatalogPinsRecordKindInputSchemas(t *testing.T) {
	tests := []struct {
		action, operation, input string
	}{
		{"agent.execution.v2.plans.get", "plans_get", `{"additionalProperties":false,"properties":{"plan_id":{"type":"string"},"record_kind":{"enum":["cloud_worker"],"type":"string"},"revision":{"type":"integer"}},"required":["plan_id"],"type":"object"}`},
		{"agent.execution.v2.plans.list", "plans_list", `{"additionalProperties":false,"properties":{"page_size":{"type":"integer"},"page_token":{"type":"string"},"record_kind":{"enum":["cloud_worker"],"type":"string"}},"type":"object"}`},
		{"agent.execution.v2.runs.get", "runs_get", `{"additionalProperties":false,"properties":{"record_kind":{"enum":["cloud_worker"],"type":"string"},"run_id":{"type":"string"}},"required":["run_id"],"type":"object"}`},
		{"agent.execution.v2.runs.list", "runs_list", `{"additionalProperties":false,"properties":{"deployment_id":{"type":"string"},"page_size":{"type":"integer"},"page_token":{"type":"string"},"project_id":{"type":"string"},"record_kind":{"enum":["cloud_worker"],"type":"string"}},"type":"object"}`},
		{"agent.execution.v2.runs.cancel", "runs_cancel", `{"additionalProperties":false,"properties":{"expected_revision":{"type":"integer"},"idempotency_key":{"type":"string"},"record_kind":{"enum":["cloud_worker"],"type":"string"},"run_id":{"type":"string"}},"required":["run_id","idempotency_key","expected_revision"],"type":"object"}`},
		{"agent.execution.v2.runs.events", "runs_events", `{"additionalProperties":false,"properties":{"after_sequence":{"type":"integer"},"limit":{"type":"integer"},"record_kind":{"enum":["cloud_worker"],"type":"string"},"run_id":{"type":"string"}},"required":["run_id"],"type":"object"}`},
		{"agent.execution.v2.artifacts.get", "artifacts_get", `{"additionalProperties":false,"properties":{"artifact_id":{"type":"string"},"record_kind":{"enum":["cloud_worker"],"type":"string"}},"required":["artifact_id"],"type":"object"}`},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			descriptor := catalogTestDescriptor("agent.execution.v2", test.operation, test.input, `{"type":"object","additionalProperties":true}`)
			catalog := catalogTestWithDigest(t, descriptor)
			requirement := NewCatalogRequirement(test.action)
			// This focused fixture owns the record_kind request boundary only.
			// Result projections are exercised through their typed adapter tests and
			// the live Agent catalog instead of duplicating those large schemas here.
			requirement.ResultSchemaDigest = append([]byte(nil), descriptor.Operations[0].ResultSchemaDigest...)
			if err := ValidateCatalog(catalog, []CatalogRequirement{requirement}); err != nil {
				t.Fatalf("current Agent Cloud Worker schema rejected: %v", err)
			}
		})
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
		},
	}
	catalog := catalogTestWithDigest(t, descriptor)
	wantDigest := map[string]string{"agent.backends.get": "4a0f95cd99ddf917e51efbf74e83f2dd78775f7602437f9afe31df0a25e82d19"}
	for _, action := range []string{"agent.backends.get"} {
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
	streamInput := `{"additionalProperties":false,"type":"object","properties":{"accepted_attachment_ids":{"items":{"format":"uuid","type":"string"},"maxItems":4,"uniqueItems":true,"type":"array"},"idempotency_key":{"format":"uuid","type":"string"},"conversation_id":{"format":"uuid","type":"string"},"message":{"minLength":1,"type":"string"},"model_profile_id":{"format":"uuid","type":"string"},"model_profile_revision":{"minimum":1,"type":"integer"},"credential_version":{"minimum":1,"type":"integer"},"extensions":{"items":{"additionalProperties":false,"properties":{"allowed_tools":{"items":{"maxLength":256,"minLength":1,"type":"string"},"maxItems":64,"minItems":1,"type":"array","uniqueItems":true},"digest":{"pattern":"^[a-f0-9]{64}$","type":"string"},"id":{"format":"uuid","type":"string"},"kind":{"const":"mcp","type":"string"},"pinned_version":{"maxLength":256,"minLength":1,"type":"string"}},"required":["kind","id","pinned_version","digest","allowed_tools"],"type":"object"},"maxItems":64,"minItems":1,"type":"array","uniqueItems":true}},"required":["idempotency_key","message","model_profile_id","model_profile_revision","credential_version"]}`
	for _, forbidden := range [][]byte{[]byte(`"prompt"`), []byte(`"turn_id"`), []byte(`"client_message_id"`), []byte(`"request_id"`)} {
		if bytes.Contains([]byte(streamInput), forbidden) {
			t.Fatalf("pinned Agent chat schema contains unsupported start field %s", forbidden)
		}
	}
	result := `{"type":"object"}`
	streamResult := `{"additionalProperties":false,"properties":{"conversation_id":{"format":"uuid","type":"string"},"done":{"const":true,"type":"boolean"},"idempotency_key":{"format":"uuid","type":"string"},"message":{"type":"object"},"model_profile_id":{"format":"uuid","type":"string"},"references":{"items":{"type":"object"},"type":"array"},"related_plan_ids":{"items":{"format":"uuid","type":"string"},"type":"array"},"related_task_ids":{"items":{"format":"uuid","type":"string"},"type":"array"},"revision":{"minimum":1,"type":"integer"},"tool_results":{"items":{"type":"object"},"type":"array"},"tool_summaries":{"items":{"type":"string"},"type":"array"}},"required":["idempotency_key","conversation_id","revision","message","done","model_profile_id"],"type":"object"}`
	streamEvent := `{"additionalProperties":false,"allOf":[{"if":{"properties":{"kind":{"const":"waiting_confirmation"}}},"then":{"not":{"anyOf":[{"required":["text"]},{"required":["tool_call"]},{"required":["tool_result"]},{"required":["response"]},{"required":["error_code"]},{"required":["error_summary"]}]},"required":["confirmation_id","execution_id","status"]},"else":{"not":{"anyOf":[{"required":["confirmation_id"]},{"required":["execution_id"]},{"required":["status"]}]}}}],"properties":{"confirmation_id":{"format":"uuid","type":"string"},"conversation_id":{"format":"uuid","type":"string"},"error_code":{"type":"string"},"error_summary":{"type":"string"},"execution_id":{"format":"uuid","type":"string"},"idempotency_key":{"format":"uuid","type":"string"},"kind":{"enum":["accepted","started","delta","tool_call","tool_result","waiting_confirmation","done","error"],"type":"string"},"response":` + streamResult + `,"revision":{"minimum":1,"type":"integer"},"status":{"const":"waiting_confirmation","type":"string"},"text":{"type":"string"},"tool_call":{"type":"object"},"tool_result":{"type":"object"},"turn_id":{"format":"uuid","type":"string"}},"required":["kind","idempotency_key","conversation_id","turn_id","revision"],"type":"object"}`
	inputDigest := sha256.Sum256([]byte(input))
	streamInputDigest := sha256.Sum256([]byte(streamInput))
	resultDigest := sha256.Sum256([]byte(result))
	streamResultDigest := sha256.Sum256([]byte(streamResult))
	streamEventDigest := sha256.Sum256([]byte(streamEvent))
	descriptor := &capv1.CapabilityDescriptor{
		CapabilityId:    "agent.chat.v1",
		SemanticVersion: "1.0.0",
		ProtocolVersion: 1,
		Readiness:       true,
		Operations: []*capv1.OperationDescriptor{
			{OperationId: "chat", InputSchemaJson: input, InputSchemaDigest: inputDigest[:], ResultSchemaJson: result, ResultSchemaDigest: resultDigest[:]},
			{OperationId: "stream_chat", InputSchemaJson: streamInput, InputSchemaDigest: streamInputDigest[:], ResultSchemaJson: streamResult, ResultSchemaDigest: streamResultDigest[:], EventSchemaJson: streamEvent, EventSchemaDigest: streamEventDigest[:]},
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
	descriptor.Operations[0].InputSchemaJson = input
	descriptor.Operations[0].InputSchemaDigest = inputDigest[:]
	descriptor.Operations[1].EventSchemaJson = `{"type":"object"}`
	driftedEvent := sha256.Sum256([]byte(descriptor.Operations[1].EventSchemaJson))
	descriptor.Operations[1].EventSchemaDigest = driftedEvent[:]
	catalog.CatalogDigest = computeCatalogDigest(catalog.Capabilities)
	if err := ValidateCatalog(catalog, requirements); err == nil {
		t.Fatal("chat catalog accepted drifted durable event schema")
	}
}

func TestValidateCatalogAcceptsCurrentMutationSchemas(t *testing.T) {
	const objectResult = `{"type":"object"}`
	const knowledgeConfigResult = `{"type":"object","properties":{"embedding_profile_id":{"type":"string"},"embedding_profile_revision":{"type":"integer"},"embedding_model":{"type":"string"},"dimension":{"type":"integer"},"collection":{"type":"string"},"collection_config_digest":{"type":"string"},"revision":{"type":"integer"},"updated_at":{"type":"string"}},"required":["embedding_profile_id","embedding_profile_revision","embedding_model","collection_config_digest","revision"]}`
	tests := []struct {
		action, capability, operation, input, result string
	}{
		{"agent.chat.attachment.begin", "agent.chat.v1", "upload_attachment_begin", `{"additionalProperties":false,"properties":{"content_sha256":{"pattern":"^[a-f0-9]{64}$","type":"string"},"declared_size":{"maximum":8388608,"minimum":1,"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"},"kind":{"enum":["image","file","workspace_archive"],"type":"string"},"mime_type":{"maxLength":255,"minLength":1,"type":"string"},"name":{"maxLength":255,"minLength":1,"type":"string"},"turn_request_id":{"format":"uuid","type":"string"}},"required":["idempotency_key","turn_request_id","kind","name","mime_type","declared_size","content_sha256"],"type":"object"}`, `{"additionalProperties":false,"properties":{"expires_at":{"format":"date-time","type":"string"},"max_chunk_bytes":{"minimum":1,"type":"integer"},"received_size":{"minimum":0,"type":"integer"},"revision":{"minimum":1,"type":"integer"},"source_id":{"format":"uuid","type":"string"},"status":{"enum":["receiving","committed","consumed"],"type":"string"},"turn_request_id":{"format":"uuid","type":"string"},"upload_id":{"format":"uuid","type":"string"}},"required":["upload_id","source_id","turn_request_id","status","received_size","max_chunk_bytes","revision","expires_at"],"type":"object"}`},
		{"agent.chat.attachment.append", "agent.chat.v1", "upload_attachment_append", `{"additionalProperties":false,"properties":{"chunk_sha256":{"pattern":"^[a-f0-9]{64}$","type":"string"},"data_base64":{"maxLength":1398104,"minLength":4,"type":"string"},"expected_revision":{"minimum":1,"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"},"offset_bytes":{"minimum":0,"type":"integer"},"ordinal":{"minimum":0,"type":"integer"},"upload_id":{"format":"uuid","type":"string"}},"required":["idempotency_key","upload_id","expected_revision","ordinal","offset_bytes","data_base64","chunk_sha256"],"type":"object"}`, `{"additionalProperties":false,"properties":{"expires_at":{"format":"date-time","type":"string"},"max_chunk_bytes":{"minimum":1,"type":"integer"},"received_size":{"minimum":0,"type":"integer"},"revision":{"minimum":1,"type":"integer"},"source_id":{"format":"uuid","type":"string"},"status":{"enum":["receiving","committed","consumed"],"type":"string"},"turn_request_id":{"format":"uuid","type":"string"},"upload_id":{"format":"uuid","type":"string"}},"required":["upload_id","source_id","turn_request_id","status","received_size","max_chunk_bytes","revision","expires_at"],"type":"object"}`},
		{"agent.chat.attachment.commit", "agent.chat.v1", "upload_attachment_commit", `{"additionalProperties":false,"properties":{"content_sha256":{"pattern":"^[a-f0-9]{64}$","type":"string"},"expected_revision":{"minimum":1,"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"},"upload_id":{"format":"uuid","type":"string"}},"required":["idempotency_key","upload_id","expected_revision","content_sha256"],"type":"object"}`, `{"additionalProperties":false,"properties":{"expires_at":{"format":"date-time","type":"string"},"kind":{"enum":["image","file","workspace_archive"],"type":"string"},"mime_type":{"maxLength":255,"minLength":1,"type":"string"},"name":{"maxLength":255,"minLength":1,"type":"string"},"revision":{"minimum":1,"type":"integer"},"sha256":{"pattern":"^[a-f0-9]{64}$","type":"string"},"size_bytes":{"maximum":8388608,"minimum":1,"type":"integer"},"source_id":{"format":"uuid","type":"string"},"status":{"const":"committed","type":"string"},"turn_request_id":{"format":"uuid","type":"string"}},"required":["source_id","revision","turn_request_id","kind","name","mime_type","size_bytes","sha256","status","expires_at"],"type":"object"}`},
		{"agent.chat.conversations.create", "agent.chat.v1", "create_conversation", `{"type":"object","properties":{"title":{"type":"string"},"conversation_id":{"type":"string","format":"uuid"},"idempotency_key":{"type":"string","format":"uuid"}},"required":["conversation_id","idempotency_key"]}`, objectResult},
		{"agent.chat.turn.stop", "agent.chat.v1", "stop_turn", `{"additionalProperties":false,"properties":{"expected_revision":{"minimum":1,"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"},"turn_id":{"format":"uuid","type":"string"}},"required":["idempotency_key","turn_id","expected_revision"],"type":"object"}`, `{"additionalProperties":false,"properties":{"conversation_id":{"format":"uuid","type":"string"},"created_at":{"format":"date-time","type":"string"},"idempotency_key":{"format":"uuid","type":"string"},"last_sequence":{"minimum":0,"type":"integer"},"revision":{"minimum":1,"type":"integer"},"state":{"enum":["accepted","running","waiting_confirmation","completed","canceled","failed"],"type":"string"},"terminal_code":{"type":"string"},"terminal_summary":{"type":"string"},"turn_id":{"format":"uuid","type":"string"},"updated_at":{"format":"date-time","type":"string"}},"required":["turn_id","idempotency_key","conversation_id","state","revision","last_sequence","terminal_code","terminal_summary","created_at","updated_at"],"type":"object"}`},
		{"agent.knowledge.config.update", "agent.knowledge.v1", "update_config", `{"additionalProperties":false,"properties":{"collection":{"type":"string"},"collection_config_digest":{"type":"string"},"dimension":{"type":"integer"},"embedding_profile_id":{"format":"uuid","type":"string"},"expected_revision":{"minimum":1,"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["idempotency_key","expected_revision"],"type":"object"}`, knowledgeConfigResult},
		{"agent.knowledge.sources.delete", "agent.knowledge.v1", "delete_source", `{"type":"object","properties":{"source_id":{"type":"string"},"expected_revision":{"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["source_id","expected_revision","idempotency_key"]}`, objectResult},
		{"agent.knowledge.upload.start", "agent.knowledge.v1", "start_upload", `{"type":"object","properties":{"upload_id":{"type":"string"},"source_id":{"type":"string"},"title":{"type":"string"},"relative_path":{"type":"string"},"media_type":{"type":"string"},"declared_size":{"type":"integer"},"content_sha256":{"type":"string"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["declared_size","content_sha256","idempotency_key"]}`, objectResult},
		{"agent.knowledge.upload.chunk", "agent.knowledge.v1", "append_upload_chunk", `{"type":"object","properties":{"upload_id":{"type":"string"},"ordinal":{"type":"integer"},"offset_bytes":{"type":"integer"},"data":{"type":"string"},"chunk_sha256":{"type":"string"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["upload_id","data","chunk_sha256","idempotency_key"]}`, objectResult},
		{"agent.knowledge.upload.finish", "agent.knowledge.v1", "commit_upload", `{"additionalProperties":false,"properties":{"content_sha256":{"pattern":"^[a-f0-9]{64}$","type":"string"},"expected_revision":{"minimum":1,"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"},"upload_id":{"format":"uuid","type":"string"}},"required":["upload_id","expected_revision","content_sha256","idempotency_key"],"type":"object"}`, objectResult},
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
