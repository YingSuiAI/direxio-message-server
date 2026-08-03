package serviceapi

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestActionContractArtifactMatchesActionSpecs(t *testing.T) {
	data, err := os.ReadFile("../../docs/product-action-contract.json")
	if err != nil {
		t.Fatalf("read generated contract: %v", err)
	}
	var artifact ActionContractDocument
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("parse generated contract: %v", err)
	}
	expected := ActionContract()
	if !reflect.DeepEqual(artifact, expected) {
		t.Fatalf("generated action contract is stale; run go run ./cmd/dirextalk-action-contract > docs/product-action-contract.json")
	}
}

func TestActionSpecCopiesDeepCloneNestedSchemas(t *testing.T) {
	const action = "agent.web_search.test"

	want, ok := ActionSpecFor(action)
	if !ok || want.Schema == nil {
		t.Fatalf("%s must publish a schema", action)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal baseline action spec: %v", err)
	}
	got, _ := ActionSpecFor(action)
	credentials := got.Schema.Request["tool_credentials"]
	webSearch := credentials.Properties["web_search"]
	provider := webSearch.Properties["provider"]
	if provider.Presence == nil {
		t.Fatal("web-search provider must publish presence metadata")
	}
	provider.Presence.Omitted = "mutated"
	apiKey := webSearch.Properties["api_key"]
	apiKey.Type = "mutated"
	webSearch.Properties["api_key"] = apiKey
	credentials.Properties["web_search"] = webSearch
	got.Schema.Request["tool_credentials"] = credentials

	fresh, _ := ActionSpecFor(action)
	freshJSON, err := json.Marshal(fresh)
	if err != nil {
		t.Fatalf("marshal fresh action spec: %v", err)
	}
	if string(freshJSON) != string(wantJSON) {
		t.Fatal("mutating a nested ActionSpecFor schema contaminated the registry")
	}

	contract := ActionContract()
	contractBeforeJSON, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("marshal baseline action contract: %v", err)
	}
	found := false
	for _, spec := range contract.Actions {
		if spec.Name != action {
			continue
		}
		found = true
		contractCredentials := spec.Schema.Request["tool_credentials"]
		contractWebSearch := contractCredentials.Properties["web_search"]
		contractWebSearch.Properties["provider"].Presence.Present = "mutated"
		contractCredentials.Properties["web_search"] = contractWebSearch
		break
	}
	if !found {
		t.Fatalf("generated action contract is missing %s", action)
	}
	contractAfterJSON, err := json.Marshal(ActionContract())
	if err != nil {
		t.Fatalf("marshal fresh action contract: %v", err)
	}
	if string(contractAfterJSON) != string(contractBeforeJSON) {
		t.Fatal("mutating a generated action contract schema contaminated later contracts")
	}
}
