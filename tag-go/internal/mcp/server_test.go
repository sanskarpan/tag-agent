package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// newPipedPair wires the real Client (client.go) to a real Server (server.go)
// via two in-process io.Pipe pairs, proving the two halves interoperate over
// the wire format without any subprocess or network.
func newPipedPair(t *testing.T, srv *Server) *Client {
	t.Helper()
	// client writes -> serverIn; server writes -> clientIn (client reads)
	serverInR, serverInW := io.Pipe()
	clientInR, clientInW := io.Pipe()
	go func() {
		_ = srv.Serve(serverInR, clientInW)
	}()
	return NewClient(serverInW, clientInR)
}

func testServer() *Server {
	s := NewServer("tag-test-server")
	s.Register("upper", "uppercase text", map[string]any{"type": "object"},
		func(args map[string]any) (string, error) {
			txt, _ := args["text"].(string)
			return strings.ToUpper(txt), nil
		})
	return s
}

func TestServerHandshakeAndList(t *testing.T) {
	c := newPipedPair(t, testServer())
	if err := c.Initialize("tag-client"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !c.inited {
		t.Error("client should be marked initialized after server handshake")
	}
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "upper" {
		t.Fatalf("expected the upper tool, got %+v", tools)
	}
	if tools[0].Description != "uppercase text" {
		t.Errorf("description not round-tripped: %q", tools[0].Description)
	}
}

func TestServerCallTool(t *testing.T) {
	c := newPipedPair(t, testServer())
	if err := c.Initialize("tag-client"); err != nil {
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

func TestServerUnknownToolError(t *testing.T) {
	c := newPipedPair(t, testServer())
	if err := c.Initialize("tag-client"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallTool("ghost", nil); err == nil {
		t.Error("calling an unknown tool should return a JSON-RPC error")
	}
}

func TestServerIgnoresNotifications(t *testing.T) {
	// A notification (method, no id) must produce NO response frame.
	srv := NewServer("t")
	var out bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	if err := srv.Serve(in, &out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("notification must get no response, got %q", out.String())
	}
}

func TestServerEchoesStringID(t *testing.T) {
	// A request with a spec-legal STRING id must get a response echoing that id
	// (previously the int-typed id silently dropped it → client deadlock).
	srv := NewServer("t")
	var out bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","id":"abc","method":"initialize","params":{}}` + "\n")
	if err := srv.Serve(in, &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result map[string]any  `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("response not parseable: %q err=%v", out.String(), err)
	}
	if string(resp.ID) != `"abc"` {
		t.Errorf("string id must be echoed verbatim, got %s", resp.ID)
	}
	if resp.Result["protocolVersion"] == nil {
		t.Errorf("initialize should return a protocolVersion: %v", resp.Result)
	}
}

func TestClientTimesOutOnSilentServer(t *testing.T) {
	// A server that accepts input but never replies must not hang the client.
	// Drain the client's writes (so Write doesn't block on the unbuffered pipe),
	// but never write a response back — the read must time out.
	serverInR, serverInW := io.Pipe() // client writes here
	clientInR, _ := io.Pipe()         // client reads here; nobody ever writes
	go io.Copy(io.Discard, serverInR) // drain requests into the void
	c := NewClient(serverInW, clientInR)
	c.Timeout = 100 * time.Millisecond
	err := c.Initialize("x")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected a timeout error, got %v", err)
	}
}

// Regression for issue #523: after a call times out, the next call must still
// work and must NOT race on the shared reader. Run under -race. Previously a
// per-call reader goroutine leaked past the timeout and a second reader raced
// it, stealing the next response. The first (late) response for the timed-out
// call must be dropped, and the second call must receive its own reply.
func TestClientTimeoutThenNextCallSucceeds(t *testing.T) {
	reqR, reqW := io.Pipe()   // client writes requests here
	respR, respW := io.Pipe() // client reads responses here
	c := NewClient(reqW, respR)
	c.Timeout = 80 * time.Millisecond

	// Controlled "server": read framed requests, respond per-id on a schedule.
	go func() {
		br := bufio.NewReader(reqR)
		// First request (id=1): reply only AFTER the client's timeout elapses,
		// so the client has already given up — this stale frame must be dropped.
		line1, _ := br.ReadBytes('\n')
		var r1 rpcRequest
		json.Unmarshal(line1, &r1)
		time.Sleep(160 * time.Millisecond)
		respW.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"stale":true}}`+"\n", r1.ID)))
		// Second request (id=2): reply promptly.
		line2, _ := br.ReadBytes('\n')
		var r2 rpcRequest
		json.Unmarshal(line2, &r2)
		respW.Write([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`+"\n", r2.ID)))
	}()

	if err := c.Initialize("x"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("first call should time out, got %v", err)
	}
	// The second call must succeed despite the stale first response arriving late.
	if _, err := c.ListTools(); err != nil {
		t.Fatalf("second call after a timeout should succeed, got %v", err)
	}
}
