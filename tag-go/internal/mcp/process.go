package mcp

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// ProcessClient is an MCP Client bound to an external server subprocess. Closing
// it terminates the child. This is how TAG consumes third-party MCP servers
// (e.g. `npx @modelcontextprotocol/server-github`): spawn the process, speak
// JSON-RPC over its stdin/stdout.
type ProcessClient struct {
	*Client
	cmd   *exec.Cmd
	stdin io.Closer
}

// NewProcessClient spawns `command args...` and wires a Client to its stdio.
// The caller must Initialize() then use the client; Close() stops the child.
func NewProcessClient(ctx context.Context, command string, args ...string) (*ProcessClient, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting MCP server %q: %w", command, err)
	}
	return &ProcessClient{
		Client: NewClient(stdin, stdout),
		cmd:    cmd,
		stdin:  stdin,
	}, nil
}

// Close terminates the subprocess and reaps it.
func (p *ProcessClient) Close() error {
	if p.stdin != nil {
		p.stdin.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	}
	return nil
}
