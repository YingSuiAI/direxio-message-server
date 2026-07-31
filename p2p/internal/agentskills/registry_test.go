package agentskills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
)

func TestBuiltinManifestsAndExactDigestPins(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(r.Manifests()); got != 9 {
		t.Fatalf("built-in skill count = %d, want 9", got)
	}
	m := r.Manifests()[0]
	if _, err := r.ResolveExact(m.ID, m.Version, m.ContentDigest); err != nil {
		t.Fatalf("exact pin: %v", err)
	}
	if _, err := r.ResolveExact(m.ID, m.Version, strings.Repeat("0", 64)); err == nil {
		t.Fatal("tampered digest unexpectedly resolved")
	}
	raw, err := builtinFS.ReadFile("manifests/" + m.ID + ".json")
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"intent_tags":[`, `"intent_tags":["tampered",`, 1)
	if _, err := Parse([]byte(tampered)); err == nil {
		t.Fatal("tampered manifest unexpectedly parsed")
	}
}

func TestDeploymentSkillsCloseOverRecipeForwardNetwork(t *testing.T) {
	skills, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := agentrecipes.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"container-service-deploy", "source-build-systemd"} {
		var skill Manifest
		for _, candidate := range skills.Manifests() {
			if candidate.ID == id {
				skill = candidate
			}
		}
		var recipe agentrecipes.RecipeManifest
		for _, candidate := range recipes.Manifests() {
			if candidate.ID == id {
				recipe = candidate
			}
		}
		if !skill.NetworkAccess.Allowed {
			t.Fatalf("%s denies recipe egress", id)
		}
		for _, stage := range recipe.Stages {
			for _, step := range stage.Steps {
				if !contains(skill.AllowedStepKinds, step.Kind) {
					t.Fatalf("%s does not allow forward %s", id, step.Kind)
				}
			}
		}
	}
}

func TestDeploymentSkillsDeclareAWSExecutionTargetCapabilities(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"container-service-deploy", "source-build-systemd"} {
		var skill Manifest
		for _, candidate := range r.Manifests() {
			if candidate.ID == id {
				skill = candidate
			}
		}
		if !contains(skill.RequiredTargetCapabilities, "target.aws_ec2_instance") || !contains(skill.RequiredTargetCapabilities, "transport.aws_ssm") {
			t.Fatalf("%s does not require the AWS SSM execution target: %v", id, skill.RequiredTargetCapabilities)
		}
		if id == "container-service-deploy" && !contains(skill.RequiredTargetCapabilities, "runtime.container") {
			t.Fatalf("container skill omitted runtime.container: %v", skill.RequiredTargetCapabilities)
		}
	}
}

func TestSelectionIsDeterministicAndCapped(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Select(SelectionQuery{Intent: "deploy", TargetCapabilities: []string{"artifact.collect", "artifact.fetch", "probe.http", "probe.tcp", "runtime.container", "runtime.systemd", "secret.reference", "target.aws_ec2_instance", "transport.aws_ssm"}, Limit: 99})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Select(SelectionQuery{Intent: "deploy", TargetCapabilities: []string{"artifact.collect", "artifact.fetch", "probe.http", "probe.tcp", "runtime.container", "runtime.systemd", "secret.reference", "target.aws_ec2_instance", "transport.aws_ssm"}, Limit: 99})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) > 3 || len(first) != len(second) {
		t.Fatalf("selection cap/order: %d and %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("selection is not stable: %v vs %v", first, second)
		}
	}
}

func TestValidationRejectsExecutionFieldsUnknownsAndURLs(t *testing.T) {
	cases := []string{
		`{"id":"safe","version":"1.0.0","schema_version":"1.0.0","content_digest":"0000000000000000000000000000000000000000000000000000000000000000","minimum_core_version":"1.0.0","input_schema":{"type":"object"},"output_schema":{"type":"object"},"allowed_step_kinds":["shell"],"required_target_capabilities":[],"network_access":{"allowed":false},"intent_tags":[],"steps":[]}`,
		`{"id":"safe","version":"1.0.0","schema_version":"1.0.0","content_digest":"0000000000000000000000000000000000000000000000000000000000000000","minimum_core_version":"1.0.0","input_schema":{"type":"object"},"output_schema":{"type":"object"},"allowed_step_kinds":["analysis"],"required_target_capabilities":["gpu"],"network_access":{"allowed":false},"intent_tags":[],"steps":[]}`,
		`{"id":"safe","version":"1.0.0","schema_version":"1.0.0","content_digest":"0000000000000000000000000000000000000000000000000000000000000000","minimum_core_version":"1.0.0","input_schema":{"type":"object"},"output_schema":{"type":"object"},"allowed_step_kinds":["analysis"],"required_target_capabilities":[],"network_access":{"allowed":false},"intent_tags":[],"steps":[{"id":"x","kind":"analysis","command":"echo nope"}]}`,
		`{"id":"safe","version":"1.0.0","schema_version":"1.0.0","content_digest":"0000000000000000000000000000000000000000000000000000000000000000","minimum_core_version":"1.0.0","input_schema":{"$ref":"https://example.test/schema"},"output_schema":{"type":"object"},"allowed_step_kinds":["analysis"],"required_target_capabilities":[],"network_access":{"allowed":false},"intent_tags":[],"steps":[]}`,
	}
	for _, content := range cases {
		if _, err := Parse([]byte(content)); err == nil {
			t.Fatal("invalid manifest unexpectedly accepted")
		}
	}
}

func TestCoreSourceHasNoGeoLibreBranch(t *testing.T) {
	for _, name := range []string{"registry.go", "builtin.go"} {
		content, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(content)), "geolibre") {
			t.Fatalf("core source contains GeoLibre branch: %s", name)
		}
	}
}

func TestContentDigestCanonicalizesManifest(t *testing.T) {
	var value map[string]interface{}
	if err := json.Unmarshal([]byte(`{"id":"x","content_digest":"ignored","version":"1.0.0"}`), &value); err != nil {
		t.Fatal(err)
	}
	if got := ContentDigest([]byte(`{"id":"x","content_digest":"ignored","version":"1.0.0"}`)); got == "" {
		t.Fatal("empty digest")
	}
}

func TestSkillValidationBoundaries(t *testing.T) {
	raw, err := builtinFS.ReadFile("manifests/project-intake-analyzer.json")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["network_access"] = map[string]interface{}{"allowed": true, "purpose": "bounded-access", "endpoints": []interface{}{"https://*.example/${repo}"}}
	if _, err := Parse(skillWithDigest(value)); err == nil {
		t.Fatal("wildcard skill endpoint unexpectedly accepted")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["allowed_step_kinds"] = []interface{}{"analysis.evil"}
	value["steps"] = []interface{}{map[string]interface{}{"id": "step", "kind": "analysis.evil"}}
	if _, err := Parse(skillWithDigest(value)); err == nil {
		t.Fatal("open planning step vocabulary unexpectedly accepted")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["input_schema_ref"] = "../outside.json"
	delete(value, "input_schema")
	if _, err := Parse(skillWithDigest(value)); err == nil {
		t.Fatal("traversing schema ref unexpectedly accepted")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["steps"] = []interface{}{map[string]interface{}{"id": "same", "kind": "analysis.project"}, map[string]interface{}{"id": "same", "kind": "analysis.project"}}
	if _, err := Parse(skillWithDigest(value)); err == nil {
		t.Fatal("duplicate step IDs unexpectedly accepted")
	}
}

func TestDuplicateKeysRejected(t *testing.T) {
	if _, err := Parse([]byte(`{"id":"x","id":"y"}`)); err == nil {
		t.Fatal("duplicate skill key accepted")
	}
}

func skillWithDigest(value map[string]interface{}) []byte {
	value["content_digest"] = ""
	without, _ := json.Marshal(value)
	value["content_digest"] = ContentDigest(without)
	result, _ := json.Marshal(value)
	return result
}
