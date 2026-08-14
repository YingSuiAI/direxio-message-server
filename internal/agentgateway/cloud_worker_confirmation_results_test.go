package agentgateway

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCloudWorkerConfirmationActionsUseIdentityRevisionAndQuoteProjection(t *testing.T) {
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
			want := []string{"account_generation", "execution_id", "operation_domain", "owner_id", "plan_id", "plan_revision", "quote", "target_id", "target_revision"}
			if keys := sortedMapKeys(binding); !reflect.DeepEqual(keys, want) {
				t.Fatalf("Cloud Worker binding keys=%v want=%v", keys, want)
			}
		})
	}
}

func TestCloudWorkerConfirmationActionsFailClosedOnRetiredFieldsAndAuthorityDrift(t *testing.T) {
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
		{"retired digest", func(confirmation map[string]any) {
			confirmation["binding"].(map[string]any)["digest"] = strings.Repeat("f", 64)
		}},
		{"retired recipe", func(confirmation map[string]any) {
			confirmation["binding"].(map[string]any)["target_kind"] = "ephemeral_pi_worker"
		}},
		{"missing plan revision", func(confirmation map[string]any) {
			delete(confirmation["binding"].(map[string]any), "plan_revision")
		}},
		{"missing quote", func(confirmation map[string]any) {
			delete(confirmation["binding"].(map[string]any), "quote")
		}},
		{"invalid quote maximum", func(confirmation map[string]any) {
			confirmation["binding"].(map[string]any)["quote"].(map[string]any)["maximum_authorized_cost_micros"] = float64(1)
		}},
		{"missing hourly compute quote", func(confirmation map[string]any) {
			delete(confirmation["binding"].(map[string]any)["quote"].(map[string]any), "compute_micros_per_hour")
		}},
		{"missing quote source time", func(confirmation map[string]any) {
			delete(confirmation["binding"].(map[string]any)["quote"].(map[string]any), "source_time")
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

func TestExtensionExecutionConfirmationUsesOwnerAuthorityWithoutCloudProjection(t *testing.T) {
	confirmation := map[string]any{
		"confirmation_id": "11111111-1111-4111-8111-111111111111",
		"owner_id":        "@owner:example.test",
		"task_id":         "22222222-2222-4222-8222-222222222222",
		"state":           "pending",
		"revision":        float64(1),
		"created_at":      "2026-08-10T23:40:27Z",
		"updated_at":      "2026-08-10T23:40:27Z",
		"expires_at":      "2026-08-11T00:40:27Z",
		"terminal_code":   "",
		"terminal_note":   "",
		"terminal_reason": "",
		"binding": map[string]any{
			"owner_id": "@owner:example.test", "account_generation": float64(9),
			"operation_domain": "extension.execute", "target_id": "33333333-3333-4333-8333-333333333333",
			"target_revision": float64(4), "target_kind": "mcp", "source_version": "v1",
			"source_commit": strings.Repeat("a", 40), "content_digest": strings.Repeat("b", 64),
			"manifest_digest": strings.Repeat("c", 64), "execution_digest": strings.Repeat("d", 64),
			"permission_digest": strings.Repeat("e", 64), "parameter_digest": strings.Repeat("f", 64),
			"network_digest": strings.Repeat("1", 64), "secret_grant_digest": strings.Repeat("2", 64),
			"selected_tool": "write_html", "selected_command": []any{}, "network_grants": []any{},
			"secret_grants": []any{},
		},
	}
	request := map[string]any{"confirmation_id": confirmation["confirmation_id"]}
	authority := actionResultAuthority{ownerID: "@owner:example.test", accountGeneration: 9}
	got, err := adaptActionResultForRequestWithAuthority("agent.core.confirmations.get", request, map[string]any{"confirmation": confirmation}, authority)
	if err != nil {
		t.Fatalf("extension confirmation rejected: %v", err)
	}
	if !reflect.DeepEqual(got["confirmation"], confirmation) {
		t.Fatalf("extension confirmation projection changed: %#v", got)
	}
	if _, err := adaptActionResultForRequestWithAuthority("agent.core.confirmations.get", request, map[string]any{"confirmation": confirmation}, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("extension confirmation accepted missing prepared authority: %v", err)
	}

	for _, drift := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"foreign owner", func(value map[string]any) { value["owner_id"] = "@foreign:example.test" }},
		{"foreign binding owner", func(value map[string]any) { value["binding"].(map[string]any)["owner_id"] = "@foreign:example.test" }},
		{"stale generation", func(value map[string]any) { value["binding"].(map[string]any)["account_generation"] = float64(8) }},
	} {
		t.Run(drift.name, func(t *testing.T) {
			cloned := cloneCloudWorkerConfirmation(t, confirmation)
			drift.mutate(cloned)
			_, err := adaptActionResultForRequestWithAuthority("agent.core.confirmations.get", request, map[string]any{"confirmation": cloned}, authority)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("authority drift accepted: %v", err)
			}
		})
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
