package nativeagent

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentskills"
)

func TestSkillsListUsesOnlyBuiltInDeclarativeManifests(t *testing.T) {
	runtime := New(Config{})
	result, err := runtime.skillsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	skills, ok := result["skills"].([]map[string]any)
	if !ok || len(skills) == 0 {
		t.Fatalf("built-in skills = %#v", result)
	}
	for _, skill := range skills {
		for _, key := range []string{"id", "version", "content_digest"} {
			if trimString(skill[key]) == "" {
				t.Fatalf("skill %q omitted %s: %#v", skill["id"], key, skill)
			}
		}
		if _, ok := skill["allowed_step_kinds"].([]string); !ok {
			t.Fatalf("skill %q omitted allowed step metadata: %#v", skill["id"], skill)
		}
		if _, ok := skill["required_target_capabilities"].([]string); !ok {
			t.Fatalf("skill %q omitted declarative metadata: %#v", skill["id"], skill)
		}
		if strings.Contains(strings.ToLower(trimString(skill["id"])), "geolibre") {
			t.Fatalf("fixture skill leaked into default list: %#v", skill)
		}
	}
}

func TestEnabledSkillsPromptSelectsExplicitOrIntentSkillsFailClosed(t *testing.T) {
	registry, err := agentskills.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	runtime := New(Config{PlanningSkills: registry})
	ctx := WithRequestContext(context.Background(), "@owner:example.org", "", "deploy a service")
	prompt := runtime.enabledSkillsPromptForRequest(ctx, nil, nil)
	if prompt == "" || !strings.Contains(prompt, "content_digest") || !strings.Contains(prompt, "allowed_step_kinds") || !strings.Contains(prompt, "required_target_capabilities") {
		t.Fatalf("intent-selected prompt omitted fixed metadata: %q", prompt)
	}
	if strings.Count(prompt, `"id"`) > maxNativeAgentSkills {
		t.Fatalf("intent selection exceeded cap: %q", prompt)
	}

	explicit := runtime.enabledSkillsPromptForRequest(context.Background(), nil, map[string]any{"selected_skill_ids": []any{"health-verifier"}})
	if !strings.Contains(explicit, `"id":"health-verifier"`) || strings.Contains(explicit, `"id":"container-service-deploy"`) {
		t.Fatalf("explicit selection was not deterministic: %q", explicit)
	}
	if got := runtime.enabledSkillsPromptForRequest(context.Background(), nil, map[string]any{"selected_skill_ids": []any{"not-built-in"}}); got != "" {
		t.Fatalf("unknown skill must fail closed, got %q", got)
	}
	if got := runtime.enabledSkillsPromptForRequest(context.Background(), map[string]any{"skills": []any{map[string]any{"id": "not-built-in", "enabled": true}}}, nil); got != "" {
		t.Fatalf("mutable skill records must be ignored, got %q", got)
	}
}

func TestPrepareEinoRunRejectsInvalidExplicitSkillSelection(t *testing.T) {
	registry, err := agentskills.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	valid := registry.Manifests()[0].ID
	runtime := New(Config{PlanningSkills: registry})
	cases := []struct {
		name   string
		params map[string]any
	}{
		{name: "unknown", params: map[string]any{"selected_skill_ids": []any{"not-built-in"}}},
		{name: "too-many", params: map[string]any{"selected_skill_ids": []any{valid, valid + "-2", valid + "-3", valid + "-4"}}},
		{name: "wrong-type", params: map[string]any{"selected_skill_ids": "not-a-list"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := runtime.prepareEinoRun(context.Background(), nil, test.params, nativeModelProfile{}); err == nil {
				t.Fatal("invalid explicit skill selection unexpectedly prepared")
			}
		})
	}

	fixture := registry.Manifests()[0]
	fixture.ID = "fixture-skill"
	fixture.IntentTags = append(fixture.IntentTags, "fixture")
	sort.Strings(fixture.IntentTags)
	fixture.ContentDigest = ""
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fixture.ContentDigest = agentskills.ContentDigest(raw)
	raw, err = json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fixtureRegistry, err := agentskills.NewRegistry(raw)
	if err != nil {
		t.Fatal(err)
	}
	fixtureRuntime := New(Config{PlanningSkills: fixtureRegistry})
	if _, err := fixtureRuntime.prepareEinoRun(context.Background(), nil, map[string]any{"selected_skill_ids": []any{"fixture-skill"}}, nativeModelProfile{}); err == nil {
		t.Fatal("fixture skill unexpectedly prepared")
	}
}
