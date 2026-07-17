package cli_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// freePort asks the OS for an unused TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestE2EGateway starts `tag gateway` as a subprocess (echo provider, loopback)
// and exercises the OpenAI-compatible surface over real HTTP: /health, a
// non-stream chat completion, and a streaming (SSE) completion. This covers the
// CLI wiring + server that the httptest unit tests don't.
func TestE2EGateway(t *testing.T) {
	h := newHome(t)
	if _, c := run(t, h, "bootstrap"); c != 0 {
		t.Fatalf("bootstrap failed: %d", c)
	}
	port := freePort(t)
	cmd := exec.Command(tagBin, "gateway", "--provider", "echo", "--port", fmt.Sprint(port))
	cmd.Env = append(os.Environ(), "TAG_HOME="+h)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// wait for readiness
	up := false
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(base + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				up = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatal("gateway did not become ready")
	}

	// non-stream completion echoes the prompt back
	body := `{"model":"echo/x","messages":[{"role":"user","content":"e2e hello"}]}`
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat completion status %d", resp.StatusCode)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct{ Content string }
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 || out.Choices[0].Message.Content != "e2e hello" {
		t.Fatalf("unexpected completion: %+v", out)
	}

	// streaming completion emits SSE chunks ending in [DONE]
	sresp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"echo/x","stream":true,"messages":[{"role":"user","content":"streamed"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	if ct := sresp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("want SSE, got %q", ct)
	}
	all, _ := io.ReadAll(sresp.Body)
	s := string(all)
	if !strings.Contains(s, `"content":"streamed"`) || !strings.Contains(s, "data: [DONE]") {
		t.Errorf("streaming body missing content or DONE: %s", s)
	}
}
