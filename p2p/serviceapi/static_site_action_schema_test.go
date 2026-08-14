package serviceapi

import "testing"

func TestStaticSiteActionsPublishCurrentOwnerContract(t *testing.T) {
	for _, test := range []struct {
		action           string
		requestRequired  []string
		responseRequired []string
	}{
		{action: "agent.static_sites.list", responseRequired: []string{"releases", "next_page_token"}},
		{
			action:           "agent.static_sites.delete",
			requestRequired:  []string{"release_id", "idempotency_key"},
			responseRequired: []string{"release_id", "deleted", "replayed"},
		},
	} {
		spec, ok := ActionSpecFor(test.action)
		if !ok || spec.Auth != ActionAuthOwner || spec.Transport != ActionTransportHTTPOnly || spec.Schema == nil {
			t.Fatalf("%s spec = %#v", test.action, spec)
		}
		for _, field := range test.requestRequired {
			if !spec.Schema.Request[field].Required {
				t.Errorf("%s request.%s must be required", test.action, field)
			}
		}
		for _, field := range test.responseRequired {
			if !spec.Schema.Response[field].Required {
				t.Errorf("%s response.%s must be required", test.action, field)
			}
		}
	}

	list, _ := ActionSpecFor("agent.static_sites.list")
	release := list.Schema.Response["releases"].Items
	if release == nil || len(release.Properties) != 7 {
		t.Fatalf("static-site release schema = %#v", release)
	}
	for _, field := range []string{"site_id", "release_id", "conversation_id", "public_url", "public_path", "size_bytes", "created_at"} {
		if !release.Properties[field].Required {
			t.Errorf("static-site release.%s must be required", field)
		}
	}
}
