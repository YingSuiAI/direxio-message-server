package nativeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentskills"
)

const maxNativeAgentSkills = 3

// skillsList exposes only the immutable manifests compiled into this server.
// It deliberately ignores durable/configured skill records: embedded Native
// Agent has no mutable skill registry.
func (r *Runtime) skillsList(context.Context) (map[string]any, error) {
	if r == nil || r.planningSkills == nil {
		return nil, fmt.Errorf("built-in planning skill registry is unavailable")
	}
	return map[string]any{"skills": r.builtinSkillMetadata()}, nil
}

func (r *Runtime) builtinSkillMetadata() []map[string]any {
	if r == nil || r.planningSkills == nil {
		return []map[string]any{}
	}
	manifests := r.planningSkills.Manifests()
	result := make([]map[string]any, 0, len(manifests))
	for _, manifest := range manifests {
		if isFixtureSkill(manifest) {
			continue
		}
		result = append(result, skillMetadata(manifest))
	}
	return result
}

// enabledSkillsPrompt is retained as a small compatibility wrapper for tests
// and callers that only have config. Chat requests use the request-aware form.
func (r *Runtime) enabledSkillsPrompt(ctx context.Context, config map[string]any) string {
	return r.enabledSkillsPromptForRequest(ctx, config, nil)
}

func (r *Runtime) enabledSkillsPromptForRequest(ctx context.Context, config, params map[string]any) string {
	selected, err := r.selectPlanningSkills(ctx, config, params)
	if err != nil || len(selected) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Built-in Planning Skills (declarative metadata only; no installation or execution):")
	for _, manifest := range selected {
		encoded, marshalErr := json.Marshal(skillMetadata(manifest))
		if marshalErr != nil {
			return ""
		}
		b.WriteByte('\n')
		b.Write(encoded)
	}
	return b.String()
}

func (r *Runtime) selectPlanningSkills(ctx context.Context, config, params map[string]any) ([]agentskills.Manifest, error) {
	if r == nil || r.planningSkills == nil {
		return nil, fmt.Errorf("built-in planning skill registry is unavailable")
	}
	ids, explicit := selectedSkillIDs(config, params)
	if explicit {
		if len(ids) == 0 || len(ids) > maxNativeAgentSkills {
			return nil, fmt.Errorf("invalid selected planning skill count")
		}
		return r.resolveSelectedSkills(ids)
	}

	intent := selectedSkillIntent(ctx, config, params)
	if intent == "" {
		return nil, nil
	}
	if intent == "deploy" {
		_, _, userText := RequestContext(ctx)
		if selected := r.selectDeploymentPlanningSkills(userText); len(selected) > 0 {
			return selected, nil
		}
	}
	capabilities := make([]string, 0)
	seen := map[string]struct{}{}
	for _, manifest := range r.planningSkills.Manifests() {
		for _, capability := range manifest.RequiredTargetCapabilities {
			if _, ok := seen[capability]; ok {
				continue
			}
			seen[capability] = struct{}{}
			capabilities = append(capabilities, capability)
		}
	}
	sort.Strings(capabilities)
	selected, err := r.planningSkills.Select(agentskills.SelectionQuery{Intent: intent, TargetCapabilities: capabilities, Limit: maxNativeAgentSkills})
	if err != nil {
		return nil, err
	}
	result := make([]agentskills.Manifest, 0, len(selected))
	for _, manifest := range selected {
		if isFixtureSkill(manifest) {
			continue
		}
		result = append(result, manifest)
	}
	return result, nil
}

func (r *Runtime) selectDeploymentPlanningSkills(userText string) []agentskills.Manifest {
	byID := map[string]agentskills.Manifest{}
	for _, manifest := range r.planningSkills.Manifests() {
		if !isFixtureSkill(manifest) {
			byID[manifest.ID] = manifest
		}
	}
	recipe := "container-service-deploy"
	text := strings.ToLower(userText)
	for _, term := range []string{"source", "源码", "原始碼", "github", "gitlab", "systemd", "go service", "rust", "node.js", "python"} {
		if strings.Contains(text, term) {
			recipe = "source-build-systemd"
			break
		}
	}
	ids := []string{"project-intake-analyzer", "aws-target-advisor", recipe}
	result := make([]agentskills.Manifest, 0, maxNativeAgentSkills)
	for _, id := range ids {
		if manifest, ok := byID[id]; ok {
			result = append(result, manifest)
		}
	}
	return result
}

func (r *Runtime) resolveSelectedSkills(ids []string) ([]agentskills.Manifest, error) {
	manifests := r.planningSkills.Manifests()
	byID := make(map[string]agentskills.Manifest, len(manifests))
	for _, manifest := range manifests {
		if isFixtureSkill(manifest) {
			continue
		}
		// Built-in manifests are sorted by ID/version; the last version wins
		// if a future release ships more than one version of an ID.
		byID[manifest.ID] = manifest
	}
	result := make([]agentskills.Manifest, 0, len(ids))
	for _, id := range ids {
		manifest, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("planning skill %q is not built in", id)
		}
		result = append(result, manifest)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func selectedSkillIDs(config, params map[string]any) ([]string, bool) {
	for _, values := range []map[string]any{params, config} {
		if values == nil {
			continue
		}
		for _, key := range []string{"selected_skill_ids", "skill_ids", "selected_skills"} {
			raw, exists := values[key]
			if !exists {
				continue
			}
			return stringSliceParam(raw), true
		}
	}
	return nil, false
}

func selectedSkillIntent(ctx context.Context, config, params map[string]any) string {
	for _, values := range []map[string]any{params, config} {
		for _, key := range []string{"skill_intent", "intent"} {
			if value := normalizeRequestedSkillIntent(values[key]); value != "" {
				return value
			}
		}
	}
	_, _, userText := RequestContext(ctx)
	return classifySkillIntent(userText)
}

func normalizeRequestedSkillIntent(raw any) string {
	value := strings.ToLower(trimString(raw))
	if value == "" {
		return ""
	}
	switch value {
	case "deploy", "release", "health", "repair", "rollback", "placement", "sizing", "target", "intake", "runbook":
		return value
	default:
		if localized := classifySkillIntent(value); localized != "" {
			return localized
		}
		return value
	}
}

// classifySkillIntent deliberately uses ordered, bounded terms rather than
// arbitrary substrings. The order is part of the contract when a request
// mentions more than one concern (for example, "deploy and verify").
func classifySkillIntent(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	for _, pattern := range skillIntentPatterns {
		for _, term := range pattern.terms {
			if hasIntentWord(text, term) {
				return pattern.intent
			}
		}
		for _, phrase := range pattern.phrases {
			if strings.Contains(text, phrase) {
				return pattern.intent
			}
		}
	}
	return ""
}

type skillIntentPattern struct {
	intent  string
	terms   []string // English words, matched at Unicode word boundaries.
	phrases []string // Chinese/Japanese terms, which do not have spaces.
}

var skillIntentPatterns = []skillIntentPattern{
	{intent: "repair", terms: []string{"repair", "rollback", "revert", "recovery"}, phrases: []string{
		"修复", "修復", "回滚", "回滾", "恢复", "恢復", "復旧", "ロールバック", "切り戻し",
	}},
	{intent: "placement", terms: []string{"placement"}, phrases: []string{
		"部署位置", "部署地域", "地域选择", "地域選擇", "可用区", "可用區", "放置位置", "位置规划", "位置規劃",
		"配置場所", "配置先", "配置計画", "配置計畫", "リージョン選定", "アベイラビリティゾーン",
	}},
	{intent: "deploy", terms: []string{"deploy", "deployment", "release", "rollout"}, phrases: []string{
		"部署", "發布", "发布", "發佈", "上线", "上線", "デプロイ", "リリース", "ロールアウト",
	}},
	{intent: "health", terms: []string{"health", "healthy", "verify", "verification", "probe"}, phrases: []string{
		"健康检查", "健康檢查", "健康状态", "健康狀態", "健康监测", "健康監測", "探针", "探針", "探测", "探測", "存活检查", "存活檢查", "可用性检查", "可用性檢查",
		"ヘルスチェック", "健康チェック", "プローブ", "稼働確認", "状態確認",
	}},
	{intent: "sizing", terms: []string{"sizing", "resources", "resource"}, phrases: []string{
		"资源", "資源", "资源配置", "資源配置", "资源需求", "資源需求", "规格", "規格", "容量规划", "容量規劃",
		"サイジング", "リソース", "容量設計", "必要スペック",
	}},
	{intent: "target", terms: []string{"target", "aws", "ec2"}, phrases: []string{
		"目标", "目標", "目标实例", "目標實例", "目标环境", "目標環境", "対象インスタンス", "対象環境", "ターゲット",
	}},
	{intent: "intake", terms: []string{"intake", "project"}, phrases: []string{
		"项目", "項目", "專案", "项目接入", "項目接入", "项目资料", "專案資料", "プロジェクト", "案件登録", "要件整理",
	}},
	{intent: "runbook", terms: []string{"runbook"}, phrases: []string{
		"运行手册", "運行手冊", "运维手册", "運維手冊", "操作手册", "操作手冊", "操作手順書", "運用手順書", "手順書", "運用ガイド", "ランブック",
	}},
}

func hasIntentWord(text, term string) bool {
	start := 0
	for {
		offset := strings.Index(text[start:], term)
		if offset < 0 {
			return false
		}
		begin := start + offset
		end := begin + len(term)
		beforeOK := begin == 0 || !isIntentWordRune(prevRune(text, begin))
		afterOK := end == len(text) || !isIntentWordRune(nextRune(text, end))
		if beforeOK && afterOK {
			return true
		}
		start = begin + len(term)
	}
}

func isIntentWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func prevRune(text string, byteOffset int) rune {
	r, _ := utf8.DecodeLastRuneInString(text[:byteOffset])
	return r
}

func nextRune(text string, byteOffset int) rune {
	r, _ := utf8.DecodeRuneInString(text[byteOffset:])
	return r
}

func isFixtureSkill(manifest agentskills.Manifest) bool {
	for _, tag := range manifest.IntentTags {
		if tag == "fixture" {
			return true
		}
	}
	return false
}

func skillMetadata(manifest agentskills.Manifest) map[string]any {
	steps := make([]map[string]any, 0, len(manifest.Steps))
	for _, step := range manifest.Steps {
		steps = append(steps, map[string]any{
			"id":      step.ID,
			"kind":    step.Kind,
			"inputs":  append([]string(nil), step.Inputs...),
			"outputs": append([]string(nil), step.Outputs...),
		})
	}
	return map[string]any{
		"id":                           manifest.ID,
		"version":                      manifest.Version,
		"content_digest":               manifest.ContentDigest,
		"intent_tags":                  append([]string(nil), manifest.IntentTags...),
		"allowed_step_kinds":           append([]string(nil), manifest.AllowedStepKinds...),
		"required_target_capabilities": append([]string(nil), manifest.RequiredTargetCapabilities...),
		"planning_steps":               steps,
	}
}
