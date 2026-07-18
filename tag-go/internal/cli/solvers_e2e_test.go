package cli_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// runEnv is like run but with extra environment (KEY=VALUE) entries, so tests can
// prepend a fake gh to PATH or point --provider local at a stub server.
func runEnv(t *testing.T, home string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(append(os.Environ(), "TAG_HOME="+home), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// gitInit makes a temp dir a git repo with one commit (some solver paths expect a
// real working tree).
func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t.com"},
		{"config", "user.name", "t"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// fakeGH writes an executable named `gh` into a fresh dir and returns that dir,
// suitable for prepending to PATH. The script body is shell; $@ are the gh args.
func fakeGH(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gh")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// pathWith returns a PATH env entry with extra dirs prepended.
func pathWith(dirs ...string) string {
	return "PATH=" + strings.Join(dirs, string(os.PathListSeparator)) + string(os.PathListSeparator) + os.Getenv("PATH")
}

// --- swe-solve: a real file edit via the local (OpenAI-compatible) provider ----

// startWriteFileServer stands up a fake OpenAI-compatible SSE endpoint. The first
// chat/completions call (before any tool result) streams a write_file tool call;
// the second call streams final text. This drives the real agent loop through a
// genuine tool-calling turn with no network/live model.
func startWriteFileServer(t *testing.T, relPath, content string) *httptest.Server {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if n == 1 {
			// stream a single write_file tool call, split across deltas like real APIs
			args := fmt.Sprintf(`{"path":%q,"content":%q}`, relPath, content)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"write_file","arguments":""}}]}}]}`)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":%s}}]}}]}\n\n", jsonString(args))
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
			fmt.Fprintf(w, "data: [DONE]\n\n")
		} else {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"applied the edit to `+relPath+`"}}]}`)
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

// startTextServer stands up a fake OpenAI-compatible SSE endpoint that streams a
// fixed final text (no tool calls), so tests can drive the loop through a real
// provider that produces genuine output rather than the echoed context.
func startTextServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", jsonString(text))
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jsonString double-encodes s as a JSON string literal (so it can be embedded as
// the value of an "arguments" field which is itself a JSON string).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestE2ESweSolveEditsFile(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	gitInit(t, repo)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644)

	srv := startWriteFileServer(t, "fix.txt", "patched by agent")
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	out, code := runEnv(t, h, env, "swe-solve", "add fix.txt", "--repo", repo, "--provider", "local", "--tools")
	if code != 0 {
		t.Fatalf("swe-solve exit %d: %q", code, out)
	}
	b, err := os.ReadFile(filepath.Join(repo, "fix.txt"))
	if err != nil {
		t.Fatalf("agent did not create fix.txt: %v (output: %q)", err, out)
	}
	if string(b) != "patched by agent" {
		t.Errorf("fix.txt = %q, want %q", b, "patched by agent")
	}
}

func TestE2ESweSolveToolsRequireRepo(t *testing.T) {
	h := newHome(t)
	// --tools without --repo must be rejected (confinement invariant).
	if _, code := run(t, h, "swe-solve", "x", "--tools"); code == 0 {
		t.Error("--tools without --repo should fail")
	}
	// --allow-bash without --tools must be rejected.
	if _, code := run(t, h, "swe-solve", "x", "--repo", t.TempDir(), "--allow-bash"); code == 0 {
		t.Error("--allow-bash without --tools should fail")
	}
}

func TestE2ESweSolveRunTests(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	// echo provider (offline) + a passing test command.
	out, code := run(t, h, "swe-solve", "trivial", "--repo", repo, "--run-tests", "exit 0")
	if code != 0 || !strings.Contains(out, "tests: PASSED") {
		t.Errorf("run-tests pass: %q code=%d", out, code)
	}
	// a failing test command reports FAILED (command still exits 0 — the report is data).
	out, _ = run(t, h, "swe-solve", "trivial", "--repo", repo, "--run-tests", "exit 3")
	if !strings.Contains(out, "tests: FAILED") {
		t.Errorf("run-tests fail: %q", out)
	}
}

// --- issue-solve: consume a fake-gh issue body -------------------------------

func TestE2EIssueSolveFakeGH(t *testing.T) {
	h := newHome(t)
	// fake gh returns canned JSON for `gh issue view`.
	ghDir := fakeGH(t, `
case "$1 $2" in
  "issue view")
    echo '{"title":"Export button broken","body":"Clicking export does nothing on Safari."}'
    ;;
  *) echo "unexpected gh call: $@" >&2; exit 1 ;;
esac
`)
	out, code := runEnv(t, h, []string{pathWith(ghDir)}, "issue-solve", "acme/repo#42", "--repo", "acme/repo")
	if code != 0 {
		t.Fatalf("issue-solve exit %d: %q", code, out)
	}
	// The fetched title+body must reach the echoed context.
	if !strings.Contains(out, "Export button broken") || !strings.Contains(out, "Safari") {
		t.Errorf("issue body from gh not consumed: %q", out)
	}
	if !strings.Contains(out, "fetched issue acme/repo#42 via gh") {
		t.Errorf("expected fetch note: %q", out)
	}
}

func TestE2EIssueSolveGHMissingFallsBack(t *testing.T) {
	h := newHome(t)
	// PATH with an empty dir (no gh) — the ref must fall back to the honest note,
	// never a fake fetch.
	empty := t.TempDir()
	out, code := runEnv(t, h, []string{"PATH=" + empty}, "issue-solve", "#7")
	if code != 0 {
		t.Fatalf("issue-solve exit %d: %q", code, out)
	}
	if !strings.Contains(out, "gh CLI not found") {
		t.Errorf("expected honest gh-missing note: %q", out)
	}
	// The solver still ran on the raw reference (its own honest note is present).
	if !strings.Contains(out, "issue reference") {
		t.Errorf("expected issue-reference note on fallback: %q", out)
	}
}

func TestE2EIssueSolveInlineBodyUnchanged(t *testing.T) {
	h := newHome(t)
	body := "Users cannot log in after the deploy; 500 on /session."
	out, code := run(t, h, "issue-solve", body)
	if code != 0 || !strings.Contains(out, "cannot log in") {
		t.Errorf("inline body: %q code=%d", out, code)
	}
	if strings.Contains(out, "via gh") {
		t.Errorf("inline body must not trigger a gh fetch: %q", out)
	}
}

// --- review-pr: read a fake-gh diff, and --post guard ------------------------

func TestE2EReviewPRFakeGHDiff(t *testing.T) {
	h := newHome(t)
	ghDir := fakeGH(t, `
case "$1 $2" in
  "pr diff")
    printf 'diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@\n+func Add(a,b int) int { return a-b }\n'
    ;;
  *) echo "unexpected gh call: $@" >&2; exit 1 ;;
esac
`)
	out, code := runEnv(t, h, []string{pathWith(ghDir)}, "review-pr", "--pr", "13", "--repo", "acme/repo")
	if code != 0 {
		t.Fatalf("review-pr exit %d: %q", code, out)
	}
	if !strings.Contains(out, "func Add") {
		t.Errorf("fetched diff not reviewed: %q", out)
	}
	if !strings.Contains(out, "fetched diff for PR #13 via gh") {
		t.Errorf("expected fetch note: %q", out)
	}
	// No --post => dry-run note, and gh comment must NOT have been called.
	if !strings.Contains(out, "review NOT posted") {
		t.Errorf("expected dry-run note: %q", out)
	}
}

func TestE2EReviewPRPostGuard(t *testing.T) {
	h := newHome(t)
	// --post without --pr must be rejected (never post to an unknown target).
	if _, code := run(t, h, "review-pr", "--post", "--diff", "/dev/null"); code == 0 {
		t.Error("--post without --pr should fail")
	}
}

func TestE2EReviewPRPostRefusesEcho(t *testing.T) {
	h := newHome(t)
	// --post with the offline echo provider must be refused before any fetch/post:
	// echo echoes the diff rather than reviewing it, so publishing it would be
	// misleading. No gh on PATH is needed since the guard precedes the fetch.
	out, code := run(t, h, "review-pr", "--post", "--pr", "9")
	if code == 0 {
		t.Errorf("--post with provider=echo should fail: %q", out)
	}
	if !strings.Contains(out, "echo") {
		t.Errorf("expected an echo-refusal message: %q", out)
	}
}

func TestE2EReviewPRPostCallsGH(t *testing.T) {
	h := newHome(t)
	marker := filepath.Join(t.TempDir(), "posted.txt")
	ghDir := fakeGH(t, fmt.Sprintf(`
case "$1 $2" in
  "pr diff")
    printf 'diff --git a/y.go b/y.go\n+var x = 1\n'
    ;;
  "pr comment")
    echo "commented" > %q
    echo "https://github.com/acme/repo/pull/5#issuecomment-1"
    ;;
  *) echo "unexpected gh call: $@" >&2; exit 1 ;;
esac
`, marker))
	// --post is refused for provider=echo, so exercise the gh-comment path with a
	// real (stub local) provider that emits a genuine review.
	srv := startTextServer(t, "LGTM: no blocking issues found.")
	env := []string{pathWith(ghDir), "TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}
	out, code := runEnv(t, h, env, "review-pr", "--pr", "5", "--repo", "acme/repo", "--post", "--provider", "local")
	if code != 0 {
		t.Fatalf("review-pr --post exit %d: %q", code, out)
	}
	if !strings.Contains(out, "posted the review as a comment on PR #5 via gh") {
		t.Errorf("expected post-success note: %q", out)
	}
	if b, err := os.ReadFile(marker); err != nil || !strings.Contains(string(b), "commented") {
		t.Errorf("gh pr comment was not invoked: %v %q", err, string(b))
	}
}

// --- agentic-ci: iterate a failing→passing check -----------------------------

func TestE2EAgenticCIConverges(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	// A check that fails the first time (creating a marker) and passes once the
	// marker exists — proving the loop re-checks across iterations.
	os.WriteFile(filepath.Join(repo, "state"), []byte("0"), 0o644)
	check := `if [ "$(cat state)" = "1" ]; then exit 0; fi; echo 1 > state; echo "not ready"; exit 1`
	out, code := run(t, h, "agentic-ci", "make it green", "--repo", repo, "--check", check, "--max-iters", "3")
	if code != 0 {
		t.Fatalf("agentic-ci exit %d: %q", code, out)
	}
	if !strings.Contains(out, "converged") || strings.Contains(out, "NOT converged") {
		t.Errorf("expected convergence: %q", out)
	}
	if !strings.Contains(out, "iter 1: check FAIL") || !strings.Contains(out, "iter 2: check PASS") {
		t.Errorf("expected fail-then-pass iterations: %q", out)
	}
}

func TestE2EAgenticCINeverConverges(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	out, code := run(t, h, "agentic-ci", "impossible", "--repo", repo, "--check", "exit 1", "--max-iters", "2")
	if code != 0 {
		t.Fatalf("agentic-ci exit %d: %q", code, out)
	}
	if !strings.Contains(out, "NOT converged") {
		t.Errorf("expected non-convergence: %q", out)
	}
	if !strings.Contains(out, "did not converge") {
		t.Errorf("expected honest non-convergence note: %q", out)
	}
}
