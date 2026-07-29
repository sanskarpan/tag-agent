package trace

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// openSpansDB creates a throwaway DB with the shipped spans schema.
func openSpansDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE spans (
	  id TEXT PRIMARY KEY, trace_id TEXT NOT NULL, parent_id TEXT, name TEXT NOT NULL, profile TEXT, model_id TEXT,
	  started_at TEXT NOT NULL, finished_at TEXT, duration_ms INTEGER, status TEXT NOT NULL DEFAULT 'ok',
	  prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
	  attributes TEXT NOT NULL DEFAULT '{}', error_msg TEXT, kind TEXT, cost_usd REAL)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSpanIDsAreTwelveHexChars(t *testing.T) {
	// tracing.py uses uuid4().hex[:12]; otel-export relies on ids being hex so
	// they are zero-padded rather than SHA-folded into something unrecognisable.
	for i := 0; i < 50; i++ {
		id := NewID()
		if len(id) != 12 {
			t.Fatalf("id %q has length %d, want 12", id, len(id))
		}
		for _, c := range id {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("id %q is not lowercase hex", id)
			}
		}
	}
}

func TestSaveWritesEveryColumn(t *testing.T) {
	db := openSpansDB(t)
	rec := NewRecorder("tr-1", "coder")
	root := rec.Start("agent.run", KindAgent, "", "openai/gpt-4o-mini")
	turn := rec.Start("llm.call", KindLLM, root.ID, "openai/gpt-4o-mini")
	rec.Attr(turn, "tag.step", 1)
	rec.End(turn, StatusOK, "", 1_000_000, 1_000_000)
	rec.End(root, StatusOK, "", 0, 0)
	if err := rec.Save(db); err != nil {
		t.Fatalf("save: %v", err)
	}

	var id, traceID, name, kind, status, attrs string
	var parent, errMsg sql.NullString
	var pt, ct int
	var dur sql.NullInt64
	var cost sql.NullFloat64
	if err := db.QueryRow(`SELECT id,trace_id,parent_id,name,kind,status,attributes,error_msg,
		prompt_tokens,completion_tokens,duration_ms,cost_usd FROM spans WHERE kind='llm'`).
		Scan(&id, &traceID, &parent, &name, &kind, &status, &attrs, &errMsg, &pt, &ct, &dur, &cost); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if traceID != "tr-1" || name != "llm.call" || status != StatusOK {
		t.Errorf("unexpected row: %s/%s/%s", traceID, name, status)
	}
	if !parent.Valid || parent.String != root.ID {
		t.Errorf("parent_id = %v, want %s", parent, root.ID)
	}
	if pt != 1_000_000 || ct != 1_000_000 {
		t.Errorf("tokens = %d/%d", pt, ct)
	}
	if !dur.Valid {
		t.Error("duration_ms must be set on a closed span")
	}
	// gpt-4o-mini: 0.15 in + 0.60 out per 1M tokens.
	if !cost.Valid || cost.Float64 < 0.74 || cost.Float64 > 0.76 {
		t.Errorf("cost_usd = %v, want ~0.75", cost)
	}
	if errMsg.Valid {
		t.Errorf("error_msg must be NULL on an ok span, got %v", errMsg)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(attrs), &m); err != nil {
		t.Fatalf("attributes not JSON: %v", err)
	}
	// A root span must persist a NULL parent, not "".
	var rootParent sql.NullString
	if err := db.QueryRow(`SELECT parent_id FROM spans WHERE kind='agent'`).Scan(&rootParent); err != nil {
		t.Fatal(err)
	}
	if rootParent.Valid {
		t.Errorf("root parent_id = %v, want NULL", rootParent)
	}
}

func TestUnknownModelLeavesCostNull(t *testing.T) {
	db := openSpansDB(t)
	rec := NewRecorder("tr-2", "")
	s := rec.Start("llm.call", KindLLM, "", "some-vendor/not-a-real-model")
	rec.End(s, StatusOK, "", 100, 100)
	if err := rec.Save(db); err != nil {
		t.Fatal(err)
	}
	var cost sql.NullFloat64
	if err := db.QueryRow(`SELECT cost_usd FROM spans`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost.Valid {
		// $0 would understate a real cost; "unknown" must stay unknown.
		t.Errorf("cost_usd = %v, want NULL for an unpriced model", cost.Float64)
	}
}

func TestEndIsIdempotent(t *testing.T) {
	rec := NewRecorder("tr-3", "")
	s := rec.Start("llm.call", KindLLM, "", "")
	rec.End(s, StatusError, "first", 1, 2)
	rec.End(s, StatusOK, "", 99, 99)
	got := rec.Spans()[0]
	if got.Status != StatusError || got.ErrorMsg != "first" || got.PromptTokens != 1 {
		t.Fatalf("second End overwrote the first: %+v", got)
	}
}

func TestSaveClosesOpenSpans(t *testing.T) {
	db := openSpansDB(t)
	rec := NewRecorder("tr-4", "")
	rec.Start("llm.call", KindLLM, "", "") // never closed (process died mid-run)
	if err := rec.Save(db); err != nil {
		t.Fatal(err)
	}
	var status string
	var finished sql.NullString
	if err := db.QueryRow(`SELECT status, finished_at FROM spans`).Scan(&status, &finished); err != nil {
		t.Fatal(err)
	}
	if status != StatusError || !finished.Valid {
		t.Errorf("an unclosed span must persist as a finished error, got %s/%v", status, finished)
	}
}

func TestSaveIsIdempotentAcrossCalls(t *testing.T) {
	db := openSpansDB(t)
	rec := NewRecorder("tr-5", "")
	s := rec.Start("llm.call", KindLLM, "", "")
	rec.End(s, StatusOK, "", 1, 1)
	for i := 0; i < 3; i++ {
		if err := rec.Save(db); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM spans`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("re-saving duplicated rows: got %d, want 1", n)
	}
}

func TestNilRecorderIsSafe(t *testing.T) {
	var rec *Recorder
	s := rec.Start("x", KindLLM, "", "")
	rec.Attr(s, "k", "v")
	rec.End(s, StatusOK, "", 1, 1)
	if err := rec.Save(nil); err != nil {
		t.Fatal(err)
	}
	if got := rec.Spans(); got != nil {
		t.Errorf("nil recorder returned spans: %v", got)
	}
}

func TestRecorderIsConcurrencySafe(t *testing.T) {
	rec := NewRecorder("tr-6", "")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := rec.Start("llm.call", KindLLM, "", "openai/gpt-4o-mini")
			rec.Attr(s, "n", 1)
			rec.End(s, StatusOK, "", 10, 10)
		}()
	}
	wg.Wait()
	if got := len(rec.Spans()); got != 32 {
		t.Fatalf("recorded %d spans, want 32", got)
	}
}
