package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tag-agent/tag/internal/store"
)

// This file drives the CLI through `runEnv` (declared in solvers_e2e_test.go),
// which lets a test point the openai adapter at a LOCAL mock SSE server. NO live
// API call is made anywhere in this file.

// spanRow is one row of the spans table as the regression assertions read it.
type spanRow struct {
	ID, TraceID, ParentID, Name, Kind, Status, ModelID string
	PromptTokens, CompletionTokens                     int
	CostUSD                                            *float64
}

func querySpans(t *testing.T, home, traceID string) []spanRow {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(home, "runtime", "tag.sqlite3"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	q := `SELECT id, trace_id, COALESCE(parent_id,''), name, COALESCE(kind,''), status,
	      COALESCE(model_id,''), prompt_tokens, completion_tokens, cost_usd FROM spans`
	var args []any
	if traceID != "" {
		q += ` WHERE trace_id=?`
		args = append(args, traceID)
	}
	q += ` ORDER BY started_at`
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("query spans: %v", err)
	}
	defer rows.Close()
	var out []spanRow
	for rows.Next() {
		var s spanRow
		if err := rows.Scan(&s.ID, &s.TraceID, &s.ParentID, &s.Name, &s.Kind, &s.Status,
			&s.ModelID, &s.PromptTokens, &s.CompletionTokens, &s.CostUSD); err != nil {
			t.Fatalf("scan span: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// runIDFromJSON pulls the run id out of `tag run --json`. The helper captures
// stdout+stderr together and some flags (e.g. --auto-approve) print a notice to
// stderr, so the JSON document is taken from the first '{'.
func runIDFromJSON(t *testing.T, out string) string {
	t.Helper()
	doc := out
	if i := strings.IndexByte(out, '{'); i >= 0 {
		doc = out[i:]
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("run --json output is not JSON (%v): %s", err, out)
	}
	id, _ := m["run_id"].(string)
	if id == "" {
		t.Fatalf("no run_id in %s", out)
	}
	return id
}

// TestRunEmitsSpans is the core regression for #590: `tag run` recorded a run but
// never wrote a single span, so `tag trace list` always said "No spans recorded"
// and the whole trace/otel-export/per-span-cost surface was permanently empty.
func TestRunEmitsSpans(t *testing.T) {
	h := newHome(t)

	out, code := runEnv(t, h, nil, "--json", "run", "list files", "--provider", "echo")
	if code != 0 {
		t.Fatalf("run exit=%d out=%s", code, out)
	}
	runID := runIDFromJSON(t, out)

	spans := querySpans(t, h, runID)
	if len(spans) == 0 {
		t.Fatalf("`tag run` produced NO spans for run %s (#590)", runID)
	}

	// The trace id IS the run id, so trace show <run> resolves.
	var root, llm *spanRow
	for i := range spans {
		switch spans[i].Kind {
		case "agent":
			root = &spans[i]
		case "llm":
			llm = &spans[i]
		}
	}
	if root == nil {
		t.Fatal("no agent.run root span")
	}
	if root.ParentID != "" {
		t.Errorf("root span must have no parent, got %q", root.ParentID)
	}
	if llm == nil {
		t.Fatal("no llm span for the provider turn")
	}
	if llm.ParentID != root.ID {
		t.Errorf("llm span parent = %q, want the root %q", llm.ParentID, root.ID)
	}
	if llm.PromptTokens == 0 && llm.CompletionTokens == 0 {
		t.Error("llm span carries no token usage")
	}
	// The root must NOT restate the turn's tokens, or summing the spans
	// population double-counts a run against its own turns (#586).
	if root.PromptTokens != 0 || root.CompletionTokens != 0 {
		t.Errorf("root span must not duplicate turn tokens, got %d/%d",
			root.PromptTokens, root.CompletionTokens)
	}

	// trace list / trace show must now find the trace.
	if out, code := run(t, h, "trace", "list"); code != 0 || !strings.Contains(out, runID) {
		t.Fatalf("trace list did not list run %s: exit=%d out=%s", runID, code, out)
	}
	out, code = run(t, h, "trace", "show", runID)
	if code != 0 {
		t.Fatalf("trace show exit=%d out=%s", code, out)
	}
	if strings.Contains(out, "No spans found") || !strings.Contains(out, "llm.call") {
		t.Fatalf("trace show %s did not render the run's spans: %s", runID, out)
	}
}

// TestRunSpanCostIsAttributed covers PRD-046: a turn whose model is in the
// pricing table must land a per-span cost_usd, and it must be > 0.
// TestEchoRunSpansAreFree: an echo (offline) run must record its llm spans with
// model "echo" and NO cost — echo does no real inference, so billing it at the
// profile's configured model rate is fabricated cost (#742). Priced attribution
// for a REAL model is covered by the seeded cost-attribution tests.
func TestEchoRunSpansAreFree(t *testing.T) {
	h := newHome(t)
	// A priced model is configured, but the echo provider is what actually runs.
	if out, code := run(t, h, "set-model", "orchestrator", "openai/gpt-4o-mini"); code != 0 {
		t.Fatalf("set-model exit=%d out=%s", code, out)
	}
	out, code := runEnv(t, h, nil, "--json", "run", "price me",
		"--provider", "echo", "--profile", "orchestrator")
	if code != 0 {
		t.Fatalf("run exit=%d out=%s", code, out)
	}
	runID := runIDFromJSON(t, out)
	var llm int
	for _, s := range querySpans(t, h, runID) {
		if s.Kind != "llm" {
			continue
		}
		llm++
		if s.ModelID != "echo" {
			t.Errorf("echo run llm span %s recorded model %q, want \"echo\"", s.ID, s.ModelID)
		}
		if s.CostUSD != nil && *s.CostUSD != 0 {
			t.Errorf("echo run llm span %s cost_usd = %v, want 0/nil (echo is free)", s.ID, *s.CostUSD)
		}
	}
	if llm == 0 {
		t.Fatal("no llm span recorded")
	}
}

// mockToolCallServer is a local OpenAI-compatible SSE endpoint: the first
// completion requests a `list_dir` tool call, the second answers with text. It
// lets the tool-call span path be exercised end to end with NO live API call.
func mockToolCallServer(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(payload string) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if n == 1 {
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]}}]}`)
		} else {
			send(`{"choices":[{"delta":{"content":"listed the directory"}}]}`)
		}
		send(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
		send("[DONE]")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunToolCallEmitsChildSpan is the PRD-048 half of #590: a tool call must
// produce a span that is a real CHILD of the llm turn that requested it.
func TestRunToolCallEmitsChildSpan(t *testing.T) {
	h := newHome(t)
	srv := mockToolCallServer(t)
	env := []string{
		"OPENAI_BASE_URL=" + srv.URL,
		"OPENAI_API_KEY=mock-key-not-a-real-credential",
	}
	out, code := runEnv(t, h, env, "--json", "run", "list the files",
		"--provider", "openai", "--tools", "--auto-approve")
	if code != 0 {
		t.Fatalf("run exit=%d out=%s", code, out)
	}
	runID := runIDFromJSON(t, out)

	spans := querySpans(t, h, runID)
	if len(spans) == 0 {
		t.Fatalf("no spans for tool-calling run %s (#590)", runID)
	}
	byID := map[string]spanRow{}
	for _, s := range spans {
		byID[s.ID] = s
	}
	var tool *spanRow
	for i := range spans {
		if spans[i].Kind == "tool" {
			tool = &spans[i]
		}
	}
	if tool == nil {
		t.Fatalf("the list_dir tool call produced no tool span (PRD-048); spans=%+v", spans)
	}
	if tool.Name != "list_dir" {
		t.Errorf("tool span name = %q, want list_dir", tool.Name)
	}
	parent, ok := byID[tool.ParentID]
	if !ok {
		t.Fatalf("tool span parent %q is not a span in this trace", tool.ParentID)
	}
	if parent.Kind != "llm" {
		t.Errorf("tool span parent kind = %q, want llm", parent.Kind)
	}
	grand, ok := byID[parent.ParentID]
	if !ok || grand.Kind != "agent" {
		t.Errorf("llm span must nest under the agent root, parent=%q", parent.ParentID)
	}
	// trace show must render the child.
	if out, code := run(t, h, "trace", "show", runID); code != 0 || !strings.Contains(out, "list_dir") {
		t.Fatalf("trace show missing the tool span: exit=%d out=%s", code, out)
	}
}

// TestOtelExportOfProducedSpansStaysValid re-runs the OTLP contract from
// TestOtelExportEmitsValidOTLP against spans the agent loop ACTUALLY produced
// (rather than hand-seeded rows), so the new producer cannot regress the
// exporter: 32/16 lowercase-hex ids, integer status enum, no non-spec top-level
// keys, and no empty parentSpanId.
func TestOtelExportOfProducedSpansStaysValid(t *testing.T) {
	h := newHome(t)
	srv := mockToolCallServer(t)
	env := []string{"OPENAI_BASE_URL=" + srv.URL, "OPENAI_API_KEY=mock-key-not-a-real-credential"}
	out, code := runEnv(t, h, env, "--json", "run", "list the files",
		"--provider", "openai", "--tools", "--auto-approve")
	if code != 0 {
		t.Fatalf("run exit=%d out=%s", code, out)
	}
	runID := runIDFromJSON(t, out)

	for _, args := range [][]string{{"otel-export"}, {"otel-export", "--trace-id", runID}, {"trace", "export"}} {
		out, code := run(t, h, args...)
		if code != 0 {
			t.Fatalf("%v exit=%d out=%s", args, code, out)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatalf("%v: not JSON: %v\n%s", args, err, out)
		}
		for k := range raw {
			if k != "resourceSpans" {
				t.Errorf("%v: non-spec top-level key %q", args, k)
			}
		}
		var doc otlpDoc
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		n := 0
		for _, rs := range doc.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					n++
					if len(sp.TraceID) != 32 || !isLowerHex(sp.TraceID) {
						t.Errorf("%v: traceId %q must be 32 lowercase-hex chars", args, sp.TraceID)
					}
					if len(sp.SpanID) != 16 || !isLowerHex(sp.SpanID) {
						t.Errorf("%v: spanId %q must be 16 lowercase-hex chars", args, sp.SpanID)
					}
					if sp.ParentSpanID != "" && (len(sp.ParentSpanID) != 16 || !isLowerHex(sp.ParentSpanID)) {
						t.Errorf("%v: parentSpanId %q must be 16 lowercase-hex chars", args, sp.ParentSpanID)
					}
					var codeNum int
					if err := json.Unmarshal(sp.Status.Code, &codeNum); err != nil {
						t.Errorf("%v: status.code %s must be the numeric enum", args, sp.Status.Code)
					}
				}
			}
		}
		if n == 0 {
			t.Errorf("%v exported no spans even though a run just produced some", args)
		}
	}
}

// TestCostsSeparatesProducedRunsAndSpans keeps the #586 contract working now
// that Go actually writes spans: runs and spans stay separately labelled
// populations and the overlap is warned about rather than silently summed.
func TestCostsSeparatesProducedRunsAndSpans(t *testing.T) {
	h := newHome(t)
	if out, code := runEnv(t, h, nil, "run", "hello", "--provider", "echo"); code != 0 {
		t.Fatalf("run exit=%d out=%s", code, out)
	}
	out, code := run(t, h, "--json", "costs")
	if code != 0 {
		t.Fatalf("costs exit=%d out=%s", code, out)
	}
	var doc struct {
		BySource map[string]struct {
			Rows int64 `json:"rows"`
		} `json:"by_source"`
		Totals struct {
			Sources        []string `json:"sources"`
			OverlapWarning bool     `json:"overlap_warning"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("costs --json: %v\n%s", err, out)
	}
	if doc.BySource["runs"].Rows == 0 {
		t.Error("costs lost the runs population")
	}
	if doc.BySource["spans"].Rows == 0 {
		t.Error("costs reports no spans even though the loop now produces them")
	}
	if !doc.Totals.OverlapWarning {
		t.Error("runs and spans both populated must still raise overlap_warning (#586)")
	}
	if len(doc.Totals.Sources) != 2 {
		t.Errorf("sources = %v, want both runs and spans", doc.Totals.Sources)
	}
}
