package agentgateway

import (
	"errors"
	"testing"
)

const (
	staticSiteTestSiteID         = "11111111-1111-4111-8111-111111111111"
	staticSiteTestReleaseID      = "22222222-2222-4222-8222-222222222222"
	staticSiteTestConversationID = "33333333-3333-4333-8333-333333333333"
	staticSiteTestIdempotencyKey = "44444444-4444-4444-8444-444444444444"
)

func TestStaticSiteActionBindingsUseCurrentAgentCapability(t *testing.T) {
	for action, operation := range map[string]string{
		"agent.static_sites.list":   "list_releases",
		"agent.static_sites.delete": "delete_release",
	} {
		binding, ok := actionBindingFor(action)
		if !ok || binding.capabilityID != "agent.static_sites.v1" || binding.operation != operation {
			t.Errorf("%s binding = %#v, want agent.static_sites.v1/%s", action, binding, operation)
		}
	}
}

func TestStaticSiteCatalogPinsCurrentAgentSchemas(t *testing.T) {
	const (
		listInput    = `{"additionalProperties":false,"properties":{"page_size":{"maximum":100,"minimum":1,"type":"integer"},"page_token":{"maxLength":4096,"type":"string"}},"type":"object"}`
		release      = `{"additionalProperties":false,"properties":{"conversation_id":{"format":"uuid","type":"string"},"created_at":{"format":"date-time","type":"string"},"public_path":{"type":"string"},"public_url":{"format":"uri","type":"string"},"release_id":{"format":"uuid","type":"string"},"site_id":{"format":"uuid","type":"string"},"size_bytes":{"maximum":196608,"minimum":1,"type":"integer"}},"required":["site_id","release_id","conversation_id","public_url","public_path","size_bytes","created_at"],"type":"object"}`
		listResult   = `{"additionalProperties":false,"properties":{"next_page_token":{"type":"string"},"releases":{"items":` + release + `,"type":"array"}},"required":["releases","next_page_token"],"type":"object"}`
		deleteInput  = `{"additionalProperties":false,"properties":{"idempotency_key":{"format":"uuid","type":"string"},"release_id":{"format":"uuid","type":"string"}},"required":["release_id","idempotency_key"],"type":"object"}`
		deleteResult = `{"additionalProperties":false,"properties":{"deleted":{"const":true,"type":"boolean"},"release_id":{"format":"uuid","type":"string"},"replayed":{"type":"boolean"}},"required":["release_id","deleted","replayed"],"type":"object"}`
	)
	for _, test := range []struct{ action, operation, input, result string }{
		{"agent.static_sites.list", "list_releases", listInput, listResult},
		{"agent.static_sites.delete", "delete_release", deleteInput, deleteResult},
	} {
		descriptor := catalogTestDescriptor("agent.static_sites.v1", test.operation, test.input, test.result)
		if err := ValidateCatalog(catalogTestWithDigest(t, descriptor), []CatalogRequirement{NewCatalogRequirement(test.action)}); err != nil {
			t.Fatalf("%s current Agent schema rejected: %v", test.action, err)
		}
	}
}

func TestStaticSiteRequestsRejectNonCanonicalInputs(t *testing.T) {
	if err := ValidateActionRequest("agent.static_sites.list", map[string]any{"page_size": 100, "page_token": "next"}); err != nil {
		t.Fatalf("valid static-site list rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.static_sites.list", map[string]any{"page_size": 101}); !errors.Is(err, ErrInvalidActionRequest) {
		t.Fatalf("oversized list page error = %v", err)
	}
	validDelete := map[string]any{"release_id": staticSiteTestReleaseID, "idempotency_key": staticSiteTestIdempotencyKey}
	if err := ValidateActionRequest("agent.static_sites.delete", validDelete); err != nil {
		t.Fatalf("valid static-site delete rejected: %v", err)
	}
	validDelete["release_id"] = "not-a-uuid"
	if err := ValidateActionRequest("agent.static_sites.delete", validDelete); !errors.Is(err, ErrInvalidActionRequest) {
		t.Fatalf("invalid release identity error = %v", err)
	}
}

func TestStaticSiteListResultProjectsExactPublicRelease(t *testing.T) {
	release := staticSiteTestRelease()
	release["private_field"] = "must-not-cross"
	output := map[string]any{"releases": []any{release}, "next_page_token": "next", "private_field": "must-not-cross"}
	if _, err := adaptActionResultForRequest("agent.static_sites.list", map[string]any{}, output); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("open static-site response error = %v", err)
	}

	delete(release, "private_field")
	delete(output, "private_field")
	projected, err := adaptActionResultForRequest("agent.static_sites.list", map[string]any{}, output)
	if err != nil {
		t.Fatal(err)
	}
	releases, ok := projected["releases"].([]any)
	if !ok || len(releases) != 1 {
		t.Fatalf("projected releases = %#v", projected["releases"])
	}
}

func TestStaticSiteResultsRejectForeignLocationAndDeleteIdentity(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"http URL": func(release map[string]any) {
			release["public_url"] = "http://s3.dirextalk.ai" + release["public_path"].(string)
		},
		"foreign path": func(release map[string]any) {
			release["public_path"] = "/.sites/" + staticSiteTestSiteID + "/55555555-5555-4555-8555-555555555555/"
		},
		"query": func(release map[string]any) { release["public_url"] = release["public_url"].(string) + "?token=x" },
	} {
		t.Run(name, func(t *testing.T) {
			release := staticSiteTestRelease()
			mutate(release)
			_, err := adaptActionResultForRequest("agent.static_sites.list", map[string]any{}, map[string]any{"releases": []any{release}, "next_page_token": ""})
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid public location error = %v", err)
			}
		})
	}

	request := map[string]any{"release_id": staticSiteTestReleaseID, "idempotency_key": staticSiteTestIdempotencyKey}
	output := map[string]any{"release_id": "55555555-5555-4555-8555-555555555555", "deleted": true, "replayed": false}
	if _, err := adaptActionResultForRequest("agent.static_sites.delete", request, output); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("foreign delete identity error = %v", err)
	}
}

func staticSiteTestRelease() map[string]any {
	path := "/.sites/" + staticSiteTestSiteID + "/" + staticSiteTestReleaseID + "/"
	return map[string]any{
		"site_id": staticSiteTestSiteID, "release_id": staticSiteTestReleaseID, "conversation_id": staticSiteTestConversationID,
		"public_url": "https://s3.dirextalk.ai" + path, "public_path": path, "size_bytes": int64(1024), "created_at": "2026-08-13T10:00:00Z",
	}
}
