package nativeagent

import "context"

func (r *Runtime) mcpServersList(context.Context) (map[string]any, error) {
	return map[string]any{"servers": []map[string]any{}}, nil
}

func (r *Runtime) discoverMCPTools(context.Context, map[string]any) ([]any, error) {
	return nil, embeddedExtensionsForbidden()
}
