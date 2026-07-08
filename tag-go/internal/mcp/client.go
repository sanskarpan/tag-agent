// Package mcp is a minimal Model Context Protocol client (Track B). It speaks
// JSON-RPC 2.0 over a stdio transport (newline-delimited JSON), enough to
// initialize a server, list its tools, and call them — the subset the agent
// loop needs to consume external MCP tool servers. Transport is an io.Reader/
// io.Writer pair, so it is fully testable in-process (no subprocess required).
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// ProtocolVersion is the MCP revision this client advertises.
const ProtocolVersion = "2024-11-05"

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// ToolDef is an MCP tool descriptor.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Client is a JSON-RPC MCP client over a stdio transport.
//
// A single long-lived reader goroutine owns the underlying bufio.Reader and
// routes each response frame to the waiting caller by id. This is deliberate:
// an earlier design spawned a per-call reader goroutine, so a call that timed
// out left its goroutine blocked on the shared Reader, and the next call
// spawned a second reader — two goroutines racing on one bufio.Reader, with the
// stale one stealing/discarding the next response. With one owner, a timed-out
// call simply deregisters its waiter; no competing reader is ever created.
type Client struct {
	w       io.Writer
	r       *bufio.Reader
	mu      sync.Mutex // guards nextID, waiters, inited, readErr, and w writes
	nextID  int
	inited  bool
	Timeout time.Duration // per-call read timeout (0 = wait forever)

	started bool
	waiters map[int]chan rpcResponse
	readErr error // set once the reader loop exits; fails subsequent calls fast
}

// NewClient wraps a transport (e.g. a subprocess's stdin/stdout). A default
// 120s per-call timeout guards against a server that accepts input but never
// replies.
func NewClient(w io.Writer, r io.Reader) *Client {
	return &Client{w: w, r: bufio.NewReader(r), nextID: 1, Timeout: 120 * time.Second, waiters: map[int]chan rpcResponse{}}
}

// readLoop is the single owner of c.r. It parses frames and delivers each to
// the registered waiter by id, dropping notifications and unclaimed frames. On
// any read error it records it and fails all current + future waiters.
func (c *Client) readLoop() {
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			for id, ch := range c.waiters {
				ch <- rpcResponse{Error: &rpcError{Code: -1, Message: err.Error()}}
				delete(c.waiters, id)
			}
			c.mu.Unlock()
			return
		}
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // skip unparseable / notification frames
		}
		c.mu.Lock()
		if ch, ok := c.waiters[resp.ID]; ok {
			ch <- resp
			delete(c.waiters, resp.ID)
		}
		c.mu.Unlock()
	}
}

// call sends a request and waits for the matching-id response, bounded by
// c.Timeout. Notifications and any unmatched frames are handled by readLoop.
func (c *Client) call(method string, params any, out any) error {
	c.mu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.mu.Unlock()
		return fmt.Errorf("mcp: transport closed: %w", err)
	}
	if !c.started {
		c.started = true
		go c.readLoop()
	}
	id := c.nextID
	c.nextID++
	ch := make(chan rpcResponse, 1)
	c.waiters[id] = ch
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	b, err := json.Marshal(req)
	if err != nil {
		delete(c.waiters, id)
		c.mu.Unlock()
		return err
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		delete(c.waiters, id)
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	deliver := func(resp rpcResponse) error {
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}

	if c.Timeout <= 0 {
		return deliver(<-ch)
	}
	select {
	case resp := <-ch:
		return deliver(resp)
	case <-time.After(c.Timeout):
		// Deregister so the reader doesn't deliver into an abandoned channel;
		// the buffered channel means a race with an in-flight delivery is safe.
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		return fmt.Errorf("mcp: timed out after %s waiting for response to %q", c.Timeout, method)
	}
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (c *Client) notify(method string, params any) error {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return fmt.Errorf("mcp: transport closed: %w", c.readErr)
	}
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// Initialize performs the MCP handshake: the initialize request/response
// followed by the notifications/initialized notification.
func (c *Client) Initialize(clientName string) error {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": "0.9.0-go"},
	}
	if err := c.call("initialize", params, nil); err != nil {
		return err
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		return err
	}
	c.inited = true
	return nil
}

// ListTools returns the server's advertised tools.
func (c *Client) ListTools() ([]ToolDef, error) {
	var out struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := c.call("tools/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallResult is the (text) result of a tool call.
type CallResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

// ContentBlock is one piece of MCP tool output.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Text concatenates all text content blocks.
func (r CallResult) Text() string {
	var s string
	for _, b := range r.Content {
		if b.Type == "text" {
			s += b.Text
		}
	}
	return s
}

// CallTool invokes a tool by name with arguments.
func (c *Client) CallTool(name string, args map[string]any) (*CallResult, error) {
	var out CallResult
	params := map[string]any{"name": name, "arguments": args}
	if err := c.call("tools/call", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
