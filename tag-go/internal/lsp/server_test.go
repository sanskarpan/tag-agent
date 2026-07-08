package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

// frame builds an LSP-framed message from a raw JSON string.
func frame(json string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(json), json)
}

// parseFrames reads every LSP-framed message from raw and returns the decoded
// JSON bodies.
func parseFrames(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	rest := raw
	for len(rest) > 0 {
		sep := []byte("\r\n\r\n")
		idx := bytes.Index(rest, sep)
		if idx < 0 {
			t.Fatalf("no header separator in remaining bytes: %q", rest)
		}
		header := string(rest[:idx])
		var length int
		for _, line := range strings.Split(header, "\r\n") {
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				v := strings.TrimSpace(line[len("content-length:"):])
				n, err := strconv.Atoi(v)
				if err != nil {
					t.Fatalf("bad content-length %q: %v", v, err)
				}
				length = n
			}
		}
		bodyStart := idx + len(sep)
		if bodyStart+length > len(rest) {
			t.Fatalf("body length %d exceeds available bytes", length)
		}
		body := rest[bodyStart : bodyStart+length]
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("unmarshal body %q: %v", body, err)
		}
		out = append(out, m)
		rest = rest[bodyStart+length:]
	}
	return out
}

func run(t *testing.T, input string) []map[string]any {
	t.Helper()
	srv := NewServer([]string{"coder", "reviewer"})
	var out bytes.Buffer
	if err := srv.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	return parseFrames(t, out.Bytes())
}

func TestInitializeCapabilities(t *testing.T) {
	msgs := run(t, frame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	result, ok := msgs[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result object: %v", msgs[0])
	}
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("no capabilities: %v", result)
	}
	if caps["hoverProvider"] != true {
		t.Errorf("hoverProvider not true: %v", caps["hoverProvider"])
	}
	if caps["textDocumentSync"] != float64(1) {
		t.Errorf("textDocumentSync != 1: %v", caps["textDocumentSync"])
	}
	info, ok := result["serverInfo"].(map[string]any)
	if !ok || info["name"] != serverName {
		t.Errorf("serverInfo missing/wrong: %v", result["serverInfo"])
	}
}

func TestHoverReturnsContents(t *testing.T) {
	msgs := run(t, frame(`{"jsonrpc":"2.0","id":2,"method":"textDocument/hover","params":{}}`))
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	result, ok := msgs[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", msgs[0])
	}
	contents, ok := result["contents"].(map[string]any)
	if !ok {
		t.Fatalf("no contents object: %v", result)
	}
	if contents["kind"] != "markdown" {
		t.Errorf("kind != markdown: %v", contents["kind"])
	}
	val, _ := contents["value"].(string)
	if val == "" {
		t.Errorf("empty hover value")
	}
	if !strings.Contains(val, "coder") {
		t.Errorf("hover should mention configured profiles, got: %q", val)
	}
}

func TestShutdownReturnsNull(t *testing.T) {
	msgs := run(t, frame(`{"jsonrpc":"2.0","id":3,"method":"shutdown","params":{}}`))
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	if _, present := msgs[0]["result"]; !present {
		// result must be present and null
		t.Fatalf("shutdown response missing result key: %v", msgs[0])
	}
	if msgs[0]["result"] != nil {
		t.Errorf("shutdown result should be null, got: %v", msgs[0]["result"])
	}
	if _, hasErr := msgs[0]["error"]; hasErr {
		t.Errorf("shutdown should not error: %v", msgs[0])
	}
}

func TestUnknownMethodMethodNotFound(t *testing.T) {
	msgs := run(t, frame(`{"jsonrpc":"2.0","id":4,"method":"no/suchMethod","params":{}}`))
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	errObj, ok := msgs[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object: %v", msgs[0])
	}
	if errObj["code"] != float64(-32601) {
		t.Errorf("expected code -32601, got %v", errObj["code"])
	}
}

func TestUnknownNotificationIgnored(t *testing.T) {
	// No "id" => notification; unknown notifications produce no response.
	msgs := run(t, frame(`{"jsonrpc":"2.0","method":"custom/notify","params":{}}`))
	if len(msgs) != 0 {
		t.Fatalf("expected no responses for unknown notification, got %d: %v", len(msgs), msgs)
	}
}

func TestInitializedNotificationNoResponse(t *testing.T) {
	msgs := run(t, frame(`{"jsonrpc":"2.0","method":"initialized","params":{}}`))
	if len(msgs) != 0 {
		t.Fatalf("expected no response to initialized, got %d", len(msgs))
	}
}

// TestConcatenatedMessages feeds two framed messages back-to-back in one
// stream and asserts both are handled in order.
func TestConcatenatedMessages(t *testing.T) {
	input := frame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
		frame(`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`)
	msgs := run(t, input)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 responses, got %d: %v", len(msgs), msgs)
	}
	if msgs[0]["id"] != float64(1) {
		t.Errorf("first response id != 1: %v", msgs[0]["id"])
	}
	if _, ok := msgs[0]["result"].(map[string]any); !ok {
		t.Errorf("first response should be initialize result: %v", msgs[0])
	}
	if msgs[1]["id"] != float64(2) || msgs[1]["result"] != nil {
		t.Errorf("second response should be shutdown null: %v", msgs[1])
	}
}

// TestExitStopsLoop asserts exit halts Serve and any trailing bytes are not
// processed.
func TestExitStopsLoop(t *testing.T) {
	input := frame(`{"jsonrpc":"2.0","method":"exit"}`) +
		frame(`{"jsonrpc":"2.0","id":9,"method":"initialize","params":{}}`)
	msgs := run(t, input)
	if len(msgs) != 0 {
		t.Fatalf("expected no responses after exit, got %d: %v", len(msgs), msgs)
	}
}

// TestSplitAcrossReads verifies the frame reader reassembles a message that
// arrives in multiple chunks (header split from body, body split mid-way).
func TestSplitAcrossReads(t *testing.T) {
	full := frame(`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}`)
	// A reader that yields one byte at a time exercises partial reads.
	srv := NewServer(nil)
	var out bytes.Buffer
	if err := srv.Serve(iotest_oneByteReader(full), &out); err != nil {
		t.Fatalf("Serve error: %v", err)
	}
	msgs := parseFrames(t, out.Bytes())
	if len(msgs) != 1 || msgs[0]["id"] != float64(7) {
		t.Fatalf("expected single initialize response, got %v", msgs)
	}
}

// iotest_oneByteReader returns a reader delivering s one byte per Read call.
func iotest_oneByteReader(s string) io.Reader {
	return &oneByteReader{data: []byte(s)}
}

type oneByteReader struct {
	data []byte
	pos  int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func TestReadMessageRejectsHugeContentLength(t *testing.T) {
	// A giant Content-Length must NOT panic/OOM — it must return an error.
	frame := "Content-Length: 9223372036854775807\r\n\r\n{}"
	_, err := readMessage(bufio.NewReader(strings.NewReader(frame)))
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("huge Content-Length should be rejected, got err=%v", err)
	}
}
