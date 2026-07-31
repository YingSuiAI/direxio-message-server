package agentrecipes

import (
	"embed"
	"encoding/json"
	"strings"
	"testing"
)

// Fixture manifests are compiled only into tests; production code has no
// project-specific recipe name or selection branch.
//
//go:embed geolibre-deploy/*.json
var fixtureManifestFS embed.FS

func fixtureBuiltin() (*Registry, error) {
	b, err := fixtureManifestFS.ReadFile("geolibre-deploy/geolibre-deploy.json")
	if err != nil {
		return nil, err
	}
	return NewRegistry(b)
}

func TestBuiltinRecipesIncludeOnlyGenericRecipes(t *testing.T) {
	for _, name := range []string{"manifests/aws-ec2-provision.json", "manifests/generic-container-service.json", "manifests/source-build-systemd.json"} {
		raw, err := manifestFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var manifest RecipeManifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		if got := ContentDigest(raw); got != manifest.ContentDigest {
			t.Fatalf("%s content_digest=%s want=%s", name, manifest.ContentDigest, got)
		}
	}
	fixtureRaw, err := fixtureManifestFS.ReadFile("geolibre-deploy/geolibre-deploy.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtureManifest RecipeManifest
	if err := json.Unmarshal(fixtureRaw, &fixtureManifest); err != nil {
		t.Fatal(err)
	}
	if got := ContentDigest(fixtureRaw); got != fixtureManifest.ContentDigest {
		t.Fatalf("fixture content_digest=%s want=%s", fixtureManifest.ContentDigest, got)
	}
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Manifests()) != 3 {
		t.Fatalf("recipe count = %d, want 3", len(r.Manifests()))
	}
	for _, m := range r.Manifests() {
		if m.ID != "aws-ec2-provision" && m.ID != "generic-container-service" && m.ID != "source-build-systemd" {
			t.Fatalf("unexpected core recipe %q", m.ID)
		}
		for _, stage := range m.Stages {
			if stage.StageKey == "" || stage.TimeoutSeconds == 0 {
				t.Fatalf("invalid stage in %q", m.ID)
			}
		}
	}
}

func TestDeploymentRecipesDeclareAWSExecutionTargetCapabilities(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, recipe := range r.Manifests() {
		if recipe.ID == "aws-ec2-provision" {
			if !contains(recipe.RequiredTargetCapabilities, "target.aws_compute_reservation") || !contains(recipe.RequiredTargetCapabilities, "compute.provision") {
				t.Fatalf("%s does not require a reserved compute target: %v", recipe.ID, recipe.RequiredTargetCapabilities)
			}
			continue
		}
		if !contains(recipe.RequiredTargetCapabilities, "target.aws_ec2_instance") || !contains(recipe.RequiredTargetCapabilities, "transport.aws_ssm") {
			t.Fatalf("%s does not require the AWS SSM execution target: %v", recipe.ID, recipe.RequiredTargetCapabilities)
		}
		if recipe.ID == "generic-container-service" && contains(recipe.RequiredTargetCapabilities, "runtime.container") {
			t.Fatalf("container recipe incorrectly requires a preinstalled container runtime: %v", recipe.RequiredTargetCapabilities)
		}
	}
	fixtureRegistry, err := fixtureBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	fixtures := fixtureRegistry.Manifests()
	if len(fixtures) != 1 || fixtures[0].ID != "geolibre-deploy" {
		t.Fatalf("fixture registry = %#v", fixtures)
	}
	if !contains(fixtures[0].RequiredTargetCapabilities, "target.aws_ec2_instance") || !contains(fixtures[0].RequiredTargetCapabilities, "transport.aws_ssm") || contains(fixtures[0].RequiredTargetCapabilities, "runtime.container") {
		t.Fatalf("fixture target capabilities are not bootstrap-compatible: %v", fixtures[0].RequiredTargetCapabilities)
	}
}

func TestRecipeManifestsDeepCopyAndFixtureSelection(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	first := r.Manifests()
	first[0].Stages[0].StageKey = "mutated"
	if len(first[0].Stages[0].Steps) > 0 && first[0].Stages[0].Steps[0].Template == nil {
		first[0].Stages[0].Steps[0].Template = map[string]interface{}{}
	}
	if len(first[0].Stages[0].Steps) > 0 {
		first[0].Stages[0].Steps[0].Template["mutated"] = true
	}
	second := r.Manifests()
	if second[0].Stages[0].StageKey == "mutated" {
		t.Fatal("registry stage was mutable through returned slice")
	}
	selected, err := r.Select(SelectionQuery{Intent: "deploy", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range selected {
		if strings.Contains(m.ID, "geolibre") {
			t.Fatal("fixture selected for non-fixture intent")
		}
	}
	fixturesRegistry, err := fixtureBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := fixturesRegistry.Select(SelectionQuery{Intent: "fixture", TargetCapabilities: []string{"artifact.fetch", "probe.http", "runtime.container", "target.aws_ec2_instance", "transport.aws_ssm"}, Limit: 3})
	if err != nil || len(fixtures) != 1 || fixtures[0].ID != "geolibre-deploy" {
		t.Fatalf("fixture selection = %#v, err=%v", fixtures, err)
	}
}

func TestRecipeSelectionAcceptsOneServerOwnedInstanceIdentityOnly(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	base := []string{"target.aws_ec2_instance", "transport.aws_ssm"}
	withIdentity := append(append([]string(nil), base...), "target.instance.i-0123456789abcdef0")
	selected, err := r.Select(SelectionQuery{Intent: "deploy", TargetCapabilities: withIdentity, Limit: 3})
	if err != nil || len(selected) == 0 || selected[0].ID != "generic-container-service" {
		t.Fatalf("identity-bearing target selection=%#v err=%v", selected, err)
	}
	for name, capabilities := range map[string][]string{
		"arbitrary": append(append([]string(nil), base...), "target.external.arbitrary"),
		"malformed": append(append([]string(nil), base...), "target.instance.i-NOTHEX"),
		"prefix":    append(append([]string(nil), base...), "target.instance.other-i-01234567"),
		"duplicate": append(append([]string(nil), withIdentity...), "target.instance.i-0123456789abcdef0"),
		"multiple":  append(append([]string(nil), withIdentity...), "target.instance.i-fedcba9876543210f"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Select(SelectionQuery{Intent: "deploy", TargetCapabilities: capabilities, Limit: 3}); err == nil {
				t.Fatal("unsafe target identity capability was accepted")
			}
		})
	}
}

func TestGenericContainerRecipeSelectsInitialDeployOnly(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	capabilities := []string{"target.aws_ec2_instance", "transport.aws_ssm"}
	for _, intent := range []string{"upgrade", "repair", "container", "recipe"} {
		selected, err := r.Select(SelectionQuery{Intent: intent, TargetCapabilities: capabilities, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range selected {
			if candidate.ID == "generic-container-service" {
				t.Fatalf("generic container selected for %q", intent)
			}
		}
	}
}

func TestRecipeValidationRejectsCycleAndUnsafeFields(t *testing.T) {
	raw, err := manifestFS.ReadFile("manifests/generic-container-service.json")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	stages := value["stages"].([]interface{})
	stages[0].(map[string]interface{})["depends_on"] = []interface{}{"apply"}
	if _, err := Parse(withDigest(value)); err == nil {
		t.Fatal("cyclic recipe unexpectedly parsed")
	}
	value = map[string]interface{}{"id": "new-unrelated-recipe", "version": "1.0.0", "schema_version": "recipe/v1", "content_digest": "", "minimum_core_version": "1.0.0", "input_schema": map[string]interface{}{"type": "object"}, "output_schema": map[string]interface{}{"type": "object"}, "intent_tags": []interface{}{"custom"}, "allowed_step_kinds": []interface{}{"target.inspect"}, "required_target_capabilities": []interface{}{}, "network_access": map[string]interface{}{"allowed": false}, "secret_purposes": []interface{}{}, "stages": []interface{}{map[string]interface{}{"stage_key": "inspect", "title": "Inspect", "kind": "target.inspect", "risk": "R0", "gate": "none", "steps": []interface{}{map[string]interface{}{"step_key": "inspect", "kind": "target.inspect", "template": map[string]interface{}{}, "timeout_seconds": 1, "idempotency_marker": "inspect"}}, "timeout_seconds": 1}}}
	if _, err := Parse(withDigest(value)); err != nil {
		t.Fatalf("unrelated recipe should parse: %v", err)
	}
	value["stages"].([]interface{})[0].(map[string]interface{})["steps"].([]interface{})[0].(map[string]interface{})["template"] = map[string]interface{}{"inline_shell": "echo unsafe"}
	if _, err := Parse(withDigest(value)); err == nil {
		t.Fatal("unsafe recipe unexpectedly parsed")
	}
}

func TestRecipeRiskNetworkAndAllowedKindBoundaries(t *testing.T) {
	raw, err := manifestFS.ReadFile("manifests/source-build-systemd.json")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	stages := value["stages"].([]interface{})
	fetch := stages[1].(map[string]interface{})
	fetch["gate"] = "repository_write"
	if _, err := Parse(withDigest(value)); err == nil {
		t.Fatal("read-only source.fetch accepted repository_write gate")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["network_access"].(map[string]interface{})["endpoints"] = []interface{}{"https://*.example/{repository}"}
	if _, err := Parse(withDigest(value)); err == nil {
		t.Fatal("wildcard endpoint unexpectedly accepted")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["allowed_step_kinds"] = []interface{}{"target.inspect"}
	if _, err := Parse(withDigest(value)); err == nil {
		t.Fatal("step kind outside allowlist unexpectedly accepted")
	}
}

func TestDuplicateRecipeKeysRejected(t *testing.T) {
	if _, err := Parse([]byte(`{"id":"x","id":"y"}`)); err == nil {
		t.Fatal("duplicate recipe key accepted")
	}
}

func TestRecipeScriptSecretOutputAndPlaceholderBindings(t *testing.T) {
	raw, err := manifestFS.ReadFile("manifests/source-build-systemd.json")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	build := value["stages"].([]interface{})[2].(map[string]interface{})["steps"].([]interface{})[0].(map[string]interface{})
	build["output_policy"] = "raw"
	if _, err := Parse(withDigest(value)); err == nil {
		t.Fatal("unbounded output policy accepted")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	build = value["stages"].([]interface{})[2].(map[string]interface{})["steps"].([]interface{})[0].(map[string]interface{})
	build["redaction"] = map[string]interface{}{}
	if _, err := Parse(withDigest(value)); err == nil {
		t.Fatal("empty redaction accepted")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	build = value["stages"].([]interface{})[2].(map[string]interface{})["steps"].([]interface{})[0].(map[string]interface{})
	build["secret_refs"] = []interface{}{"undeclared-secret"}
	if _, err := Parse(withDigest(value)); err == nil {
		t.Fatal("undeclared secret reference accepted")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["input_schema"].(map[string]interface{})["properties"].(map[string]interface{})["build"].(map[string]interface{})["properties"].(map[string]interface{})["artifact_id"] = map[string]interface{}{"type": "string"}
	build = value["stages"].([]interface{})[2].(map[string]interface{})["steps"].([]interface{})[0].(map[string]interface{})
	build["artifact_ref"].(map[string]interface{})["artifact_id"] = "${unknown.artifact_id}"
	if _, err := Parse(withDigest(value)); err == nil {
		t.Fatal("undeclared template placeholder accepted")
	}
}

func withDigest(value map[string]interface{}) []byte {
	value["content_digest"] = ""
	without, _ := json.Marshal(value)
	value["content_digest"] = ContentDigest(without)
	result, _ := json.Marshal(value)
	return result
}
