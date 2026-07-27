package mcp

import (
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// endlessReader emits bytes forever and never a newline — what a hostile or
// merely buggy MCP peer looks like on the wire. It counts what it served so a
// test can assert the consumer stopped instead of buffering it all.
type endlessReader struct{ served int64 }

func (e *endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	e.served += int64(len(p))
	return len(p), nil
}

// TestServerRejectsUnboundedFrame pins F3 (server half): Serve read frames with
// bufio.Reader.ReadBytes('\n') and no cap, so a newline-less stream drove RSS
// past 1 GB and climbing. TAG's purpose here is consuming THIRD-PARTY MCP
// servers, so an untrusted peer must not OOM the host.
func TestServerRejectsUnboundedFrame(t *testing.T) {
	s := NewServer("tag")
	s.Register("echo", "echo", nil, func(a map[string]any) (string, error) { return "", nil })
	er := &endlessReader{}
	done := make(chan error, 1)
	go func() { done <- s.Serve(er, io.Discard) }()

	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("Serve never returned; it consumed %d bytes of a newline-less stream", er.served)
	}
	if err == nil {
		t.Fatal("Serve must return an error on an over-long frame, not succeed")
	}
	if !strings.Contains(err.Error(), "frame") {
		t.Errorf("error should name the frame limit: %v", err)
	}
	if er.served > 64<<20 {
		t.Errorf("buffered %d bytes before giving up — the cap is not effective", er.served)
	}
	assertHeapUnder(t, 256<<20)
}

// TestClientRejectsUnboundedFrame pins F3 (client half): readLoop had the same
// uncapped ReadBytes('\n'), and drove RSS to ~1.75 GB against a hostile server.
func TestClientRejectsUnboundedFrame(t *testing.T) {
	er := &endlessReader{}
	c := NewClient(io.Discard, er)
	c.Timeout = 20 * time.Second
	var out map[string]any
	errCh := make(chan error, 1)
	go func() { errCh <- c.call("tools/list", nil, &out) }()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("call must fail when the peer never terminates a frame")
		}
	case <-time.After(25 * time.Second):
		t.Fatalf("client never gave up; it buffered %d bytes", er.served)
	}
	if er.served > 64<<20 {
		t.Errorf("buffered %d bytes before giving up — the cap is not effective", er.served)
	}
	assertHeapUnder(t, 256<<20)
}

func assertHeapUnder(t *testing.T, limit uint64) {
	t.Helper()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.HeapAlloc > limit {
		t.Errorf("heap in use %d bytes exceeds %d — the frame was buffered whole", m.HeapAlloc, limit)
	}
}

// TestServerAnswersParseErrors pins F4: JSON-RPC 2.0 §5.1 requires an
// unparseable frame to be answered with id:null / code -32700. Silently
// dropping it hangs a conforming client (TAG's own client waits its full
// timeout), and desynchronises the response stream.
func TestServerAnswersParseErrors(t *testing.T) {
	s := NewServer("tag")
	s.Register("echo", "echo", nil, func(a map[string]any) (string, error) { return "hi", nil })
	in := "{bad json\n" + `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"
	var out strings.Builder
	if err := s.Serve(strings.NewReader(in), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 response frames (parse error + tools/list), got %d:\n%s", len(lines), out.String())
	}
	var pe struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &pe); err != nil {
		t.Fatalf("parse-error frame is not JSON: %v (%s)", err, lines[0])
	}
	if pe.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q", pe.JSONRPC)
	}
	if string(pe.ID) != "null" {
		t.Errorf("a parse error must carry id:null, got %s", pe.ID)
	}
	if pe.Error == nil || pe.Error.Code != -32700 {
		t.Errorf("expected code -32700, got %+v", pe.Error)
	}
	// The following well-formed request must still be answered normally.
	if !strings.Contains(lines[1], `"id":1`) {
		t.Errorf("second frame should answer id 1: %s", lines[1])
	}
}

// TestServerStillIgnoresNotifications guards the neighbouring rule: a valid
// frame with no id is a notification and must receive NO response, even now
// that parse errors do get one.
func TestServerStillIgnoresNotifications(t *testing.T) {
	s := NewServer("tag")
	var out strings.Builder
	in := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	if err := s.Serve(strings.NewReader(in), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("notifications must get no response, got: %s", out.String())
	}
}
