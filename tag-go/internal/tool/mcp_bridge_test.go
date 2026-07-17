package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/mcp"
)

// minimal in-process MCP server exposing an "echo_upper" tool
func mcpMock(in io.Reader, out io.Writer) {
	r := bufio.NewReader(in)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcp.ProtocolVersion}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "echo_upper", "description": "upper", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			var p struct {
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			txt, _ := p.Arguments["text"].(string)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": strings.ToUpper(txt)}}}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		out.Write(append(b, '\n'))
	}
}

// scripted provider: turn 1 calls the MCP tool, turn 2 returns final text
type twoTurn struct{ n int }

func (twoTurn) Name() string { return "twoturn" }
func (p *twoTurn) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event, 4)
	i := p.n
	p.n++
	go func() {
		defer close(ch)
		if i == 0 {
			ch <- llm.Event{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "mcp__demo__echo_upper", Input: map[string]any{"text": "via mcp"}}}
		} else {
			ch <- llm.Event{Type: llm.EventTextDelta, Text: "final"}
		}
		ch <- llm.Event{Type: llm.EventFinish}
	}()
	return ch, nil
}

func TestAgentLoopUsesMCPTool(t *testing.T) {
	serverInR, serverInW := io.Pipe()
	clientInR, clientInW := io.Pipe()
	go mcpMock(serverInR, clientInW)
	client := mcp.NewClient(serverInW, clientInR)
	if err := client.Initialize("tag"); err != nil {
		t.Fatal(err)
	}
	reg := agent.NewRegistry()
	if err := RegisterMCP(reg, client, "demo"); err != nil {
		t.Fatal(err)
	}
	// the MCP tool should be registered under the namespaced name
	found := false
	for _, d := range reg.Defs() {
		if d.Name == "mcp__demo__echo_upper" {
			found = true
		}
	}
	if !found {
		t.Fatal("MCP tool not registered into the agent registry")
	}
	// drive the loop; it should call the MCP tool and get "VIA MCP"
	l := &agent.Loop{Provider: &twoTurn{}, Tools: reg}
	res, err := l.Run(context.Background(), "go", agent.Options{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) == 0 || len(res.Steps[0].ToolCalls) == 0 || res.Steps[0].ToolCalls[0].Result != "VIA MCP" {
		t.Errorf("agent should invoke the MCP tool and get VIA MCP: %+v", res.Steps)
	}
}
