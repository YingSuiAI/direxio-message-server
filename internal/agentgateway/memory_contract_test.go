package agentgateway

import (
	"errors"
	"testing"
)

const (
	memoryEmptyInputSchema  = `{"additionalProperties":false,"properties":{},"type":"object"}`
	memoryUpdateInputSchema = `{"additionalProperties":false,"properties":{"enabled":{"type":"boolean"},"expected_revision":{"minimum":0,"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["idempotency_key","expected_revision","enabled"],"type":"object"}`
	memoryConfigSchema      = `{"additionalProperties":false,"properties":{"embedding_configured":{"type":"boolean"},"embedding_model":{"type":"string"},"embedding_profile_id":{"format":"uuid","type":"string"},"enabled":{"type":"boolean"},"revision":{"minimum":0,"type":"integer"},"updated_at":{"format":"date-time","type":"string"}},"required":["enabled","embedding_configured","revision"],"type":"object"}`
	memoryStatusSchema      = `{"additionalProperties":false,"properties":{"active_fact_count":{"minimum":0,"type":"integer"},"embedding_configured":{"type":"boolean"},"embedding_model":{"type":"string"},"embedding_profile_id":{"format":"uuid","type":"string"},"enabled":{"type":"boolean"},"facts":{"items":{"additionalProperties":false,"properties":{"confidence":{"maximum":1,"minimum":0,"type":"number"},"id":{"format":"uuid","type":"string"},"kind":{"enum":["identity","preference","relationship","goal","constraint","context","fact"],"type":"string"},"last_confirmed_at":{"format":"date-time","type":"string"},"predicate":{"type":"string"},"subject":{"enum":["user"],"type":"string"},"valid_from":{"format":"date-time","type":"string"},"value":{"type":"string"}},"required":["id","subject","predicate","value","kind","confidence","valid_from","last_confirmed_at"],"type":"object"},"type":"array"},"failed_observation_count":{"minimum":0,"type":"integer"},"pending_observation_count":{"minimum":0,"type":"integer"},"revision":{"minimum":0,"type":"integer"},"timeline":{"items":{"additionalProperties":false,"properties":{"effective_at":{"format":"date-time","type":"string"},"kind":{"enum":["added","confirmed","replaced","retracted"],"type":"string"},"observed_at":{"format":"date-time","type":"string"},"summary":{"type":"string"}},"required":["kind","summary","effective_at","observed_at"],"type":"object"},"type":"array"},"timeline_event_count":{"minimum":0,"type":"integer"},"updated_at":{"format":"date-time","type":"string"}},"required":["enabled","embedding_configured","revision","active_fact_count","timeline_event_count","pending_observation_count","failed_observation_count","facts","timeline"],"type":"object"}`
	memoryFactSchema        = `{"additionalProperties":false,"properties":{"confidence":{"maximum":1,"minimum":0,"type":"number"},"id":{"format":"uuid","type":"string"},"kind":{"enum":["identity","preference","relationship","goal","constraint","context","fact"],"type":"string"},"last_confirmed_at":{"format":"date-time","type":"string"},"predicate":{"type":"string"},"subject":{"enum":["user"],"type":"string"},"valid_from":{"format":"date-time","type":"string"},"value":{"type":"string"}},"required":["id","subject","predicate","value","kind","confidence","valid_from","last_confirmed_at"],"type":"object"}`
	memoryFactUpdateInput   = `{"additionalProperties":false,"properties":{"fact_id":{"format":"uuid","type":"string"},"idempotency_key":{"format":"uuid","type":"string"},"value":{"maxLength":2048,"minLength":1,"type":"string"}},"required":["fact_id","idempotency_key","value"],"type":"object"}`
	memoryFactDeleteInput   = `{"additionalProperties":false,"properties":{"fact_id":{"format":"uuid","type":"string"},"idempotency_key":{"format":"uuid","type":"string"}},"required":["fact_id","idempotency_key"],"type":"object"}`
	memoryFactDeleteResult  = `{"additionalProperties":false,"properties":{"deleted":{"const":true,"type":"boolean"},"fact_id":{"format":"uuid","type":"string"}},"required":["fact_id","deleted"],"type":"object"}`
)

func TestMemoryBindingsAndPinnedCatalogSchemas(t *testing.T) {
	tests := []struct{ action, operation, input, result string }{
		{"agent.memory.config.get", "get_config", memoryEmptyInputSchema, memoryConfigSchema},
		{"agent.memory.config.update", "update_config", memoryUpdateInputSchema, memoryConfigSchema},
		{"agent.memory.status", "status", memoryEmptyInputSchema, memoryStatusSchema},
		{"agent.memory.facts.update", "update_fact", memoryFactUpdateInput, memoryFactSchema},
		{"agent.memory.facts.delete", "delete_fact", memoryFactDeleteInput, memoryFactDeleteResult},
	}
	for _, test := range tests {
		binding, ok := actionBindingFor(test.action)
		if !ok || binding.capabilityID != "agent.memory.v1" || binding.operation != test.operation {
			t.Fatalf("%s binding = %#v", test.action, binding)
		}
		descriptor := catalogTestDescriptor("agent.memory.v1", test.operation, test.input, test.result)
		if err := ValidateCatalog(catalogTestWithDigest(t, descriptor), []CatalogRequirement{NewCatalogRequirement(test.action)}); err != nil {
			t.Fatalf("%s schema pin rejected canonical descriptor: %v", test.action, err)
		}
	}
}

func TestMemoryRequestValidationIsClosed(t *testing.T) {
	valid := map[string]any{"idempotency_key": "11111111-1111-4111-8111-111111111111", "expected_revision": float64(0), "enabled": true}
	for _, action := range []string{"agent.memory.config.get", "agent.memory.status"} {
		if err := ValidateActionRequest(action, map[string]any{}); err != nil {
			t.Fatalf("%s rejected empty input: %v", action, err)
		}
		if err := ValidateActionRequest(action, map[string]any{"extra": true}); !errors.Is(err, ErrInvalidActionRequest) {
			t.Fatalf("%s accepted unknown input: %v", action, err)
		}
	}
	if err := ValidateActionRequest("agent.memory.config.update", valid); err != nil {
		t.Fatalf("valid memory update rejected: %v", err)
	}
	invalid := []map[string]any{
		{"expected_revision": float64(0), "enabled": true},
		{"idempotency_key": valid["idempotency_key"], "enabled": true},
		{"idempotency_key": valid["idempotency_key"], "expected_revision": float64(0)},
		{"idempotency_key": valid["idempotency_key"], "expected_revision": float64(-1), "enabled": true},
		{"idempotency_key": valid["idempotency_key"], "expected_revision": float64(0), "enabled": "true"},
		{"idempotency_key": valid["idempotency_key"], "expected_revision": float64(0), "enabled": true, "extra": true},
	}
	for _, params := range invalid {
		if err := ValidateActionRequest("agent.memory.config.update", params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Fatalf("invalid memory update accepted: %#v error=%v", params, err)
		}
	}
}

func TestMemoryFactMutationRequestValidationIsClosed(t *testing.T) {
	factID := "33333333-3333-4333-8333-333333333333"
	idempotencyKey := "11111111-1111-4111-8111-111111111111"
	if err := ValidateActionRequest("agent.memory.facts.update", map[string]any{"fact_id": factID, "idempotency_key": idempotencyKey, "value": "architect"}); err != nil {
		t.Fatalf("valid fact update rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.memory.facts.delete", map[string]any{"fact_id": factID, "idempotency_key": idempotencyKey}); err != nil {
		t.Fatalf("valid fact delete rejected: %v", err)
	}
	for _, test := range []struct {
		action string
		params map[string]any
	}{
		{"agent.memory.facts.update", map[string]any{"fact_id": factID, "idempotency_key": idempotencyKey, "value": ""}},
		{"agent.memory.facts.update", map[string]any{"fact_id": factID, "idempotency_key": idempotencyKey, "value": " architect"}},
		{"agent.memory.facts.update", map[string]any{"fact_id": factID, "idempotency_key": idempotencyKey, "value": "architect", "extra": true}},
		{"agent.memory.facts.delete", map[string]any{"fact_id": "bad", "idempotency_key": idempotencyKey}},
		{"agent.memory.facts.delete", map[string]any{"fact_id": factID}},
	} {
		if err := ValidateActionRequest(test.action, test.params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Fatalf("invalid %s request accepted: %#v error=%v", test.action, test.params, err)
		}
	}
}

func TestMemoryResultValidationAndProjectionAreClosed(t *testing.T) {
	valid := map[string]any{
		"enabled": true, "embedding_configured": true,
		"embedding_profile_id": "22222222-2222-4222-8222-222222222222", "embedding_model": "text-embedding-3-small",
		"revision": float64(3), "updated_at": "2026-08-12T08:00:00Z",
		"active_fact_count": float64(1), "timeline_event_count": float64(1), "pending_observation_count": float64(0), "failed_observation_count": float64(0),
		"facts": []any{map[string]any{
			"id": "33333333-3333-4333-8333-333333333333", "subject": "user", "predicate": "occupation", "value": "designer", "kind": "identity", "confidence": float64(.9),
			"valid_from": "2026-08-12T07:00:00Z", "last_confirmed_at": "2026-08-12T08:00:00Z",
		}},
		"timeline": []any{map[string]any{"kind": "added", "summary": "user.occupation = designer", "effective_at": "2026-08-12T07:00:00Z", "observed_at": "2026-08-12T08:00:00Z"}},
	}
	projected, err := adaptActionResult("agent.memory.status", valid)
	if err != nil || projected["enabled"] != true {
		t.Fatalf("valid memory status rejected: result=%#v err=%v", projected, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"top-level extra":        func(value map[string]any) { value["credential"] = "drop" },
		"malformed fact":         func(value map[string]any) { value["facts"] = []any{map[string]any{"id": "bad"}} },
		"fact extra":             func(value map[string]any) { value["facts"].([]any)[0].(map[string]any)["internal"] = true },
		"malformed timeline":     func(value map[string]any) { value["timeline"] = []any{map[string]any{"kind": "other"}} },
		"timeline extra":         func(value map[string]any) { value["timeline"].([]any)[0].(map[string]any)["internal"] = true },
		"inconsistent embedding": func(value map[string]any) { value["embedding_configured"] = false },
	} {
		t.Run(name, func(t *testing.T) {
			value := cloneParams(valid)
			mutate(value)
			if _, err := adaptActionResult("agent.memory.status", value); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid memory status accepted: %v", err)
			}
		})
	}
}

func TestMemoryFactMutationResultValidationAndProjectionAreClosed(t *testing.T) {
	fact := map[string]any{
		"id": "33333333-3333-4333-8333-333333333333", "subject": "user", "predicate": "occupation", "value": "architect", "kind": "identity", "confidence": float64(.9),
		"valid_from": "2026-08-12T07:00:00Z", "last_confirmed_at": "2026-08-12T08:00:00Z",
	}
	if got, err := adaptActionResult("agent.memory.facts.update", fact); err != nil || got["value"] != "architect" {
		t.Fatalf("valid fact update result rejected: result=%#v err=%v", got, err)
	}
	if got, err := adaptActionResult("agent.memory.facts.delete", map[string]any{"fact_id": fact["id"], "deleted": true}); err != nil || got["deleted"] != true {
		t.Fatalf("valid fact delete result rejected: result=%#v err=%v", got, err)
	}
	invalidFact := cloneParams(fact)
	invalidFact["internal"] = true
	if _, err := adaptActionResult("agent.memory.facts.update", invalidFact); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("fact update retained unknown result field: %v", err)
	}
	if _, err := adaptActionResult("agent.memory.facts.delete", map[string]any{"fact_id": fact["id"], "deleted": false}); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("fact delete accepted deleted=false: %v", err)
	}
}
