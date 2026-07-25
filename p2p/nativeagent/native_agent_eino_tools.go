package nativeagent

import (
	"context"
	"encoding/json"
	"strings"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

func (r *Runtime) enabledEinoTools(ctx context.Context, config map[string]any, params map[string]any) ([]einotool.BaseTool, func(), error) {
	nativeTools, cleanup, err := r.enabledNativeEinoTools(ctx, config, params)
	if err != nil {
		return nil, nil, err
	}
	return nativeTools, cleanup, nil
}

func (r *Runtime) enabledNativeEinoTools(ctx context.Context, config map[string]any, params map[string]any) ([]einotool.BaseTool, func(), error) {
	tools := make([]einotool.BaseTool, 0)
	for _, nativeTool := range r.enabledTools(ctx, config, params) {
		if !embeddedDirextalkTool(nativeTool.Name) {
			continue
		}
		info, err := nativeToolInfo(nativeTool)
		if err != nil {
			return nil, nil, err
		}
		tools = append(tools, &einoNativeTool{native: nativeTool, info: info})
	}
	return tools, func() {}, nil
}

func nativeToolInfo(nativeTool Tool) (*schema.ToolInfo, error) {
	info := &schema.ToolInfo{
		Name: nativeTool.Name,
		Desc: nativeTool.Description,
	}
	if len(nativeTool.Parameters) == 0 {
		return info, nil
	}
	data, err := json.Marshal(nativeTool.Parameters)
	if err != nil {
		return nil, err
	}
	js := &jsonschema.Schema{}
	if err := json.Unmarshal(data, js); err != nil {
		return nil, err
	}
	info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(js)
	return info, nil
}

type einoNativeTool struct {
	native Tool
	info   *schema.ToolInfo
}

func (t *einoNativeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *einoNativeTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...einotool.Option) (string, error) {
	var args map[string]any
	if strings.TrimSpace(argumentsInJSON) != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return jsonValue(map[string]any{"error": err.Error()}), nil
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	result, err := t.native.Handler(ctx, args)
	if err != nil {
		return jsonValue(map[string]any{"error": err.Error()}), nil
	}
	return jsonValue(map[string]any{"result": result}), nil
}
