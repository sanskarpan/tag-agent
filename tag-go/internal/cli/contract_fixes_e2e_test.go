package cli_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestTraceListIsTimeOrdered: `trace list` claims "recent traces" and must list
// the most-recently-active trace first, regardless of trace_id string ordering.
// RED against pre-fix code, which ordered by trace_id DESC so a lexically larger
// but older id sorted above a newer one (#749).
func TestTraceListIsTimeOrdered(t *testing.T) {
	h := newHome(t)
	db, err := sql.Open("sqlite", filepath.Join(h, "runtime", "tag.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS spans (
		id TEXT PRIMARY KEY, trace_id TEXT NOT NULL, parent_id TEXT, name TEXT NOT NULL,
		profile TEXT, model_id TEXT, started_at TEXT NOT NULL, finished_at TEXT,
		duration_ms INTEGER, status TEXT NOT NULL DEFAULT 'ok',
		prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
		attributes TEXT NOT NULL DEFAULT '{}', error_msg TEXT, kind TEXT, cost_usd REAL)`); err != nil {
		t.Fatal(err)
	}
	// The NEWER trace has the lexically SMALLER id — so a trace_id sort would
	// wrongly put the older one first.
	db.Exec(`INSERT INTO spans(id,trace_id,name,started_at) VALUES('s1','zzz-old','x','2026-08-21T10:00:00.000000Z')`)
	db.Exec(`INSERT INTO spans(id,trace_id,name,started_at) VALUES('s2','aaa-new','y','2026-08-21T12:00:00.000000Z')`)

	out, code := run(t, h, "trace", "list")
	if code != 0 {
		t.Fatalf("trace list exit %d: %s", code, out)
	}
	iNew, iOld := strings.Index(out, "aaa-new"), strings.Index(out, "zzz-old")
	if iNew < 0 || iOld < 0 || iNew > iOld {
		t.Errorf("newest trace must be listed first; got order:\n%s", out)
	}
}

// TestRunsShowNotFoundExitsNonZero: `runs show <bad> --json` must exit non-zero
// with a parseable {"error":...} on stdout, matching the eval/loop/swarm/queue
// detail family. RED against pre-fix code, which exited 0 (#739).
func TestRunsShowNotFoundExitsNonZero(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "--json", "runs", "show", "no-such-run")
	if code == 0 {
		t.Fatalf("a not-found run must exit non-zero, got 0: %s", out)
	}
	// The helper combines stdout+stderr; the JSON error object is emitted on
	// stdout (the "error:" line goes to stderr). Assert one line parses as a
	// JSON error object.
	found := false
	for _, ln := range strings.Split(out, "\n") {
		var obj map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(ln)), &obj) == nil && obj["error"] != nil {
			found = true
		}
	}
	if !found {
		t.Errorf("--json not-found must print a {\"error\":...} object, got: %q", out)
	}
}

// TestRunsListNegativeLimitRejected: `runs list --limit -1` must be a usage
// error (exit 2), not a silent unbounded dump (SQLite LIMIT -1). RED against
// pre-fix code, which returned exit 0 (#750).
func TestRunsListNegativeLimitRejected(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "runs", "list", "--limit", "-1"); code != 2 {
		t.Errorf("runs list --limit -1 should exit 2, got %d", code)
	}
}
