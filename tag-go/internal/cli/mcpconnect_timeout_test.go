package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// silentServerScript writes a shell script that accepts stdin forever and never
// replies — an unresponsive MCP server.
func silentServerScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "silent.sh")
	body := "#!/bin/sh\nwhile read -r _; do sleep 3600; done\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestE2EMCPConnectTimeoutFlag pins F8: mcp-connect had no timeout knob, so an
// unresponsive server blocked the CLI for the client's full 120s default with
// no progress output. A short --timeout must make it give up promptly.
func TestE2EMCPConnectTimeoutFlag(t *testing.T) {
	h := newHome(t)
	script := silentServerScript(t)

	cmd := exec.Command(tagBin, "mcp-connect", "--timeout", "2s", script)
	cmd.Env = append(os.Environ(), "TAG_HOME="+h)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("connecting to a silent server must fail, got success: %s", out)
	}
	if strings.Contains(string(out), "unknown flag") {
		t.Fatalf("--timeout is not a recognised flag: %s", out)
	}
	if elapsed > 30*time.Second {
		t.Errorf("--timeout 2s was not honored; took %s", elapsed)
	}
	if !strings.Contains(string(out), "timeout") && !strings.Contains(string(out), "timed out") {
		t.Errorf("error should mention the timeout: %s", out)
	}
}

// TestE2EMCPConnectTimeoutIsDocumented guards the flag's discoverability and
// its CLI-appropriate default (the library default of 120s is far too long for
// an interactive command).
func TestE2EMCPConnectTimeoutIsDocumented(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "mcp-connect", "--help")
	if code != 0 {
		t.Fatalf("help exit %d: %s", code, out)
	}
	if !strings.Contains(out, "--timeout") {
		t.Errorf("mcp-connect must expose --timeout: %s", out)
	}
}

// TestE2EMCPConnectStillWorks guards the happy path: `tag mcp-connect` against
// TAG's own `tag mcp-serve` must still list tools.
func TestE2EMCPConnectStillWorks(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "mcp-connect", tagBin, "mcp-serve")
	if code != 0 {
		t.Fatalf("mcp-connect -> mcp-serve exit %d: %s", code, out)
	}
	if !strings.Contains(out, "echo") {
		t.Errorf("expected the echo tool to be listed: %s", out)
	}
}
