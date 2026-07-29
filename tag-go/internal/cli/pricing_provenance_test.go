package cli_test

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// Issue: the pricing table carried unsourced rates and `pricing get` ignored the
// global --json flag, so provenance could not be surfaced and no caller could
// parse a cost. `costs` meanwhile aggregated only `spans`, which nothing in the
// Go engine writes.

func decodeJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return m
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("key %q missing or not numeric in %v", key, m)
	}
	return v
}

// TestPricingGetFlagsEstimatedModel: a rate TAG cannot corroborate must be
// marked in both surfaces rather than presented as a published price.
func TestPricingGetFlagsEstimatedModel(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "pricing", "get", "--model", "google/gemini-2.5-flash",
		"--input-tokens", "1000000", "--output-tokens", "1000000")
	if code != 0 {
		t.Fatalf("pricing get exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "(estimated — not an authoritative published rate)") {
		t.Errorf("estimated model not flagged in human output: %q", out)
	}

	jout, code := run(t, h, "--json", "pricing", "get", "--model", "google/gemini-2.5-flash",
		"--input-tokens", "1000000", "--output-tokens", "1000000")
	if code != 0 {
		t.Fatalf("pricing get --json exit=%d out=%s", code, jout)
	}
	m := decodeJSON(t, jout)
	if m["estimated"] != true {
		t.Errorf(`"estimated" = %v, want true: %v`, m["estimated"], m)
	}
	if s, _ := m["source"].(string); s == "" {
		t.Errorf("estimated rate must carry a provenance note, got %v", m)
	}
}

// TestPricingGetVerifiedModelNotFlagged: gpt-5.4 now carries a corroborated
// rate, so neither surface may mark it estimated.
func TestPricingGetVerifiedModelNotFlagged(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "pricing", "get", "--model", "gpt-5.4",
		"--input-tokens", "1000000", "--output-tokens", "1000000")
	if code != 0 {
		t.Fatalf("pricing get exit=%d out=%s", code, out)
	}
	if strings.Contains(out, "estimated") {
		t.Errorf("verified model flagged as estimated: %q", out)
	}
	// 2.50 in + 15.00 out per 1M (models.dev, 2026-07), not the unsourced 11.25.
	if !strings.Contains(out, "$17.50000000") {
		t.Errorf("gpt-5.4 cost = %q, want $17.50000000", strings.TrimSpace(out))
	}

	jout, code := run(t, h, "--json", "pricing", "get", "--model", "gpt-5.4",
		"--input-tokens", "1000000", "--output-tokens", "1000000")
	if code != 0 {
		t.Fatalf("pricing get --json exit=%d out=%s", code, jout)
	}
	m := decodeJSON(t, jout)
	if m["estimated"] != false {
		t.Errorf(`"estimated" = %v, want false: %v`, m["estimated"], m)
	}
}

// TestPricingGetJSONIsParseable: pre-fix `pricing get --json` ignored the flag
// and printed "$11.25000000", which is not JSON.
func TestPricingGetJSONIsParseable(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "--json", "pricing", "get", "--model", "gpt-4o",
		"--input-tokens", "1000000", "--output-tokens", "500000")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "$") {
		t.Fatalf("pricing get --json emitted a bare dollar string: %q", out)
	}
	m := decodeJSON(t, out)
	for _, k := range []string{"model_id", "input_tokens", "output_tokens",
		"input_usd_per_1m", "output_usd_per_1m", "cost_usd", "estimated", "source"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing contract key %q in %v", k, m)
		}
	}
	if m["model_id"] != "gpt-4o" {
		t.Errorf("model_id = %v, want gpt-4o", m["model_id"])
	}
	if got := num(t, m, "cost_usd"); math.Abs(got-7.5) > 1e-9 {
		t.Errorf("cost_usd = %v, want 7.5", got)
	}
}

// TestPricingListMarksEstimatedAndIsDeterministic: the human table iterated a
// map, so its row order changed on every run.
func TestPricingListMarksEstimatedAndIsDeterministic(t *testing.T) {
	h := newHome(t)
	first, code := run(t, h, "pricing", "list")
	if code != 0 {
		t.Fatalf("pricing list exit=%d out=%s", code, first)
	}
	for i := 0; i < 3; i++ {
		again, _ := run(t, h, "pricing", "list")
		if again != first {
			t.Fatalf("pricing list output is not deterministic:\n%s\nvs\n%s", first, again)
		}
	}
	for _, l := range strings.Split(first, "\n") {
		if strings.Contains(l, "gemini-2.5-flash") && !strings.Contains(l, "estimated") {
			t.Errorf("estimated row not marked: %q", l)
		}
		if strings.Contains(l, "gpt-5.4") && strings.Contains(l, "estimated") {
			t.Errorf("verified row wrongly marked estimated: %q", l)
		}
	}

	jout, code := run(t, h, "--json", "pricing", "list")
	if code != 0 {
		t.Fatalf("pricing list --json exit=%d out=%s", code, jout)
	}
	rows := map[string]map[string]any{}
	if err := json.Unmarshal([]byte(jout), &rows); err != nil {
		t.Fatalf("pricing list --json not decodable: %v\n%s", err, jout)
	}
	flash, ok := rows["google/gemini-2.5-flash"]
	if !ok {
		t.Fatalf("gemini-2.5-flash missing from list --json")
	}
	for _, k := range []string{"input_usd_per_1m", "output_usd_per_1m", "estimated", "source"} {
		if _, ok := flash[k]; !ok {
			t.Errorf("row missing key %q: %v", k, flash)
		}
	}
	if flash["estimated"] != true {
		t.Errorf("gemini-2.5-flash estimated = %v, want true", flash["estimated"])
	}
}

const seedRunTemplate = `INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status,
	model_id,prompt_tokens,completion_tokens,estimated_cost_usd)
	VALUES('%s','2026-07-01T00:00:00Z','run','code','local','orchestrator','main','p','{}','ok','%s',%d,%d,%s)`

// sprintfRun builds a `runs` INSERT; cost is raw SQL so tests can pass NULL.
func sprintfRun(id, model string, pt, ct int, cost string) string {
	return fmt.Sprintf(seedRunTemplate, id, model, pt, ct, cost)
}

// TestCostsReportsRunsAndSpansSeparately: Go writes only `runs`, Python
// populates tokens/cost only on `spans`, so a single-table rollup reported zero
// for the other engine's data. Both populations must be labelled and totalled.
func TestCostsReportsRunsAndSpansSeparately(t *testing.T) {
	h := newHome(t)
	seedSpans(t, h,
		// stored cost on the run; derived cost on the span
		sprintfRun("r1", "gpt-4o", 1000, 2000, "1.5"),
		`INSERT INTO spans(id,trace_id,name,model_id,started_at,prompt_tokens,completion_tokens,cost_usd)
		 VALUES('s1','tr-1','llm.call','anthropic/claude-sonnet-4-6','2026-07-01T00:00:00Z',1000000,1000000,NULL)`,
	)

	out, code := run(t, h, "costs")
	if code != 0 {
		t.Fatalf("costs exit=%d out=%s", code, out)
	}
	for _, want := range []string{"runs", "spans", "TOTAL",
		"note: runs and spans are different populations"} {
		if !strings.Contains(out, want) {
			t.Errorf("costs human output missing %q:\n%s", want, out)
		}
	}
	lineFor := func(label string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, label) {
				return l
			}
		}
		return ""
	}
	if l := lineFor("runs"); !strings.Contains(l, "prompt=1000") || !strings.Contains(l, "total=3000") {
		t.Errorf("runs line wrong: %q", l)
	}
	if l := lineFor("spans"); !strings.Contains(l, "total=2000000") {
		t.Errorf("spans line wrong: %q", l)
	}
	if l := lineFor("TOTAL"); !strings.Contains(l, "total=2003000") {
		t.Errorf("TOTAL line wrong: %q", l)
	}

	jout, code := run(t, h, "--json", "costs")
	if code != 0 {
		t.Fatalf("costs --json exit=%d out=%s", code, jout)
	}
	doc := decodeJSON(t, jout)
	bySource, ok := doc["by_source"].(map[string]any)
	if !ok {
		t.Fatalf("missing by_source: %v", doc)
	}
	runsSec, _ := bySource["runs"].(map[string]any)
	spansSec, _ := bySource["spans"].(map[string]any)
	if runsSec == nil || spansSec == nil {
		t.Fatalf("by_source must carry both runs and spans: %v", bySource)
	}
	if runsSec["source"] != "runs" || spansSec["source"] != "spans" {
		t.Errorf("sections mislabelled: %v", bySource)
	}
	if got := num(t, runsSec, "rows"); got != 1 {
		t.Errorf("runs rows = %v, want 1", got)
	}
	if got := num(t, runsSec, "total_tokens"); got != 3000 {
		t.Errorf("runs total_tokens = %v, want 3000", got)
	}
	if got := num(t, runsSec, "cost_usd"); math.Abs(got-1.5) > 1e-9 {
		t.Errorf("runs cost_usd = %v, want 1.5 (stored)", got)
	}
	if got := num(t, spansSec, "cost_usd"); math.Abs(got-18.0) > 1e-9 {
		t.Errorf("spans cost_usd = %v, want 18.0 (derived from sonnet-4-6)", got)
	}
	totals, _ := doc["totals"].(map[string]any)
	if totals == nil {
		t.Fatalf("missing totals: %v", doc)
	}
	if got := num(t, totals, "total_tokens"); got != 2003000 {
		t.Errorf("totals.total_tokens = %v, want 2003000", got)
	}
	if got := num(t, totals, "cost_usd"); math.Abs(got-19.5) > 1e-9 {
		t.Errorf("totals.cost_usd = %v, want 19.5", got)
	}
	if totals["overlap_warning"] != true {
		t.Errorf("overlap_warning = %v, want true when both populations exist", totals["overlap_warning"])
	}
	srcs, _ := totals["sources"].([]any)
	if len(srcs) != 2 || srcs[0] != "runs" || srcs[1] != "spans" {
		t.Errorf(`totals.sources = %v, want ["runs","spans"]`, totals["sources"])
	}
}

// TestCostsEmptyDBHasNoOverlapWarning pins the other end of the contract.
func TestCostsEmptyDBHasNoOverlapWarning(t *testing.T) {
	h := newHome(t)
	jout, code := run(t, h, "--json", "costs")
	if code != 0 {
		t.Fatalf("costs --json exit=%d out=%s", code, jout)
	}
	totals, _ := decodeJSON(t, jout)["totals"].(map[string]any)
	if totals == nil {
		t.Fatalf("missing totals: %s", jout)
	}
	if totals["overlap_warning"] != false {
		t.Errorf("overlap_warning = %v on empty DB, want false", totals["overlap_warning"])
	}
	srcs, ok := totals["sources"].([]any)
	if !ok || len(srcs) != 0 {
		t.Errorf("totals.sources = %v, want []", totals["sources"])
	}
}

// TestCostsIncludesEstimatedRates: a cost derived from an unverified rate must
// be declared; one derived only from corroborated rates must not be.
func TestCostsIncludesEstimatedRates(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  bool
	}{
		{"estimated rate", "google/gemini-2.5-flash", true},
		{"verified rate", "gpt-4o", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHome(t)
			// estimated_cost_usd is NOT NULL DEFAULT 0, so an unpopulated run
			// reads as 0 — that is the case the pricing fallback must cover.
			seedSpans(t, h, sprintfRun("r1", tc.model, 1000000, 1000000, "0"))
			jout, code := run(t, h, "--json", "costs")
			if code != 0 {
				t.Fatalf("costs --json exit=%d out=%s", code, jout)
			}
			doc := decodeJSON(t, jout)
			totals, _ := doc["totals"].(map[string]any)
			bySource, _ := doc["by_source"].(map[string]any)
			runsSec, _ := bySource["runs"].(map[string]any)
			if totals["includes_estimated_rates"] != tc.want {
				t.Errorf("totals.includes_estimated_rates = %v, want %v", totals["includes_estimated_rates"], tc.want)
			}
			if runsSec["includes_estimated_rates"] != tc.want {
				t.Errorf("runs.includes_estimated_rates = %v, want %v", runsSec["includes_estimated_rates"], tc.want)
			}
			if got := num(t, runsSec, "cost_usd"); got <= 0 {
				t.Errorf("derived cost = %v, want > 0 (fallback pricing did not fire)", got)
			}
			out, _ := run(t, h, "costs")
			noted := strings.Contains(out, "note: total includes estimated rates")
			if noted != tc.want {
				t.Errorf("human estimated note present=%v, want %v:\n%s", noted, tc.want, out)
			}
		})
	}
}
