package nativeagent

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentskills"
)

func TestClassifySkillIntentSupportsEnglishChineseAndJapanese(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"english deploy", "deploy this service", "deploy"},
		{"english health", "run a health probe", "health"},
		{"english repair", "rollback the failed release", "repair"},
		{"english placement", "compare placement options", "placement"},
		{"english sizing", "estimate resources for the service", "sizing"},
		{"english target", "which AWS EC2 target should I use?", "target"},
		{"english intake", "start the project intake", "intake"},
		{"english runbook", "write the operations runbook", "runbook"},
		{"chinese deploy", "请部署这个服务", "deploy"},
		{"chinese health", "检查服务健康状态", "health"},
		{"chinese repair", "请回滚并修复发布", "repair"},
		{"chinese placement", "比较部署位置和地域", "placement"},
		{"chinese sizing", "评估资源规格", "sizing"},
		{"chinese target", "选择 AWS EC2 目标实例", "target"},
		{"chinese intake", "开始项目资料接入", "intake"},
		{"chinese runbook", "生成运维手册", "runbook"},
		{"japanese deploy", "サービスをデプロイして", "deploy"},
		{"japanese health", "ヘルスチェックを実行", "health"},
		{"japanese repair", "失敗したリリースをロールバック", "repair"},
		{"japanese placement", "配置場所を比較", "placement"},
		{"japanese sizing", "必要スペックを見積もる", "sizing"},
		{"japanese target", "AWSの対象インスタンス", "target"},
		{"japanese intake", "プロジェクトの要件整理", "intake"},
		{"japanese runbook", "運用手順書を作成", "runbook"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := classifySkillIntent(test.text); got != test.want {
				t.Fatalf("classifySkillIntent(%q) = %q, want %q", test.text, got, test.want)
			}
		})
	}
}

func TestClassifySkillIntentUsesDeterministicPrecedenceAndWordBoundaries(t *testing.T) {
	if got := classifySkillIntent("deploy it and then verify health"); got != "deploy" {
		t.Fatalf("precedence = %q, want deploy", got)
	}
	for _, text := range []string{"projector output", "resourceful planning", "targeting users", "a deployment plan"} {
		want := ""
		if text == "a deployment plan" {
			want = "deploy"
		}
		if got := classifySkillIntent(text); got != want {
			t.Fatalf("classifySkillIntent(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestSelectedSkillIntentNormalizesLocalizedExplicitIntent(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "部署", want: "deploy"},
		{value: "ヘルスチェック", want: "health"},
		{value: "rollback", want: "rollback"},
	} {
		if got := selectedSkillIntent(context.Background(), nil, map[string]any{"intent": test.value}); got != test.want {
			t.Fatalf("selectedSkillIntent(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestIntentSelectionExcludesFixtureManifest(t *testing.T) {
	registry, err := agentskills.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	manifests := registry.Manifests()
	var fixture agentskills.Manifest
	for _, manifest := range manifests {
		if manifest.ID == "container-service-deploy" {
			fixture = manifest
			break
		}
	}
	if fixture.ID == "" {
		t.Fatal("container-service-deploy manifest not found")
	}
	fixture.IntentTags = append([]string(nil), fixture.IntentTags...)
	fixture.ID = "fixture-deploy"
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
	fixtureRegistry, err := agentskills.NewRegistry(raw, mustManifestJSON(t, manifests, "health-verifier"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := New(Config{PlanningSkills: fixtureRegistry})
	selected, err := runtime.selectPlanningSkills(WithRequestContext(context.Background(), "owner", "conversation", "verify service health"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range selected {
		if strings.Contains(manifest.ID, "fixture") {
			t.Fatalf("fixture manifest selected: %#v", selected)
		}
	}
	if len(selected) != 1 || selected[0].ID != "health-verifier" {
		t.Fatalf("selected manifests = %#v, want only health-verifier", selected)
	}
}

func TestDeploymentIntentHidesAWSPlanningSkills(t *testing.T) {
	runtime := New(Config{})
	selected, err := runtime.selectPlanningSkills(WithRequestContext(context.Background(), "owner", "conversation", "请把这个 Docker 服务部署到 AWS"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(selected))
	for _, manifest := range selected {
		ids = append(ids, manifest.ID)
	}
	want := []string{"project-intake-analyzer"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("deployment skills = %#v, want %#v", ids, want)
	}

	selected, err = runtime.selectPlanningSkills(WithRequestContext(context.Background(), "owner", "conversation", "部署 GitHub 上的 Go 源码服务"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].ID != "project-intake-analyzer" {
		t.Fatalf("source deployment skills = %#v", selected)
	}
}

func TestPlacementAndSizingIntentHideAWSTargetAdvisor(t *testing.T) {
	runtime := New(Config{})
	for _, intent := range []string{"placement", "sizing"} {
		selected, err := runtime.selectPlanningSkills(
			context.Background(),
			nil,
			map[string]any{"skill_intent": intent},
		)
		if err != nil {
			t.Fatalf("select %s: %v", intent, err)
		}
		if len(selected) != 0 {
			t.Fatalf("select %s = %#v, want no AWS skills", intent, selected)
		}
	}
}

func mustManifestJSON(t *testing.T, manifests []agentskills.Manifest, id string) []byte {
	t.Helper()
	for _, manifest := range manifests {
		if manifest.ID != id {
			continue
		}
		manifest.ContentDigest = ""
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		manifest.ContentDigest = agentskills.ContentDigest(raw)
		raw, err = json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	t.Fatalf("manifest %q not found", id)
	return nil
}
