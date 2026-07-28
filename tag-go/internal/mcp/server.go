package mcp

// This file implements the MCP SERVER side, symmetric to the client in
// client.go. It reads newline-delimited JSON-RPC 2.0 requests from an
// io.Reader and writes responses to an io.Writer (stdio transport), handling
// the same three methods the client speaks: "initialize", "tools/list", and
// "tools/call". Tools are registered in-process via Register, so TAG can
// expose its own capabilities to external MCP clients. It reuses ToolDef,
// CallResult, and ContentBlock from client.go (same package).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"

	"github.com/tag-agent/tag/internal/version"
)

// JSON-RPC error codes used by the server.
const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
)

// Handler executes a registered tool with decoded arguments and returns the
// text result (or an error, surfaced to the client as a JSON-RPC error).
type Handler func(args map[string]any) (string, error)

// registeredTool couples a tool descriptor with its handler.
type registeredTool struct {
	def ToolDef
	h   Handler
}

// Server is a JSON-RPC MCP server over a stdio transport. It is the mirror of
// Client: where the client sends requests and reads responses, the server
// reads requests and writes responses.
type Server struct {
	name  string
	tools map[string]registeredTool
	order []string
}

// NewServer creates a server that advertises the given name in serverInfo.
func NewServer(name string) *Server {
	return &Server{name: name, tools: map[string]registeredTool{}}
}

// Register adds a tool. schema is the JSON Schema for the tool's arguments
// (the MCP "inputSchema"); pass nil for a schema-less object tool.
func (s *Server) Register(name, description string, schema map[string]any, h Handler) {
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	if _, exists := s.tools[name]; !exists {
		s.order = append(s.order, name)
	}
	s.tools[name] = registeredTool{
		def: ToolDef{Name: name, Description: description, InputSchema: schema},
		h:   h,
	}
}

// Serve reads requests until EOF, dispatching each to the matching handler. It
// returns nil on a clean EOF and any non-EOF read error otherwise.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	for {
		// Bounded read: a peer must not be able to OOM us with a newline-less
		// stream (see maxFrameBytes).
		line, err := readFrame(br)
		if len(line) > 0 {
			if werr := s.handleLine(line, w); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// serverRequest is an inbound frame. ID stays raw so it can be echoed back
// verbatim (string OR number, per JSON-RPC 2.0) and its ABSENCE detected — a
// request without an id is a notification and must receive no response. Params
// stays raw so the specific method handler decodes it into its own shape.
type serverRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// serverResponse mirrors rpcResponse but keeps ID raw for verbatim echo.
type serverResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// handleLine parses one frame and writes its response frame (unless it's a notification).
func (s *Server) handleLine(line []byte, w io.Writer) error {
	// A blank/whitespace-only frame is not a message at all — no response.
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var req serverRequest
	if err := json.Unmarshal(line, &req); err != nil {
		// JSON-RPC 2.0 §5.1: a parse error MUST be answered with id:null and
		// code -32700. Silently dropping the frame hangs a conforming client —
		// TAG's own client sits for its full 120s timeout — and desynchronises
		// the response stream.
		return writeFrame(w, serverResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &rpcError{Code: errParse, Message: "Parse error: " + err.Error()},
		})
	}
	if req.Method == "" {
		// Valid JSON but not a request. With an id it is an Invalid Request and
		// must be answered; without one it is indistinguishable from a
		// notification, so stay silent.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			return nil
		}
		return writeFrame(w, serverResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: errInvalidRequest, Message: "Invalid Request: missing method"},
		})
	}
	// A notification has no id — dispatch for side effects but send NO response
	// (responding to a notification violates JSON-RPC 2.0; real MCP clients send
	// `notifications/initialized` after the handshake).
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil
	}

	result, rpcErr := s.dispatch(req.Method, req.Params)
	resp := serverResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		b, err := json.Marshal(result)
		if err != nil {
			resp.Error = &rpcError{Code: errInternal, Message: err.Error()}
		} else {
			resp.Result = b
		}
	}
	return writeFrame(w, resp)
}

// writeFrame marshals one response and writes it as a newline-delimited frame.
func writeFrame(w io.Writer, resp serverResponse) error {
	out, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = w.Write(append(out, '\n'))
	return err
}

// dispatch routes a method to its result or a JSON-RPC error.
func (s *Server) dispatch(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": version.Version},
		}, nil
	case "tools/list":
		tools := make([]ToolDef, 0, len(s.order))
		for _, name := range s.order {
			tools = append(tools, s.tools[name].def)
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		return s.callTool(params)
	default:
		return nil, &rpcError{Code: errMethodNotFound, Message: "unknown method: " + method}
	}
}

// callTool decodes the call params and invokes the named handler.
func (s *Server) callTool(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: errInvalidParams, Message: err.Error()}
		}
	}
	tool, ok := s.tools[p.Name]
	if !ok {
		return nil, &rpcError{Code: errMethodNotFound, Message: "unknown tool: " + p.Name}
	}
	text, err := tool.h(p.Arguments)
	if err != nil {
		return CallResult{
			Content: []ContentBlock{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}
	return CallResult{Content: []ContentBlock{{Type: "text", Text: text}}}, nil
}
