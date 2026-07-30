package cli_test

// End-to-end tests for the six `tag agentic-ci` subcommands ported from the
// Python control plane (PRD-057/059/060/061/062/063).
//
// Every test drives the real binary in an isolated TAG_HOME with the offline
// `echo` provider or a local mock SSE server -- there are no live API calls.
// The assertions deliberately cover the four things a green unit suite has
// historically failed to catch in this repo: dispatch wiring, exit codes,
// --json contracts, and fabricated success.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runAt is `run` with a working directory and extra environment, so a test can
// exercise the repo-relative behaviour of fix-vuln / gen-pipeline / flaky-fix.
func runAt(t *testing.T, home, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "TAG_HOME="+home), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const fixtureSARIF = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"bandit","rules":[
  {"id":"B605","defaultConfiguration":{"level":"error"}}]}},
"results":[
  {"ruleId":"B605","message":{"text":"shell injection"},"locations":[{"physicalLocation":{
     "artifactLocation":{"uri":"vuln.py"},"region":{"startLine":3}}}]},
  {"ruleId":"B999","level":"note","message":{"text":"escape attempt"},"locations":[{"physicalLocation":{
     "artifactLocation":{"uri":"file:///../../etc/passwd"},"region":{"startLine":1}}}]}
]}]}`

const fixtureCILog = `Run pytest
  File "app/svc.py", line 42, in handle
    return payload["id"]
KeyError: 'id'
FAILED tests/test_svc.py::test_handle - KeyError: 'id'
1 failed, 3 passed
`

const fixtureFlakyLog = `=== run 1 ===
PASSED tests/test_a.py::test_x
FAILED tests/test_b.py::test_y
=== run 2 ===
FAILED tests/test_a.py::test_x
FAILED tests/test_b.py::test_y
=== run 3 ===
PASSED tests/test_a.py::test_x
FAILED tests/test_b.py::test_y
`

// ---------------------------------------------------------------------------
// dispatch: the legacy single-command form must keep working
// ---------------------------------------------------------------------------

func TestE2EAgenticCILegacyFormStillWorks(t *testing.T) {
	h := newHome(t)
	// A bare task argument that is NOT a subcommand name must still reach the
	// parent's RunE (the check→fix solver), not be rejected as an unknown
	// subcommand. This is the exact regression attaching subcommands can cause.
	out, code := run(t, h, "agentic-ci", "fix the flaky build")
	if code != 0 {
		t.Fatalf("legacy form broke: code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "[agentic-ci]") {
		t.Errorf("legacy form did not run the solver: %s", out)
	}

	// ...including with its --check loop.
	out, code = run(t, h, "agentic-ci", "make it pass", "--check", "true")
	if code != 0 || !strings.Contains(out, "converged") {
		t.Errorf("legacy --check loop broke: code=%d out=%s", code, out)
	}

	// And every subcommand is reachable.
	for _, sub := range []string{"test-gen", "fix-vuln", "ci-diagnose", "review", "gen-pipeline", "flaky-fix"} {
		if out, code := run(t, h, "agentic-ci", sub, "--help"); code != 0 {
			t.Errorf("agentic-ci %s --help: code=%d out=%s", sub, code, out)
		}
	}

	// `tag ci diagnose` is the second spelling; `tag ci <task>` must survive it.
	if out, code := run(t, h, "ci", "build the thing"); code != 0 || !strings.Contains(out, "iteration 1") {
		t.Errorf("legacy `tag ci <task>` broke: code=%d out=%s", code, out)
	}
}

// ---------------------------------------------------------------------------
// PRD-059: fix-vuln
// ---------------------------------------------------------------------------

func TestE2EFixVulnOfflineReportsFindingsAndExits3(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	writeFile(t, repo, "vuln.py", "import os\ndef run(c):\n    os.system(c)\n")
	writeFile(t, repo, "r.sarif", fixtureSARIF)

	out, code := runAt(t, h, repo, nil, "agentic-ci", "fix-vuln", "r.sarif")
	if code != 3 {
		t.Fatalf("findings must exit 3 (ran fine, found problems), got %d: %s", code, out)
	}
	if !strings.Contains(out, "UNFIXED") {
		t.Errorf("findings not reported: %s", out)
	}
	// No fabricated success: echo cannot fix anything and must say so.
	if !strings.Contains(out, "provider=echo cannot produce a fix") {
		t.Errorf("offline degradation not disclosed: %s", out)
	}
	if strings.Contains(out, "FIXED") && !strings.Contains(out, "UNFIXED") {
		t.Errorf("offline run claimed a fix: %s", out)
	}
	// The escaping SARIF path must never be treated as a repo file.
	if !strings.Contains(out, "file not found in repo") {
		t.Errorf("traversal finding not handled: %s", out)
	}
	// Nothing may be written without --apply.
	b, _ := os.ReadFile(filepath.Join(repo, "vuln.py"))
	if !strings.Contains(string(b), "os.system(c)") {
		t.Error("report-only mode modified the source file")
	}
}

func TestE2EFixVulnCleanSARIFExitsZero(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	writeFile(t, repo, "clean.sarif", `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"x"}},"results":[]}]}`)
	out, code := runAt(t, h, repo, nil, "--json", "agentic-ci", "fix-vuln", "clean.sarif")
	if code != 0 {
		t.Fatalf("a clean scan must exit 0, got %d: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	// Empty list must serialise as [] not null.
	if !strings.Contains(out, `"results": []`) {
		t.Errorf("empty results must be [] not null: %s", out)
	}
	if got["unfixed"].(float64) != 0 {
		t.Errorf("unfixed should be 0: %v", got["unfixed"])
	}
}

func TestE2EFixVulnMalformedInputIsUsageErrorNotPanic(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	cases := map[string]string{
		"garbage":     "not json at all",
		"json array":  "[]",
		"no runs key": `{"version":"2.1.0"}`,
		"truncated":   `{"runs":[{"results":[`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, repo, "bad.sarif", body)
			out, code := runAt(t, h, repo, nil, "agentic-ci", "fix-vuln", p)
			if code != 2 {
				t.Fatalf("malformed SARIF must be a usage error (2), got %d: %s", code, out)
			}
			if strings.Contains(out, "panic:") || strings.Contains(out, "goroutine ") {
				t.Fatalf("panicked on malformed input: %s", out)
			}
			if !strings.Contains(out, "malformed SARIF") {
				t.Errorf("error message unhelpful: %s", out)
			}
		})
	}
	// A missing file is also a usage error, not a crash.
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "fix-vuln", "nope.sarif"); code != 2 {
		t.Errorf("missing SARIF must exit 2, got %d: %s", code, out)
	}
	// A misspelled severity must not silently select nothing and report success.
	p := writeFile(t, repo, "ok.sarif", fixtureSARIF)
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "fix-vuln", p, "--severity", "critcal"); code != 2 {
		t.Errorf("bad --severity must exit 2, got %d: %s", code, out)
	}
}

func TestE2EFixVulnJSONErrorObject(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	p := writeFile(t, repo, "bad.sarif", "nope")
	out, code := runAt(t, h, repo, nil, "--json", "agentic-ci", "fix-vuln", p)
	if code == 0 {
		t.Fatalf("--json must NOT swallow the failure into exit 0: %s", out)
	}
	line := firstJSONLineT(t, out)
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("no JSON error object emitted: %v\n%s", err, out)
	}
	if _, ok := obj["error"]; !ok {
		t.Errorf("JSON error path must carry an \"error\" key: %s", line)
	}
}

func TestE2EFixVulnApplyRequiresWritePermission(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	writeFile(t, repo, "vuln.py", "import os\ndef run(c):\n    os.system(c)\n")
	writeFile(t, repo, "r.sarif", fixtureSARIF)
	srv := startTextServer(t, "import subprocess\ndef run(c):\n    subprocess.run(c, shell=False)\n")
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	// Without a grant the guard must refuse the write (headless => ask is deny).
	out, code := runAt(t, h, repo, env, "agentic-ci", "fix-vuln", "r.sarif", "--provider", "local", "--apply")
	if code != 3 {
		t.Fatalf("a refused write leaves the finding unfixed => exit 3, got %d: %s", code, out)
	}
	if !strings.Contains(out, "refusing to write") {
		t.Errorf("write must be gated by the permission model: %s", out)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "vuln.py"))
	if !strings.Contains(string(b), "os.system") {
		t.Fatal("file was written despite the guard denying it")
	}

	// With the grant the fix lands.
	out, code = runAt(t, h, repo, env, "agentic-ci", "fix-vuln", "r.sarif",
		"--provider", "local", "--apply", "--allow-tool", "write_file")
	if !strings.Contains(out, "FIXED") {
		t.Fatalf("granted write did not apply the fix (code=%d): %s", code, out)
	}
	b, _ = os.ReadFile(filepath.Join(repo, "vuln.py"))
	if !strings.Contains(string(b), "shell=False") {
		t.Fatalf("fix not written: %s", b)
	}
}

// ---------------------------------------------------------------------------
// PRD-060: ci-diagnose
// ---------------------------------------------------------------------------

func TestE2ECIDiagnoseOfflineIsHonest(t *testing.T) {
	h := newHome(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "ci.log", fixtureCILog)

	out, code := runAt(t, h, dir, nil, "--json", "agentic-ci", "ci-diagnose", p)
	if code != 0 {
		t.Fatalf("ci-diagnose is advisory and must exit 0, got %d: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if got["parsed"] != true {
		t.Errorf("the pytest/traceback log should parse: %v", got["parsed"])
	}
	f := got["failure"].(map[string]any)
	if f["file_path"] != "app/svc.py" || f["line_number"].(float64) != 42 {
		t.Errorf("failure extraction wrong: %v", f)
	}
	if f["error_type"] != "KeyError" {
		t.Errorf("error type wrong: %v", f["error_type"])
	}
	if got["degraded"] != true {
		t.Errorf("an echo run must be flagged degraded: %v", got["degraded"])
	}
	// The honesty note must state that the "diagnosis" is echoed, not analysed.
	if !strings.Contains(out, "echoed prompt, NOT an analysis") {
		t.Errorf("offline degradation not disclosed: %s", out)
	}
}

func TestE2ECIDiagnoseUnrecognisedLogSaysSo(t *testing.T) {
	h := newHome(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "weird.log", "the build machine caught fire\n")
	out, code := runAt(t, h, dir, nil, "agentic-ci", "ci-diagnose", p)
	if code != 0 {
		t.Fatalf("code=%d: %s", code, out)
	}
	if !strings.Contains(out, "no known failure pattern matched") {
		t.Errorf("an unparseable log must be disclosed, not silently diagnosed: %s", out)
	}
	if !strings.Contains(out, "parsed=false") {
		t.Errorf("parsed flag wrong: %s", out)
	}
}

func TestE2ECIDiagnoseBadInputIsUsageError(t *testing.T) {
	h := newHome(t)
	dir := t.TempDir()
	empty := writeFile(t, dir, "empty.log", "   \n")
	if out, code := runAt(t, h, dir, nil, "agentic-ci", "ci-diagnose", empty); code != 2 {
		t.Errorf("empty log must exit 2, got %d: %s", code, out)
	}
	if out, code := runAt(t, h, dir, nil, "agentic-ci", "ci-diagnose", "missing.log"); code != 2 {
		t.Errorf("missing log must exit 2, got %d: %s", code, out)
	}
	// Both spellings are wired to the same implementation.
	p := writeFile(t, dir, "ci.log", fixtureCILog)
	if out, code := runAt(t, h, dir, nil, "ci", "diagnose", p); code != 0 || !strings.Contains(out, "ci-diagnose") {
		t.Errorf("`tag ci diagnose` alias broken: code=%d out=%s", code, out)
	}
}

// ---------------------------------------------------------------------------
// PRD-061: review --signals
// ---------------------------------------------------------------------------

func TestE2EReviewDegradesHonestlyWithoutGH(t *testing.T) {
	h := newHome(t)
	dir := t.TempDir()
	// An empty PATH-ish environment with no gh must produce a clear message and a
	// runtime failure -- never a fabricated "review".
	out, code := runAt(t, h, dir, []string{"PATH=/nonexistent"}, "agentic-ci", "review", "owner/repo#42")
	if code != 1 {
		t.Fatalf("missing gh is a runtime failure (1), got %d: %s", code, out)
	}
	if !strings.Contains(out, "gh CLI not found") || !strings.Contains(out, "--diff") {
		t.Errorf("degradation message must name gh and the offline escape hatch: %s", out)
	}
}

func TestE2EReviewOfflineWithLocalDiff(t *testing.T) {
	h := newHome(t)
	dir := t.TempDir()
	d := writeFile(t, dir, "d.patch", "--- a/x.py\n+++ b/x.py\n@@\n+os.system(cmd)\n")
	out, code := runAt(t, h, dir, nil, "--json", "agentic-ci", "review", "#42", "--diff", d,
		"--signals", "security,style")
	if code != 0 {
		t.Fatalf("an inconclusive offline review must exit 0, got %d: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	sig := got["signals"].([]any)
	if len(sig) != 2 || sig[0] != "security" || sig[1] != "style" {
		t.Errorf("signals wrong: %v", sig)
	}
	// The Python bug: signals_found was every class whose NAME appeared in the
	// text -- and the prompt names them all. Echo returns the prompt verbatim, so
	// the buggy implementation would report BOTH classes as found here.
	found := got["signals_found"].([]any)
	if len(found) != 0 {
		t.Errorf("echoing the prompt must not be read as findings, got %v", found)
	}
	if got["verdict_parsed"] != false {
		t.Errorf("no verdict lines => verdict_parsed must be false: %v", got["verdict_parsed"])
	}
	if !strings.Contains(out, `"signals_found": []`) {
		t.Errorf("empty signals_found must be [] not null: %s", out)
	}
}

func TestE2EReviewWithFakeGHAndRealFindingsExits3(t *testing.T) {
	ghDir := fakeGH(t, `case "$2" in
  diff) printf -- '--- a/x.py\n+++ b/x.py\n@@\n+os.system(cmd)\n' ;;
  view) printf '{"title":"T","body":"B","author":{"login":"me"},"baseRefName":"main","headRefName":"f","labels":[{"name":"bug"}]}' ;;
  comment) echo posted ;;
esac`)
	srv := startTextServer(t, "security: FINDINGS\n- shell injection at x.py:1\nstyle: CLEAN\nREQUEST_CHANGES")
	env := []string{pathWith(ghDir), "TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}
	h := newHome(t)
	dir := t.TempDir()

	out, code := runAt(t, h, dir, env, "--json", "agentic-ci", "review", "owner/repo#42",
		"--signals", "security,style", "--provider", "local")
	if code != 3 {
		t.Fatalf("a review that found something must exit 3, got %d: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	found := got["signals_found"].([]any)
	if len(found) != 1 || found[0] != "security" {
		t.Errorf("want exactly [security] (style said CLEAN), got %v", found)
	}
	if got["verdict_parsed"] != true {
		t.Error("verdict lines present but not parsed")
	}

	// --exit-zero turns the gate off for advisory use.
	if _, code := runAt(t, h, dir, env, "agentic-ci", "review", "owner/repo#42",
		"--signals", "security", "--provider", "local", "--exit-zero"); code != 0 {
		t.Errorf("--exit-zero must force 0, got %d", code)
	}
}

func TestE2EReviewCleanVerdictExitsZero(t *testing.T) {
	ghDir := fakeGH(t, `case "$2" in
  diff) printf -- '--- a/x.py\n+++ b/x.py\n@@\n+x = 1\n' ;;
  view) printf '{"title":"T","body":"","author":{"login":"me"},"baseRefName":"main","headRefName":"f","labels":[]}' ;;
esac`)
	srv := startTextServer(t, "security: CLEAN\nstyle: CLEAN\nAPPROVE")
	env := []string{pathWith(ghDir), "TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}
	out, code := runAt(t, newHome(t), t.TempDir(), env, "agentic-ci", "review", "owner/repo#42",
		"--signals", "security,style", "--provider", "local")
	if code != 0 {
		t.Fatalf("a clean review must exit 0, got %d: %s", code, out)
	}
	if !strings.Contains(out, "CLEAN on every requested signal class") {
		t.Errorf("clean verdict not reported: %s", out)
	}
}

func TestE2EReviewUsageErrors(t *testing.T) {
	h := newHome(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bad ref", []string{"agentic-ci", "review", "garbage"}},
		{"bad signal", []string{"agentic-ci", "review", "#1", "--signals", "bogus"}},
		{"post with echo", []string{"agentic-ci", "review", "#1", "--post"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, code := runAt(t, h, dir, nil, tc.args...); code != 2 {
				t.Errorf("want exit 2, got %d: %s", code, out)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PRD-062: gen-pipeline
// ---------------------------------------------------------------------------

func TestE2EGenPipeline(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	writeFile(t, repo, "go.mod", "module x\n")
	writeFile(t, repo, "Dockerfile", "FROM scratch\n")

	// --dry-run must print and write NOTHING.
	out, code := runAt(t, h, repo, nil, "agentic-ci", "gen-pipeline", "--dry-run")
	if code != 0 {
		t.Fatalf("code=%d: %s", code, out)
	}
	if !strings.Contains(out, "go-test:") || !strings.Contains(out, "docker-build:") {
		t.Errorf("stack detection wrong: %s", out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".gitlab-ci.yml")); !os.IsNotExist(err) {
		t.Fatal("--dry-run wrote a file")
	}
	// The Python generator suffixed every script with `|| true`, so the pipeline
	// could never fail. Refuse to reproduce that.
	if strings.Contains(out, "|| true") {
		t.Error("generated pipeline neuters its own jobs with `|| true`")
	}

	// A real write is gated by the permission model.
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "gen-pipeline"); code != 1 ||
		!strings.Contains(out, "refusing to write") {
		t.Errorf("ungranted write should be refused: code=%d out=%s", code, out)
	}
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "gen-pipeline", "--allow-tool", "write_file"); code != 0 {
		t.Fatalf("granted write failed: code=%d out=%s", code, out)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".gitlab-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "stages:") {
		t.Errorf("written pipeline looks wrong: %s", body)
	}

	// Overwriting without --force is a usage error, not a silent clobber.
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "gen-pipeline", "--allow-tool", "write_file"); code != 2 {
		t.Errorf("overwrite without --force must exit 2, got %d: %s", code, out)
	}
	if _, code := runAt(t, h, repo, nil, "agentic-ci", "gen-pipeline", "--allow-tool", "write_file", "--force"); code != 0 {
		t.Errorf("--force overwrite failed: %d", code)
	}
}

func TestE2EGenPipelineEmptyStackJSON(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	out, code := runAt(t, h, repo, nil, "--json", "agentic-ci", "gen-pipeline", "--dry-run")
	if code != 0 {
		t.Fatalf("code=%d: %s", code, out)
	}
	if !strings.Contains(out, `"stack": []`) {
		t.Errorf("an empty stack must serialise as [] not null: %s", out)
	}
	if !strings.Contains(out, "no known stack detected") {
		t.Errorf("an undetectable repo must say so: %s", out)
	}
	if !strings.Contains(out, "placeholder:") {
		t.Errorf("placeholder job missing: %s", out)
	}
}

// ---------------------------------------------------------------------------
// PRD-063: flaky-fix
// ---------------------------------------------------------------------------

func TestE2EFlakyFixDetectsAndExits3(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	writeFile(t, repo, "tests/test_a.py", "def test_x():\n    pass\n")
	p := writeFile(t, repo, "flaky.log", fixtureFlakyLog)

	out, code := runAt(t, h, repo, nil, "--json", "agentic-ci", "flaky-fix", p)
	if code != 3 {
		t.Fatalf("detected flaky tests must exit 3, got %d: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if got["flaky_count"].(float64) != 1 {
		t.Errorf("test_y always fails (broken, not flaky) so exactly 1 is expected: %v", got["flaky_count"])
	}
	if got["fixes_applied"].(float64) != 0 {
		t.Error("offline run must not claim any applied fix")
	}
	if !strings.Contains(out, "provider=echo cannot produce a rewrite") {
		t.Errorf("offline degradation not disclosed: %s", out)
	}
	// The test file must be untouched.
	b, _ := os.ReadFile(filepath.Join(repo, "tests", "test_a.py"))
	if string(b) != "def test_x():\n    pass\n" {
		t.Fatalf("report-only mode modified the test file: %q", b)
	}
}

func TestE2EFlakyFixStableLogExitsZero(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	p := writeFile(t, repo, "ok.log", "PASSED tests/test_a.py::test_x\nPASSED tests/test_a.py::test_x\n")
	out, code := runAt(t, h, repo, nil, "--json", "agentic-ci", "flaky-fix", p)
	if code != 0 {
		t.Fatalf("no flaky tests must exit 0, got %d: %s", code, out)
	}
	if !strings.Contains(out, `"results": []`) {
		t.Errorf("empty results must be [] not null: %s", out)
	}
	// Honesty: a single-run log cannot prove stability, and the command says so.
	if !strings.Contains(out, "the log holds only ONE run") {
		t.Errorf("must disclose that a single run cannot detect flakiness: %s", out)
	}
}

func TestE2EFlakyFixBadInput(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	empty := writeFile(t, repo, "e.log", "\n\n")
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "flaky-fix", empty); code != 2 {
		t.Errorf("empty log must exit 2, got %d: %s", code, out)
	}
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "flaky-fix", "missing.log"); code != 2 {
		t.Errorf("missing log must exit 2, got %d: %s", code, out)
	}
	p := writeFile(t, repo, "f.log", fixtureFlakyLog)
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "flaky-fix", p, "--quarantine-threshold", "7"); code != 2 {
		t.Errorf("out-of-range threshold must exit 2, got %d: %s", code, out)
	}
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "flaky-fix", p, "--apply", "--dry-run"); code != 2 {
		t.Errorf("--apply with --dry-run must exit 2, got %d: %s", code, out)
	}
}

// TestE2EFlakyFixRefusesUnparseableRewrite pins the Python behaviour this port
// declined to copy: fix_flaky_test wrote the model's whole raw response to the
// test file whenever the "EXPLANATION:" marker was missing, i.e. it could
// replace a test with prose and report fix_applied=true.
func TestE2EFlakyFixRefusesUnparseableRewrite(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	original := "def test_x():\n    pass\n"
	writeFile(t, repo, "tests/test_a.py", original)
	p := writeFile(t, repo, "flaky.log", fixtureFlakyLog)
	srv := startTextServer(t, "I think this test is flaky because of timing issues.")
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	out, code := runAt(t, h, repo, env, "agentic-ci", "flaky-fix", p,
		"--provider", "local", "--apply", "--allow-tool", "write_file")
	if code != 3 {
		t.Fatalf("still-flaky => exit 3, got %d: %s", code, out)
	}
	if !strings.Contains(out, "refusing to overwrite") {
		t.Errorf("must refuse an unparseable rewrite: %s", out)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "tests", "test_a.py"))
	if string(b) != original {
		t.Fatalf("test file was clobbered with prose: %q", b)
	}
}

// ---------------------------------------------------------------------------
// PRD-057: test-gen
// ---------------------------------------------------------------------------

func TestE2ETestGenOfflineRefusesToWriteEchoedOutput(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	writeFile(t, repo, "go.mod", "module x\n")
	d := writeFile(t, repo, "d.patch", "--- a/x.go\n+++ b/x.go\n@@\n+func Add(a,b int) int { return a+b }\n")

	out, code := runAt(t, h, repo, nil, "--json", "agentic-ci", "test-gen",
		"--diff", d, "--out", "gen_test.go", "--allow-tool", "write_file")
	if code != 0 {
		t.Fatalf("code=%d: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if got["framework"] != "go-test" {
		t.Errorf("framework detection wrong: %v", got["framework"])
	}
	if got["written"] != false {
		t.Error("echoed context must NOT be written out as generated tests")
	}
	if _, err := os.Stat(filepath.Join(repo, "gen_test.go")); !os.IsNotExist(err) {
		t.Fatal("echoed context was written to disk")
	}
	if !strings.Contains(out, "ECHOED CONTEXT, not generated tests") {
		t.Errorf("degradation not disclosed: %s", out)
	}
}

func TestE2ETestGenWritesWithARealProvider(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	writeFile(t, repo, "go.mod", "module x\n")
	d := writeFile(t, repo, "d.patch", "--- a/x.go\n+++ b/x.go\n@@\n+func Add(a,b int) int { return a+b }\n")
	srv := startTextServer(t, "func TestAdd(t *testing.T) { _ = Add(1,2) }")
	env := []string{"TAG_LOCAL_BASE_URL=" + srv.URL + "/v1", "TAG_LOCAL_API_KEY=x"}

	out, code := runAt(t, h, repo, env, "agentic-ci", "test-gen", "--diff", d,
		"--out", "gen_test.go", "--provider", "local", "--allow-tool", "write_file")
	if code != 0 {
		t.Fatalf("code=%d: %s", code, out)
	}
	b, err := os.ReadFile(filepath.Join(repo, "gen_test.go"))
	if err != nil {
		t.Fatalf("tests not written: %v\n%s", err, out)
	}
	if !strings.Contains(string(b), "func TestAdd") {
		t.Errorf("wrong content written: %s", b)
	}
}

func TestE2ETestGenUsageErrors(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	// Python happily "generated tests" from an empty diff. That is confidently
	// worded output about nothing; refuse it.
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "test-gen"); code != 2 {
		t.Errorf("empty diff must exit 2, got %d: %s", code, out)
	}
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "test-gen", "--staged", "--diff", "x"); code != 2 {
		t.Errorf("--staged with --diff must exit 2, got %d: %s", code, out)
	}
	if out, code := runAt(t, h, repo, nil, "agentic-ci", "test-gen", "--diff", "x", "--provider", "nope"); code != 2 {
		t.Errorf("unknown provider must exit 2, got %d: %s", code, out)
	}
}

// ---------------------------------------------------------------------------
// spans: every LLM-backed subcommand must produce a trace
// ---------------------------------------------------------------------------

func TestE2EAgenticCISubcommandsEmitSpans(t *testing.T) {
	h := newHome(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "ci.log", fixtureCILog)
	out, code := runAt(t, h, dir, nil, "--json", "agentic-ci", "ci-diagnose", p)
	if code != 0 {
		t.Fatalf("code=%d: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	traceID, _ := got["trace_id"].(string)
	if traceID == "" {
		t.Fatal("no trace_id reported")
	}
	spans, code := run(t, h, "trace", "show", traceID)
	if code != 0 || strings.Contains(spans, "No spans") {
		t.Errorf("no spans recorded for trace %s: code=%d out=%s", traceID, code, spans)
	}
}

// firstJSONLineT returns the first line of out that parses as a JSON object,
// so an assertion is not confused by a trailing human-readable "error:" line.
func firstJSONLineT(t *testing.T, out string) string {
	t.Helper()
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "{") {
			return ln
		}
	}
	t.Fatalf("no JSON object line in output:\n%s", out)
	return ""
}
