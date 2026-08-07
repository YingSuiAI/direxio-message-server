package agentgateway

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCloudWorkerConfirmationActionsUseStrictPurposeOnlyGoldenProjection(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)
	for _, action := range []string{
		"agent.core.confirmations.get",
		"agent.core.confirmations.list",
		"agent.core.confirmations.confirm",
		"agent.core.confirmations.reject",
	} {
		t.Run(action, func(t *testing.T) {
			output := cloudWorkerConfirmationActionOutput(action, fixture.Confirmation)
			request := cloudWorkerConfirmationActionRequest(action, fixture.Confirmation)
			got, err := adaptActionResultForRequestWithAuthority(action, request, output, authority)
			if err != nil {
				t.Fatalf("golden confirmation rejected: %v", err)
			}
			confirmation := cloudWorkerProjectedConfirmation(t, action, got)
			if !reflect.DeepEqual(confirmation, fixture.Confirmation) {
				t.Fatalf("confirmation projection=%#v want golden=%#v", confirmation, fixture.Confirmation)
			}
			binding := confirmation["binding"].(map[string]any)
			for _, raw := range binding["secret_grants"].([]any) {
				grant := raw.(map[string]any)
				if keys := sortedMapKeys(grant); !reflect.DeepEqual(keys, []string{"purpose"}) {
					t.Fatalf("Cloud Worker secret grant keys=%v", keys)
				}
			}
		})
	}
}

func TestCloudWorkerConfirmationActionsFailClosedOnSecretLocatorAndAuthorityDrift(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)
	actions := []string{
		"agent.core.confirmations.get",
		"agent.core.confirmations.list",
		"agent.core.confirmations.confirm",
		"agent.core.confirmations.reject",
	}
	mutations := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"reference_id leak", func(confirmation map[string]any) {
			cloudWorkerConfirmationGrant(confirmation)["reference_id"] = "private-reference-canary"
		}},
		{"binding_digest leak", func(confirmation map[string]any) {
			cloudWorkerConfirmationGrant(confirmation)["binding_digest"] = strings.Repeat("f", 64)
		}},
		{"duplicate purpose", func(confirmation map[string]any) {
			binding := confirmation["binding"].(map[string]any)
			binding["secret_grants"] = []any{map[string]any{"purpose": "model_api_key"}, map[string]any{"purpose": "model_api_key"}}
		}},
		{"missing plan digest", func(confirmation map[string]any) {
			delete(confirmation["binding"].(map[string]any), "plan_digest")
		}},
		{"operation drift", func(confirmation map[string]any) {
			confirmation["binding"].(map[string]any)["operation_domain"] = "extension.execute"
		}},
		{"foreign outer owner", func(confirmation map[string]any) {
			confirmation["owner_id"] = "@foreign:example.test"
		}},
		{"foreign binding owner", func(confirmation map[string]any) {
			confirmation["binding"].(map[string]any)["owner_id"] = "@foreign:example.test"
		}},
		{"stale account generation", func(confirmation map[string]any) {
			confirmation["binding"].(map[string]any)["account_generation"] = float64(authority.accountGeneration - 1)
		}},
		{"plan revision mismatch", func(confirmation map[string]any) {
			confirmation["binding"].(map[string]any)["target_revision"] = float64(2)
		}},
		{"command injection", func(confirmation map[string]any) {
			confirmation["binding"].(map[string]any)["selected_command"] = []any{"pi", "--unsafe"}
		}},
		{"pending updated after expiry", func(confirmation map[string]any) {
			confirmation["state"] = "pending"
			confirmation["updated_at"] = "2026-08-07T10:16:00.123456Z"
		}},
	}
	for _, action := range actions {
		for _, mutation := range mutations {
			t.Run(action+"/"+mutation.name, func(t *testing.T) {
				confirmation := cloneCloudWorkerConfirmation(t, fixture.Confirmation)
				mutation.mutate(confirmation)
				request := cloudWorkerConfirmationActionRequest(action, confirmation)
				_, err := adaptActionResultForRequestWithAuthority(action, request, cloudWorkerConfirmationActionOutput(action, confirmation), authority)
				if !errors.Is(err, ErrInvalidActionResult) {
					t.Fatalf("drift accepted: %v", err)
				}
				if strings.Contains(err.Error(), "private-reference-canary") {
					t.Fatal("Cloud Worker result error reflected a private reference value")
				}
			})
		}
	}

	for _, action := range actions {
		request := cloudWorkerConfirmationActionRequest(action, fixture.Confirmation)
		_, err := adaptActionResultForRequestWithAuthority(action, request, cloudWorkerConfirmationActionOutput(action, fixture.Confirmation), actionResultAuthority{})
		if !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("%s accepted missing prepared authority: %v", action, err)
		}
	}
}

func TestCloudWorkerConfirmationActionsAcceptAuthoritativeExpiredState(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)
	confirmation := cloneCloudWorkerConfirmation(t, fixture.Confirmation)
	confirmation["state"] = "expired"
	confirmation["updated_at"] = "2026-08-07T10:16:00.123456Z"
	confirmation["terminal_code"] = "confirmation_expired"
	confirmation["terminal_reason"] = "quote_expired"

	for _, action := range []string{
		"agent.core.confirmations.get",
		"agent.core.confirmations.list",
		"agent.core.confirmations.confirm",
		"agent.core.confirmations.reject",
	} {
		t.Run(action, func(t *testing.T) {
			request := cloudWorkerConfirmationActionRequest(action, confirmation)
			if _, err := adaptActionResultForRequestWithAuthority(action, request, cloudWorkerConfirmationActionOutput(action, confirmation), authority); err != nil {
				t.Fatalf("authoritative expired confirmation rejected: %v", err)
			}
		})
	}
}

func TestCloudWorkerConfirmationActionsRejectResponseSubstitutionAndListDrift(t *testing.T) {
	fixture := loadCloudWorkerPublicFixture(t)
	authority := cloudWorkerFixtureAuthority(t, fixture)
	for _, action := range []string{
		"agent.core.confirmations.get",
		"agent.core.confirmations.confirm",
		"agent.core.confirmations.reject",
	} {
		t.Run(action+"/confirmation_id", func(t *testing.T) {
			request := cloudWorkerConfirmationActionRequest(action, fixture.Confirmation)
			request["confirmation_id"] = "11111111-1111-4111-8111-111111111111"
			_, err := adaptActionResultForRequestWithAuthority(action, request, cloudWorkerConfirmationActionOutput(action, fixture.Confirmation), authority)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("same-owner confirmation substitution accepted: %v", err)
			}
		})
	}

	listDrifts := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"operation_domain", func(request map[string]any) { request["operation_domain"] = "extension.execute" }},
		{"target_id", func(request map[string]any) { request["target_id"] = "11111111-1111-4111-8111-111111111111" }},
		{"state", func(request map[string]any) { request["states"] = []any{"pending"} }},
	}
	for _, drift := range listDrifts {
		t.Run("list/"+drift.name, func(t *testing.T) {
			request := cloudWorkerConfirmationActionRequest("agent.core.confirmations.list", fixture.Confirmation)
			drift.mutate(request)
			_, err := adaptActionResultForRequestWithAuthority("agent.core.confirmations.list", request, cloudWorkerConfirmationActionOutput("agent.core.confirmations.list", fixture.Confirmation), authority)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("list filter drift accepted: %v", err)
			}
		})
	}
}

func TestGenericConfirmationSecretDescriptorsRemainUnchanged(t *testing.T) {
	generic := map[string]any{
		"confirmation_id": "11111111-1111-4111-8111-111111111111",
		"owner_id":        "@owner:example.test",
		"task_id":         "22222222-2222-4222-8222-222222222222",
		"state":           "pending",
		"revision":        float64(1),
		"created_at":      "2026-08-07T10:00:00Z",
		"updated_at":      "2026-08-07T10:00:00Z",
		"expires_at":      "2026-08-07T10:15:00Z",
		"terminal_code":   "",
		"terminal_note":   "",
		"terminal_reason": "",
		"binding": map[string]any{
			"owner_id": "@owner:example.test", "operation_domain": "mcp.execute",
			"target_id": "server-1", "target_revision": float64(1),
			"secret_grants": []any{map[string]any{
				"reference_id": "33333333-3333-4333-8333-333333333333",
				"purpose":      "tool_api_key", "binding_digest": strings.Repeat("a", 64),
			}},
		},
	}
	request := map[string]any{"confirmation_id": generic["confirmation_id"]}
	got, err := adaptActionResultForRequest("agent.core.confirmations.get", request, map[string]any{"confirmation": generic})
	if err != nil {
		t.Fatalf("generic confirmation rejected: %v", err)
	}
	if !reflect.DeepEqual(got["confirmation"], generic) {
		t.Fatalf("generic confirmation projection changed: %#v", got)
	}
	request["confirmation_id"] = "44444444-4444-4444-8444-444444444444"
	if _, err := adaptActionResultForRequest("agent.core.confirmations.get", request, map[string]any{"confirmation": generic}); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("generic confirmation substitution accepted: %v", err)
	}
}

func cloudWorkerConfirmationActionOutput(action string, confirmation map[string]any) map[string]any {
	if action == "agent.core.confirmations.list" {
		return map[string]any{"confirmations": []any{confirmation}, "next_page_token": ""}
	}
	return map[string]any{"confirmation": confirmation}
}

func cloudWorkerConfirmationActionRequest(action string, confirmation map[string]any) map[string]any {
	binding := confirmation["binding"].(map[string]any)
	if action == "agent.core.confirmations.list" {
		return map[string]any{
			"operation_domain": binding["operation_domain"],
			"target_id":        binding["target_id"],
			"states":           []any{confirmation["state"]},
		}
	}
	request := map[string]any{"confirmation_id": confirmation["confirmation_id"]}
	if action == "agent.core.confirmations.confirm" || action == "agent.core.confirmations.reject" {
		request["expected_revision"] = confirmation["revision"]
		request["idempotency_key"] = "11111111-1111-4111-8111-111111111111"
	}
	return request
}

func cloudWorkerProjectedConfirmation(t *testing.T, action string, output map[string]any) map[string]any {
	t.Helper()
	if action == "agent.core.confirmations.list" {
		return output["confirmations"].([]any)[0].(map[string]any)
	}
	return output["confirmation"].(map[string]any)
}

func cloudWorkerConfirmationGrant(confirmation map[string]any) map[string]any {
	return confirmation["binding"].(map[string]any)["secret_grants"].([]any)[0].(map[string]any)
}

func cloneCloudWorkerConfirmation(t *testing.T, confirmation map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}
