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
	if got := len(r.Manifests()); got != 7 {
		t.Fatalf("built-in skill count = %d, want 7", got)
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

func TestBuiltinTargetAdvisorConsolidatesPlacementAndSizing(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range r.Manifests() {
		if manifest.ID == "placement-advisor" || manifest.ID == "resource-sizing" {
			t.Fatalf("redundant built-in skill remains: %q", manifest.ID)
		}
		if manifest.ID != "aws-target-advisor" {
			continue
		}
		for _, tag := range []string{"placement", "sizing"} {
			if !contains(manifest.IntentTags, tag) {
				t.Fatalf("aws target advisor omitted %q intent tag: %v", tag, manifest.IntentTags)
			}
		}
	}
	for _, intent := range []string{"placement", "sizing"} {
		selected, err := r.Select(SelectionQuery{Intent: intent, Limit: 3})
		if err != nil {
			t.Fatalf("select %s: %v", intent, err)
		}
		if len(selected) != 1 || selected[0].ID != "aws-target-advisor" {
			t.Fatalf("select %s = %#v, want aws-target-advisor only", intent, selected)
		}
	}
}

func TestBuiltinHistoricalSkillPinsAndSelectionsRemainImmutable(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id, version, digest string
	}{
		{"placement-advisor", "1.0.0", "8ba47184e18c1ce354a1d5af106fc706ae9003dd4984dc1a4cfc757c238cbb99"},
		{"resource-sizing", "1.0.0", "28bdc0b940968fb4ad2e63c1cd364f6c7538e6cd826fa2140b19e320f8608ccf"},
		{"aws-target-advisor", "1.1.0", "bcb4aae7ae549146342b4bc7b23f2161cdb8f5d9f41a631bc046db1ea3b65763"},
	}
	for _, test := range cases {
		manifest, err := r.ResolveExact(test.id, test.version, test.digest)
		if err != nil {
			t.Fatalf("resolve historical %s@%s: %v", test.id, test.version, err)
		}
		if manifest.ID != test.id || manifest.Version != test.version || manifest.ContentDigest != test.digest {
			t.Fatalf("historical pin = %#v, want %s@%s/%s", manifest, test.id, test.version, test.digest)
		}
	}
	for _, test := range []struct {
		selected, id, version string
	}{
		{"placement-advisor", "placement-advisor", "1.0.0"},
		{"resource-sizing", "resource-sizing", "1.0.0"},
		{"aws-target-advisor@1.1.0#bcb4aae7ae549146342b4bc7b23f2161cdb8f5d9f41a631bc046db1ea3b65763", "aws-target-advisor", "1.1.0"},
	} {
		manifest, err := r.ResolveSelectedID(test.selected)
		if err != nil {
			t.Fatalf("resolve historical selection %q: %v", test.selected, err)
		}
		if manifest.ID != test.id || manifest.Version != test.version {
			t.Fatalf("selection %q = %s@%s, want %s@%s", test.selected, manifest.ID, manifest.Version, test.id, test.version)
		}
	}
	active, err := r.ResolveSelectedID("aws-target-advisor")
	if err != nil {
		t.Fatal(err)
	}
	if active.Version != "1.2.0" {
		t.Fatalf("bare active selection resolved archived version %s", active.Version)
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
