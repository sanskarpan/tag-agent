package cli_test

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// stalledListener accepts TCP connections and never writes a byte back — the
// shape of a wedged inference server / proxy. It returns the base URL.
func stalledListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		var held []net.Conn
		for {
			c, err := ln.Accept()
			if err != nil {
				for _, h := range held {
					h.Close()
				}
				close(done)
				return
			}
			held = append(held, c) // hold it open, answer nothing
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
	})
	return "http://" + ln.Addr().String() + "/v1"
}

// TestE2ERunTimesOutOnStalledProvider is the regression guard for the silent
// hang: `tag run` drove the agent loop with context.Background() and the
// adapters used a bare 10-minute http.Client timeout, so a provider that
// accepted the socket and never responded produced ZERO output for minutes.
// TAG must never hang silently — the run has to fail fast with an honest error.
func TestE2ERunTimesOutOnStalledProvider(t *testing.T) {
	h := newHome(t)
	base := stalledListener(t)

	cmd := exec.Command(tagBin, "run", "hello", "--provider", "local", "--timeout", "2")
	cmd.Env = append(os.Environ(), "TAG_HOME="+h, "TAG_LOCAL_BASE_URL="+base)
	type result struct {
		out  string
		code int
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		b, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		ch <- result{string(b), code}
	}()

	select {
	case r := <-ch:
		if r.code == 0 {
			t.Fatalf("stalled provider should fail, got exit 0: %s", r.out)
		}
		if !strings.Contains(r.out, "timed out") {
			t.Errorf("expected an honest timeout message, got: %q", r.out)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("--timeout 2 took %v to fire", elapsed)
		}
	case <-time.After(25 * time.Second):
		_ = cmd.Process.Kill()
		<-ch
		t.Fatal("tag run hung on a stalled provider (no --timeout enforcement)")
	}
}

// TestE2ERunTimeoutFlagExists keeps the flag documented and discoverable; the
// silent-hang defect had no way at all for a user to bound a run.
func TestE2ERunTimeoutFlagExists(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "run", "--help")
	if code != 0 {
		t.Fatalf("run --help exited %d: %s", code, out)
	}
	if !strings.Contains(out, "--timeout") {
		t.Errorf("`tag run --help` does not advertise --timeout:\n%s", out)
	}
}
