package nativeagent

import "context"

func (r *Runtime) skillsList(context.Context) (map[string]any, error) {
	return map[string]any{"skills": []map[string]any{}}, nil
}

func (r *Runtime) enabledSkillsPrompt(context.Context, map[string]any) string { return "" }
