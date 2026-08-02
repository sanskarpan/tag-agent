package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// mock coordinator/agent server (OpenAI-compatible SSE, no network, no keys)
// ---------------------------------------------------------------------------

// swarmManifest is the JSON a real coordinator model would return.
const swarmManifest = `{"swarm_id":"x","goal":"audit the repo","tasks":[` +
	`{"task_id":"docs","description":"review docs","profile":"p","context_slice":{"type":"file_paths","selector":["README.md"]},"context_bus_writes":["docs_note"]},` +
	`{"task_id":"code","description":"review code","profile":"p","context_slice":{"type":"file_paths","selector":["main.go"]}}` +
	`]}`

// startSwarmServer answers the coordinator turn with a manifest and every
// sub-agent turn with a per-task line. If hangTasks is true the sub-agent turns
// block until the request context is cancelled, which is how the SIGTERM test
// gets a task parked in 'running'.
func startSwarmServer(t *testing.T, manifest string, hangTasks bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		isCoordinator := bytes.Contains(body, []byte("task coordinator"))
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		emit := func(text string) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", jsonString(text))
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			fmt.Fprintf(w, "data: %s\n\n", "[DONE]")
			if fl != nil {
				fl.Flush()
			}
		}
		if isCoordinator {
			emit(manifest)
			return
		}
		if hangTasks {
			<-r.Context().Done()
			return
		}
		emit("agent output")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func localEnv(url string) []string {
	return []string{"TAG_LOCAL_BASE_URL=" + url + "/v1", "TAG_LOCAL_API_KEY=x"}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestE2ESwarmRunPopulatesListStatusResults is the headline regression.
//
// Before this change `tag swarm run` did not exist at all (exit 2, "unknown
// command"), and list/status/results read swarm_runs/swarm_tasks that nothing in
// Go ever wrote — so they always reported nothing against a Go-only TAG_HOME.
func TestE2ESwarmRunPopulatesListStatusResults(t *testing.T) {
	h := newHome(t)
	srv := startSwarmServer(t, swarmManifest, false)
	env := localEnv(srv.URL)

	// Empty state first: --json must be [] and not null.
	out, code := run(t, h, "--json", "swarm", "list")
	if code != 0 {
		t.Fatalf("swarm list exit %d: %q", code, out)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty swarm list --json = %q, want []", strings.TrimSpace(out))
	}

	out, code = runEnv(t, h, env, "swarm", "run", "--goal", "audit the repo", "--provider", "local")
	if code != 0 {
		t.Fatalf("swarm run exit %d: %q", code, out)
	}
	if strings.Contains(out, "DEGRADED") {
		t.Fatalf("a real manifest must not degrade: %q", out)
	}

	// list
	out, code = run(t, h, "--json", "swarm", "list")
	if code != 0 {
		t.Fatalf("swarm list exit %d: %q", code, out)
	}
	var runs []map[string]any
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		t.Fatalf("swarm list --json is not JSON: %v (%q)", err, out)
	}
	if len(runs) != 1 {
		t.Fatalf("swarm list = %d runs, want 1 — swarm run did not persist swarm_runs", len(runs))
	}
	if runs[0]["status"] != "completed" {
		t.Errorf("run status = %v, want completed", runs[0]["status"])
	}
	if n, _ := runs[0]["task_count"].(float64); n != 2 {
		t.Errorf("task_count = %v, want 2", runs[0]["task_count"])
	}
	sid, _ := runs[0]["swarm_id"].(string)
	if sid == "" {
		t.Fatal("no swarm_id in list output")
	}

	// status
	out, code = run(t, h, "--json", "swarm", "status", sid)
	if code != 0 {
		t.Fatalf("swarm status exit %d: %q", code, out)
	}
	var st struct {
		Run   map[string]any   `json:"run"`
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("swarm status --json: %v (%q)", err, out)
	}
	if len(st.Tasks) != 2 {
		t.Fatalf("status shows %d tasks, want 2 — swarm run did not persist swarm_tasks", len(st.Tasks))
	}
	for _, tk := range st.Tasks {
		if tk["status"] != "done" {
			t.Errorf("task %v status = %v, want done", tk["task_id"], tk["status"])
		}
	}

	// results (both --json and Python's --format json)
	for _, args := range [][]string{
		{"--json", "swarm", "results", sid},
		{"swarm", "results", sid, "--format", "json"},
	} {
		out, code = run(t, h, args...)
		if code != 0 {
			t.Fatalf("%v exit %d: %q", args, code, out)
		}
		var res map[string]any
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("%v is not JSON: %v (%q)", args, err, out)
		}
		if res["final_output"] == "" || res["final_output"] == nil {
			t.Errorf("%v produced no final_output", args)
		}
		tasks, _ := res["tasks"].([]any)
		if len(tasks) != 2 {
			t.Errorf("%v shows %d tasks, want 2", args, len(tasks))
		}
	}

	// the context bus a declared write produced is auditable
	out, code = run(t, h, "swarm", "results", sid, "--format", "json", "--include-context")
	if code != 0 {
		t.Fatalf("results --include-context exit %d: %q", code, out)
	}
	if !strings.Contains(out, "docs_note") {
		t.Errorf("context_bus missing the declared write 'docs_note': %q", out)
	}
}

// The offline echo path must work AND say plainly that it is degraded.
func TestE2ESwarmEchoIsOfflineAndHonest(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "swarm", "run", "--goal", "do a thing", "--provider", "echo")
	if code != 0 {
		t.Fatalf("echo swarm run exit %d: %q", code, out)
	}
	if !strings.Contains(out, "DEGRADED") {
		t.Fatalf("echo run must announce degradation, got %q", out)
	}
	out, code = run(t, h, "--json", "swarm", "run", "--goal", "do a thing", "--provider", "echo")
	if code != 0 {
		t.Fatalf("echo swarm run --json exit %d: %q", code, out)
	}
	// The warning goes to stderr; the JSON object is still parseable from the
	// combined stream's last brace-delimited block.
	i := strings.Index(out, "{")
	var res map[string]any
	if i < 0 || json.Unmarshal([]byte(out[i:]), &res) != nil {
		t.Fatalf("echo run --json did not emit a JSON object: %q", out)
	}
	if res["degraded"] != true {
		t.Errorf("degraded flag missing from --json: %v", res)
	}
	if res["degraded_reason"] == "" || res["degraded_reason"] == nil {
		t.Errorf("degraded_reason missing from --json: %v", res)
	}
}

// --dry-run states what it would do and writes NOTHING.
func TestE2ESwarmDryRunWritesNothing(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "swarm", "run", "--goal", "g", "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit %d: %q", code, out)
	}
	for _, want := range []string{"Would:", "swarm_runs", "dry run"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q: %q", want, out)
		}
	}
	if lst, _ := run(t, h, "--json", "swarm", "list"); strings.TrimSpace(lst) != "[]" {
		t.Fatalf("--dry-run persisted a run: %q", lst)
	}
	// JSON form too.
	out, code = run(t, h, "--json", "swarm", "run", "--goal", "g", "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run --json exit %d: %q", code, out)
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("dry-run --json: %v (%q)", err, out)
	}
	if plan["dry_run"] != true || plan["would"] == nil {
		t.Errorf("dry-run --json missing dry_run/would: %v", plan)
	}
	if lst, _ := run(t, h, "--json", "swarm", "list"); strings.TrimSpace(lst) != "[]" {
		t.Fatalf("--dry-run --json persisted a run: %q", lst)
	}
}

// Usage errors exit 2; runtime "not found" exits 1 with a JSON error object.
func TestE2ESwarmExitCodesAndJSONErrors(t *testing.T) {
	h := newHome(t)
	usage := [][]string{
		{"swarm", "run"}, // missing --goal
		{"swarm", "run", "--goal", "g", "--max-agents", "0"},        // below range
		{"swarm", "run", "--goal", "g", "--max-agents", "11"},       // above cap
		{"swarm", "run", "--goal", "g", "--failure-policy", "yolo"}, // bad policy
		{"swarm", "run", "--goal", "g", "--provider", "nope"},       // unknown provider
		{"swarm", "run", "--goal", "g", "--timeout-per-agent", "0"}, // bad timeout
		{"swarm", "bogus"}, // unknown subcommand
		{"swarm", "results", "x", "--format", "yaml"}, // bad format
	}
	for _, args := range usage {
		out, code := run(t, h, args...)
		if code != 2 {
			t.Errorf("%v exit %d, want 2 (usage): %q", args, code, out)
		}
	}
	for _, args := range [][]string{
		{"swarm", "status", "nope"},
		{"swarm", "results", "nope"},
		{"swarm", "abort", "nope"},
	} {
		out, code := run(t, h, args...)
		if code != 1 {
			t.Errorf("%v exit %d, want 1 (runtime): %q", args, code, out)
		}
		jargs := append([]string{"--json"}, args...)
		out, code = run(t, h, jargs...)
		if code != 1 {
			t.Errorf("%v exit %d, want 1: %q", jargs, code, out)
		}
		var obj map[string]any
		i := strings.Index(out, "{")
		if i < 0 || json.Unmarshal([]byte(out[i:]), &obj) != nil || obj["error"] == nil {
			t.Errorf("%v did not emit a JSON error object: %q", jargs, out)
		}
	}
	// Empty-collection JSON must be [] and never null.
	if out, _ := run(t, h, "--json", "swarm", "list"); strings.TrimSpace(out) != "[]" {
		t.Errorf("empty list --json = %q, want []", out)
	}
	if out, _ := run(t, h, "--json", "swarm", "list", "--status", "running"); strings.TrimSpace(out) != "[]" {
		t.Errorf("filtered empty list --json = %q, want []", out)
	}
}

// `swarm abort` refuses a run that already finished: rewriting a completed run
// to 'aborted' destroyed the record of what actually happened, and reported
// that as a successful abort. Aborting a LIVE run is covered by
// TestE2ESwarmAbortFromAnotherProcess.
func TestE2ESwarmAbortRefusesFinishedRun(t *testing.T) {
	h := newHome(t)
	srv := startSwarmServer(t, swarmManifest, false)
	if out, code := runEnv(t, h, localEnv(srv.URL), "swarm", "run", "--goal", "g", "--provider", "local"); code != 0 {
		t.Fatalf("swarm run: %d %q", code, out)
	}
	sid := firstSwarmID(t, h)

	out, code := run(t, h, "--json", "swarm", "abort", sid)
	if code == 0 {
		t.Fatalf("aborting a finished run must not report success: %q", out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("abort --json: %v (%q)", err, out)
	}
	if e, _ := res["error"].(string); !strings.Contains(e, "not running") {
		t.Errorf("expected a 'not running' explanation, got %v", res)
	}
	if res["swarm_id"] != sid {
		t.Errorf("the error payload must identify the run: %v", res)
	}
	// The real outcome survives the refused abort.
	if lst, _ := run(t, h, "--json", "swarm", "list", "--status", "aborted"); strings.Contains(lst, sid) {
		t.Errorf("a completed run must not be relabelled aborted: %q", lst)
	}
	if lst, _ := run(t, h, "--json", "swarm", "list", "--status", "completed"); !strings.Contains(lst, sid) {
		t.Errorf("the completed run should still be listed as completed: %q", lst)
	}
}

// Aborting a LIVE run really stops it, from another process. This is the
// capability TestE2ESwarmAbortRefusesFinishedRun deliberately does not cover:
// the run row IS the abort channel, so `swarm abort` has to be a real signal
// and not just a status rewrite.
func TestE2ESwarmAbortFromAnotherProcess(t *testing.T) {
	h := newHome(t)
	srv := startSwarmServer(t, swarmManifest, true) // sub-agent turns hang
	cmd := exec.Command(tagBin, "swarm", "run", "--goal", "g", "--provider", "local",
		"--timeout-per-agent", "300")
	cmd.Env = append(append(os.Environ(), "TAG_HOME="+h), localEnv(srv.URL)...)
	var buf syncBuf
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	sid := ""
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if sid == "" {
			sid = firstSwarmIDSoft(t, h)
		}
		if sid != "" && swarmHasTaskStatus(t, h, sid, "running") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if sid == "" {
		t.Fatalf("swarm run never persisted a run: %q", buf.String())
	}

	out, code := run(t, h, "--json", "swarm", "abort", sid)
	if code != 0 {
		t.Fatalf("aborting a running swarm should succeed: exit %d %q", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("abort --json: %v (%q)", err, out)
	}
	if res["status"] != "aborted" || res["swarm_id"] != sid {
		t.Errorf("abort payload = %v", res)
	}
	if _, ok := res["signalled"]; !ok {
		t.Error("abort --json must keep Python's 'signalled' field")
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() != 4 {
			t.Errorf("an aborted run exits 4, got %d: %q", ee.ExitCode(), buf.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("swarm run ignored a cross-process abort: %q", buf.String())
	}

	if swarmHasTaskStatus(t, h, sid, "running") {
		st, _ := run(t, h, "--json", "swarm", "status", sid)
		t.Errorf("abort left a task in 'running': %s", st)
	}
	if lst, _ := run(t, h, "--json", "swarm", "list", "--status", "aborted"); !strings.Contains(lst, sid) {
		t.Errorf("aborted run not visible via list --status aborted: %q", lst)
	}
}

// SIGTERM must not strand a task in 'running' (#574 applied to swarm).
func TestE2ESwarmSigtermLeavesNoRunningTask(t *testing.T) {
	h := newHome(t)
	srv := startSwarmServer(t, swarmManifest, true) // sub-agent turns hang
	cmd := exec.Command(tagBin, "swarm", "run", "--goal", "g", "--provider", "local",
		"--timeout-per-agent", "300")
	cmd.Env = append(append(os.Environ(), "TAG_HOME="+h), localEnv(srv.URL)...)
	var buf syncBuf
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for a task to actually be claimed as 'running'.
	sid := ""
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if sid == "" {
			sid = firstSwarmIDSoft(t, h)
		}
		if sid != "" && swarmHasTaskStatus(t, h, sid, "running") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if sid == "" {
		_ = cmd.Process.Kill()
		t.Fatalf("swarm run never persisted a run: %q", buf.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("swarm run ignored SIGTERM (silent hang): %q", buf.String())
	}

	if swarmHasTaskStatus(t, h, sid, "running") {
		out, _ := run(t, h, "--json", "swarm", "status", sid)
		t.Fatalf("SIGTERM stranded a task in 'running': %s", out)
	}
	out, _ := run(t, h, "--json", "swarm", "status", sid)
	var st struct {
		Run map[string]any `json:"run"`
	}
	_ = json.Unmarshal([]byte(out), &st)
	if s := fmt.Sprint(st.Run["status"]); s == "running" || s == "pending" {
		t.Fatalf("swarm_runs left in %q after SIGTERM: %s", s, out)
	}
}

// --sequential and --max-agents are accepted and produce a complete run.
func TestE2ESwarmSequentialAndMaxAgents(t *testing.T) {
	h := newHome(t)
	srv := startSwarmServer(t, swarmManifest, false)
	env := localEnv(srv.URL)
	// --sequential still runs the full 2-task manifest.
	out, code := runEnv(t, h, env, "swarm", "run", "--goal", "g", "--provider", "local", "--sequential")
	if code != 0 {
		t.Fatalf("--sequential exit %d: %q", code, out)
	}
	if strings.Contains(out, "DEGRADED") {
		t.Errorf("--sequential should not degrade: %q", out)
	}
	// --max-agents 1 REJECTS the 2-task manifest (Python validates the manifest
	// against max_agents) — it must degrade loudly, never silently truncate the
	// plan to whichever task happened to sort first.
	out, code = runEnv(t, h, env, "swarm", "run", "--goal", "g", "--provider", "local", "--max-agents", "1")
	if code != 0 {
		t.Fatalf("--max-agents 1 exit %d: %q", code, out)
	}
	if !strings.Contains(out, "DEGRADED") || !strings.Contains(out, "max_agents=1") {
		t.Errorf("--max-agents 1 must reject the oversized manifest loudly: %q", out)
	}
	out, _ = run(t, h, "--json", "swarm", "list")
	var runs []map[string]any
	if err := json.Unmarshal([]byte(out), &runs); err != nil || len(runs) != 2 {
		t.Fatalf("expected 2 persisted runs, got %q (%v)", out, err)
	}
}

// --approve without a TTY must fail fast, never block on an unanswerable read.
func TestE2ESwarmApproveNeedsTTY(t *testing.T) {
	h := newHome(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, code := run(t, h, "swarm", "run", "--goal", "g", "--approve")
		if code == 0 {
			t.Errorf("--approve without a TTY should fail, got 0: %q", out)
		}
		if !strings.Contains(out, "interactive terminal") {
			t.Errorf("--approve error should explain the TTY requirement: %q", out)
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("--approve without a TTY hung waiting for input")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func firstSwarmID(t *testing.T, home string) string {
	t.Helper()
	id := firstSwarmIDSoft(t, home)
	if id == "" {
		t.Fatal("no swarm runs persisted")
	}
	return id
}

func firstSwarmIDSoft(t *testing.T, home string) string {
	t.Helper()
	out, code := run(t, home, "--json", "swarm", "list")
	if code != 0 {
		return ""
	}
	var runs []map[string]any
	if err := json.Unmarshal([]byte(out), &runs); err != nil || len(runs) == 0 {
		return ""
	}
	id, _ := runs[0]["swarm_id"].(string)
	return id
}

func swarmHasTaskStatus(t *testing.T, home, sid, status string) bool {
	t.Helper()
	out, code := run(t, home, "--json", "swarm", "status", sid)
	if code != 0 {
		return false
	}
	var st struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return false
	}
	for _, tk := range st.Tasks {
		if tk["status"] == status {
			return true
		}
	}
	return false
}
