package nativeagent

import (
	"context"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Embedded Native Agent never opens third-party MCP transports. The public
// POST /mcp endpoint has its own bearer-authenticated service boundary.
func (r *Runtime) enabledOfficialMCPTools(context.Context, map[string]any, map[string]any) ([]einotool.BaseTool, func(), error) {
	return nil, func() {}, nil
}

func (r *Runtime) discoverOfficialMCPTools(context.Context, map[string]any) ([]any, error) {
	return nil, embeddedExtensionsForbidden()
}

func (r *Runtime) openMCPClientSession(context.Context, map[string]any) (*mcp.ClientSession, error) {
	return nil, embeddedExtensionsForbidden()
}

func (r *Runtime) mcpTransport(map[string]any) (mcp.Transport, error) {
	return nil, embeddedExtensionsForbidden()
}
