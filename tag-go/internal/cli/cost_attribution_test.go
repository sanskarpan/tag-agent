package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// seedCostTrace inserts a realistic nested trace:
//
//	agent.run (no model, no cost)
//	├─ llm.call   gpt-4o          1M/1M  -> $12.500000 (authoritative rate)
//	│  └─ list_dir (tool, no model, no cost)
//	├─ llm.call   qwen/qwen-plus  1M/1M  -> $1.600000  (ESTIMATED rate)
//	└─ llm.call   made-up/nope    100/100 -> no rate at all (cost stays NULL)
//
// Total of the known costs is $14.100000; the unpriced span must be excluded.
func seedCostTrace(t *testing.T, home string) {
	t.Helper()
	seedSpans(t, home,
		`INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens,kind,cost_usd)
		 VALUES('c0','tr-cost',NULL,'agent.run','researcher',NULL,'2026-07-01T00:00:00Z','2026-07-01T00:00:10Z',10000,'ok',0,0,'agent',NULL)`,
		`INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens,kind,cost_usd)
		 VALUES('c1','tr-cost','c0','llm.call','researcher','gpt-4o','2026-07-01T00:00:01Z','2026-07-01T00:00:04Z',3000,'ok',1000000,1000000,'llm',12.5)`,
		`INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens,kind,cost_usd)
		 VALUES('c2','tr-cost','c1','list_dir','researcher',NULL,'2026-07-01T00:00:02Z','2026-07-01T00:00:03Z',900,'ok',0,0,'tool',NULL)`,
		`INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens,kind,cost_usd)
		 VALUES('c3','tr-cost','c0','llm.call','researcher','qwen/qwen-plus','2026-07-01T00:00:05Z','2026-07-01T00:00:07Z',2000,'ok',1000000,1000000,'llm',1.6)`,
		`INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens,kind,cost_usd)
		 VALUES('c4','tr-cost','c0','llm.call','researcher','made-up/nope','2026-07-01T00:00:08Z','2026-07-01T00:00:09Z',1000,'ok',100,100,'llm',NULL)`,
	)
}

// leadingSpaces counts the indentation of the first line whose trimmed text
// starts with want.
func leadingSpaces(out, want string) (int, bool) {
	for _, l := range strings.Split(out, "\n") {
		trimmed := strings.TrimLeft(l, " ")
		// Skip the status glyph so both the plain and the cost view work.
		body := strings.TrimLeft(trimmed, "✓✗⚠▸ ")
		if strings.HasPrefix(body, want) {
			return len(l) - len(trimmed), true
		}
	}
	return 0, false
}

// TestTraceShowRendersNestedFlameChart covers PRD-013 G3 ("tag trace <run_id>
// displays a terminal flame-chart of the run"). The Go renderer printed a FLAT
// list — every span at column 0, no parent/child structure and no duration bar —
// so a trace's shape was invisible even though parent_id was recorded.
func TestTraceShowRendersNestedFlameChart(t *testing.T) {
	h := newHome(t)
	seedCostTrace(t, h)

	out, code := run(t, h, "trace", "show", "tr-cost")
	if code != 0 {
		t.Fatalf("trace show exit=%d out=%s", code, out)
	}
	root, ok := leadingSpaces(out, "agent.run")
	if !ok {
		t.Fatalf("no agent.run row:\n%s", out)
	}
	llm, ok := leadingSpaces(out, "llm.call")
	if !ok {
		t.Fatalf("no llm.call row:\n%s", out)
	}
	tool, ok := leadingSpaces(out, "list_dir")
	if !ok {
		t.Fatalf("no list_dir row:\n%s", out)
	}
	if !(root < llm && llm < tool) {
		t.Errorf("spans are not nested by parent: agent.run@%d llm.call@%d list_dir@%d\n%s",
			root, llm, tool, out)
	}
	if !strings.Contains(out, "█") {
		t.Errorf("no duration bar in the flame chart:\n%s", out)
	}
}

// TestTraceShowJSONCarriesCostAndKind covers PRD-046 FR-20: the machine-readable
// span list must expose the per-span cost and span kind that the schema already
// stores. `trace show --json` emitted neither.
func TestTraceShowJSONCarriesCostAndKind(t *testing.T) {
	h := newHome(t)
	seedCostTrace(t, h)

	out, code := run(t, h, "--json", "trace", "show", "tr-cost")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var spans []map[string]any
	if err := json.Unmarshal([]byte(out), &spans); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	byID := map[string]map[string]any{}
	for _, s := range spans {
		byID[s["id"].(string)] = s
	}
	c1 := byID["c1"]
	if c1 == nil {
		t.Fatalf("span c1 missing from %s", out)
	}
	cost, ok := c1["cost_usd"].(float64)
	if !ok {
		t.Fatalf("span c1 has no numeric cost_usd: %v", c1["cost_usd"])
	}
	if cost != 12.5 {
		t.Errorf("c1 cost_usd = %v, want 12.5", cost)
	}
	if c1["kind"] != "llm" {
		t.Errorf("c1 kind = %v, want llm", c1["kind"])
	}
	if v, present := byID["c4"]["cost_usd"]; !present || v != nil {
		t.Errorf("unpriced span c4 cost_usd = %v, want JSON null", v)
	}
}

// TestTraceShowCostView covers PRD-046 G4/G10/FR-12: `--cost` renders a per-span
// cost waterfall with a TOTAL row, unpriced spans show an em dash and are left
// out of the total, and a single summary warning is emitted.
func TestTraceShowCostView(t *testing.T) {
	h := newHome(t)
	seedCostTrace(t, h)

	out, code := run(t, h, "trace", "show", "tr-cost", "--cost")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	for _, want := range []string{"COST USD", "$12.500000", "$1.600000", "TOTAL", "$14.100000"} {
		if !strings.Contains(out, want) {
			t.Errorf("cost view missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "—") {
		t.Errorf("unpriced span not rendered as an em dash:\n%s", out)
	}
	if !strings.Contains(out, "no known rate") {
		t.Errorf("no summary warning for the unpriced span:\n%s", out)
	}
	// The estimated qwen rate must not be laundered into an exact-looking number.
	if !strings.Contains(out, "estimated") {
		t.Errorf("estimated rate not flagged in the cost view:\n%s", out)
	}
}

// TestTraceShowCostJSON covers PRD-046 §12.4: `--cost --json` emits an object
// with total_cost_usd summing only the priced spans, and it must declare that the
// total includes an estimated rate.
func TestTraceShowCostJSON(t *testing.T) {
	h := newHome(t)
	seedCostTrace(t, h)

	out, code := run(t, h, "--json", "trace", "show", "tr-cost", "--cost")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var doc struct {
		RunID       string  `json:"run_id"`
		Profile     string  `json:"profile"`
		TotalCost   float64 `json:"total_cost_usd"`
		Estimated   bool    `json:"includes_estimated_rates"`
		UnpricedNum int     `json:"unpriced_spans"`
		Spans       []struct {
			ID        string   `json:"id"`
			CostUSD   *float64 `json:"cost_usd"`
			Estimated bool     `json:"cost_rate_estimated"`
		} `json:"spans"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if doc.RunID != "tr-cost" {
		t.Errorf("run_id = %q", doc.RunID)
	}
	if doc.Profile != "researcher" {
		t.Errorf("profile = %q", doc.Profile)
	}
	if d := doc.TotalCost - 14.1; d > 1e-9 || d < -1e-9 {
		t.Errorf("total_cost_usd = %v, want 14.1", doc.TotalCost)
	}
	if !doc.Estimated {
		t.Error("total derived partly from an estimated rate must set includes_estimated_rates")
	}
	if doc.UnpricedNum != 1 {
		t.Errorf("unpriced_spans = %d, want 1", doc.UnpricedNum)
	}
	if len(doc.Spans) != 5 {
		t.Fatalf("want 5 spans, got %d", len(doc.Spans))
	}
	for _, s := range doc.Spans {
		switch s.ID {
		case "c3":
			if !s.Estimated {
				t.Error("c3 (qwen, estimated rate) must set cost_rate_estimated")
			}
		case "c1":
			if s.Estimated {
				t.Error("c1 (gpt-4o, published rate) must NOT be flagged estimated")
			}
		case "c4":
			if s.CostUSD != nil {
				t.Errorf("c4 cost_usd = %v, want null", *s.CostUSD)
			}
		}
	}
}

// TestTraceShowMinCostFilter covers PRD-046 §7.1 --min-cost-usd.
func TestTraceShowMinCostFilter(t *testing.T) {
	h := newHome(t)
	seedCostTrace(t, h)

	out, code := run(t, h, "--json", "trace", "show", "tr-cost", "--cost", "--min-cost-usd", "2")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var doc struct {
		Spans []struct {
			ID string `json:"id"`
		} `json:"spans"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(doc.Spans) != 1 || doc.Spans[0].ID != "c1" {
		t.Fatalf("--min-cost-usd 2 kept %+v, want only c1", doc.Spans)
	}
	// An empty result must still be an array, never null.
	out, code = run(t, h, "--json", "trace", "show", "tr-cost", "--cost", "--min-cost-usd", "9999")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, `"spans": []`) {
		t.Errorf("empty span list must serialize as [], got:\n%s", out)
	}
	// A negative threshold is a usage error, not a silently-accepted filter.
	if out, code := run(t, h, "trace", "show", "tr-cost", "--cost", "--min-cost-usd", "-1"); code != 2 {
		t.Errorf("--min-cost-usd -1 exit=%d, want 2\n%s", code, out)
	}
}

// TestStatsCostAggregation covers PRD-046 G5/§7.2: `tag stats --cost --by model`.
func TestStatsCostAggregation(t *testing.T) {
	h := newHome(t)
	seedCostTrace(t, h)

	if out, code := run(t, h, "stats", "--cost", "--by", "bogus", "--since", "3650d"); code != 2 {
		t.Errorf("--by bogus exit=%d, want 2 (usage)\n%s", code, out)
	}
	if out, code := run(t, h, "stats", "--since", "3650d"); code != 2 {
		t.Errorf("stats without --cost exit=%d, want 2 (usage)\n%s", code, out)
	}

	out, code := run(t, h, "--json", "stats", "--cost", "--by", "model", "--since", "3650d")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var doc struct {
		By        []string `json:"by"`
		TotalCost float64  `json:"total_cost_usd"`
		Estimated bool     `json:"includes_estimated_rates"`
		Groups    []struct {
			ModelID   string   `json:"model_id"`
			SpanCount int      `json:"span_count"`
			InTok     int      `json:"input_tokens"`
			OutTok    int      `json:"output_tokens"`
			CostUSD   *float64 `json:"cost_usd"`
			Estimated bool     `json:"includes_estimated_rates"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(doc.By) != 1 || doc.By[0] != "model" {
		t.Errorf("by = %v", doc.By)
	}
	if d := doc.TotalCost - 14.1; d > 1e-9 || d < -1e-9 {
		t.Errorf("total_cost_usd = %v, want 14.1", doc.TotalCost)
	}
	if !doc.Estimated {
		t.Error("aggregate over an estimated rate must set includes_estimated_rates")
	}
	found := map[string]*float64{}
	seen := map[string]bool{}
	for _, g := range doc.Groups {
		found[g.ModelID] = g.CostUSD
		seen[g.ModelID] = true
		if g.ModelID == "qwen/qwen-plus" && !g.Estimated {
			t.Error("qwen group must be flagged as an estimated rate")
		}
	}
	if found["gpt-4o"] == nil || *found["gpt-4o"] != 12.5 {
		t.Errorf("gpt-4o group cost = %v, want 12.5", found["gpt-4o"])
	}
	if !seen["made-up/nope"] {
		t.Error("unpriced model must still be reported as a group")
	}
	// A group nothing in which could be priced must report null, NOT $0.00 — a
	// hard zero would claim the spans were free rather than unpriceable.
	if c := found["made-up/nope"]; c != nil {
		t.Errorf("unpriceable group cost_usd = %v, want null", *c)
	}

	// Empty window -> groups must be [] and not null.
	out, code = run(t, h, "--json", "stats", "--cost", "--since", "1d")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, `"groups": []`) {
		t.Errorf("empty aggregation must emit [], got:\n%s", out)
	}
}

// TestStatsCostIncludesJustWrittenSpans is a dispatch-layer regression: every
// seeded-fixture test passed while `tag stats --cost` reported ZERO spans for a
// run made seconds earlier. The window bounds are second-precision ISO strings
// but span timestamps carry a fractional part and a trailing Z, so the raw string
// comparison `started_at <= until` excluded everything written in the current
// second — i.e. exactly the run the user had just made.
//
// It also pins the "unpriced" rule: the root agent.run span records a KNOWN model
// with zero tokens, and must not be counted as a span whose rate is missing.
func TestStatsCostIncludesJustWrittenSpans(t *testing.T) {
	h := newHome(t)
	// Seed a trace with a PRICED llm span (gpt-4o-mini, tokens, cost left NULL so
	// the pipeline must price it) plus a root span on the same priced model but
	// with zero tokens (which must NOT count as a missing rate). This formerly
	// ran the echo provider, which is now correctly free (#742), so priced
	// attribution is exercised with a real model via a seeded span instead.
	runID := "tr-just"
	seedSpans(t, h,
		`INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens,kind,cost_usd)
		 VALUES('r0','tr-just',NULL,'agent.run','orchestrator','openai/gpt-4o-mini','2026-07-01T00:00:00Z','2026-07-01T00:00:10Z',10000,'ok',0,0,'agent',NULL)`,
		`INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens,kind,cost_usd)
		 VALUES('r1','tr-just','r0','llm.call','orchestrator','openai/gpt-4o-mini','2026-07-01T00:00:01Z','2026-07-01T00:00:04Z',3000,'ok',100000,100000,'llm',NULL)`,
	)

	out, code := run(t, h, "--json", "stats", "--cost", "--since", "2026-06-01")
	if code != 0 {
		t.Fatalf("stats: exit=%d out=%s", code, out)
	}
	var doc struct {
		SpanCount int     `json:"span_count"`
		TotalCost float64 `json:"total_cost_usd"`
		Groups    []struct {
			ModelID string `json:"model_id"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if doc.SpanCount == 0 {
		t.Fatalf("stats --cost saw none of the spans a run just wrote:\n%s", out)
	}
	if doc.TotalCost <= 0 {
		t.Errorf("total_cost_usd = %v, want > 0", doc.TotalCost)
	}
	if len(doc.Groups) == 0 {
		t.Errorf("no groups for a run that just happened:\n%s", out)
	}

	// The root span names a priced model but carries no tokens: not billable, and
	// NOT a missing rate.
	out, code = run(t, h, "--json", "trace", "show", runID, "--cost")
	if code != 0 {
		t.Fatalf("trace show: exit=%d out=%s", code, out)
	}
	var show struct {
		Unpriced  int     `json:"unpriced_spans"`
		TotalCost float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal([]byte(out), &show); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if show.Unpriced != 0 {
		t.Errorf("unpriced_spans = %d; a zero-token span on a PRICED model is not a missing rate\n%s",
			show.Unpriced, out)
	}
	if show.TotalCost <= 0 {
		t.Errorf("total_cost_usd = %v, want > 0", show.TotalCost)
	}
}

// TestCostsRunIDReport covers PRD-046 §7.4 / U4 / U9.
func TestCostsRunIDReport(t *testing.T) {
	h := newHome(t)
	seedCostTrace(t, h)

	out, code := run(t, h, "costs", "--run-id", "tr-cost")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	for _, want := range []string{"llm.call", "gpt-4o", "$12.500000", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("costs --run-id missing %q:\n%s", want, out)
		}
	}

	out, code = run(t, h, "--json", "costs", "--run-id", "tr-cost", "--by", "model")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var doc struct {
		RunID     string  `json:"run_id"`
		By        string  `json:"by"`
		TotalCost float64 `json:"total_cost_usd"`
		Rows      []struct {
			ModelID string   `json:"model_id"`
			CostUSD *float64 `json:"cost_usd"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if doc.By != "model" || doc.RunID != "tr-cost" {
		t.Errorf("by=%q run_id=%q", doc.By, doc.RunID)
	}
	if d := doc.TotalCost - 14.1; d > 1e-9 || d < -1e-9 {
		t.Errorf("total_cost_usd = %v, want 14.1", doc.TotalCost)
	}
	if len(doc.Rows) == 0 {
		t.Fatalf("no rows: %s", out)
	}

	// Unknown run: runtime error (1) with a JSON error object on --json.
	out, code = run(t, h, "--json", "costs", "--run-id", "nope")
	if code != 1 {
		t.Errorf("unknown run exit=%d, want 1\n%s", code, out)
	}
	if _, ok := decodeFirstJSONObject(t, out)["error"]; !ok {
		t.Errorf("error path JSON has no \"error\" key: %s", out)
	}
	if out, code := run(t, h, "costs", "--run-id", "tr-cost", "--by", "bogus"); code != 2 {
		t.Errorf("--by bogus exit=%d, want 2\n%s", code, out)
	}
	// The default (no --run-id) contract must be untouched.
	if out, code := run(t, h, "--json", "costs"); code != 0 || !strings.Contains(out, "overlap_warning") {
		t.Errorf("plain `costs` contract changed: exit=%d out=%s", code, out)
	}
}

// TestPricingShow covers PRD-046 §7.5 / U5 / U6.
func TestPricingShow(t *testing.T) {
	h := newHome(t)

	out, code := run(t, h, "pricing", "show")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	for _, want := range []string{"gpt-4o", "IN $/1M", "models total"} {
		if !strings.Contains(out, want) {
			t.Errorf("pricing show missing %q:\n%s", want, out)
		}
	}

	out, code = run(t, h, "--json", "pricing", "show", "--model", "qwen/qwen-plus")
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	var one struct {
		ModelID   string  `json:"model_id"`
		In        float64 `json:"input_usd_per_1m"`
		Estimated bool    `json:"estimated"`
		Source    string  `json:"source"`
	}
	if err := json.Unmarshal([]byte(out), &one); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if one.ModelID != "qwen/qwen-plus" || !one.Estimated || one.Source == "" {
		t.Errorf("pricing show --model lost provenance: %+v", one)
	}

	out, code = run(t, h, "--json", "pricing", "show", "--model", "no/such-model")
	if code != 1 {
		t.Errorf("unknown model exit=%d, want 1\n%s", code, out)
	}
	if _, ok := decodeFirstJSONObject(t, out)["error"]; !ok {
		t.Errorf("no \"error\" key: %s", out)
	}
}

// TestOtelExportCarriesSpanCost covers PRD-046 §10.7: a priced span must export
// gen_ai.usage.cost_usd, and the document must stay valid OTLP.
func TestOtelExportCarriesSpanCost(t *testing.T) {
	h := newHome(t)
	seedCostTrace(t, h)

	for _, args := range [][]string{{"otel-export"}, {"trace", "export"}} {
		out, code := run(t, h, args...)
		if code != 0 {
			t.Fatalf("%v exit=%d out=%s", args, code, out)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatalf("%v: not JSON: %v", args, err)
		}
		for k := range raw {
			if k != "resourceSpans" {
				t.Errorf("%v: non-spec top-level key %q", args, k)
			}
		}
		var doc struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						SpanID     string `json:"spanId"`
						Attributes []struct {
							Key   string `json:"key"`
							Value struct {
								DoubleValue *float64 `json:"doubleValue"`
							} `json:"value"`
						} `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%v: decode: %v", args, err)
		}
		costs := 0
		for _, rs := range doc.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					for _, a := range sp.Attributes {
						if a.Key != "gen_ai.usage.cost_usd" {
							continue
						}
						costs++
						if a.Value.DoubleValue == nil {
							t.Errorf("%v: cost attribute must be a doubleValue", args)
						}
					}
				}
			}
		}
		if costs != 2 {
			t.Errorf("%v: %d spans carry gen_ai.usage.cost_usd, want 2 (only the priced ones)", args, costs)
		}
	}
}

// decodeFirstJSONObject pulls the FIRST JSON object out of combined
// stdout+stderr. The --json error contract prints `{"error": ...}` on stdout and
// Execute still prints its `error: ...` line on stderr, so the captured stream
// has trailing non-JSON text that a plain Unmarshal would choke on.
func decodeFirstJSONObject(t *testing.T, out string) map[string]any {
	t.Helper()
	i := strings.IndexByte(out, '{')
	if i < 0 {
		t.Fatalf("no JSON object in output:\n%s", out)
	}
	var doc map[string]any
	if err := json.NewDecoder(strings.NewReader(out[i:])).Decode(&doc); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, out)
	}
	return doc
}
