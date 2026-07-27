package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// The tests below pin the --json contract for the mem / mem2 / cron surfaces.
// Two rules are being enforced:
//
//  1. Under --json, EVERY terminating path prints JSON on stdout — success
//     objects on success, a {"error": ...} object on failure. Bare prose under
//     --json is unparseable, and a success-shaped object on a failure path is
//     worse than nothing because a consumer that ignores the exit code acts on
//     it.
//  2. Field names match the Python implementation, which is the reference for
//     this CLI (src/tag/cmd/memory.py, src/tag/cmd/ci_loop.py).

// mustJSONObject fails unless s is exactly one JSON object.
func mustJSONObject(t *testing.T, what, s string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &obj); err != nil {
		t.Fatalf("%s: stdout is not a JSON object (%v): %q", what, err, s)
	}
	return obj
}

func mustJSONArray(t *testing.T, what, s string) []map[string]any {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &arr); err != nil {
		t.Fatalf("%s: stdout is not a JSON array (%v): %q", what, err, s)
	}
	return arr
}

func wantKeys(t *testing.T, what string, obj map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			t.Errorf("%s --json is missing %q: %v", what, k, obj)
		}
	}
}

// wantJSONError asserts a failing command emitted a JSON error object on stdout
// and exited non-zero.
func wantJSONError(t *testing.T, what, stdout string, code int) {
	t.Helper()
	if code == 0 {
		t.Errorf("%s: expected a non-zero exit, got 0 (stdout %q)", what, stdout)
	}
	obj := mustJSONObject(t, what, stdout)
	if _, ok := obj["error"]; !ok {
		t.Errorf("%s --json on a failure path = %v, want an {\"error\": ...} object", what, obj)
	}
}

// ---------------------------------------------------------------------------
// mem
// ---------------------------------------------------------------------------

// Python: print(json.dumps({"id": mem_id, "profile": profile}))
func TestE2EMemAddJSONIncludesProfile(t *testing.T) {
	h := newHome(t)
	stdout, _, code := runIO(t, h, "--json", "mem", "add", "the sky is blue")
	if code != 0 {
		t.Fatalf("mem add --json: exit %d: %s", code, stdout)
	}
	obj := mustJSONObject(t, "mem add", stdout)
	wantKeys(t, "mem add", obj, "id", "profile")
}

// Python's `mem search` takes --type (memory_type) and filters on it; Go
// rejected the flag outright with "unknown flag: --type", exit 2.
func TestE2EMemSearchHasTypeFilter(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "mem", "add", "postgres tuning notes", "--type", "fact"); code != 0 {
		t.Fatalf("mem add fact: exit %d: %s", code, out)
	}
	if out, code := run(t, h, "mem", "add", "postgres tuning gotcha", "--type", "gotcha"); code != 0 {
		t.Fatalf("mem add gotcha: exit %d: %s", code, out)
	}
	stdout, stderr, code := runIO(t, h, "--json", "mem", "search", "postgres", "--type", "gotcha")
	if code != 0 {
		t.Fatalf("mem search --type: exit %d: %s%s", code, stdout, stderr)
	}
	arr := mustJSONArray(t, "mem search --type", stdout)
	if len(arr) != 1 {
		t.Fatalf("mem search --type gotcha = %d hits, want 1: %s", len(arr), stdout)
	}
	if arr[0]["memory_type"] != "gotcha" {
		t.Errorf("filtered hit has memory_type %v, want \"gotcha\"", arr[0]["memory_type"])
	}
}

// A miss used to print the SUCCESS-shaped {"deleted":false} on stdout while
// exiting 1 — a consumer reading stdout sees a well-formed result object for an
// operation that failed.
func TestE2EMemForgetMissEmitsJSONError(t *testing.T) {
	h := newHome(t)
	stdout, _, code := runIO(t, h, "--json", "mem", "forget", "nosuchid")
	wantJSONError(t, "mem forget (miss)", stdout, code)
	if strings.Contains(stdout, `"deleted"`) {
		t.Errorf("mem forget miss still emits a success-shaped payload: %q", stdout)
	}
}

func TestE2EMemForgetHitEmitsJSON(t *testing.T) {
	h := newHome(t)
	stdout, _, code := runIO(t, h, "--json", "mem", "add", "forget me")
	if code != 0 {
		t.Fatalf("mem add: exit %d: %s", code, stdout)
	}
	id, _ := mustJSONObject(t, "mem add", stdout)["id"].(string)
	stdout, _, code = runIO(t, h, "--json", "mem", "forget", id)
	if code != 0 {
		t.Fatalf("mem forget --json: exit %d: %s", code, stdout)
	}
	obj := mustJSONObject(t, "mem forget", stdout)
	wantKeys(t, "mem forget", obj, "deleted", "id", "profile")
	if obj["deleted"] != true {
		t.Errorf("mem forget on a hit = %v, want deleted true", obj)
	}
}

// `queue list --limit -1 --json` already emitted {"error": ...}; the mem
// commands printed bare prose on stderr for the same class of input.
func TestE2EMemNegativeLimitEmitsJSONError(t *testing.T) {
	h := newHome(t)
	for _, sub := range []string{"list", "search"} {
		args := []string{"--json", "mem", sub, "--limit", "-1"}
		if sub == "search" {
			args = []string{"--json", "mem", "search", "q", "--limit", "-1"}
		}
		stdout, _, code := runIO(t, h, args...)
		wantJSONError(t, "mem "+sub+" --limit -1", stdout, code)
	}
	// ...and stays consistent with queue's wording.
	stdout, _, code := runIO(t, h, "--json", "queue", "list", "--limit", "-1")
	wantJSONError(t, "queue list --limit -1", stdout, code)
	memOut, _, _ := runIO(t, h, "--json", "mem", "list", "--limit", "-1")
	if mustJSONObject(t, "mem list", memOut)["error"] != mustJSONObject(t, "queue list", stdout)["error"] {
		t.Errorf("mem list and queue list disagree on the negative-limit message:\n  mem:   %s\n  queue: %s", memOut, stdout)
	}
}

// ---------------------------------------------------------------------------
// cron
// ---------------------------------------------------------------------------

// Python: print(json.dumps({"id": job_id, "name": name, "schedule": schedule}))
func TestE2ECronAddJSONIncludesSchedule(t *testing.T) {
	h := newHome(t)
	stdout, _, code := runIO(t, h, "--json", "cron", "add", "t", "--name", "n1", "--schedule", "* * * * *")
	if code != 0 {
		t.Fatalf("cron add --json: exit %d: %s", code, stdout)
	}
	obj := mustJSONObject(t, "cron add", stdout)
	wantKeys(t, "cron add", obj, "id", "name", "schedule")
}

// Python selects id, name, schedule, profile, enabled, last_run_at, run_count.
func TestE2ECronListJSONFieldSet(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "cron", "add", "t", "--name", "n1", "--schedule", "* * * * *"); code != 0 {
		t.Fatalf("cron add: exit %d: %s", code, out)
	}
	stdout, _, code := runIO(t, h, "--json", "cron", "list")
	if code != 0 {
		t.Fatalf("cron list --json: exit %d: %s", code, stdout)
	}
	arr := mustJSONArray(t, "cron list", stdout)
	if len(arr) != 1 {
		t.Fatalf("cron list = %d rows, want 1: %s", len(arr), stdout)
	}
	wantKeys(t, "cron list", arr[0], "id", "name", "schedule", "profile", "enabled", "run_count", "last_run_at")
}

// Python: print(f"removed: {job_id}") — the id was dropped by Go.
func TestE2ECronRemoveEchoesID(t *testing.T) {
	h := newHome(t)
	stdout, _, code := runIO(t, h, "--json", "cron", "add", "t", "--name", "n1", "--schedule", "* * * * *")
	if code != 0 {
		t.Fatalf("cron add: exit %d: %s", code, stdout)
	}
	id, _ := mustJSONObject(t, "cron add", stdout)["id"].(string)

	textOut, textCode := run(t, h, "cron", "remove", id)
	if textCode != 0 {
		t.Fatalf("cron remove: exit %d: %s", textCode, textOut)
	}
	if !strings.Contains(textOut, "removed: "+id) {
		t.Errorf("cron remove = %q, want %q", strings.TrimSpace(textOut), "removed: "+id)
	}
}

func TestE2ECronRemoveJSON(t *testing.T) {
	h := newHome(t)
	stdout, _, _ := runIO(t, h, "--json", "cron", "add", "t", "--name", "n1", "--schedule", "* * * * *")
	id, _ := mustJSONObject(t, "cron add", stdout)["id"].(string)
	stdout, _, code := runIO(t, h, "--json", "cron", "remove", id)
	if code != 0 {
		t.Fatalf("cron remove --json: exit %d: %s", code, stdout)
	}
	obj := mustJSONObject(t, "cron remove", stdout)
	if obj["removed"] != id {
		t.Errorf("cron remove --json = %v, want removed=%q", obj, id)
	}
}

// Every cron subcommand must produce JSON under --json, on both the success and
// the failure path.
func TestE2ECronJSONOnAllPaths(t *testing.T) {
	h := newHome(t)
	stdout, _, _ := runIO(t, h, "--json", "cron", "add", "t", "--name", "n1", "--schedule", "* * * * *")
	id, _ := mustJSONObject(t, "cron add", stdout)["id"].(string)

	t.Run("next", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "cron", "next", "* * * * *")
		if code != 0 {
			t.Fatalf("cron next: exit %d: %s", code, out)
		}
		obj := mustJSONObject(t, "cron next", out)
		wantKeys(t, "cron next", obj, "schedule", "next")
	})
	t.Run("next-invalid", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "cron", "next", "not a cron expr")
		wantJSONError(t, "cron next (invalid)", out, code)
	})
	t.Run("disable", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "cron", "disable", id)
		if code != 0 {
			t.Fatalf("cron disable: exit %d: %s", code, out)
		}
		obj := mustJSONObject(t, "cron disable", out)
		if obj["id"] != id || obj["enabled"] != false {
			t.Errorf("cron disable --json = %v, want id=%q enabled=false", obj, id)
		}
	})
	t.Run("enable", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "cron", "enable", id)
		if code != 0 {
			t.Fatalf("cron enable: exit %d: %s", code, out)
		}
		obj := mustJSONObject(t, "cron enable", out)
		if obj["id"] != id || obj["enabled"] != true {
			t.Errorf("cron enable --json = %v, want id=%q enabled=true", obj, id)
		}
	})
	t.Run("run", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "cron", "run", id)
		if code != 0 {
			t.Fatalf("cron run: exit %d: %s", code, out)
		}
		obj := mustJSONObject(t, "cron run", out)
		wantKeys(t, "cron run", obj, "cron_job_id", "queue_job_id", "status")
	})
	for _, sub := range []string{"enable", "disable", "run", "remove"} {
		t.Run(sub+"-missing", func(t *testing.T) {
			out, _, code := runIO(t, h, "--json", "cron", sub, "nosuchjob")
			wantJSONError(t, "cron "+sub+" (missing)", out, code)
			if !strings.Contains(out, "Job 'nosuchjob' not found") {
				t.Errorf("cron %s (missing) = %q, want Python's \"Job 'nosuchjob' not found\"", sub, strings.TrimSpace(out))
			}
		})
	}
	t.Run("add-invalid-schedule", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "cron", "add", "t", "--name", "z", "--schedule", "nope")
		wantJSONError(t, "cron add (invalid schedule)", out, code)
	})
	t.Run("add-missing-flags", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "cron", "add", "t")
		wantJSONError(t, "cron add (missing flags)", out, code)
	})
}

// ---------------------------------------------------------------------------
// mem2
// ---------------------------------------------------------------------------

func TestE2EMem2JSONOnAllPaths(t *testing.T) {
	h := newHome(t)

	t.Run("episode-start", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "mem2", "episode", "start", "--summary", "s1")
		if code != 0 {
			t.Fatalf("mem2 episode start: exit %d: %s", code, out)
		}
		obj := mustJSONObject(t, "mem2 episode start", out)
		wantKeys(t, "mem2 episode start", obj, "episode_id", "profile")
	})
	t.Run("episode-end", func(t *testing.T) {
		start, _, _ := runIO(t, h, "--json", "mem2", "episode", "start", "--summary", "s2")
		id, _ := mustJSONObject(t, "mem2 episode start", start)["episode_id"].(string)
		out, _, code := runIO(t, h, "--json", "mem2", "episode", "end", "--id", id)
		if code != 0 {
			t.Fatalf("mem2 episode end: exit %d: %s", code, out)
		}
		obj := mustJSONObject(t, "mem2 episode end", out)
		wantKeys(t, "mem2 episode end", obj, "episode_id", "status")
	})
	t.Run("episode-end-missing", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "mem2", "episode", "end", "--id", "nosuch")
		wantJSONError(t, "mem2 episode end (missing)", out, code)
	})
	t.Run("fact-update", func(t *testing.T) {
		add, _, _ := runIO(t, h, "--json", "mem", "add", "the capital is Bonn")
		id, _ := mustJSONObject(t, "mem add", add)["id"].(string)
		out, _, code := runIO(t, h, "--json", "mem2", "fact", "update", "--id", id, "--content", "the capital is Berlin")
		if code != 0 {
			t.Fatalf("mem2 fact update: exit %d: %s", code, out)
		}
		obj := mustJSONObject(t, "mem2 fact update", out)
		wantKeys(t, "mem2 fact update", obj, "id", "previous_id")
		if obj["previous_id"] != id {
			t.Errorf("mem2 fact update previous_id = %v, want %q", obj["previous_id"], id)
		}
	})
	t.Run("fact-update-missing-id", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "mem2", "fact", "update", "--content", "x")
		wantJSONError(t, "mem2 fact update (no --id)", out, code)
	})
	t.Run("gc-dry-run", func(t *testing.T) {
		out, _, code := runIO(t, h, "--json", "mem2", "gc", "--dry-run")
		if code != 0 {
			t.Fatalf("mem2 gc --dry-run: exit %d: %s", code, out)
		}
		obj := mustJSONObject(t, "mem2 gc --dry-run", out)
		wantKeys(t, "mem2 gc --dry-run", obj, "profile", "dry_run")
		if obj["dry_run"] != true {
			t.Errorf("mem2 gc --dry-run = %v, want dry_run true", obj)
		}
		// A dry run must not report work it did not do.
		for _, k := range []string{"evicted_count", "merged_count", "promoted_count"} {
			if v, ok := obj[k]; ok && v != float64(0) {
				t.Errorf("mem2 gc --dry-run reports %s=%v; a preview changes nothing", k, v)
			}
		}
	})
}
