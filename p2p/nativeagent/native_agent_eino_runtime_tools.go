package nativeagent

import einotool "github.com/cloudwego/eino/components/tool"

// Runtime shell and dynamically configured CLI tools are intentionally absent
// from the embedded Native Agent release.
func (r *Runtime) enabledRuntimeEinoTools(map[string]any, map[string]any) []einotool.BaseTool {
	return nil
}
