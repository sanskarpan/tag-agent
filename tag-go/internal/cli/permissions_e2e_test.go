package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// startBashServer stands up a fake OpenAI-compatible SSE endpoint whose first
// turn requests a `bash` tool call and whose second turn answers in text. This
// drives the REAL agent loop through a genuine tool-calling turn with no network
// and no API key, so the consent gate is exercised end-to-end in the built binary.
func startBashServer(t *testing.T, command string) *httptest.Server {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if n == 1 {
			args := fmt.Sprintf(`{"command":%q}`, command)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":""}}]}}]}`)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":%s}}]}}]}\n\n", jsonString(args))
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
			fmt.Fprintf(w, "data: [DONE]\n\n")
		} else {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"finished"}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			fmt.Fprintf(w, "data: [DONE]\n\n")
		}
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runBounded runs the binary with a hard wall-clock bound. A hang FAILS the test
// instead of wedging the suite. (`perl -e 'alarm'` does not bound a Go binary —
// Go ignores an unhandled SIGALRM — so the process is killed explicitly.)
func runBounded(t *testing.T, home string, env []string, d time.Duration, args ...string) (string, int, bool) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(append(os.Environ(), "TAG_HOME="+home), env...)
	// Give it a closed stdin: this is what a daemon/CI process looks like.
	cmd.Stdin = strings.NewReader("")
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return out.String(), code, false
	case <-time.After(d):
		_ = cmd.Process.Kill()
		<-done
		return out.String(), -1, true
	}
}

// ---------------------------------------------------------------------------

// TestE2EBashDeniedByDefault: with --tools and a provider that asks for bash,
// the command must NOT run and the model must be told it was denied.
func TestE2EBashDeniedByDefault(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "pwned.txt")
	srv := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	out, code, timedOut := runBounded(t, h, env, 30*time.Second,
		"run", "clean up", "--provider", "local", "--tools")
	if timedOut {
		t.Fatalf("HANG: `tag run --tools` blocked with no TTY. output: %s", out)
	}
	if code != 0 {
		t.Fatalf("exit %d (a denied tool must not crash the run): %s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("SIDE EFFECT RAN: bash executed without approval. output: %s", out)
	}
	if !strings.Contains(out, "permission denied") {
		t.Errorf("expected an honest permission denial in the transcript: %s", out)
	}
	if !strings.Contains(out, "finished") {
		t.Errorf("the loop should continue to a final answer after a deny: %s", out)
	}
}

// TestE2EBashAllowedWithExplicitFlag: the opt-in actually works.
func TestE2EBashAllowedWithExplicitFlag(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "allowed.txt")
	srv := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	out, code, timedOut := runBounded(t, h, env, 30*time.Second,
		"run", "do it", "--provider", "local", "--tools", "--allow-tool", "bash")
	if timedOut {
		t.Fatalf("HANG. output: %s", out)
	}
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("--allow-tool bash did not permit execution: %v\n%s", err, out)
	}
}

// TestE2EAutoApproveOptIn: --auto-approve is the automation door and it is
// recorded as such in the audit log.
func TestE2EAutoApproveOptIn(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "auto.txt")
	srv := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	out, code, timedOut := runBounded(t, h, env, 30*time.Second,
		"run", "do it", "--provider", "local", "--tools", "--auto-approve")
	if timedOut {
		t.Fatalf("HANG. output: %s", out)
	}
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("--auto-approve did not permit execution: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--auto-approve is set") {
		t.Errorf("--auto-approve should announce itself loudly: %s", out)
	}
	// audit trail
	logOut, logCode := run(t, h, "permissions", "log", "--json")
	if logCode != 0 {
		t.Fatalf("permissions log exit %d: %s", logCode, logOut)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(logOut), &entries); err != nil {
		t.Fatalf("permissions log --json is not JSON: %v\n%s", err, logOut)
	}
	found := false
	for _, e := range entries {
		if e["tool"] == "bash" && e["verdict"] == "allow" && e["via"] == "auto-approve" {
			found = true
			if s, _ := e["subject"].(string); !strings.Contains(s, "touch") {
				t.Errorf("audit row should record the concrete command: %v", e)
			}
		}
	}
	if !found {
		t.Errorf("no auto-approve row in the audit log: %s", logOut)
	}
}

// TestE2EDenyIsAudited: a blocked call shows up in `permissions log`.
func TestE2EDenyIsAudited(t *testing.T) {
	h := newHome(t)
	srv := startBashServer(t, "id")
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}
	if out, code, to := runBounded(t, h, env, 30*time.Second, "run", "x", "--provider", "local", "--tools"); to || code != 0 {
		t.Fatalf("run failed (timeout=%v code=%d): %s", to, code, out)
	}
	out, code := run(t, h, "permissions", "log")
	if code != 0 {
		t.Fatalf("permissions log exit %d: %s", code, out)
	}
	if !strings.Contains(out, "deny") || !strings.Contains(out, "bash") {
		t.Errorf("the deny should be in the audit log: %s", out)
	}
}

// TestE2EDenyToolBeatsConfigAllow proves flag > config precedence through the
// real binary and a real config.yaml.
func TestE2EPermissionsPrecedenceThroughConfig(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "cfg.txt")

	cfgPath := configPathFor(t, h)
	appendYAML(t, cfgPath, "permissions:\n  tools:\n    bash: allow\n")

	// config alone allows it (a fresh server per run: each one scripts turn 1 =
	// tool call, turn 2 = text)
	srv1 := startBashServer(t, "touch "+marker)
	env1 := []string{"TAG_LOCAL_BASE_URL=" + srv1.URL + "/v1", "TAG_LOCAL_API_KEY=x"}
	if out, code, to := runBounded(t, h, env1, 30*time.Second, "run", "x", "--provider", "local", "--tools"); to || code != 0 {
		t.Fatalf("run failed (timeout=%v code=%d): %s", to, code, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("config `permissions.tools.bash: allow` did not take effect: %v", err)
	}
	os.Remove(marker)

	// ...and a flag overrides it
	srv2 := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv2.URL + "/v1", "TAG_LOCAL_API_KEY=x"}
	out, code, to := runBounded(t, h, env, 30*time.Second, "run", "x", "--provider", "local", "--tools", "--deny-tool", "bash")
	if to || code != 0 {
		t.Fatalf("run failed (timeout=%v code=%d): %s", to, code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("--deny-tool must override the config allow")
	}
	if !strings.Contains(out, "permission denied") {
		t.Errorf("expected a denial in the transcript: %s", out)
	}
}

// TestE2EPermissionsShow renders the resolved ruleset and the honest
// non-interactive statement.
func TestE2EPermissionsShow(t *testing.T) {
	h := newHome(t)
	out, code, to := runBounded(t, h, nil, 20*time.Second, "permissions", "show")
	if to || code != 0 {
		t.Fatalf("permissions show (timeout=%v code=%d): %s", to, code, out)
	}
	for _, want := range []string{
		"resolved ruleset", "bash:* = ask [builtin]", "read_file:* = allow [builtin]",
		"resolves to DENY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("permissions show missing %q:\n%s", want, out)
		}
	}
	// flag overrides are reflected
	out, code, to = runBounded(t, h, nil, 20*time.Second, "permissions", "show", "--allow-tool", "bash:git *")
	if to || code != 0 {
		t.Fatalf("permissions show with flags (timeout=%v code=%d): %s", to, code, out)
	}
	if !strings.Contains(out, "bash command:git * = allow [flag]") {
		t.Errorf("flag rule not shown:\n%s", out)
	}
}

// TestE2EQueueWorkerDoesNotHangOnAsk is the headless-daemon guarantee: a queue
// drain that hits an `ask` must finish, not block forever waiting on a human.
func TestE2EQueueWorkerDoesNotHangOnAsk(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "queue.txt")
	srv := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	if out, code := runEnv(t, h, env, "queue", "add", "run the command", "--profile", "coder"); code != 0 {
		t.Fatalf("queue add: %s", out)
	}
	out, code, to := runBounded(t, h, env, 45*time.Second, "queue", "worker", "--provider", "local", "--tools")
	if to {
		t.Fatalf("HANG: `queue worker --tools` blocked on a permission prompt. output: %s", out)
	}
	if code != 0 {
		t.Fatalf("queue worker exit %d: %s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("SIDE EFFECT RAN: the worker executed bash without approval")
	}
}

// TestE2EDangerouslyAllowAllIsLoud
func TestE2EDangerouslyAllowAllIsLoud(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "danger.txt")
	srv := startBashServer(t, "touch "+marker)
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}
	out, code, to := runBounded(t, h, env, 30*time.Second,
		"run", "x", "--provider", "local", "--tools", "--dangerously-allow-all")
	if to || code != 0 {
		t.Fatalf("run failed (timeout=%v code=%d): %s", to, code, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("--dangerously-allow-all should execute: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "DISABLED") {
		t.Errorf("the escape hatch must warn loudly: %s", out)
	}
}

// TestE2EBadPermissionSpecIsUsageError
func TestE2EBadPermissionSpecIsUsageError(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "run", "x", "--tools", "--allow-tool", ":oops")
	if code != 2 {
		t.Errorf("a malformed --allow-tool should be a usage error (exit 2), got %d: %s", code, out)
	}
}

// TestE2EEchoProviderUnchanged: the default offline flows must behave exactly as
// before (the echo provider emits no tool calls, so nothing is gated).
func TestE2EEchoProviderUnchanged(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "run", "hello there", "--tools")
	if code != 0 {
		t.Fatalf("`tag run --tools` with the echo provider must still work: %d %s", code, out)
	}
	if strings.Contains(out, "permission") {
		t.Errorf("no permission noise expected on the echo path: %s", out)
	}
}

// --- helpers ---------------------------------------------------------------

func configPathFor(t *testing.T, home string) string {
	t.Helper()
	out, code := run(t, home, "doctor", "--json")
	if code != 0 {
		t.Fatalf("doctor: %s", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err == nil {
		if p, ok := m["config_path"].(string); ok && p != "" {
			return p
		}
	}
	// conventional location: TAG_HOME/config/tag.yaml
	return filepath.Join(home, "config", "tag.yaml")
}

func appendYAML(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString("\n" + text); err != nil {
		t.Fatal(err)
	}
}
