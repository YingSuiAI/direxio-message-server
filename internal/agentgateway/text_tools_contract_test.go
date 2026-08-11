package agentgateway

import (
	"errors"
	"strings"
	"testing"
)

const (
	textToolsEmptyInputSchema    = `{"additionalProperties":false,"properties":{},"type":"object"}`
	textToolsUpdateInputSchema   = `{"additionalProperties":false,"properties":{"enabled":{"type":"boolean"},"expected_revision":{"minimum":0,"type":"integer"},"idempotency_key":{"format":"uuid","type":"string"},"tools":{"items":{"additionalProperties":false,"properties":{"enabled":{"type":"boolean"},"name":{"maxLength":64,"minLength":1,"type":"string"},"order":{"minimum":0,"type":"integer"},"system_prompt":{"maxLength":16384,"minLength":1,"type":"string"},"tool_id":{"anyOf":[{"enum":["translation","summary","explanation","search"],"type":"string"},{"format":"uuid","type":"string"}]}},"required":["tool_id","name","system_prompt","order","enabled"],"type":"object"},"maxItems":32,"type":"array"}},"required":["idempotency_key","expected_revision","enabled","tools"],"type":"object"}`
	textToolsConfigResultSchema  = `{"additionalProperties":false,"properties":{"enabled":{"type":"boolean"},"revision":{"minimum":0,"type":"integer"},"tools":{"items":{"additionalProperties":false,"properties":{"enabled":{"type":"boolean"},"name":{"maxLength":64,"minLength":1,"type":"string"},"order":{"minimum":0,"type":"integer"},"system_prompt":{"maxLength":16384,"minLength":1,"type":"string"},"tool_id":{"anyOf":[{"enum":["translation","summary","explanation","search"],"type":"string"},{"format":"uuid","type":"string"}]}},"required":["tool_id","name","system_prompt","order","enabled"],"type":"object"},"maxItems":32,"type":"array"},"updated_at":{"format":"date-time","type":"string"}},"required":["enabled","revision","tools","updated_at"],"type":"object"}`
	textToolsExecuteInputSchema  = `{"additionalProperties":false,"properties":{"output_language":{"enum":["zh","en"],"type":"string"},"selected_text":{"maxLength":65536,"minLength":1,"type":"string"},"tool_id":{"anyOf":[{"enum":["translation","summary","explanation","search"],"type":"string"},{"format":"uuid","type":"string"}]}},"required":["tool_id","selected_text","output_language"],"type":"object"}`
	textToolsExecuteResultSchema = `{"additionalProperties":false,"properties":{"output":{"maxLength":65536,"minLength":1,"type":"string"},"sources":{"items":{"additionalProperties":false,"properties":{"snippet":{"maxLength":4096,"type":"string"},"title":{"maxLength":512,"minLength":1,"type":"string"},"url":{"maxLength":8192,"minLength":1,"type":"string"}},"required":["title","url","snippet"],"type":"object"},"maxItems":5,"type":"array"},"tool_id":{"anyOf":[{"enum":["translation","summary","explanation","search"],"type":"string"},{"format":"uuid","type":"string"}]}},"required":["tool_id","output","sources"],"type":"object"}`
)

func TestTextToolsBindingsAndPinnedCatalogSchemas(t *testing.T) {
	tests := []struct {
		action, operation, input, result string
	}{
		{"agent.text_tools.config.get", "get_config", textToolsEmptyInputSchema, textToolsConfigResultSchema},
		{"agent.text_tools.config.update", "update_config", textToolsUpdateInputSchema, textToolsConfigResultSchema},
		{"agent.text_tools.execute", "execute", textToolsExecuteInputSchema, textToolsExecuteResultSchema},
	}
	for _, test := range tests {
		binding, ok := actionBindingFor(test.action)
		if !ok || binding.capabilityID != "agent.text_tools.v1" || binding.operation != test.operation {
			t.Fatalf("%s binding = %#v", test.action, binding)
		}
		descriptor := catalogTestDescriptor("agent.text_tools.v1", test.operation, test.input, test.result)
		catalog := catalogTestWithDigest(t, descriptor)
		if err := ValidateCatalog(catalog, []CatalogRequirement{NewCatalogRequirement(test.action)}); err != nil {
			t.Fatalf("%s schema pin rejected canonical descriptor: %v", test.action, err)
		}
	}
}

func TestTextToolsRequestValidationIsClosedAndBounded(t *testing.T) {
	validTools := []any{
		map[string]any{"tool_id": "translation", "name": "Translate", "system_prompt": "Translate selected text", "order": float64(0), "enabled": true},
		map[string]any{"tool_id": "11111111-1111-4111-8111-111111111111", "name": "Custom", "system_prompt": "Explain", "order": float64(1), "enabled": false},
	}
	validUpdate := map[string]any{"idempotency_key": "22222222-2222-4222-8222-222222222222", "expected_revision": float64(0), "enabled": true, "tools": validTools}
	if err := ValidateActionRequest("agent.text_tools.config.get", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateActionRequest("agent.text_tools.config.update", validUpdate); err != nil {
		t.Fatalf("valid update rejected: %v", err)
	}
	for _, outputLanguage := range []string{"zh", "en"} {
		if err := ValidateActionRequest("agent.text_tools.execute", map[string]any{"tool_id": "search", "selected_text": "query", "output_language": outputLanguage}); err != nil {
			t.Fatalf("valid %s execute rejected: %v", outputLanguage, err)
		}
	}

	invalid := []struct {
		action string
		params map[string]any
	}{
		{"agent.text_tools.config.get", map[string]any{"prompt": "no"}},
		{"agent.text_tools.config.update", map[string]any{"idempotency_key": validUpdate["idempotency_key"], "expected_revision": float64(0), "enabled": true, "tools": []any{map[string]any{"tool_id": "translation", "name": "x", "system_prompt": "x", "order": float64(1), "enabled": true}}}},
		{"agent.text_tools.config.update", map[string]any{"idempotency_key": validUpdate["idempotency_key"], "expected_revision": float64(0), "enabled": true, "tools": []any{map[string]any{"tool_id": "translation", "name": "x", "system_prompt": "x", "order": float64(0), "enabled": true, "api_key": "secret"}}}},
		{"agent.text_tools.execute", map[string]any{"tool_id": "search", "selected_text": "query"}},
		{"agent.text_tools.execute", map[string]any{"tool_id": "search", "selected_text": "query", "output_language": "fr"}},
		{"agent.text_tools.execute", map[string]any{"tool_id": "search", "selected_text": "query", "output_language": "ZH"}},
		{"agent.text_tools.execute", map[string]any{"tool_id": "search", "selected_text": "query", "output_language": true}},
		{"agent.text_tools.execute", map[string]any{"tool_id": "search", "selected_text": "query", "model_profile_id": "forbidden"}},
		{"agent.text_tools.execute", map[string]any{"tool_id": "search", "selected_text": strings.Repeat("界", 21846), "output_language": "en"}},
	}
	enabledTools := make([]any, 7)
	for index := range enabledTools {
		enabledTools[index] = map[string]any{
			"tool_id": "11111111-1111-4111-8111-" + string(rune('0'+index)) + "11111111111",
			"name":    "x", "system_prompt": "x", "order": float64(index), "enabled": true,
		}
	}
	invalid = append(invalid, struct {
		action string
		params map[string]any
	}{"agent.text_tools.config.update", map[string]any{
		"idempotency_key": validUpdate["idempotency_key"], "expected_revision": float64(0), "enabled": true, "tools": enabledTools,
	}})
	for _, test := range invalid {
		if err := ValidateActionRequest(test.action, test.params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("%s invalid request error = %v", test.action, err)
		}
	}
}

func TestTextToolsResultProjectionRejectsAliasesExtrasAndDrift(t *testing.T) {
	request := map[string]any{"tool_id": "search", "selected_text": "query", "output_language": "en"}
	valid := map[string]any{
		"tool_id": "search", "output": "answer", "sources": []any{
			map[string]any{"title": "result", "url": "https://example.test", "snippet": ""},
		},
	}
	_, err := adaptActionResultForRequest("agent.text_tools.execute", request, valid)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryExtra := cloneParams(valid)
	ordinaryExtra["diagnostic"] = "must-be-rejected"
	if _, err := adaptActionResultForRequest("agent.text_tools.execute", request, ordinaryExtra); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("ordinary extra field error = %v", err)
	}
	sourceExtra := cloneParams(valid)
	sourceExtra["sources"] = []any{map[string]any{"title": "result", "url": "https://example.test", "snippet": "", "rank": float64(1)}}
	if _, err := adaptActionResultForRequest("agent.text_tools.execute", request, sourceExtra); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("ordinary source extra field error = %v", err)
	}
	camelCase := map[string]any{"ToolID": "search", "Output": "answer", "Sources": []any{}}
	if _, err := adaptActionResultForRequest("agent.text_tools.execute", request, camelCase); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("CamelCase execution result error = %v", err)
	}

	secret := cloneParams(valid)
	secret["sources"] = []any{map[string]any{"title": "result", "url": "https://example.test", "snippet": "", "api_key": "must-not-leak"}}
	if _, err := adaptActionResultForRequest("agent.text_tools.execute", request, secret); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("secret-like source field error = %v", err)
	}
	nestedSecret := cloneParams(valid)
	nestedSecret["diagnostic"] = map[string]any{"credential": "must-not-leak"}
	if _, err := adaptActionResultForRequest("agent.text_tools.execute", request, nestedSecret); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("nested secret-like field error = %v", err)
	}
	oversized := cloneParams(valid)
	oversized["output"] = strings.Repeat("x", maxTextToolOutputBytes+1)
	if _, err := adaptActionResultForRequest("agent.text_tools.execute", request, oversized); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("oversized output error = %v", err)
	}
	drifted := cloneParams(valid)
	drifted["tool_id"] = "summary"
	if _, err := adaptActionResultForRequest("agent.text_tools.execute", request, drifted); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("drifted tool id error = %v", err)
	}
}

func TestTextToolsConfigResultRequiresContiguousFullList(t *testing.T) {
	valid := map[string]any{
		"enabled": true, "revision": float64(2), "updated_at": "2026-08-08T10:00:00Z",
		"tools": []any{map[string]any{"tool_id": "summary", "name": "Summary", "system_prompt": "Summarize", "order": float64(0), "enabled": true}},
	}
	if _, err := adaptActionResult("agent.text_tools.config.get", valid); err != nil {
		t.Fatal(err)
	}
	extra := cloneParams(valid)
	extra["status"] = "ready"
	if _, err := adaptActionResult("agent.text_tools.config.get", extra); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("config extra field error = %v", err)
	}
	camelCase := map[string]any{"Enabled": true, "Revision": float64(2), "UpdatedAt": "2026-08-08T10:00:00Z", "Tools": []any{}}
	if _, err := adaptActionResult("agent.text_tools.config.get", camelCase); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("CamelCase config result error = %v", err)
	}
	invalid := cloneParams(valid)
	invalid["tools"] = []any{map[string]any{"tool_id": "summary", "name": "Summary", "system_prompt": "Summarize", "order": float64(1), "enabled": true}}
	if _, err := adaptActionResult("agent.text_tools.config.get", invalid); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("non-contiguous config error = %v", err)
	}
	enabledTools := make([]any, 7)
	for index := range enabledTools {
		enabledTools[index] = map[string]any{
			"tool_id": "11111111-1111-4111-8111-" + string(rune('0'+index)) + "11111111111",
			"name":    "x", "system_prompt": "x", "order": float64(index), "enabled": true,
		}
	}
	invalid["tools"] = enabledTools
	if _, err := adaptActionResult("agent.text_tools.config.get", invalid); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("too many enabled tools result error = %v", err)
	}
}
