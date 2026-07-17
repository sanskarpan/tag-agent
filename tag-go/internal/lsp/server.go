// Package lsp implements a minimal Language Server Protocol (LSP) server for
// TAG over stdio. It speaks JSON-RPC 2.0 using LSP's "Content-Length" header
// framing (NOT newline-delimited): each message is prefixed by a
// "Content-Length: <n>\r\n\r\n" header followed by exactly <n> bytes of JSON.
//
// The server handles the LSP lifecycle (initialize, initialized, shutdown,
// exit) plus textDocument/hover, which surfaces TAG-contextual help (including
// the configured profile names when available).
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tag-agent/tag/internal/version"
)

// serverName is reported back to the client in the initialize response's
// serverInfo; the version comes from internal/version so the LSP server reports
// the same build version as the rest of TAG (#537b), matching MCP.
const serverName = "tag-lsp"

// Server is a minimal TAG LSP server. It is transport-agnostic: Serve reads
// framed messages from an io.Reader and writes framed responses to an
// io.Writer.
type Server struct {
	// Profiles holds the configured TAG profile names, surfaced in hover
	// content. It may be empty.
	Profiles []string

	shutdown bool
}

// NewServer constructs a Server with the given profile names.
func NewServer(profiles []string) *Server {
	return &Server{Profiles: profiles}
}

// request is an incoming JSON-RPC message. Notifications omit "id".
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is an outgoing JSON-RPC message. MarshalJSON emits "result" for
// success responses (even when null, as LSP shutdown requires) and "error"
// for failures, never both.
type response struct {
	JSONRPC string
	ID      json.RawMessage
	Result  any
	Error   *rpcError
}

func (r *response) MarshalJSON() ([]byte, error) {
	m := map[string]any{"jsonrpc": r.JSONRPC, "id": r.ID}
	if r.Error != nil {
		m["error"] = r.Error
	} else {
		m["result"] = r.Result // present even when nil -> JSON null
	}
	return json.Marshal(m)
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the read/dispatch/write loop until EOF, an "exit" notification, or
// an unrecoverable framing error. It returns nil on a clean end of stream.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	for {
		body, err := readMessage(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		var req request
		if err := json.Unmarshal(body, &req); err != nil {
			// Malformed JSON with no recoverable id: skip it.
			continue
		}
		resp, stop := s.handle(&req)
		if resp != nil {
			if err := writeMessage(w, resp); err != nil {
				return err
			}
		}
		if stop {
			return nil
		}
	}
}

// handle dispatches a single request. It returns the response to write (or nil
// for notifications) and whether the loop should stop (on "exit").
func (s *Server) handle(req *request) (*response, bool) {
	hasID := len(req.ID) > 0 && string(req.ID) != "null"

	switch req.Method {
	case "initialize":
		return s.ok(req.ID, s.initializeResult()), false
	case "initialized":
		return nil, false // notification, no response
	case "shutdown":
		s.shutdown = true
		return s.ok(req.ID, nil), false
	case "exit":
		return nil, true // notification, stop the loop
	case "textDocument/hover":
		return s.ok(req.ID, s.hoverResult(req.Params)), false
	case "$/setTrace", "$/cancelRequest":
		return nil, false // ignore
	default:
		if hasID {
			return &response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &rpcError{
					Code:    -32601,
					Message: fmt.Sprintf("Method not found: %s", req.Method),
				},
			}, false
		}
		return nil, false // unknown notification: ignore
	}
}

// ok builds a successful JSON-RPC response.
func (s *Server) ok(id json.RawMessage, result any) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: result}
}

// initializeResult reports the server's capabilities and identity.
func (s *Server) initializeResult() map[string]any {
	return map[string]any{
		"capabilities": map[string]any{
			"textDocumentSync": 1, // full document sync
			"hoverProvider":    true,
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": version.Version,
		},
	}
}

// hoverResult returns a Hover with markdown contents describing TAG and the
// configured profiles.
func (s *Server) hoverResult(_ json.RawMessage) map[string]any {
	var b strings.Builder
	b.WriteString("**TAG — the Agent Gateway**\n\n")
	b.WriteString("Route prompts through TAG profiles from your editor.\n")
	if len(s.Profiles) > 0 {
		b.WriteString("\nConfigured profiles:\n")
		for _, p := range s.Profiles {
			b.WriteString("- `" + p + "`\n")
		}
	} else {
		b.WriteString("\nNo profiles configured.\n")
	}
	return map[string]any{
		"contents": map[string]any{
			"kind":  "markdown",
			"value": b.String(),
		},
	}
}

// readMessage reads one LSP-framed message from br: it parses the header block
// (terminated by a blank line), finds Content-Length, then reads exactly that
// many bytes of body. Returns io.EOF only when the stream ends cleanly at a
// message boundary.
func readMessage(br *bufio.Reader) ([]byte, error) {
	contentLength := -1
	sawHeader := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF && !sawHeader && line == "" {
				return nil, io.EOF
			}
			if err == io.EOF {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		sawHeader = true
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			// End of headers.
			break
		}
		if idx := strings.IndexByte(trimmed, ':'); idx >= 0 {
			key := strings.ToLower(strings.TrimSpace(trimmed[:idx]))
			val := strings.TrimSpace(trimmed[idx+1:])
			if key == "content-length" {
				n, err := strconv.Atoi(val)
				if err != nil {
					return nil, fmt.Errorf("invalid Content-Length: %q", val)
				}
				contentLength = n
			}
		}
	}

	if contentLength < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	// Bound the declared length so a malicious/garbage header can't force a huge
	// allocation (make([]byte, huge) panics with "makeslice: len out of range"
	// or OOMs). 64 MiB is far beyond any real LSP message.
	const maxContentLength = 64 * 1024 * 1024
	if contentLength > maxContentLength {
		return nil, fmt.Errorf("Content-Length %d exceeds maximum %d", contentLength, maxContentLength)
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(br, body); err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

// writeMessage encodes msg as JSON and writes it with LSP header framing.
func writeMessage(w io.Writer, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
