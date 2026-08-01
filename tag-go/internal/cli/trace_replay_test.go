package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTraceReplayAgainstProducedSpans is the PRD-032 regression: snapshot /
// checkpoint / replay / diff must work against spans the agent loop ACTUALLY
// emitted, not hand-seeded rows. Before #590 the loop wrote no spans at all, so
// every one of these commands was permanently vacuous; this test fails the
// moment span emission regresses.
func TestTraceReplayAgainstProducedSpans(t *testing.T) {
	h := newHome(t)

	out, code := runEnv(t, h, nil, "--json", "run", "alpha", "--provider", "echo")
	if code != 0 {
		t.Fatalf("run a: exit=%d out=%s", code, out)
	}
	runA := runIDFromJSON(t, out)
	out, code = runEnv(t, h, nil, "--json", "run", "beta beta beta", "--provider", "echo")
	if code != 0 {
		t.Fatalf("run b: exit=%d out=%s", code, out)
	}
	runB := runIDFromJSON(t, out)

	// snapshot must persist a real snapshot (not a no-op on an empty trace).
	if out, code := run(t, h, "trace", "snapshot", runA); code != 0 || !strings.Contains(out, "Snapshot captured") {
		t.Fatalf("snapshot: exit=%d out=%s", code, out)
	}
	// checkpoint lists at least one stored checkpoint for the trace.
	cout, code := run(t, h, "--json", "trace", "checkpoint", runA)
	if code != 0 {
		t.Fatalf("checkpoint: exit=%d out=%s", code, cout)
	}
	var checkpoints []map[string]any
	if err := json.Unmarshal([]byte(cout), &checkpoints); err != nil {
		t.Fatalf("checkpoint --json: %v\n%s", err, cout)
	}
	if len(checkpoints) == 0 {
		t.Fatalf("no checkpoint stored for a trace that has spans: %s", cout)
	}

	// replay must reconstruct the emitted spans.
	rout, code := run(t, h, "trace", "replay", runA)
	if code != 0 {
		t.Fatalf("replay: exit=%d out=%s", code, rout)
	}
	if strings.Contains(rout, "Spans: 0") {
		t.Fatalf("replay found no spans to replay (#590 regression):\n%s", rout)
	}
	for _, want := range []string{"agent.run", "llm.call"} {
		if !strings.Contains(rout, want) {
			t.Errorf("replay output missing %q:\n%s", want, rout)
		}
	}

	jout, code := run(t, h, "--json", "trace", "replay", runA)
	if code != 0 {
		t.Fatalf("replay --json: exit=%d out=%s", code, jout)
	}
	var snap struct {
		TraceID string `json:"trace_id"`
		Spans   []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			PT     int    `json:"prompt_tokens"`
			CT     int    `json:"completion_tokens"`
		} `json:"spans"`
	}
	if err := json.Unmarshal([]byte(jout), &snap); err != nil {
		t.Fatalf("replay --json decode: %v\n%s", err, jout)
	}
	if snap.TraceID != runA {
		t.Errorf("replay trace_id = %q, want %q", snap.TraceID, runA)
	}
	if len(snap.Spans) < 2 {
		t.Fatalf("replay reconstructed %d spans, want >= 2 (agent.run + llm.call)", len(snap.Spans))
	}
	tokens := 0
	for _, s := range snap.Spans {
		tokens += s.PT + s.CT
		if s.Status == "" {
			t.Errorf("span %q replayed without a status", s.Name)
		}
	}
	if tokens == 0 {
		t.Error("replayed spans carry no token usage at all")
	}

	// diff of two REAL traces: "beta beta beta" is longer than "alpha", so the
	// echo provider's token counts differ and the delta must be non-zero.
	dout, code := run(t, h, "trace", "diff", runA, runB)
	if code != 0 {
		t.Fatalf("diff: exit=%d out=%s", code, dout)
	}
	var totalLine string
	for _, l := range strings.Split(dout, "\n") {
		if strings.Contains(l, "TOTAL") {
			totalLine = l
		}
	}
	if totalLine == "" {
		t.Fatalf("diff has no TOTAL row:\n%s", dout)
	}
	if strings.Contains(totalLine, "         0          0") {
		t.Errorf("diff of two real traces reported all-zero tokens:\n%s", dout)
	}
}

// TestTraceJSONErrorPaths covers the --json error contract (issue #530 applied to
// the trace surface): replay/diff of an unknown trace exited 1 but printed a
// bare "error: ..." line to stderr, so a --json consumer got nothing parseable
// on stdout.
func TestTraceJSONErrorPaths(t *testing.T) {
	h := newHome(t)

	cases := [][]string{
		{"--json", "trace", "replay", "no-such-trace"},
		{"--json", "trace", "diff", "no-such-a", "no-such-b"},
	}
	for _, args := range cases {
		out, code := run(t, h, args...)
		if code != 1 {
			t.Errorf("%v exit=%d, want 1\n%s", args, code, out)
		}
		if !strings.HasPrefix(strings.TrimSpace(out), "{") {
			t.Errorf("%v printed no JSON error object on stdout:\n%s", args, out)
			continue
		}
		doc := decodeFirstJSONObject(t, out)
		if _, ok := doc["error"]; !ok {
			t.Errorf("%v JSON error object has no \"error\" key: %s", args, out)
		}
	}

	// A trace with no spans still returns 1 and an empty ARRAY (Python parity),
	// never `null` and never a panic.
	out, code := run(t, h, "--json", "trace", "show", "no-such-trace")
	if code != 1 {
		t.Errorf("trace show unknown exit=%d, want 1\n%s", code, out)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("trace show --json on an unknown trace = %q, want []", strings.TrimSpace(out))
	}
}
