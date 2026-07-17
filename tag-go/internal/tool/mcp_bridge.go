package tool

import (
	"context"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/mcp"
)

// RegisterMCP adds an MCP server's tools to the agent registry, namespaced as
// "mcp__<name>". Each call proxies through the MCP client to the server. This is
// how the native agent loop consumes external MCP tool servers.
func RegisterMCP(reg *agent.Registry, client *mcp.Client, serverName string) error {
	tools, err := client.ListTools()
	if err != nil {
		return err
	}
	for _, mt := range tools {
		mt := mt // capture
		name := "mcp__" + serverName + "__" + mt.Name
		reg.Add(agent.Tool{
			Def: llm.ToolDef{Name: name, Description: mt.Description, Schema: mt.InputSchema},
			Exec: func(ctx context.Context, in map[string]any) (string, error) {
				res, err := client.CallTool(mt.Name, in)
				if err != nil {
					return "", err
				}
				return res.Text(), nil
			},
		})
	}
	return nil
}
