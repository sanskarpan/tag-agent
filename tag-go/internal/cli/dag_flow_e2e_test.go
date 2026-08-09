package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// PRD-112: conditional edges + state reducers on the existing `dag` engine.
//
// Surface choice: these extend `dag save/run/show/list` (plus a new read-only
// `dag state`) rather than introducing a `tag workflow graph` command group, so
// they do not collide with PRD-105's `tag plan decompose` or a future PRD-109
// workflow interrupt. Everything runs offline through --provider echo, which
// echoes a job's task back as its result — that makes both the guard input and
// the reduced state deterministic without a single API call.

// dagJobStatuses returns id/name -> status for every job in the queue, read
// from `dag show --json`.
func dagJobStatuses(t *testing.T, home string) map[string]string {
	t.Helper()
	out, code := run(t, home, "dag", "show", "--json")
	if code != 0 {
		t.Fatalf("dag show --json: exit %d: %s", code, out)
	}
	var rows []struct {
		ID     string `json:"id"`
		Task   string `json:"task"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("dag show --json is not JSON: %v: %s", err, out)
	}
	byTask := map[string]string{}
	for _, r := range rows {
		byTask[r.Task] = r.Status
	}
	return byTask
}

// TestE2EDagConditionalEdgeTakesOneBranch is the core conditional-edge test:
// the guard that matches runs, and the guard that does not match must NOT
// execute — it ends terminal-'skipped', never 'done'.
func TestE2EDagConditionalEdgeTakesOneBranch(t *testing.T) {
	h := newHome(t)
	steps := `[
	  {"name":"gate","task":"verdict PASS","output":"verdict"},
	  {"name":"on_pass","task":"took the pass branch","depends_on":["gate"],
	   "when":{"source":"verdict","op":"contains","value":"PASS"}},
	  {"name":"on_fail","task":"took the fail branch","depends_on":["gate"],
	   "when":{"source":"verdict","op":"contains","value":"FAIL"}}
	]`
	if out, code := run(t, h, "dag", "save", "branch", "--steps", steps); code != 0 {
		t.Fatalf("dag save: exit %d: %s", code, out)
	}
	out, code := run(t, h, "dag", "run", "branch", "--execute", "--provider", "echo")
	if code != 0 {
		t.Fatalf("dag run --execute: exit %d: %s", code, out)
	}
	st := dagJobStatuses(t, h)
	if st["verdict PASS"] != "done" {
		t.Errorf("gate status = %q, want done (%v)", st["verdict PASS"], st)
	}
	if st["took the pass branch"] != "done" {
		t.Errorf("taken branch status = %q, want done (%v)", st["took the pass branch"], st)
	}
	if st["took the fail branch"] != "skipped" {
		t.Errorf("untaken branch status = %q, want skipped (%v)", st["took the fail branch"], st)
	}
	// The untaken branch must not have produced a result: "not executed" has to
	// be observable, not merely a status label.
	res, _ := run(t, h, "dag", "state", "--json")
	if strings.Contains(res, "took the fail branch") {
		t.Errorf("untaken branch leaked a result into the run state: %s", res)
	}
}

// TestE2EDagSkippedBranchCascades: a node behind an untaken branch must not
// execute either, and must not sit 'pending' forever.
func TestE2EDagSkippedBranchCascades(t *testing.T) {
	h := newHome(t)
	steps := `[
	  {"name":"gate","task":"verdict PASS","output":"verdict"},
	  {"name":"on_fail","task":"remediate","depends_on":["gate"],
	   "when":{"source":"verdict","op":"contains","value":"FAIL"}},
	  {"name":"after_fail","task":"notify the on-call","depends_on":["on_fail"]}
	]`
	if out, code := run(t, h, "dag", "save", "cascade", "--steps", steps); code != 0 {
		t.Fatalf("dag save: exit %d: %s", code, out)
	}
	if out, code := run(t, h, "dag", "run", "cascade", "--execute", "--provider", "echo"); code != 0 {
		t.Fatalf("dag run --execute: exit %d: %s", code, out)
	}
	st := dagJobStatuses(t, h)
	if st["remediate"] != "skipped" {
		t.Errorf("untaken branch = %q, want skipped (%v)", st["remediate"], st)
	}
	if st["notify the on-call"] != "skipped" {
		t.Errorf("node behind the untaken branch = %q, want skipped (%v)", st["notify the on-call"], st)
	}
}

// TestE2EDagReducerMergesState: two nodes writing the same output key with
// reduce=append produce ONE merged value, in step order, and that merged value
// is what a downstream node's {{state.<key>}} reference resolves to.
func TestE2EDagReducerMergesState(t *testing.T) {
	h := newHome(t)
	steps := `[
	  {"name":"a","task":"alpha","output":"findings","reduce":"append"},
	  {"name":"b","task":"beta","output":"findings","reduce":"append"},
	  {"name":"report","task":"REPORT: {{state.findings}}","depends_on":["a","b"]}
	]`
	if out, code := run(t, h, "dag", "save", "reduce", "--steps", steps); code != 0 {
		t.Fatalf("dag save: exit %d: %s", code, out)
	}
	if out, code := run(t, h, "dag", "run", "reduce", "--execute", "--provider", "echo"); code != 0 {
		t.Fatalf("dag run --execute: exit %d: %s", code, out)
	}
	out, code := run(t, h, "dag", "state", "--json")
	if code != 0 {
		t.Fatalf("dag state --json: exit %d: %s", code, out)
	}
	var payload struct {
		State map[string]string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("dag state --json is not JSON: %v: %s", err, out)
	}
	if got := payload.State["findings"]; got != "alpha\nbeta" {
		t.Errorf("reduced state findings = %q, want %q", got, "alpha\nbeta")
	}
	// The reducer output must have actually reached the downstream node: echo
	// returns the interpolated task verbatim.
	if got := payload.State["report"]; got != "REPORT: alpha\nbeta" {
		t.Errorf("downstream node saw state = %q, want %q", got, "REPORT: alpha\nbeta")
	}
}

// TestE2EDagFlowSpecValidation pins the save-time rejections. Every one of
// these is a spec the engine cannot execute, so accepting it and failing later
// (or silently dropping the guard) is the failure mode being guarded against.
func TestE2EDagFlowSpecValidation(t *testing.T) {
	h := newHome(t)
	cases := []struct {
		name  string
		steps string
		want  string
	}{
		{"unknown-op", `[{"task":"a","output":"x"},{"task":"b","depends_on":[0],"when":{"source":"x","op":"bogus","value":"y"}}]`, "op"},
		{"when-without-deps", `[{"task":"a","when":{"source":"x","op":"contains","value":"y"}}]`, "depends_on"},
		{"when-not-object", `[{"task":"a"},{"task":"b","depends_on":[0],"when":"x==y"}]`, "when"},
		{"unknown-reducer", `[{"task":"a","output":"x","reduce":"frobnicate"}]`, "reduce"},
		{"conflicting-reducers", `[{"task":"a","output":"x","reduce":"append"},{"task":"b","output":"x","reduce":"last"}]`, "reduce"},
		{"unknown-source", `[{"task":"a","output":"x"},{"task":"b","depends_on":[0],"when":{"source":"nope","op":"contains","value":"y"}}]`, "nope"},
		{"missing-value", `[{"task":"a","output":"x"},{"task":"b","depends_on":[0],"when":{"source":"x","op":"contains"}}]`, "value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := run(t, h, "dag", "save", "bad-"+tc.name, "--steps", tc.steps)
			if code == 0 {
				t.Fatalf("dag save accepted an unexecutable spec: %s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("error = %q, want it to mention %q", strings.TrimSpace(out), tc.want)
			}
		})
	}
}

// TestE2EDagStateUnknownRunFailsHonestly: asking for a run that does not exist
// must be an error, not an empty-but-successful state map.
func TestE2EDagStateUnknownRunFailsHonestly(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "dag", "state", "no-such-run")
	if code != 1 {
		t.Fatalf("dag state <unknown> should exit 1, got %d: %s", code, out)
	}
	out, code = run(t, h, "dag", "state", "no-such-run", "--json")
	if code != 1 {
		t.Fatalf("dag state <unknown> --json should exit 1, got %d: %s", code, out)
	}
	// run() merges stdout and stderr; the JSON error object is the stdout line.
	var e struct {
		Error string `json:"error"`
	}
	first := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &e); err != nil || e.Error == "" {
		t.Errorf("--json error path must emit a JSON error object, got: %s", out)
	}
}

// TestE2EDagUnknownStateReferenceRejectedAtSave: a reference to a key NO step
// produces is caught at save time, so the operator learns about it before any
// job is enqueued.
func TestE2EDagUnknownStateReferenceRejectedAtSave(t *testing.T) {
	h := newHome(t)
	steps := `[{"task":"a","output":"x"},{"task":"use {{state.ghost}}","depends_on":[0]}]`
	out, code := run(t, h, "dag", "save", "ghost", "--steps", steps)
	if code == 0 {
		t.Fatalf("dag save accepted a reference to a state key no step produces: %s", out)
	}
	if !strings.Contains(out, "ghost") {
		t.Errorf("error must name the unresolvable key, got: %s", out)
	}
}

// TestE2EDagStateRefFromSkippedBranchFailsJob is the runtime half: the key IS
// declared by a step, so save cannot reject it — but that step's conditional
// edge was not taken, so at dispatch time the key does not exist. Interpolating
// "" there would hand the model a silently truncated prompt and record the
// result as a success; the job must fail instead.
func TestE2EDagStateRefFromSkippedBranchFailsJob(t *testing.T) {
	h := newHome(t)
	steps := `[
	  {"name":"gate","task":"verdict PASS","output":"verdict"},
	  {"name":"branch","task":"remediation notes","output":"remedy","depends_on":["gate"],
	   "when":{"source":"verdict","op":"contains","value":"FAIL"}},
	  {"name":"summary","task":"summary: {{state.remedy}}","depends_on":["gate"]}
	]`
	if out, code := run(t, h, "dag", "save", "ghostrun", "--steps", steps); code != 0 {
		t.Fatalf("dag save: exit %d: %s", code, out)
	}
	// exitFindings, not 0. This test asserts a job FAILED and simultaneously
	// required exit 0 — the contradiction that let `dag run --execute` report
	// success with every node failed. A DAG with a failed node is "ran fine,
	// found problems".
	out, code := run(t, h, "dag", "run", "ghostrun", "--execute", "--provider", "echo")
	if code != 3 {
		t.Fatalf("dag run --execute with a failing node should exit 3, got %d: %s", code, out)
	}
	if !strings.Contains(out, "1 failed") {
		t.Errorf("a job whose state reference was never produced must fail, got: %s", out)
	}
	st := dagJobStatuses(t, h)
	if st["remediation notes"] != "skipped" {
		t.Errorf("untaken branch = %q, want skipped", st["remediation notes"])
	}
	if st["summary: {{state.remedy}}"] != "failed" {
		t.Errorf("consumer of a never-produced key = %q, want failed (%v)",
			st["summary: {{state.remedy}}"], st)
	}
}

// TestE2EDagBackwardCompatPlainSpec: a spec with no flow keys keeps working
// exactly as before (the engine must not require the new keys).
func TestE2EDagBackwardCompatPlainSpec(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "dag", "save", "plain", "--steps",
		`[{"task":"a"},{"task":"b","depends_on":[0]}]`); code != 0 {
		t.Fatalf("dag save: exit %d: %s", code, out)
	}
	out, code := run(t, h, "dag", "run", "plain", "--execute", "--provider", "echo")
	if code != 0 || !strings.Contains(out, "submitted: 2 jobs") {
		t.Fatalf("dag run: exit %d: %s", code, out)
	}
	st := dagJobStatuses(t, h)
	if st["a"] != "done" || st["b"] != "done" {
		t.Errorf("plain DAG statuses = %v, want both done", st)
	}
}
