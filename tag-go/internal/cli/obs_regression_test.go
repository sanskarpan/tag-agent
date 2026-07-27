package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/store"
)

// seedSpans opens the TAG_HOME control-plane DB directly and inserts spans.
func seedSpans(t *testing.T, home string, stmts ...string) {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(home, "runtime", "tag.sqlite3"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

// otlpDoc is the subset of an OTLP/JSON export the collector contract covers.
type otlpDoc struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []struct {
				Key   string `json:"key"`
				Value struct {
					StringValue string `json:"stringValue"`
				} `json:"value"`
			} `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				TraceID      string `json:"traceId"`
				SpanID       string `json:"spanId"`
				ParentSpanID string `json:"parentSpanId"`
				Name         string `json:"name"`
				Status       struct {
					Code json.RawMessage `json:"code"`
				} `json:"status"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return s != ""
}

// TestOtelExportEmitsValidOTLP covers finding #6: traceId/spanId must be 32/16
// lowercase-hex chars, status.code must be the numeric OTLP StatusCode enum,
// service.name must be "tag-agent", and no underscore-prefixed top-level keys
// may appear in the document.
func TestOtelExportEmitsValidOTLP(t *testing.T) {
	h := newHome(t)
	seedSpans(t, h,
		`INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens)
		 VALUES('sp1','tr-abc',NULL,'llm.call','coder','anthropic/claude-sonnet-4-6','2026-07-01T00:00:00Z','2026-07-01T00:00:01Z',1500,'ok',100,200)`,
		`INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens)
		 VALUES('sp2','tr-abc','sp1','llm.call','coder','anthropic/claude-sonnet-4-6','2026-07-01T00:00:02Z','2026-07-01T00:00:04Z',2700,'error',300,400)`,
	)

	for _, args := range [][]string{{"otel-export"}, {"trace", "export"}} {
		out, code := run(t, h, args...)
		if code != 0 {
			t.Fatalf("%v exit=%d out=%s", args, code, out)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatalf("%v: output is not JSON: %v\n%s", args, err, out)
		}
		for k := range raw {
			if strings.HasPrefix(k, "_") {
				t.Errorf("%v: OTLP document contains non-spec top-level key %q", args, k)
			}
		}
		var doc otlpDoc
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%v: decode otlp: %v", args, err)
		}
		if len(doc.ResourceSpans) != 1 {
			t.Fatalf("%v: want 1 resourceSpans, got %d", args, len(doc.ResourceSpans))
		}
		rs := doc.ResourceSpans[0]
		svc := ""
		for _, a := range rs.Resource.Attributes {
			if a.Key == "service.name" {
				svc = a.Value.StringValue
			}
		}
		if svc != "tag-agent" {
			t.Errorf("%v: service.name = %q, want tag-agent", args, svc)
		}
		spans := rs.ScopeSpans[0].Spans
		if len(spans) != 2 {
			t.Fatalf("%v: want 2 spans, got %d", args, len(spans))
		}
		codesSeen := map[string]bool{}
		for _, s := range spans {
			if len(s.TraceID) != 32 || !isLowerHex(s.TraceID) {
				t.Errorf("%v: traceId %q is not 32 lowercase-hex chars", args, s.TraceID)
			}
			if len(s.SpanID) != 16 || !isLowerHex(s.SpanID) {
				t.Errorf("%v: spanId %q is not 16 lowercase-hex chars", args, s.SpanID)
			}
			if s.ParentSpanID != "" && (len(s.ParentSpanID) != 16 || !isLowerHex(s.ParentSpanID)) {
				t.Errorf("%v: parentSpanId %q is not 16 lowercase-hex chars", args, s.ParentSpanID)
			}
			var n int
			if err := json.Unmarshal(s.Status.Code, &n); err != nil {
				t.Errorf("%v: status.code %s is not the numeric OTLP enum", args, s.Status.Code)
				continue
			}
			if n < 0 || n > 2 {
				t.Errorf("%v: status.code %d out of OTLP range", args, n)
			}
			codesSeen[string(s.Status.Code)] = true
		}
		if !codesSeen["1"] || !codesSeen["2"] {
			t.Errorf("%v: want OK(1) and ERROR(2) codes, saw %v", args, codesSeen)
		}
	}
}

// TestTraceDiffKeepsSameNamedSpans covers finding #7: two spans named llm.call
// in trace A must both be reflected in the diff (previously the second
// overwrote the first, so 300+700 tokens were reported as 700).
func TestTraceDiffKeepsSameNamedSpans(t *testing.T) {
	h := newHome(t)
	seedSpans(t, h,
		`INSERT INTO spans(id,trace_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens)
		 VALUES('a1','tr-a','llm.call','coder','gpt-4o','2026-07-01T00:00:00Z','2026-07-01T00:00:01Z',1500,'ok',100,200)`,
		`INSERT INTO spans(id,trace_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens)
		 VALUES('a2','tr-a','llm.call','coder','gpt-4o','2026-07-01T00:00:02Z','2026-07-01T00:00:04Z',2700,'ok',300,400)`,
		`INSERT INTO spans(id,trace_id,name,profile,model_id,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens)
		 VALUES('b1','tr-b','llm.call','coder','gpt-4o','2026-07-01T00:00:00Z','2026-07-01T00:00:01Z',1000,'ok',10,20)`,
	)
	if out, code := run(t, h, "trace", "snapshot", "tr-a"); code != 0 {
		t.Fatalf("snapshot a: %s", out)
	}
	if out, code := run(t, h, "trace", "snapshot", "tr-b"); code != 0 {
		t.Fatalf("snapshot b: %s", out)
	}

	out, code := run(t, h, "trace", "diff", "tr-a", "tr-b")
	if code != 0 {
		t.Fatalf("diff exit=%d out=%s", code, out)
	}
	// trace A totals 1000 tokens across the two llm.call spans; B totals 30.
	if !strings.Contains(out, "TOTAL") {
		t.Fatalf("diff has no TOTAL row:\n%s", out)
	}
	var totalLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "TOTAL") {
			totalLine = l
		}
	}
	for _, want := range []string{"1000", "30", "-970"} {
		if !strings.Contains(totalLine, want) {
			t.Errorf("TOTAL row %q missing %q (same-named spans were dropped)", totalLine, want)
		}
	}

	jout, code := run(t, h, "--json", "trace", "diff", "tr-a", "tr-b")
	if code != 0 {
		t.Fatalf("json diff exit=%d out=%s", code, jout)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(jout), &entries); err != nil {
		t.Fatalf("json diff decode: %v\n%s", err, jout)
	}
	llmEntries := 0
	sumA := 0
	for _, e := range entries {
		if e["name"] != "llm.call" {
			continue
		}
		llmEntries++
		if a, ok := e["a"].(map[string]any); ok {
			pt, _ := a["prompt_tokens"].(float64)
			ct, _ := a["completion_tokens"].(float64)
			sumA += int(pt) + int(ct)
		}
	}
	if llmEntries != 2 {
		t.Errorf("want 2 llm.call diff entries, got %d (same-named spans collapsed)", llmEntries)
	}
	if sumA != 1000 {
		t.Errorf("trace A llm.call tokens = %d, want 1000", sumA)
	}
}

// TestSecurityScanJSONIsAlwaysArray covers finding #10 (scan half): a clean
// directory must emit `[]`, not `null`.
func TestSecurityScanJSONIsAlwaysArray(t *testing.T) {
	h := newHome(t)
	clean := t.TempDir()
	if err := os.WriteFile(filepath.Join(clean, "readme.txt"), []byte("nothing to see\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, h, "--json", "security", "scan", clean)
	if code != 0 {
		t.Fatalf("scan exit=%d out=%s", code, out)
	}
	if strings.TrimSpace(out) == "null" {
		t.Fatalf("clean scan emitted `null`, want `[]`")
	}
	var findings []map[string]any
	if err := json.Unmarshal([]byte(out), &findings); err != nil {
		t.Fatalf("scan --json not decodable: %v\n%s", err, out)
	}
	if findings == nil {
		t.Errorf("scan --json decoded to nil (JSON null), want empty array")
	}
	if len(findings) != 0 {
		t.Errorf("clean dir produced findings: %v", findings)
	}
}

// TestSecurityListJSON covers finding #10 (list half): `security list --json`
// must emit valid JSON — `[]` when there are no scans and an array of objects
// afterwards. It previously ignored --json entirely (empty output).
func TestSecurityListJSON(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "--json", "security", "list")
	if code != 0 {
		t.Fatalf("list exit=%d out=%s", code, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("security list --json produced no output at all")
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("empty list --json not decodable: %v\n%s", err, out)
	}
	if len(rows) != 0 {
		t.Errorf("fresh home should have no scans, got %v", rows)
	}

	clean := t.TempDir()
	if err := os.WriteFile(filepath.Join(clean, "ok.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := run(t, h, "security", "scan", clean); code != 0 {
		t.Fatalf("scan exit=%d out=%s", code, out)
	}
	out, code = run(t, h, "--json", "security", "list")
	if code != 0 {
		t.Fatalf("list exit=%d out=%s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list --json not decodable: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 recorded scan, got %d: %v", len(rows), rows)
	}
	if _, ok := rows[0]["scanned_path"]; !ok {
		t.Errorf("row missing scanned_path: %v", rows[0])
	}
}
