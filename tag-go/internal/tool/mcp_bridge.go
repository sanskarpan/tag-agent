package tool

import (
	"context"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/mcp"
	"github.com/tag-agent/tag/internal/permission"
)

// RegisterMCP adds an MCP server's tools to the agent registry, namespaced as
// "mcp__<name>". Each call proxies through the MCP client to the server. This is
// how the native agent loop consumes external MCP tool servers.
//
// MCP tools are third-party code with unknown side effects, so they go through
// the same consent gate as the built-ins (opts.Guard; nil = the secure default
// policy). They have no known subject kind, so only tool-name rules apply and
// the built-in catch-all `ask` governs them — which means denied when headless
// unless explicitly allowed.
func RegisterMCP(reg *agent.Registry, client *mcp.Client, serverName string, opts Options) error {
	tools, err := client.ListTools()
	if err != nil {
		return err
	}
	g := opts.guard()
	for _, mt := range tools {
		mt := mt // capture
		name := "mcp__" + serverName + "__" + mt.Name
		reg.Add(permission.Wrap(g, agent.Tool{
			Def: llm.ToolDef{Name: name, Description: mt.Description, Schema: mt.InputSchema},
			Exec: func(ctx context.Context, in map[string]any) (string, error) {
				res, err := client.CallTool(mt.Name, in)
				if err != nil {
					return "", err
				}
				return res.Text(), nil
			},
		}, permission.NoSubject))
	}
	return nil
}
