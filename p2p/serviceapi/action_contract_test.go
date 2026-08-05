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
	const action = "agent.models.list"

	want, ok := ActionSpecFor(action)
	if !ok || want.Schema == nil {
		t.Fatalf("%s must publish a schema", action)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal baseline action spec: %v", err)
	}
	got, _ := ActionSpecFor(action)
	modelKind := got.Schema.Request["model_kind"]
	if modelKind.Presence == nil {
		t.Fatal("model catalog kind must publish presence metadata")
	}
	modelKind.Presence.Omitted = "mutated"
	got.Schema.Request["model_kind"] = modelKind
	providers := got.Schema.Response["providers"]
	provider := providers.Items.Properties["provider"]
	provider.Type = "mutated"
	providers.Items.Properties["provider"] = provider
	got.Schema.Response["providers"] = providers

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
		contractProviders := spec.Schema.Response["providers"]
		contractProvider := contractProviders.Items.Properties["provider"]
		contractProvider.Required = false
		contractProviders.Items.Properties["provider"] = contractProvider
		spec.Schema.Response["providers"] = contractProviders
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
