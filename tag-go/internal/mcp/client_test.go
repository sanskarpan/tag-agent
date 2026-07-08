package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// mockServer implements a tiny MCP server over the given reader/writer: it
// handles initialize, tools/list, and tools/call (an "upper" tool).
func mockServer(t *testing.T, in io.Reader, out io.Writer) {
	t.Helper()
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
			result = map[string]any{"protocolVersion": ProtocolVersion, "serverInfo": map[string]any{"name": "mock"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{"name": "upper", "description": "uppercase text", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			if p.Name == "upper" {
				txt, _ := p.Arguments["text"].(string)
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": strings.ToUpper(txt)}}}
			} else {
				// error response
				resp := map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "unknown tool"}}
				b, _ := json.Marshal(resp)
				out.Write(append(b, '\n'))
				continue
			}
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		b, _ := json.Marshal(resp)
		out.Write(append(b, '\n'))
	}
}

func newPipedClient(t *testing.T) *Client {
	// client writes -> serverIn; server writes -> clientIn (client reads)
	serverInR, serverInW := io.Pipe()
	clientInR, clientInW := io.Pipe()
	go mockServer(t, serverInR, clientInW)
	return NewClient(serverInW, clientInR)
}

func TestMCPHandshakeAndTools(t *testing.T) {
	c := newPipedClient(t)
	if err := c.Initialize("tag-test"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !c.inited {
		t.Error("client should be marked initialized")
	}
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "upper" {
		t.Fatalf("expected the upper tool, got %+v", tools)
	}
}

func TestMCPCallTool(t *testing.T) {
	c := newPipedClient(t)
	if err := c.Initialize("tag-test"); err != nil {
		t.Fatal(err)
	}
	res, err := c.CallTool("upper", map[string]any{"text": "hello mcp"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Text() != "HELLO MCP" {
		t.Errorf("tool result wrong: %q", res.Text())
	}
}

func TestMCPUnknownToolError(t *testing.T) {
	c := newPipedClient(t)
	c.Initialize("tag-test")
	if _, err := c.CallTool("ghost", nil); err == nil {
		t.Error("calling an unknown tool should return the server error")
	}
}
