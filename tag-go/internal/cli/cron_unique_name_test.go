package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestE2ECronAddRejectsDuplicateName pins the uniqueness contract Python has
// enforced since PRD-022 (`src/tag/cmd/ci_loop.py`): a cron job NAME identifies
// a schedule, so adding a second job under a name already in use must fail.
// Go accepted the duplicate and silently created a second row, leaving `cron
// enable <name>`-style workflows and the daemon's per-name bookkeeping
// ambiguous.
func TestE2ECronAddRejectsDuplicateName(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "cron", "add", "t1", "--name", "dup", "--schedule", "* * * * *"); code != 0 {
		t.Fatalf("first cron add: exit %d: %s", code, out)
	}
	out, code := run(t, h, "cron", "add", "t1", "--name", "dup", "--schedule", "* * * * *")
	if code == 0 {
		t.Fatalf("duplicate cron add succeeded (exit 0): %s", out)
	}
	want := "A cron job named 'dup' already exists (names must be unique)"
	if !strings.Contains(out, want) {
		t.Errorf("duplicate cron add error = %q, want it to contain %q", out, want)
	}
	// And it must not have persisted a second row.
	listOut, listCode := run(t, h, "--json", "cron", "list")
	if listCode != 0 {
		t.Fatalf("cron list: exit %d: %s", listCode, listOut)
	}
	var jobs []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(listOut)), &jobs); err != nil {
		t.Fatalf("cron list --json is not valid JSON (%v): %s", err, listOut)
	}
	if len(jobs) != 1 {
		t.Errorf("cron list after rejected duplicate = %d jobs, want 1: %s", len(jobs), listOut)
	}
}

// TestE2ECronAddDuplicateNameJSON: the rejection must also be machine-readable
// under --json rather than bare text on stderr.
func TestE2ECronAddDuplicateNameJSON(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "cron", "add", "t1", "--name", "dup", "--schedule", "* * * * *"); code != 0 {
		t.Fatalf("first cron add: exit %d: %s", code, out)
	}
	stdout, _, code := runIO(t, h, "--json", "cron", "add", "t1", "--name", "dup", "--schedule", "* * * * *")
	if code == 0 {
		t.Fatalf("duplicate cron add --json succeeded (exit 0): %s", stdout)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &obj); err != nil {
		t.Fatalf("duplicate cron add --json stdout is not JSON (%v): %q", err, stdout)
	}
	if _, ok := obj["error"]; !ok {
		t.Errorf("duplicate cron add --json = %v, want an {\"error\": ...} object", obj)
	}
}

// TestE2ECronDuplicateNamesInExistingHomeStillOpens guards the migration
// hazard: a TAG_HOME written by an older build may already hold duplicate
// names. Whatever enforcement we add must not make those homes unopenable.
func TestE2ECronDuplicateNamesInExistingHome(t *testing.T) {
	h := newHome(t)
	// Fabricate the pre-fix state directly in SQLite, bypassing the CLI check.
	seedDuplicateCronRows(t, h)
	// Every cron path must still work on this home.
	if out, code := run(t, h, "--json", "cron", "list"); code != 0 {
		t.Fatalf("cron list on a home with duplicate names: exit %d: %s", code, out)
	}
	if out, code := run(t, h, "mem", "add", "still works"); code != 0 {
		t.Fatalf("unrelated command on a home with duplicate names: exit %d: %s", code, out)
	}
}
