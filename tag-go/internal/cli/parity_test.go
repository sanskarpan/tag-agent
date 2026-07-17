package cli_test

import (
	"strings"
	"testing"
)

// TestParityEmptyJSONLists verifies empty --json list outputs emit "[]" (not
// "null"), matching Python's json.dumps of an empty list.
func TestParityEmptyJSONLists(t *testing.T) {
	h := newHome(t)
	cases := [][]string{
		{"mem", "search", "nothing-here", "--json"},
		{"alert", "list", "--json"},
		{"alert", "check", "--json"},
		{"mem2", "episode", "list", "--json"},
		{"mem2", "fact", "list-at", "--json"},
		{"lsp", "status", "--json"},
	}
	for _, args := range cases {
		out, code := run(t, h, args...)
		if code != 0 {
			t.Errorf("%v: exit=%d out=%q", args, code, out)
		}
		if !strings.Contains(out, "[]") || strings.Contains(out, "null") {
			t.Errorf("%v: expected [] not null, got %q", args, out)
		}
	}
}

// TestParityNoArgsValidators verifies flag-only commands reject stray
// positionals (Python argparse errors; Go must too via cobra.NoArgs).
func TestParityNoArgsValidators(t *testing.T) {
	h := newHome(t)
	cases := [][]string{
		{"workspace", "index", "/tmp"},
		{"queue", "list", "extra"},
		{"budget", "check", "foo"},
		{"workspace", "map", "bar"},
	}
	for _, args := range cases {
		if _, code := run(t, h, args...); code == 0 {
			t.Errorf("%v: expected nonzero exit for stray positional", args)
		}
	}
}

// TestParityQueueDag exercises the new queue/dag subcommands offline.
func TestParityQueueDag(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "queue", "clear"); code != 0 || !strings.Contains(out, "cleared") {
		t.Fatalf("queue clear: %q %d", out, code)
	}
	if out, code := run(t, h, "queue", "result", "missing"); code == 0 || !strings.Contains(out, "not found") {
		t.Fatalf("queue result missing: %q %d", out, code)
	}
	run(t, h, "dag", "save", "pipe", "--steps", `[{"task":"a"},{"task":"b","depends_on":[0]}]`)
	if out, code := run(t, h, "dag", "run", "pipe"); code != 0 || !strings.Contains(out, "submitted: 2 jobs") {
		t.Fatalf("dag run: %q %d", out, code)
	}
	if out, code := run(t, h, "dag", "show"); code != 0 || !strings.Contains(out, "Dependency Graph") {
		t.Fatalf("dag show: %q %d", out, code)
	}
}

// TestParityCronLifecycle exercises cron enable/disable/run.
func TestParityCronLifecycle(t *testing.T) {
	h := newHome(t)
	run(t, h, "cron", "add", "task", "--name", "n", "--schedule", "0 2 * * *")
	out, _ := run(t, h, "cron", "list")
	fields := strings.Fields(out)
	if len(fields) < 2 {
		t.Fatalf("cron list unexpected: %q", out)
	}
	id := fields[1]
	if out, code := run(t, h, "cron", "disable", id); code != 0 || !strings.Contains(out, "disabled") {
		t.Errorf("cron disable: %q %d", out, code)
	}
	if out, code := run(t, h, "cron", "enable", id); code != 0 || !strings.Contains(out, "enabled") {
		t.Errorf("cron enable: %q %d", out, code)
	}
	if out, code := run(t, h, "cron", "run", id); code != 0 || !strings.Contains(out, "triggered") {
		t.Errorf("cron run: %q %d", out, code)
	}
}

// TestParityPersona exercises persona show/install/delete/preview.
func TestParityPersona(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "persona", "show", "terse-engineer"); code != 0 || !strings.Contains(out, "Style Prompt") {
		t.Fatalf("persona show: %q %d", out, code)
	}
	if out, code := run(t, h, "persona", "delete", "terse-engineer"); code == 0 || !strings.Contains(out, "built-in") {
		t.Fatalf("persona delete builtin should fail: %q %d", out, code)
	}
	if out, code := run(t, h, "persona", "show", "does-not-exist"); code == 0 {
		t.Fatalf("persona show missing should fail: %q %d", out, code)
	}
}

// TestParityBudgetCheck exercises budget check unlimited and configured paths.
func TestParityBudgetCheck(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "budget", "check"); code != 0 || !strings.Contains(out, "unlimited") {
		t.Fatalf("budget check unlimited: %q %d", out, code)
	}
	run(t, h, "budget", "set", "--max-tokens", "1000", "--period", "daily")
	if out, code := run(t, h, "budget", "check"); code != 0 || !strings.Contains(out, "1,000") {
		t.Fatalf("budget check configured: %q %d", out, code)
	}
}

// TestParityWorkspaceMapClear exercises workspace map and clear.
func TestParityWorkspaceMapClear(t *testing.T) {
	h := newHome(t)
	run(t, h, "workspace", "index", "--path", ".")
	if out, code := run(t, h, "workspace", "map"); code != 0 {
		t.Fatalf("workspace map: %q %d", out, code)
	}
	if out, code := run(t, h, "workspace", "clear"); code != 0 || !strings.Contains(out, "cleared") {
		t.Fatalf("workspace clear: %q %d", out, code)
	}
	if out, _ := run(t, h, "workspace", "map"); !strings.Contains(out, "not indexed") {
		t.Errorf("workspace map after clear: %q", out)
	}
}

// TestParityTemplateFetchSSRF verifies the fetch guard rejects private hosts.
func TestParityTemplateFetchSSRF(t *testing.T) {
	h := newHome(t)
	cases := []string{"http://127.0.0.1/x", "file:///etc/passwd", "http://169.254.169.254/"}
	for _, u := range cases {
		if out, code := run(t, h, "template", "fetch", u); code == 0 {
			t.Errorf("template fetch %s should be refused: %q", u, out)
		}
	}
}

// TestParityRouteFallbackRemove exercises route-fallback remove.
func TestParityRouteFallbackRemove(t *testing.T) {
	h := newHome(t)
	run(t, h, "route-fallback", "add", "--primary", "m1", "--fallback", "m2")
	out, _ := run(t, h, "route-fallback", "list")
	id := strings.Fields(out)[0]
	if o, code := run(t, h, "route-fallback", "remove", id); code != 0 || !strings.Contains(o, "removed") {
		t.Errorf("route-fallback remove: %q %d", o, code)
	}
	if _, code := run(t, h, "route-fallback", "remove", "nope"); code == 0 {
		t.Errorf("route-fallback remove missing should fail")
	}
}

// TestParityMemoryJournalClear exercises memory-journal clear --confirm gate.
func TestParityMemoryJournalClear(t *testing.T) {
	h := newHome(t)
	run(t, h, "memory-journal", "save", "k", "v")
	if out, code := run(t, h, "memory-journal", "clear"); code == 0 || !strings.Contains(out, "confirm") {
		t.Fatalf("clear without confirm should fail: %q %d", out, code)
	}
	if out, code := run(t, h, "memory-journal", "clear", "--confirm"); code != 0 || !strings.Contains(out, "cleared") {
		t.Fatalf("clear with confirm: %q %d", out, code)
	}
}

// TestParityCacheAndTrace exercises cache trend/tips and trace snapshot.
func TestParityCacheAndTrace(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "cache", "trend", "--since", "7d", "--buckets", "2"); code != 0 || !strings.Contains(out, "Cache hit rate") {
		t.Fatalf("cache trend: %q %d", out, code)
	}
	if _, code := run(t, h, "cache", "tips"); code == 0 {
		t.Fatalf("cache tips without --profile should fail")
	}
	if out, code := run(t, h, "cache", "tips", "--profile", "orchestrator"); code != 0 || !strings.Contains(out, "Cache tips") {
		t.Fatalf("cache tips: %q %d", out, code)
	}
	// snapshot of a nonexistent trace is a no-op success
	if out, code := run(t, h, "trace", "snapshot", "no-such"); code != 0 || !strings.Contains(out, "Snapshot captured") {
		t.Fatalf("trace snapshot: %q %d", out, code)
	}
}

// TestParityPluginInstall verifies unknown vs known plugin install behavior.
func TestParityPluginInstall(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "plugin", "install", "definitely-not-a-plugin"); code == 0 {
		t.Errorf("unknown plugin install should fail")
	}
}

// TestParityBenchmarkBugFixes covers issues #528-#531 found by the Python↔Go
// benchmark: graph JSON shape, mem stats human default, route --json error
// path, and the usage-error exit code.
func TestParityBenchmarkBugFixes(t *testing.T) {
	h := newHome(t)

	// #528: graph show --json must have non-null entities AND a relations array.
	out, code := run(t, h, "--json", "graph", "show")
	if code != 0 {
		t.Fatalf("graph show --json: %q %d", out, code)
	}
	if strings.Contains(out, `"entities": null`) || strings.Contains(out, `"entities":null`) {
		t.Errorf("#528: entities must be [] not null: %s", out)
	}
	if !strings.Contains(out, `"relations"`) {
		t.Errorf("#528: graph show --json must include relations: %s", out)
	}

	// #529: mem stats default is human, not JSON; --json is JSON.
	run(t, h, "mem", "add", "python rocks", "--type", "fact")
	if o, _ := run(t, h, "mem", "stats"); strings.HasPrefix(strings.TrimSpace(o), "{") {
		t.Errorf("#529: mem stats default should be human, got JSON: %s", o)
	}
	if o, _ := run(t, h, "--json", "mem", "stats"); !strings.HasPrefix(strings.TrimSpace(o), "{") {
		t.Errorf("#529: mem stats --json should be JSON: %s", o)
	}

	// #530: route --json on an error emits a JSON {"error":...} on stdout.
	o, code := run(t, h, "--json", "route", "bogus-task-type")
	if code == 0 {
		t.Errorf("#530: bad route should be non-zero")
	}
	if !strings.Contains(o, `"error"`) {
		t.Errorf("#530: route --json error path must emit JSON: %s", o)
	}

	// #531: usage errors exit 2 (argparse parity); valid commands don't.
	if _, code := run(t, h, "nonexistent-cmd"); code != 2 {
		t.Errorf("#531: unknown command should exit 2, got %d", code)
	}
	if _, code := run(t, h, "mem", "add"); code != 2 {
		t.Errorf("#531: missing required arg should exit 2, got %d", code)
	}
	if _, code := run(t, h, "mem", "list"); code != 0 {
		t.Errorf("#531: valid command should exit 0, got %d", code)
	}
}
