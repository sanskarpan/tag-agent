package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var tagBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tagbin")
	if err != nil {
		panic(err)
	}
	tagBin = filepath.Join(dir, "tag")
	build := exec.Command("go", "build", "-tags", "ssrf_testhook", "-o", tagBin, "../../cmd/tag")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		panic("build failed: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// run executes the binary with an isolated TAG_HOME and returns stdout+stderr, exit code.
func run(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(os.Environ(), "TAG_HOME="+home)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

func newHome(t *testing.T) string {
	h := t.TempDir()
	run(t, h, "bootstrap")
	return h
}

func TestE2EVersionAndDoctor(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "--version"); code != 0 || !strings.Contains(out, "0.9.0-go") {
		t.Errorf("version: %q code=%d", out, code)
	}
	if out, code := run(t, h, "doctor"); code != 0 || !strings.Contains(out, "tag_home") {
		t.Errorf("doctor: %q code=%d", out, code)
	}
}

func TestE2EMemoryLifecycle(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "mem", "add", "the sky is blue", "--type", "fact"); code != 0 || !strings.Contains(out, "Memory saved") {
		t.Fatalf("mem add: %q %d", out, code)
	}
	if _, code := run(t, h, "mem", "add", "x", "--confidence", "0"); code == 0 {
		t.Error("confidence 0 should exit nonzero")
	}
	if out, _ := run(t, h, "mem", "search", "sky"); !strings.Contains(out, "the sky is blue") {
		t.Errorf("search: %q", out)
	}
	if out, _ := run(t, h, "mem", "list", "--json"); !strings.Contains(out, "the sky is blue") {
		t.Errorf("list json: %q", out)
	}
}

func TestE2ECronValidation(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "cron", "add", "t", "--name", "n", "--schedule", "0 2 * * *"); code != 0 {
		t.Error("valid cron should succeed")
	}
	for _, bad := range [][]string{{"-1 0 * * *"}, {"*/0 0 * * *"}, {"50-10 0 * * *"}} {
		if _, code := run(t, h, "cron", "add", "t", "--name", "n", "--schedule", bad[0]); code == 0 {
			t.Errorf("invalid cron %q should fail", bad[0])
		}
	}
}

func TestE2EBudgetAndRouteFallback(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "budget", "set", "--profile", "coder", "--max-tokens", "100000"); code != 0 {
		t.Error("budget set failed")
	}
	if out, _ := run(t, h, "budget", "get", "--profile", "coder"); !strings.Contains(out, "100000") {
		t.Errorf("budget get: %q", out)
	}
	run(t, h, "route-fallback", "add", "--primary", "a", "--fallback", "b")
	if _, code := run(t, h, "route-fallback", "add", "--primary", "b", "--fallback", "a"); code == 0 {
		t.Error("cycle should be rejected")
	}
}

func TestE2ESecurityScan(t *testing.T) {
	h := newHome(t)
	f := filepath.Join(t.TempDir(), "secrets.env")
	os.WriteFile(f, []byte("AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\n"), 0o644)
	out, code := run(t, h, "security", "scan", f)
	if code == 0 || !strings.Contains(out, "potential secret") {
		t.Errorf("scan should find the key and exit nonzero: %q %d", out, code)
	}
	clean := filepath.Join(t.TempDir(), "clean.txt")
	os.WriteFile(clean, []byte("hello world\n"), 0o644)
	if out, code := run(t, h, "security", "scan", clean); code != 0 || !strings.Contains(out, "No secrets") {
		t.Errorf("clean scan: %q %d", out, code)
	}
}

func TestE2EQueueAndDag(t *testing.T) {
	h := newHome(t)
	if out, _ := run(t, h, "queue", "add", "run tests"); !strings.Contains(out, "queued") {
		t.Errorf("queue add: %q", out)
	}
	if out, _ := run(t, h, "dag", "save", "p", "--steps", `[{"name":"a","task":"build"}]`); !strings.Contains(out, "saved") {
		t.Errorf("dag save: %q", out)
	}
	if _, code := run(t, h, "dag", "save", "bad", "--steps", `[{"name":"a"}]`); code == 0 {
		t.Error("dag with missing task should fail")
	}
}

func TestE2EPricing(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "pricing", "get", "--model", "openai/gpt-4o", "--input-tokens", "1000", "--output-tokens", "500")
	if code != 0 || !strings.Contains(out, "0.00750000") {
		t.Errorf("pricing: %q %d", out, code)
	}
}

func TestE2EHelpSweepNoCrash(t *testing.T) {
	h := newHome(t)
	for _, c := range []string{"mem", "memory-journal", "budget", "persona", "route-fallback", "cron", "queue", "dag", "security", "workspace", "costs", "pricing", "trace"} {
		if out, code := run(t, h, c, "--help"); code != 0 {
			t.Errorf("%s --help exit %d: %q", c, code, out)
		}
	}
}

func TestE2ERouting(t *testing.T) {
	h := newHome(t)
	// route resolves master/worker/verifier from config
	if out, code := run(t, h, "route", "research"); code != 0 ||
		!strings.Contains(out, "master: orchestrator") ||
		!strings.Contains(out, "worker: researcher") {
		t.Errorf("route research: %q code=%d", out, code)
	}
	// unknown task type errors
	if out, code := run(t, h, "route", "bogus"); code == 0 || !strings.Contains(out, "unknown task type") {
		t.Errorf("route bogus should fail: %q code=%d", out, code)
	}
	// assignments lists profiles
	if out, code := run(t, h, "assignments"); code != 0 || !strings.Contains(out, "coder:") {
		t.Errorf("assignments: %q code=%d", out, code)
	}
	// set-model persists into config, reflected by assignments
	if out, code := run(t, h, "set-model", "coder", "openrouter/foo-model"); code != 0 ||
		!strings.Contains(out, "coder primary model -> openrouter/foo-model") {
		t.Errorf("set-model: %q code=%d", out, code)
	}
	if out, code := run(t, h, "assignments"); code != 0 || !strings.Contains(out, "coder: openrouter/foo-model") {
		t.Errorf("assignments after set-model: %q code=%d", out, code)
	}
	// invalid model ref rejected
	if _, code := run(t, h, "set-model", "coder", "badref"); code == 0 {
		t.Error("set-model badref should fail")
	}
	// unknown profile rejected
	if _, code := run(t, h, "set-model", "nobody", "openrouter/x"); code == 0 {
		t.Error("set-model unknown profile should fail")
	}
	// master-model override applies
	if out, code := run(t, h, "route", "research", "--master-model", "anthropic/claude-opus"); code != 0 ||
		!strings.Contains(out, "master: orchestrator -> anthropic/claude-opus") {
		t.Errorf("route master-model override: %q code=%d", out, code)
	}
	// worker override naming a non-worker is rejected (typo detection)
	if _, code := run(t, h, "route", "research", "--worker-model", "coder=openrouter/x"); code == 0 {
		t.Error("worker override for non-route-worker should fail")
	}
}

func TestE2ENotify(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "notify", "list"); code != 0 || !strings.Contains(out, "No notification hooks") {
		t.Errorf("notify list empty: %q code=%d", out, code)
	}
	if out, code := run(t, h, "notify", "add", "--channel", "slack", "--event", "budget.exceeded",
		"--config-json", `{"url":"https://x"}`, "--template", "blown {{profile}}"); code != 0 ||
		!strings.Contains(out, "Notification hook added") {
		t.Errorf("notify add: %q code=%d", out, code)
	}
	// invalid channel / event / json rejected
	if _, code := run(t, h, "notify", "add", "--channel", "pigeon"); code == 0 {
		t.Error("bad channel should fail")
	}
	if _, code := run(t, h, "notify", "add", "--event", "run.exploded"); code == 0 {
		t.Error("bad event should fail")
	}
	if _, code := run(t, h, "notify", "add", "--config-json", "{bad"); code == 0 {
		t.Error("bad json should fail")
	}
	// list shows the hook; grab its id
	out, code := run(t, h, "notify", "list")
	if code != 0 || !strings.Contains(out, "slack") || !strings.Contains(out, "budget.exceeded") {
		t.Fatalf("notify list: %q code=%d", out, code)
	}
	id := strings.Fields(out)[1] // "✓ <id> slack ..."
	// test renders template
	if o, c := run(t, h, "notify", "test", id); c != 0 || !strings.Contains(o, "rendered message") {
		t.Errorf("notify test: %q code=%d", o, c)
	}
	// disable flips the marker
	run(t, h, "notify", "disable", id)
	if o, _ := run(t, h, "notify", "list"); !strings.Contains(o, "✗") {
		t.Errorf("notify disable should show ✗: %q", o)
	}
	// remove, then removing again fails
	if o, c := run(t, h, "notify", "remove", id); c != 0 || !strings.Contains(o, "removed") {
		t.Errorf("notify remove: %q code=%d", o, c)
	}
	if _, c := run(t, h, "notify", "remove", "deadbeef"); c == 0 {
		t.Error("removing missing hook should fail")
	}
}

func TestE2EGraph(t *testing.T) {
	h := newHome(t)
	run(t, h, "mem", "add", "We use Python and Docker with Postgres", "--type", "fact")
	run(t, h, "mem", "add", "Alice Johnson works at Acme Corp on Python", "--type", "fact")
	if out, code := run(t, h, "graph", "show"); code != 0 || !strings.Contains(out, "0 entities") {
		t.Errorf("graph show pre-build: %q code=%d", out, code)
	}
	out, code := run(t, h, "graph", "build")
	if code != 0 || !strings.Contains(out, "Built graph from 2 memories") {
		t.Fatalf("graph build: %q code=%d", out, code)
	}
	// idempotent rebuild yields identical output
	out2, _ := run(t, h, "graph", "build")
	if out2 != out {
		t.Errorf("graph build not idempotent:\n%q\nvs\n%q", out, out2)
	}
	if o, c := run(t, h, "graph", "query", "Python"); c != 0 || !strings.Contains(o, "Python (technology) mentions=2") {
		t.Errorf("graph query Python: %q code=%d", o, c)
	}
	if o, _ := run(t, h, "graph", "query", "Zebra"); !strings.Contains(o, "No entities matching") {
		t.Errorf("graph query missing: %q", o)
	}
}

func TestE2EPrompt(t *testing.T) {
	h := newHome(t)
	p1 := filepath.Join(t.TempDir(), "p1.txt")
	p2 := filepath.Join(t.TempDir(), "p2.txt")
	os.WriteFile(p1, []byte("Summarize {{topic}}.\nBe concise.\n"), 0o644)
	os.WriteFile(p2, []byte("Summarize {{topic}}.\nBe concise and cite.\nUse bullets.\n"), 0o644)

	if out, code := run(t, h, "prompt", "save", "sum", p1, "--notes", "init"); code != 0 || !strings.Contains(out, "v1") {
		t.Fatalf("prompt save v1: %q code=%d", out, code)
	}
	if out, code := run(t, h, "prompt", "save", "sum", p2, "--notes", "cite"); code != 0 || !strings.Contains(out, "v2") {
		t.Fatalf("prompt save v2: %q code=%d", out, code)
	}
	// blank name + missing file rejected
	if _, code := run(t, h, "prompt", "save", "  ", p1); code == 0 {
		t.Error("blank name should fail")
	}
	if _, code := run(t, h, "prompt", "save", "x", filepath.Join(h, "nope.txt")); code == 0 {
		t.Error("missing file should fail")
	}
	// list shows latest version
	if out, code := run(t, h, "prompt", "list"); code != 0 || !strings.Contains(out, "v2 (2 versions)") {
		t.Errorf("prompt list: %q code=%d", out, code)
	}
	// get latest vs pinned
	if out, _ := run(t, h, "prompt", "get", "sum"); !strings.Contains(out, "Use bullets") {
		t.Errorf("get latest wrong: %q", out)
	}
	if out, _ := run(t, h, "prompt", "get", "sum", "--version", "1"); strings.Contains(out, "Use bullets") {
		t.Errorf("get v1 should not have v2 content: %q", out)
	}
	// missing prompt vs missing version distinguished (C043)
	if o, c := run(t, h, "prompt", "get", "nope"); c == 0 || !strings.Contains(o, "prompt not found") {
		t.Errorf("get missing prompt: %q code=%d", o, c)
	}
	if o, c := run(t, h, "prompt", "get", "sum", "--version", "9"); c == 0 || !strings.Contains(o, "version 9 not found") {
		t.Errorf("get missing version: %q code=%d", o, c)
	}
	// diff shows removed/added lines
	out, code := run(t, h, "prompt", "diff", "sum", "1", "2")
	if code != 0 || !strings.Contains(out, "-Be concise.") || !strings.Contains(out, "+Use bullets.") {
		t.Errorf("prompt diff: %q code=%d", out, code)
	}
}

func TestE2EAlert(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "alert", "create", "too-many", "memory_count", "gt", "2", "--severity", "critical"); code != 0 ||
		!strings.Contains(out, "Created rule") {
		t.Fatalf("alert create: %q code=%d", out, code)
	}
	// validation: bad metric / condition / severity
	if _, c := run(t, h, "alert", "create", "x", "bogus", "gt", "1"); c == 0 {
		t.Error("bad metric should fail")
	}
	if _, c := run(t, h, "alert", "create", "x", "memory_count", "zz", "1"); c == 0 {
		t.Error("bad condition should fail")
	}
	if _, c := run(t, h, "alert", "create", "x", "memory_count", "gt", "1", "--severity", "meh"); c == 0 {
		t.Error("bad severity should fail")
	}
	// no memories -> no firing
	if out, _ := run(t, h, "alert", "check"); !strings.Contains(out, "No alerts firing") {
		t.Errorf("check with 0 memories should not fire: %q", out)
	}
	// 3 memories -> rule (gt 2) fires
	run(t, h, "mem", "add", "a", "--type", "fact")
	run(t, h, "mem", "add", "b", "--type", "fact")
	run(t, h, "mem", "add", "c", "--type", "fact")
	if out, _ := run(t, h, "alert", "check"); !strings.Contains(out, "[CRITICAL]") || !strings.Contains(out, "memory_count = 3 > 2") {
		t.Errorf("check should fire: %q", out)
	}
	// cooldown suppresses the immediate re-check
	if out, _ := run(t, h, "alert", "check"); !strings.Contains(out, "No alerts firing") {
		t.Errorf("cooldown should suppress re-fire: %q", out)
	}
	// firing recorded
	if out, _ := run(t, h, "alert", "firings"); !strings.Contains(out, "too-many") {
		t.Errorf("firings should list the firing: %q", out)
	}
	// delete removes rule + firing history
	out, _ := run(t, h, "alert", "list")
	id := strings.Fields(out)[0]
	if o, c := run(t, h, "alert", "delete", id); c != 0 || !strings.Contains(o, "Deleted") {
		t.Errorf("alert delete: %q code=%d", o, c)
	}
	if _, c := run(t, h, "alert", "delete", "deadbeef"); c == 0 {
		t.Error("delete missing should fail")
	}
}

func TestE2EAnnotate(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "annotate", "add", "sky color?", "--question", "factual?", "--priority", "1"); code != 0 {
		t.Fatal("annotate add 1 failed")
	}
	if _, code := run(t, h, "annotate", "add", "2+2=5", "--question", "correct?", "--priority", "5"); code != 0 {
		t.Fatal("annotate add 2 failed")
	}
	// missing --question rejected
	if _, code := run(t, h, "annotate", "add", "x"); code == 0 {
		t.Error("add without --question should fail")
	}
	// stats: 2 pending
	if out, _ := run(t, h, "annotate", "stats"); !strings.Contains(out, `"pending": 2`) {
		t.Errorf("stats pending: %q", out)
	}
	// next claims highest priority (2+2=5)
	out, code := run(t, h, "annotate", "next", "--assignee", "alice")
	if code != 0 || !strings.Contains(out, "2+2=5") {
		t.Fatalf("next should claim highest-priority task: %q code=%d", out, code)
	}
	tid := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "["), "]")
	tid = strings.Split(tid, "]")[0]
	// label it
	if o, c := run(t, h, "annotate", "label", tid, "incorrect", "--notes", "math"); c != 0 || !strings.Contains(o, "Labeled") {
		t.Errorf("annotate label: %q code=%d", o, c)
	}
	if _, c := run(t, h, "annotate", "label", "deadbeef", "x"); c == 0 {
		t.Error("label missing task should fail")
	}
	// export jsonl contains the labeled record
	if o, _ := run(t, h, "annotate", "export", "--format", "jsonl"); !strings.Contains(o, `"label":"incorrect"`) {
		t.Errorf("export jsonl: %q", o)
	}
	// export csv has a header row
	if o, _ := run(t, h, "annotate", "export", "--format", "csv"); !strings.Contains(o, "id,source_type,source_id") {
		t.Errorf("export csv: %q", o)
	}
	// bad format rejected
	if _, c := run(t, h, "annotate", "export", "--format", "xml"); c == 0 {
		t.Error("bad export format should fail")
	}
}

func TestE2EEvalDataset(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "eval-dataset", "create", "qa", "--description", "cases"); code != 0 || !strings.Contains(out, "Created dataset 'qa'") {
		t.Fatalf("eval-dataset create: %q code=%d", out, code)
	}
	// duplicate rejected
	if _, code := run(t, h, "eval-dataset", "create", "qa"); code == 0 {
		t.Error("duplicate dataset should fail")
	}
	run(t, h, "eval-dataset", "add-case", "qa", "c1", "2+2?", "--expected", "4")
	run(t, h, "eval-dataset", "add-case", "qa", "c2", "silence", "--expected", "")
	run(t, h, "eval-dataset", "add-case", "qa", "c3", "open")
	// add-case to missing dataset fails
	if _, c := run(t, h, "eval-dataset", "add-case", "nope", "c1", "x"); c == 0 {
		t.Error("add-case to missing dataset should fail")
	}
	if out, _ := run(t, h, "eval-dataset", "list"); !strings.Contains(out, "qa") || !strings.Contains(out, "3 cases") {
		t.Errorf("eval-dataset list: %q", out)
	}
	// export preserves explicit "" (C022) and omits unset expected_output
	out, code := run(t, h, "eval-dataset", "export", "qa")
	if code != 0 {
		t.Fatalf("export failed: %q", out)
	}
	if !strings.Contains(out, `expected_output: "4"`) {
		t.Errorf("c1 expected_output missing: %q", out)
	}
	if !strings.Contains(out, `expected_output: ""`) {
		t.Errorf("c2 explicit empty expected_output must be preserved (C022): %q", out)
	}
	// c3 block must not carry expected_output
	if strings.Count(out, "expected_output") != 2 {
		t.Errorf("c3 should omit expected_output (want 2 total): %q", out)
	}
	// export/delete of missing dataset fails
	if _, c := run(t, h, "eval-dataset", "export", "nope"); c == 0 {
		t.Error("export missing should fail")
	}
	if o, c := run(t, h, "eval-dataset", "delete", "qa"); c != 0 || !strings.Contains(o, "Deleted") {
		t.Errorf("delete: %q code=%d", o, c)
	}
}

func TestE2EMem2(t *testing.T) {
	h := newHome(t)
	run(t, h, "mem", "add", "deploy uses github actions and docker", "--type", "fact", "--confidence", "0.95")
	run(t, h, "mem", "add", "deploy uses github actions and docker containers", "--type", "fact", "--confidence", "0.6")
	run(t, h, "mem", "add", "an architectural decision here", "--type", "decision", "--confidence", "0.85")
	// tier classification present
	if out, code := run(t, h, "mem2", "tier"); code != 0 || !strings.Contains(out, "=== CORE") || !strings.Contains(out, "=== RECALL") {
		t.Errorf("mem2 tier: %q code=%d", out, code)
	}
	// dry-run makes no changes
	if out, _ := run(t, h, "mem2", "gc", "--dry-run"); !strings.Contains(out, "dry-run") {
		t.Errorf("mem2 gc dry-run: %q", out)
	}
	// real gc merges the near-duplicate
	if out, code := run(t, h, "mem2", "gc"); code != 0 || !strings.Contains(out, "merged=1") {
		t.Errorf("mem2 gc should merge duplicate: %q code=%d", out, code)
	}
	// invalid tier rejected
	if _, code := run(t, h, "mem2", "tier", "--tier", "bogus"); code == 0 {
		t.Error("invalid tier should fail")
	}
}

func TestE2EDiffContext(t *testing.T) {
	h := newHome(t)
	repo := t.TempDir()
	gitCmd := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitCmd("init", "-q")
	os.WriteFile(filepath.Join(repo, "app.py"), []byte("line1\nline2\n"), 0o644)
	gitCmd("add", "-A")
	gitCmd("commit", "-qm", "init")
	os.WriteFile(filepath.Join(repo, "app.py"), []byte("line1\nline2 changed\nline3\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "creds.token"), []byte("API_KEY=xyz\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "logo.png"), []byte("binary"), 0o644)
	gitCmd("add", "-A")

	// staged output-only: app.py included, secret+binary skipped
	out, code := run(t, h, "diff-context", "--staged", "--output-only", "--workdir", repo)
	if code != 0 || !strings.Contains(out, "app.py") || !strings.Contains(out, "line2 changed") {
		t.Errorf("diff-context: %q code=%d", out, code)
	}
	// The skip line (stderr) names the file, but the secret VALUE must never appear
	// in the diff body, and the token file must not get its own diff block.
	if strings.Contains(out, "API_KEY=xyz") || strings.Contains(out, "### creds.token") {
		t.Errorf("secret must be filtered out of the diff body: %q", out)
	}
	// non-git dir errors concisely
	if o, c := run(t, h, "diff-context", "--output-only", "--workdir", h); c == 0 || !strings.Contains(o, "not a git repository") {
		t.Errorf("non-git dir should error: %q code=%d", o, c)
	}
}

func TestE2EHooks(t *testing.T) {
	h := newHome(t)
	// no hooks configured yet
	if out, code := run(t, h, "hooks", "list"); code != 0 || !strings.Contains(out, "No hooks configured") {
		t.Errorf("hooks list empty: %q code=%d", out, code)
	}
	// append a hooks section to the rendered config
	marker := filepath.Join(t.TempDir(), "hookmark.txt")
	cfgPath := filepath.Join(h, "config", "tag.yaml")
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\nhooks:\n  run.completed:\n    - name: mark\n      type: shell\n      command: 'printf fired > %s'\n    - name: noop\n      type: shell\n      command: 'true'\n", marker)
	f.Close()

	if out, code := run(t, h, "hooks", "list"); code != 0 || !strings.Contains(out, "mark: shell") {
		t.Errorf("hooks list: %q code=%d", out, code)
	}
	// test fires both shell hooks
	if out, code := run(t, h, "hooks", "test", "run.completed"); code != 0 || !strings.Contains(out, "Fired 2 hook(s)") {
		t.Errorf("hooks test: %q code=%d", out, code)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "fired" {
		t.Errorf("shell hook did not write marker: %q err=%v", string(b), err)
	}
	// unmatched event warns
	if out, _ := run(t, h, "hooks", "test", "no.such.event"); !strings.Contains(out, "No hooks matched") {
		t.Errorf("hooks test unmatched: %q", out)
	}
	// log records the firings
	if out, _ := run(t, h, "hooks", "log"); !strings.Contains(out, "mark") || !strings.Contains(out, "ok") {
		t.Errorf("hooks log: %q", out)
	}
	// invalid limit rejected
	if _, code := run(t, h, "hooks", "log", "--limit", "0"); code == 0 {
		t.Error("hooks log --limit 0 should fail")
	}
}

func TestE2EMCPRegistry(t *testing.T) {
	h := newHome(t)
	// list shows the catalog
	if out, code := run(t, h, "mcp-registry", "list"); code != 0 || !strings.Contains(out, "mcp-github") {
		t.Errorf("mcp-registry list: %q code=%d", out, code)
	}
	// category filter narrows results
	out, _ := run(t, h, "mcp-registry", "list", "--category", "web")
	if !strings.Contains(out, "mcp-fetch") || strings.Contains(out, "mcp-github") {
		t.Errorf("category filter web: %q", out)
	}
	// unknown server enable fails
	if _, code := run(t, h, "mcp-registry", "enable", "nope"); code == 0 {
		t.Error("enabling unknown server should fail")
	}
	// enable writes the profile config, idempotent on re-enable
	if o, c := run(t, h, "mcp-registry", "enable", "mcp-github", "--profile", "coder"); c != 0 || !strings.Contains(o, "Enabled") {
		t.Errorf("enable: %q code=%d", o, c)
	}
	if o, _ := run(t, h, "mcp-registry", "enable", "mcp-github", "--profile", "coder"); !strings.Contains(o, "already enabled") {
		t.Errorf("re-enable should be idempotent: %q", o)
	}
	// the profile config.yaml now lists the server
	cfgFile := filepath.Join(h, "runtime", "home", ".hermes", "profiles", "coder", "config.yaml")
	if b, err := os.ReadFile(cfgFile); err != nil || !strings.Contains(string(b), "mcp-github") {
		t.Errorf("profile config should contain mcp-github: %q err=%v", string(b), err)
	}
	// disable removes it
	if o, c := run(t, h, "mcp-registry", "disable", "mcp-github", "--profile", "coder"); c != 0 || !strings.Contains(o, "Disabled") {
		t.Errorf("disable: %q code=%d", o, c)
	}
}

func TestE2ETemplate(t *testing.T) {
	h := newHome(t)
	// seed a profile home with a secret + non-secret env and a config
	pdir := filepath.Join(h, "runtime", "home", ".hermes", "profiles", "coder")
	os.MkdirAll(pdir, 0o755)
	os.WriteFile(filepath.Join(pdir, ".env"), []byte("ANTHROPIC_API_KEY=sk-secret\nLOG_LEVEL=debug\n"), 0o600)
	os.WriteFile(filepath.Join(pdir, "config.yaml"), []byte("model:\n  provider: openrouter\n"), 0o644)

	// export redacts the secret, keeps the rest
	out, code := run(t, h, "template", "export", "--profile", "coder")
	if code != 0 || !strings.Contains(out, "ANTHROPIC_API_KEY: <ANTHROPIC_API_KEY>") || !strings.Contains(out, "LOG_LEVEL: debug") {
		t.Fatalf("template export: %q code=%d", out, code)
	}
	if strings.Contains(out, "sk-secret") {
		t.Errorf("secret value must not appear in export: %q", out)
	}
	// round-trip through a file
	tmplFile := filepath.Join(t.TempDir(), "tmpl.yaml")
	run(t, h, "template", "export", "--profile", "coder", "--output", tmplFile)
	if o, c := run(t, h, "template", "import", tmplFile, "--profile", "coder2"); c != 0 || !strings.Contains(o, "imported as profile 'coder2'") {
		t.Errorf("template import: %q code=%d", o, c)
	}
	// imported .env comments the placeholder secret and is 0600
	envFile := filepath.Join(h, "runtime", "home", ".hermes", "profiles", "coder2", ".env")
	b, err := os.ReadFile(envFile)
	if err != nil || !strings.Contains(string(b), "# ANTHROPIC_API_KEY=<fill in>") || !strings.Contains(string(b), "LOG_LEVEL=debug") {
		t.Errorf("imported .env wrong: %q err=%v", string(b), err)
	}
	if fi, _ := os.Stat(envFile); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Errorf(".env should be 0600, got %o", fi.Mode().Perm())
	}
	// re-import over existing profile fails
	if _, c := run(t, h, "template", "import", tmplFile, "--profile", "coder2"); c == 0 {
		t.Error("import over existing profile should fail")
	}
	// path-traversal profile name rejected
	if _, c := run(t, h, "template", "import", tmplFile, "--profile", "../evil"); c == 0 {
		t.Error("path-traversal profile name should be rejected")
	}
}

func TestE2ECompare(t *testing.T) {
	h := newHome(t)
	run(t, h, "mem", "stats") // touch the DB so the schema exists
	dbPath := filepath.Join(h, "runtime", "tag.sqlite3")
	// seed a comparison + two results via the sqlite3 CLI if available; else skip
	seed := "INSERT INTO benchmark_comparisons(id,suite_path,models,created_at,status) VALUES('cmp1','suites/qa.yaml','[\"gpt-5\"]','2026-07-01T00:00:00Z','completed');" +
		"INSERT INTO benchmark_results(id,comparison_id,model_id,case_id,quality_score,latency_ms,created_at) VALUES('r1','cmp1','gpt-5','c1',0.92,1200,'2026-07-01T00:00:00Z');"
	c := exec.Command("sqlite3", dbPath, seed)
	if err := c.Run(); err != nil {
		t.Skip("sqlite3 CLI not available to seed benchmark rows")
	}
	if out, code := run(t, h, "compare", "list"); code != 0 || !strings.Contains(out, "cmp1") || !strings.Contains(out, "completed") {
		t.Errorf("compare list: %q code=%d", out, code)
	}
	if out, code := run(t, h, "compare", "show", "cmp1"); code != 0 || !strings.Contains(out, "gpt-5") || !strings.Contains(out, "0.92") {
		t.Errorf("compare show: %q code=%d", out, code)
	}
	if _, code := run(t, h, "compare", "show", "nope"); code == 0 {
		t.Error("compare show missing should fail")
	}
	if out, _ := run(t, h, "compare", "list", "--json"); !strings.Contains(out, `"suite_path"`) {
		t.Errorf("compare list --json should use snake_case keys: %q", out)
	}
}

func TestE2EMem2Episode(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "mem2", "episode", "start", "--summary", "debugging")
	if code != 0 || !strings.Contains(out, "Episode started:") {
		t.Fatalf("episode start: %q code=%d", out, code)
	}
	ep := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "Episode started:"))
	if o, _ := run(t, h, "mem2", "episode", "list"); !strings.Contains(o, ep) || !strings.Contains(o, `"status": "open"`) {
		t.Errorf("episode list: %q", o)
	}
	// get by positional id
	if o, c := run(t, h, "mem2", "episode", "get", ep); c != 0 || !strings.Contains(o, ep) {
		t.Errorf("episode get: %q code=%d", o, c)
	}
	// end it
	if o, c := run(t, h, "mem2", "episode", "end", "--id", ep, "--summary", "done"); c != 0 || !strings.Contains(o, "Episode ended") {
		t.Errorf("episode end: %q code=%d", o, c)
	}
	if o, _ := run(t, h, "mem2", "episode", "list"); !strings.Contains(o, `"status": "closed"`) {
		t.Errorf("episode should be closed: %q", o)
	}
	// error paths
	if _, c := run(t, h, "mem2", "episode", "end", "--id", "nope"); c == 0 {
		t.Error("ending missing episode should fail")
	}
	if _, c := run(t, h, "mem2", "episode", "frob"); c == 0 {
		t.Error("bad episode action should fail")
	}
}

func TestE2EPlugin(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "plugin", "list"); code != 0 || !strings.Contains(out, "hermes-web-search") {
		t.Errorf("plugin list: %q code=%d", out, code)
	}
	if _, code := run(t, h, "plugin", "enable", "nope"); code == 0 {
		t.Error("enabling unknown plugin should fail")
	}
	if o, c := run(t, h, "plugin", "enable", "hermes-web-search", "--profile", "coder"); c != 0 || !strings.Contains(o, "Enabled") {
		t.Errorf("plugin enable: %q code=%d", o, c)
	}
	envFile := filepath.Join(h, "runtime", "home", ".hermes", "profiles", "coder", ".env")
	b, err := os.ReadFile(envFile)
	if err != nil || !strings.Contains(string(b), "TAG_PLUGIN_HERMES_WEB_SEARCH_ENABLED=true") {
		t.Errorf(".env should enable the plugin: %q err=%v", string(b), err)
	}
	// idempotent: re-enable keeps a single line
	run(t, h, "plugin", "enable", "hermes-web-search", "--profile", "coder")
	b2, _ := os.ReadFile(envFile)
	if strings.Count(string(b2), "TAG_PLUGIN_HERMES_WEB_SEARCH_ENABLED") != 1 {
		t.Errorf("re-enable should not duplicate: %q", string(b2))
	}
	// disable removes it
	if o, c := run(t, h, "plugin", "disable", "hermes-web-search", "--profile", "coder"); c != 0 || !strings.Contains(o, "Disabled") {
		t.Errorf("plugin disable: %q code=%d", o, c)
	}
	b3, _ := os.ReadFile(envFile)
	if strings.Contains(string(b3), "TAG_PLUGIN_HERMES_WEB_SEARCH_ENABLED") {
		t.Errorf("disable should remove the line: %q", string(b3))
	}
}

func TestE2EEval(t *testing.T) {
	h := newHome(t)
	run(t, h, "mem", "stats") // create the DB
	dbPath := filepath.Join(h, "runtime", "tag.sqlite3")
	seed := "INSERT INTO eval_runs(id,suite_path,profile,suite_name,status,pass_count,fail_count,total_count,created_at) VALUES('run1','suites/qa.yaml','coder','QA Suite','completed',2,1,3,'2026-07-01T00:00:00Z');" +
		"INSERT INTO eval_cases(id,eval_run_id,case_id,input,output,passed,score,created_at) VALUES('c1','run1','case-a','in','out',1,0.95,'2026-07-01T00:00:00Z');" +
		"INSERT INTO eval_cases(id,eval_run_id,case_id,input,output,passed,score,failure_reason,created_at) VALUES('c2','run1','case-b','in','out',0,0.30,'wrong','2026-07-01T00:00:00Z');"
	if err := exec.Command("sqlite3", dbPath, seed).Run(); err != nil {
		t.Skip("sqlite3 CLI not available to seed eval rows")
	}
	if out, code := run(t, h, "eval", "list"); code != 0 || !strings.Contains(out, "run1") || !strings.Contains(out, "QA Suite") {
		t.Errorf("eval list: %q code=%d", out, code)
	}
	out, code := run(t, h, "eval", "show", "run1")
	if code != 0 || !strings.Contains(out, "2/3 passed") || !strings.Contains(out, "case-a") || !strings.Contains(out, "(wrong)") {
		t.Errorf("eval show: %q code=%d", out, code)
	}
	if _, code := run(t, h, "eval", "show", "nope"); code == 0 {
		t.Error("eval show missing should fail")
	}
}

func TestE2ESwarm(t *testing.T) {
	h := newHome(t)
	run(t, h, "mem", "stats")
	dbPath := filepath.Join(h, "runtime", "tag.sqlite3")
	seed := "INSERT INTO swarm_runs(swarm_id,goal,coordinator_profile,status,task_count,total_cost_usd,final_output,created_at) VALUES('sw1','Refactor auth','orchestrator','completed',2,0.0345,'Done.','2026-07-01T00:00:00Z');" +
		"INSERT INTO swarm_tasks(swarm_id,task_id,profile,status,cost_usd,tokens_prompt,tokens_completion) VALUES('sw1','t-1','coder','done',0.02,1000,500);" +
		"INSERT INTO swarm_tasks(swarm_id,task_id,profile,status,cost_usd,error_message) VALUES('sw1','t-2','reviewer','failed',0.0145,'timeout');"
	if err := exec.Command("sqlite3", dbPath, seed).Run(); err != nil {
		t.Skip("sqlite3 CLI not available to seed swarm rows")
	}
	if out, code := run(t, h, "swarm", "list"); code != 0 || !strings.Contains(out, "sw1") || !strings.Contains(out, "Refactor auth") {
		t.Errorf("swarm list: %q code=%d", out, code)
	}
	out, code := run(t, h, "swarm", "status", "sw1")
	if code != 0 || !strings.Contains(out, "t-1") || !strings.Contains(out, "timeout") {
		t.Errorf("swarm status: %q code=%d", out, code)
	}
	out, code = run(t, h, "swarm", "results", "sw1")
	if code != 0 || !strings.Contains(out, "Final Output") || !strings.Contains(out, "1500") {
		t.Errorf("swarm results: %q code=%d", out, code)
	}
	if _, code := run(t, h, "swarm", "status", "nope"); code == 0 {
		t.Error("swarm status missing should fail")
	}
}

func TestE2EMem2Fact(t *testing.T) {
	h := newHome(t)
	out, _ := run(t, h, "mem", "add", "capital is Alpha", "--type", "fact", "--json")
	var added struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &added); err != nil || added.ID == "" {
		t.Fatalf("could not parse mem add id from %q: %v", out, err)
	}
	upd, code := run(t, h, "mem2", "fact", "update", "--id", added.ID, "--content", "capital is Beta")
	if code != 0 || !strings.Contains(upd, "Updated fact, new id=") {
		t.Fatalf("fact update: %q code=%d", upd, code)
	}
	// only the new version is live
	if o, _ := run(t, h, "mem", "list"); !strings.Contains(o, "capital is Beta") || strings.Contains(o, "capital is Alpha") {
		t.Errorf("live memory should be the new version only: %q", o)
	}
	// history shows the superseded version
	if o, _ := run(t, h, "mem2", "fact", "history", "--id", added.ID); !strings.Contains(o, "capital is Alpha") || !strings.Contains(o, "successor_id") {
		t.Errorf("fact history: %q", o)
	}
	// validation
	if _, c := run(t, h, "mem2", "fact", "update", "--content", "x"); c == 0 {
		t.Error("update without --id should fail")
	}
	if _, c := run(t, h, "mem2", "fact", "list-at", "--at", "not-a-date"); c == 0 {
		t.Error("list-at with bad timestamp should fail")
	}
}

func TestE2ERun(t *testing.T) {
	h := newHome(t)
	// echo provider is offline-safe: the loop echoes the prompt back as final text
	out, code := run(t, h, "run", "Hello native runtime")
	if code != 0 || !strings.Contains(out, "Hello native runtime") || !strings.Contains(out, "done in 1 step") {
		t.Errorf("run: %q code=%d", out, code)
	}
	// JSON surface
	jout, _ := run(t, h, "run", "ping", "--json")
	var r struct {
		Provider  string `json:"provider"`
		Stopped   string `json:"stopped"`
		FinalText string `json:"final_text"`
	}
	if err := json.Unmarshal([]byte(jout), &r); err != nil || r.Provider != "echo" || r.Stopped != "done" || r.FinalText != "ping" {
		t.Errorf("run --json: %q err=%v parsed=%+v", jout, err, r)
	}
	// unknown provider rejected
	if _, code := run(t, h, "run", "x", "--provider", "gpt99"); code == 0 {
		t.Error("unknown provider should fail")
	}
	// the run is recorded and visible via `trace`/DB — check it persisted by re-running and listing runs is enough here
	if out, code := run(t, h, "run", "with tools", "--tools"); code != 0 || !strings.Contains(out, "with tools") {
		t.Errorf("run --tools: %q code=%d", out, code)
	}
}

func TestE2EToolIndex(t *testing.T) {
	h := newHome(t)
	if out, _ := run(t, h, "tool-index", "status"); !strings.Contains(out, "not built") {
		t.Errorf("status before index: %q", out)
	}
	if out, code := run(t, h, "tool-index", "index"); code != 0 || !strings.Contains(out, "10 tools indexed") {
		t.Fatalf("tool-index index: %q code=%d", out, code)
	}
	if out, _ := run(t, h, "tool-index", "status"); !strings.Contains(out, "10 tools") {
		t.Errorf("status after index: %q", out)
	}
	// keyword search ranks matching tools
	out, code := run(t, h, "tool-index", "search", "web search")
	if code != 0 || !strings.Contains(out, "mcp-brave-search") {
		t.Errorf("search web search: %q code=%d", out, code)
	}
	// top-k limits results
	if out, _ := run(t, h, "tool-index", "search", "database", "--top-k", "1"); !strings.Contains(out, "Top 1 tools") {
		t.Errorf("search top-k: %q", out)
	}
	// no match
	if out, _ := run(t, h, "tool-index", "search", "zzzznomatch"); !strings.Contains(out, "No tools found") {
		t.Errorf("search no match: %q", out)
	}
	// empty query rejected
	if _, code := run(t, h, "tool-index", "search", "   "); code == 0 {
		t.Error("empty query should fail")
	}
}

func TestE2ECacheStats(t *testing.T) {
	h := newHome(t)
	// tag run records token usage into the runs table
	run(t, h, "run", "first task")
	run(t, h, "run", "second longer task here")
	out, code := run(t, h, "cache", "stats")
	if code != 0 || !strings.Contains(out, "orchestrator") || !strings.Contains(out, "HitRate") {
		t.Errorf("cache stats: %q code=%d", out, code)
	}
	// bad --since rejected
	if _, code := run(t, h, "cache", "stats", "--since", "zzz"); code == 0 {
		t.Error("bad --since should fail")
	}
	// JSON surface uses Python's field names (#541): hit_rate, runs_total, total_cost_usd
	if out, _ := run(t, h, "cache", "stats", "--json"); !strings.Contains(out, `"hit_rate"`) {
		t.Errorf("cache stats --json: %q", out)
	}
}

func TestE2EOtelExport(t *testing.T) {
	h := newHome(t)
	run(t, h, "mem", "stats")
	dbPath := filepath.Join(h, "runtime", "tag.sqlite3")
	seed := "INSERT INTO spans(id,trace_id,name,profile,model_id,started_at,duration_ms,status,prompt_tokens,completion_tokens) VALUES('sp1','tr1','chat','coder','gpt-5','2026-07-01T00:00:00Z',1200,'ok',100,50);"
	if err := exec.Command("sqlite3", dbPath, seed).Run(); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	out, code := run(t, h, "otel-export")
	if code != 0 || !strings.Contains(out, "resourceSpans") || !strings.Contains(out, "gen_ai.request.model") || !strings.Contains(out, "gpt-5") {
		t.Errorf("otel-export: %q code=%d", out, code)
	}
}

func TestE2EWebhookRules(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "webhook", "rule-add", "--platform", "github", "--event", "pull_request.*", "--profile", "coder"); code != 0 || !strings.Contains(out, "Added trigger rule") {
		t.Fatalf("rule-add: %q code=%d", out, code)
	}
	// validation
	if _, code := run(t, h, "webhook", "rule-add", "--platform", "gitlab", "--event", "x", "--profile", "c"); code == 0 {
		t.Error("invalid platform should fail")
	}
	if _, code := run(t, h, "webhook", "rule-add", "--platform", "github"); code == 0 {
		t.Error("missing flags should fail")
	}
	if out, _ := run(t, h, "webhook", "rule-list"); !strings.Contains(out, "pull_request.*") || !strings.Contains(out, "coder") {
		t.Errorf("rule-list: %q", out)
	}
	if out, _ := run(t, h, "webhook", "events"); !strings.Contains(out, "No webhook events") {
		t.Errorf("events empty: %q", out)
	}
}

func TestE2EMCPConnectSubprocess(t *testing.T) {
	h := newHome(t)
	// spawn our OWN mcp-serve as the external MCP server and list its tools
	out, code := run(t, h, "mcp-connect", tagBin, "mcp-serve")
	if code != 0 || !strings.Contains(out, "3 tool(s)") || !strings.Contains(out, "tag_profiles") {
		t.Errorf("mcp-connect: %q code=%d", out, code)
	}
	// call the 'now' tool through the subprocess
	if out, code := run(t, h, "mcp-connect", "--call", "now", tagBin, "mcp-serve"); code != 0 || !strings.Contains(out, "T") {
		t.Errorf("mcp-connect --call now: %q code=%d", out, code)
	}
}

func TestE2EProfileTraversalBlocked(t *testing.T) {
	h := newHome(t)
	// plugin / mcp-registry / marketplace must reject a traversal profile name
	if _, c := run(t, h, "plugin", "enable", "hermes-web-search", "--profile", "../../../../tmp/EVIL"); c == 0 {
		t.Error("plugin enable must reject a path-traversal profile")
	}
	if _, c := run(t, h, "mcp-registry", "enable", "mcp-github", "--profile", "../../etc/EVIL"); c == 0 {
		t.Error("mcp-registry enable must reject a path-traversal profile")
	}
	if _, c := run(t, h, "marketplace", "pull", "https://example.com/x.yaml", "--name", "../../../tmp/PWNED"); c == 0 {
		t.Error("marketplace pull must reject a path-traversal --name")
	}
	// a valid profile still works
	if o, c := run(t, h, "plugin", "enable", "hermes-web-search", "--profile", "coder"); c != 0 || !strings.Contains(o, "Enabled") {
		t.Errorf("valid profile should still work: %q code=%d", o, c)
	}
}

func TestE2ECacheSinceRejectsNegative(t *testing.T) {
	h := newHome(t)
	if _, c := run(t, h, "cache", "stats", "--since", "-1d"); c == 0 {
		t.Error("negative --since must be rejected")
	}
	if _, c := run(t, h, "cache", "stats", "--since", "7d"); c != 0 {
		t.Error("valid --since should work")
	}
}

func TestE2EAlertMetricsReflectData(t *testing.T) {
	h := newHome(t)
	run(t, h, "mem", "stats")
	dbPath := filepath.Join(h, "runtime", "tag.sqlite3")
	seed := "INSERT INTO eval_runs(id,suite_path,profile,suite_name,status,pass_count,fail_count,total_count,created_at) VALUES('r','s','orchestrator','S','completed',8,2,10,'2026-07-01T00:00:00Z');"
	for i := 0; i < 10; i++ {
		pass := 0
		if i < 8 {
			pass = 1
		}
		seed += fmt.Sprintf("INSERT INTO eval_cases(id,eval_run_id,case_id,input,output,passed,score,created_at) VALUES('c%d','r','k%d','in','out',%d,0.5,'2026-07-01T00:00:00Z');", i, i, pass)
	}
	if err := exec.Command("sqlite3", dbPath, seed).Run(); err != nil {
		t.Skip("sqlite3 CLI unavailable")
	}
	// eval_pass_rate is 0.8 → an "lt 0.5" rule must NOT fire (was firing before the fix)
	run(t, h, "alert", "create", "pr", "eval_pass_rate", "lt", "0.5", "--severity", "warning")
	if out, _ := run(t, h, "alert", "check"); !strings.Contains(out, "No alerts firing") {
		t.Errorf("eval_pass_rate=0.8 must not trip an lt-0.5 rule: %q", out)
	}
}

func TestE2EAnnotateExportIncludesLabelSchema(t *testing.T) {
	h := newHome(t)
	run(t, h, "annotate", "add", "content here", "--question", "ok?")
	out, _ := run(t, h, "annotate", "next")
	tid := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "["), "]")
	tid = strings.Split(tid, "]")[0]
	run(t, h, "annotate", "label", tid, "yes")
	if o, _ := run(t, h, "annotate", "export", "--format", "jsonl"); !strings.Contains(o, "label_schema") {
		t.Errorf("jsonl export must include label_schema: %q", o)
	}
	if o, _ := run(t, h, "annotate", "export", "--format", "csv"); !strings.Contains(o, "label_schema") {
		t.Errorf("csv export header must include label_schema: %q", o)
	}
}

func TestE2EPersonaBuiltinsSeeded(t *testing.T) {
	h := newHome(t)
	// builtins must be listed (feature was entirely dead before)
	if out, code := run(t, h, "persona", "list"); code != 0 || !strings.Contains(out, "terse-engineer") || !strings.Contains(out, "security-focused") {
		t.Fatalf("persona list should show builtins: %q code=%d", out, code)
	}
	// apply works; re-apply upserts (no duplicate row)
	if o, c := run(t, h, "persona", "apply", "terse-engineer", "--profile", "orchestrator"); c != 0 || !strings.Contains(o, "Applied persona") {
		t.Errorf("persona apply: %q code=%d", o, c)
	}
	run(t, h, "persona", "apply", "terse-engineer", "--profile", "orchestrator")
	out, _ := run(t, h, "persona", "stack", "--profile", "orchestrator")
	if strings.Count(out, "terse-engineer") != 1 {
		t.Errorf("re-apply must not duplicate the stack row: %q", out)
	}
	if _, c := run(t, h, "persona", "apply", "nonexistent", "--profile", "orchestrator"); c == 0 {
		t.Error("applying an unknown persona should fail")
	}
}

func TestE2EBudgetPeriodValidation(t *testing.T) {
	h := newHome(t)
	if _, c := run(t, h, "budget", "set", "--profile", "orchestrator", "--max-tokens", "100", "--period", "garbage"); c == 0 {
		t.Error("invalid --period must be rejected")
	}
	if _, c := run(t, h, "budget", "set", "--profile", "orchestrator", "--max-tokens", "100", "--period", "weekly"); c != 0 {
		t.Error("valid --period should work")
	}
	// get --json carries id + enabled
	out, _ := run(t, h, "budget", "get", "--profile", "orchestrator", "--json")
	if !strings.Contains(out, `"id"`) || !strings.Contains(out, `"enabled"`) {
		t.Errorf("budget get --json must include id + enabled: %q", out)
	}
}

func TestE2EDoctorJSON(t *testing.T) {
	h := newHome(t)
	out, _ := run(t, h, "doctor", "--json")
	// checks must be non-empty objects (unexported-field bug)
	if !strings.Contains(out, `"name"`) || !strings.Contains(out, `"tag_home"`) {
		t.Errorf("doctor --json must serialize check fields: %q", out)
	}
}

// TestE2EEmptyJSONListsAreArrays covers #559: `--json` on an empty result set
// must emit `[]`, never `null`. Before the fix these commands marshaled a nil
// slice → `null`, breaking `--json` consumers that iterate the result.
func TestE2EEmptyJSONListsAreArrays(t *testing.T) {
	h := newHome(t)
	run(t, h, "mem", "stats") // touch DB so schema/tables exist
	cases := [][]string{
		{"swarm", "list", "--json"},
		{"trace", "list", "--json"},
		{"webhook", "rule-list", "--json"},
		{"notify", "list", "--json"},
		{"memory-journal", "list", "--json"},
	}
	for _, c := range cases {
		out, code := run(t, h, c...)
		if code != 0 {
			t.Errorf("%v exit %d: %q", c, code, out)
		}
		trimmed := strings.TrimSpace(out)
		if trimmed != "[]" {
			t.Errorf("%v empty --json must be [] not %q", c, trimmed)
		}
	}
}

func TestE2EDagValidation(t *testing.T) {
	h := newHome(t)
	if _, c := run(t, h, "dag", "save", "d", "--steps", `[{"task":""}]`); c == 0 {
		t.Error("empty task should be rejected")
	}
	if _, c := run(t, h, "dag", "save", "d", "--steps", `[{"task":"a","xdeps":[0]}]`); c == 0 {
		t.Error("unknown key should be rejected")
	}
	// Dependency aliases must be rejected in favor of the canonical `depends_on`
	// (parity with Python dag.py:validate_dag_spec — see issue #520).
	if o, c := run(t, h, "dag", "save", "d", "--steps", `[{"task":"a","deps":[]}]`); c == 0 {
		t.Errorf("dependency alias 'deps' should be rejected, use depends_on: %q", o)
	}
	if _, c := run(t, h, "dag", "save", "", "--steps", `[{"task":"a"}]`); c == 0 {
		t.Error("empty DAG name should be rejected")
	}
	// The canonical `depends_on` key must be accepted.
	if o, c := run(t, h, "dag", "save", "good", "--steps", `[{"task":"a"},{"task":"b","depends_on":["a"]}]`); c != 0 || !strings.Contains(o, "saved") {
		t.Errorf("valid DAG with depends_on should save: %q code=%d", o, c)
	}
}
